package driverrepo

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

type installedPolicyFixture struct {
	root      string
	statePath string
	store     *state.Store
	repo      config.DriverRepositorySource
	installed state.DriverRepoInstall
	driver    config.Driver
}

type installedPolicyModbus struct {
	value  uint16
	writes int
}

func (m *installedPolicyModbus) Read(uint16, uint16, int32) ([]uint16, error) {
	return []uint16{m.value}, nil
}

func (m *installedPolicyModbus) WriteSingle(_ uint16, value uint16) error {
	m.value = value
	m.writes++
	return nil
}

func (m *installedPolicyModbus) WriteMulti(_ uint16, values []uint16) error {
	if len(values) > 0 {
		m.value = values[0]
	}
	m.writes++
	return nil
}

func (*installedPolicyModbus) Close() error { return nil }

func makeSourcefulDefaultObservable(t *testing.T, fixture *sourcefulFixture, serverURL string) {
	t.Helper()
	fixture.artifact = []byte(`DRIVER = {
  id = "sdm630",
  name = "Eastron SDM630 meter",
  version = "1.1.1",
  host_api_min = 2,
  host_api_max = 2,
  protocols = { "modbus" },
  capabilities = { "meter" },
  read_only = false,
}
function driver_init(config) end
function driver_poll() return 1000 end
function driver_command_v2(command)
  return {status="applied", code="ok", device_state="controlled", evidence={"write_ack", "readback"}}
end
function driver_default_mode_v2(context)
  local write_err = host.modbus_write(10, 0)
  if write_err then error(write_err) end
  local value, read_err = host.modbus_read(10, 1, "holding")
  if read_err then error(read_err) end
  return {status="defaulted", code="default_restored", device_state="default", evidence={"write_ack", "readback"}}
end
`)
	artifactSum := sha256.Sum256(fixture.artifact)
	artifactHash := hex.EncodeToString(artifactSum[:])
	artifactFilename := "sdm630-1.1.1-ftw-core-ftw.lua51.source-" + artifactHash + ".lua"
	fixture.artifactPath = "/" + artifactFilename

	var packageEnvelope sourcefulSignedEnvelope
	if err := json.Unmarshal(fixture.packageEnvelope, &packageEnvelope); err != nil {
		t.Fatal(err)
	}
	var pkg sourcefulPackage
	if err := json.Unmarshal(packageEnvelope.Payload, &pkg); err != nil {
		t.Fatal(err)
	}
	pkg.Artifacts[0].Filename = artifactFilename
	pkg.Artifacts[0].URL = serverURL + fixture.artifactPath
	pkg.Artifacts[0].SHA256 = artifactHash
	pkg.Artifacts[0].SizeBytes = int64(len(fixture.artifact))
	pkg.Provenance.Materials[0].SHA256 = artifactHash
	fixture.packageEnvelope = signSourcefulFixture(
		t, fixture.private, sourcefulPackageEnvelopeSchema, sourcefulPackagePayloadType, pkg,
	)
	packageSum := sha256.Sum256(fixture.packageEnvelope)

	var indexEnvelope sourcefulSignedEnvelope
	if err := json.Unmarshal(fixture.indexEnvelope, &indexEnvelope); err != nil {
		t.Fatal(err)
	}
	var index sourcefulDriverIndex
	if err := json.Unmarshal(indexEnvelope.Payload, &index); err != nil {
		t.Fatal(err)
	}
	index.Packages[0].EnvelopeSHA256 = hex.EncodeToString(packageSum[:])
	fixture.indexEnvelope = signSourcefulFixture(
		t, fixture.private, sourcefulIndexEnvelopeSchema, sourcefulIndexPayloadType, index,
	)
}

func newInstalledSourcefulControlFixture(t *testing.T) *installedPolicyFixture {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &sourcefulFixture{private: private}
	server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	t.Cleanup(server.Close)
	fixture.build(t, server.URL, false, true)
	makeSourcefulDefaultObservable(t, fixture, server.URL)

	root := t.TempDir()
	statePath := filepath.Join(root, "state.db")
	store, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	f := &installedPolicyFixture{
		root:      root,
		statePath: statePath,
		store:     store,
		repo: config.DriverRepositorySource{
			ID: "sourceful", Format: config.DriverRepositoryFormatSourcefulIndexV1,
			ManifestURL: server.URL + "/index.json", Enabled: true, AllowInsecure: true,
			TrustedKeys: map[string]string{"sourceful-test-1": base64.StdEncoding.EncodeToString(public)},
		},
	}
	t.Cleanup(func() {
		if f.store != nil {
			_ = f.store.Close()
		}
	})

	manager := f.manager(t, []config.DriverRepositorySource{f.repo}, "1.7.0")
	if err := manager.Refresh(context.Background(), f.repo.ID); err != nil {
		t.Fatal(err)
	}
	catalog, err := manager.Catalog()
	if err != nil || len(catalog) != 1 {
		t.Fatalf("control v2 catalog = %+v, %v", catalog, err)
	}
	f.installed, err = manager.Install(context.Background(), f.repo.ID, "sdm630", "1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	if f.installed.RepositoryFormat != config.DriverRepositoryFormatSourcefulIndexV1 {
		t.Fatalf("installed repository format = %q", f.installed.RepositoryFormat)
	}
	f.driver = config.Driver{
		Name: "sdm630", Lua: filepath.Join(manager.ActiveDir(), "sdm630.lua"),
		Modbus: &config.ModbusConfig{Host: "device", Port: 502},
	}
	return f
}

func (f *installedPolicyFixture) manager(
	t *testing.T,
	repositories []config.DriverRepositorySource,
	hostVersion string,
) *Manager {
	t.Helper()
	if f.store == nil {
		t.Fatal("manager requested while state store is closed")
	}
	cfg := &config.DeviceRepository{Enabled: true, Repositories: repositories}
	return NewWithHostVersion(cfg, f.root, f.store, hostVersion)
}

func (f *installedPolicyFixture) closeStore(t *testing.T) {
	t.Helper()
	if f.store == nil {
		return
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store = nil
}

func (f *installedPolicyFixture) reopenStore(t *testing.T) {
	t.Helper()
	if f.store != nil {
		t.Fatal("state store is already open")
	}
	store, err := state.Open(f.statePath)
	if err != nil {
		t.Fatal(err)
	}
	f.store = store
}

func (f *installedPolicyFixture) setHistoricalUnknownFormat(t *testing.T) {
	t.Helper()
	f.closeStore(t)
	db, err := sql.Open("sqlite", f.statePath)
	if err != nil {
		t.Fatal(err)
	}
	result, execErr := db.Exec(`UPDATE driver_repo_installs SET repository_format = '' WHERE id = ?`, f.installed.ID)
	closeErr := db.Close()
	if execErr != nil {
		t.Fatal(execErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		t.Fatalf("rows changed = %d, %v", rows, err)
	}
	f.reopenStore(t)
}

func (f *installedPolicyFixture) installRow(t *testing.T) state.DriverRepoInstall {
	t.Helper()
	installed, err := f.store.ActiveDriverRepoInstall(f.installed.LogicalPath)
	if err != nil {
		t.Fatal(err)
	}
	return installed
}

func (f *installedPolicyFixture) invalidateLua(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(f.installed.InstalledPath, []byte(`this is not Lua`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertRegistryRejectsBeforeLua(t *testing.T, manager *Manager, driver config.Driver) {
	t.Helper()
	registry := drivers.NewRegistry(telemetry.NewStore())
	registry.RuntimePolicyResolver = manager.RuntimePolicy
	modbus := &installedPolicyModbus{}
	registry.ModbusFactory = func(string, *config.ModbusConfig) (drivers.ModbusCap, error) {
		return modbus, nil
	}
	defer registry.ShutdownAll()
	err := registry.Add(context.Background(), driver)
	if err == nil {
		t.Fatal("Registry.Add accepted invalid installed policy state")
	}
	if !strings.Contains(err.Error(), "runtime policy:") {
		t.Fatalf("Registry.Add reached Lua instead of stopping at runtime policy: %v", err)
	}
}

func TestInstalledSourcefulControlPolicyRunsDefaultWithoutSiteOptIn(t *testing.T) {
	f := newInstalledSourcefulControlFixture(t)
	manager := f.manager(t, []config.DriverRepositorySource{f.repo}, "1.7.0")
	policy, err := manager.RuntimePolicy(f.driver)
	if err != nil {
		t.Fatal(err)
	}
	if policy == nil || !policy.IsControlV2() {
		t.Fatalf("runtime policy = %+v", policy)
	}
	if policy.SiteEnabled {
		t.Error("control v2 policy enabled site control without cfg.Control")
	}
	if policy.DefaultMode != "driver_default_mode_v2" || !policy.Permissions["modbus.write"] {
		t.Fatalf("control v2 default policy = %+v", policy)
	}

	registry := drivers.NewRegistry(telemetry.NewStore())
	registry.RuntimePolicyResolver = manager.RuntimePolicy
	modbus := &installedPolicyModbus{}
	registry.ModbusFactory = func(string, *config.ModbusConfig) (drivers.ModbusCap, error) {
		return modbus, nil
	}
	defer registry.ShutdownAll()
	if err := registry.Add(context.Background(), f.driver); err != nil {
		t.Fatalf("add unselected control v2 driver and run startup default: %v", err)
	}
	if err := registry.SendDefault(context.Background(), f.driver.Name); err != nil {
		t.Fatalf("run verified control v2 default: %v", err)
	}
	if modbus.writes != 2 || modbus.value != 0 {
		t.Fatalf("default writes = %d, register = %d", modbus.writes, modbus.value)
	}
}

func TestInstalledSourcefulControlPolicyRejectsTrustChangesBeforeLua(t *testing.T) {
	tests := []struct {
		name       string
		repos      func(*installedPolicyFixture) []config.DriverRepositorySource
		host       string
		mutateDisk func(*testing.T, *installedPolicyFixture)
	}{
		{
			name:  "repository removed",
			repos: func(*installedPolicyFixture) []config.DriverRepositorySource { return nil },
			host:  "1.7.0",
		},
		{
			name: "repository changed to direct manifest",
			repos: func(f *installedPolicyFixture) []config.DriverRepositorySource {
				repo := f.repo
				repo.Format = config.DriverRepositoryFormatFTWManifestV1
				return []config.DriverRepositorySource{repo}
			},
			host: "1.7.0",
		},
		{
			name: "repository alias now points to another source",
			repos: func(f *installedPolicyFixture) []config.DriverRepositorySource {
				repo := f.repo
				repo.ManifestURL = "https://other.invalid/index.json"
				return []config.DriverRepositorySource{repo}
			},
			host: "1.7.0",
		},
		{
			name: "signed package envelope missing",
			repos: func(f *installedPolicyFixture) []config.DriverRepositorySource {
				return []config.DriverRepositorySource{f.repo}
			},
			host: "1.7.0",
			mutateDisk: func(t *testing.T, f *installedPolicyFixture) {
				if err := os.Remove(filepath.Join(filepath.Dir(f.installed.InstalledPath), sourcefulInstalledPackageEnvelope)); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "signed package envelope corrupt",
			repos: func(f *installedPolicyFixture) []config.DriverRepositorySource {
				return []config.DriverRepositorySource{f.repo}
			},
			host: "1.7.0",
			mutateDisk: func(t *testing.T, f *installedPolicyFixture) {
				path := filepath.Join(filepath.Dir(f.installed.InstalledPath), sourcefulInstalledPackageEnvelope)
				if err := os.WriteFile(path, []byte(`{"broken":`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "host version incompatible",
			repos: func(f *installedPolicyFixture) []config.DriverRepositorySource {
				return []config.DriverRepositorySource{f.repo}
			},
			host: "1.6.9",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInstalledSourcefulControlFixture(t)
			if tt.mutateDisk != nil {
				tt.mutateDisk(t, f)
			}
			manager := f.manager(t, tt.repos(f), tt.host)
			f.invalidateLua(t)
			assertRegistryRejectsBeforeLua(t, manager, f.driver)
		})
	}
}

func TestHistoricalInstalledSourcefulFormatRequiresMetadata(t *testing.T) {
	f := newInstalledSourcefulControlFixture(t)
	f.setHistoricalUnknownFormat(t)
	if got := f.installRow(t).RepositoryFormat; got != "" {
		t.Fatalf("historical repository format = %q", got)
	}
	if err := os.Remove(filepath.Join(filepath.Dir(f.installed.InstalledPath), sourcefulInstalledPackageEnvelope)); err != nil {
		t.Fatal(err)
	}
	manager := f.manager(t, []config.DriverRepositorySource{f.repo}, "1.7.0")
	f.invalidateLua(t)
	assertRegistryRejectsBeforeLua(t, manager, f.driver)
	if got := f.installRow(t).RepositoryFormat; got != "" {
		t.Fatalf("unverified historical repository format was backfilled as %q", got)
	}
}

func TestHistoricalInstalledSourcefulFormatDoesNotInferLegacyAfterSourceRemoval(t *testing.T) {
	for _, keepEnvelope := range []bool{true, false} {
		t.Run(map[bool]string{true: "envelope retained", false: "envelope missing"}[keepEnvelope], func(t *testing.T) {
			f := newInstalledSourcefulControlFixture(t)
			f.setHistoricalUnknownFormat(t)
			if !keepEnvelope {
				if err := os.Remove(filepath.Join(filepath.Dir(f.installed.InstalledPath), sourcefulInstalledPackageEnvelope)); err != nil {
					t.Fatal(err)
				}
			}
			manager := f.manager(t, nil, "1.7.0")
			f.invalidateLua(t)
			assertRegistryRejectsBeforeLua(t, manager, f.driver)
			if got := f.installRow(t).RepositoryFormat; got != "" {
				t.Fatalf("unverified historical install became %q", got)
			}
		})
	}
}

func TestHistoricalInstalledSourcefulFormatBackfillSurvivesRestart(t *testing.T) {
	f := newInstalledSourcefulControlFixture(t)
	f.setHistoricalUnknownFormat(t)
	manager := f.manager(t, []config.DriverRepositorySource{f.repo}, "1.7.0")

	registry := drivers.NewRegistry(telemetry.NewStore())
	registry.RuntimePolicyResolver = manager.RuntimePolicy
	registry.ModbusFactory = func(string, *config.ModbusConfig) (drivers.ModbusCap, error) {
		return &installedPolicyModbus{}, nil
	}
	if err := registry.Add(context.Background(), f.driver); err != nil {
		registry.ShutdownAll()
		t.Fatalf("verify and backfill historical Sourceful install: %v", err)
	}
	registry.ShutdownAll()
	if got := f.installRow(t).RepositoryFormat; got != config.DriverRepositoryFormatSourcefulIndexV1 {
		t.Fatalf("backfilled repository format = %q", got)
	}

	f.closeStore(t)
	f.reopenStore(t)
	if got := f.installRow(t).RepositoryFormat; got != config.DriverRepositoryFormatSourcefulIndexV1 {
		t.Fatalf("repository format after restart = %q", got)
	}
	if err := os.Remove(filepath.Join(filepath.Dir(f.installed.InstalledPath), sourcefulInstalledPackageEnvelope)); err != nil {
		t.Fatal(err)
	}
	restarted := f.manager(t, []config.DriverRepositorySource{f.repo}, "1.7.0")
	f.invalidateLua(t)
	assertRegistryRejectsBeforeLua(t, restarted, f.driver)
	if got := f.installRow(t).RepositoryFormat; got != config.DriverRepositoryFormatSourcefulIndexV1 {
		t.Fatalf("repository format after missing-envelope rejection = %q", got)
	}
}

func TestInactiveInstalledSourcefulControlPolicyStopsBeforeLua(t *testing.T) {
	f := newInstalledSourcefulControlFixture(t)
	manager := f.manager(t, []config.DriverRepositorySource{f.repo}, "1.7.0")
	if err := f.store.DeactivateDriverRepoInstall(f.installed.LogicalPath); err != nil {
		t.Fatal(err)
	}
	driver := f.driver
	driver.Lua = f.installed.InstalledPath
	f.invalidateLua(t)
	assertRegistryRejectsBeforeLua(t, manager, driver)
}

func TestInstallerCannotReclassifyRetainedSourcefulArtifact(t *testing.T) {
	f := newInstalledSourcefulControlFixture(t)
	manager := f.manager(t, []config.DriverRepositorySource{f.repo}, "1.7.0")
	repo, manifest, entry, err := manager.find(f.repo.ID, "sdm630", "1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	repo.Format = config.DriverRepositoryFormatFTWManifestV1
	envelopePath := filepath.Join(filepath.Dir(f.installed.InstalledPath), sourcefulInstalledPackageEnvelope)
	sentinel := []byte("the retained envelope must not be rewritten")
	if err := os.WriteFile(envelopePath, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.installResolved(context.Background(), repo, manifest, entry); err == nil ||
		!strings.Contains(err.Error(), "metadata format cannot change") {
		t.Fatalf("reclassify retained Sourceful artifact error = %v", err)
	}
	got, err := os.ReadFile(envelopePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(sentinel) {
		t.Fatalf("retained package envelope changed to %q", got)
	}
}
