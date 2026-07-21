// ftw-updater is the sidecar that executes `docker compose pull` +
// `docker compose up -d` on behalf of the main FTW container.
//
// It runs in its own container with the Docker socket mounted in and a
// read-only bind to docker-compose.yml, and listens on a Unix socket shared
// with the main container via a shared Docker volume. The main container never
// touches the Docker socket itself — all destructive actions cross this
// one-way boundary.
//
// State is written to state.json in the shared volume before and after
// each step so the main container can reflect progress to the UI even
// after it has been recreated.
package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	canonicalMainServiceName = "ftw"
	legacyMainServiceName    = "forty-two-watts"
	canonicalMainImage       = "ghcr.io/srcfl/ftw"
	optimizerServiceName     = "ftw-optimizer"
	canonicalOptimizerImage  = "ghcr.io/srcfl/ftw-optimizer"
)

// State mirrors selfupdate.UpdateStatus (we keep a local copy to avoid
// importing the main module's internal package from this separate cmd).
type State struct {
	State           string            `json:"state"`            // idle, pulling, restarting, restoring, done, failed
	Action          string            `json:"action,omitempty"` // update, restart, rollback (#152)
	Component       string            `json:"component,omitempty"`
	Target          string            `json:"target,omitempty"`
	Snapshot        string            `json:"snapshot,omitempty"` // snapshot id (rollback only, #152)
	Files           []string          `json:"files,omitempty"`
	SafetySnapshot  string            `json:"safety_snapshot,omitempty"`
	SafetyFiles     []string          `json:"safety_files,omitempty"`
	StartedAt       time.Time         `json:"started_at,omitempty"`
	UpdatedAt       time.Time         `json:"updated_at,omitempty"`
	Message         string            `json:"message,omitempty"`
	PreviousImageID string            `json:"previous_image_id,omitempty"`
	PreviousImages  map[string]string `json:"previous_images,omitempty"`
}

type server struct {
	composeFile     string
	mainServiceName string
	// overrideFiles are auto-discovered compose overrides next to
	// composeFile (e.g. docker-compose.override.yml). Each one is added
	// as an extra -f arg to every compose invocation so the sidecar
	// sees the same merged config the user sees when running compose by
	// hand from the project dir.
	overrideFiles []string
	// updateOverrideFile is a short-lived, updater-owned Compose override
	// appended after the host's files when an older deployment hard-codes its
	// main image. It lets immutable updates migrate safely without modifying
	// the read-only host Compose file or changing its service/data layout.
	updateOverrideFile string
	statusPath         string
	stateMu            sync.Mutex
	// skipPull is a dev-only escape hatch: when true, the "pulling" phase
	// becomes a no-op. Needed for local smoke tests where the image lives
	// only on the dev machine (`docker compose pull` would fail, or worse,
	// overwrite the local build with a stale GHCR tag). Production leaves
	// this at false.
	skipPull bool
	// pullRetryDelay is the wait between pull attempts. Defaults to 60s
	// in production; tests set it to a small value to keep runs fast.
	pullRetryDelay time.Duration
	// maxPullAttempts caps retries before giving up with "failed". 0 means
	// unlimited — the production default so a slow connection never gives up
	// after an arbitrary N. Tests that exercise the "always-fail" path set
	// this to a small value to avoid looping forever.
	maxPullAttempts int

	// runMu ensures only one pull+up runs at a time. HTTP handlers that
	// arrive while a job is in flight return 409.
	runMu sync.Mutex
	// runner lets tests inject a fake exec. env is the extra KEY=VALUE
	// entries to append to the docker process's environment — used to
	// pass FTW_IMAGE_TAG=<target> so compose's image tag substitution
	// pins to the requested version. nil/empty means "inherit only".
	runner func(ctx context.Context, env []string, args ...string) error
	// imageID captures the image backing the running service before an update.
	// healthCheck waits for the recreated service to become healthy. Both are
	// injectable so the rollback path is testable without Docker.
	imageID           func(ctx context.Context, service string) (string, error)
	containerID       func(ctx context.Context, service string) (string, error)
	healthCheck       func(ctx context.Context, service string) error
	chownFile         func(string, int, int) error
	checkSnapshotFile func(context.Context, string, string, string) error
	stageSnapshotFile func(context.Context, string, string, string, string) error
}

// composeArgs returns the common prefix of every `docker compose` invocation
// the sidecar makes — the base file plus any auto-discovered overrides,
// followed by whatever subcommand + args the caller passes.
func (s *server) composeArgs(sub ...string) []string {
	out := []string{"compose", "-f", s.composeFile}
	for _, o := range s.overrideFiles {
		out = append(out, "-f", o)
	}
	if s.updateOverrideFile != "" {
		out = append(out, "-f", s.updateOverrideFile)
	}
	return append(out, sub...)
}

func (s *server) composeFiles() []string {
	files := append([]string{s.composeFile}, s.overrideFiles...)
	if s.updateOverrideFile != "" {
		files = append(files, s.updateOverrideFile)
	}
	return files
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

// selectMainService accepts the canonical and legacy service names, but only
// when exactly one of them owns the persistent /app/data mount. This prevents
// an ambiguous compose file from ever starting two host-networked controllers.
func selectMainService(files []string, configured string) (string, error) {
	mapsData, err := servicesMappingAppData(files)
	if err != nil {
		return "", err
	}
	if configured != "" {
		if !mapsData[configured] {
			return "", fmt.Errorf("configured service %q does not map persistent /app/data", configured)
		}
		return configured, nil
	}
	var candidates []string
	for _, name := range []string{canonicalMainServiceName, legacyMainServiceName} {
		if mapsData[name] {
			candidates = append(candidates, name)
		}
	}
	if len(candidates) != 1 {
		return "", fmt.Errorf("expected exactly one FTW service mapping /app/data, found %v", candidates)
	}
	return candidates[0], nil
}

func servicesMappingAppData(files []string) (map[string]bool, error) {
	out := make(map[string]bool)
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		var doc struct {
			Services map[string]struct {
				Volumes []any `yaml:"volumes"`
			} `yaml:"services"`
		}
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		for name, svc := range doc.Services {
			for _, volume := range svc.Volumes {
				switch v := volume.(type) {
				case string:
					parts := strings.Split(v, ":")
					if (len(parts) >= 2 && parts[len(parts)-1] == "/app/data") ||
						(len(parts) >= 3 && parts[len(parts)-2] == "/app/data") {
						out[name] = true
					}
				case map[string]any:
					if target, _ := v["target"].(string); target == "/app/data" {
						out[name] = true
					}
				}
			}
		}
	}
	return out, nil
}

func main() {
	socket := flag.String("socket", envOr("FTW_UPDATER_SOCKET", "/run/ftw-update/sock"), "Unix socket to listen on")
	statusPath := flag.String("status", envOr("FTW_UPDATER_STATUS", "/run/ftw-update/state.json"), "State file to write")
	compose := flag.String("compose", envOr("FTW_UPDATER_COMPOSE", "/compose/docker-compose.yml"), "Path to docker-compose.yml")
	mainService := flag.String("main-service", envOr("FTW_UPDATER_MAIN_SERVICE", ""), "Compose service for FTW (auto-detected when empty)")
	skipPull := flag.Bool("skip-pull", envOr("FTW_UPDATER_SKIP_PULL", "") != "", "Dev: skip docker compose pull (keeps local image)")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	slog.Info("ftw-updater starting", "socket", *socket, "status", *statusPath, "compose", *compose)

	// Guarantee the shared status/socket dir exists even if the volume mount
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
	serviceFiles := append([]string{srv.composeFile}, srv.overrideFiles...)
	selectedService, err := selectMainService(serviceFiles, *mainService)
	if err != nil {
		slog.Error("ftw-updater: unsafe compose layout", "err", err)
		os.Exit(1)
	}
	srv.mainServiceName = selectedService
	srv.imageID = srv.currentServiceImageID
	srv.containerID = srv.serviceContainerID
	srv.healthCheck = srv.waitForServiceHealth
	srv.chownFile = os.Chown
	srv.checkSnapshotFile = func(ctx context.Context, containerID, snapshotID, file string) error {
		return srv.runner(ctx, nil, "exec", containerID, "test", "-f", "/app/data/snapshots/"+snapshotID+"/"+file)
	}
	srv.stageSnapshotFile = func(ctx context.Context, containerID, snapshotID, file, dst string) error {
		return srv.runner(ctx, nil, "cp", "-a", containerID+":/app/data/snapshots/"+snapshotID+"/"+file, dst)
	}
	slog.Info("ftw-updater: main service selected", "service", selectedService)
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
	// Socket is in a shared Docker volume; restrict to world-rw so the main
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
		Action         string    `json:"action"`
		Component      string    `json:"component,omitempty"`
		Target         string    `json:"target,omitempty"`
		Snapshot       string    `json:"snapshot,omitempty"` // rollback-only (#152)
		Files          []string  `json:"files,omitempty"`    // rollback: basenames to restore
		SafetySnapshot string    `json:"safety_snapshot,omitempty"`
		SafetyFiles    []string  `json:"safety_files,omitempty"`
		StartedAt      time.Time `json:"started_at,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body); err != nil {
		http.Error(w, "bad json: "+err.Error(), 400)
		return
	}
	if body.Component == "" {
		body.Component = "core"
	}
	if body.Component != "core" && body.Component != "optimizer" {
		http.Error(w, "component must be core or optimizer", 400)
		return
	}
	if body.Component == "optimizer" && body.Action != "update" && body.Action != "restart" && body.Action != "component_rollback" {
		http.Error(w, "optimizer component supports update, restart, or component_rollback", 400)
		return
	}
	switch body.Action {
	case "update":
		// Updates must be version-pinned: the sidecar refuses to fall
		// back to `:latest` because that re-introduces the race where a
		// pull resolves to the old digest while the build workflow's
		// retag is in flight.
		if body.Target == "" {
			http.Error(w, "update requires an immutable image tag", 400)
			return
		}
		if !isImmutableImageTag(body.Target) {
			http.Error(w, "target must be stable vX.Y.Z or beta vX.Y.Z-beta.N", 400)
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
		if body.SafetySnapshot == "" {
			http.Error(w, "rollback requires safety snapshot id", 400)
			return
		}
		// Basename-only — never let a client traverse out of the
		// conventional snapshots dir on the host.
		if !validSnapshotID(body.Snapshot) {
			http.Error(w, "invalid snapshot id", 400)
			return
		}
		if !validSnapshotID(body.SafetySnapshot) {
			http.Error(w, "invalid safety snapshot id", 400)
			return
		}
		for _, f := range append(append([]string{}, body.Files...), body.SafetyFiles...) {
			if !validRollbackFile(f) {
				http.Error(w, "invalid file in rollback request", 400)
				return
			}
		}
		if !hasRollbackState(body.Files) || !hasRollbackState(body.SafetyFiles) {
			http.Error(w, "rollback and safety snapshots must include state.db.gz", 400)
			return
		}
	case "component_rollback":
		if body.Component != "optimizer" {
			http.Error(w, "component_rollback is only available for optimizer", 400)
			return
		}
		if s.previousImageID(body.Component) == "" {
			http.Error(w, "no previous optimizer image is available", 409)
			return
		}
	default:
		http.Error(w, "action must be update, restart, rollback, or component_rollback", 400)
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
			s.runRollback(body.Snapshot, body.Files, body.SafetySnapshot, body.SafetyFiles)
		} else if body.Action == "component_rollback" {
			s.runComponentRollback(body.Component, body.StartedAt)
		} else {
			s.runComponentJob(body.Action, body.Target, body.Component, body.StartedAt)
		}
	}()

	w.WriteHeader(202)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":    "started",
		"action":    body.Action,
		"component": body.Component,
		"target":    body.Target,
		"snapshot":  body.Snapshot,
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
	s.runComponentJob(action, target, "core", time.Time{})
}

func (s *server) runComponentJob(action, target, component string, startedAt time.Time) {
	now := startedAt
	if now.IsZero() {
		now = time.Now()
	}
	spec, err := s.componentSpec(component)
	if err != nil {
		s.writeState(State{State: "failed", Action: action, Component: component, Target: target, StartedAt: now, UpdatedAt: now, Message: err.Error()})
		return
	}
	if action == "update" && spec.name == "core" {
		if err := s.requireHealthyOptimizer(); err != nil {
			msg := "core update blocked: " + err.Error()
			s.writeState(State{State: "failed", Action: action, Component: component, Target: target, StartedAt: now, UpdatedAt: time.Now(), Message: msg})
			slog.Error("core update blocked", "err", err)
			return
		}
	}
	s.writeState(State{State: "pulling", Action: action, Component: component, Target: target, StartedAt: now, UpdatedAt: now})

	var env []string
	if target != "" {
		env = []string{spec.tagEnv + "=" + target}
	}
	if action == "update" || action == "restart" {
		cleanup, err := s.prepareComponentImagePin(spec)
		if err != nil {
			msg := "compose preflight failed: " + err.Error()
			s.writeState(State{State: "failed", Action: action, Component: component, Target: target, StartedAt: now, UpdatedAt: time.Now(), Message: msg})
			slog.Error("compose preflight failed", "err", err)
			return
		}
		defer cleanup()
		if err := s.validateComponentImagePin(spec); err != nil {
			msg := "compose preflight failed: " + err.Error()
			s.writeState(State{State: "failed", Action: action, Component: component, Target: target, StartedAt: now, UpdatedAt: time.Now(), Message: msg})
			slog.Error("compose preflight failed", "err", err)
			return
		}
	}

	// Capture the current immutable image ID before pulling. Docker retains the
	// old image object after the tag moves, which lets us retag and recreate it
	// if the new container never becomes healthy.
	var previousImageID string
	if action == "update" && s.imageID != nil {
		inspectCtx, cancelInspect := context.WithTimeout(context.Background(), 30*time.Second)
		var err error
		previousImageID, err = s.imageID(inspectCtx, spec.service)
		cancelInspect()
		if err != nil {
			s.writeState(State{State: "failed", Action: action, Component: component, Target: target, StartedAt: now, UpdatedAt: time.Now(), Message: "cannot capture current image for rollback: " + err.Error()})
			return
		}
	}

	if !s.skipPull {
		pullArgs := s.composeArgs("pull", spec.service)
		var pullErr error
		for attempt := 1; ; attempt++ {
			// Per-attempt timeout: each pull gets a full 2 h window so a
			// single slow download on a 0.5 Mbps link (~400 MB image ≈ 90 min)
			// is never cut short by a shared outer deadline.
			attemptCtx, cancelAttempt := context.WithTimeout(context.Background(), 2*time.Hour)
			pullErr = s.runner(attemptCtx, env, pullArgs...)
			cancelAttempt()
			if pullErr == nil {
				break
			}
			slog.Warn("pull failed, retrying", "attempt", attempt, "err", pullErr)
			if s.maxPullAttempts > 0 && attempt >= s.maxPullAttempts {
				break
			}
			time.Sleep(s.pullRetryDelay)
		}
		if pullErr != nil {
			s.writeState(State{State: "failed", Action: action, Component: component, Target: target, StartedAt: now, UpdatedAt: time.Now(), Message: "pull failed: " + pullErr.Error()})
			slog.Error("pull failed", "err", pullErr)
			return
		}
	} else {
		slog.Info("skip-pull active; continuing straight to compose up")
	}

	s.writeState(State{State: "restarting", Action: action, Component: component, Target: target, StartedAt: now, UpdatedAt: time.Now()})

	// compose up -d just recreates the container from an already-pulled
	// image — should complete in seconds, 10 min is a generous upper bound.
	upCtx, upCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer upCancel()

	upArgs := s.composeArgs("up", "-d", spec.service)
	if action == "restart" {
		// --force-recreate is what makes restart actually restart when the
		// image digest didn't change — exactly the dev/test path the main
		// UI exposes as the "Restart" button.
		upArgs = s.composeArgs("up", "-d", "--force-recreate", spec.service)
	}
	if err := s.runner(upCtx, env, upArgs...); err != nil {
		s.writeState(State{State: "failed", Action: action, Component: component, Target: target, StartedAt: now, UpdatedAt: time.Now(), Message: "up -d failed: " + err.Error()})
		slog.Error("compose up failed", "err", err)
		return
	}

	if s.healthCheck != nil {
		healthCtx, cancelHealth := context.WithTimeout(context.Background(), componentHealthTimeout(spec.name))
		healthErr := s.healthCheck(healthCtx, spec.service)
		cancelHealth()
		if healthErr != nil {
			if action == "update" && previousImageID != "" {
				if rollbackErr := s.restorePreviousComponentImage(previousImageID, spec); rollbackErr == nil {
					s.writeState(State{State: "failed", Action: action, Component: component, Target: target, StartedAt: now, UpdatedAt: time.Now(), Message: "new image failed health check; previous image restored: " + healthErr.Error()})
					return
				} else {
					healthErr = fmt.Errorf("%v; automatic image rollback failed: %w", healthErr, rollbackErr)
				}
			}
			s.writeState(State{State: "failed", Action: action, Component: component, Target: target, StartedAt: now, UpdatedAt: time.Now(), Message: "health check failed: " + healthErr.Error()})
			return
		}
	}

	// The main container is now being recreated. The brand-new replica
	// will read this "done" state on startup and serve it to the UI that's
	// still polling in the browser.
	s.writeState(State{State: "done", Action: action, Component: component, Target: target, StartedAt: now, UpdatedAt: time.Now(), Message: "compose up -d completed", PreviousImageID: previousImageID})
}

// requireHealthyOptimizer keeps a Core image without embedded Python from
// replacing a legacy image before its optimizer sidecar works. It only reads
// the merged Compose files and running container state. In particular, it
// never rewrites an operator-owned override file.
func (s *server) requireHealthyOptimizer() error {
	spec, err := s.componentSpec("optimizer")
	if err != nil {
		return fmt.Errorf("a healthy %s service is required; add the optimizer sidecar with scripts/migrate-legacy-compose.sh or follow docs/upgrade-from-legacy.md: %w", optimizerServiceName, err)
	}
	if s.healthCheck == nil {
		return fmt.Errorf("cannot verify that %s is healthy", optimizerServiceName)
	}
	ctx, cancel := context.WithTimeout(context.Background(), componentHealthTimeout(spec.name))
	defer cancel()
	if err := s.healthCheck(ctx, spec.service); err != nil {
		return fmt.Errorf("%s must be running and healthy before Core can update: %w", optimizerServiceName, err)
	}
	return nil
}

func (s *server) runComponentRollback(component string, startedAt time.Time) {
	now := startedAt
	if now.IsZero() {
		now = time.Now()
	}
	previous := s.previousImageID(component)
	spec, err := s.componentSpec(component)
	if err != nil {
		s.writeState(State{State: "failed", Action: "component_rollback", Component: component, StartedAt: now, UpdatedAt: time.Now(), Message: err.Error(), PreviousImageID: previous})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	current, currentErr := s.imageID(ctx, spec.service)
	cancel()
	if currentErr != nil {
		s.writeState(State{State: "failed", Action: "component_rollback", Component: component, StartedAt: now, UpdatedAt: time.Now(), Message: currentErr.Error(), PreviousImageID: previous})
		return
	}
	s.writeState(State{State: "restoring", Action: "component_rollback", Component: component, StartedAt: now, UpdatedAt: time.Now(), Message: "restoring previous component image", PreviousImageID: previous})
	if err := s.restorePreviousComponentImage(previous, spec); err != nil {
		s.writeState(State{State: "failed", Action: "component_rollback", Component: component, StartedAt: now, UpdatedAt: time.Now(), Message: err.Error(), PreviousImageID: previous})
		return
	}
	s.writeState(State{State: "done", Action: "component_rollback", Component: component, StartedAt: now, UpdatedAt: time.Now(), Message: "previous component image restored", PreviousImageID: current})
}

type componentSpec struct {
	name, service, image, tagEnv, tagVariable string
}

func (s *server) componentSpec(component string) (componentSpec, error) {
	switch component {
	case "", "core":
		return componentSpec{name: "core", service: s.mainServiceName, image: canonicalMainImage, tagEnv: "FTW_IMAGE_TAG", tagVariable: "FTW_IMAGE_TAG"}, nil
	case "optimizer":
		if _, ok, err := serviceImageFromComposeFiles(s.composeFiles(), optimizerServiceName); err != nil {
			return componentSpec{}, err
		} else if !ok {
			return componentSpec{}, fmt.Errorf("compose service %q is unavailable", optimizerServiceName)
		}
		return componentSpec{name: "optimizer", service: optimizerServiceName, image: canonicalOptimizerImage, tagEnv: "FTW_OPTIMIZER_IMAGE_TAG", tagVariable: "FTW_OPTIMIZER_IMAGE_TAG"}, nil
	default:
		return componentSpec{}, fmt.Errorf("unsupported component %q", component)
	}
}

func (s *server) restorePreviousImage(imageID string) error {
	spec, err := s.componentSpec("core")
	if err != nil {
		return err
	}
	return s.restorePreviousComponentImage(imageID, spec)
}

func (s *server) restorePreviousComponentImage(imageID string, spec componentSpec) error {
	image, ok, err := serviceImageFromComposeFiles(s.composeFiles(), spec.service)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("service %q has no image", spec.service)
	}
	repository, err := composeImageRepository(image)
	if err != nil {
		return err
	}
	rollbackTag := fmt.Sprintf("ftw-rollback-%d", time.Now().Unix())
	rollbackRef := repository + ":" + rollbackTag
	timeout := 10 * time.Minute
	if spec.name == "core" {
		timeout = 35 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := s.runner(ctx, nil, "image", "tag", imageID, rollbackRef); err != nil {
		return fmt.Errorf("tag previous image: %w", err)
	}
	env := []string{spec.tagEnv + "=" + rollbackTag}
	if err := s.runner(ctx, env, s.composeArgs("up", "-d", spec.service)...); err != nil {
		return fmt.Errorf("recreate previous image: %w", err)
	}
	if s.healthCheck != nil {
		if err := s.healthCheck(ctx, spec.service); err != nil {
			return fmt.Errorf("previous image health check: %w", err)
		}
	}
	return nil
}

// prepareUpdateImagePin makes old Compose layouts safe for immutable updates.
// Older and developer installations often hard-code an image tag, so merely
// exporting FTW_IMAGE_TAG would leave the running service on the old digest.
// The project directory is deliberately mounted read-only in the updater;
// instead of rewriting user configuration, add a generated override last in
// the Compose file chain for this update only. Compose keeps the original
// project, service, volumes, networking, and every other user setting.
func (s *server) prepareUpdateImagePin() (func(), error) {
	spec, err := s.componentSpec("core")
	if err != nil {
		return func() {}, err
	}
	return s.prepareComponentImagePin(spec)
}

func (s *server) prepareComponentImagePin(spec componentSpec) (func(), error) {
	image, ok, err := serviceImageFromComposeFiles(s.composeFiles(), spec.service)
	if err != nil {
		return func() {}, err
	}
	if !ok {
		return func() {}, fmt.Errorf("service %q is missing an image entry in compose files", spec.service)
	}
	if strings.Contains(image, spec.tagVariable) {
		return func() {}, nil
	}

	type imageService struct {
		Image string `yaml:"image"`
	}
	doc := struct {
		Services map[string]imageService `yaml:"services"`
	}{
		Services: map[string]imageService{
			spec.service: {Image: spec.image + ":${" + spec.tagVariable + ":-latest}"},
		},
	}
	data, err := yaml.Marshal(doc)
	if err != nil {
		return func() {}, fmt.Errorf("build compatibility override: %w", err)
	}

	// Keep the override on the updater's private container filesystem. The
	// shared status directory must be writable by the unprivileged main
	// container; placing Compose input there would create an unlink/replace
	// race across the Docker-socket trust boundary.
	f, err := os.CreateTemp("", "ftw-compose-update-*.yml")
	if err != nil {
		return func() {}, fmt.Errorf("write compatibility override: %w", err)
	}
	path := f.Name()
	removeOnError := func() {
		_ = f.Close()
		_ = os.Remove(path)
	}
	if _, err := f.Write(data); err != nil {
		removeOnError()
		return func() {}, fmt.Errorf("write compatibility override: %w", err)
	}
	if err := f.Sync(); err != nil {
		removeOnError()
		return func() {}, fmt.Errorf("sync compatibility override: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return func() {}, fmt.Errorf("close compatibility override: %w", err)
	}
	s.updateOverrideFile = path
	slog.Warn("using transient canonical image override for legacy compose", "service", spec.service, "previous_image", image)

	cleanup := func() {
		s.updateOverrideFile = ""
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("remove compatibility override", "path", path, "err", err)
		}
	}
	return cleanup, nil
}

func composeImageRepository(image string) (string, error) {
	if i := strings.Index(image, ":${"); i > 0 {
		return image[:i], nil
	}
	return "", fmt.Errorf("image %q does not use a supported repository:${*_IMAGE_TAG} form", image)
}

// validateComposeImagePin catches old host-side compose files that hard-code
// :latest. In that layout FTW_IMAGE_TAG is passed to docker compose but never
// read, so the sidecar can report "done" while the main service stays on the
// previous image.
func (s *server) validateComposeImagePin() error {
	spec, err := s.componentSpec("core")
	if err != nil {
		return err
	}
	return s.validateComponentImagePin(spec)
}

func (s *server) validateComponentImagePin(spec componentSpec) error {
	image, ok, err := serviceImageFromComposeFiles(s.composeFiles(), spec.service)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("service %q is missing an image entry in compose files", spec.service)
	}
	if !strings.Contains(image, spec.tagVariable) {
		return fmt.Errorf("service %q image %q does not reference %s", spec.service, image, spec.tagVariable)
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
//  1. docker stop <container> — main releases its SQLite handle so
//     the swap doesn't write into a live DB.
//  2. docker cp each file out of the stopped container's /app/data mount,
//     prepare it in a private temporary file, then archive-copy it back to
//     /app/data. Going through the container makes named volumes and custom
//     host-bind locations behave exactly like the default ./data bind.
//  3. docker start <container> — the same container/image comes up on the
//     restored state.
//
// Any restore or health failure automatically restores the mandatory safety
// backup captured immediately before rollback and starts the original state.
func (s *server) runRollback(snapshotID string, files []string, safetySnapshotID string, safetyFiles []string) {
	now := time.Now()
	base := State{
		Action: "rollback", Snapshot: snapshotID, Files: append([]string{}, files...),
		SafetySnapshot: safetySnapshotID, SafetyFiles: append([]string{}, safetyFiles...), StartedAt: now,
	}
	initial := base
	initial.State = "restoring"
	initial.UpdatedAt = now
	initial.Message = "stopping main service"
	s.writeState(initial)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	if s.imageID == nil {
		s.writeState(State{State: "failed", Action: base.Action, Snapshot: base.Snapshot, StartedAt: now, UpdatedAt: time.Now(), Message: "cannot preserve current image: image inspection unavailable"})
		return
	}
	if s.containerID == nil {
		s.writeState(State{State: "failed", Action: base.Action, Snapshot: base.Snapshot, StartedAt: now, UpdatedAt: time.Now(), Message: "cannot locate main container: container inspection unavailable"})
		return
	}
	containerID, err := s.containerID(ctx, s.mainServiceName)
	if err != nil {
		s.writeState(State{State: "failed", Action: base.Action, Snapshot: base.Snapshot, StartedAt: now, UpdatedAt: time.Now(), Message: "cannot locate main container: " + err.Error()})
		return
	}
	if s.checkSnapshotFile == nil || s.stageSnapshotFile == nil {
		s.writeState(State{State: "failed", Action: base.Action, Snapshot: base.Snapshot, StartedAt: now, UpdatedAt: time.Now(), Message: "snapshot access unavailable"})
		return
	}
	preflight := []struct {
		label string
		id    string
		files []string
	}{{"snapshot", snapshotID, files}, {"safety snapshot", safetySnapshotID, safetyFiles}}
	for _, backup := range preflight {
		for _, file := range backup.files {
			if err := s.checkSnapshotFile(ctx, containerID, backup.id, file); err != nil {
				s.writeState(State{State: "failed", Action: base.Action, Snapshot: base.Snapshot, StartedAt: now, UpdatedAt: time.Now(), Message: backup.label + " file " + file + " not readable: " + err.Error()})
				return
			}
		}
	}
	imageID, err := s.imageID(ctx, s.mainServiceName)
	if err != nil {
		s.writeState(State{State: "failed", Action: base.Action, Snapshot: base.Snapshot, StartedAt: now, UpdatedAt: time.Now(), Message: "cannot preserve current image: " + err.Error()})
		return
	}

	// 1. Stop the main service so SQLite isn't holding a file handle
	// while we swap state.db under it.
	if err := s.runner(ctx, nil, "stop", "--time", "30", containerID); err != nil {
		s.writeState(State{State: "failed", Action: base.Action, Snapshot: base.Snapshot, StartedAt: now, UpdatedAt: time.Now(), Message: "container stop failed: " + err.Error()})
		return
	}

	s.writeState(State{State: "restoring", Action: base.Action, Snapshot: base.Snapshot, StartedAt: now, UpdatedAt: time.Now(), Message: "copying snapshot files"})
	if err := s.restoreSnapshotFiles(ctx, snapshotID, files, containerID, imageID); err != nil {
		s.recoverRollbackSafety(ctx, base, safetySnapshotID, safetyFiles, containerID, imageID, "restore failed: "+err.Error())
		return
	}

	// 3. Start the exact stopped container again. This guarantees a state-only
	// rollback cannot silently move the application to another image tag.
	s.writeState(State{State: "restarting", Action: base.Action, Snapshot: base.Snapshot, StartedAt: now, UpdatedAt: time.Now(), Message: "starting main service on restored state"})
	if err := s.runner(ctx, nil, "start", containerID); err != nil {
		s.recoverRollbackSafety(ctx, base, safetySnapshotID, safetyFiles, containerID, imageID, "container start failed: "+err.Error())
		return
	}
	if s.healthCheck != nil {
		healthErr := s.healthCheck(ctx, s.mainServiceName)
		if healthErr != nil {
			s.recoverRollbackSafety(ctx, base, safetySnapshotID, safetyFiles, containerID, imageID, "restored service failed health check: "+healthErr.Error())
			return
		}
	}
	s.writeState(State{State: "done", Action: base.Action, Snapshot: base.Snapshot, StartedAt: now, UpdatedAt: time.Now(), Message: "rollback complete"})
}

func validRollbackFile(file string) bool {
	return file == "state.db.gz" || file == "config.yaml"
}

func validSnapshotID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, "/\\")
}

func hasRollbackState(files []string) bool {
	for _, file := range files {
		if file == "state.db.gz" {
			return true
		}
	}
	return false
}

// restoreSnapshotFiles copies only allowlisted backup files into the stopped
// container. docker cp defaults container-side ownership to root, which made
// the uid-100 FTW process unable to write a restored database. Archive mode
// preserves uid/gid; compressed databases are materialised with uid 100:101
// first so the same rule applies.
func (s *server) restoreSnapshotFiles(ctx context.Context, snapshotID string, files []string, containerID, imageRef string) error {
	for _, file := range files {
		if !validRollbackFile(file) {
			return fmt.Errorf("unsupported rollback file %q", file)
		}
		staged, err := os.CreateTemp("", "ftw-rollback-source-*")
		if err != nil {
			return fmt.Errorf("create snapshot staging file: %w", err)
		}
		stagedPath := staged.Name()
		_ = staged.Close()
		_ = os.Remove(stagedPath)
		defer os.Remove(stagedPath)
		if s.stageSnapshotFile == nil {
			return errors.New("snapshot staging unavailable")
		}
		if err := s.stageSnapshotFile(ctx, containerID, snapshotID, file, stagedPath); err != nil {
			return fmt.Errorf("stage %s: %w", file, err)
		}
		copySrc := stagedPath
		dstName := file
		if file == "state.db.gz" {
			dstName = "state.db"
			tmp, err := os.CreateTemp("", "ftw-rollback-state-*.db")
			if err != nil {
				return fmt.Errorf("create rollback temp: %w", err)
			}
			tmpPath := tmp.Name()
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
			defer os.Remove(tmpPath)
			if err := decompressGzipFile(stagedPath, tmpPath); err != nil {
				return fmt.Errorf("decompress state.db.gz: %w", err)
			}
			if s.chownFile == nil {
				return errors.New("set restored database owner: chown unavailable")
			}
			if err := s.chownFile(tmpPath, 100, 101); err != nil {
				return fmt.Errorf("set restored database owner: %w", err)
			}
			if err := os.Chmod(tmpPath, 0o600); err != nil {
				return fmt.Errorf("set restored database mode: %w", err)
			}
			copySrc = tmpPath
		}
		if err := s.runner(ctx, nil, "cp", "-a", copySrc, containerID+":/app/data/"+dstName); err != nil {
			return fmt.Errorf("docker cp %s: %w", file, err)
		}
	}
	// A clean stop normally removes SQLite's WAL sidecars. Remove any stale
	// remnants explicitly so pages from the pre-rollback database cannot be
	// replayed over the restored main file.
	if err := s.runner(ctx, nil, "run", "--rm", "--network", "none", "--user", "0:0", "--volumes-from", containerID, "--entrypoint", "rm", imageRef, "-f", "/app/data/state.db-wal", "/app/data/state.db-shm"); err != nil {
		return fmt.Errorf("remove stale SQLite WAL files: %w", err)
	}
	return nil
}

func decompressGzipFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	zr, err := gzip.NewReader(in)
	if err != nil {
		return err
	}
	defer zr.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		_ = out.Close()
		if !committed {
			_ = os.Remove(dst)
		}
	}()
	if _, err := io.Copy(out, zr); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	committed = true
	return nil
}

func (s *server) recoverRollbackSafety(ctx context.Context, base State, safetySnapshotID string, safetyFiles []string, containerID, imageRef, cause string) {
	s.writeState(State{State: "restoring", Action: base.Action, Snapshot: base.Snapshot, StartedAt: base.StartedAt, UpdatedAt: time.Now(), Message: "rollback failed; restoring pre-rollback safety backup"})
	_ = s.runner(ctx, nil, "stop", "--time", "30", containerID)
	restoreErr := s.restoreSnapshotFiles(ctx, safetySnapshotID, safetyFiles, containerID, imageRef)
	var startErr error
	if restoreErr == nil {
		startErr = s.runner(ctx, nil, "start", containerID)
	}
	if restoreErr == nil && startErr == nil && s.healthCheck != nil {
		startErr = s.healthCheck(ctx, s.mainServiceName)
	}
	message := cause
	if restoreErr != nil {
		message += "; safety backup restore failed: " + restoreErr.Error()
	} else if startErr != nil {
		message += "; safety backup restored but service recovery failed: " + startErr.Error()
	} else {
		message += "; pre-rollback state restored and service recovered"
	}
	s.writeState(State{State: "failed", Action: base.Action, Snapshot: base.Snapshot, StartedAt: base.StartedAt, UpdatedAt: time.Now(), Message: message})
}

func (s *server) writeState(st State) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if st.UpdatedAt.IsZero() {
		st.UpdatedAt = time.Now()
	}
	// Rollback history belongs to the component, not to whichever update job
	// happened most recently. Preserve it across all state transitions and
	// migrate the original single previous_image_id field on first write.
	previousImages := make(map[string]string)
	if data, err := os.ReadFile(s.statusPath); err == nil {
		var previous State
		if json.Unmarshal(data, &previous) == nil {
			if st.Action == "rollback" && previous.Action == "rollback" && st.Snapshot == previous.Snapshot {
				if len(st.Files) == 0 {
					st.Files = append([]string{}, previous.Files...)
				}
				if st.SafetySnapshot == "" {
					st.SafetySnapshot = previous.SafetySnapshot
				}
				if len(st.SafetyFiles) == 0 {
					st.SafetyFiles = append([]string{}, previous.SafetyFiles...)
				}
			}
			for component, imageID := range previous.PreviousImages {
				previousImages[component] = imageID
			}
			if previous.Component != "" && previous.PreviousImageID != "" && previousImages[previous.Component] == "" {
				previousImages[previous.Component] = previous.PreviousImageID
			}
		}
	}
	for component, imageID := range st.PreviousImages {
		previousImages[component] = imageID
	}
	if st.Component != "" && st.PreviousImageID != "" {
		previousImages[st.Component] = st.PreviousImageID
	}
	if len(previousImages) > 0 {
		st.PreviousImages = previousImages
		if st.PreviousImageID == "" && st.Component != "" {
			st.PreviousImageID = previousImages[st.Component]
		}
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

func (s *server) previousImageID(component string) string {
	st := s.readState()
	if imageID := st.PreviousImages[component]; imageID != "" {
		return imageID
	}
	// Backward compatibility with state files written before per-component
	// history was introduced.
	if st.Component == component {
		return st.PreviousImageID
	}
	return ""
}

// recoverCrashedState runs once at boot. Any in-flight state belongs to the
// updater process that just died; waiting for a stale timeout would leave the
// UI stuck forever. Rollback carries its safety backup in state.json, so it
// can restore the pre-rollback data before reporting failure.
func (s *server) recoverCrashedState() {
	st := s.readState()
	if st.State != "pulling" && st.State != "restarting" && st.State != "restoring" {
		return
	}
	prev := st.State
	if st.Action == "rollback" && validSnapshotID(st.SafetySnapshot) && hasRollbackState(st.SafetyFiles) && rollbackFilesValid(st.SafetyFiles) && s.containerID != nil && s.imageID != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		containerID, containerErr := s.containerID(ctx, s.mainServiceName)
		imageID, imageErr := s.imageID(ctx, s.mainServiceName)
		if containerErr == nil && imageErr == nil {
			s.recoverRollbackSafety(ctx, st, st.SafetySnapshot, st.SafetyFiles, containerID, imageID, "updater restarted during rollback")
			slog.Warn("recovered interrupted rollback from safety backup", "prev_state", prev)
			return
		}
		st.Message = fmt.Sprintf("updater restarted during rollback; safety recovery unavailable: container=%v image=%v", containerErr, imageErr)
	} else if st.Message == "" {
		st.Message = "updater process restarted while in-flight"
	}
	st.State = "failed"
	st.UpdatedAt = time.Now()
	s.writeState(st)
	slog.Warn("recovered in-flight state as failed", "prev_state", prev)
}

func rollbackFilesValid(files []string) bool {
	for _, file := range files {
		if !validRollbackFile(file) {
			return false
		}
	}
	return true
}

func (s *server) serviceContainerID(ctx context.Context, service string) (string, error) {
	out, err := dockerOutput(ctx, s.composeArgs("ps", "-q", "--all", service)...)
	if err != nil {
		return "", err
	}
	ids := strings.Fields(out)
	if len(ids) != 1 {
		return "", fmt.Errorf("service %q resolved to %d containers", service, len(ids))
	}
	return ids[0], nil
}

func (s *server) currentServiceImageID(ctx context.Context, service string) (string, error) {
	containerID, err := s.serviceContainerID(ctx, service)
	if err != nil {
		return "", err
	}
	out, err := dockerOutput(ctx, "inspect", "--format", "{{.Image}}", containerID)
	if err != nil {
		return "", err
	}
	imageID := strings.TrimSpace(out)
	if imageID == "" {
		return "", errors.New("running container has no image ID")
	}
	return imageID, nil
}

func (s *server) waitForServiceHealth(ctx context.Context, service string) error {
	containerID, err := s.serviceContainerID(ctx, service)
	if err != nil {
		return err
	}
	for {
		out, inspectErr := dockerOutput(ctx, "inspect", "--format", "{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}", containerID)
		if inspectErr == nil {
			switch status := strings.TrimSpace(out); status {
			case "healthy", "running":
				if service != s.mainServiceName {
					return nil
				}
				// Core intentionally answers /api/health while opening or
				// migrating a large database so Docker does not kill valid slow
				// boots. /api/status is served only by the fully wired API, so
				// require it before committing an update or restored state.
				if _, readyErr := dockerOutput(ctx, "exec", containerID, "wget", "-q", "-T", "4", "-O", "/dev/null", "http://127.0.0.1:8080/api/status"); readyErr == nil {
					return nil
				}
				restarts, _ := dockerOutput(ctx, "inspect", "--format", "{{.RestartCount}}", containerID)
				if count, parseErr := strconv.Atoi(strings.TrimSpace(restarts)); parseErr == nil && count > 0 {
					return fmt.Errorf("core restarted %d time(s) before its API became ready", count)
				}
			case "unhealthy", "exited", "dead":
				return fmt.Errorf("container status is %s", status)
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for service health: %w", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

func componentHealthTimeout(component string) time.Duration {
	if component == "core" {
		return 30 * time.Minute
	}
	return 3 * time.Minute
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

func dockerOutput(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, truncate(string(out), 400))
	}
	return string(out), nil
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

// isImmutableImageTag accepts only tags produced by the stable and beta
// publishers. Moving aliases, arbitrary prereleases, metadata, and shell-shaped
// input are rejected at the Docker boundary.
func isImmutableImageTag(s string) bool {
	if !strings.HasPrefix(s, "v") {
		return false
	}
	version := s[1:]
	prerelease := ""
	if i := strings.IndexByte(version, '-'); i >= 0 {
		prerelease = version[i+1:]
		version = version[:i]
	}
	if strings.ContainsAny(version, "+-") {
		return false
	}
	parts := strings.Split(version, ".")
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
	if prerelease == "" {
		return true
	}
	pre := strings.Split(prerelease, ".")
	return len(pre) == 2 && pre[0] == "beta" && allDigits(pre[1])
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
