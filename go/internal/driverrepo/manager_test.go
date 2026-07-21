package driverrepo

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/components"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
)

func testDriver(version string) []byte {
	return []byte(`DRIVER = {
  id = "demo",
  name = "Demo",
  version = "` + version + `",
  host_api_min = 1,
  host_api_max = 1,
  protocols = { "http" },
  capabilities = { "meter" },
}
function driver_init(config) end
function driver_poll() return 1000 end
function driver_default_mode() end
`)
}

type signedFixture struct {
	mu       sync.Mutex
	private  ed25519.PrivateKey
	manifest Manifest
	driver   []byte
}

func (f *signedFixture) envelope(t *testing.T) []byte {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	payload, err := json.Marshal(f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(ManifestEnvelope{
		SchemaVersion: 1, KeyID: "test", Payload: payload,
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(f.private, payload)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (f *signedFixture) setVersion(serverURL, version string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.driver = testDriver(version)
	sum := sha256.Sum256(f.driver)
	f.manifest = Manifest{SchemaVersion: 1, Repository: serverURL, Drivers: []ManifestDriver{{
		ID: "demo", Path: "drivers/demo.lua", Filename: "demo.lua", Version: version,
		SHA256: hex.EncodeToString(sum[:]), URL: serverURL + "/demo.lua",
		HostAPI: components.CompatibleRange{Min: 1, Max: 1},
	}}}
}

func TestSignedInstallUpdateAndRollback(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &signedFixture{private: private}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest.json":
			_, _ = w.Write(fixture.envelope(t))
		case "/demo.lua":
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			_, _ = w.Write(fixture.driver)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	fixture.setVersion(server.URL, "1.0.0")

	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := &config.DeviceRepository{Enabled: true, Repositories: []config.DriverRepositorySource{{
		ID: "test", ManifestURL: server.URL + "/manifest.json", Enabled: true,
		AllowInsecure: true, TrustedKeys: map[string]string{"test": base64.StdEncoding.EncodeToString(public)},
	}}}
	manager := New(cfg, dir, store)
	if err := manager.Refresh(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	first, err := manager.Install(context.Background(), "test", "demo", "")
	if err != nil {
		t.Fatal(err)
	}
	activePath := filepath.Join(manager.ActiveDir(), "demo.lua")
	if got, err := os.ReadFile(activePath); err != nil || !strings.Contains(string(got), `version = "1.0.0"`) {
		t.Fatalf("active v1: %q, %v", got, err)
	}

	fixture.setVersion(server.URL, "1.1.0")
	if err := manager.Refresh(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	second, err := manager.Install(context.Background(), "test", "demo", "1.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if second.PreviousInstalledPath != first.InstalledPath {
		t.Fatalf("previous = %q, want %q", second.PreviousInstalledPath, first.InstalledPath)
	}
	rolledBack, err := manager.Rollback("drivers/demo.lua")
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.Version != "1.0.0" {
		t.Fatalf("rollback version = %s", rolledBack.Version)
	}
	if got, _ := os.ReadFile(activePath); !strings.Contains(string(got), `version = "1.0.0"`) {
		t.Fatalf("active after rollback: %q", got)
	}
	versions, err := manager.InstalledVersions("demo")
	if err != nil || len(versions) != 2 {
		t.Fatalf("installed versions = %+v err=%v", versions, err)
	}
	reactivated, err := manager.ActivateInstalled("demo", "1.1.0", second.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if reactivated.Version != "1.1.0" {
		t.Fatalf("reactivated version = %s", reactivated.Version)
	}
	if got, _ := os.ReadFile(activePath); !strings.Contains(string(got), `version = "1.1.0"`) {
		t.Fatalf("active after exact activation: %q", got)
	}

	// A fresh manager can serve the signed last-good cache without network.
	server.Close()
	reloaded := New(cfg, dir, store)
	if catalog, err := reloaded.Catalog(); err != nil || len(catalog) != 1 {
		t.Fatalf("offline catalog = %+v, %v", catalog, err)
	}
	if err := os.Remove(filepath.Join(dir, "driver-repository", "cache", "test.json")); err != nil {
		t.Fatal(err)
	}
	withoutManifest := New(cfg, dir, store)
	available, err := withoutManifest.AvailableVersions("demo")
	if err != nil || len(available) != 2 || available[0].Installed == nil || available[1].Installed == nil {
		t.Fatalf("retained offline versions = %+v, %v", available, err)
	}
}

func TestDirectManifestBindsReadOnlyRuntimePolicy(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &signedFixture{private: private}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest.json":
			_, _ = w.Write(fixture.envelope(t))
		case "/demo.lua":
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			_, _ = w.Write(fixture.driver)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	fixture.setVersion(server.URL, "1.0.0")
	fixture.mu.Lock()
	fixture.manifest.Repository = "https://github.com/srcfl/device-drivers"
	fixture.manifest.Drivers[0].ReadOnly = true
	fixture.manifest.Drivers[0].Permissions = []string{"http.get"}
	fixture.manifest.Drivers[0].Metadata.ReadOnly = true
	fixture.mu.Unlock()

	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := &config.DeviceRepository{Enabled: true, Repositories: []config.DriverRepositorySource{{
		ID: "ftw-official", ManifestURL: server.URL + "/manifest.json", Enabled: true,
		AllowInsecure: true, TrustedKeys: map[string]string{"test": base64.StdEncoding.EncodeToString(public)},
	}}}
	manager := New(cfg, dir, store)
	if err := manager.Refresh(context.Background(), "ftw-official"); err != nil {
		t.Fatal(err)
	}
	installed, err := manager.Install(context.Background(), "ftw-official", "demo", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := manager.RuntimePolicy(config.Driver{
		Name: "demo", Lua: filepath.Join(manager.ActiveDir(), "demo.lua"),
	})
	if err != nil || policy == nil || !policy.IsReadOnly() || !policy.Permissions["http.get"] {
		t.Fatalf("direct runtime policy = %+v, %v", policy, err)
	}
	if policy.ArtifactSHA256 != installed.SHA256 || policy.Version != installed.Version ||
		policy.PackageID != "com.sourceful.driver.demo" {
		t.Fatalf("direct runtime identity = %+v", policy)
	}

	manager.mu.Lock()
	manifest := manager.manifests["ftw-official"]
	manifest.Drivers[0].ReadOnly = false
	manager.manifests["ftw-official"] = manifest
	manager.mu.Unlock()
	if _, err := manager.RuntimePolicy(config.Driver{
		Name: "demo", Lua: filepath.Join(manager.ActiveDir(), "demo.lua"),
	}); err == nil || !strings.Contains(err.Error(), "lacks signed read-only policy") {
		t.Fatalf("unsigned write policy error = %v", err)
	}
}

func TestManifestRejectsTamperTraversalAndHashMismatch(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	repo := config.DriverRepositorySource{TrustedKeys: map[string]string{"test": base64.StdEncoding.EncodeToString(public)}}
	payload, _ := json.Marshal(Manifest{SchemaVersion: 1, Drivers: []ManifestDriver{{
		ID: "bad", Path: "drivers/../bad.lua", Filename: "bad.lua", Version: "1.0.0",
		SHA256: strings.Repeat("0", 64), URL: "https://example.test/bad.lua",
	}}})
	envelope, _ := json.Marshal(ManifestEnvelope{SchemaVersion: 1, KeyID: "test", Payload: payload, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(private, payload))})
	manifest, _, err := verifyManifest(envelope, repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateManifest(manifest, false); err == nil {
		t.Fatal("path traversal accepted")
	}

	var decoded ManifestEnvelope
	_ = json.Unmarshal(envelope, &decoded)
	decoded.Payload[0] ^= 1
	tampered, _ := json.Marshal(decoded)
	if _, _, err := verifyManifest(tampered, repo); err == nil {
		t.Fatal("tampered signed payload accepted")
	}

	installedPath := filepath.Join(t.TempDir(), "bad.lua")
	if err := os.WriteFile(installedPath, []byte("not the expected artifact"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := validateInstalledFile(state.DriverRepoInstall{
		InstalledPath: installedPath,
		SHA256:        strings.Repeat("0", 64),
	}); err == nil {
		t.Fatal("installed artifact hash mismatch accepted")
	}
}

func TestStartupReconcilesCrashWindowAndRejectsModifiedArtifact(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	root := filepath.Join(dir, "driver-repository")
	v1Path := filepath.Join(root, "installed", "test", "demo", "1.0.0", "v1", "demo.lua")
	v2Path := filepath.Join(root, "installed", "test", "demo", "1.1.0", "v2", "demo.lua")
	for path, raw := range map[string][]byte{v1Path: testDriver("1.0.0"), v2Path: testDriver("1.1.0")} {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	install := func(version, path string) state.DriverRepoInstall {
		raw, _ := os.ReadFile(path)
		sum := sha256.Sum256(raw)
		return state.DriverRepoInstall{
			RepoID: "test", DriverID: "demo", LogicalPath: "drivers/demo.lua",
			Version: version, SHA256: hex.EncodeToString(sum[:]), InstalledPath: path,
		}
	}
	v1, v2 := install("1.0.0", v1Path), install("1.1.0", v2Path)
	if _, err := store.ActivateDriverRepoInstall(v1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ActivateDriverRepoInstall(v2); err != nil {
		t.Fatal(err)
	}

	// Simulate a crash after the DB commit but before the symlink swap: the
	// resolver still points to v1 while SQLite says v2.
	activePath := filepath.Join(root, "active", "demo.lua")
	if err := os.MkdirAll(filepath.Dir(activePath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(v1Path, activePath); err != nil {
		t.Fatal(err)
	}
	_ = New(nil, dir, store)
	active, err := store.ActiveDriverRepoInstall("drivers/demo.lua")
	if err != nil || active.InstalledPath != v1Path {
		t.Fatalf("reconciled activation = %+v, %v", active, err)
	}

	if err := os.WriteFile(v1Path, []byte("modified"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = New(nil, dir, store)
	if _, err := store.ActiveDriverRepoInstall("drivers/demo.lua"); err == nil {
		t.Fatal("modified managed artifact remained active")
	}
	if _, err := os.Lstat(activePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("active symlink still exists: %v", err)
	}
}
