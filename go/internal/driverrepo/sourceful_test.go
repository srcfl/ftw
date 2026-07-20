package driverrepo

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/state"
)

type sourcefulFixture struct {
	mu              sync.Mutex
	private         ed25519.PrivateKey
	indexEnvelope   []byte
	packageEnvelope []byte
	artifact        []byte
	artifactPath    string
}

func (f *sourcefulFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/index.json":
		_, _ = w.Write(f.indexEnvelope)
	case "/sdm630-package.json":
		_, _ = w.Write(f.packageEnvelope)
	case f.artifactPath:
		_, _ = w.Write(f.artifact)
	default:
		http.NotFound(w, r)
	}
}

func signSourcefulFixture(
	t *testing.T,
	private ed25519.PrivateKey,
	schema string,
	payloadType string,
	payload any,
) []byte {
	t.Helper()
	payloadRaw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := canonicalJSON(payloadRaw)
	if err != nil {
		t.Fatal(err)
	}
	envelope := map[string]any{
		"schema_version":   schema,
		"payload_type":     payloadType,
		"canonicalization": sourcefulCanonicalJSON,
		"key_id":           "sourceful-test-1",
		"algorithm":        "Ed25519",
		"payload":          json.RawMessage(payloadRaw),
		"signature":        base64.StdEncoding.EncodeToString(ed25519.Sign(private, canonical)),
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}

func (f *sourcefulFixture) build(t *testing.T, serverURL string, readOnly, controlEnabled bool) {
	t.Helper()
	hostAPIMin, hostAPIMax := 1, 1
	runtimeABI, hostAPIProfile := sourcefulFTWABIV1, sourcefulFTWHostAPIProfileV1
	readOnlyLua := "true"
	commandEntrypoints := `function driver_command() return false end
function driver_default_mode() end`
	if !readOnly {
		hostAPIMin, hostAPIMax = 2, 2
		runtimeABI, hostAPIProfile = sourcefulFTWABIV2, sourcefulFTWHostAPIProfileV2
		readOnlyLua = "false"
		commandEntrypoints = `function driver_command_v2(command)
  return {status="applied", code="ok", device_state="controlled", evidence={"write_ack", "readback"}}
end
function driver_default_mode_v2(context)
  return {status="defaulted", code="default_restored", device_state="default", evidence={"write_ack", "readback"}}
end`
	}
	f.artifact = []byte(fmt.Sprintf(`DRIVER = {
  id = "sdm630",
  name = "Eastron SDM630 meter",
  version = "1.1.1",
  host_api_min = %d,
  host_api_max = %d,
  protocols = { "modbus" },
  capabilities = { "meter" },
  read_only = %s,
}
function driver_init(config) end
function driver_poll() return 1000 end
%s
`, hostAPIMin, hostAPIMax, readOnlyLua, commandEntrypoints))
	artifactSum := sha256.Sum256(f.artifact)
	artifactHash := hex.EncodeToString(artifactSum[:])
	artifactFilename := "sdm630-1.1.1-ftw-core-ftw.lua51.source-" + artifactHash + ".lua"
	f.artifactPath = "/" + artifactFilename

	control := []string{}
	commands := []any{}
	defaultMode := map[string]any{
		"strategy":    "not_applicable",
		"description": "Telemetry-only meter.",
	}
	lease := map[string]any{
		"required_for_control": false,
		"expiry_action":        "not_applicable",
	}
	if !readOnly {
		control = []string{"set_power"}
		commands = []any{map[string]any{
			"id": "set_power", "capability": "set_power", "runtime_action": "set_power",
			"description": "Set power.", "inputs": []any{},
		}}
		defaultMode = map[string]any{
			"strategy": "vendor_autonomous", "entrypoint": "driver_default_mode_v2",
			"description": "Return to vendor control.",
		}
		lease = map[string]any{
			"required_for_control": true, "max_duration_seconds": 30,
			"heartbeat_interval_seconds": 10, "expiry_action": "return_to_default",
		}
	}

	permissions := []string{"modbus.read"}
	if !readOnly {
		permissions = append(permissions, "modbus.write")
	}
	hostMinVersion := "1.4.0"
	if !readOnly {
		hostMinVersion = "1.7.0"
	}
	packagePayload := map[string]any{
		"schema_version": sourcefulPackageSchema,
		"package_id":     "com.sourceful.driver.sdm630",
		"version":        "1.1.1",
		"channel":        "beta",
		"display_name":   "Eastron SDM630",
		"identity": map[string]any{
			"schema": "sourceful.hardware-identity/v1", "make": "driver_reported",
			"serial": "driver_reported_when_available", "host_fallbacks": []string{"mac", "endpoint"},
			"persistent_state_owner": "host",
		},
		"source": map[string]any{
			"repository": serverURL, "commit": strings.Repeat("a", 40), "path": "drivers/lua/sdm630.lua",
		},
		"provenance": map[string]any{
			"builder_id": serverURL + "/builder", "build_type": sourcefulPackageSchema,
			"source_date_epoch": 1700000000,
			"materials":         []any{map[string]any{"uri": "git+sourceful", "sha256": artifactHash}},
		},
		"device_matches": []any{map[string]any{
			"manufacturer": "Eastron", "model_family": "SDM630",
			"variants": []string{"SDM630-Modbus"}, "regions": []string{},
		}},
		"capabilities": map[string]any{"telemetry": []string{"meter"}, "control": control},
		"permissions":  permissions,
		"telemetry": map[string]any{
			"schema": "sourceful.telemetry/v2", "sign_convention": "sourceful.site-import-positive/v1",
			"streams": []any{map[string]any{
				"kind": "meter", "power_field": "w", "meaning": "Positive means grid import.",
			}},
		},
		"commands":     commands,
		"read_only":    readOnly,
		"default_mode": defaultMode,
		"lease_policy": lease,
		"rollback": map[string]any{
			"strategy": "install_previous_verified_package", "state_owner": "host", "automatic": false,
		},
		"compatibility": []any{map[string]any{
			"target": sourcefulFTWTarget, "artifact_id": "ftw.lua51.source",
			"host": map[string]any{
				"product": sourcefulFTWHostProduct, "min_version": hostMinVersion, "max_version_exclusive": "2.0.0",
			},
			"runtime": map[string]any{
				"name": sourcefulFTWRuntime, "semantics": sourcefulFTWSemantics,
				"version": sourcefulFTWRuntimeVersion, "abi": runtimeABI,
				"host_api": map[string]any{"profile": hostAPIProfile, "min": hostAPIMin, "max": hostAPIMax},
			},
			"control_enabled": controlEnabled,
		}},
		"artifacts": []any{map[string]any{
			"artifact_id": "ftw.lua51.source", "target": sourcefulFTWTarget,
			"media_type": "application/vnd.sourceful.lua.source", "filename": artifactFilename,
			"url": serverURL + f.artifactPath, "sha256": artifactHash, "size_bytes": len(f.artifact),
		}},
	}
	f.packageEnvelope = signSourcefulFixture(
		t, f.private, sourcefulPackageEnvelopeSchema, sourcefulPackagePayloadType, packagePayload,
	)
	packageSum := sha256.Sum256(f.packageEnvelope)
	packageHash := hex.EncodeToString(packageSum[:])
	indexPayload := map[string]any{
		"schema_version": sourcefulIndexSchema, "channel": "beta", "source_date_epoch": 1700000000,
		"packages": []any{map[string]any{
			"package_id": "com.sourceful.driver.sdm630", "version": "1.1.1",
			"envelope_url": serverURL + "/sdm630-package.json", "envelope_sha256": packageHash,
			"targets": []string{sourcefulFTWTarget},
		}},
	}
	f.indexEnvelope = signSourcefulFixture(
		t, f.private, sourcefulIndexEnvelopeSchema, sourcefulIndexPayloadType, indexPayload,
	)
}

func TestSourcefulIndexPackageInstallAndOfflineCache(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &sourcefulFixture{private: private}
	server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	fixture.build(t, server.URL, true, false)

	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := &config.DeviceRepository{Enabled: true, Repositories: []config.DriverRepositorySource{{
		ID: "sourceful", Name: "Sourceful Device Support",
		Format:      config.DriverRepositoryFormatSourcefulIndexV1,
		ManifestURL: server.URL + "/index.json", Enabled: true, AllowInsecure: true,
		TrustedKeys: map[string]string{"sourceful-test-1": base64.StdEncoding.EncodeToString(public)},
	}}}
	manager := NewWithHostVersion(cfg, dir, store, "1.4.0")
	if err := manager.Refresh(context.Background(), "sourceful"); err != nil {
		t.Fatal(err)
	}
	catalog, err := manager.Catalog()
	if err != nil || len(catalog) != 1 {
		t.Fatalf("catalog = %+v, %v", catalog, err)
	}
	driver := catalog[0].Driver
	if driver.ID != "sdm630" || !driver.Metadata.ReadOnly || driver.PackageID != "com.sourceful.driver.sdm630" ||
		driver.Channel != "beta" || driver.SourceCommit != strings.Repeat("a", 40) ||
		driver.RuntimeABI != sourcefulFTWABIV1 || driver.HostAPIProfile != sourcefulFTWHostAPIProfileV1 ||
		driver.PackageKeyID != "sourceful-test-1" {
		t.Fatalf("adapted driver = %+v", driver)
	}
	installed, err := manager.Install(context.Background(), "sourceful", "sdm630", "1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	if installed.Version != "1.1.1" {
		t.Fatalf("installed = %+v", installed)
	}
	active, err := os.ReadFile(filepath.Join(manager.ActiveDir(), "sdm630.lua"))
	if err != nil || string(active) != string(fixture.artifact) {
		t.Fatalf("active artifact mismatch: %v", err)
	}
	managed := manager.EnrichCatalog([]drivers.CatalogEntry{{
		Path: "drivers/sdm630.lua", ID: "sdm630", Version: "1.1.1", Source: "managed",
	}})[0]
	if managed.PackageID != "com.sourceful.driver.sdm630" || managed.PackageChannel != "beta" ||
		managed.ArtifactSHA256 != installed.SHA256 || managed.RuntimeABI != sourcefulFTWABIV1 ||
		managed.HostAPIProfile != sourcefulFTWHostAPIProfileV1 {
		t.Fatalf("managed catalog provenance = %+v", managed)
	}
	local := manager.EnrichCatalog([]drivers.CatalogEntry{{
		Path: "drivers/sdm630.lua", ID: "sdm630", Version: "local", Source: "local",
	}})[0]
	if local.RepositoryID != "" || local.PackageID != "" || local.ArtifactSHA256 != "" || local.UpdateAvailable {
		t.Fatalf("local catalog claimed managed provenance = %+v", local)
	}
	policy, err := manager.RuntimePolicy(config.Driver{
		Name: "sdm630", Lua: filepath.Join(manager.ActiveDir(), "sdm630.lua"),
	})
	if err != nil || policy == nil || !policy.IsReadOnly() || !policy.Permissions["modbus.read"] || policy.Permissions["modbus.write"] {
		t.Fatalf("signed read-only runtime policy = %+v, %v", policy, err)
	}

	// A failed refresh cannot replace the last-good in-memory or on-disk view.
	fixture.mu.Lock()
	fixture.packageEnvelope = append([]byte(nil), fixture.packageEnvelope...)
	fixture.packageEnvelope[0] ^= 1
	fixture.mu.Unlock()
	if err := manager.Refresh(context.Background(), "sourceful"); err == nil {
		t.Fatal("tampered package envelope accepted")
	}
	if catalog, err := manager.Catalog(); err != nil || len(catalog) != 1 {
		t.Fatalf("last-good catalog lost after tamper: %+v, %v", catalog, err)
	}

	server.Close()
	reloaded := NewWithHostVersion(cfg, dir, store, "1.4.0")
	if catalog, err := reloaded.Catalog(); err != nil || len(catalog) != 1 {
		t.Fatalf("offline Sourceful catalog = %+v, %v", catalog, err)
	}
	packageCachePath := reloaded.sourcefulPackageCachePath(
		cfg.Repositories[0], driver.PackageEnvelopeSHA256,
	)
	if err := os.WriteFile(packageCachePath, []byte("tampered cache"), 0o600); err != nil {
		t.Fatal(err)
	}
	corrupted := NewWithHostVersion(cfg, dir, store, "1.4.0")
	if _, err := corrupted.manifestFor(cfg.Repositories[0]); err == nil {
		t.Fatal("tampered offline package cache accepted")
	}
}

func TestSourcefulRejectsAmbiguousJSON(t *testing.T) {
	if _, err := canonicalJSON([]byte(`{"a":1,"a":2}`)); err == nil {
		t.Fatal("duplicate JSON object keys accepted")
	}
	if canonical, err := canonicalJSON([]byte(`{"empty":[]}`)); err != nil || string(canonical) != `{"empty":[]}` {
		t.Fatalf("empty array canonicalization = %q, %v", canonical, err)
	}
}

func TestSourcefulStagedControlTargetIsExcluded(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	fixture := &sourcefulFixture{private: private}
	server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	defer server.Close()
	fixture.build(t, server.URL, false, false)
	cfg := &config.DeviceRepository{Enabled: true, Repositories: []config.DriverRepositorySource{{
		ID: "sourceful", Format: config.DriverRepositoryFormatSourcefulIndexV1,
		ManifestURL: server.URL + "/index.json", Enabled: true, AllowInsecure: true,
		TrustedKeys: map[string]string{"sourceful-test-1": base64.StdEncoding.EncodeToString(public)},
	}}}
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager := NewWithHostVersion(cfg, dir, store, "1.7.0")
	if err := manager.Refresh(context.Background(), "sourceful"); err != nil {
		t.Fatal(err)
	}
	catalog, err := manager.Catalog()
	if err != nil || len(catalog) != 0 {
		t.Fatalf("staged control catalog = %+v, %v", catalog, err)
	}
}

func TestSourcefulControlV2RequiresExactActivePackagePin(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	fixture := &sourcefulFixture{private: private}
	server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	defer server.Close()
	fixture.build(t, server.URL, false, true)

	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := &config.DeviceRepository{Enabled: true, Repositories: []config.DriverRepositorySource{{
		ID: "sourceful", Format: config.DriverRepositoryFormatSourcefulIndexV1,
		ManifestURL: server.URL + "/index.json", Enabled: true, AllowInsecure: true,
		TrustedKeys: map[string]string{"sourceful-test-1": base64.StdEncoding.EncodeToString(public)},
	}}}
	manager := NewWithHostVersion(cfg, dir, store, "1.7.0")
	if err := manager.Refresh(context.Background(), "sourceful"); err != nil {
		t.Fatal(err)
	}
	catalog, err := manager.Catalog()
	if err != nil || len(catalog) != 1 {
		t.Fatalf("control v2 catalog = %+v, %v", catalog, err)
	}
	entry := catalog[0].Driver
	if entry.ReadOnly || !entry.ControlEnabled || entry.RuntimeABI != sourcefulFTWABIV2 ||
		entry.HostAPIProfile != sourcefulFTWHostAPIProfileV2 {
		t.Fatalf("control v2 entry = %+v", entry)
	}
	installed, err := manager.Install(context.Background(), "sourceful", "sdm630", "1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(installed.InstalledPath), sourcefulInstalledPackageEnvelope)); err != nil {
		t.Fatalf("installed signed package envelope: %v", err)
	}
	driverCfg := config.Driver{
		Name: "sdm630", Lua: filepath.Join(manager.ActiveDir(), "sdm630.lua"),
		Control: &config.DriverControlOptIn{
			Enabled: true, PackageID: entry.PackageID, Version: entry.Version, ArtifactSHA256: entry.SHA256,
		},
	}
	resolved, err := filepath.EvalSymlinks(driverCfg.Lua)
	installedRoot, rootErr := filepath.EvalSymlinks(filepath.Join(manager.root, "installed"))
	if err != nil || rootErr != nil || !pathInside(installedRoot, resolved) {
		t.Fatalf("active managed path %q resolved to %q: %v", driverCfg.Lua, resolved, err)
	}
	policy, err := manager.RuntimePolicy(driverCfg)
	if err != nil || policy == nil || !policy.SiteEnabled || !policy.Permissions["modbus.write"] {
		t.Fatalf("selected control policy = %+v, %v", policy, err)
	}
	driverCfg.Control = nil
	policy, err = manager.RuntimePolicy(driverCfg)
	if err != nil || policy == nil || policy.SiteEnabled {
		t.Fatalf("unselected control policy = %+v, %v", policy, err)
	}
	driverCfg.Control = &config.DriverControlOptIn{
		Enabled: true, PackageID: entry.PackageID, Version: entry.Version, ArtifactSHA256: entry.SHA256,
	}
	driverCfg.Control.ArtifactSHA256 = strings.Repeat("0", 64)
	if _, err := manager.RuntimePolicy(driverCfg); err == nil || !strings.Contains(err.Error(), "pin does not match") {
		t.Fatalf("wrong artifact pin error = %v", err)
	}
}

func TestDeviceSupportPythonContract(t *testing.T) {
	indexPath := os.Getenv("FTW_DEVICE_SUPPORT_INDEX")
	packagePath := os.Getenv("FTW_DEVICE_SUPPORT_PACKAGE")
	artifactDir := os.Getenv("FTW_DEVICE_SUPPORT_ARTIFACT_DIR")
	publicKey := os.Getenv("FTW_DEVICE_SUPPORT_PUBLIC_KEY")
	if indexPath == "" || packagePath == "" || artifactDir == "" || publicKey == "" {
		t.Skip("Device Support cross-language fixture not configured")
	}
	indexRaw, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	packageRaw, err := os.ReadFile(packagePath)
	if err != nil {
		t.Fatal(err)
	}
	repo := config.DriverRepositorySource{
		ID: "device-support-ci", Format: config.DriverRepositoryFormatSourcefulIndexV1,
		ManifestURL: "https://packages.example/index.json", Enabled: true,
		TrustedKeys: map[string]string{"sourceful-test-1": publicKey},
	}
	manager := NewWithHostVersion(&config.DeviceRepository{}, t.TempDir(), nil, "1.4.0")
	manifest, _, err := manager.sourcefulManifest(repo, indexRaw, func(sourcefulDriverPackageRef) ([]byte, error) {
		return packageRaw, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Drivers) != 1 || manifest.Drivers[0].ID != "sdm630" {
		t.Fatalf("cross-language manifest = %+v", manifest)
	}
	entry := manifest.Drivers[0]
	artifactURL, err := url.Parse(entry.URL)
	if err != nil {
		t.Fatal(err)
	}
	artifactPath := filepath.Join(artifactDir, filepath.Base(artifactURL.Path))
	raw, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != entry.SHA256 || int64(len(raw)) != entry.SizeBytes {
		t.Fatal("Device Support artifact does not match the signed FTW target")
	}
	if err := validateLuaArtifact(artifactPath, entry); err != nil {
		t.Fatalf("Device Support FTW Lua target: %v", err)
	}
}
