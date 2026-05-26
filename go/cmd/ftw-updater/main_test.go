package main

import (
	"bytes"
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
		composeFile: filepath.Join(dir, "docker-compose.yml"),
		statusPath:  filepath.Join(dir, "state.json"),
		runner:      runner.run,
	}
	writeCompose(t, s.composeFile, `services:
  forty-two-watts:
    image: ghcr.io/frahlg/forty-two-watts:${FTW_IMAGE_TAG:-latest}
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
	waitForState(t, s, "done")
	calls := runner.snapshot()
	if len(calls) != 1 {
		t.Fatalf("skip-pull should yield 1 call (up only), got %d: %v", len(calls), calls)
	}
	if !strings.Contains(strings.Join(calls[0], " "), "up -d") {
		t.Errorf("single call should be `up -d`: %v", calls[0])
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

func TestHandleUpdate_RestartForceRecreates(t *testing.T) {
	s, runner := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(`{"action":"restart"}`))
	rr := httptest.NewRecorder()
	s.handleUpdate(rr, req)
	if rr.Code != 202 {
		t.Fatalf("status = %d", rr.Code)
	}
	waitForState(t, s, "done")
	up := strings.Join(runner.snapshot()[1], " ")
	if !strings.Contains(up, "--force-recreate") {
		t.Errorf("restart path must force-recreate: %v", up)
	}
}

func TestHandleUpdate_PullFailure(t *testing.T) {
	s, runner := newTestServer(t)
	runner.fail = true

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

func TestHandleUpdate_FailsWhenComposeDoesNotReadImageTag(t *testing.T) {
	s, runner := newTestServer(t)
	writeCompose(t, s.composeFile, `services:
  forty-two-watts:
    image: ghcr.io/frahlg/forty-two-watts:latest
`)

	req := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(`{"action":"update","target":"v0.44.0"}`))
	rr := httptest.NewRecorder()
	s.handleUpdate(rr, req)
	if rr.Code != 202 {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	st := waitForState(t, s, "failed")
	if !strings.Contains(st.Message, "FTW_IMAGE_TAG") {
		t.Fatalf("failure should explain stale compose image pinning, got %+v", st)
	}
	if calls := runner.snapshot(); len(calls) != 0 {
		t.Fatalf("preflight failure should not call docker, got %v", calls)
	}
}

// `restart` is the dev path — no target needed, no env override, falls
// through to compose's :latest default.
func TestHandleUpdate_RestartLeavesEnvUnset(t *testing.T) {
	s, runner := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(`{"action":"restart"}`))
	rr := httptest.NewRecorder()
	s.handleUpdate(rr, req)
	if rr.Code != 202 {
		t.Fatalf("status = %d", rr.Code)
	}
	waitForState(t, s, "done")
	for i, env := range runner.envSnapshot() {
		if len(env) != 0 {
			t.Errorf("restart call %d should have no extra env, got %v", i, env)
		}
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
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"state.db", "config.yaml"} {
		if err := os.WriteFile(filepath.Join(snapDir, f), []byte("fake"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	body := `{"action":"rollback","snapshot":"` + snapID + `","files":["state.db","config.yaml"]}`
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
	// Expected sequence: compose stop → cp state.db → cp config.yaml → compose up.
	if len(calls) != 4 {
		t.Fatalf("want 4 docker calls, got %d: %v", len(calls), calls)
	}
	if !strings.Contains(strings.Join(calls[0], " "), "stop") {
		t.Errorf("first call must be compose stop: %v", calls[0])
	}
	for i, f := range []string{"state.db", "config.yaml"} {
		joined := strings.Join(calls[i+1], " ")
		if !strings.HasPrefix(joined, "cp ") || !strings.Contains(joined, f) {
			t.Errorf("call %d should be docker cp for %s: %v", i+1, f, calls[i+1])
		}
		if !strings.Contains(joined, snapID) {
			t.Errorf("call %d should reference snapshot id %s: %v", i+1, snapID, calls[i+1])
		}
	}
	up := strings.Join(calls[3], " ")
	if !strings.Contains(up, "up -d") || !strings.Contains(up, "--force-recreate") {
		t.Errorf("final call must be compose up -d --force-recreate: %v", calls[3])
	}
}

// Rollback with a missing snapshot dir fails at stat, never stops the
// main service. Safer than optimistically stopping then discovering no
// files to restore.
func TestHandleUpdate_RollbackMissingSnapshot(t *testing.T) {
	s, runner := newTestServer(t)
	body := `{"action":"rollback","snapshot":"nope","files":["state.db"]}`
	req := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(body))
	rr := httptest.NewRecorder()
	s.handleUpdate(rr, req)
	if rr.Code != 202 {
		t.Fatalf("handler should 202 and fail async, got %d", rr.Code)
	}
	st := waitForState(t, s, "failed")
	if !strings.Contains(st.Message, "snapshot not readable") {
		t.Errorf("failure should mention missing snapshot: %+v", st)
	}
	if len(runner.snapshot()) != 0 {
		t.Errorf("no docker calls should fire when snapshot is missing, got: %v", runner.snapshot())
	}
}

// Rollback with dangerous snapshot ids is rejected at the handler level,
// before any goroutine is spawned.
func TestHandleUpdate_RollbackRejectsTraversal(t *testing.T) {
	s, _ := newTestServer(t)
	for _, evil := range []string{"", "..", "../foo", "a/b"} {
		body := `{"action":"rollback","snapshot":"` + evil + `"}`
		req := httptest.NewRequest(http.MethodPost, "/update", strings.NewReader(body))
		rr := httptest.NewRecorder()
		s.handleUpdate(rr, req)
		if rr.Code != 400 {
			t.Errorf("snapshot %q: want 400, got %d", evil, rr.Code)
		}
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

func TestRecoverCrashedState(t *testing.T) {
	s, _ := newTestServer(t)
	s.writeState(State{State: "pulling", UpdatedAt: time.Now().Add(-10 * time.Minute)})
	s.recoverCrashedState()
	if st := s.readState(); st.State != "failed" {
		t.Errorf("recovery should have flipped state to failed, got %q", st.State)
	}
}
