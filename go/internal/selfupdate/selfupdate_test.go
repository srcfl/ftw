package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
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

// fakeRelease is one entry of the GitHub release list. Unless noAssets is
// set it carries this host's native package and checksum.
type fakeRelease struct {
	tag        string
	htmlURL    string
	body       string
	published  time.Time
	prerelease bool
	noAssets   bool
}

func (r fakeRelease) json() map[string]any {
	entry := map[string]any{
		"tag_name":     r.tag,
		"html_url":     r.htmlURL,
		"body":         r.body,
		"published_at": r.published.Format(time.RFC3339),
		"prerelease":   r.prerelease,
	}
	if !r.noAssets {
		archive := "ftw-linux-" + runtime.GOARCH + ".tar.gz"
		entry["assets"] = []map[string]string{{"name": archive}, {"name": archive + ".sha256"}}
	}
	return entry
}

// fakeReleasesServer serves the release list and counts requests.
func fakeReleasesServer(t *testing.T, calls *atomic.Int32, releases ...fakeRelease) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls != nil {
			calls.Add(1)
		}
		list := make([]map[string]any, 0, len(releases))
		for _, r := range releases {
			list = append(list, r.json())
		}
		_ = json.NewEncoder(w).Encode(list)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newCheckerOnFakes(t *testing.T, currentVersion string, releases *httptest.Server, st Store) *Checker {
	t.Helper()
	return New(Config{
		CurrentVersion: currentVersion,
		NativeRoot:     t.TempDir(),
		ReleasesURL:    releases.URL,
		CheckInterval:  time.Hour,
	}, st)
}

func TestCheck_UpdateAvailable(t *testing.T) {
	published := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)
	rls := fakeReleasesServer(t, nil, fakeRelease{
		tag: "v0.133.0", htmlURL: "https://example/releases/0.133.0",
		body: "## Features\n* shiny", published: published,
	})

	c := newCheckerOnFakes(t, "v0.132.4", rls, newMemStore())
	info, err := c.Check(context.Background(), false)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info.Latest != "v0.133.0" {
		t.Errorf("latest = %q, want v0.133.0", info.Latest)
	}
	if !info.UpdateAvailable {
		t.Error("UpdateAvailable should be true")
	}
	if info.ReleaseNotesURL != "https://example/releases/0.133.0" {
		t.Errorf("notes url = %q", info.ReleaseNotesURL)
	}
	if info.PublishedAt.IsZero() {
		t.Error("PublishedAt not parsed")
	}
}

func TestCheckRequiresBackupOnlyWhenReleaseSchemaIsMissingOrDifferent(t *testing.T) {
	for _, tc := range []struct {
		name           string
		body           string
		targetSchema   int
		backupRequired bool
	}{
		{name: "same", body: "<!-- ftw-state-schema:7 -->", targetSchema: 7, backupRequired: false},
		{name: "different", body: "<!-- ftw-state-schema:8 -->", targetSchema: 8, backupRequired: true},
		{name: "v2 wins over the legacy floor", body: "<!-- ftw-state-schema:4 -->\n<!-- ftw-state-schema-v2:7 -->", targetSchema: 7, backupRequired: false},
		{name: "v2 differs", body: "<!-- ftw-state-schema:4 -->\n<!-- ftw-state-schema-v2:8 -->", targetSchema: 8, backupRequired: true},
		{name: "invalid v2 falls back to legacy", body: "<!-- ftw-state-schema:7 -->\n<!-- ftw-state-schema-v2:no -->", targetSchema: 7, backupRequired: false},
		{name: "missing", body: "ordinary release notes", targetSchema: 0, backupRequired: true},
		{name: "invalid", body: "<!-- ftw-state-schema:no -->", targetSchema: 0, backupRequired: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			releases := fakeReleasesServer(t, nil, fakeRelease{
				tag: "v0.133.0", body: tc.body, published: time.Now(),
			})
			c := New(Config{
				CurrentVersion: "v0.132.0", CurrentStateSchema: 7,
				NativeRoot: t.TempDir(), ReleasesURL: releases.URL,
			}, newMemStore())
			info, err := c.Check(t.Context(), true)
			if err != nil {
				t.Fatal(err)
			}
			if info.CurrentStateSchema != 7 || info.TargetStateSchema != tc.targetSchema {
				t.Fatalf("schema info = %+v", info)
			}
			if info.FullBackupRequired != tc.backupRequired {
				t.Fatalf("FullBackupRequired = %v, want %v", info.FullBackupRequired, tc.backupRequired)
			}
			if strings.Contains(info.ReleaseBody, "ftw-state-schema") {
				t.Fatalf("internal schema marker leaked into release notes: %q", info.ReleaseBody)
			}
		})
	}
}

// The beta and stable workflows publish both markers. A Core before
// v3.6.0-beta.1 reads only the legacy one and must see its own schema, 4,
// so it skips the full history copy that cannot finish on a Raspberry Pi
// (#1302). A newer Core reads the real schema from the v2 marker.
func TestReleaseStateSchemaReadsV2BeforeLegacyMarker(t *testing.T) {
	body := "FTW 3.7.0\n\n<!-- ftw-state-schema:4 -->\n<!-- ftw-state-schema-v2:7 -->\n"
	if got := releaseStateSchema(body); got != 7 {
		t.Fatalf("v2 marker = %d, want 7", got)
	}
	if got := parseStateSchemaMarker(body, stateSchemaMarkerLegacy); got != 4 {
		t.Fatalf("legacy marker seen by an old Core = %d, want 4", got)
	}
	if got := releaseBodyWithoutStateSchema(body); got != "FTW 3.7.0" {
		t.Fatalf("stripped body = %q", got)
	}
	if got := releaseStateSchema("<!-- ftw-state-schema:7 -->"); got != 7 {
		t.Fatalf("legacy-only marker = %d, want 7", got)
	}
}

func TestCheck_NoReleasesYet(t *testing.T) {
	rls := fakeReleasesServer(t, nil)
	c := newCheckerOnFakes(t, "v0.131.0", rls, newMemStore())
	info, err := c.Check(context.Background(), false)
	if err != nil {
		t.Fatalf("an empty release list should not error: %v", err)
	}
	if info.UpdateAvailable {
		t.Error("no releases → no update available")
	}
}

func TestCheck_StableIgnoresPrerelease(t *testing.T) {
	rls := fakeReleasesServer(t, nil, fakeRelease{tag: "v0.135.0-beta.1", prerelease: true})
	c := newCheckerOnFakes(t, "v0.134.0", rls, newMemStore())
	info, err := c.Check(context.Background(), false)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info.UpdateAvailable {
		t.Error("a prerelease must not be offered on stable")
	}
}

func TestCheck_BetaChannelSelectsNewestBetaAndPersistsChannel(t *testing.T) {
	releases := fakeReleasesServer(t, nil,
		fakeRelease{tag: "v0.135.0-rc.1", prerelease: true},
		fakeRelease{tag: "v0.135.0-beta.2", prerelease: true},
		fakeRelease{tag: "v0.135.0-beta.1", prerelease: true},
	)
	st := newMemStore()
	c := newCheckerOnFakes(t, "v0.134.0", releases, st)
	if err := c.SetChannel(ChannelBeta); err != nil {
		t.Fatalf("SetChannel: %v", err)
	}
	info, err := c.Check(context.Background(), true)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info.Channel != ChannelBeta || info.Latest != "v0.135.0-beta.2" || !info.UpdateAvailable {
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
		{"v0.132.3", ChannelStable},
		{"v0.133.0-beta.4", ChannelBeta},
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
	c := New(Config{CurrentVersion: "v0.132.3"}, st)
	if got := c.Info().Channel; got != ChannelBeta {
		t.Fatalf("channel = %q, want beta", got)
	}
	if got := st.m[channelKey]; got != "beta" {
		t.Fatalf("persisted channel = %q, want beta", got)
	}
}

func TestCheck_SameVersion(t *testing.T) {
	rls := fakeReleasesServer(t, nil, fakeRelease{tag: "v0.132.0"})
	c := newCheckerOnFakes(t, "v0.132.0", rls, newMemStore())
	info, err := c.Check(context.Background(), false)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info.UpdateAvailable {
		t.Error("UpdateAvailable should be false when same version")
	}
}

func TestCheck_DevCurrent(t *testing.T) {
	rls := fakeReleasesServer(t, nil, fakeRelease{tag: "v0.132.1"})
	c := newCheckerOnFakes(t, "dev", rls, newMemStore())
	info, _ := c.Check(context.Background(), false)
	if !info.UpdateAvailable {
		t.Error("dev builds should always see an upgrade as available")
	}
}

func TestCheck_CacheRespected(t *testing.T) {
	var calls atomic.Int32
	rls := fakeReleasesServer(t, &calls, fakeRelease{tag: "v0.133.0"})
	c := newCheckerOnFakes(t, "v0.132.0", rls, newMemStore())

	if _, err := c.Check(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	first := calls.Load()
	if _, err := c.Check(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != first {
		t.Errorf("expected cache to suppress the 2nd release check; calls=%d (was %d)", calls.Load(), first)
	}
	if _, err := c.Check(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if calls.Load() <= first {
		t.Errorf("force=true should check again; calls=%d", calls.Load())
	}
}

func withoutGitHubBackoff(t *testing.T) {
	t.Helper()
	old := githubRetryBackoff
	githubRetryBackoff = func(time.Duration) {}
	t.Cleanup(func() { githubRetryBackoff = old })
}

func TestCheck_GHReleasesError(t *testing.T) {
	withoutGitHubBackoff(t)
	var calls int
	rls := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(503)
		_, _ = w.Write([]byte("github down"))
	}))
	defer rls.Close()

	c := newCheckerOnFakes(t, "v0.132.0", rls, newMemStore())
	if _, err := c.Check(context.Background(), false); err == nil {
		t.Fatal("expected error for 503")
	}
	if c.Info().Err == "" {
		t.Error("error should be recorded in Info.Err")
	}
	if calls != githubRetryLimit {
		t.Fatalf("github 503 calls = %d, want %d retries", calls, githubRetryLimit)
	}
}

// flakyReleasesServer answers 504 for the first failures calls, then the
// release list.
func flakyReleasesServer(t *testing.T, failures int, calls *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		if *calls <= failures {
			w.WriteHeader(http.StatusGatewayTimeout)
			_, _ = w.Write([]byte("gateway timeout"))
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			fakeRelease{tag: "v0.133.0", htmlURL: "https://example/releases/v0.133.0", body: "ok", published: time.Now()}.json(),
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCheck_RetriesTransientGitHub504(t *testing.T) {
	withoutGitHubBackoff(t)
	var calls int
	rls := flakyReleasesServer(t, 2, &calls)
	c := newCheckerOnFakes(t, "v0.132.0", rls, newMemStore())
	info, err := c.Check(context.Background(), false)
	if err != nil {
		t.Fatalf("Check after 504s: %v", err)
	}
	if info.Latest != "v0.133.0" || info.Err != "" {
		t.Fatalf("info = %+v", info)
	}
	if calls != 3 {
		t.Fatalf("github calls = %d, want 3", calls)
	}
}

func TestCheck_DoesNotCacheGitHubError(t *testing.T) {
	withoutGitHubBackoff(t)
	var calls int
	rls := flakyReleasesServer(t, githubRetryLimit, &calls)
	c := newCheckerOnFakes(t, "v0.132.0", rls, newMemStore())
	if _, err := c.Check(context.Background(), false); err == nil {
		t.Fatal("first check should fail after exhausted 504s")
	}
	failedCalls := calls
	info, err := c.Check(context.Background(), false)
	if err != nil {
		t.Fatalf("retry after recorded 504: %v", err)
	}
	if info.Latest != "v0.133.0" || info.Err != "" {
		t.Fatalf("info after retry = %+v", info)
	}
	if calls <= failedCalls {
		t.Fatal("second Check used the error cache instead of contacting GitHub")
	}
}

func TestSkipAndUnskip(t *testing.T) {
	rls := fakeReleasesServer(t, nil, fakeRelease{tag: "v0.133.0"})
	st := newMemStore()
	c := newCheckerOnFakes(t, "v0.132.0", rls, st)
	if _, err := c.Check(context.Background(), false); err != nil {
		t.Fatal(err)
	}

	if err := c.Skip("v0.133.0"); err != nil {
		t.Fatal(err)
	}
	if !c.Info().Skipped {
		t.Error("Skipped should be true after skipping latest")
	}
	if v, _ := st.LoadConfig("update.skipped_version"); v != "v0.133.0" {
		t.Errorf("persisted key = %q, want v0.133.0", v)
	}

	if err := c.Skip("v0.132.5"); err != nil {
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
	fresh := UpdateStatus{State: "pulling", Action: "update", Target: "v0.133.0", UpdatedAt: time.Now()}
	writeJSON(t, path, fresh)

	c := New(Config{StatusPath: path}, newMemStore())
	if s := c.Status(); s.State != "pulling" || s.Target != "v0.133.0" {
		t.Errorf("fresh status = %+v, want pulling v0.133.0", s)
	}

	stale := UpdateStatus{
		State:          "pulling",
		Action:         "update",
		PhaseStartedAt: time.Now().Add(-10 * time.Minute),
		UpdatedAt:      time.Now().Add(-10 * time.Minute),
	}
	writeJSON(t, path, stale)
	if s := c.Status(); s.State != "failed" {
		t.Errorf("stale state = %q, want failed", s.State)
	}
	stale.State = "checking"
	writeJSON(t, path, stale)
	if s := c.Status(); s.State != "failed" {
		t.Errorf("stale checking state = %q, want failed", s.State)
	}
}

func TestStatus_NativeRestartLastsUntilTheTrialDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	now := time.Now()
	c := New(Config{StatusPath: path, NativeRoot: t.TempDir(), NativeTrialTimeout: 6 * time.Hour,
		Now: func() time.Time { return now }}, nil)
	restarting := UpdateStatus{State: "restarting", Action: "update", Target: "v0.133.0",
		PhaseStartedAt: now.Add(-20 * time.Minute), UpdatedAt: now.Add(-20 * time.Minute)}

	writeJSON(t, path, restarting)
	if got := c.Status(); got.State != "restarting" {
		t.Fatalf("20-minute native start = %q, want restarting", got.State)
	}
	restarting.UpdatedAt = now.Add(-6*time.Hour - time.Second)
	writeJSON(t, path, restarting)
	if got := c.Status(); got.State != "failed" {
		t.Fatalf("native start past the trial deadline = %q, want failed", got.State)
	}
	restarting.State, restarting.UpdatedAt = "pulling", now.Add(-20*time.Minute)
	writeJSON(t, path, restarting)
	if got := c.Status(); got.State != "failed" {
		t.Fatalf("silent native download = %q, want failed", got.State)
	}
}

func TestWriteStatusRoundTripsProgress(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	c := New(Config{StatusPath: path}, newMemStore())

	started := time.Now().Add(-time.Second)
	if err := c.WriteStatus(UpdateStatus{
		State:           "pulling",
		Action:          "update",
		Target:          "v0.133.0",
		StartedAt:       started,
		PhaseStartedAt:  started,
		Message:         "Downloading verified Core release",
		Step:            1,
		TotalSteps:      NativeUpdateSteps,
		ProgressCurrent: 50,
		ProgressTotal:   100,
		ProgressUnit:    "bytes",
	}); err != nil {
		t.Fatalf("write status: %v", err)
	}

	got := c.Status()
	if got.State != "pulling" || got.Action != "update" || got.Target != "v0.133.0" {
		t.Fatalf("status = %+v, want pulling update v0.133.0", got)
	}
	if got.UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt should be filled")
	}
	if got.Step != 1 || got.TotalSteps != NativeUpdateSteps || got.ProgressCurrent != 50 ||
		got.ProgressTotal != 100 || got.ProgressUnit != "bytes" || got.PhaseStartedAt.IsZero() {
		t.Fatalf("progress fields = %+v", got)
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
		{"v1.3.2-beta.1", "v1.3.1-beta.1", true},
		{"v1.3.2-beta.1", "v1.3.1", true},
	}
	for _, tc := range cases {
		if got := isNewer(tc.latest, tc.current); got != tc.want {
			t.Errorf("isNewer(%q, %q) = %v, want %v", tc.latest, tc.current, got, tc.want)
		}
	}
}

func TestCheck_TruncatesHugeReleaseBody(t *testing.T) {
	huge := strings.Repeat("* entry\n", (MaxReleaseBodyBytes/8)+200)
	if len(huge) <= MaxReleaseBodyBytes {
		t.Fatalf("test fixture too small: %d bytes", len(huge))
	}
	rls := fakeReleasesServer(t, nil, fakeRelease{tag: "v0.133.0", body: huge})
	c := newCheckerOnFakes(t, "v0.132.0", rls, newMemStore())
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

func TestTriggersNeedANativeInstall(t *testing.T) {
	c := New(Config{CurrentVersion: "v0.132.0"}, newMemStore())
	if err := c.TriggerUpdate(context.Background(), "v0.133.0", time.Time{}); err == nil {
		t.Error("update without release slots reported success")
	}
	if err := c.TriggerRestart(); err == nil {
		t.Error("restart without release slots reported success")
	}
	if _, err := c.TriggerNativeRollback(); err == nil {
		t.Error("rollback without release slots reported success")
	}
}

// A restart starts the same release again: the saved status names the
// running version and nothing else is selected.
func TestTriggerRestartRecordsTheRunningVersion(t *testing.T) {
	root := t.TempDir()
	restarts := 0
	c := New(Config{CurrentVersion: "v0.132.0", NativeRoot: root,
		StatusPath:    filepath.Join(root, "update-status.json"),
		NativeRestart: func() error { restarts++; return nil }}, newMemStore())
	if err := c.TriggerRestart(); err != nil {
		t.Fatal(err)
	}
	if st := c.Status(); restarts != 1 || st.State != "restarting" || st.Action != "restart" || st.Target != "v0.132.0" {
		t.Fatalf("restarts=%d status=%+v", restarts, st)
	}

	c.cfg.NativeRestart = func() error { return errors.New("stop refused") }
	if err := c.TriggerRestart(); err == nil {
		t.Fatal("refused restart reported success")
	}
	if st := c.Status(); st.State != "failed" || st.Message != "stop refused" {
		t.Fatalf("refused restart status = %+v", st)
	}
}

// Absence of knowledge must not read as an old version: an empty current
// version would otherwise compare as older than everything and light the
// update badge forever.
func TestUnknownCurrentVersionDoesNotClaimAnUpdate(t *testing.T) {
	for _, tc := range []struct {
		name, latest, current string
		want                  bool
	}{
		{name: "unknown current version", latest: "v0.133.2", current: "", want: false},
		{name: "whitespace is still unknown", latest: "v0.133.2", current: "   ", want: false},
		// An unstamped local build reports a real value and keeps the update
		// flow testable.
		{name: "unstamped dev build still sees releases", latest: "v0.133.2", current: "dev", want: true},
		{name: "stable site on the newest release", latest: "v0.133.0", current: "v0.133.0", want: false},
		{name: "stable site behind a release", latest: "v0.133.1", current: "v0.133.0", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := channelUpdateAvailable(tc.latest, tc.current); got != tc.want {
				t.Fatalf("channelUpdateAvailable(%q, %q) = %v, want %v", tc.latest, tc.current, got, tc.want)
			}
		})
	}
}
