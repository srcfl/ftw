// ftw-updater is the sidecar that executes `docker compose pull` +
// `docker compose up -d` on behalf of the main forty-two-watts container.
//
// It runs in its own container with the Docker socket mounted in and a
// read-only bind to docker-compose.yml, and listens on a Unix socket shared
// with the main container via a tmpfs volume. The main container never
// touches the Docker socket itself — all destructive actions cross this
// one-way boundary.
//
// State is written to state.json in the shared volume before and after
// each step so the main container can reflect progress to the UI even
// after it has been recreated.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

const mainServiceName = "forty-two-watts"

// State mirrors selfupdate.UpdateStatus (we keep a local copy to avoid
// importing the main module's internal package from this separate cmd).
type State struct {
	State     string    `json:"state"`            // idle, pulling, restarting, restoring, done, failed
	Action    string    `json:"action,omitempty"` // update, restart, rollback (#152)
	Target    string    `json:"target,omitempty"`
	Snapshot  string    `json:"snapshot,omitempty"` // snapshot id (rollback only, #152)
	StartedAt time.Time `json:"started_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	Message   string    `json:"message,omitempty"`
}

type server struct {
	composeFile string
	// overrideFiles are auto-discovered compose overrides next to
	// composeFile (e.g. docker-compose.override.yml). Each one is added
	// as an extra -f arg to every compose invocation so the sidecar
	// sees the same merged config the user sees when running compose by
	// hand from the project dir.
	overrideFiles []string
	statusPath    string
	stateMu       sync.Mutex
	// skipPull is a dev-only escape hatch: when true, the "pulling" phase
	// becomes a no-op. Needed for local smoke tests where the image lives
	// only on the dev machine (`docker compose pull` would fail, or worse,
	// overwrite the local build with a stale GHCR tag). Production leaves
	// this at false.
	skipPull bool
	// pullRetryDelay is the wait between pull attempts. Defaults to 60s
	// in production; tests set it to a small value to keep runs fast.
	pullRetryDelay time.Duration

	// runMu ensures only one pull+up runs at a time. HTTP handlers that
	// arrive while a job is in flight return 409.
	runMu sync.Mutex
	// runner lets tests inject a fake exec. env is the extra KEY=VALUE
	// entries to append to the docker process's environment — used to
	// pass FTW_IMAGE_TAG=<target> so compose's image tag substitution
	// pins to the requested version. nil/empty means "inherit only".
	runner func(ctx context.Context, env []string, args ...string) error
}

// composeArgs returns the common prefix of every `docker compose` invocation
// the sidecar makes — the base file plus any auto-discovered overrides,
// followed by whatever subcommand + args the caller passes.
func (s *server) composeArgs(sub ...string) []string {
	out := []string{"compose", "-f", s.composeFile}
	for _, o := range s.overrideFiles {
		out = append(out, "-f", o)
	}
	return append(out, sub...)
}

// discoverOverrides looks for the standard override filenames in the same
// directory as base, in the same order the compose CLI resolves them.
// Only returns files that actually exist — the list is used verbatim as
// additional -f flags so a missing file would error the command.
func discoverOverrides(base string) []string {
	dir := filepath.Dir(base)
	var out []string
	for _, name := range []string{"docker-compose.override.yml", "docker-compose.override.yaml", "compose.override.yml", "compose.override.yaml"} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

func main() {
	socket := flag.String("socket", envOr("FTW_UPDATER_SOCKET", "/run/ftw-update/sock"), "Unix socket to listen on")
	statusPath := flag.String("status", envOr("FTW_UPDATER_STATUS", "/run/ftw-update/state.json"), "State file to write")
	compose := flag.String("compose", envOr("FTW_UPDATER_COMPOSE", "/compose/docker-compose.yml"), "Path to docker-compose.yml")
	skipPull := flag.Bool("skip-pull", envOr("FTW_UPDATER_SKIP_PULL", "") != "", "Dev: skip docker compose pull (keeps local image)")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	slog.Info("ftw-updater starting", "socket", *socket, "status", *statusPath, "compose", *compose)

	// Guarantee the shared status/socket dir exists even if the tmpfs mount
	// is empty. It must be writable by the non-root main container too,
	// because the main service publishes early states while it captures the
	// pre-update snapshot.
	if err := prepareSharedDir(filepath.Dir(*statusPath)); err != nil {
		slog.Error("mkdir status dir", "err", err)
		os.Exit(1)
	}
	if err := prepareSharedDir(filepath.Dir(*socket)); err != nil {
		slog.Error("mkdir socket dir", "err", err)
		os.Exit(1)
	}

	srv := &server{
		composeFile:    *compose,
		statusPath:     *statusPath,
		skipPull:       *skipPull,
		pullRetryDelay: 60 * time.Second,
		runner:         dockerCompose,
	}
	// Auto-discover override files alongside the base, the same way the
	// compose CLI does when invoked without -f. Without this the sidecar
	// would ignore local dev overrides (network_mode, build args, …) and
	// recreate the main container with base-file settings only.
	srv.overrideFiles = discoverOverrides(*compose)
	if len(srv.overrideFiles) > 0 {
		slog.Info("ftw-updater: override files discovered", "files", srv.overrideFiles)
	}
	if *skipPull {
		slog.Warn("ftw-updater: skip-pull enabled — production deploys should leave this off")
	}
	srv.recoverCrashedState()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /update", srv.handleUpdate)
	mux.HandleFunc("GET /status", srv.handleStatus)

	// Remove a stale socket — common pattern; the listener would EADDRINUSE otherwise.
	_ = os.Remove(*socket)
	ln, err := net.Listen("unix", *socket)
	if err != nil {
		slog.Error("listen unix", "err", err)
		os.Exit(1)
	}
	// Socket is in a shared tmpfs volume; restrict to world-rw so the main
	// container (ftw uid=100) can connect without caring about ownership.
	if err := os.Chmod(*socket, 0o666); err != nil {
		slog.Warn("chmod socket", "err", err)
	}

	httpSrv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = httpSrv.Close()
	}()

	slog.Info("ftw-updater listening")
	if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
		slog.Error("serve", "err", err)
	}
}

func (s *server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action   string   `json:"action"`
		Target   string   `json:"target,omitempty"`
		Snapshot string   `json:"snapshot,omitempty"` // rollback-only (#152)
		Files    []string `json:"files,omitempty"`    // rollback: basenames to restore
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body); err != nil {
		http.Error(w, "bad json: "+err.Error(), 400)
		return
	}
	switch body.Action {
	case "update":
		// Updates must be version-pinned: the sidecar refuses to fall
		// back to `:latest` because that re-introduces the race where a
		// pull resolves to the old digest while the build workflow's
		// retag is in flight.
		if body.Target == "" {
			http.Error(w, "update requires target version (vX.Y.Z)", 400)
			return
		}
		if !isSemverTag(body.Target) {
			http.Error(w, "target must look like vX.Y.Z", 400)
			return
		}
	case "restart":
		// target optional — when empty, compose's `${FTW_IMAGE_TAG:-latest}`
		// substitution falls through to :latest. That's the dev path for
		// exercising the flow without a real release.
	case "rollback":
		if body.Snapshot == "" {
			http.Error(w, "rollback requires snapshot id", 400)
			return
		}
		// Basename-only — never let a client traverse out of the
		// conventional snapshots dir on the host.
		if strings.ContainsAny(body.Snapshot, "/\\") || body.Snapshot == "." || body.Snapshot == ".." {
			http.Error(w, "invalid snapshot id", 400)
			return
		}
		for _, f := range body.Files {
			if strings.ContainsAny(f, "/\\") || f == "." || f == ".." {
				http.Error(w, "invalid file in rollback request", 400)
				return
			}
		}
	default:
		http.Error(w, "action must be update, restart, or rollback", 400)
		return
	}
	if !s.runMu.TryLock() {
		http.Error(w, "update already in progress", 409)
		return
	}

	// Hand off to a goroutine so the HTTP caller gets 202 immediately; the
	// main container's UI polls /status for progress.
	go func() {
		defer s.runMu.Unlock()
		if body.Action == "rollback" {
			s.runRollback(body.Snapshot, body.Files)
		} else {
			s.runJob(body.Action, body.Target)
		}
	}()

	w.WriteHeader(202)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":   "started",
		"action":   body.Action,
		"target":   body.Target,
		"snapshot": body.Snapshot,
	})
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	st := s.readState()
	_ = json.NewEncoder(w).Encode(st)
}

// runJob executes a pull+up (or pull+up --force-recreate) sequence,
// emitting state transitions between steps. Runs inside a goroutine so
// the HTTP handler that kicked it off has already responded.
//
// When target is non-empty (always the case for action=update), it's
// passed as FTW_IMAGE_TAG=<target> so docker-compose.yml's image tag
// substitution pulls the specific version. action=restart with empty
// target falls through to compose's default (`:latest`) — that's the
// dev path for exercising the flow without a real release.
func (s *server) runJob(action, target string) {
	now := time.Now()
	s.writeState(State{State: "pulling", Action: action, Target: target, StartedAt: now, UpdatedAt: now})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	var env []string
	if target != "" {
		env = []string{"FTW_IMAGE_TAG=" + target}
	}
	if action == "update" {
		if err := s.validateComposeImagePin(); err != nil {
			msg := "compose preflight failed: " + err.Error()
			s.writeState(State{State: "failed", Action: action, Target: target, StartedAt: now, UpdatedAt: time.Now(), Message: msg})
			slog.Error("compose preflight failed", "err", err)
			return
		}
	}

	if !s.skipPull {
		pullArgs := s.composeArgs("pull", mainServiceName)
		var pullErr error
	pullLoop:
		for attempt := 1; attempt <= 3; attempt++ {
			pullErr = s.runner(ctx, env, pullArgs...)
			if pullErr == nil || attempt == 3 {
				break
			}
			slog.Warn("pull failed, retrying", "attempt", attempt, "err", pullErr)
			select {
			case <-time.After(s.pullRetryDelay):
			case <-ctx.Done():
				pullErr = ctx.Err()
				break pullLoop
			}
		}
		if pullErr != nil {
			s.writeState(State{State: "failed", Action: action, Target: target, StartedAt: now, UpdatedAt: time.Now(), Message: "pull failed: " + pullErr.Error()})
			slog.Error("pull failed", "err", pullErr)
			return
		}
	} else {
		slog.Info("skip-pull active; continuing straight to compose up")
	}

	s.writeState(State{State: "restarting", Action: action, Target: target, StartedAt: now, UpdatedAt: time.Now()})

	upArgs := s.composeArgs("up", "-d", mainServiceName)
	if action == "restart" {
		// --force-recreate is what makes restart actually restart when the
		// image digest didn't change — exactly the dev/test path the main
		// UI exposes as the "Restart" button.
		upArgs = s.composeArgs("up", "-d", "--force-recreate", mainServiceName)
	}
	if err := s.runner(ctx, env, upArgs...); err != nil {
		s.writeState(State{State: "failed", Action: action, Target: target, StartedAt: now, UpdatedAt: time.Now(), Message: "up -d failed: " + err.Error()})
		slog.Error("compose up failed", "err", err)
		return
	}

	// The main container is now being recreated. The brand-new replica
	// will read this "done" state on startup and serve it to the UI that's
	// still polling in the browser.
	s.writeState(State{State: "done", Action: action, Target: target, StartedAt: now, UpdatedAt: time.Now(), Message: "compose up -d completed"})
}

// validateComposeImagePin catches old host-side compose files that hard-code
// :latest. In that layout FTW_IMAGE_TAG is passed to docker compose but never
// read, so the sidecar can report "done" while the main service stays on the
// previous image.
func (s *server) validateComposeImagePin() error {
	image, ok, err := serviceImageFromComposeFiles(append([]string{s.composeFile}, s.overrideFiles...), mainServiceName)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("service %q is missing an image entry in compose files", mainServiceName)
	}
	if !strings.Contains(image, "FTW_IMAGE_TAG") {
		return fmt.Errorf("service %q image %q does not reference FTW_IMAGE_TAG; update docker-compose.yml from the latest install script", mainServiceName, image)
	}
	return nil
}

func serviceImageFromComposeFiles(files []string, service string) (string, bool, error) {
	var image string
	for _, file := range files {
		img, ok, err := serviceImageFromComposeFile(file, service)
		if err != nil {
			return "", false, err
		}
		if ok {
			image = img
		}
	}
	if image == "" {
		return "", false, nil
	}
	return image, true, nil
}

func serviceImageFromComposeFile(path, service string) (string, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, fmt.Errorf("read %s: %w", path, err)
	}
	var doc struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return "", false, fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.Services == nil {
		return "", false, nil
	}
	svc, ok := doc.Services[service]
	if !ok || svc.Image == "" {
		return "", false, nil
	}
	return svc.Image, true, nil
}

// runRollback restores a snapshot's files over the main container's data
// volume, keeping the image unchanged ("soft" rollback). The sequence is
// intentionally simple:
//
//  1. compose stop forty-two-watts — main releases its SQLite handle so
//     the swap doesn't write into a live DB.
//  2. docker cp each file from the snapshot dir on the host into the
//     stopped container. Bind-mount semantics make this a direct write
//     to the host-side data dir; no read-only/writable bind trick needed.
//  3. compose up -d forty-two-watts — main comes up on the restored state.
//
// Paths: the snapshots live at <host_project_dir>/data/snapshots/<id>.
// We compute host_project_dir from FTW_UPDATER_COMPOSE (an absolute host
// path already, documented in docker-compose.yml). Operators who move
// the data bind off the default `./data:/app/data` layout would need a
// separate override; tracked as a follow-up if we hit that in practice.
//
// A failed rollback leaves state: "failed" with a descriptive message
// and — critically — still runs compose up -d in defer so the main
// container comes back, even if wrong. Leaving the service down would
// strand the operator.
func (s *server) runRollback(snapshotID string, files []string) {
	now := time.Now()
	base := State{Action: "rollback", Snapshot: snapshotID, StartedAt: now}
	s.writeState(State{State: "restoring", Action: base.Action, Snapshot: base.Snapshot, StartedAt: now, UpdatedAt: now, Message: "stopping main service"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	hostProjectDir := filepath.Dir(s.composeFile)
	hostSnapDir := filepath.Join(hostProjectDir, "data", "snapshots", snapshotID)
	if _, err := os.Stat(hostSnapDir); err != nil {
		s.writeState(State{State: "failed", Action: base.Action, Snapshot: base.Snapshot, StartedAt: now, UpdatedAt: time.Now(), Message: "snapshot not readable from sidecar: " + err.Error()})
		return
	}

	// 1. Stop the main service so SQLite isn't holding a file handle
	// while we swap state.db under it.
	stopArgs := s.composeArgs("stop", mainServiceName)
	if err := s.runner(ctx, nil, stopArgs...); err != nil {
		s.writeState(State{State: "failed", Action: base.Action, Snapshot: base.Snapshot, StartedAt: now, UpdatedAt: time.Now(), Message: "compose stop failed: " + err.Error()})
		return
	}

	// 2. Restore each requested file via docker cp. Defaults to the
	// ones we always snapshot — state.db + config.yaml — when the
	// caller left Files empty.
	if len(files) == 0 {
		files = []string{"state.db", "config.yaml"}
	}
	s.writeState(State{State: "restoring", Action: base.Action, Snapshot: base.Snapshot, StartedAt: now, UpdatedAt: time.Now(), Message: "copying snapshot files"})
	for _, f := range files {
		src := filepath.Join(hostSnapDir, f)
		if _, err := os.Stat(src); err != nil {
			// Optional files (e.g. config.yaml when the operator
			// runs with defaults and no YAML on disk) just get
			// skipped — not a rollback failure.
			slog.Info("rollback: source missing, skipping", "file", f, "err", err)
			continue
		}
		dst := "/app/data/" + f
		cpArgs := []string{"cp", src, mainServiceName + ":" + dst}
		if err := s.runner(ctx, nil, cpArgs...); err != nil {
			// File-swap failure is serious. Try to bring the
			// service back anyway so the operator isn't stranded.
			slog.Error("rollback docker cp failed", "file", f, "err", err)
			s.writeState(State{State: "failed", Action: base.Action, Snapshot: base.Snapshot, StartedAt: now, UpdatedAt: time.Now(), Message: "docker cp " + f + " failed: " + err.Error()})
			_ = s.runner(ctx, nil, s.composeArgs("up", "-d", mainServiceName)...)
			return
		}
	}

	// 3. Start the main service again. --force-recreate so the new
	// process certainly sees the swapped files (same semantics as the
	// restart flow).
	s.writeState(State{State: "restarting", Action: base.Action, Snapshot: base.Snapshot, StartedAt: now, UpdatedAt: time.Now(), Message: "starting main service on restored state"})
	upArgs := s.composeArgs("up", "-d", "--force-recreate", mainServiceName)
	if err := s.runner(ctx, nil, upArgs...); err != nil {
		s.writeState(State{State: "failed", Action: base.Action, Snapshot: base.Snapshot, StartedAt: now, UpdatedAt: time.Now(), Message: "compose up -d failed: " + err.Error()})
		return
	}
	s.writeState(State{State: "done", Action: base.Action, Snapshot: base.Snapshot, StartedAt: now, UpdatedAt: time.Now(), Message: "rollback complete"})
}

func (s *server) writeState(st State) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if st.UpdatedAt.IsZero() {
		st.UpdatedAt = time.Now()
	}
	tmp := s.statusPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		slog.Warn("state write", "err", err)
		return
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(st); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		slog.Warn("state encode", "err", err)
		return
	}
	_ = f.Close()
	// Atomic swap so partial writes never leak to a reader on the other
	// side of the shared volume. On Windows the rename fails when the
	// destination is open for reading; fall back to remove-then-rename
	// so tests pass without relaxing production semantics on Linux.
	if err := os.Rename(tmp, s.statusPath); err != nil {
		_ = os.Remove(s.statusPath)
		if err2 := os.Rename(tmp, s.statusPath); err2 != nil {
			slog.Warn("state rename", "err", err2)
		}
	}
}

func (s *server) readState() State {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	data, err := os.ReadFile(s.statusPath)
	if err != nil {
		return State{State: "idle"}
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" {
		return State{State: "idle"}
	}
	return st
}

// recoverCrashedState runs once at boot. If state.json says we're in-flight
// but the heartbeat is older than 5 min, we know the previous updater
// process was killed mid-run and we flip to failed so the UI unblocks.
func (s *server) recoverCrashedState() {
	st := s.readState()
	if (st.State == "pulling" || st.State == "restarting") && time.Since(st.UpdatedAt) > 5*time.Minute {
		prev := st.State
		st.State = "failed"
		if st.Message == "" {
			st.Message = "updater process restarted while in-flight"
		}
		st.UpdatedAt = time.Now()
		s.writeState(st)
		slog.Warn("recovered in-flight state as failed", "prev_state", prev)
	}
}

// dockerCompose shells out to the `docker` CLI (the compose plugin ships
// in the docker:27-cli image). stdout+stderr are captured so a failure
// includes the compose error text in state.json. extraEnv is appended
// to the inherited process environment so callers can set
// FTW_IMAGE_TAG=<version> for compose's `${FTW_IMAGE_TAG:-latest}`
// substitution to pin a specific version.
func dockerCompose(ctx context.Context, extraEnv []string, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, truncate(string(out), 400))
	}
	slog.Info("docker compose ok", "args", args, "env", extraEnv, "out", truncate(string(out), 200))
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}

func prepareSharedDir(dir string) error {
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	return os.Chmod(dir, 0o777)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// isSemverTag matches the strict "vX.Y.Z" tag shape that release-please
// produces. Pre-release suffixes (-rc1, +meta) are intentionally
// rejected here so the sidecar can't be talked into pinning the deploy
// to a release candidate via a crafted target string.
func isSemverTag(s string) bool {
	if !strings.HasPrefix(s, "v") {
		return false
	}
	parts := strings.Split(s[1:], ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}
