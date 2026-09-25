package driverrepo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A site that activated a Device Support package before FTW stopped reading
// them must not lose the driver: the next start deactivates the package and
// the release's own copy runs.
func TestDeviceSupportPackageIsDeactivatedAtStartupAndTheBundledDriverRuns(t *testing.T) {
	for _, tc := range []struct {
		name     string
		format   string
		envelope bool
	}{
		{name: "recorded format", format: retiredPackageFormat},
		{name: "older row with its package envelope", envelope: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bundled := filepath.Join(dir, "bundled")
			if err := os.MkdirAll(bundled, 0o755); err != nil {
				t.Fatal(err)
			}
			release := append(testDriver("1.0.0"), []byte("\nhost.emit_metric(\"release_copy\", 1)\n")...)
			if err := os.WriteFile(filepath.Join(bundled, "demo.lua"), release, 0o644); err != nil {
				t.Fatal(err)
			}
			store, err := state.Open(filepath.Join(dir, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			repository := &config.DeviceRepository{Enabled: true}
			before := NewWithHostVersion(repository, dir, store, "v1.0.0")

			// What an older Core left behind: a newer package artifact, active.
			pkg := testDriver("2.0.0")
			sum := sha256.Sum256(pkg)
			hash := hex.EncodeToString(sum[:])
			installPath := filepath.Join(before.root, "installed", "device-support", "demo", "2.0.0", hash, "demo.lua")
			if err := atomicWrite(installPath, pkg, 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.envelope {
				if err := atomicWrite(filepath.Join(filepath.Dir(installPath), retiredPackageEnvelope), []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.ActivateDriverRepoInstall(state.DriverRepoInstall{
				RepoURL: "https://packages.example/index.json", RepoID: "device-support", DriverID: "demo",
				LogicalPath: "drivers/demo.lua", Version: "2.0.0", SHA256: hash, InstalledPath: installPath,
				RepositoryFormat: tc.format,
			}); err != nil {
				t.Fatal(err)
			}
			commit, cleanup, err := before.prepareSymlink("drivers/demo.lua", installPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := commit(); err != nil {
				t.Fatal(err)
			}
			cleanup()
			if _, err := before.RuntimePolicy(config.Driver{Name: "demo", Lua: filepath.Join(before.ActiveDir(), "demo.lua")}); err == nil ||
				!strings.Contains(err.Error(), "Device Support") {
				t.Fatalf("an active package must not start as is: %v", err)
			}

			// The next start.
			manager := NewWithHostVersion(repository, dir, store, "v1.0.0")
			manager.SetBundledDir(bundled)
			manager.ApplyBundled()
			if active, err := store.ActiveDriverRepoInstalls(); err != nil || len(active) != 0 {
				t.Fatalf("active installs = %+v, %v; want the package deactivated", active, err)
			}
			if exists(t, filepath.Join(manager.ActiveDir(), "demo.lua")) || exists(t, filepath.Join(manager.EffectiveDir(), "demo.lua")) {
				t.Fatal("the package still resolves")
			}

			origBundled, origManaged, origUser := config.DriversDirOverride, config.ManagedDriversDirOverride, config.UserDriversDirOverride
			config.DriversDirOverride, config.ManagedDriversDirOverride, config.UserDriversDirOverride = bundled, manager.EffectiveDir(), ""
			t.Cleanup(func() {
				config.DriversDirOverride, config.ManagedDriversDirOverride, config.UserDriversDirOverride = origBundled, origManaged, origUser
			})
			site := config.Config{Drivers: []config.Driver{{Name: "demo", Lua: "drivers/demo.lua"}}}
			site.ResolveDriverPaths(dir)
			if site.Drivers[0].Lua != filepath.Join(bundled, "demo.lua") {
				t.Fatalf("driver resolved to %q; want the release's copy", site.Drivers[0].Lua)
			}
			tel := telemetry.NewStore()
			registry := drivers.NewRegistry(tel)
			registry.RuntimePolicyResolver = manager.RuntimePolicy
			defer registry.ShutdownAll()
			if err := registry.Add(context.Background(), site.Drivers[0]); err != nil {
				t.Fatalf("the release's driver did not start: %v", err)
			}
			if value, _, ok := tel.LatestMetric("demo", "release_copy"); !ok || value != 1 {
				t.Fatal("the release's copy is not the one running")
			}

			// Retained on disk, but never offered or activated again.
			if versions, err := manager.InstalledVersions("demo"); err != nil || len(versions) != 0 {
				t.Fatalf("retained versions = %+v, %v; want the package hidden", versions, err)
			}
			if _, err := manager.ActivateInstalled("demo", "2.0.0", ""); err == nil {
				t.Fatal("a Device Support package was activated again")
			}
		})
	}
}
