package ftwcli

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func testEnv() env {
	return env{now: time.Now, sleep: time.Sleep, requestTimeout: time.Second,
		pollInterval: time.Millisecond, followLimit: 5 * time.Second, logEvery: time.Hour,
		healthSettle: 20 * time.Millisecond, systemd: true}
}

func runCLI(t *testing.T, e env, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut strings.Builder
	code := run(args, &out, &errOut, e)
	return code, out.String(), errOut.String()
}

// fakeCore answers the routes the CLI uses and records every POST.
type fakeCore struct {
	mu     sync.Mutex
	routes map[string]http.HandlerFunc
	posts  []string
	bodies []string
}

func newFakeCore(t *testing.T) (*fakeCore, *httptest.Server) {
	f := &fakeCore{routes: map[string]http.HandlerFunc{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		f.mu.Lock()
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			f.posts = append(f.posts, r.URL.Path)
			f.bodies = append(f.bodies, string(body))
		}
		handler := f.routes[key]
		f.mu.Unlock()
		if handler == nil {
			t.Errorf("unexpected %s", key)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	// A Core older than the driver lines answers the catalog without them.
	f.on("GET", "/api/drivers/catalog", reply(404, "404 page not found"))
	return f, srv
}

func (f *fakeCore) on(method, path string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[method+" "+path] = h
}

func (f *fakeCore) postedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.posts...)
}

func reply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// sequence answers each call with the next body and repeats the last one.
func sequence(bodies ...string) http.HandlerFunc {
	var mu sync.Mutex
	next := 0
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		body := bodies[min(next, len(bodies)-1)]
		next++
		mu.Unlock()
		status := http.StatusOK
		if strings.HasPrefix(body, "503 ") {
			status, body = http.StatusServiceUnavailable, strings.TrimPrefix(body, "503 ")
		}
		reply(status, body)(w, r)
	}
}

const (
	oldCore = `{"current":"v0.132.1-beta.1","native":true,"channel":"beta","previous":"v0.132.0-beta.1"}`
	offer   = `{"current":"v0.132.1-beta.1","native":true,"channel":"beta","latest":"v0.132.2-beta.1","update_available":true,"install_root":"/opt/ftw","install_free_bytes":61000000000,"install_need_bytes":210000000}`
	newCore = `{"current":"v0.132.2-beta.1","native":true,"channel":"beta","previous":"v0.132.1-beta.1","install_root":"/opt/ftw"}`
)

func TestHelpAndCommandLineErrors(t *testing.T) {
	e := testEnv()
	for _, args := range [][]string{nil, {"help"}, {"--help"}} {
		if code, out, _ := runCLI(t, e, args...); code != exitOK || !strings.Contains(out, "ftw update [--channel beta|stable]") {
			t.Fatalf("%v: %d\n%s", args, code, out)
		}
	}
	if code, out, _ := runCLI(t, e, "update", "--help"); code != exitOK || !strings.Contains(out, "usage: ftw update") {
		t.Fatalf("update --help: %d %s", code, out)
	}
	for _, args := range [][]string{
		{"doctor"}, {"update", "--channel", "nightly"}, {"update", "--channel"},
		{"update", "now"}, {"status", "--url", "127.0.0.1:8080"}, {"backup", "--port", "9090"},
	} {
		if code, _, errOut := runCLI(t, e, args...); code != exitUsage || !strings.Contains(errOut, "ftw help") {
			t.Fatalf("%v: exit %d, stderr %q", args, code, errOut)
		}
	}
}

func TestUpdateRollsThroughTheRestart(t *testing.T) {
	f, srv := newFakeCore(t)
	var mu sync.Mutex
	finished := false
	f.on("GET", "/api/version/check", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case finished:
			reply(200, newCore)(w, r)
		case r.URL.Query().Get("force") == "1":
			reply(200, offer)(w, r)
		default:
			reply(200, oldCore)(w, r)
		}
	})
	f.on("GET", "/api/version/update/status", sequence(
		`{"state":"done","action":"update","target":"v0.132.1-beta.1"}`,
		`{"state":"starting","action":"update","target":"v0.132.2-beta.1","total_steps":3,"message":"starting update"}`,
		`{"state":"pulling","action":"update","target":"v0.132.2-beta.1","step":1,"total_steps":3,"message":"Downloading verified Core release","progress_current":10000000,"progress_total":21600000,"progress_unit":"bytes"}`,
		`{"state":"restarting","action":"update","target":"v0.132.2-beta.1","step":3,"total_steps":3,"message":"Starting the new Core once"}`,
		`503 {"error":"starting"}`,
		`503 {"error":"starting"}`,
		`{"state":"done","action":"update","target":"v0.132.2-beta.1","step":3,"total_steps":3,"message":"Core is ready on v0.132.2-beta.1"}`,
	))
	f.on("POST", "/api/version/update", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		finished = true
		mu.Unlock()
		reply(202, `{"status":"started","target":"v0.132.2-beta.1"}`)(w, r)
	})
	f.on("GET", "/api/health", sequence(`{"status":"degraded","drivers_ok":2,"drivers_offline":1}`, `{"status":"ok","drivers_ok":3}`))

	code, out, errOut := runCLI(t, testEnv(), "update", "--url", srv.URL)
	if code != exitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{
		"Updating v0.132.1-beta.1 -> v0.132.2-beta.1 on beta",
		"Releases: /opt/ftw/releases (61.0 GB free, the next release needs 210.0 MB)",
		"1/3 Downloading verified Core release   46%  10.0 MB of 21.6 MB",
		"✓ 1/3 Downloading verified Core release  10.0 MB in ",
		"3/3 Starting the new Core once",
		"✓ 3/3 Starting the new Core once  in ",
		"Now running v0.132.2-beta.1 (was v0.132.1-beta.1)",
		"Previous: /opt/ftw/releases/v0.132.1-beta.1 (ftw rollback returns to it)",
		"Health: ok; drivers 3 ok, 0 degraded, 0 offline, 0 faulted.",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if got := strings.Join(f.postedPaths(), ","); got != "/api/version/update" {
		t.Fatalf("posts %s", got)
	}
}

func TestUpdateChangesTheChannelOnlyWhenAsked(t *testing.T) {
	f, srv := newFakeCore(t)
	f.on("GET", "/api/version/check", reply(200, oldCore))
	f.on("GET", "/api/version/update/status", reply(200, `{"state":"idle"}`))
	f.on("POST", "/api/version/channel", reply(200, `{}`))

	code, out, errOut := runCLI(t, testEnv(), "update", "--channel", "beta", "--url", srv.URL)
	if code != exitOK || !strings.Contains(out, "Already current on beta: v0.132.1-beta.1") || len(f.postedPaths()) != 0 {
		t.Fatalf("same channel: %d %v\n%s%s", code, f.postedPaths(), out, errOut)
	}
	code, out, errOut = runCLI(t, testEnv(), "update", "--channel=stable", "--url", srv.URL)
	if code != exitOK || !strings.Contains(out, "Channel: beta -> stable") {
		t.Fatalf("new channel: %d\n%s%s", code, out, errOut)
	}
	if f.postedPaths()[0] != "/api/version/channel" || f.bodies[0] != `{"channel":"stable"}` {
		t.Fatalf("posted %v %v", f.posts, f.bodies)
	}
}

func TestUpdateRefusesACoreThatIsNotNative(t *testing.T) {
	for _, check := range []http.HandlerFunc{
		reply(200, `{"current":"v2.3.2","native":false,"channel":"stable","update_available":true,"latest":"v2.3.3"}`),
		reply(503, `{"error":"self-update disabled"}`),
	} {
		f, srv := newFakeCore(t)
		f.on("GET", "/api/version/check", check)
		f.on("GET", "/api/health", reply(200, `{"status":"ok"}`))
		code, _, errOut := runCLI(t, testEnv(), "update", "--channel", "beta", "--url", srv.URL)
		if code != exitFailed || !strings.Contains(errOut, "not a native install") || len(f.postedPaths()) != 0 {
			t.Fatalf("exit %d posts %v stderr %q", code, f.postedPaths(), errOut)
		}
	}
}

func TestUpdateStopsBeforeAStateSchemaChange(t *testing.T) {
	f, srv := newFakeCore(t)
	f.on("GET", "/api/version/check", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("force") == "1" {
			reply(200, `{"current":"v0.132.1-beta.1","native":true,"channel":"beta","latest":"v0.133.0-beta.1","update_available":true,
				"full_backup_required":true,"current_state_schema":7,"target_state_schema":8}`)(w, r)
			return
		}
		reply(200, oldCore)(w, r)
	})
	f.on("GET", "/api/version/update/status", reply(200, `{"state":"idle"}`))
	code, _, errOut := runCLI(t, testEnv(), "update", "--url", srv.URL)
	if code != exitFailed || !strings.Contains(errOut, "state schema 7 -> 8") || len(f.postedPaths()) != 0 {
		t.Fatalf("exit %d posts %v stderr %q", code, f.postedPaths(), errOut)
	}
}

func TestUpdateFollowsARunThatIsAlreadyGoing(t *testing.T) {
	f, srv := newFakeCore(t)
	f.on("GET", "/api/version/check", sequence(oldCore, newCore))
	f.on("GET", "/api/version/update/status", sequence(
		`{"state":"pulling","action":"update","target":"v0.132.2-beta.1","message":"Downloading verified Core release"}`,
		`{"state":"done","action":"update","target":"v0.132.2-beta.1","message":"Core is ready on v0.132.2-beta.1"}`,
	))
	f.on("GET", "/api/health", reply(200, `{"status":"ok"}`))
	code, out, errOut := runCLI(t, testEnv(), "update", "--url", srv.URL)
	if code != exitOK || !strings.Contains(out, "already running; following it") || len(f.postedPaths()) != 0 {
		t.Fatalf("exit %d posts %v\n%s%s", code, f.postedPaths(), out, errOut)
	}
}

func TestUpdateReportsAFailedTrialAndWhatRuns(t *testing.T) {
	f, srv := newFakeCore(t)
	f.on("GET", "/api/version/check", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("force") == "1" {
			reply(200, offer)(w, r)
			return
		}
		reply(200, oldCore)(w, r)
	})
	f.on("GET", "/api/version/update/status", sequence(
		`{"state":"idle"}`,
		`{"state":"restarting","action":"update","target":"v0.132.2-beta.1"}`,
		`{"state":"failed","action":"update","target":"v0.132.2-beta.1","message":"New Core did not reach readiness; previous Core is running"}`,
	))
	f.on("POST", "/api/version/update", reply(202, `{"target":"v0.132.2-beta.1"}`))
	code, _, errOut := runCLI(t, testEnv(), "update", "--url", srv.URL)
	if code != exitFailed || !strings.Contains(errOut, "did not reach readiness") || !strings.Contains(errOut, "Core v0.132.1-beta.1 is running") {
		t.Fatalf("exit %d stderr %q", code, errOut)
	}
}

func TestUpdateGivesUpOnACoreThatNeverAnswers(t *testing.T) {
	release := make(chan struct{})
	f, srv := newFakeCore(t)
	t.Cleanup(func() { close(release) })
	hang := func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}
	f.on("GET", "/api/version/check", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("force") == "1" {
			reply(200, offer)(w, r)
			return
		}
		reply(200, oldCore)(w, r)
	})
	f.on("POST", "/api/version/update", reply(202, `{"target":"v0.132.2-beta.1"}`))
	f.on("GET", "/api/version/update/status", hang)
	f.on("GET", "/api/health", hang)
	e := testEnv()
	e.requestTimeout, e.followLimit = 50*time.Millisecond, 300*time.Millisecond
	start := time.Now()
	code, out, errOut := runCLI(t, e, "update", "--url", srv.URL)
	if code != exitFailed || !strings.Contains(errOut, "no result after") || time.Since(start) > 3*time.Second {
		t.Fatalf("exit %d after %s\n%s%s", code, time.Since(start), out, errOut)
	}
}

func TestRollbackReturnsToThePreviousRelease(t *testing.T) {
	f, srv := newFakeCore(t)
	f.on("GET", "/api/version/check", sequence(newCore, `{"current":"v0.132.1-beta.1","native":true,"channel":"beta"}`))
	f.on("GET", "/api/version/update/status", sequence(
		`{"state":"done","action":"update","target":"v0.132.2-beta.1"}`,
		`{"state":"restarting","action":"rollback","target":"v0.132.1-beta.1","message":"Starting the previous Core once"}`,
		`{"state":"done","action":"rollback","target":"v0.132.1-beta.1","message":"Core is ready on v0.132.1-beta.1"}`,
	))
	f.on("POST", "/api/version/binary-rollback", reply(202, `{"status":"started","action":"rollback","target":"v0.132.1-beta.1"}`))
	f.on("GET", "/api/health", reply(200, `{"status":"degraded","drivers_offline":1}`))
	code, out, errOut := runCLI(t, testEnv(), "rollback", "--url", srv.URL)
	if code != exitOK || !strings.Contains(out, "Returning v0.132.2-beta.1 -> v0.132.1-beta.1") || !strings.Contains(out, "Now running v0.132.1-beta.1") ||
		!strings.Contains(out, "Health is degraded: drivers 0 ok, 0 degraded, 1 offline") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}

	f, srv = newFakeCore(t)
	f.on("GET", "/api/version/check", reply(200, `{"current":"v0.132.1-beta.1","native":true,"channel":"beta"}`))
	if code, _, errOut := runCLI(t, testEnv(), "rollback", "--url", srv.URL); code != exitFailed || !strings.Contains(errOut, "no previous release") {
		t.Fatalf("without previous: %d %q", code, errOut)
	}
}

func TestStatusShowsReleaseLastRunAndHealth(t *testing.T) {
	f, srv := newFakeCore(t)
	f.on("GET", "/api/health", reply(200, `{"status":"ok","drivers_ok":1,"history_storage":{"migration":{"state":"complete"},"writer":{"commit_failures":0}}}`))
	f.on("GET", "/api/version/check", reply(200, `{"current":"v0.132.1-beta.1","native":true,"channel":"beta","latest":"v0.132.2-beta.1","update_available":true,"previous":"v0.132.0-beta.1",
		"install_root":"/opt/ftw","install_free_bytes":61000000000,"install_need_bytes":210000000}`))
	f.on("GET", "/api/version/update/status", reply(200, `{"state":"failed","action":"update","target":"v0.132.1-beta.1","message":"New Core did not reach readiness; previous Core is running"}`))
	f.on("GET", "/api/version/snapshots", reply(200, `{"dir":"/var/lib/ftw/snapshots","snapshots":[{"size_bytes":453000000},{"size_bytes":453000000}]}`))
	f.on("GET", "/api/backups", reply(200, `{"dir":"/var/lib/ftw/backups","free_bytes":61000000000,"backups":[{"size_bytes":95000}]}`))
	code, out, _ := runCLI(t, testEnv(), "status", "--url", srv.URL)
	for _, want := range []string{
		"Core:     v0.132.1-beta.1, native, beta channel",
		"Release:  v0.132.2-beta.1 is published. Install it with: ftw update",
		"Update:   last update FAILED: New Core did not reach readiness",
		"Previous: v0.132.0-beta.1 (ftw rollback returns to it)",
		"Releases: /opt/ftw/releases (61.0 GB free, the next release needs 210.0 MB)",
		"Unused:   2 rollback points from older updates, 906.0 MB, in /var/lib/ftw/snapshots; native Core does not use them",
		"Backups:  /var/lib/ftw/backups, 1 archives, 61.0 GB free",
		"Health:   ok; drivers 1 ok",
		"History:  complete; 0 write failures",
		"journalctl -u ftw",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}

	if strings.Contains(out, "Drivers:") {
		t.Fatalf("a Core without used_by must leave the drivers line out:\n%s", out)
	}
	f.on("GET", "/api/drivers/catalog", reply(200, `{"entries":[
		{"version":"1.5.8","source":"bundled","used_by":["sungrow"]},
		{"version":"1.3.2","source":"managed","used_by":["easee"],"release_version":"1.3.3"},
		{"version":"0.1.0","source":"local","used_by":["meter"]},
		{"version":"2.1.2","source":"bundled"}]}`))
	_, out, _ = runCLI(t, testEnv(), "status", "--url", srv.URL)
	for _, want := range []string{
		"Drivers:  easee 1.3.2, meter 0.1.0, sungrow 1.5.8\n",
		"Override: easee 1.3.2 from the driver channel; the release has 1.3.3",
		"Override: meter runs a local file; the release has no copy",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "sungrow runs") || strings.Contains(out, "2.1.2") {
		t.Fatalf("a release driver or an unused file got its own line:\n%s", out)
	}

	f.on("GET", "/api/health", reply(200, `{"status":"degraded","drivers_offline":1}`))
	if code, _, errOut := runCLI(t, testEnv(), "status", "--url", srv.URL); code != exitFailed || !strings.Contains(errOut, "health is degraded") {
		t.Fatalf("degraded: %d %q", code, errOut)
	}
	f.on("GET", "/api/health", reply(200, `{"status":"ok"}`))
	f.on("GET", "/api/version/check", reply(200, `{"current":"v0.132.1-beta.1","native":true,"channel":"beta","install_root":"/opt/ftw","install_free_bytes":90000000,"install_need_bytes":210000000}`))
	if code, out, errOut := runCLI(t, testEnv(), "status", "--url", srv.URL); code != exitFailed ||
		!strings.Contains(errOut, "not enough disk space for the next release") || !strings.Contains(out, "90.0 MB free") {
		t.Fatalf("low disk: %d %q\n%s", code, errOut, out)
	}
	f.on("GET", "/api/health", reply(200, `{"status":"starting","phase":"initializing state"}`))
	if code, out, _ := runCLI(t, testEnv(), "status", "--url", srv.URL); code != exitFailed || !strings.Contains(out, "starting: initializing state") {
		t.Fatalf("starting: %d %s", code, out)
	}
	f.on("GET", "/api/health", reply(200, `{"status":"ok"}`))
	f.on("GET", "/api/version/check", reply(503, `{"error":"self-update disabled"}`))
	f.on("GET", "/api/status", reply(200, `{"version":"v2.3.2"}`))
	if code, out, _ := runCLI(t, testEnv(), "status", "--url", srv.URL); code != exitOK || !strings.Contains(out, "v2.3.2, updates are managed outside FTW") {
		t.Fatalf("self-update off: %d %s", code, out)
	}
	container := testEnv()
	container.systemd = false
	if code, out, _ := runCLI(t, container, "status", "--url", srv.URL); code != exitOK || strings.Contains(out, "journalctl") || strings.Contains(out, "systemctl") {
		t.Fatalf("container: %d %s", code, out)
	}

	srv.Close()
	if code, out, _ := runCLI(t, testEnv(), "status", "--url", srv.URL); code != exitFailed || !strings.Contains(out, "not answering") || !strings.Contains(out, "journalctl -u ftw") {
		t.Fatalf("down: %d %s", code, out)
	}
	if code, out, _ := runCLI(t, container, "status", "--url", srv.URL); code != exitFailed || !strings.Contains(out, "not answering") || strings.Contains(out, "ftw-launcher") {
		t.Fatalf("container down: %d %s", code, out)
	}
}

func TestBackupCopiesOnlyAVerifiedMatch(t *testing.T) {
	archive := []byte("verified archive bytes")
	sum := sha256.Sum256(archive)
	digest := hex.EncodeToString(sum[:])
	id := "ftw-full-backup-20260924T052200.199Z.ftwbak"
	f, srv := newFakeCore(t)
	created, _ := json.Marshal(map[string]any{"backup": map[string]any{"id": id, "sha256": digest, "size_bytes": len(archive), "verified": true}})
	f.on("POST", "/api/backups", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		reply(201, string(created))(w, r)
	})
	f.on("GET", "/api/backups", reply(200, `{"dir":"/var/lib/ftw/backups","progress":{"phase":"packing_archive","completed_bytes":5,"total_bytes":22}}`))
	served := archive
	f.on("GET", "/api/backups/"+id, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(served) })

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, id+".part"), []byte("interrupted"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCLI(t, testEnv(), "backup", "--output-dir", dir, "--url", srv.URL)
	if code != exitOK || !strings.Contains(out, "Backup on the box: /var/lib/ftw/backups/"+id) || !strings.Contains(out, "Copied and checked") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	got, err := os.ReadFile(filepath.Join(dir, id))
	if err != nil || !bytes.Equal(got, archive) {
		t.Fatalf("copy %q %v", got, err)
	}
	if info, _ := os.Stat(filepath.Join(dir, id)); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
	if _, err := os.Stat(filepath.Join(dir, id+".part")); !os.IsNotExist(err) {
		t.Fatalf("leftover part: %v", err)
	}
	if code, out, _ := runCLI(t, testEnv(), "backup", "--output-dir", dir, "--url", srv.URL); code != exitOK || !strings.Contains(out, "Already saved") {
		t.Fatalf("second copy: %d %s", code, out)
	}

	served = []byte("a different archive!!!")
	other := t.TempDir()
	if code, _, errOut := runCLI(t, testEnv(), "backup", "--output-dir", other, "--url", srv.URL); code != exitFailed || !strings.Contains(errOut, "does not match") {
		t.Fatalf("mismatch: %d %q", code, errOut)
	}
	if entries, _ := os.ReadDir(other); len(entries) != 0 {
		t.Fatalf("a mismatched copy left %v", entries)
	}
}

func TestSupportWritesAPrivateReadableZip(t *testing.T) {
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	w, _ := zw.Create("00-help-report.md")
	_, _ = w.Write([]byte("# FTW help report\n"))
	_ = zw.Close()
	f, srv := newFakeCore(t)
	served := archive.Bytes()
	f.on("GET", "/api/support/dump", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(served) })

	t.Chdir(t.TempDir())
	e := testEnv()
	e.now = func() time.Time { return time.Date(2026, 9, 24, 7, 30, 0, 0, time.Local) }
	code, out, errOut := runCLI(t, e, "support", "--url", srv.URL)
	name := "ftw-support-20260924-073000.zip"
	if code != exitOK || !strings.Contains(out, name) {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if info, err := os.Stat(name); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("support file %v %v", info, err)
	}

	served = served[:len(served)/2]
	if code, _, errOut := runCLI(t, testEnv(), "support", "--output", "cut.zip", "--url", srv.URL); code != exitFailed || !strings.Contains(errOut, "not written") {
		t.Fatalf("cut zip: %d %q", code, errOut)
	}
	if _, err := os.Stat("cut.zip"); !os.IsNotExist(err) {
		t.Fatalf("cut zip kept: %v", err)
	}
}

func TestUpdateStopsWhenTheDiskCannotHoldTheNextRelease(t *testing.T) {
	f, srv := newFakeCore(t)
	f.on("GET", "/api/version/check", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("force") == "1" {
			reply(200, `{"current":"v0.132.1-beta.1","native":true,"channel":"beta","latest":"v0.132.2-beta.1","update_available":true,
				"install_root":"/opt/ftw","install_free_bytes":90000000,"install_need_bytes":210000000}`)(w, r)
			return
		}
		reply(200, oldCore)(w, r)
	})
	f.on("GET", "/api/version/update/status", reply(200, `{"state":"idle"}`))
	code, out, errOut := runCLI(t, testEnv(), "update", "--url", srv.URL)
	if code != exitFailed || !strings.Contains(errOut, "not enough disk space for v0.132.2-beta.1: 90.0 MB free, 210.0 MB needed") ||
		!strings.Contains(out, "Releases: /opt/ftw/releases (90.0 MB free") || len(f.postedPaths()) != 0 {
		t.Fatalf("exit %d posts %v\n%s%s", code, f.postedPaths(), out, errOut)
	}
}

func TestUpdateShowsHistoryMigrationWhileTheNewCoreStarts(t *testing.T) {
	f, srv := newFakeCore(t)
	var mu sync.Mutex
	finished := false
	f.on("GET", "/api/version/check", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case finished:
			reply(200, newCore)(w, r)
		case r.URL.Query().Get("force") == "1":
			reply(200, offer)(w, r)
		default:
			reply(200, oldCore)(w, r)
		}
	})
	f.on("GET", "/api/version/update/status", sequence(
		`{"state":"idle"}`,
		`{"state":"restarting","action":"update","target":"v0.132.2-beta.1","step":3,"total_steps":3,"message":"Starting the new Core once"}`,
		`503 {"error":"starting"}`,
		`503 {"error":"starting"}`,
		`{"state":"done","action":"update","target":"v0.132.2-beta.1","step":3,"total_steps":3,"message":"Core is ready on v0.132.2-beta.1"}`,
	))
	f.on("POST", "/api/version/update", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		finished = true
		mu.Unlock()
		reply(202, `{"target":"v0.132.2-beta.1"}`)(w, r)
	})
	f.on("GET", "/api/health", sequence(
		`{"status":"starting","phase":"initializing state","migration":{"state":"running","source_bytes_done":300000000,"source_bytes_total":1200000000}}`,
		`{"status":"starting","phase":"initializing state","migration":{"state":"running","source_bytes_done":600000000,"source_bytes_total":1200000000}}`,
		`{"status":"ok","drivers_ok":1}`,
	))
	e := testEnv()
	e.logEvery = 0 // log every reading
	code, out, errOut := runCLI(t, e, "update", "--url", srv.URL)
	if code != exitOK || !strings.Contains(out, "3/3 Starting the new Core once   25%  300.0 MB of 1.2 GB") ||
		!strings.Contains(out, "Core migrating history") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
}

// clock advances only when a test says so.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestMeterDrawsABarWithRateAndTimeLeftOnATerminal(t *testing.T) {
	var out strings.Builder
	clk := &clock{t: time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)}
	e := testEnv()
	e.now, e.tty, e.width = clk.now, true, 100
	m := newMeter(&out, e)
	step := sample{key: "pull", label: "1/3 Downloading verified Core release", unit: "bytes", total: 100_000_000}
	for i := int64(0); i <= 4; i++ {
		step.done = i * 10_000_000
		m.show(step)
		clk.advance(time.Second)
	}
	screen := out.String()
	bar := "[" + strings.Repeat("█", 12) + strings.Repeat("░", 18) + "]"
	for _, want := range []string{
		"  1/3 Downloading verified Core release\n",
		clearLine + "    " + bar + "   40%  40.0 MB of 100.0 MB  10.0 MB/s  ETA 6s  4s",
	} {
		if !strings.Contains(screen, want) {
			t.Fatalf("missing %q in %q", want, screen)
		}
	}
	if strings.Count(screen, "Downloading") != 1 {
		t.Fatalf("the phase name is drawn once: %q", screen)
	}
	// On 80 columns the numbers still fit beside a shorter bar.
	var narrow strings.Builder
	clk80 := &clock{t: clk.t}
	e80 := e
	e80.now, e80.width = clk80.now, 80
	n := newMeter(&narrow, e80)
	n.show(sample{key: "pull", label: "1/3 Downloading", unit: "bytes", done: 246_400_000, total: 411_700_000})
	clk80.advance(3 * time.Second)
	n.show(sample{key: "pull", label: "1/3 Downloading", unit: "bytes", done: 300_000_000, total: 411_700_000})
	last := narrow.String()[strings.LastIndex(narrow.String(), clearLine)+len(clearLine):]
	if strings.Contains(last, "…") || !strings.HasSuffix(last, "  3s") || utf8.RuneCountInString(last) > 79 {
		t.Fatalf("80 columns: %q", last)
	}
	out.Reset()
	m.finish(true)
	if got := out.String(); got != clearLine+lineUp+clearLine+"✓ 1/3 Downloading verified Core release  40.0 MB in 5.0s (8.0 MB/s)\n" {
		t.Fatalf("finish %q", got)
	}

	out.Reset()
	e.ascii = true
	m = newMeter(&out, e)
	m.show(sample{key: "start", label: "3/3 Starting the new Core once", detail: "new Core starting: initializing state"})
	m.finish(false)
	if got := out.String(); !strings.Contains(got, "    /  new Core starting: initializing state  0s") ||
		!strings.HasSuffix(got, "FAILED 3/3 Starting the new Core once  after 0.0s\n") {
		t.Fatalf("ascii %q", got)
	}
	out.Reset()
	m.show(sample{key: "setup", label: "starting update", quiet: true})
	m.finish(true)
	if got := out.String(); strings.Contains(got, "done") || !strings.HasSuffix(got, lineUp+clearLine) {
		t.Fatalf("a quiet phase leaves nothing: %q", got)
	}
}

func TestUpdateShowsEveryPhaseCoreRecordedEvenWhenPollingMissedThem(t *testing.T) {
	f, srv := newFakeCore(t)
	var mu sync.Mutex
	finished := false
	f.on("GET", "/api/version/check", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case finished:
			reply(200, newCore)(w, r)
		case r.URL.Query().Get("force") == "1":
			reply(200, offer)(w, r)
		default:
			reply(200, oldCore)(w, r)
		}
	})
	// The download and check finish between two reads; only Core's record
	// knows them.
	f.on("GET", "/api/version/update/status", sequence(
		`{"state":"idle"}`,
		`{"state":"starting","action":"update","target":"v0.132.2-beta.1","total_steps":3,"message":"starting update"}`,
		`503 {"error":"starting"}`,
		`{"state":"done","action":"update","target":"v0.132.2-beta.1","step":3,"total_steps":3,"message":"Core is ready on v0.132.2-beta.1","phases":[
			{"step":1,"total_steps":3,"message":"Downloading verified Core release","started_at":"2026-09-24T09:00:00Z","finished_at":"2026-09-24T09:00:00.4Z","bytes":24000000},
			{"step":2,"total_steps":3,"message":"Checking release and preparing restart","started_at":"2026-09-24T09:00:00.4Z","finished_at":"2026-09-24T09:00:00.5Z"},
			{"step":3,"total_steps":3,"message":"Starting the new Core once","started_at":"2026-09-24T09:00:00.5Z","finished_at":"2026-09-24T09:00:02.5Z"}]}`,
	))
	f.on("POST", "/api/version/update", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		finished = true
		mu.Unlock()
		reply(202, `{"target":"v0.132.2-beta.1"}`)(w, r)
	})
	f.on("GET", "/api/health", reply(200, `{"status":"ok","drivers_ok":1}`))
	code, out, errOut := runCLI(t, testEnv(), "update", "--url", srv.URL)
	for _, want := range []string{
		"✓ 1/3 Downloading verified Core release  24.0 MB in 0.4s (60.0 MB/s)\n",
		"✓ 2/3 Checking release and preparing restart  in 0.1s\n",
		"✓ 3/3 Starting the new Core once  in 2.0s\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q\n%s%s", want, out, errOut)
		}
	}
	if code != exitOK || strings.Contains(out, "✓ Restarting into") {
		t.Fatalf("exit %d; the client's own restart phase must give way to Core's record\n%s", code, out)
	}
}

func TestAPhaseClosedBeforeTheRestartIsNotRepeatedFromCoresRecord(t *testing.T) {
	f, srv := newFakeCore(t)
	var mu sync.Mutex
	finished := false
	f.on("GET", "/api/version/check", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case finished:
			reply(200, newCore)(w, r)
		case r.URL.Query().Get("force") == "1":
			reply(200, offer)(w, r)
		default:
			reply(200, oldCore)(w, r)
		}
	})
	// Seen on the home box: the download is read live, then Core is already
	// gone; its record arrives with the new Core.
	f.on("GET", "/api/version/update/status", sequence(
		`{"state":"idle"}`,
		`{"state":"pulling","action":"update","target":"v0.132.2-beta.1","step":1,"total_steps":3,"message":"Downloading verified Core release","progress_current":24000000,"progress_total":24000000,"progress_unit":"bytes"}`,
		`503 {"error":"starting"}`,
		`{"state":"done","action":"update","target":"v0.132.2-beta.1","step":3,"total_steps":3,"message":"Core is ready on v0.132.2-beta.1","phases":[
			{"step":1,"total_steps":3,"message":"Downloading verified Core release","started_at":"2026-09-24T09:00:00Z","finished_at":"2026-09-24T09:00:02.5Z","bytes":24000000},
			{"step":2,"total_steps":3,"message":"Unpacking and checking the release","started_at":"2026-09-24T09:00:02.5Z","finished_at":"2026-09-24T09:00:04Z"},
			{"step":3,"total_steps":3,"message":"Starting the new Core once","started_at":"2026-09-24T09:00:04Z","finished_at":"2026-09-24T09:00:11Z"}]}`,
	))
	f.on("POST", "/api/version/update", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		finished = true
		mu.Unlock()
		reply(202, `{"target":"v0.132.2-beta.1"}`)(w, r)
	})
	f.on("GET", "/api/health", reply(200, `{"status":"ok","drivers_ok":1}`))
	for _, tty := range []bool{false, true} {
		e := testEnv()
		e.tty, e.width = tty, 100
		code, out, errOut := runCLI(t, e, "update", "--url", srv.URL)
		if code != exitOK || strings.Count(out, "✓ 1/3 Downloading verified Core release") != 1 ||
			!strings.Contains(out, "✓ 2/3 Unpacking and checking the release  in 1.5s") ||
			!strings.Contains(out, "✓ 3/3 Starting the new Core once  in 7.0s") {
			t.Fatalf("tty=%v exit %d\n%s%s", tty, code, out, errOut)
		}
		// Reset the status sequence for the next pass.
		mu.Lock()
		finished = false
		mu.Unlock()
		f.on("GET", "/api/version/update/status", sequence(
			`{"state":"idle"}`,
			`{"state":"pulling","action":"update","target":"v0.132.2-beta.1","step":1,"total_steps":3,"message":"Downloading verified Core release","progress_current":24000000,"progress_total":24000000,"progress_unit":"bytes"}`,
			`503 {"error":"starting"}`,
			`{"state":"done","action":"update","target":"v0.132.2-beta.1","step":3,"total_steps":3,"message":"Core is ready on v0.132.2-beta.1","phases":[
				{"step":1,"total_steps":3,"message":"Downloading verified Core release","started_at":"2026-09-24T09:00:00Z","finished_at":"2026-09-24T09:00:02.5Z","bytes":24000000},
				{"step":2,"total_steps":3,"message":"Unpacking and checking the release","started_at":"2026-09-24T09:00:02.5Z","finished_at":"2026-09-24T09:00:04Z"},
				{"step":3,"total_steps":3,"message":"Starting the new Core once","started_at":"2026-09-24T09:00:04Z","finished_at":"2026-09-24T09:00:11Z"}]}`,
		))
	}
}

func TestUpdateDoesNotReinstallAReleaseThatFailedHere(t *testing.T) {
	f, srv := newFakeCore(t)
	failedOffer := `{"current":"v0.132.1-beta.1","native":true,"channel":"beta","latest":"v0.132.2-beta.1","update_available":true,"last_failed":"v0.132.2-beta.1"}`
	f.on("GET", "/api/version/check", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("force") == "1" {
			reply(200, failedOffer)(w, r)
			return
		}
		reply(200, oldCore)(w, r)
	})
	f.on("GET", "/api/version/update/status", reply(200, `{"state":"idle"}`))
	code, _, errOut := runCLI(t, testEnv(), "update", "--url", srv.URL)
	if code != exitFailed || !strings.Contains(errOut, "v0.132.2-beta.1 already failed on this box") || !strings.Contains(errOut, "--retry") || len(f.postedPaths()) != 0 {
		t.Fatalf("exit %d posts %v %q", code, f.postedPaths(), errOut)
	}
	f.on("POST", "/api/version/update", reply(409, `{"error":"stop here"}`))
	code, _, errOut = runCLI(t, testEnv(), "update", "--retry", "--url", srv.URL)
	if code != exitFailed || !strings.Contains(errOut, "stop here") || len(f.postedPaths()) != 1 {
		t.Fatalf("--retry did not ask Core: %d %v %q", code, f.postedPaths(), errOut)
	}

	f.on("GET", "/api/health", reply(200, `{"status":"ok"}`))
	f.on("GET", "/api/version/check", reply(200, failedOffer))
	f.on("GET", "/api/version/snapshots", reply(200, `{"snapshots":[]}`))
	f.on("GET", "/api/backups", reply(200, `{}`))
	if _, out, _ := runCLI(t, testEnv(), "status", "--url", srv.URL); !strings.Contains(out, "Release:  v0.132.2-beta.1 is published but failed on this box; ftw update waits for a newer release") || strings.Contains(out, "Install it with") {
		t.Fatalf("status %s", out)
	}
}

func TestADownCoreNamesTheOfflineWayBack(t *testing.T) {
	_, srv := newFakeCore(t)
	srv.Close()
	if code, out, _ := runCLI(t, testEnv(), "status", "--url", srv.URL); code != exitFailed || !strings.Contains(out, "Back:     if a new release does not start, run: sudo -u ftw /opt/ftw/ftw-launcher -root /opt/ftw rollback") {
		t.Fatalf("status %d %s", code, out)
	}
	if code, _, errOut := runCLI(t, testEnv(), "rollback", "--url", srv.URL); code != exitFailed || !strings.Contains(errOut, "ftw-launcher -root /opt/ftw rollback") {
		t.Fatalf("rollback %d %q", code, errOut)
	}
}

func TestACoreWaitingForSetupIsNamed(t *testing.T) {
	f, srv := newFakeCore(t)
	f.on("GET", "/api/health", reply(404, "404 page not found"))
	f.on("GET", "/setup", reply(200, "<html>setup</html>"))
	f.on("GET", "/api/version/check", reply(503, `{"error":"self-update disabled"}`))
	code, out, _ := runCLI(t, testEnv(), "status", "--url", srv.URL)
	if code != exitFailed || !strings.Contains(out, "Core:     waiting for setup; open "+srv.URL+"/setup") {
		t.Fatalf("status %d\n%s", code, out)
	}
	code, _, errOut := runCLI(t, testEnv(), "update", "--url", srv.URL)
	if code != exitFailed || !strings.Contains(errOut, "waiting for setup; finish it at "+srv.URL+"/setup first") {
		t.Fatalf("update %d %q", code, errOut)
	}
}

func TestUpdateReportsHealthOnlyOnceItHolds(t *testing.T) {
	var out strings.Builder
	f, srv := newFakeCore(t)
	// As seen after a restart: ok at first, offline for a moment, ok again.
	f.on("GET", "/api/health", sequence(
		`{"status":"ok","drivers_ok":1}`,
		`{"status":"degraded","drivers_offline":1}`,
		`{"status":"ok","drivers_ok":1}`,
	))
	clk := &clock{t: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)}
	e := testEnv()
	e.now, e.sleep = clk.now, func(d time.Duration) { clk.advance(time.Second) }
	e.healthSettle, e.healthSteady = 30*time.Second, 3*time.Second
	c := newClient(srv.URL, e)
	c.reportHealth(t.Context(), &out)
	if got := out.String(); got != "Health: ok; drivers 1 ok, 0 degraded, 0 offline, 0 faulted.\n" {
		t.Fatalf("health %q", got)
	}
	// It needed the ok after the dip to hold for three seconds.
	if elapsed := clk.t.Sub(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)); elapsed < 5*time.Second {
		t.Fatalf("reported after %s, before ok held", elapsed)
	}
}
