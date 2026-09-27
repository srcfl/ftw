package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/nativeupdate"
	"github.com/srcfl/ftw/go/internal/selfupdate"
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

// installNativeSlots writes the unpacked release directories a launcher
// leaves under root, each with its receipt at state schema 7.
func installNativeSlots(t *testing.T, root string, tags ...string) {
	t.Helper()
	for _, tag := range tags {
		dir := filepath.Join(root, "releases", tag)
		for _, name := range []string{"ftw", "web/index.html", "drivers/BUNDLED_SOURCE.json", "optimizer/native/bundle/manifest.json"} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte("test"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		receipt := fmt.Sprintf(`{"tag":%q,"arch":%q,"archive_sha256":%q,"state_schema":7}`, tag, runtime.GOARCH, strings.Repeat("a", 64))
		if err := os.WriteFile(filepath.Join(dir, ".ftw-release.json"), []byte(receipt), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// releaseListJSON is a GitHub release list with one prerelease that carries
// this host's package and checksum.
func releaseListJSON(tag, body string) string {
	asset := "ftw-linux-" + runtime.GOARCH + ".tar.gz"
	entry, _ := json.Marshal(map[string]any{
		"tag_name": tag, "prerelease": strings.Contains(tag, "-beta."), "body": body,
		"html_url": "https://example/releases/" + tag,
		"assets":   []map[string]string{{"name": asset}, {"name": asset + ".sha256"}},
	})
	return "[" + string(entry) + "]"
}

// newCheckerAgainst returns a native Checker on a ready install of current,
// primed with one Check against a release list that publishes tag at the
// same state schema. The status file lives beside the slots.
func newCheckerAgainst(t *testing.T, tag, current string) *selfupdate.Checker {
	t.Helper()
	root := t.TempDir()
	installNativeSlots(t, root, current)
	if err := (nativeupdate.Manager{Root: root}).Init(current); err != nil {
		t.Fatal(err)
	}
	return newCheckerOnRoot(t, root, tag, current, "<!-- ftw-state-schema-v2:7 -->", func() error { return nil })
}

func newCheckerOnRoot(t *testing.T, root, tag, current, body string, restart func() error) *selfupdate.Checker {
	t.Helper()
	releases := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(releaseListJSON(tag, body)))
	}))
	t.Cleanup(releases.Close)
	c := selfupdate.New(selfupdate.Config{
		CurrentVersion:     current,
		CurrentStateSchema: 7,
		CheckInterval:      time.Hour,
		NativeRoot:         root,
		StatusPath:         filepath.Join(root, "update-status.json"),
		ReleasesURL:        releases.URL,
		NativeRestart:      restart,
	}, newMemStore())
	if _, err := c.Check(t.Context(), true); err != nil {
		t.Fatalf("priming check: %v", err)
	}
	return c
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
	c := newCheckerAgainst(t, "v0.133.0-beta.1", "v0.132.0")
	if err := c.SetChannel(selfupdate.ChannelBeta); err != nil {
		t.Fatal(err)
	}
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
	if info.Latest != "v0.133.0-beta.1" || !info.UpdateAvailable || !info.Native || !info.InstallReady {
		t.Errorf("unexpected info: %+v", info)
	}
}

func TestVersionChannel_RoundTrip(t *testing.T) {
	c := newCheckerAgainst(t, "v0.133.0", "v0.132.0")
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
	c := newCheckerAgainst(t, "v0.133.0", "v0.132.0")
	srv := New(&Deps{SelfUpdate: c})
	req := httptest.NewRequest(http.MethodPost, "/api/version/channel", strings.NewReader(`{"channel":"nightly"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown channel status = %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestVersionChannel_BlockedWhileUpdateIsInFlight(t *testing.T) {
	c := newCheckerAgainst(t, "v0.133.0", "v0.132.0")
	if err := c.WriteStatus(selfupdate.UpdateStatus{State: "pulling", Action: "update", Target: "v0.133.0"}); err != nil {
		t.Fatal(err)
	}
	srv := New(&Deps{SelfUpdate: c})
	req := httptest.NewRequest(http.MethodPost, "/api/version/channel", strings.NewReader(`{"channel":"beta"}`))
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
	c := newCheckerAgainst(t, "v0.133.0", "v0.132.0")
	srv := New(&Deps{SelfUpdate: c})

	// Skip v0.133.0 — should now report Skipped=true.
	body := strings.NewReader(`{"version":"v0.133.0"}`)
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
	c := newCheckerAgainst(t, "v0.133.0", "v0.132.0")
	srv := New(&Deps{SelfUpdate: c})
	req := httptest.NewRequest(http.MethodPost, "/api/version/skip", strings.NewReader(`{"version":""}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty version should be 400, got %d", rr.Code)
	}
}

// Without readable release slots nothing can be staged, and without a way to
// stop Core nothing can restart; both surface as 502.
func TestVersionUpdateAndRestartNeedAReadyInstall(t *testing.T) {
	c := newCheckerOnRoot(t, t.TempDir(), "v0.133.0", "v0.132.0", "<!-- ftw-state-schema-v2:7 -->", nil)
	srv := New(&Deps{SelfUpdate: c})

	for path, want := range map[string]string{
		"/api/version/update":  "native release slot not ready",
		"/api/version/restart": "native restart is not configured",
	} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), want) {
			t.Errorf("%s = %d %s, want 502 %q", path, rr.Code, rr.Body.String(), want)
		}
	}
}

func TestVersionRestartKeepsTheInstalledRelease(t *testing.T) {
	root := t.TempDir()
	restarts := 0
	c := newCheckerOnRoot(t, root, "v0.133.0", "v0.132.0", "", func() error { restarts++; return nil })
	srv := New(&Deps{SelfUpdate: c})
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/version/restart", nil))
	if rr.Code != http.StatusAccepted || restarts != 1 {
		t.Fatalf("restart = %d %s, restarts=%d", rr.Code, rr.Body.String(), restarts)
	}
	if st := c.Status(); st.Action != "restart" || st.State != "restarting" || st.Target != "v0.132.0" {
		t.Fatalf("restart status = %+v", st)
	}
	// A second request while the first is in flight is refused.
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/version/restart", nil))
	if rr.Code != http.StatusConflict || restarts != 1 {
		t.Fatalf("second restart = %d %s, restarts=%d", rr.Code, rr.Body.String(), restarts)
	}
}

// seedSnapshot writes a rollback point as the older Docker Core left it.
func seedSnapshot(t *testing.T, snapshotDir, id string, created time.Time) string {
	t.Helper()
	dir := filepath.Join(snapshotDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := fmt.Sprintf(`{"schema_version":2,"created_at":%q,"from_version":"v3.8.0","to_version":"v3.8.1","action":"update","complete_database":true,"files":["state.db.gz","config.yaml"]}`,
		created.UTC().Format(time.RFC3339Nano))
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.db.gz"), []byte("settings"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Delete by id removes the directory + returns 200, so an owner can reclaim
// the space an older Core's rollback points still take.
func TestVersionSnapshots_DeleteByID(t *testing.T) {
	c := newCheckerAgainst(t, "v0.133.0", "v0.132.0")
	snapDir := filepath.Join(t.TempDir(), "snapshots")
	path := seedSnapshot(t, snapDir, "2026-09-01T10-00-00Z_3.8.0_to_3.8.1", time.Now())
	srv := New(&Deps{SelfUpdate: c, SnapshotDir: snapDir})

	req := httptest.NewRequest(http.MethodDelete, "/api/version/snapshots/"+filepath.Base(path), nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("DELETE = %d body=%s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("snapshot dir should be gone after DELETE, stat err = %v", err)
	}
}

func TestVersionSnapshots_DeleteRejectedDuringUpdate(t *testing.T) {
	c := newCheckerAgainst(t, "v0.133.0", "v0.132.0")
	snapDir := filepath.Join(t.TempDir(), "snapshots")
	path := seedSnapshot(t, snapDir, "2026-09-01T10-00-00Z_3.8.0_to_3.8.1", time.Now())
	srv := New(&Deps{SelfUpdate: c, SnapshotDir: snapDir})
	if err := c.WriteStatus(selfupdate.UpdateStatus{State: "pulling", Action: "update", Target: "v0.133.0"}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/api/version/snapshots/"+filepath.Base(path), nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("DELETE during update = %d body=%s, want 409", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("snapshot should remain after rejected DELETE: %v", err)
	}
}

// Traversal + missing-id guards. A rogue client can't escape SnapshotDir
// or delete arbitrary files via the endpoint.
func TestVersionSnapshots_DeleteRejectsInvalidID(t *testing.T) {
	c := newCheckerAgainst(t, "v0.133.0", "v0.132.0")
	snapDir := filepath.Join(t.TempDir(), "snapshots")
	srv := New(&Deps{SelfUpdate: c, SnapshotDir: snapDir})

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
	c := newCheckerAgainst(t, "v0.133.0", "v0.132.0")
	snapDir := filepath.Join(t.TempDir(), "snapshots")
	now := time.Now()
	for i, id := range []string{"b", "c", "a"} {
		seedSnapshot(t, snapDir, id, now.Add(time.Duration(i)*time.Minute))
	}
	srv := New(&Deps{SelfUpdate: c, SnapshotDir: snapDir})

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
	if !out.Enabled || out.Dir != snapDir || len(out.Snapshots) != 3 {
		t.Fatalf("got %d snapshots, enabled=%v dir=%q", len(out.Snapshots), out.Enabled, out.Dir)
	}
	for i := 1; i < len(out.Snapshots); i++ {
		if !out.Snapshots[i-1].CreatedAt.After(out.Snapshots[i].CreatedAt) {
			t.Errorf("snapshots not ordered newest-first at idx %d", i)
		}
	}
	if out.Snapshots[0].ID != "a" || out.Snapshots[0].SizeBytes <= 0 || out.Snapshots[0].FromVersion != "v3.8.0" {
		t.Fatalf("newest snapshot = %+v", out.Snapshots[0])
	}
}

func TestVersionUpdateStatus_Idle(t *testing.T) {
	c := newCheckerAgainst(t, "v0.133.0", "v0.132.0")
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
		t.Errorf("state = %q, want idle (no update has run)", out.State)
	}
}

func TestNativeVersionRoutesRefuseWithoutAvailableRelease(t *testing.T) {
	root := t.TempDir()
	checker := selfupdate.New(selfupdate.Config{
		CurrentVersion: "v0.131.0-beta.1", NativeRoot: root,
		StatusPath:    filepath.Join(root, "update-status.json"),
		NativeRestart: func() error { return nil },
	}, newMemStore())
	srv := New(&Deps{SelfUpdate: checker, SnapshotDir: filepath.Join(root, "snapshots")})
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/version/update", nil))
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "no newer native release") {
		t.Fatalf("native update without target: %d %s", rr.Code, rr.Body.String())
	}
	if st := checker.Status(); st.State != "idle" {
		t.Fatalf("refused update wrote status %+v", st)
	}
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/version/binary-rollback", nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("native rollback without previous release: %d %s", rr.Code, rr.Body.String())
	}
}
