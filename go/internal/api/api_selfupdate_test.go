package api

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/selfupdate"
	"github.com/srcfl/ftw/go/internal/state"
)

// memStore satisfies selfupdate.Store for the wiring tests.
type memStore struct {
	mu sync.Mutex
	m  map[string]string
}

func newMemStore() *memStore { return &memStore{m: map[string]string{}} }
func (s *memStore) SaveConfig(k, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[k] = v
	return nil
}
func (s *memStore) LoadConfig(k string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[k]
	return v, ok
}

// newCheckerAgainst returns a Checker primed with one Check against a
// fake GH /releases/latest pointing at tag and a fake GHCR /tags/list
// that has the same tag pushed. After the priming Check the Info cache
// matches what handler tests expect to see.
func newCheckerAgainst(t *testing.T, tag, current string) *selfupdate.Checker {
	t.Helper()
	return newCheckerAgainstWithStatus(t, tag, current, "")
}

func newCheckerAgainstWithStatus(t *testing.T, tag, current, statusPath string) *selfupdate.Checker {
	t.Helper()
	return newCheckerAgainstWithStatusAndSocket(t, tag, current, statusPath, "")
}

func newCheckerAgainstWithStatusAndSocket(t *testing.T, tag, current, statusPath, socketPath string) *selfupdate.Checker {
	t.Helper()
	const repo = "srcfl/ftw"

	regMux := http.NewServeMux()
	regMux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "stub"})
	})
	regMux.HandleFunc("/v2/"+repo+"/tags/list", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": repo,
			"tags": []string{tag},
		})
	})
	regSrv := httptest.NewServer(regMux)
	t.Cleanup(regSrv.Close)

	relSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name":     tag,
			"html_url":     "https://example/releases/" + tag,
			"published_at": time.Now().Format(time.RFC3339),
		})
	}))
	t.Cleanup(relSrv.Close)

	c := selfupdate.New(selfupdate.Config{
		Repo:             repo,
		CurrentVersion:   current,
		RegistryBaseURL:  regSrv.URL,
		LatestReleaseURL: relSrv.URL,
		CheckInterval:    time.Hour,
		SocketPath:       socketPath,
		StatusPath:       statusPath,
	}, newMemStore())
	if _, err := c.Check(t.Context(), true); err != nil {
		t.Fatalf("priming check: %v", err)
	}
	return c
}

func startFakeSidecar(t *testing.T, statusCode int) string {
	t.Helper()
	socketPath := filepath.Join("/tmp", "ftw-"+strconv.FormatInt(time.Now().UnixNano(), 36)+".sock")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen fake sidecar: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(statusCode)
		_, _ = w.Write([]byte(`{"status":"stub"}`))
	})}
	go func() {
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
		_ = os.Remove(socketPath)
	})
	return socketPath
}

func waitUntil(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached before timeout")
}

func TestVersionCheck_Disabled(t *testing.T) {
	srv := New(&Deps{})
	req := httptest.NewRequest(http.MethodGet, "/api/version/check", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled should be 503, got %d", rr.Code)
	}
}

func TestVersionCheck_ReturnsInfo(t *testing.T) {
	c := newCheckerAgainst(t, "v1.5.0", "v1.4.0")
	srv := New(&Deps{SelfUpdate: c})

	req := httptest.NewRequest(http.MethodGet, "/api/version/check", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status = %d", rr.Code)
	}
	var info selfupdate.Info
	if err := json.Unmarshal(rr.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Latest != "v1.5.0" || !info.UpdateAvailable {
		t.Errorf("unexpected info: %+v", info)
	}
}

func TestVersionChannel_RoundTrip(t *testing.T) {
	c := newCheckerAgainst(t, "v1.5.0", "v1.4.0")
	srv := New(&Deps{SelfUpdate: c})

	req := httptest.NewRequest(http.MethodPost, "/api/version/channel", strings.NewReader(`{"channel":"beta"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("channel status = %d body=%s", rr.Code, rr.Body.String())
	}
	var info selfupdate.Info
	if err := json.Unmarshal(rr.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Channel != selfupdate.ChannelBeta || info.Latest != "" || info.UpdateAvailable {
		t.Fatalf("channel response = %+v", info)
	}
}

func TestVersionChannel_RejectsUnknownChannel(t *testing.T) {
	c := newCheckerAgainst(t, "v1.5.0", "v1.4.0")
	srv := New(&Deps{SelfUpdate: c})
	req := httptest.NewRequest(http.MethodPost, "/api/version/channel", strings.NewReader(`{"channel":"nightly"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown channel status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestVersionChannel_BlockedWhileSidecarUpdateIsInFlight(t *testing.T) {
	statusPath := filepath.Join(t.TempDir(), "state.json")
	c := newCheckerAgainstWithStatus(t, "v1.5.0", "v1.4.0", statusPath)
	if err := c.WriteStatus(selfupdate.UpdateStatus{State: "pulling", Action: "update", Target: "v1.5.0"}); err != nil {
		t.Fatal(err)
	}
	srv := New(&Deps{SelfUpdate: c})
	req := httptest.NewRequest(http.MethodPost, "/api/version/channel", strings.NewReader(`{"channel":"edge"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("in-flight channel status = %d body=%s", rr.Code, rr.Body.String())
	}
	if c.Info().Channel != selfupdate.ChannelStable {
		t.Fatalf("channel changed during update: %+v", c.Info())
	}
}

func TestVersionSkip_RoundTrip(t *testing.T) {
	c := newCheckerAgainst(t, "v1.5.0", "v1.4.0")
	srv := New(&Deps{SelfUpdate: c})

	// Skip v1.5.0 — should now report Skipped=true.
	body := strings.NewReader(`{"version":"v1.5.0"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/version/skip", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("skip status = %d body=%s", rr.Code, rr.Body.String())
	}
	if !c.Info().Skipped {
		t.Error("Skipped should be true after POST /skip")
	}

	// Unskip — should clear.
	req = httptest.NewRequest(http.MethodPost, "/api/version/unskip", nil)
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("unskip status = %d", rr.Code)
	}
	if c.Info().Skipped {
		t.Error("Skipped should be false after POST /unskip")
	}
}

func TestVersionSkip_EmptyVersionRejected(t *testing.T) {
	c := newCheckerAgainst(t, "v1.5.0", "v1.4.0")
	srv := New(&Deps{SelfUpdate: c})
	req := httptest.NewRequest(http.MethodPost, "/api/version/skip", strings.NewReader(`{"version":""}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty version should be 400, got %d", rr.Code)
	}
}

func TestVersionUpdate_NoSidecar502(t *testing.T) {
	// Checker has no socket configured — Trigger returns an error and the
	// handler surfaces it as 502 so the UI can show the sidecar is missing.
	c := newCheckerAgainst(t, "v1.5.0", "v1.4.0")
	srv := New(&Deps{SelfUpdate: c})

	for _, path := range []string{"/api/version/update", "/api/version/restart"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusBadGateway {
			t.Errorf("%s without sidecar = %d, want 502", path, rr.Code)
		}
	}
}

// Snapshot is captured BEFORE the handler hands off to the sidecar, so
// an operator who hits Update always has a rollback point — even if the
// sidecar is missing and the trigger fails. Issue #140.
func TestVersionUpdate_CreatesSnapshotBeforeTrigger(t *testing.T) {
	dir := t.TempDir()
	statusPath := filepath.Join(dir, "update-state.json")
	socketPath := startFakeSidecar(t, http.StatusInternalServerError)
	c := newCheckerAgainstWithStatusAndSocket(t, "v1.5.0", "v1.4.0", statusPath, socketPath)
	st, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	_ = st.SaveConfig("mode", "planner_self")

	snapDir := filepath.Join(dir, "snapshots")
	srv := New(&Deps{SelfUpdate: c, State: st, SnapshotDir: snapDir, ConfigPath: ""})

	req := httptest.NewRequest(http.MethodPost, "/api/version/update", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	// The handler returns immediately; snapshot + sidecar trigger finish in
	// the background and publish failure via /status if the sidecar rejects.
	if rr.Code != http.StatusAccepted {
		t.Errorf("expected 202, got %d", rr.Code)
	}
	waitUntil(t, func() bool { return c.Status().State == "failed" })
	entries, err := os.ReadDir(snapDir)
	if err != nil {
		t.Fatalf("snapshot dir not created: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 snapshot dir, got %d", len(entries))
	}
	// Snapshot dir should contain state.db + meta.json (no config.yaml
	// because ConfigPath was empty).
	snapPath := filepath.Join(snapDir, entries[0].Name())
	for _, f := range []string{"state.db", "meta.json"} {
		if _, err := os.Stat(filepath.Join(snapPath, f)); err != nil {
			t.Errorf("snapshot missing %s: %v", f, err)
		}
	}
	// state.db in the snapshot must be usable.
	snap, err := state.Open(filepath.Join(snapPath, "state.db"))
	if err != nil {
		t.Fatalf("snapshot state.db unusable: %v", err)
	}
	if v, ok := snap.LoadConfig("mode"); !ok || v != "planner_self" {
		t.Errorf("snapshot missing seeded mode config: %q ok=%v", v, ok)
	}
	snap.Close()

	gotStatus := c.Status()
	if gotStatus.State != "failed" || gotStatus.Action != "update" || gotStatus.Target != "v1.5.0" {
		t.Errorf("trigger failure should be published to status file, got %+v", gotStatus)
	}
}

// Operator opt-out: POST {skip_snapshot: true} skips the snapshot
// capture step and the handler proceeds to trigger the sidecar without
// creating a rollback point. Used when the retained set already covers
// the operator or when they consciously want to save the ~200 MB per
// snapshot on a constrained SD card. Issue #149.
func TestVersionUpdate_SkipSnapshotOptOut(t *testing.T) {
	dir := t.TempDir()
	statusPath := filepath.Join(dir, "update-state.json")
	socketPath := startFakeSidecar(t, http.StatusInternalServerError)
	c := newCheckerAgainstWithStatusAndSocket(t, "v1.5.0", "v1.4.0", statusPath, socketPath)
	st, _ := state.Open(filepath.Join(dir, "state.db"))
	t.Cleanup(func() { st.Close() })
	snapDir := filepath.Join(dir, "snapshots")
	srv := New(&Deps{SelfUpdate: c, State: st, SnapshotDir: snapDir})

	body := strings.NewReader(`{"skip_snapshot":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/version/update", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	// Sidecar trigger still fails, but the handler has already accepted
	// the async job and must honour the skip — i.e. no snapshot appears.
	if rr.Code != http.StatusAccepted {
		t.Errorf("want 202, got %d body=%s", rr.Code, rr.Body.String())
	}
	waitUntil(t, func() bool { return c.Status().State == "failed" })
	if entries, _ := os.ReadDir(snapDir); len(entries) != 0 {
		t.Errorf("snapshots dir should be empty after skip_snapshot=true, found %d entries", len(entries))
	}
}

// With ConfigPath set the snapshot must also copy config.yaml so a
// rollback can restore the exact YAML the operator was running.
func TestVersionUpdate_SnapshotIncludesConfigWhenPathSet(t *testing.T) {
	dir := t.TempDir()
	statusPath := filepath.Join(dir, "update-state.json")
	socketPath := startFakeSidecar(t, http.StatusInternalServerError)
	c := newCheckerAgainstWithStatusAndSocket(t, "v1.5.0", "v1.4.0", statusPath, socketPath)
	st, _ := state.Open(filepath.Join(dir, "state.db"))
	t.Cleanup(func() { st.Close() })
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("site:\n  name: test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapDir := filepath.Join(dir, "snapshots")
	srv := New(&Deps{SelfUpdate: c, State: st, SnapshotDir: snapDir, ConfigPath: cfgPath})

	req := httptest.NewRequest(http.MethodPost, "/api/version/update", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d body=%s", rr.Code, rr.Body.String())
	}
	waitUntil(t, func() bool { return c.Status().State == "failed" })
	entries, _ := os.ReadDir(snapDir)
	if len(entries) == 0 {
		t.Fatal("no snapshot created")
	}
	copied := filepath.Join(snapDir, entries[0].Name(), "config.yaml")
	got, err := os.ReadFile(copied)
	if err != nil {
		t.Fatalf("config.yaml not copied: %v", err)
	}
	if !strings.Contains(string(got), "name: test") {
		t.Errorf("config.yaml contents wrong: %s", got)
	}
}

// Snapshot failure must abort the update with 500 — we never want to
// pull a new image without a rollback point when the operator opted in.
func TestVersionUpdate_SnapshotFailureAborts(t *testing.T) {
	dir := t.TempDir()
	statusPath := filepath.Join(dir, "update-state.json")
	socketPath := startFakeSidecar(t, http.StatusAccepted)
	c := newCheckerAgainstWithStatusAndSocket(t, "v1.5.0", "v1.4.0", statusPath, socketPath)
	st, _ := state.Open(filepath.Join(dir, "state.db"))
	t.Cleanup(func() { st.Close() })

	// Point SnapshotDir at a PATH THAT IS A FILE, not a directory.
	// Mkdir on that path will fail and the whole update should abort.
	badPath := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(badPath, []byte("block"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := New(&Deps{SelfUpdate: c, State: st, SnapshotDir: badPath})

	req := httptest.NewRequest(http.MethodPost, "/api/version/update", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("want 202 when async snapshot job starts, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	waitUntil(t, func() bool {
		st := c.Status()
		return st.State == "failed" && strings.Contains(st.Message, "snapshot failed")
	})
}

// Retention keeps the N newest snapshots — older ones pruned after each
// new snapshot. Exercised via direct calls to avoid running the whole
// update pipeline five times.
func TestSnapshotsPruneToKeepNewest(t *testing.T) {
	dir := t.TempDir()
	st, _ := state.Open(filepath.Join(dir, "state.db"))
	t.Cleanup(func() { st.Close() })
	snapDir := filepath.Join(dir, "snapshots")

	srv := New(&Deps{State: st, SnapshotDir: snapDir})
	// Create snapshotKeepCount+3 snapshots. The createPreUpdateSnapshot
	// helper calls pruneSnapshots at the end of each, so after the
	// loop we should have exactly snapshotKeepCount dirs.
	for i := 0; i < snapshotKeepCount+3; i++ {
		if _, err := srv.createPreUpdateSnapshot("update", "v0.0.0", "v0.0.1"); err != nil {
			t.Fatalf("snapshot %d: %v", i, err)
		}
		// sleep a tick so the timestamp in the dir name changes —
		// otherwise mkdir fails because the sibling dir already exists.
		time.Sleep(1100 * time.Millisecond)
	}

	entries, _ := os.ReadDir(snapDir)
	if len(entries) != snapshotKeepCount {
		t.Errorf("retained %d snapshots, want %d", len(entries), snapshotKeepCount)
	}
}

// Delete by id removes the directory + returns 200. Guards #150's
// operator-self-service promise: "I see these snapshots in the UI and
// can reclaim disk from them without SSH."
func TestVersionSnapshots_DeleteByID(t *testing.T) {
	c := newCheckerAgainst(t, "v1.5.0", "v1.4.0")
	dir := t.TempDir()
	st, _ := state.Open(filepath.Join(dir, "state.db"))
	t.Cleanup(func() { st.Close() })
	snapDir := filepath.Join(dir, "snapshots")
	srv := New(&Deps{SelfUpdate: c, State: st, SnapshotDir: snapDir})
	snap, err := srv.createPreUpdateSnapshot("update", "v0.0.0", "v0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/version/snapshots/"+snap.ID, nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("DELETE = %d body=%s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(snap.Path); !os.IsNotExist(err) {
		t.Errorf("snapshot dir should be gone after DELETE, stat err = %v", err)
	}
}

// Traversal + missing-id guards. A rogue client can't escape SnapshotDir
// or delete arbitrary files via the endpoint.
func TestVersionSnapshots_DeleteRejectsInvalidID(t *testing.T) {
	c := newCheckerAgainst(t, "v1.5.0", "v1.4.0")
	dir := t.TempDir()
	st, _ := state.Open(filepath.Join(dir, "state.db"))
	t.Cleanup(func() { st.Close() })
	snapDir := filepath.Join(dir, "snapshots")
	srv := New(&Deps{SelfUpdate: c, State: st, SnapshotDir: snapDir})

	// Non-existent id: handler hits the stat check, returns 404.
	req := httptest.NewRequest(http.MethodDelete, "/api/version/snapshots/no-such-snapshot", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 404 {
		t.Errorf("missing snapshot: want 404, got %d (body=%s)", rr.Code, rr.Body.String())
	}

	// Traversal attempts: Go's ServeMux cleans paths and redirects
	// anything containing `..`, so our handler is never reached — but
	// that's the outcome we want (no delete, no 500, no information
	// leak). Accept any non-200 outcome.
	for _, evilID := range []string{"..", "../etc/passwd"} {
		req := httptest.NewRequest(http.MethodDelete, "/api/version/snapshots/"+evilID, nil)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code == 200 {
			t.Errorf("DELETE %q returned 200 — traversal reached the handler", evilID)
		}
	}
}

func TestVersionSnapshots_ListsNewestFirst(t *testing.T) {
	c := newCheckerAgainst(t, "v1.5.0", "v1.4.0")
	dir := t.TempDir()
	st, _ := state.Open(filepath.Join(dir, "state.db"))
	t.Cleanup(func() { st.Close() })
	snapDir := filepath.Join(dir, "snapshots")
	srv := New(&Deps{SelfUpdate: c, State: st, SnapshotDir: snapDir})

	for i := 0; i < 3; i++ {
		if _, err := srv.createPreUpdateSnapshot("update", "v0.0.0", "v0.0.1"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1100 * time.Millisecond)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/version/snapshots", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("snapshots list = %d", rr.Code)
	}
	var out struct {
		Snapshots []SnapshotInfo `json:"snapshots"`
		Dir       string         `json:"dir"`
		Enabled   bool           `json:"enabled"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Enabled || len(out.Snapshots) != 3 {
		t.Fatalf("got %d snapshots, enabled=%v", len(out.Snapshots), out.Enabled)
	}
	for i := 1; i < len(out.Snapshots); i++ {
		if !out.Snapshots[i-1].CreatedAt.After(out.Snapshots[i].CreatedAt) {
			t.Errorf("snapshots not ordered newest-first at idx %d", i)
		}
	}
}

// Rollback creates a pre-rollback safety snapshot, then asks the sidecar
// to restore (#152). The sidecar is absent in this test, so Trigger
// fails with 502 — but the safety snapshot must land first, proving
// the "we always capture current state before touching it" promise.
func TestVersionRollback_CreatesSafetySnapshotBeforeTrigger(t *testing.T) {
	c := newCheckerAgainst(t, "v1.5.0", "v1.4.0")
	dir := t.TempDir()
	st, _ := state.Open(filepath.Join(dir, "state.db"))
	t.Cleanup(func() { st.Close() })
	snapDir := filepath.Join(dir, "snapshots")
	srv := New(&Deps{SelfUpdate: c, State: st, SnapshotDir: snapDir})

	// Seed one snapshot so the rollback target exists.
	seed, err := srv.createPreUpdateSnapshot("update", "v1.3.0", "v1.4.0")
	if err != nil {
		t.Fatal(err)
	}

	body := strings.NewReader(`{"snapshot_id":"` + seed.ID + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/version/rollback", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	// Sidecar trigger fails → 502. The safety snapshot should have
	// been captured before the trigger attempt regardless.
	if rr.Code != http.StatusBadGateway {
		t.Errorf("want 502 from missing sidecar, got %d body=%s", rr.Code, rr.Body.String())
	}
	entries, _ := os.ReadDir(snapDir)
	// Seed + safety = 2 entries (retention is 5, way under).
	if len(entries) < 2 {
		t.Fatalf("want ≥ 2 snapshots on disk (seed + safety), got %d", len(entries))
	}
	// At least one snapshot must be tagged "pre-rollback" in meta.
	foundSafety := false
	for _, e := range entries {
		meta, err := readSnapshotMeta(filepath.Join(snapDir, e.Name()))
		if err != nil {
			continue
		}
		if meta.Action == "pre-rollback" {
			foundSafety = true
			break
		}
	}
	if !foundSafety {
		t.Error("expected a snapshot with meta.action = pre-rollback after rollback request")
	}
}

// Bad snapshot id → handler refuses without creating any files.
func TestVersionRollback_ValidatesSnapshotID(t *testing.T) {
	c := newCheckerAgainst(t, "v1.5.0", "v1.4.0")
	dir := t.TempDir()
	st, _ := state.Open(filepath.Join(dir, "state.db"))
	t.Cleanup(func() { st.Close() })
	srv := New(&Deps{SelfUpdate: c, State: st, SnapshotDir: filepath.Join(dir, "snapshots")})

	cases := []struct {
		body string
		want int
		desc string
	}{
		{`{}`, 400, "missing snapshot_id"},
		{`{"snapshot_id":""}`, 400, "empty snapshot_id"},
		{`{"snapshot_id":".."}`, 400, "traversal"},
		{`{"snapshot_id":"nope"}`, 404, "nonexistent id"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, "/api/version/rollback", strings.NewReader(c.body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != c.want {
			t.Errorf("%s: got %d, want %d (body=%s)", c.desc, rr.Code, c.want, rr.Body.String())
		}
	}
}

func TestVersionUpdateStatus_Idle(t *testing.T) {
	c := newCheckerAgainst(t, "v1.5.0", "v1.4.0")
	srv := New(&Deps{SelfUpdate: c})
	req := httptest.NewRequest(http.MethodGet, "/api/version/update/status", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status = %d", rr.Code)
	}
	var out selfupdate.UpdateStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.State != "idle" {
		t.Errorf("state = %q, want idle (no StatusPath configured)", out.State)
	}
}
