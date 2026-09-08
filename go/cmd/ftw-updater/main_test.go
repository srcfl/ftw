package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// fakeRunner records the compose commands the server attempted to run so
// tests can assert on arg order without touching docker. envs[i] is the
// extra env passed alongside calls[i].
type fakeRunner struct {
	mu     sync.Mutex
	calls  [][]string
	envs   [][]string
	fail   bool
	failOn string
}

func (f *fakeRunner) run(ctx context.Context, env []string, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{}, args...))
	f.envs = append(f.envs, append([]string{}, env...))
	if f.fail {
		return errors.New("forced failure")
	}
	if f.failOn != "" {
		for _, a := range args {
			if a == f.failOn {
				return errors.New("forced failure on " + f.failOn)
			}
		}
	}
	return nil
}

func (f *fakeRunner) snapshot() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeRunner) envSnapshot() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.envs))
	copy(out, f.envs)
	return out
}

func newTestServer(t *testing.T) (*server, *fakeRunner) {
	t.Helper()
	dir := t.TempDir()
	runner := &fakeRunner{}
	s := &server{
		composeFile:     filepath.Join(dir, "docker-compose.yml"),
		mainServiceName: canonicalMainServiceName,
		statusPath:      filepath.Join(dir, "state.json"),
		pullRetryDelay:  time.Millisecond,
		runner:          runner.run,
		healthCheck:     func(context.Context, string) error { return nil },
		imageID:         func(context.Context, string) (string, error) { return "sha256:current", nil },
		containerID:     func(context.Context, string) (string, error) { return "ftw-container", nil },
		chownFile:       func(string, int, int) error { return nil },
	}
	s.checkSnapshotFile = func(_ context.Context, _ string, snapshotID, file string) error {
		_, err := os.Stat(filepath.Join(dir, "data", "snapshots", snapshotID, file))
		return err
	}
	s.stageSnapshotFile = func(_ context.Context, _ string, snapshotID, file, dst string) error {
		data, err := os.ReadFile(filepath.Join(dir, "data", "snapshots", snapshotID, file))
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o600)
	}
	writeCompose(t, s.composeFile, `services:
  ftw:
    image: ghcr.io/srcfl/ftw:${FTW_IMAGE_TAG:-latest}
    volumes:
      - ./data:/app/data
  ftw-optimizer:
    image: ghcr.io/srcfl/ftw-optimizer:${FTW_OPTIMIZER_IMAGE_TAG:-latest}
`)
	return s, runner
}

func writeCompose(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSkipPull_BypassesPullStep(t *testing.T) {
	s, runner := newTestServer(t)
	s.skipPull = true

	req := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(`{"action":"update","target":"v1.2.3"}`))
	rr := httptest.NewRecorder()
	s.handleUpdate(rr, req)
	done := waitForState(t, s, "done")
	if done.Step != 4 || done.TotalSteps != 4 || done.PhaseStartedAt.IsZero() {
		t.Fatalf("done progress = %+v", done)
	}
	calls := runner.snapshot()
	if len(calls) != 1 {
		t.Fatalf("skip-pull should yield 1 call (up only), got %d: %v", len(calls), calls)
	}
	if !strings.Contains(strings.Join(calls[0], " "), "up -d") {
		t.Errorf("single call should be `up -d`: %v", calls[0])
	}
}

func TestRunWithStateHeartbeatRefreshesLongPhase(t *testing.T) {
	s, _ := newTestServer(t)
	s.statusHeartbeatInterval = 5 * time.Millisecond
	started := time.Now()
	initial := State{
		State: "pulling", Action: "update", Component: "core", Target: "v1.2.3",
		StartedAt: started, PhaseStartedAt: started, UpdatedAt: started,
		Message: "Downloading pinned release image", Step: 2, TotalSteps: 4,
	}
	if err := s.runWithStateHeartbeat(initial, func() error {
		time.Sleep(18 * time.Millisecond)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got := s.readState()
	if !got.UpdatedAt.After(started) || got.State != "pulling" || got.Step != 2 || got.TotalSteps != 4 {
		t.Fatalf("heartbeat state = %+v", got)
	}
}

func TestComponentRollbackHistorySurvivesOtherComponentUpdates(t *testing.T) {
	s, _ := newTestServer(t)
	s.writeState(State{
		State: "done", Component: "optimizer", PreviousImageID: "sha256:optimizer-old",
	})
	s.writeState(State{
		State: "done", Component: "core", PreviousImageID: "sha256:core-old",
	})

	state := s.readState()
	if got := state.PreviousImages["optimizer"]; got != "sha256:optimizer-old" {
		t.Fatalf("optimizer history = %q", got)
	}
	if got := state.PreviousImages["core"]; got != "sha256:core-old" {
		t.Fatalf("core history = %q", got)
	}
	if got := s.previousImageID("optimizer"); got != "sha256:optimizer-old" {
		t.Fatalf("optimizer rollback image = %q", got)
	}
}

func waitForState(t *testing.T, s *server, want string) State {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st := s.readState(); st.State == want {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("state never reached %q (last=%+v)", want, s.readState())
	return State{}
}

func TestHandleUpdate_HappyPath(t *testing.T) {
	s, runner := newTestServer(t)
	body := bytes.NewBufferString(`{"action":"update","target":"v1.2.3"}`)
	req := httptest.NewRequest(http.MethodPost, "/update", body)
	rr := httptest.NewRecorder()
	s.handleUpdate(rr, req)
	if rr.Code != 202 {
		t.Fatalf("status = %d", rr.Code)
	}
	st := waitForState(t, s, "done")
	if st.Action != "update" || st.Target != "v1.2.3" {
		t.Errorf("unexpected final state: %+v", st)
	}
	calls := runner.snapshot()
	if len(calls) != 2 {
		t.Fatalf("expected 2 docker calls, got %d: %v", len(calls), calls)
	}
	if calls[0][0] != "compose" || !strings.Contains(strings.Join(calls[0], " "), "pull") {
		t.Errorf("first call should be pull: %v", calls[0])
	}
	up := strings.Join(calls[1], " ")
	if !strings.Contains(up, "up -d") || strings.Contains(up, "--force-recreate") {
		t.Errorf("update path should NOT force-recreate: %v", calls[1])
	}
}

func TestHandleUpdate_WithoutOptimizerPreservesUserOverride(t *testing.T) {
	s, _ := newTestServer(t)
	writeCompose(t, s.composeFile, `services:
  ftw:
    image: ghcr.io/srcfl/ftw:${FTW_IMAGE_TAG:-latest}
    volumes:
      - ./data:/app/data
`)
	override := filepath.Join(filepath.Dir(s.composeFile), "docker-compose.override.yml")
	original := []byte("services:\n  ftw:\n    environment:\n      OPERATOR_SETTING: preserved\n")
	if err := os.WriteFile(override, original, 0o644); err != nil {
		t.Fatal(err)
	}
	s.overrideFiles = []string{override}

	req := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(`{"action":"update","target":"v1.2.3"}`))
	rr := httptest.NewRecorder()
	s.handleUpdate(rr, req)
	waitForState(t, s, "done")
	got, err := os.ReadFile(override)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("operator override changed:\n%s", got)
	}
}

func TestHandleUpdate_RestartDoesNotRecreate(t *testing.T) {
	s, runner := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(`{"action":"restart"}`))
	rr := httptest.NewRecorder()
	s.handleUpdate(rr, req)
	if rr.Code != 202 {
		t.Fatalf("status = %d", rr.Code)
	}
	waitForState(t, s, "done")
	calls := runner.snapshot()
	if len(calls) != 1 || strings.Join(calls[0], " ") != strings.Join(s.composeArgs("restart", "--no-deps", s.mainServiceName), " ") {
		t.Fatalf("restart must only restart the existing container: %v", calls)
	}
}

func TestHandleUpdate_PullFailure(t *testing.T) {
	s, runner := newTestServer(t)
	runner.fail = true
	s.maxPullAttempts = 3 // cap retries so the always-fail runner doesn't loop forever

	req := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(`{"action":"update","target":"v1.2.3"}`))
	rr := httptest.NewRecorder()
	s.handleUpdate(rr, req)
	st := waitForState(t, s, "failed")
	if !strings.Contains(st.Message, "pull") {
		t.Errorf("failure should mention pull: %+v", st)
	}
}

func TestHandleUpdate_UpFailure(t *testing.T) {
	s, runner := newTestServer(t)
	runner.failOn = "up"

	req := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(`{"action":"update","target":"v1.2.3"}`))
	rr := httptest.NewRecorder()
	s.handleUpdate(rr, req)
	st := waitForState(t, s, "failed")
	if !strings.Contains(st.Message, "up") {
		t.Errorf("failure should mention up: %+v", st)
	}
}

// Update with no target version is rejected at the boundary — the
// sidecar refuses to pull `:latest`, which is what re-introduced the
// race in the first place.
func TestHandleUpdate_RequiresTarget(t *testing.T) {
	s, _ := newTestServer(t)
	for _, body := range []string{
		`{"action":"update"}`,
		`{"action":"update","target":""}`,
		`{"action":"update","target":"latest"}`,
		`{"action":"update","target":"beta"}`,
		`{"action":"update","target":"edge"}`,
		`{"action":"update","target":"v1.2"}`,
		`{"action":"update","target":"v1.2.3-rc1"}`,
		`{"action":"update","target":"v1.2.3 ; rm -rf /"}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(body))
		rr := httptest.NewRecorder()
		s.handleUpdate(rr, req)
		if rr.Code != 400 {
			t.Errorf("body %s: want 400, got %d (%s)", body, rr.Code, rr.Body.String())
		}
	}
}

func TestImmutableImageTagChannels(t *testing.T) {
	for _, tc := range []struct {
		tag  string
		want bool
	}{
		{"v1.2.3", true},
		{"v1.3.0-beta.1", true},
		{"edge-20260712T120000Z-a48529b", false},
		{"latest", false},
		{"beta", false},
		{"edge", false},
		{"v1.3.0-rc.1", false},
		{"v1.3.0-beta", false},
		{"v1.3.0-beta.x", false},
		{"edge-20260712T120000Z-nothexz", false},
		{"edge-not-a-time-a48529b", false},
		{"v1.2.3+meta", false},
	} {
		if got := isImmutableImageTag(tc.tag); got != tc.want {
			t.Errorf("isImmutableImageTag(%q) = %v, want %v", tc.tag, got, tc.want)
		}
	}
}

// The whole point of this redesign: `update` must pin compose to the
// requested version via FTW_IMAGE_TAG, so `docker compose pull` fetches
// the exact image and is immune to the :latest-retag race.
func TestHandleUpdate_PinsImageTagViaEnv(t *testing.T) {
	s, runner := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(`{"action":"update","target":"v0.44.0"}`))
	rr := httptest.NewRecorder()
	s.handleUpdate(rr, req)
	if rr.Code != 202 {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	waitForState(t, s, "done")
	envs := runner.envSnapshot()
	if len(envs) != 2 {
		t.Fatalf("expected 2 docker calls (pull, up); got %d", len(envs))
	}
	for i, env := range envs {
		found := false
		for _, e := range env {
			if e == "FTW_IMAGE_TAG=v0.44.0" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("call %d missing FTW_IMAGE_TAG=v0.44.0; env=%v", i, env)
		}
	}
}

func TestHandleUpdate_MigratesHardcodedImageWithTransientOverride(t *testing.T) {
	s, runner := newTestServer(t)
	writeCompose(t, s.composeFile, `services:
  forty-two-watts:
    image: forty-two-watts:optimizer-champion-recourse-b10acacd
    volumes:
      - ./data:/app/data
  ftw-optimizer:
    image: ghcr.io/srcfl/ftw-optimizer:${FTW_OPTIMIZER_IMAGE_TAG:-latest}
`)
	s.mainServiceName = legacyMainServiceName

	req := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(`{"action":"update","target":"v0.44.0"}`))
	rr := httptest.NewRecorder()
	s.handleUpdate(rr, req)
	if rr.Code != 202 {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	waitForState(t, s, "done")
	calls := runner.snapshot()
	if len(calls) != 2 {
		t.Fatalf("want pull + up, got %v", calls)
	}
	for _, call := range calls {
		joined := strings.Join(call, " ")
		if !strings.Contains(joined, "-f "+s.composeFile+" -f ") || !strings.Contains(joined, "ftw-compose-update-") {
			t.Fatalf("legacy update must append compatibility override after base file: %v", call)
		}
		if call[len(call)-1] != legacyMainServiceName {
			t.Fatalf("legacy service identity must be preserved: %v", call)
		}
	}
}

func TestHandleUpdate_RestartPreservesHardcodedImage(t *testing.T) {
	s, runner := newTestServer(t)
	writeCompose(t, s.composeFile, `services:
  forty-two-watts:
    image: forty-two-watts:optimizer-champion-recourse-b10acacd
    volumes:
      - ./data:/app/data
  ftw-optimizer:
    image: ghcr.io/srcfl/ftw-optimizer:${FTW_OPTIMIZER_IMAGE_TAG:-latest}
`)
	s.mainServiceName = legacyMainServiceName

	req := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(`{"action":"restart"}`))
	rr := httptest.NewRecorder()
	s.handleUpdate(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	waitForState(t, s, "done")
	for _, call := range runner.snapshot() {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "ftw-compose-update-") || !strings.Contains(joined, "restart --no-deps") {
			t.Fatalf("legacy restart must keep its container without a migration override: %v", call)
		}
		if call[len(call)-1] != legacyMainServiceName {
			t.Fatalf("legacy service identity must be preserved: %v", call)
		}
	}
}

func TestPrepareUpdateImagePin_LeavesHostComposeUntouched(t *testing.T) {
	s, _ := newTestServer(t)
	original := `name: forty-two-watts
services:
  forty-two-watts:
    image: ghcr.io/frahlg/forty-two-watts:latest
    volumes:
      - ./data:/app/data
`
	writeCompose(t, s.composeFile, original)
	s.mainServiceName = legacyMainServiceName

	cleanup, err := s.prepareUpdateImagePin()
	if err != nil {
		t.Fatal(err)
	}
	if s.updateOverrideFile == "" {
		t.Fatal("hard-coded image should create an updater-owned override")
	}
	image, ok, err := serviceImageFromComposeFiles(s.composeFiles(), legacyMainServiceName)
	if err != nil || !ok {
		t.Fatalf("effective image: %q, %v, %v", image, ok, err)
	}
	if image != canonicalMainImage+":${FTW_IMAGE_TAG:-latest}" {
		t.Fatalf("effective image = %q", image)
	}
	overrideData, err := os.ReadFile(s.updateOverrideFile)
	if err != nil {
		t.Fatal(err)
	}
	var override struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(overrideData, &override); err != nil {
		t.Fatal(err)
	}
	if got := override.Services[legacyMainServiceName].Environment[mainTagEnv]; got != "${FTW_IMAGE_TAG:-latest}" {
		t.Fatalf("compatibility override %s = %q", mainTagEnv, got)
	}
	got, err := os.ReadFile(s.composeFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatal("compatibility migration modified the host Compose file")
	}
	overridePath := s.updateOverrideFile
	cleanup()
	if s.updateOverrideFile != "" {
		t.Fatal("cleanup should remove the override from the Compose chain")
	}
	if _, err := os.Stat(overridePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transient override still exists: %v", err)
	}
}

func TestPrepareUpdateImagePin_AddsTagEnvironmentToVariableImage(t *testing.T) {
	s, _ := newTestServer(t)
	original := `services:
  ftw:
    image: ghcr.io/srcfl/ftw:${FTW_IMAGE_TAG:-latest}
    volumes:
      - ./data:/app/data
`
	writeCompose(t, s.composeFile, original)

	cleanup, err := s.prepareUpdateImagePin()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if s.updateOverrideFile == "" {
		t.Fatal("variable image without a container environment mapping should create an override")
	}
	overrideData, err := os.ReadFile(s.updateOverrideFile)
	if err != nil {
		t.Fatal(err)
	}
	var override struct {
		Services map[string]struct {
			Image       string            `yaml:"image"`
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(overrideData, &override); err != nil {
		t.Fatal(err)
	}
	svc := override.Services[canonicalMainServiceName]
	if svc.Image != "" {
		t.Fatalf("identity-only override changed image to %q", svc.Image)
	}
	if got := svc.Environment[mainTagEnv]; got != "${FTW_IMAGE_TAG:-latest}" {
		t.Fatalf("compatibility override %s = %q", mainTagEnv, got)
	}
	got, err := os.ReadFile(s.composeFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatal("compatibility migration modified the host Compose file")
	}
}

func TestPrepareUpdateImagePin_OverridesStaleContainerTag(t *testing.T) {
	s, _ := newTestServer(t)
	writeCompose(t, s.composeFile, `services:
  ftw:
    image: ghcr.io/srcfl/ftw:${FTW_IMAGE_TAG:-latest}
    environment:
      FTW_IMAGE_TAG: stale-fixed-tag
`)

	cleanup, err := s.prepareUpdateImagePin()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if s.updateOverrideFile == "" {
		t.Fatal("variable image with a stale container tag should create an override")
	}
	overrideData, err := os.ReadFile(s.updateOverrideFile)
	if err != nil {
		t.Fatal(err)
	}
	var override struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(overrideData, &override); err != nil {
		t.Fatal(err)
	}
	if got := override.Services[canonicalMainServiceName].Environment[mainTagEnv]; got != "${FTW_IMAGE_TAG:-latest}" {
		t.Fatalf("compatibility override %s = %q", mainTagEnv, got)
	}
}

func TestPrepareUpdateImagePin_ReplacesUnsupportedTagExpressions(t *testing.T) {
	for _, image := range []string{
		"ghcr.io/srcfl/ftw:${MY_FTW_IMAGE_TAG:-latest}",
		"ghcr.io/srcfl/ftw:$FTW_IMAGE_TAG",
		"ghcr.io/srcfl/ftw:${FTW_IMAGE_TAGGED:-latest}",
		"ghcr.io/srcfl/ftw:${FTW_IMAGE_TAG:-latest}-${ARCH}",
		"${FTW_REGISTRY:-ghcr.io/srcfl}/ftw:${FTW_IMAGE_TAG:-latest}",
	} {
		t.Run(image, func(t *testing.T) {
			s, _ := newTestServer(t)
			writeCompose(t, s.composeFile, "services:\n  ftw:\n    image: "+image+"\n")

			cleanup, err := s.prepareUpdateImagePin()
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			effective, ok, err := serviceImageFromComposeFiles(s.composeFiles(), canonicalMainServiceName)
			if err != nil || !ok {
				t.Fatalf("effective image = %q, %v, %v", effective, ok, err)
			}
			want := canonicalMainImage + ":${FTW_IMAGE_TAG:-latest}"
			if effective != want {
				t.Fatalf("effective image = %q, want %q", effective, want)
			}
			if got, ok := composeImageRepositoryForTag(effective, mainTagEnv); !ok || got != canonicalMainImage {
				t.Fatalf("rollback repository = %q, supported=%v", got, ok)
			}
		})
	}
}

func TestValidateComponentImagePinRequiresExactVariable(t *testing.T) {
	for _, image := range []string{
		"ghcr.io/srcfl/ftw:${MY_FTW_IMAGE_TAG:-latest}",
		"ghcr.io/srcfl/ftw:$FTW_IMAGE_TAG",
		"ghcr.io/srcfl/ftw:${FTW_IMAGE_TAGGED:-latest}",
		"ghcr.io/srcfl/ftw:${FTW_IMAGE_TAG:-latest}-${ARCH}",
		"${FTW_REGISTRY:-ghcr.io/srcfl}/ftw:${FTW_IMAGE_TAG:-latest}",
	} {
		t.Run(image, func(t *testing.T) {
			s, _ := newTestServer(t)
			writeCompose(t, s.composeFile, "services:\n  ftw:\n    image: "+image+"\n")
			spec, err := s.componentSpec("core")
			if err != nil {
				t.Fatal(err)
			}
			if err := s.validateComponentImagePin(spec); err == nil {
				t.Fatalf("validate accepted unsupported image %q", image)
			}
		})
	}
}

func TestPrepareUpdateImagePin_WinsOverHardcodedUserOverride(t *testing.T) {
	s, _ := newTestServer(t)
	userOverride := filepath.Join(filepath.Dir(s.composeFile), "docker-compose.override.yml")
	writeCompose(t, userOverride, `services:
  ftw:
    image: local-ftw:developer-build
`)
	s.overrideFiles = []string{userOverride}

	cleanup, err := s.prepareUpdateImagePin()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	args := s.composeArgs("pull", canonicalMainServiceName)
	joined := strings.Join(args, " ")
	wantOrder := "-f " + s.composeFile + " -f " + userOverride + " -f " + s.updateOverrideFile + " pull"
	if !strings.Contains(joined, wantOrder) {
		t.Fatalf("generated override must be last in Compose order:\n got %s\nwant fragment %s", joined, wantOrder)
	}
}

func TestHandleUpdate_RestartKeepsEveryRunningImage(t *testing.T) {
	for _, tc := range []struct{ name, image, target, action string }{
		{"beta with stale env", "ghcr.io/srcfl/ftw:v2.14.0-beta.1", "", "restart"},
		{"wrong caller version", "ghcr.io/srcfl/ftw:v2.14.0-beta.1", "v2.0.0", "restart"},
		{"moving tag", "ghcr.io/srcfl/ftw:latest", "", "restart"},
		{"local review", "ftw-ev-review:46d04a7a", "", "restart_existing"},
		{"digest", "ghcr.io/srcfl/ftw@sha256:abc123", "", "restart_existing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, runner := newTestServer(t)
			writeCompose(t, s.composeFile, "services:\n  ftw:\n    image: "+tc.image+"\n")
			writeCompose(t, filepath.Join(filepath.Dir(s.composeFile), ".env"), "FTW_IMAGE_TAG=v2.0.0-beta.2\n")
			s.imageID = func(context.Context, string) (string, error) {
				t.Error("restart must not resolve an image")
				return "", errors.New("inspect unavailable")
			}
			healthChecks := 0
			s.healthCheck = func(_ context.Context, service string) error {
				healthChecks++
				if service != canonicalMainServiceName {
					t.Errorf("health checked %s", service)
				}
				return nil
			}
			req := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(`{"action":"`+tc.action+`","target":"`+tc.target+`"}`))
			rr := httptest.NewRecorder()
			s.handleUpdate(rr, req)
			if rr.Code != http.StatusAccepted {
				t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
			}
			st := waitForState(t, s, "done")
			if st.Action != "restart" || st.Target != "" || st.Step != st.TotalSteps {
				t.Fatalf("restart state = %+v", st)
			}
			calls := runner.snapshot()
			if len(calls) != 1 || strings.Join(calls[0], " ") != strings.Join(s.composeArgs("restart", "--no-deps", canonicalMainServiceName), " ") {
				t.Fatalf("restart selected or pulled a replacement image: %v", calls)
			}
			for _, env := range runner.envSnapshot() {
				if len(env) != 0 {
					t.Fatalf("restart image env = %v", env)
				}
			}
			if healthChecks != 1 {
				t.Fatalf("health checks = %d", healthChecks)
			}
			pin, _ := os.ReadFile(filepath.Join(filepath.Dir(s.composeFile), ".env"))
			if string(pin) != "FTW_IMAGE_TAG=v2.0.0-beta.2\n" {
				t.Fatalf("restart rewrote .env: %s", pin)
			}
		})
	}
}

func TestHandleUpdate_RestartRejectsMovingTarget(t *testing.T) {
	s, runner := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(`{"action":"restart","target":"latest"}`))
	rr := httptest.NewRecorder()
	s.handleUpdate(rr, req)
	if rr.Code != 400 {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if len(runner.snapshot()) != 0 {
		t.Fatal("rejected restart called Docker")
	}
}

func TestRestartFailureDoesNotFallBackToRecreate(t *testing.T) {
	for _, failed := range []string{"restart", "health"} {
		t.Run(failed, func(t *testing.T) {
			s, runner := newTestServer(t)
			runner.fail = failed == "restart"
			s.healthCheck = func(context.Context, string) error { return errors.New("health did not recover") }
			s.runJob("restart", "v2.0.0")
			st := s.readState()
			if st.State != "failed" || !strings.Contains(st.Message, "restart failed") {
				t.Fatalf("state = %+v", st)
			}
			if len(runner.snapshot()) != 1 {
				t.Fatalf("failure must not pull/recreate: %v", runner.snapshot())
			}
		})
	}
}

// Rollback flow: stop → docker cp per file → up. Issue #152.
func TestHandleUpdate_RollbackRestoresFiles(t *testing.T) {
	s, runner := newTestServer(t)

	// Create a fake snapshot on disk at the sidecar's expected host
	// path: <compose_dir>/data/snapshots/<id>/. The sidecar derives
	// this from FTW_UPDATER_COMPOSE (s.composeFile in tests).
	composeDir := filepath.Dir(s.composeFile)
	snapID := "2026-04-20T10-00-00Z_0.30.0_to_0.31.0"
	snapDir := filepath.Join(composeDir, "data", "snapshots", snapID)
	safetyID := "2026-04-20T10-05-00Z_pre-rollback"
	safetyDir := filepath.Join(composeDir, "data", "snapshots", safetyID)
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(safetyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGzipFile(t, filepath.Join(snapDir, "state.db.gz"), []byte("target database"))
	writeGzipFile(t, filepath.Join(safetyDir, "state.db.gz"), []byte("safety database"))
	if err := os.WriteFile(filepath.Join(snapDir, "config.yaml"), []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}

	body := `{"action":"rollback","snapshot":"` + snapID + `","files":["state.db.gz","config.yaml"],"safety_snapshot":"` + safetyID + `","safety_files":["state.db.gz"]}`
	req := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(body))
	rr := httptest.NewRecorder()
	s.handleUpdate(rr, req)
	if rr.Code != 202 {
		t.Fatalf("rollback 202 expected, got %d body=%s", rr.Code, rr.Body.String())
	}
	st := waitForState(t, s, "done")
	if st.Action != "rollback" || st.Snapshot != snapID {
		t.Errorf("state should track rollback + snapshot id: %+v", st)
	}

	calls := runner.snapshot()
	// Expected sequence: stop exact container → archive-copy state/config →
	// clear stale SQLite WAL sidecars → start the same container.
	if len(calls) != 5 {
		t.Fatalf("want 5 docker calls, got %d: %v", len(calls), calls)
	}
	if got := strings.Join(calls[0], " "); got != "stop --time 30 ftw-container" {
		t.Errorf("first call must stop the exact running container: %v", calls[0])
	}
	for i, f := range []string{"state.db", "config.yaml"} {
		joined := strings.Join(calls[i+1], " ")
		if !strings.HasPrefix(joined, "cp -a ") || !strings.Contains(joined, "ftw-container:/app/data/"+f) {
			t.Errorf("call %d should archive-copy %s: %v", i+1, f, calls[i+1])
		}
	}
	if got := strings.Join(calls[3], " "); !strings.Contains(got, "--volumes-from ftw-container") || !strings.Contains(got, "state.db-wal") {
		t.Errorf("fourth call must clear SQLite WAL files: %v", calls[3])
	}
	if got := strings.Join(calls[4], " "); got != "start ftw-container" {
		t.Errorf("final call must start the same container: %v", calls[4])
	}
}

// Rollback with a missing snapshot dir fails at stat, never stops the
// main service. Safer than optimistically stopping then discovering no
// files to restore.
func TestHandleUpdate_RollbackMissingSnapshot(t *testing.T) {
	s, runner := newTestServer(t)
	body := `{"action":"rollback","snapshot":"nope","files":["state.db.gz"],"safety_snapshot":"safety","safety_files":["state.db.gz"]}`
	req := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(body))
	rr := httptest.NewRecorder()
	s.handleUpdate(rr, req)
	if rr.Code != 202 {
		t.Fatalf("handler should 202 and fail async, got %d", rr.Code)
	}
	st := waitForState(t, s, "failed")
	if !strings.Contains(st.Message, "snapshot file") || !strings.Contains(st.Message, "not readable") {
		t.Errorf("failure should mention missing snapshot: %+v", st)
	}
	if len(runner.snapshot()) != 0 {
		t.Errorf("no docker calls should fire when snapshot is missing, got: %v", runner.snapshot())
	}
}

func TestHandleUpdate_RollbackHealthFailureRestoresSafetyBackup(t *testing.T) {
	s, runner := newTestServer(t)
	checks := 0
	var stagedMu sync.Mutex
	var staged []string
	originalStage := s.stageSnapshotFile
	s.stageSnapshotFile = func(ctx context.Context, containerID, snapshotID, file, dst string) error {
		stagedMu.Lock()
		staged = append(staged, snapshotID+"/"+file)
		stagedMu.Unlock()
		return originalStage(ctx, containerID, snapshotID, file, dst)
	}
	s.healthCheck = func(context.Context, string) error {
		checks++
		if checks == 1 {
			return errors.New("restored database did not boot")
		}
		return nil
	}
	root := filepath.Join(filepath.Dir(s.composeFile), "data", "snapshots")
	for _, id := range []string{"target", "safety"} {
		dir := filepath.Join(root, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeGzipFile(t, filepath.Join(dir, "state.db.gz"), []byte(id+" database"))
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(id), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	body := `{"action":"rollback","snapshot":"target","files":["state.db.gz","config.yaml"],"safety_snapshot":"safety","safety_files":["state.db.gz","config.yaml"]}`
	req := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(body))
	rr := httptest.NewRecorder()
	s.handleUpdate(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("rollback = %d: %s", rr.Code, rr.Body.String())
	}
	state := waitForState(t, s, "failed")
	if !strings.Contains(state.Message, "pre-rollback state restored and service recovered") {
		t.Fatalf("safety recovery status = %+v", state)
	}
	calls := runner.snapshot()
	if len(calls) != 10 {
		t.Fatalf("target + safety recovery calls = %d: %v", len(calls), calls)
	}
	stagedMu.Lock()
	stagedFiles := append([]string{}, staged...)
	stagedMu.Unlock()
	wantStaged := []string{"target/state.db.gz", "target/config.yaml", "safety/state.db.gz", "safety/config.yaml"}
	if strings.Join(stagedFiles, ",") != strings.Join(wantStaged, ",") {
		t.Fatalf("snapshot staging order = %v, want %v", stagedFiles, wantStaged)
	}
	if checks != 2 {
		t.Fatalf("health checks = %d, want target + recovered safety", checks)
	}
}

// Rollback with dangerous snapshot ids is rejected at the handler level,
// before any goroutine is spawned.
func TestHandleUpdate_RollbackRejectsTraversal(t *testing.T) {
	s, _ := newTestServer(t)
	for _, evil := range []string{"", "..", "../foo", "a/b"} {
		body := `{"action":"rollback","snapshot":"` + evil + `","files":["state.db.gz"],"safety_snapshot":"safety","safety_files":["state.db.gz"]}`
		req := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(body))
		rr := httptest.NewRecorder()
		s.handleUpdate(rr, req)
		if rr.Code != 400 {
			t.Errorf("snapshot %q: want 400, got %d", evil, rr.Code)
		}
	}
}

func writeGzipFile(t *testing.T, path string, body []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	if _, err := zw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestHandleUpdate_RejectsBadAction(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(`{"action":"rm -rf"}`))
	rr := httptest.NewRecorder()
	s.handleUpdate(rr, req)
	if rr.Code != 400 {
		t.Fatalf("bad action should 400, got %d", rr.Code)
	}
}

func TestHandleUpdate_ConcurrentRejected(t *testing.T) {
	s, _ := newTestServer(t)
	// Swap the runner for a blocking one so the first job lingers and the
	// second request arrives while runMu is held.
	block := make(chan struct{})
	s.runner = func(ctx context.Context, _ []string, _ ...string) error {
		<-block
		return nil
	}

	req1 := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(`{"action":"update","target":"v1.2.3"}`))
	rr1 := httptest.NewRecorder()
	s.handleUpdate(rr1, req1)
	if rr1.Code != 202 {
		t.Fatalf("first call = %d", rr1.Code)
	}

	// Second call while the first is holding the lock → 409.
	req2 := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(`{"action":"restart"}`))
	rr2 := httptest.NewRecorder()
	s.handleUpdate(rr2, req2)
	if rr2.Code != 409 {
		t.Fatalf("second call should 409, got %d", rr2.Code)
	}
	close(block)
	waitForState(t, s, "done")
}

func TestHandleStatus_ReadsFile(t *testing.T) {
	s, _ := newTestServer(t)
	s.writeState(State{State: "pulling", Action: "update", UpdatedAt: time.Now()})
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rr := httptest.NewRecorder()
	s.handleStatus(rr, req)
	var st State
	if err := json.NewDecoder(rr.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.State != "pulling" {
		t.Errorf("state = %q", st.State)
	}
}

func TestDiscoverOverridesAndComposeArgs(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(base, []byte("version: '3'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// No override yet → no extra -f flags.
	if got := discoverOverrides(base); len(got) != 0 {
		t.Errorf("no override yet, got %v", got)
	}
	override := filepath.Join(dir, "docker-compose.override.yml")
	if err := os.WriteFile(override, []byte("version: '3'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &server{composeFile: base, overrideFiles: discoverOverrides(base)}
	args := s.composeArgs("up", "-d", "svc")
	want := []string{"compose", "-f", base, "-f", override, "up", "-d", "svc"}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Errorf("composeArgs =\n  %v\nwant\n  %v", args, want)
	}
}

func TestSelectMainServiceSupportsCanonicalAndLegacyLayouts(t *testing.T) {
	for _, service := range []string{canonicalMainServiceName, legacyMainServiceName} {
		t.Run(service, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "docker-compose.yml")
			writeCompose(t, path, "services:\n  "+service+":\n    image: ghcr.io/srcfl/ftw:${FTW_IMAGE_TAG:-latest}\n    volumes:\n      - ./data:/app/data\n")
			got, err := selectMainService([]string{path}, "")
			if err != nil || got != service {
				t.Fatalf("selectMainService() = %q, %v; want %q", got, err, service)
			}
		})
	}
}

func TestSelectMainServiceRejectsAmbiguousDataOwners(t *testing.T) {
	path := filepath.Join(t.TempDir(), "docker-compose.yml")
	writeCompose(t, path, `services:
  ftw:
    volumes: ["./data:/app/data"]
  forty-two-watts:
    volumes: ["./data:/app/data"]
`)
	if _, err := selectMainService([]string{path}, ""); err == nil {
		t.Fatal("ambiguous main services should be rejected")
	}
}

func TestRecoverCrashedState(t *testing.T) {
	s, _ := newTestServer(t)
	s.writeState(State{State: "pulling", UpdatedAt: time.Now()})
	s.recoverCrashedState()
	if st := s.readState(); st.State != "failed" {
		t.Errorf("recovery should have flipped state to failed, got %q", st.State)
	}
}

func TestRecoverCrashedRollbackRestoresSafetyBackup(t *testing.T) {
	s, runner := newTestServer(t)
	safetyDir := filepath.Join(filepath.Dir(s.composeFile), "data", "snapshots", "safety")
	if err := os.MkdirAll(safetyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGzipFile(t, filepath.Join(safetyDir, "state.db.gz"), []byte("current state"))
	s.writeState(State{
		State: "restoring", Action: "rollback", Snapshot: "target",
		SafetySnapshot: "safety", SafetyFiles: []string{"state.db.gz"},
		StartedAt: time.Now().Add(-time.Minute), UpdatedAt: time.Now(),
	})

	s.recoverCrashedState()
	state := s.readState()
	if state.State != "failed" || !strings.Contains(state.Message, "pre-rollback state restored and service recovered") {
		t.Fatalf("crashed rollback recovery = %+v", state)
	}
	calls := runner.snapshot()
	if len(calls) != 4 || strings.Join(calls[0], " ") != "stop --time 30 ftw-container" || strings.Join(calls[3], " ") != "start ftw-container" {
		t.Fatalf("crashed rollback recovery calls = %v", calls)
	}
}

func TestContainerIDsFromDockerPS_IgnoresNoiseAndDedupes(t *testing.T) {
	const id = "72d81f0fbb2faf0529c3bd611ed06b2d3f5c3f7a82ad1c24f925d81302c46f13"
	out := "time=\"2026-07-27T18:13:41Z\" level=warning msg=\"orphan containers\"\n" + id + "\n" + id[:12] + "\nnot-an-id\n"
	ids := containerIDsFromDockerPS(out)
	if len(ids) != 1 || ids[0] != id {
		t.Fatalf("ids = %v, want only full id after filtering noise + short prefix", ids)
	}

	// Pure noise used to become "resolved to 40 containers" via strings.Fields.
	noise := strings.Repeat("warning about compose project configuration ", 10)
	if got := containerIDsFromDockerPS(noise); len(got) != 0 {
		t.Fatalf("noise parsed as ids: %v", got)
	}

	// Single clean ID (common happy path).
	if got := containerIDsFromDockerPS(id + "\n"); len(got) != 1 || got[0] != id {
		t.Fatalf("clean id = %v", got)
	}
}

func TestUpdateReadinessFailureNeverRevertsImage(t *testing.T) {
	for _, healthErr := range []error{context.DeadlineExceeded, errors.New("container status is unhealthy")} {
		t.Run(healthErr.Error(), func(t *testing.T) {
			s, runner := newTestServer(t)
			s.healthCheck = func(ctx context.Context, _ string) error {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) < 5*time.Hour {
					t.Error("migration got less than the Core startup budget")
				}
				if st := s.readState(); st.State != "checking" || st.PreviousImageID != "sha256:current" {
					t.Errorf("missing in-flight image history: %+v", st)
				}
				return healthErr
			}
			s.selfReplace = func(string) error { t.Error("replaced updater before Core became ready"); return nil }
			s.runJob("update", "v3.2.0-beta.2")
			st := s.readState()
			if st.State != "failed" || st.Target != "v3.2.0-beta.2" || st.PreviousImageID != "sha256:current" || !strings.Contains(st.Message, "verified full backup") {
				t.Fatalf("state=%+v", st)
			}
			calls := runner.snapshot()
			if len(calls) != 2 || !strings.Contains(strings.Join(calls[0], " "), "pull ftw") || !strings.Contains(strings.Join(calls[1], " "), "up -d ftw") {
				t.Fatalf("readiness failure changed the running image: %v", calls)
			}
		})
	}
}

func TestInterruptedUpdateRetainsImageHistoryWithoutTouchingCore(t *testing.T) {
	s, runner := newTestServer(t)
	s.writeState(State{State: "checking", Action: "update", Component: "core", Target: "v3.2.0-beta.2", PreviousImageID: "sha256:before", Message: "Waiting for the new service to become ready"})
	s.recoverCrashedState()
	st := s.readState()
	if st.State != "failed" || st.PreviousImageID != "sha256:before" || !strings.Contains(st.Message, "Core and data were left in place") {
		t.Fatalf("state=%+v", st)
	}
	if calls := runner.snapshot(); len(calls) != 0 {
		t.Fatalf("restart changed Core: %v", calls)
	}
}
