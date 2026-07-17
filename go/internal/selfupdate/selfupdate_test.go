package selfupdate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// memStore is an in-memory Store for tests.
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

// fakeRegistry stands up the two GHCR endpoints the Checker hits:
// /token and /v2/.../tags/list. The Checker just verifies tag
// membership — no HEAD or digest comparison — so this is all the
// surface area the tests need to exercise.
type fakeRegistry struct {
	t          *testing.T
	repo       string
	tags       []string
	listCalls  int
	tokenCalls int
}

func newFakeRegistry(t *testing.T, repo string) *fakeRegistry {
	return &fakeRegistry{t: t, repo: repo}
}

// addTag registers a tag in /tags/list. Order doesn't matter.
func (f *fakeRegistry) addTag(tag string) {
	for _, t := range f.tags {
		if t == tag {
			return
		}
	}
	f.tags = append(f.tags, tag)
}

func (f *fakeRegistry) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		f.tokenCalls++
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "stub-token"})
	})
	mux.HandleFunc("/v2/"+f.repo+"/tags/list", func(w http.ResponseWriter, r *http.Request) {
		f.listCalls++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": f.repo,
			"tags": f.tags,
		})
	})
	return httptest.NewServer(mux)
}

// fakeRelease bundles what the test wants from /releases/latest. tag=""
// makes the server 404 (= "no releases yet"). prerelease=true mimics
// drafts/prereleases (the Checker filters those out).
type fakeRelease struct {
	tag        string
	htmlURL    string
	body       string
	published  time.Time
	prerelease bool
	draft      bool
}

func fakeReleasesServer(t *testing.T, r fakeRelease) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if r.tag == "" {
			http.NotFound(w, req)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name":     r.tag,
			"html_url":     r.htmlURL,
			"body":         r.body,
			"published_at": r.published.Format(time.RFC3339),
			"prerelease":   r.prerelease,
			"draft":        r.draft,
		})
	}))
}

func newCheckerOnFakes(currentVersion string, registry, releases *httptest.Server, repo string, st Store) *Checker {
	return New(Config{
		Repo:             repo,
		CurrentVersion:   currentVersion,
		RegistryBaseURL:  registry.URL,
		LatestReleaseURL: releases.URL,
		CheckInterval:    time.Hour,
	}, st)
}

func TestCheck_UpdateAvailable(t *testing.T) {
	const repo = "srcfl/ftw"
	reg := newFakeRegistry(t, repo)
	reg.addTag("v1.3.0")
	rsrv := reg.server()
	defer rsrv.Close()

	published := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)
	rls := fakeReleasesServer(t, fakeRelease{
		tag: "v1.3.0", htmlURL: "https://example/releases/1.3.0",
		body: "## Features\n* shiny", published: published,
	})
	defer rls.Close()

	c := newCheckerOnFakes("v1.2.4", rsrv, rls, repo, newMemStore())
	info, err := c.Check(context.Background(), false)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info.Latest != "v1.3.0" {
		t.Errorf("latest = %q, want v1.3.0", info.Latest)
	}
	if !info.UpdateAvailable {
		t.Error("UpdateAvailable should be true")
	}
	if info.ReleaseNotesURL != "https://example/releases/1.3.0" {
		t.Errorf("notes url = %q", info.ReleaseNotesURL)
	}
	if info.PublishedAt.IsZero() {
		t.Error("PublishedAt not parsed")
	}
}

// The original race: GH publishes the release seconds before the build
// workflow finishes pushing the image to GHCR. Check must report
// update_available=false in that window, then flip to true once the
// tag appears in the registry.
func TestCheck_GHReleaseExistsButImageNotPushedYet(t *testing.T) {
	const repo = "srcfl/ftw"
	reg := newFakeRegistry(t, repo)
	// No tags pushed yet.
	rsrv := reg.server()
	defer rsrv.Close()

	rls := fakeReleasesServer(t, fakeRelease{tag: "v0.44.0"})
	defer rls.Close()

	c := newCheckerOnFakes("v0.43.1", rsrv, rls, repo, newMemStore())
	info, err := c.Check(context.Background(), false)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info.UpdateAvailable {
		t.Error("registry hasn't pushed v0.44.0 yet — UpdateAvailable must be false")
	}

	// Build workflow finishes; tag now appears in the registry.
	reg.addTag("v0.44.0")
	info, err = c.Check(context.Background(), true)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !info.UpdateAvailable {
		t.Error("after registry push, UpdateAvailable should flip to true")
	}
	if info.Latest != "v0.44.0" {
		t.Errorf("Latest = %q, want v0.44.0", info.Latest)
	}
}

// Repos with non-monotonic tag history (e.g. an older `2.x.y` scheme
// in the registry alongside the current `v0.X.Y` line) must NOT report
// the older-but-numerically-higher tag as latest. GH Releases is the
// authority for which version is current.
func TestCheck_RespectsGHReleaseOverHigherSemverInRegistry(t *testing.T) {
	const repo = "srcfl/ftw"
	reg := newFakeRegistry(t, repo)
	reg.addTag("v0.43.1")
	reg.addTag("v0.44.0") // current line, what GH says is latest
	reg.addTag("2.3.0")   // legacy tag from prior versioning era
	rsrv := reg.server()
	defer rsrv.Close()

	rls := fakeReleasesServer(t, fakeRelease{tag: "v0.44.0"})
	defer rls.Close()

	c := newCheckerOnFakes("v0.43.1", rsrv, rls, repo, newMemStore())
	info, err := c.Check(context.Background(), false)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info.Latest != "v0.44.0" {
		t.Errorf("Latest = %q, want v0.44.0 (legacy 2.3.0 must not win)", info.Latest)
	}
}

func TestCheck_NoReleasesYet(t *testing.T) {
	const repo = "srcfl/ftw"
	reg := newFakeRegistry(t, repo)
	rsrv := reg.server()
	defer rsrv.Close()

	rls := fakeReleasesServer(t, fakeRelease{}) // 404
	defer rls.Close()

	c := newCheckerOnFakes("v1.0.0", rsrv, rls, repo, newMemStore())
	info, err := c.Check(context.Background(), false)
	if err != nil {
		t.Fatalf("brand-new repo (no releases) should not error: %v", err)
	}
	if info.UpdateAvailable {
		t.Error("no releases → no update available")
	}
}

func TestCheck_PrereleaseFiltered(t *testing.T) {
	const repo = "srcfl/ftw"
	reg := newFakeRegistry(t, repo)
	reg.addTag("v1.5.0-rc1")
	rsrv := reg.server()
	defer rsrv.Close()

	rls := fakeReleasesServer(t, fakeRelease{tag: "v1.5.0-rc1", prerelease: true})
	defer rls.Close()

	c := newCheckerOnFakes("v1.4.0", rsrv, rls, repo, newMemStore())
	info, err := c.Check(context.Background(), false)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info.UpdateAvailable {
		t.Error("prerelease must not auto-dispatch as latest")
	}
}

func TestCheck_BetaChannelSelectsNewestBetaAndPersistsChannel(t *testing.T) {
	const repo = "srcfl/ftw"
	reg := newFakeRegistry(t, repo)
	reg.addTag("v1.5.0-beta.1")
	reg.addTag("v1.5.0-beta.2")
	rsrv := reg.server()
	defer rsrv.Close()

	releases := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"tag_name": "v1.5.0-rc.1", "prerelease": true, "published_at": "2026-07-12T11:00:00Z"},
			{"tag_name": "v1.5.0-beta.2", "prerelease": true, "published_at": "2026-07-12T10:00:00Z"},
			{"tag_name": "v1.5.0-beta.1", "prerelease": true, "published_at": "2026-07-11T10:00:00Z"},
		})
	}))
	defer releases.Close()

	st := newMemStore()
	c := New(Config{
		Repo: repo, CurrentVersion: "v1.4.0",
		RegistryBaseURL: rsrv.URL, ReleasesURL: releases.URL,
	}, st)
	if err := c.SetChannel(ChannelBeta); err != nil {
		t.Fatalf("SetChannel: %v", err)
	}
	info, err := c.Check(context.Background(), true)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info.Channel != ChannelBeta || info.Latest != "v1.5.0-beta.2" || !info.UpdateAvailable {
		t.Fatalf("beta info = %+v", info)
	}
	if got, _ := st.LoadConfig(channelKey); got != "beta" {
		t.Fatalf("persisted channel = %q", got)
	}
}

func TestNew_InfersChannelFromBuildVersion(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    Channel
	}{
		{"v1.2.3", ChannelStable},
		{"v1.3.0-beta.4", ChannelBeta},
		{"edge-20260712T120000Z-1234abc", ChannelBeta},
	} {
		c := New(Config{CurrentVersion: tc.version}, newMemStore())
		if got := c.Info().Channel; got != tc.want {
			t.Errorf("version %q inferred %q, want %q", tc.version, got, tc.want)
		}
	}
}

func TestNew_MigratesPersistedEdgeChannelToBeta(t *testing.T) {
	st := newMemStore()
	st.m[channelKey] = "edge"
	c := New(Config{CurrentVersion: "v1.2.3"}, st)
	if got := c.Info().Channel; got != ChannelBeta {
		t.Fatalf("channel = %q, want beta", got)
	}
	if got := st.m[channelKey]; got != "beta" {
		t.Fatalf("persisted channel = %q, want beta", got)
	}
}

func TestCheck_SameVersion(t *testing.T) {
	const repo = "srcfl/ftw"
	reg := newFakeRegistry(t, repo)
	reg.addTag("v2.0.0")
	rsrv := reg.server()
	defer rsrv.Close()

	rls := fakeReleasesServer(t, fakeRelease{tag: "v2.0.0"})
	defer rls.Close()

	c := newCheckerOnFakes("v2.0.0", rsrv, rls, repo, newMemStore())
	info, err := c.Check(context.Background(), false)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info.UpdateAvailable {
		t.Error("UpdateAvailable should be false when same version")
	}
}

func TestCheck_DevCurrent(t *testing.T) {
	const repo = "srcfl/ftw"
	reg := newFakeRegistry(t, repo)
	reg.addTag("v0.17.1")
	rsrv := reg.server()
	defer rsrv.Close()

	rls := fakeReleasesServer(t, fakeRelease{tag: "v0.17.1"})
	defer rls.Close()

	c := newCheckerOnFakes("dev", rsrv, rls, repo, newMemStore())
	info, _ := c.Check(context.Background(), false)
	if !info.UpdateAvailable {
		t.Error("dev builds should always see an upgrade as available")
	}
}

func TestCheck_CacheRespected(t *testing.T) {
	const repo = "srcfl/ftw"
	reg := newFakeRegistry(t, repo)
	reg.addTag("v1.0.0")
	rsrv := reg.server()
	defer rsrv.Close()

	rls := fakeReleasesServer(t, fakeRelease{tag: "v1.0.0"})
	defer rls.Close()

	c := newCheckerOnFakes("v0.9.0", rsrv, rls, repo, newMemStore())

	if _, err := c.Check(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	first := reg.tokenCalls
	if _, err := c.Check(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if reg.tokenCalls != first {
		t.Errorf("expected cache to suppress 2nd registry probe; tokenCalls=%d (was %d)", reg.tokenCalls, first)
	}
	if _, err := c.Check(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if reg.tokenCalls <= first {
		t.Errorf("force=true should re-probe; tokenCalls=%d", reg.tokenCalls)
	}
}

func TestCheck_GHReleasesError(t *testing.T) {
	rls := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte("github down"))
	}))
	defer rls.Close()

	reg := newFakeRegistry(t, "srcfl/ftw").server()
	defer reg.Close()

	c := newCheckerOnFakes("v1.0.0", reg, rls, "srcfl/ftw", newMemStore())
	if _, err := c.Check(context.Background(), false); err == nil {
		t.Fatal("expected error for 503")
	}
	if c.Info().Err == "" {
		t.Error("error should be recorded in Info.Err")
	}
}

func TestCheck_RegistryError(t *testing.T) {
	rsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte("registry down"))
	}))
	defer rsrv.Close()

	rls := fakeReleasesServer(t, fakeRelease{tag: "v1.1.0"})
	defer rls.Close()

	c := newCheckerOnFakes("v1.0.0", rsrv, rls, "srcfl/ftw", newMemStore())
	if _, err := c.Check(context.Background(), false); err == nil {
		t.Fatal("expected error when registry is unreachable")
	}
	if c.Info().Err == "" {
		t.Error("error should be recorded in Info.Err")
	}
}

func TestSkipAndUnskip(t *testing.T) {
	const repo = "srcfl/ftw"
	reg := newFakeRegistry(t, repo)
	reg.addTag("v1.3.0")
	rsrv := reg.server()
	defer rsrv.Close()

	rls := fakeReleasesServer(t, fakeRelease{tag: "v1.3.0"})
	defer rls.Close()

	st := newMemStore()
	c := newCheckerOnFakes("v1.2.0", rsrv, rls, repo, st)
	if _, err := c.Check(context.Background(), false); err != nil {
		t.Fatal(err)
	}

	if err := c.Skip("v1.3.0"); err != nil {
		t.Fatal(err)
	}
	if !c.Info().Skipped {
		t.Error("Skipped should be true after skipping latest")
	}
	if v, _ := st.LoadConfig("update.skipped_version"); v != "v1.3.0" {
		t.Errorf("persisted key = %q, want v1.3.0", v)
	}

	if err := c.Skip("v1.2.5"); err != nil {
		t.Fatal(err)
	}
	if c.Info().Skipped {
		t.Error("Skipping a non-latest version should not hide latest")
	}

	if err := c.Unskip(); err != nil {
		t.Fatal(err)
	}
	if c.Info().Skipped {
		t.Error("Skipped should be false after Unskip")
	}
}

func TestStatus_MissingFileReturnsIdle(t *testing.T) {
	c := New(Config{StatusPath: "/nonexistent/state.json"}, newMemStore())
	if s := c.Status(); s.State != "idle" {
		t.Errorf("missing status file = %q, want idle", s.State)
	}
}

func TestStatus_ReadsAndDetectsStale(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	fresh := UpdateStatus{State: "restoring", Action: "rollback", Snapshot: "snapshot-123", UpdatedAt: time.Now()}
	writeJSON(t, path, fresh)

	c := New(Config{StatusPath: path}, newMemStore())
	if s := c.Status(); s.State != "restoring" || s.Snapshot != "snapshot-123" {
		t.Errorf("fresh status = %+v, want restoring snapshot-123", s)
	}

	stale := UpdateStatus{State: "pulling", Action: "update", UpdatedAt: time.Now().Add(-10 * time.Minute)}
	writeJSON(t, path, stale)
	if s := c.Status(); s.State != "failed" {
		t.Errorf("stale state = %q, want failed", s.State)
	}
}

func TestWriteStatusPublishesPreSidecarState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	c := New(Config{StatusPath: path}, newMemStore())

	started := time.Now().Add(-time.Second)
	if err := c.WriteStatus(UpdateStatus{
		State:     "snapshotting",
		Action:    "update",
		Target:    "v1.5.0",
		StartedAt: started,
		Message:   "creating backup snapshot",
	}); err != nil {
		t.Fatalf("write status: %v", err)
	}

	got := c.Status()
	if got.State != "snapshotting" || got.Action != "update" || got.Target != "v1.5.0" {
		t.Fatalf("status = %+v, want snapshotting update v1.5.0", got)
	}
	if got.UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt should be filled")
	}
}

func TestWriteStatusPreservesPerComponentRollbackHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	c := New(Config{StatusPath: path}, newMemStore())

	if err := c.WriteStatus(UpdateStatus{
		State: "done", Component: "optimizer", PreviousImageID: "sha256:optimizer-old",
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteStatus(UpdateStatus{
		State: "starting", Component: "core", Target: "v1.5.0",
	}); err != nil {
		t.Fatal(err)
	}

	got := c.Status()
	if got.PreviousImages["optimizer"] != "sha256:optimizer-old" {
		t.Fatalf("optimizer rollback history = %+v", got.PreviousImages)
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(v); err != nil {
		t.Fatal(err)
	}
}

func TestIsNewer(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"v1.2.3", "v1.2.2", true},
		{"v1.2.3", "v1.2.3", false},
		{"v1.2.3", "v1.3.0", false},
		{"v2.0.0", "v1.99.99", true},
		{"v1.2.3", "dev", true},
		{"", "v1.2.3", false},
		{"v1.2.3-rc1", "v1.2.2", true},
		{"v1.3.0-beta.2", "v1.3.0-beta.1", true},
		{"v1.3.0-beta.1", "v1.3.0-beta.2", false},
		{"v1.3.0", "v1.3.0-beta.2", true},
		{"v1.3.0-beta.2", "v1.3.0", false},
		{"1.2.3", "1.2.2", true},
	}
	for _, tc := range cases {
		if got := isNewer(tc.latest, tc.current); got != tc.want {
			t.Errorf("isNewer(%q, %q) = %v, want %v", tc.latest, tc.current, got, tc.want)
		}
	}
}

func TestCheck_TruncatesHugeReleaseBody(t *testing.T) {
	const repo = "srcfl/ftw"
	reg := newFakeRegistry(t, repo)
	reg.addTag("v2.0.0")
	rsrv := reg.server()
	defer rsrv.Close()

	huge := strings.Repeat("* entry\n", (MaxReleaseBodyBytes/8)+200)
	if len(huge) <= MaxReleaseBodyBytes {
		t.Fatalf("test fixture too small: %d bytes", len(huge))
	}
	rls := fakeReleasesServer(t, fakeRelease{tag: "v2.0.0", body: huge})
	defer rls.Close()

	c := newCheckerOnFakes("v1.0.0", rsrv, rls, repo, newMemStore())
	info, err := c.Check(context.Background(), false)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(info.ReleaseBody) < MaxReleaseBodyBytes || len(info.ReleaseBody) > MaxReleaseBodyBytes+200 {
		t.Errorf("ReleaseBody length = %d, expected near %d+truncation marker", len(info.ReleaseBody), MaxReleaseBodyBytes)
	}
	if !strings.Contains(info.ReleaseBody, "truncated") {
		t.Error("truncated body should carry the truncation marker")
	}
}

func TestTrigger_ValidatesAction(t *testing.T) {
	c := New(Config{SocketPath: "/tmp/notreal.sock"}, newMemStore())
	if err := c.Trigger(context.Background(), "delete-everything", ""); err == nil {
		t.Error("expected invalid action error")
	}
}

func TestTrigger_NoSocket(t *testing.T) {
	c := New(Config{}, newMemStore())
	if err := c.Trigger(context.Background(), "update", ""); err == nil {
		t.Error("expected 'socket not configured' error")
	}
}
