package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/nativeupdate"
)

// TestUpdateChain runs the native update chain with real binaries: the
// launcher installs a previous release, Core updates itself to the candidate
// through the release mirror, the candidate is killed three times inside its
// probation and the launcher falls back, and then Core takes a binary
// rollback. Slots, the running version, settings, a charging goal and the
// history count are checked at every step.
//
// It builds Core, so it only runs on request:
//
//	make update-chain                      # previous = the last three betas
//	FTW_UPDATE_CHAIN=1 go test -run TestUpdateChain ./cmd/ftw-launcher/
//
// FTW_UPDATE_CHAIN_FROM lists git refs to update from, separated by spaces.
// Each one is built from `git archive`; empty builds the previous release
// from this tree as well. FTW_UPDATE_CHAIN_TAG names the candidate.
func TestUpdateChain(t *testing.T) {
	if os.Getenv("FTW_UPDATE_CHAIN") != "1" {
		t.Skip("set FTW_UPDATE_CHAIN=1 (make update-chain) to build and run the update chain")
	}
	if runtime.GOARCH != "arm64" && runtime.GOARCH != "amd64" {
		t.Skipf("native releases exist for arm64 and amd64, not %s", runtime.GOARCH)
	}
	_, file, _, _ := runtime.Caller(0)
	repo := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	work := t.TempDir()
	candidateTag := envOr("FTW_UPDATE_CHAIN_TAG", "v0.999.0-beta.1")
	launcher := chainBuild(t, filepath.Join(repo, "go"), "./cmd/ftw-launcher", "", filepath.Join(work, "ftw-launcher"))
	candidate := chainBuild(t, filepath.Join(repo, "go"), "./cmd/ftw", candidateTag, filepath.Join(work, "candidate", "ftw"))
	mirror := newReleaseMirror(t)
	mirror.add(t, repo, repo, candidateTag, candidate, launcher)

	refs := strings.Fields(os.Getenv("FTW_UPDATE_CHAIN_FROM"))
	if len(refs) == 0 {
		refs = []string{""}
	}
	for _, ref := range refs {
		name := ref
		if name == "" {
			name = "tree"
		}
		t.Run(name, func(t *testing.T) {
			previousTag, tree := "v0.998.0-beta.1", repo
			if ref != "" {
				if nativeupdate.ValidTag(ref) {
					previousTag = ref
				}
				tree = filepath.Join(t.TempDir(), "src")
				exportRef(t, repo, ref, tree)
			}
			previous := chainBuild(t, filepath.Join(tree, "go"), "./cmd/ftw", previousTag, filepath.Join(t.TempDir(), "ftw"))
			mirror.add(t, repo, tree, previousTag, previous, launcher)
			runUpdateChain(t, launcher, mirror, previousTag, candidateTag)
		})
	}
}

func runUpdateChain(t *testing.T, launcher string, mirror *releaseMirror, previous, candidate string) {
	b := newChainBox(t, launcher, mirror)
	mirror.publish(previous)

	// Install the previous release as a fresh box does.
	archive, checksum := mirror.files(previous)
	b.launcherCmd(t, "install", previous, archive, checksum)
	b.launcherCmd(t, "init", previous)
	b.wantSlots(t, "after init", nativeupdate.Slots{Current: previous})
	b.start(t)
	b.waitVersion(t, previous)
	want := b.seed(t)
	b.stop(t, syscall.SIGTERM)

	// The previous Core sees the candidate on its channel and updates.
	b.start(t)
	b.waitVersion(t, previous)
	want = b.check(t, "previous restarted", want)
	mirror.publish(candidate)
	b.update(t, candidate, false)
	b.wantSlots(t, "update staged", nativeupdate.Slots{Current: previous, Next: candidate})
	b.start(t)
	b.waitVersion(t, candidate)
	b.waitCommitted(t, candidate, previous)
	want = b.check(t, "candidate committed", want)

	// A clean stop inside probation does not count as a crash.
	b.stop(t, syscall.SIGTERM)
	b.start(t)
	b.waitVersion(t, candidate)
	if s := b.slots(t); s.Probation == nil || len(s.Probation.Crashes) != 0 {
		t.Fatalf("a clean stop counted as a crash: %+v", s.Probation)
	}

	// Three kills inside probation send the box back to the previous release.
	for kill := 1; kill <= 3; kill++ {
		b.stop(t, syscall.SIGKILL)
		b.start(t)
		if kill < 3 {
			b.waitVersion(t, candidate)
			if s := b.slots(t); s.Current != candidate || s.Probation == nil || len(s.Probation.Crashes) != kill {
				t.Fatalf("after kill %d: slots %+v probation %+v", kill, s, s.Probation)
			}
			continue
		}
		b.waitVersion(t, previous)
	}
	b.wantSlots(t, "probation fallback", nativeupdate.Slots{Current: previous, LastFailed: candidate})
	want = b.check(t, "probation fallback", want)

	// Retry the failed candidate on request, then take a binary rollback.
	b.update(t, candidate, true)
	b.wantSlots(t, "retry staged", nativeupdate.Slots{Current: previous, Next: candidate})
	b.start(t)
	b.waitVersion(t, candidate)
	b.waitCommitted(t, candidate, previous)
	want = b.check(t, "retry committed", want)
	var rollback map[string]any
	if status := b.call(t, "POST", "/api/version/binary-rollback", nil, &rollback); status != http.StatusAccepted {
		t.Fatalf("binary rollback = %d %v", status, rollback)
	}
	b.waitExit(t, 90*time.Second)
	b.wantSlots(t, "rollback staged", nativeupdate.Slots{Current: candidate, Previous: previous, Next: previous})
	b.start(t)
	b.waitVersion(t, previous)
	b.waitCommitted(t, previous, candidate)
	b.check(t, "binary rollback", want)
	b.stop(t, syscall.SIGTERM)
}

// chainSnapshot is what an update must keep: two settings, a saved charging
// goal and the recorded history.
type chainSnapshot struct {
	Mode        string
	GridTargetW float64
	GoalSoC     float64
	GoalClock   int // minutes after local midnight
	GoalDays    uint8
	History     int
}

func (b *chainBox) seed(t *testing.T) chainSnapshot {
	t.Helper()
	for _, req := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/mode", map[string]any{"mode": "peak_shaving"}},
		{"POST", "/api/target", map[string]any{"grid_target_w": -125}},
		{"PUT", "/api/loadpoints/garage/schedule", map[string]any{"soc": 0.8, "time_of_day_min": 420, "recurring": true, "days": 31}},
	} {
		var out map[string]any
		if status := b.call(t, req.method, req.path, req.body, &out); status != http.StatusOK {
			t.Fatalf("%s %s = %d %v", req.method, req.path, status, out)
		}
	}
	if !chainWait(60*time.Second, func() bool { return b.history(t) >= 5 }) {
		t.Fatalf("history has %d rows after a minute", b.history(t))
	}
	snap := b.snapshot(t)
	want := chainSnapshot{Mode: "peak_shaving", GridTargetW: -125, GoalSoC: 0.8, GoalClock: 420, GoalDays: 31, History: snap.History}
	if snap != want {
		t.Fatalf("seeded %+v, read back %+v", want, snap)
	}
	return snap
}

// check compares the stored state with want. History only grows.
func (b *chainBox) check(t *testing.T, step string, want chainSnapshot) chainSnapshot {
	t.Helper()
	got := b.snapshot(t)
	if got.History < want.History {
		t.Fatalf("%s: history fell from %d to %d rows", step, want.History, got.History)
	}
	want.History = got.History
	if got != want {
		t.Fatalf("%s: stored state changed\n got %+v\nwant %+v", step, got, want)
	}
	return got
}

func (b *chainBox) snapshot(t *testing.T) chainSnapshot {
	t.Helper()
	var mode struct {
		Mode        string  `json:"mode"`
		GridTargetW float64 `json:"grid_target_w"`
	}
	if status := b.call(t, "GET", "/api/mode", nil, &mode); status != http.StatusOK {
		t.Fatalf("GET /api/mode = %d", status)
	}
	var lps struct {
		Loadpoints []struct {
			Schedule struct {
				SoC   float64 `json:"soc"`
				Clock int     `json:"time_of_day_min"`
				Days  uint8   `json:"days"`
			} `json:"schedule"`
		} `json:"loadpoints"`
	}
	if status := b.call(t, "GET", "/api/loadpoints", nil, &lps); status != http.StatusOK || len(lps.Loadpoints) != 1 {
		t.Fatalf("GET /api/loadpoints = %d %+v", status, lps)
	}
	goal := lps.Loadpoints[0].Schedule
	return chainSnapshot{Mode: mode.Mode, GridTargetW: mode.GridTargetW, GoalSoC: goal.SoC,
		GoalClock: goal.Clock, GoalDays: goal.Days, History: b.history(t)}
}

// history counts the samples recorded in the last hour; a chain run is shorter.
func (b *chainBox) history(t *testing.T) int {
	t.Helper()
	var out struct {
		Items []struct {
			N float64 `json:"n"`
		} `json:"items"`
	}
	if status := b.call(t, "GET", "/api/history?range=1h&points=5000", nil, &out); status != http.StatusOK {
		t.Fatalf("GET /api/history = %d %s", status, b.last)
	}
	n := 0
	for _, item := range out.Items {
		n += max(1, int(item.N))
	}
	return n
}

// update asks the running Core to install target, as the Settings page does,
// and waits for Core to stop so the launcher can start it.
func (b *chainBox) update(t *testing.T, target string, retry bool) {
	t.Helper()
	var info struct {
		Latest          string `json:"latest"`
		UpdateAvailable bool   `json:"update_available"`
		Err             string `json:"err"`
	}
	if status := b.call(t, "GET", "/api/version/check?force=1", nil, &info); status != http.StatusOK || info.Latest != target || !info.UpdateAvailable {
		t.Fatalf("version check = %d %+v, want %s available", status, info, target)
	}
	var out map[string]any
	if status := b.call(t, "POST", "/api/version/update", map[string]any{"retry": retry}, &out); status != http.StatusAccepted {
		t.Fatalf("POST /api/version/update = %d %v", status, out)
	}
	b.waitExit(t, 3*time.Minute)
}

// chainBox plays systemd for one install root: it starts the launcher, which
// execs Core in the same process, and stops or kills it.
type chainBox struct {
	launcher, root, data, config, drivers, api string
	mirror                                     *releaseMirror
	log                                        *os.File
	cmd                                        *exec.Cmd
	done                                       chan struct{}
	last                                       string // the last reply body
}

func newChainBox(t *testing.T, launcher string, mirror *releaseMirror) *chainBox {
	t.Helper()
	dir := t.TempDir()
	b := &chainBox{launcher: launcher, root: filepath.Join(dir, "opt"), data: filepath.Join(dir, "var"), mirror: mirror}
	b.config = filepath.Join(b.data, "config.yaml")
	b.drivers = filepath.Join(b.data, "drivers")
	for _, d := range []string{b.root, b.drivers} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	port := chainFreePort(t)
	b.api = fmt.Sprintf("http://127.0.0.1:%d", port)
	cfg := map[string]any{
		"site": map[string]any{"name": "update-chain", "control_interval_s": 2, "grid_target_w": 0,
			"grid_tolerance_w": 42, "watchdog_timeout_s": 60},
		"fuse": map[string]any{"max_amps": 16, "phases": 3, "voltage": 230},
		"drivers": []map[string]any{{"name": "meter", "lua": "drivers/chain_meter.lua", "is_site_meter": true,
			"capabilities": map[string]any{"standalone": true}}},
		"loadpoints":        []map[string]any{{"id": "garage", "driver_name": "chain-charger", "min_charge_w": 4140, "max_charge_w": 11040, "vehicle_capacity_wh": 60000}},
		"api":               map[string]any{"port": port},
		"ocpp":              map[string]any{"enabled": false},
		"state":             map[string]any{"path": filepath.Join(b.data, "state.db")},
		"app_link":          map[string]any{"enabled": false},
		"fleet_ping":        map[string]any{"enabled": false},
		"device_repository": map[string]any{"enabled": false},
	}
	raw, err := json.MarshalIndent(cfg, "", "  ") // JSON is valid YAML
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.config, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.drivers, "chain_meter.lua"), []byte(chainMeterLua), 0o644); err != nil {
		t.Fatal(err)
	}
	if b.log, err = os.Create(filepath.Join(dir, "box.log")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if b.cmd != nil {
			_ = b.cmd.Process.Kill()
			<-b.done
		}
		b.log.Close()
		if t.Failed() {
			if tail, err := os.ReadFile(b.log.Name()); err == nil {
				if len(tail) > 8000 {
					tail = tail[len(tail)-8000:]
					tail = tail[bytes.IndexByte(tail, '\n')+1:]
				}
				t.Logf("box log tail:\n%s", tail)
			}
		}
	})
	return b
}

// chainMeterLua is a site meter that needs no device: a steady 1.2 kW import.
const chainMeterLua = `DRIVER = { host_api_min = 1, host_api_max = 1, id = "chain_meter", name = "Update chain meter",
  version = "1.0.0", protocols = { "standalone" }, capabilities = { "meter" }, read_only = true }
function driver_init(config) host.set_make("FTW test"); host.set_sn("chain-meter-1"); return true end
function driver_poll()
  host.emit("meter", { w = 1200, hz = 50, l1_v = 230, l2_v = 230, l3_v = 230, l1_a = 2, l2_a = 2, l3_a = 1.2,
    l1_w = 460, l2_w = 460, l3_w = 280, import_wh = 1000, export_wh = 0 })
  return 1000
end
function driver_command(action, value, context) return action == "init" or action == "deinit" end
function driver_default_mode() end
`

func (b *chainBox) launcherCmd(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command(b.launcher, append([]string{"-root", b.root}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ftw-launcher %v: %v\n%s", args, err, out)
	}
}

func (b *chainBox) start(t *testing.T) {
	t.Helper()
	if b.cmd != nil {
		t.Fatal("start while Core runs")
	}
	cmd := exec.Command(b.launcher, "-root", b.root, "-config", b.config, "-user-drivers", b.drivers)
	cmd.Dir = b.data
	cmd.Env = append(os.Environ(), "FTW_RELEASE_MIRROR="+b.mirror.srv.URL)
	cmd.Stdout, cmd.Stderr = b.log, b.log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	b.cmd, b.done = cmd, done
}

func (b *chainBox) stop(t *testing.T, sig syscall.Signal) {
	t.Helper()
	if err := b.cmd.Process.Signal(sig); err != nil {
		t.Fatal(err)
	}
	b.waitExit(t, 60*time.Second)
}

func (b *chainBox) waitExit(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case <-b.done:
		b.cmd = nil
	case <-time.After(timeout):
		t.Fatalf("Core did not stop within %s", timeout)
	}
}

// waitVersion waits until the full API of tag answers.
func (b *chainBox) waitVersion(t *testing.T, tag string) {
	t.Helper()
	var last string
	ok := chainWait(3*time.Minute, func() bool {
		select {
		case <-b.done:
			t.Fatalf("Core exited while waiting for %s", tag)
		default:
		}
		var status struct {
			Version string `json:"version"`
		}
		if b.call(t, "GET", "/api/status", nil, &status) == http.StatusOK {
			last = status.Version
		}
		return last == tag
	})
	if !ok {
		t.Fatalf("Core never served %s (last %q)", tag, last)
	}
}

func (b *chainBox) waitCommitted(t *testing.T, current, previous string) {
	t.Helper()
	var s nativeupdate.Slots
	if !chainWait(time.Minute, func() bool {
		s = b.slots(t)
		return s.Current == current && s.Trial == ""
	}) {
		t.Fatalf("trial %s never committed: %+v", current, s)
	}
	if s.Previous != previous || s.Next != "" || s.Probation == nil || s.Probation.Tag != current {
		t.Fatalf("committed %s: slots %+v probation %+v", current, s, s.Probation)
	}
}

func (b *chainBox) slots(t *testing.T) nativeupdate.Slots {
	t.Helper()
	s, err := (nativeupdate.Manager{Root: b.root}).Read()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// wantSlots compares the release slots, leaving probation to the caller.
func (b *chainBox) wantSlots(t *testing.T, step string, want nativeupdate.Slots) {
	t.Helper()
	got := b.slots(t)
	got.Probation = nil
	if got != want {
		t.Fatalf("%s: slots %+v, want %+v", step, got, want)
	}
}

func (b *chainBox) call(t *testing.T, method, path string, body, out any) int {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, b.api+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	b.last = string(raw)
	if out != nil && json.Unmarshal(raw, out) != nil && resp.StatusCode == http.StatusOK {
		t.Fatalf("%s %s: unreadable reply %q", method, path, raw)
	}
	return resp.StatusCode
}

// releaseMirror serves releases the way GitHub does for FTW_RELEASE_MIRROR:
// the list at /releases and the assets at /download/<tag>/. The list holds
// only the newest release, which is all a channel check reads.
type releaseMirror struct {
	srv    *httptest.Server
	dir    string
	mu     sync.Mutex
	newest string
}

func newReleaseMirror(t *testing.T) *releaseMirror {
	m := &releaseMirror{dir: t.TempDir()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /releases", func(w http.ResponseWriter, _ *http.Request) {
		m.mu.Lock()
		tag := m.newest
		m.mu.Unlock()
		archive := "ftw-linux-" + runtime.GOARCH + ".tar.gz"
		list := []map[string]any{{"tag_name": tag, "prerelease": strings.Contains(tag, "-beta."),
			"published_at": time.Now().UTC(), "body": "<!-- ftw-state-schema-v2: " + m.schema(tag) + " -->",
			"assets": []map[string]string{{"name": archive}, {"name": archive + ".sha256"}}}}
		_ = json.NewEncoder(w).Encode(list)
	})
	mux.Handle("GET /download/", http.StripPrefix("/download/", http.FileServer(http.Dir(m.dir))))
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *releaseMirror) publish(tag string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.newest = tag
}

func (m *releaseMirror) files(tag string) (archive, checksum string) {
	archive = filepath.Join(m.dir, tag, "ftw-linux-"+runtime.GOARCH+".tar.gz")
	return archive, archive + ".sha256"
}

func (m *releaseMirror) schema(tag string) string {
	raw, _ := os.ReadFile(filepath.Join(m.dir, tag, "schema"))
	return string(raw)
}

// add packs a release as scripts/package-linux.py does, with this host's
// binaries. tree supplies web/ and state-schema.json; drivers come from repo.
func (m *releaseMirror) add(t *testing.T, repo, tree, tag, ftw, launcher string) {
	t.Helper()
	var schema struct {
		Version int `json:"version"`
	}
	raw, err := os.ReadFile(filepath.Join(tree, "state-schema.json"))
	if err == nil {
		err = json.Unmarshal(raw, &schema)
	}
	if err != nil || schema.Version <= 0 {
		t.Fatalf("%s: state-schema.json: %v", tag, err)
	}
	dir := filepath.Join(m.dir, tag)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "schema"), []byte(fmt.Sprint(schema.Version)), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	add := func(name string, mode int64, data []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	addFile := func(name, path string, mode int64) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		add(name, mode, data)
	}
	addFile("ftw", ftw, 0o755)
	addFile("ftw-launcher", launcher, 0o755)
	addFile("drivers/BUNDLED_SOURCE.json", filepath.Join(repo, "drivers", "BUNDLED_SOURCE.json"), 0o644)
	addFile("state-schema.json", filepath.Join(tree, "state-schema.json"), 0o644)
	add("optimizer/native/bundle/manifest.json", 0o644, []byte("{}\n"))
	version, _ := json.Marshal(map[string]any{"version": tag, "arch": runtime.GOARCH, "state_schema": schema.Version})
	add("release-version.json", 0o644, append(version, '\n'))
	web := filepath.Join(tree, "web")
	if err := filepath.WalkDir(web, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(web, path)
		if err == nil && d.Type().IsRegular() {
			addFile("web/"+filepath.ToSlash(rel), path, 0o644)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	archive, checksum := m.files(tag)
	if err := os.WriteFile(archive, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	line := hex.EncodeToString(sum[:]) + "  " + filepath.Base(archive) + "\n"
	if err := os.WriteFile(checksum, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
}

func chainBuild(t *testing.T, module, pkg, version, out string) string {
	t.Helper()
	ldflags := "-s -w"
	if version != "" {
		ldflags += " -X main.Version=" + version
	}
	cmd := exec.Command("go", "build", "-trimpath", "-tags=netgo,osusergo", "-ldflags", ldflags, "-o", out, pkg)
	cmd.Dir = module
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s %s: %v\n%s", pkg, version, err, raw)
	}
	return out
}

// exportRef writes the parts of ref that a release needs into dest.
func exportRef(t *testing.T, repo, ref, dest string) {
	t.Helper()
	cmd := exec.Command("git", "-C", repo, "archive", "--format=tar", ref, "go", "web", "state-schema.json")
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("git archive %s: %v", ref, err)
	}
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dest, filepath.FromSlash(h.Name))
		switch h.Typeflag {
		case tar.TypeDir:
			err = os.MkdirAll(path, 0o755)
		case tar.TypeReg:
			if err = os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
				var data []byte
				if data, err = io.ReadAll(tr); err == nil {
					err = os.WriteFile(path, data, fs.FileMode(h.Mode)&0o755|0o644)
				}
			}
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func chainFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func chainWait(timeout time.Duration, ok func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return ok()
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
