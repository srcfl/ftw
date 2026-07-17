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
	"context"
	"encoding/json"
	"errors"
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
	imageID     func(ctx context.Context, service string) (string, error)
	healthCheck func(ctx context.Context, service string) error
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
	srv.healthCheck = srv.waitForServiceHealth
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
		Action    string   `json:"action"`
		Component string   `json:"component,omitempty"`
		Target    string   `json:"target,omitempty"`
		Snapshot  string   `json:"snapshot,omitempty"` // rollback-only (#152)
		Files     []string `json:"files,omitempty"`    // rollback: basenames to restore
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
			s.runRollback(body.Snapshot, body.Files)
		} else if body.Action == "component_rollback" {
			s.runComponentRollback(body.Component)
		} else {
			s.runComponentJob(body.Action, body.Target, body.Component)
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
	s.runComponentJob(action, target, "core")
}

func (s *server) runComponentJob(action, target, component string) {
	now := time.Now()
	spec, err := s.componentSpec(component)
	if err != nil {
		s.writeState(State{State: "failed", Action: action, Component: component, Target: target, StartedAt: now, UpdatedAt: now, Message: err.Error()})
		return
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
		healthCtx, cancelHealth := context.WithTimeout(context.Background(), 3*time.Minute)
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

func (s *server) runComponentRollback(component string) {
	now := time.Now()
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
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
//  1. compose stop <main-service> — main releases its SQLite handle so
//     the swap doesn't write into a live DB.
//  2. docker cp each file from the snapshot dir on the host into the
//     stopped container. Bind-mount semantics make this a direct write
//     to the host-side data dir; no read-only/writable bind trick needed.
//  3. compose up -d <main-service> — main comes up on the restored state.
//
// Paths: the snapshots live at <host_project_dir>/data/snapshots/<id>.
// We compute host_project_dir from FTW_UPDATER_COMPOSE (an absolute host
// path already, documented in docker-compose.yml). Operators who move
// the data bind off the default `./data:/app/data` layout would need a
// separate override; tracked as a follow-up if we hit that in practice.
//
// A failed file restore leaves state: "failed" with a descriptive message
// and attempts compose up -d before returning so the main container is not
// stranded offline. A failed start remains visible as a failed state.
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

	// A state rollback must not silently change the application version. Pin
	// the exact image object that backs the running container to a temporary
	// tag, then use that tag for the recreate below. This also makes rollback
	// safe for legacy Compose files that hard-code an obsolete local tag.
	cleanup, err := s.prepareUpdateImagePin()
	if err != nil {
		s.writeState(State{State: "failed", Action: base.Action, Snapshot: base.Snapshot, StartedAt: now, UpdatedAt: time.Now(), Message: "compose preflight failed: " + err.Error()})
		return
	}
	defer cleanup()
	if err := s.validateComposeImagePin(); err != nil {
		s.writeState(State{State: "failed", Action: base.Action, Snapshot: base.Snapshot, StartedAt: now, UpdatedAt: time.Now(), Message: "compose preflight failed: " + err.Error()})
		return
	}
	if s.imageID == nil {
		s.writeState(State{State: "failed", Action: base.Action, Snapshot: base.Snapshot, StartedAt: now, UpdatedAt: time.Now(), Message: "cannot preserve current image: image inspection unavailable"})
		return
	}
	imageID, err := s.imageID(ctx, s.mainServiceName)
	if err != nil {
		s.writeState(State{State: "failed", Action: base.Action, Snapshot: base.Snapshot, StartedAt: now, UpdatedAt: time.Now(), Message: "cannot preserve current image: " + err.Error()})
		return
	}
	image, ok, err := serviceImageFromComposeFiles(s.composeFiles(), s.mainServiceName)
	if err != nil || !ok {
		if err == nil {
			err = fmt.Errorf("service %q has no image", s.mainServiceName)
		}
		s.writeState(State{State: "failed", Action: base.Action, Snapshot: base.Snapshot, StartedAt: now, UpdatedAt: time.Now(), Message: "cannot preserve current image: " + err.Error()})
		return
	}
	repository, err := composeImageRepository(image)
	if err != nil {
		s.writeState(State{State: "failed", Action: base.Action, Snapshot: base.Snapshot, StartedAt: now, UpdatedAt: time.Now(), Message: "cannot preserve current image: " + err.Error()})
		return
	}
	rollbackTag := fmt.Sprintf("ftw-state-rollback-%d", time.Now().UnixNano())
	rollbackRef := repository + ":" + rollbackTag
	if err := s.runner(ctx, nil, "image", "tag", imageID, rollbackRef); err != nil {
		s.writeState(State{State: "failed", Action: base.Action, Snapshot: base.Snapshot, StartedAt: now, UpdatedAt: time.Now(), Message: "cannot preserve current image: " + err.Error()})
		return
	}
	env := []string{"FTW_IMAGE_TAG=" + rollbackTag}

	// 1. Stop the main service so SQLite isn't holding a file handle
	// while we swap state.db under it.
	stopArgs := s.composeArgs("stop", s.mainServiceName)
	if err := s.runner(ctx, env, stopArgs...); err != nil {
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
		cpArgs := []string{"cp", src, s.mainServiceName + ":" + dst}
		if err := s.runner(ctx, nil, cpArgs...); err != nil {
			// File-swap failure is serious. Try to bring the
			// service back anyway so the operator isn't stranded.
			slog.Error("rollback docker cp failed", "file", f, "err", err)
			s.writeState(State{State: "failed", Action: base.Action, Snapshot: base.Snapshot, StartedAt: now, UpdatedAt: time.Now(), Message: "docker cp " + f + " failed: " + err.Error()})
			_ = s.runner(ctx, env, s.composeArgs("up", "-d", s.mainServiceName)...)
			return
		}
	}

	// 3. Start the main service again. --force-recreate so the new
	// process certainly sees the swapped files (same semantics as the
	// restart flow).
	s.writeState(State{State: "restarting", Action: base.Action, Snapshot: base.Snapshot, StartedAt: now, UpdatedAt: time.Now(), Message: "starting main service on restored state"})
	upArgs := s.composeArgs("up", "-d", "--force-recreate", s.mainServiceName)
	if err := s.runner(ctx, env, upArgs...); err != nil {
		s.writeState(State{State: "failed", Action: base.Action, Snapshot: base.Snapshot, StartedAt: now, UpdatedAt: time.Now(), Message: "compose up -d failed: " + err.Error()})
		return
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := s.runner(cleanupCtx, nil, "image", "rm", rollbackRef); err != nil {
		slog.Warn("rollback: remove temporary image tag", "image", rollbackRef, "err", err)
	}
	cleanupCancel()
	s.writeState(State{State: "done", Action: base.Action, Snapshot: base.Snapshot, StartedAt: now, UpdatedAt: time.Now(), Message: "rollback complete"})
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

func (s *server) serviceContainerID(ctx context.Context, service string) (string, error) {
	out, err := dockerOutput(ctx, s.composeArgs("ps", "-q", service)...)
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
				return nil
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
