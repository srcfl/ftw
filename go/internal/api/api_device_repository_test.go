package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/components"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/driverrepo"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A real signed download and Lua poll loop, with different bundled and
// repository filenames. No device transport or Registry mock is involved.
type driverUpdateFixture struct {
	t        *testing.T
	s        *Server
	repo     *httptest.Server
	private  ed25519.PrivateKey
	mu       sync.Mutex
	manifest driverrepo.Manifest
	lua      []byte
	bundled  string
	saved    config.Config
	saveErr  error
}

func updateDriverLua(version, serial, poll string) []byte {
	return []byte(fmt.Sprintf(`local metadata = {
 id = "esphome-dsmr",
 name = "ESPHome DSMR",
 version = %q,
 host_api_min = 1,
 host_api_max = 1,
 protocols = { "http" },
 capabilities = { "meter" },
 read_only = true,
}
DRIVER = metadata
function driver_init(config)
 host.set_make("ESPHome")
 host.set_sn(%q)
 host.set_poll_interval(50)
end
function driver_poll() %s end
function driver_command() return false end
function driver_default_mode() end
function driver_cleanup() end
`, version, serial, poll))
}

func newDriverUpdateFixture(t *testing.T, mode string) *driverUpdateFixture {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &driverUpdateFixture{t: t, private: private}
	f.repo = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Path == "/manifest.json" {
			payload, _ := json.Marshal(f.manifest)
			_ = json.NewEncoder(w).Encode(driverrepo.ManifestEnvelope{SchemaVersion: 1, KeyID: "test", Payload: payload, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(private, payload))})
		} else {
			_, _ = w.Write(f.lua)
		}
	}))
	t.Cleanup(f.repo.Close)
	dir := t.TempDir()
	bundledDir := filepath.Join(dir, "bundled")
	if err := os.MkdirAll(bundledDir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.bundled = filepath.Join(bundledDir, "esphome_dsmr.lua")
	if err := os.WriteFile(f.bundled, updateDriverLua("1.0.2", "P1-123", `host.emit("meter", {w=102})`), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rc := &config.DeviceRepository{Enabled: true, Repositories: []config.DriverRepositorySource{{ID: "test", ManifestURL: f.repo.URL + "/manifest.json", Enabled: true, AllowInsecure: true, TrustedKeys: map[string]string{"test": base64.StdEncoding.EncodeToString(public)}}}}
	m := driverrepo.New(rc, dir, st)
	tel := telemetry.NewStore()
	registry := drivers.NewRegistry(tel)
	registry.RuntimePolicyResolver = m.RuntimePolicy
	cfg, err := config.Parse([]byte("site:\n  name: Test\nfuse:\n  max_amps: 16\n"), dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Drivers = []config.Driver{{Name: "p1", Lua: f.bundled, IsSiteMeter: true, Disabled: mode == "disabled", Capabilities: config.Capabilities{Standalone: true}}}
	if mode == "override" {
		cfg.Drivers[0].Lua = filepath.Join(dir, "esphome-dsmr.lua")
		if err := os.WriteFile(cfg.Drivers[0].Lua, updateDriverLua("1.0.2", "P1-123", `host.emit("meter", {w=102})`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(dir, "config.yaml")
	if err := config.SaveAtomic(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.InitializeStorage(configPath, filepath.Join(dir, "state.db"), cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	f.s = New(&Deps{Cfg: cfg, CfgMu: &sync.RWMutex{}, CtrlMu: &sync.Mutex{}, Ctrl: &control.State{}, ConfigPath: filepath.Join(dir, "config.yaml"), DriverDir: bundledDir, Registry: registry, DriverRepository: m, State: st, Tel: tel,
		SaveConfig: func(path string, next *config.Config) error {
			if f.saveErr != nil {
				return f.saveErr
			}
			if err := config.SaveStored(st, path, next); err != nil {
				return err
			}
			f.saved = *next
			f.saved.Drivers = append([]config.Driver(nil), next.Drivers...)
			return nil
		},
	})
	if mode != "disabled" && mode != "stopped" {
		if err := registry.Add(context.Background(), cfg.Drivers[0]); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { registry.Remove("p1") })
		f.reading(102)
	}
	f.publish("1.0.3", "P1-123", `host.emit("meter", {w=103})`)
	return f
}

func (f *driverUpdateFixture) publish(version, serial, poll string) {
	f.t.Helper()
	f.publishSource(version, updateDriverLua(version, serial, poll))
}

func (f *driverUpdateFixture) publishSource(version string, source []byte) {
	f.t.Helper()
	f.mu.Lock()
	f.lua = source
	hash := sha256.Sum256(f.lua)
	f.manifest = driverrepo.Manifest{SchemaVersion: 1, Repository: f.repo.URL, Drivers: []driverrepo.ManifestDriver{{ID: "esphome-dsmr", Path: "drivers/esphome-dsmr.lua", Filename: "esphome-dsmr.lua", Version: version, SHA256: hex.EncodeToString(hash[:]), URL: f.repo.URL + "/driver.lua", HostAPI: components.CompatibleRange{Min: 1, Max: 1}}}}
	f.mu.Unlock()
	if err := f.s.deps.DriverRepository.Refresh(context.Background(), "test"); err != nil {
		f.t.Fatal(err)
	}
}

func (f *driverUpdateFixture) request(ctx context.Context, action, body string, want int) map[string]any {
	f.t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/device_repository/drivers/esphome-dsmr/"+action, strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(w, r)
	if w.Code != want {
		f.t.Fatalf("%s: HTTP %d want %d: %s", action, w.Code, want, w.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		f.t.Fatal(err)
	}
	return response
}

func (f *driverUpdateFixture) reading(watts float64) {
	f.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, reading := range f.s.deps.Tel.ReadingsByDriver("p1") {
			if reading.RawW == watts {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("running Lua never emitted %g W", watts)
}

func TestManagedDriverUpdateFilenameIdentity(t *testing.T) {
	f := newDriverUpdateFixture(t, "running")
	before := f.s.deps.Registry.Env("p1")
	response := f.request(context.Background(), "install", `{"repository_id":"test"}`, 200)
	if f.s.deps.Registry.Env("p1") == before {
		t.Fatal("install succeeded without replacing the bundled Lua runtime")
	}
	f.reading(103)
	if response["runtime_verified"] != true {
		t.Fatalf("missing runtime proof: %v", response)
	}
	if len(f.saved.Drivers) != 1 || f.saved.Drivers[0].Lua != filepath.Join(f.s.managedDriverDir(), "esphome-dsmr.lua") {
		t.Fatalf("managed selection not persisted: %+v", f.saved.Drivers)
	}
	f.assertReload(filepath.Join(f.s.managedDriverDir(), "esphome-dsmr.lua"), 103)
	// The open card still has its bundled path after the first install.
	f.request(context.Background(), "use_bundled", `{"logical_path":"drivers/esphome_dsmr.lua"}`, 200)
	f.reading(102)
	if f.saved.Drivers[0].Lua != f.bundled {
		t.Fatalf("bundled selection not persisted: %+v", f.saved.Drivers)
	}
	f.assertReload(f.bundled, 102)
}

func (f *driverUpdateFixture) assertReload(path string, watts float64) {
	f.t.Helper()
	loaded, err := config.Load(f.s.deps.ConfigPath)
	if err != nil {
		f.t.Fatal(err)
	}
	if loaded.Drivers[0].Lua != path {
		f.t.Fatalf("reloaded path=%s want %s", loaded.Drivers[0].Lua, path)
	}
	f.s.deps.Registry.Remove("p1")
	if err := f.s.deps.Registry.Add(context.Background(), loaded.Drivers[0]); err != nil {
		f.t.Fatal(err)
	}
	f.reading(watts)
}

func TestManagedDriverUpdateAcceptsSerialAfterMAC(t *testing.T) {
	for _, changedMAC := range []bool{false, true} {
		t.Run(fmt.Sprintf("changed_MAC_%v", changedMAC), func(t *testing.T) {
			f := newDriverUpdateFixture(t, "running")
			macOnly := updateDriverLua("1.0.2", "", `host.emit("meter", {w=102})`)
			if err := os.WriteFile(f.bundled, macOnly, 0o644); err != nil {
				t.Fatal(err)
			}
			f.s.deps.Cfg.Drivers[0].Config = map[string]any{"host": "192.0.2.10"}
			f.s.deps.Cfg.Drivers[0].Capabilities.HTTP = &config.HTTPCapability{}
			lookups := 0
			f.s.deps.Registry.ARPLookup = func(string) (string, bool) {
				lookups++
				if changedMAC && lookups == 2 {
					return "aa:bb:cc:dd:ee:ff", true
				}
				return "b8:27:b9:35:8d:1a", true
			}
			if err := f.s.deps.Registry.Restart(context.Background(), f.s.deps.Cfg.Drivers[0]); err != nil {
				t.Fatal(err)
			}
			f.reading(102)
			if got := f.s.runningDriverIdentity("p1"); got != "mac:b827b9358d1a" {
				t.Fatalf("pre-update identity = %q", got)
			}
			if changedMAC {
				response := f.request(context.Background(), "install", `{"repository_id":"test"}`, 502)
				if !strings.Contains(response["error"].(string), "hardware identity") {
					t.Fatalf("wrong failure: %v", response)
				}
				f.reading(102)
				if f.s.activeManagedDriverVersion("esphome-dsmr") != "" {
					t.Fatal("wrong hardware left the new artifact active")
				}
				return
			}
			response := f.request(context.Background(), "install", `{"repository_id":"test"}`, 200)
			if response["runtime_verified"] != true {
				t.Fatalf("same MAC with serial did not verify: %v", response)
			}
			f.reading(103)
			if got := f.s.runningDriverIdentity("p1"); got != "esphome:P1-123" {
				t.Fatalf("post-update identity = %q", got)
			}
		})
	}
}

func TestManagedDriverUpdateWithoutRecipient(t *testing.T) {
	for _, mode := range []string{"disabled", "stopped", "override"} {
		t.Run(mode, func(t *testing.T) {
			f := newDriverUpdateFixture(t, mode)
			before := f.s.deps.Registry.Env("p1")
			path := f.s.deps.Cfg.Drivers[0].Lua
			response := f.request(context.Background(), "install", `{"repository_id":"test"}`, 200)
			if response["runtime_verified"] != false || len(response["restarted_drivers"].([]any)) != 0 {
				t.Fatalf("false runtime proof: %v", response)
			}
			if f.s.deps.Registry.Env("p1") != before {
				t.Fatal("install changed a non-recipient runtime")
			}
			if mode == "override" && f.s.deps.Cfg.Drivers[0].Lua != path {
				t.Fatal("operator file was replaced")
			}
		})
	}
}

func TestManagedDriverUpdateRecovery(t *testing.T) {
	for _, failure := range []string{"init", "poll", "identity", "save"} {
		t.Run(failure, func(t *testing.T) {
			f := newDriverUpdateFixture(t, "running")
			ctx := context.Background()
			if failure == "poll" {
				f.publish("1.0.3", "P1-123", "error('sensor unavailable')")
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Second)
				defer cancel()
			}
			if failure == "init" {
				source := updateDriverLua("1.0.3", "P1-123", `host.emit("meter", {w=103})`)
				f.publishSource("1.0.3", []byte(strings.Replace(string(source), "function driver_init(config)", "function driver_init(config) error('init failed')", 1)))
			}
			if failure == "identity" {
				f.publish("1.0.3", "OTHER-METER", `host.emit("meter", {w=103})`)
			}
			if failure == "save" {
				f.saveErr = errors.New("disk full")
			}
			response := f.request(ctx, "install", `{"repository_id":"test"}`, 502)
			if strings.Contains(response["error"].(string), "rollback failed") {
				t.Fatalf("recovery failed: %v", response)
			}
			f.reading(102)
			if f.s.activeManagedDriverVersion("esphome-dsmr") != "" {
				t.Fatal("failed first install remained active")
			}
			f.assertReload(f.bundled, 102)
		})
	}
}

func TestManagedDriverUpdateRetainedVersions(t *testing.T) {
	f := newDriverUpdateFixture(t, "running")
	f.request(context.Background(), "install", `{"repository_id":"test"}`, 200)
	f.publish("1.0.4", "P1-123", `host.emit("meter", {w=104})`)
	f.request(context.Background(), "install", `{"repository_id":"test"}`, 200)
	f.reading(104)
	f.request(context.Background(), "rollback", `{}`, 200)
	f.reading(103)
	f.request(context.Background(), "activate", `{"version":"1.0.4"}`, 200)
	f.reading(104)
	f.request(context.Background(), "use_bundled", `{}`, 200)
	f.reading(102)
	f.request(context.Background(), "activate", `{"version":"1.0.3"}`, 200)
	f.reading(103)
	f.assertReload(filepath.Join(f.s.managedDriverDir(), "esphome-dsmr.lua"), 103)
}

func TestManagedDriverUpdateRejectsWrongIdentityAndAmbiguousBundled(t *testing.T) {
	f := newDriverUpdateFixture(t, "running")
	// Equal filenames alone do not identify a module.
	other := filepath.Join(f.s.deps.DriverDir, "esphome-dsmr.lua")
	if err := os.WriteFile(other, []byte(strings.Replace(string(updateDriverLua("1.0.2", "P1-123", "")), `id = "esphome-dsmr"`, `id = "different-driver"`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	f.s.deps.Cfg.Drivers = append(f.s.deps.Cfg.Drivers, config.Driver{Name: "other", Lua: other, Disabled: true, Capabilities: config.Capabilities{Standalone: true}})
	f.request(context.Background(), "install", `{"repository_id":"test"}`, 200)
	if f.s.deps.Cfg.Drivers[1].Lua != other {
		t.Fatal("same filename with a different ID was selected")
	}
	// Even duplicate basenames in subdirectories must fail closed on undo.
	duplicate := filepath.Join(f.s.deps.DriverDir, "nested", "esphome_dsmr.lua")
	if err := os.MkdirAll(filepath.Dir(duplicate), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(duplicate, updateDriverLua("1.0.2", "P1-123", ""), 0o644); err != nil {
		t.Fatal(err)
	}
	f.request(context.Background(), "use_bundled", `{}`, 422)
	f.reading(103)
	if f.s.activeManagedDriverVersion("esphome-dsmr") != "1.0.3" {
		t.Fatal("ambiguous bundled lookup deactivated the managed file")
	}
}

func TestManagedDriverUpdateBundledSaveFailureRecovers(t *testing.T) {
	f := newDriverUpdateFixture(t, "running")
	f.request(context.Background(), "install", `{"repository_id":"test"}`, 200)
	f.saveErr = errors.New("disk full")
	f.request(context.Background(), "use_bundled", `{}`, 502)
	f.reading(103)
	f.assertReload(filepath.Join(f.s.managedDriverDir(), "esphome-dsmr.lua"), 103)
}

func TestManagedDriverUpdateBadSecondVersionRecovers(t *testing.T) {
	f := newDriverUpdateFixture(t, "running")
	f.request(context.Background(), "install", `{"repository_id":"test"}`, 200)
	f.publish("1.0.4", "OTHER-METER", `host.emit("meter", {w=104})`)
	f.request(context.Background(), "install", `{"repository_id":"test"}`, 502)
	f.reading(103)
	if f.s.activeManagedDriverVersion("esphome-dsmr") != "1.0.3" {
		t.Fatal("previous signed version not restored")
	}
}

func TestManagedDriverUpdateRejectsTamperedDownload(t *testing.T) {
	f := newDriverUpdateFixture(t, "running")
	f.mu.Lock()
	f.lua = append(f.lua, []byte("-- unsigned change")...)
	f.mu.Unlock()
	before := f.s.deps.Registry.Env("p1")
	f.request(context.Background(), "install", `{"repository_id":"test"}`, 422)
	if f.s.deps.Registry.Env("p1") != before {
		t.Fatal("bad hash reached runtime activation")
	}
	f.reading(102)
}

func TestManagedDriverUpdateSettingsSave(t *testing.T) {
	f := newDriverUpdateFixture(t, "running")
	get := func() *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		f.s.Handler().ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/api/config", nil))
		if r.Code != 200 {
			t.Fatalf("GET config: %s", r.Body.String())
		}
		return r
	}
	save := func(snapshot *httptest.ResponseRecorder, want int) {
		r := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(snapshot.Body.String()))
		r.Header.Set("If-Match", snapshot.Header().Get("ETag"))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		f.s.Handler().ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("save: HTTP %d want %d: %s", w.Code, want, w.Body.String())
		}
	}
	old := get()
	response := f.request(context.Background(), "install", `{"repository_id":"test"}`, 200)
	if response["config_changed"] != true {
		t.Fatal("client not told that Settings needs to reload")
	}
	save(old, 409)
	save(get(), 200)
	f.assertReload(filepath.Join(f.s.managedDriverDir(), "esphome-dsmr.lua"), 103)
}

func TestManagedDriverUpdateBundledIdentityFailureRecovers(t *testing.T) {
	f := newDriverUpdateFixture(t, "running")
	f.request(context.Background(), "install", `{"repository_id":"test"}`, 200)
	if err := os.WriteFile(f.bundled, updateDriverLua("1.0.2", "OTHER-METER", `host.emit("meter", {w=102})`), 0o644); err != nil {
		t.Fatal(err)
	}
	f.request(context.Background(), "use_bundled", `{}`, 502)
	f.reading(103)
	f.assertReload(filepath.Join(f.s.managedDriverDir(), "esphome-dsmr.lua"), 103)
}

// `ftw status` reads which file each configured driver runs, its version and
// its source, so an override of the release's copy is visible.
func TestDriverCatalogNamesTheDriversThatRunEachFile(t *testing.T) {
	catalog := func(f *driverUpdateFixture) map[string]drivers.CatalogEntry {
		t.Helper()
		w := httptest.NewRecorder()
		f.s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/drivers/catalog", nil))
		var body struct {
			Entries []drivers.CatalogEntry `json:"entries"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatalf("catalog: HTTP %d %s", w.Code, w.Body.String())
		}
		out := make(map[string]drivers.CatalogEntry)
		for _, e := range body.Entries {
			out[e.Filename] = e
		}
		return out
	}
	f := newDriverUpdateFixture(t, "running")
	if got := catalog(f)["esphome_dsmr.lua"]; got.Source != "bundled" || strings.Join(got.UsedBy, ",") != "p1" {
		t.Fatalf("before install: %+v", got)
	}
	f.request(context.Background(), "install", `{"repository_id":"test"}`, 200)
	after := catalog(f)
	if got := after["esphome-dsmr.lua"]; got.Source != "managed" || got.Version != "1.0.3" || strings.Join(got.UsedBy, ",") != "p1" {
		t.Fatalf("after install: %+v", got)
	}
	if got := after["esphome_dsmr.lua"]; len(got.UsedBy) != 0 {
		t.Fatalf("the bundled file no longer runs, used_by = %v", got.UsedBy)
	}

	disabled := newDriverUpdateFixture(t, "disabled")
	if got := catalog(disabled)["esphome_dsmr.lua"]; len(got.UsedBy) != 0 {
		t.Fatalf("a disabled driver runs nothing, used_by = %v", got.UsedBy)
	}
}

// publishAs publishes the fixture driver under the channel's own spelling,
// as device-drivers does: the bundled source declares id "esphome-dsmr",
// the signed channel calls the same file "esphome_dsmr".
func (f *driverUpdateFixture) publishAs(id, filename, version string) {
	f.t.Helper()
	source := []byte(strings.Replace(string(updateDriverLua(version, "P1-123", `host.emit("meter", {w=103})`)),
		`id = "esphome-dsmr"`, `id = "`+id+`"`, 1))
	f.mu.Lock()
	f.lua = source
	hash := sha256.Sum256(source)
	f.manifest = driverrepo.Manifest{SchemaVersion: 1, Repository: f.repo.URL, Drivers: []driverrepo.ManifestDriver{{
		ID: id, Path: "drivers/" + filename, Filename: filename, Version: version,
		SHA256: hex.EncodeToString(hash[:]), URL: f.repo.URL + "/driver.lua", HostAPI: components.CompatibleRange{Min: 1, Max: 1},
	}}}
	f.mu.Unlock()
	if err := f.s.deps.DriverRepository.Refresh(context.Background(), "test"); err != nil {
		f.t.Fatal(err)
	}
}

func (f *driverUpdateFixture) requestFor(id, action, body string, want int) map[string]any {
	f.t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/device_repository/drivers/"+id+"/"+action, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(w, r)
	if w.Code != want {
		f.t.Fatalf("%s: HTTP %d want %d: %s", action, w.Code, want, w.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		f.t.Fatal(err)
	}
	return response
}

// On the home box "Use bundled" answered "no bundled file declares driver
// easee_cloud": the release's easee_cloud.lua declares "easee-cloud". The
// same spelling kept an install from reaching the running instance until
// Core restarted. The signed manifest names the file, and the publisher's
// identity rule ties the two spellings together.
func TestChannelAndBundledSpellingsOfOneDriverMeet(t *testing.T) {
	f := newDriverUpdateFixture(t, "running")
	f.publishAs("esphome_dsmr", "esphome_dsmr.lua", "1.0.3")

	response := f.requestFor("esphome_dsmr", "install", `{"repository_id":"test"}`, 200)
	if response["runtime_verified"] != true {
		t.Fatalf("install did not reach the running instance: %v", response)
	}
	f.reading(103)
	managed := filepath.Join(f.s.managedDriverDir(), "esphome_dsmr.lua")
	if f.saved.Drivers[0].Lua != managed {
		t.Fatalf("config points at %s, want %s", f.saved.Drivers[0].Lua, managed)
	}

	w := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/drivers/catalog", nil))
	var catalog struct {
		Entries []drivers.CatalogEntry `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range catalog.Entries {
		if e.Filename == "esphome_dsmr.lua" {
			found = true
			if e.Source != "managed" || e.Version != "1.0.3" || e.ReleaseVersion != "1.0.2" {
				t.Fatalf("catalog entry = %+v; want managed 1.0.3 over the release's 1.0.2", e)
			}
		}
	}
	if !found {
		t.Fatal("catalog has no esphome_dsmr.lua")
	}

	f.requestFor("esphome_dsmr", "use_bundled", `{}`, 200)
	f.reading(102)
	if f.saved.Drivers[0].Lua != f.bundled {
		t.Fatalf("config points at %s, want the release's %s", f.saved.Drivers[0].Lua, f.bundled)
	}
}
