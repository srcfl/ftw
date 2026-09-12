package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/api"
	"github.com/srcfl/ftw/go/internal/components"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/driverrepo"
	"github.com/srcfl/ftw/go/internal/state"
)

// A config saved from the Settings screen does not pass through applyDefaults:
// handlePostConfig unmarshals what the client sent and configreload.Apply
// swaps it in whole. So every reading of an absent fleet_ping section has to
// agree with applyDefaults, or saving anything at all switches the ping off
// until the next reboot — and asks for a restart nobody needs on the way.

// noFleetPingConfig is what a client that never learned about the section
// posts: a valid config with no fleet_ping object in it.
const noFleetPingConfig = `{
  "site": {"name": "Test", "smoothing_alpha": 0.3},
  "fuse": {"max_amps": 16, "phases": 3, "voltage": 230},
  "api": {"port": 8080},
  "drivers": []
}`

func fleetPingPostRig(t *testing.T) (*api.Server, *config.Config, *sync.RWMutex) {
	t.Helper()
	cfg, err := config.Parse([]byte("site:\n  name: Test\nfuse:\n  max_amps: 16\n  phases: 3\n  voltage: 230\napi:\n  port: 8080\n"), t.TempDir())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cfgMu := &sync.RWMutex{}
	ctrlMu := &sync.Mutex{}
	srv := api.New(&api.Deps{
		Ctrl: control.NewState(0, 42, ""), CtrlMu: ctrlMu,
		Cfg: cfg, CfgMu: cfgMu,
		ConfigPath:    filepath.Join(t.TempDir(), "config.yaml"),
		DriverDir:     t.TempDir(),
		UserDriverDir: t.TempDir(),
		SaveConfig:    func(string, *config.Config) error { return nil },
	})
	return srv, cfg, cfgMu
}

func postConfigJSON(t *testing.T, srv *api.Server, body string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /api/config = %d: %s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func TestSavingAConfigWithoutTheSectionLeavesThePingOn(t *testing.T) {
	srv, cfg, cfgMu := fleetPingPostRig(t)
	enabled := fleetPingEnabled(cfg, cfgMu)
	if !enabled() {
		t.Fatal("a fresh box is not sending; applyDefaults turns the ping on")
	}

	res := postConfigJSON(t, srv, noFleetPingConfig)

	if !enabled() {
		t.Error("saving a config with no fleet_ping section switched the ping off until the next reboot")
	}
	reasons, _ := res["restart_reasons"].([]any)
	if reasons == nil {
		t.Fatal("restart_reasons must be [] when no restart is needed, not null")
	}
	for _, reason := range reasons {
		if strings.Contains(reason.(string), "fleet_ping") {
			t.Errorf("a config that never named the endpoint asked for a restart: %v", reason)
		}
	}
}

func TestSavingAnExplicitOptOutStillTurnsThePingOff(t *testing.T) {
	// Without this the test above would pass against a switch wired to true.
	srv, cfg, cfgMu := fleetPingPostRig(t)
	enabled := fleetPingEnabled(cfg, cfgMu)

	body := strings.Replace(noFleetPingConfig, `"drivers": []`,
		`"drivers": [], "fleet_ping": {"enabled": false}`, 1)
	postConfigJSON(t, srv, body)

	if enabled() {
		t.Fatal("an explicit opt-out saved from Settings was ignored")
	}
}

func TestRunningFleetPingSeesADriverInstalledLater(t *testing.T) {
	store := openStore(t)
	active := t.TempDir()
	cfg := &config.Config{FleetPing: &config.FleetPing{Enabled: true}}
	cfgMu := &sync.RWMutex{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pinger, err := startFleetPing(ctx, cfg, cfgMu, store, "v1.4.0", active)
	if err != nil {
		t.Fatalf("startFleetPing: %v", err)
	}
	if slicesContain(pinger.Payload().Drivers, "easee_cloud") {
		t.Fatal("the driver was reported before it was installed")
	}

	writeDriver(t, active, "easee_cloud.lua")
	recordInstall(t, store, config.DefaultDriverRepositoryID, "drivers/easee_cloud.lua", true)
	cfgMu.Lock()
	cfg.Drivers = append(cfg.Drivers, config.Driver{
		Name: "Laddbox",
		Lua:  filepath.Join(active, "easee_cloud.lua"),
	})
	cfgMu.Unlock()

	if got := pinger.Payload().Drivers; !slicesContain(got, "easee_cloud") {
		t.Fatalf("drivers after live install = %v, want easee_cloud", got)
	}
}

func writeDriver(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("DRIVER = {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func openStore(t *testing.T) *state.Store {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// recordInstall writes the row driverrepo writes when it activates a driver.
// ftwSigned is the fact the installer records at the moment it installs: the
// manifest naming this artifact verified against FTW's own signing key.
//
// An install that really is FTW-signed cannot be performed in a test — the
// private key behind config.DefaultDriverRepositoryPublicKey is not in this
// repository — so the row is written here the way the installer writes it.
// That the installer writes it correctly is
// TestAHouseholdsOwnKeyDoesNotRecordAsFTWSigned, next to the installer.
func recordInstall(t *testing.T, store *state.Store, repoID, logicalPath string, ftwSigned bool) {
	t.Helper()
	if _, err := store.ActivateDriverRepoInstall(state.DriverRepoInstall{
		RepoID: repoID, DriverID: strings.TrimSuffix(path.Base(logicalPath), ".lua"),
		LogicalPath: logicalPath, Version: "1.0.0", SHA256: strings.Repeat("ab", 32),
		InstalledPath: filepath.Join(t.TempDir(), path.Base(logicalPath)),
		FTWSigned:     ftwSigned,
	}); err != nil {
		t.Fatalf("ActivateDriverRepoInstall: %v", err)
	}
}

// householdRepository publishes a directory of Lua files as a repository the
// household runs itself: a manifest over file://, which is the only shape
// allow_unsigned is accepted for. It returns the manifest URL.
func householdRepository(t *testing.T, driverIDs ...string) string {
	t.Helper()
	dir := t.TempDir()
	manifest := driverrepo.Manifest{
		SchemaVersion: components.ComponentManifestSchemaVersion,
		Repository:    "the household's own",
	}
	for _, id := range driverIDs {
		body := []byte(`DRIVER = {
  id = "` + id + `",
  name = "` + id + `",
  version = "1.0.0",
  host_api_min = 1,
  host_api_max = 1,
  protocols = { "http" },
  capabilities = { "ev" },
}
function driver_init(config) end
function driver_poll() return 1000 end
function driver_default_mode() end
`)
		driverPath := filepath.Join(dir, id+".lua")
		if err := os.WriteFile(driverPath, body, 0o600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		manifest.Drivers = append(manifest.Drivers, driverrepo.ManifestDriver{
			ID: id, Path: "drivers/" + id + ".lua", Filename: id + ".lua",
			Version: "1.0.0", SHA256: hex.EncodeToString(sum[:]),
			URL:     "file://" + driverPath,
			HostAPI: components.CompatibleRange{Min: 1, Max: 1},
		})
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(manifestPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return "file://" + manifestPath
}

// The blocker this round exists to close, on the path that actually posts.
//
// A driver file's name is the household's to choose: the manifest that
// installs it says what it is called, and a household can run a repository of
// its own. So a name may travel only when the box's own install history says
// that exact file arrived under FTW's signature — a fact recorded when it
// happened, which no later edit to the config can restate.
//
// Four ways a household could otherwise have got its own filename into the
// message, all of them present here at once:
//
//   - claim the id the binary pins for the official beta channel, install
//     unsigned under it, then delete that entry the way Settings posts a
//     config. The name outlives the entry, and reading the config afterwards
//     finds nothing to condemn it;
//   - install unsigned, then switch the entry off. Validation skips every URL
//     and key rule for a disabled repository, so it can say anything;
//   - copy a file into the active dir by hand. No entry ever named it;
//   - install something real and rename the file afterwards, so the name on
//     disk is not the name that was installed.
//
// The two controls matter as much: a shipped catalogue name and an FTW-signed
// install must still travel, or this passes by sending nothing.
func TestNoNameTheHouseholdChoseReachesThePostedPing(t *testing.T) {
	root := t.TempDir()
	store := openStore(t)

	claimsTheBetaID := householdRepository(t, "pappas-laddbox")
	switchedOffLater := householdRepository(t, "grannens-vaxelriktare")

	manager := driverrepo.New(&config.DeviceRepository{
		Enabled: true,
		Repositories: []config.DriverRepositorySource{
			{
				ID: config.DefaultDriverRepositoryID, Enabled: true,
				ManifestURL: config.DefaultDriverRepositoryManifestURL,
				TrustedKeys: map[string]string{
					config.DefaultDriverRepositorySigningKeyID: config.DefaultDriverRepositoryPublicKey,
				},
			},
			// The id the binary pins for the official beta channel, taken by
			// the household for a directory of its own Lua files.
			{
				ID: config.DefaultDriverRepositoryBetaID, Enabled: true,
				ManifestURL: claimsTheBetaID, AllowUnsigned: true, AllowInsecure: true,
			},
			{
				ID: "grannen", Enabled: true,
				ManifestURL: switchedOffLater, AllowUnsigned: true, AllowInsecure: true,
			},
		},
	}, root, store)
	for _, id := range []string{config.DefaultDriverRepositoryBetaID, "grannen"} {
		if err := manager.Refresh(context.Background(), id); err != nil {
			t.Fatalf("refresh %s: %v", id, err)
		}
	}
	if _, err := manager.Install(context.Background(), config.DefaultDriverRepositoryBetaID, "pappas-laddbox", ""); err != nil {
		t.Fatalf("install under the claimed beta id: %v", err)
	}
	if _, err := manager.Install(context.Background(), "grannen", "grannens-vaxelriktare", ""); err != nil {
		t.Fatalf("install from the entry that is switched off later: %v", err)
	}
	active := manager.ActiveDir()
	for _, name := range []string{"pappas-laddbox.lua", "grannens-vaxelriktare.lua"} {
		if _, err := os.Stat(filepath.Join(active, name)); err != nil {
			t.Fatalf("%s was not activated, so there is nothing to leak: %v", name, err)
		}
	}

	// Copied in by hand. Nothing ever installed it.
	writeDriver(t, active, "handkopierad.lua")

	// Installed under FTW's signature as drivers/tibber.lua, then renamed on
	// disk. The record still names tibber; the file does not.
	writeDriver(t, active, "pappas-elmatare.lua")
	recordInstall(t, store, config.DefaultDriverRepositoryID, "drivers/tibber.lua", true)

	// The controls. easee_cloud came from the official repository under FTW's
	// signature; sungrow is in drivers/BUNDLED_SOURCE.json, so it is a name
	// this build publishes whatever is on disk. The file exists because the
	// config has to point at something, not because the catalogue reads it.
	writeDriver(t, active, "easee_cloud.lua")
	recordInstall(t, store, config.DefaultDriverRepositoryID, "drivers/easee_cloud.lua", true)
	bundled := t.TempDir()
	writeDriver(t, bundled, "sungrow.lua")

	// The config as Settings saves it: posted whole, with no applyDefaults on
	// the way in. The entry that installed pappas-laddbox is gone; the one that
	// installed grannens-vaxelriktare is still there but switched off, and has
	// dropped the allow_unsigned that would have condemned it — validation
	// skips every URL and key rule for a disabled repository, so a disabled
	// entry can say whatever it likes.
	srv, cfg, cfgMu := fleetPingPostRig(t)
	lua := func(name string) string { return filepath.Join(active, name) }
	postConfigJSON(t, srv, strings.Replace(noFleetPingConfig, `"drivers": []`,
		`"drivers": [`+
			driverJSON("Pappas laddbox", lua("pappas-laddbox.lua"))+`,`+
			driverJSON("Grannens", lua("grannens-vaxelriktare.lua"))+`,`+
			driverJSON("Handkopierad", lua("handkopierad.lua"))+`,`+
			driverJSON("Pappas elmatare", lua("pappas-elmatare.lua"))+`,`+
			driverJSON("Laddbox", lua("easee_cloud.lua"))+`,`+
			siteMeterJSON("Vaxelriktare", filepath.Join(bundled, "sungrow.lua"))+
			`], "device_repository": {"enabled": true, "repositories": [`+
			`{"id": "`+config.DefaultDriverRepositoryID+`", "enabled": true, `+
			`"manifest_url": "`+config.DefaultDriverRepositoryManifestURL+`", `+
			`"trusted_keys": {"`+config.DefaultDriverRepositorySigningKeyID+`": "`+
			config.DefaultDriverRepositoryPublicKey+`"}},`+
			`{"id": "grannen", "enabled": false}]}`, 1))

	// Cancelled before the pinger is built: Run has nothing to do, and the rest
	// of startFleetPing — endpoint, catalogue, facts closure — is the production
	// path unchanged. Payload() is what sendOnce marshals and posts.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pinger, err := startFleetPing(ctx, cfg, cfgMu, store, "v1.4.0", active)
	if err != nil {
		t.Fatalf("startFleetPing: %v", err)
	}
	body, err := json.Marshal(pinger.Payload())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	posted := string(body)

	for _, leak := range []string{"pappas", "laddbox", "grannen", "handkopierad", "elmatare"} {
		if strings.Contains(strings.ToLower(posted), leak) {
			t.Errorf("the posted ping contains %q: %s", leak, posted)
		}
	}
	for _, kept := range []string{`"sungrow"`, `"easee_cloud"`, `"other"`} {
		if !strings.Contains(posted, kept) {
			t.Errorf("the posted ping is missing %s, so this passes by sending nothing: %s", kept, posted)
		}
	}
}

func driverJSON(name, luaPath string) string {
	return `{"name": "` + name + `", "lua": "` + luaPath +
		`", "capabilities": {"standalone": true}}`
}

// A site needs one meter or the config will not validate.
func siteMeterJSON(name, luaPath string) string {
	return `{"name": "` + name + `", "lua": "` + luaPath +
		`", "is_site_meter": true, "capabilities": {"standalone": true}}`
}

// Provenance is history, and the config is not. A config that says every
// repository on the box is signed, official and enabled changes nothing about
// a file that was installed under a repository that was none of those.
func TestARewrittenConfigCannotVouchForWhatItDidNotInstall(t *testing.T) {
	active := t.TempDir()
	writeDriver(t, active, "pappas-laddbox.lua")
	writeDriver(t, active, "easee_cloud.lua")

	store := openStore(t)
	// Both rows name the official repository. Only one of them was installed
	// under FTW's signature, and that is the only thing read.
	recordInstall(t, store, config.DefaultDriverRepositoryID, "drivers/pappas-laddbox.lua", false)
	recordInstall(t, store, config.DefaultDriverRepositoryID, "drivers/easee_cloud.lua", true)

	names := fleetCatalogue(store, active)

	if slicesContain(names, "pappas-laddbox") {
		t.Errorf("catalogue = %v; naming the official repository in the config is not a signature", names)
	}
	if !slicesContain(names, "easee_cloud") {
		t.Errorf("catalogue = %v; a driver FTW signed is a type the fleet has in common", names)
	}
	if !slicesContain(names, "sungrow") {
		t.Errorf("catalogue = %v; the shipped catalogue is compiled in and always counts", names)
	}
}

// The shipped catalogue must be something the config cannot move.
//
// `device_repository.root_dir` is an ordinary config field — it says where
// installed drivers are kept — and nothing stops it naming a directory inside
// the one the build's own drivers sit in. A shipped catalogue discovered by
// walking that directory therefore asks the household's config what shipped,
// and every managed artifact underneath it answers "I did".
//
// One POST is the whole thing: the root, and a repository the household runs.
// The install record is written correctly — not FTW's signature — and then
// never read, because the file is found under the bundled tree and never
// reaches the branch that asks. No shell on the box, nothing written to its
// filesystem by hand.
func TestARepositoryRootInsideTheBundledTreeIsNotTheShippedCatalogue(t *testing.T) {
	bundled := t.TempDir()
	// The control. sungrow is in drivers/BUNDLED_SOURCE.json, so it is a name
	// this build really publishes, and the file is on disk the way it is on a
	// box. Without it this test could pass by sending nothing.
	writeDriver(t, bundled, "sungrow.lua")
	store := openStore(t)
	srv, cfg, cfgMu := fleetPingPostRig(t)

	// The one POST. The root lands inside the bundled tree; the repository is
	// a directory of the household's own Lua files.
	own := householdRepository(t, "pappas-laddbox")
	repositoryJSON := `"device_repository": {"enabled": true, "root_dir": "` +
		filepath.Join(bundled, "mina-drivrutiner") + `", "repositories": [` +
		`{"id": "mina", "enabled": true, "manifest_url": "` + own + `", ` +
		`"allow_unsigned": true, "allow_insecure": true}]}`
	postConfigJSON(t, srv, strings.Replace(noFleetPingConfig, `"drivers": []`,
		`"drivers": [`+siteMeterJSON("Vaxelriktare", filepath.Join(bundled, "sungrow.lua"))+`], `+
			repositoryJSON, 1))

	// The next boot, wired the way main.go wires it: the manager reads its
	// root out of the config that was just saved, and its active dir is what
	// the ping is handed as the managed one.
	manager := driverrepo.NewWithHostVersion(cfg.DeviceRepository, t.TempDir(), store, "v1.4.0")
	if err := manager.Refresh(context.Background(), "mina"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, err := manager.Install(context.Background(), "mina", "pappas-laddbox", ""); err != nil {
		t.Fatalf("install: %v", err)
	}
	active := manager.ActiveDir()
	if _, err := os.Stat(filepath.Join(active, "pappas-laddbox.lua")); err != nil {
		t.Fatalf("pappas-laddbox.lua was not activated, so there is nothing to leak: %v", err)
	}
	// The premise of the whole test: the file really is under the bundled tree.
	rel, err := filepath.Rel(bundled, active)
	if err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("the active dir landed outside the bundled tree (%q), so the config never moved anything", active)
	}
	// And the box recorded what happened, correctly.
	installed, err := store.ActiveDriverRepoInstall("drivers/pappas-laddbox.lua")
	if err != nil {
		t.Fatalf("read the install record back: %v", err)
	}
	if installed.FTWSigned {
		t.Fatal("the install record claims FTW signed it; this test is aimed at the wrong thing")
	}

	// The config naming it, posted the way Settings posts one.
	postConfigJSON(t, srv, strings.Replace(noFleetPingConfig, `"drivers": []`,
		`"drivers": [`+
			driverJSON("Pappas laddbox", filepath.Join(active, "pappas-laddbox.lua"))+`,`+
			siteMeterJSON("Vaxelriktare", filepath.Join(bundled, "sungrow.lua"))+`], `+
			repositoryJSON, 1))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pinger, err := startFleetPing(ctx, cfg, cfgMu, store, "v1.4.0", active)
	if err != nil {
		t.Fatalf("startFleetPing: %v", err)
	}
	body, err := json.Marshal(pinger.Payload())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	posted := string(body)

	for _, leak := range []string{"pappas", "laddbox", "drivrutiner"} {
		if strings.Contains(strings.ToLower(posted), leak) {
			t.Errorf("the posted ping contains %q: %s", leak, posted)
		}
	}
	for _, kept := range []string{`"sungrow"`, `"other"`} {
		if !strings.Contains(posted, kept) {
			t.Errorf("the posted ping is missing %s, so this passes by sending nothing: %s", kept, posted)
		}
	}
}

func slicesContain(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
