package driverrepo

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
)

// supersedeSite is one data directory with a signed channel offering demo at
// channelVersion and a bundled demo.lua at bundledVersion ("" = not bundled).
type supersedeSite struct {
	dir, bundled string
	store        *state.Store
	cfg          *config.DeviceRepository
}

func newSupersedeSite(t *testing.T, channelVersion, bundledVersion string) *supersedeSite {
	t.Helper()
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
	t.Cleanup(server.Close)
	fixture.setVersion(server.URL, channelVersion)

	site := &supersedeSite{dir: t.TempDir()}
	site.bundled = filepath.Join(site.dir, "bundled")
	if err := os.MkdirAll(site.bundled, 0o755); err != nil {
		t.Fatal(err)
	}
	if bundledVersion != "" {
		if err := os.WriteFile(filepath.Join(site.bundled, "demo.lua"), testDriver(bundledVersion), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	site.store, err = state.Open(filepath.Join(site.dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { site.store.Close() })
	site.cfg = &config.DeviceRepository{Enabled: true, Repositories: []config.DriverRepositorySource{{
		ID: "test", ManifestURL: server.URL + "/manifest.json", Enabled: true,
		AllowInsecure: true, TrustedKeys: map[string]string{"test": base64.StdEncoding.EncodeToString(public)},
	}}}
	return site
}

// boot starts Core at release and runs the startup reconciliation.
func (s *supersedeSite) boot(release string) (*Manager, []Superseded) {
	manager := NewWithHostVersion(s.cfg, s.dir, s.store, release)
	return manager, manager.RetireSupersededByBundled(s.bundled)
}

func (s *supersedeSite) install(t *testing.T, manager *Manager) {
	t.Helper()
	if err := manager.Refresh(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Install(context.Background(), "test", "demo", ""); err != nil {
		t.Fatal(err)
	}
}

func managedActive(t *testing.T, manager *Manager) bool {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(manager.ActiveDir(), "demo.lua")); err == nil {
		return true
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return false
}

// The owner's box on 2026-09-25: easee_cloud 1.3.2 from the beta channel kept
// running over the bundled 1.3.3 in v0.136.4-beta.1 until someone clicked.
// A release that bundles the same or a newer driver ends the override.
func TestNewReleaseRetiresManagedInstallItCaughtUpWith(t *testing.T) {
	site := newSupersedeSite(t, "1.3.3", "1.3.3")
	manager, _ := site.boot("v0.136.3-beta.1")
	site.install(t, manager)

	manager, retired := site.boot("v0.136.4-beta.1")
	if len(retired) != 1 || retired[0].Version != "1.3.3" || retired[0].BundledVersion != "1.3.3" {
		t.Fatalf("retired = %+v, want demo 1.3.3 superseded by bundled 1.3.3", retired)
	}
	if managedActive(t, manager) {
		t.Fatal("managed symlink still shadows the bundled driver")
	}
	if active, err := site.store.ActiveDriverRepoInstalls(); err != nil || len(active) != 0 {
		t.Fatalf("active installs = %+v, %v; want none", active, err)
	}
}

// A box that already carries a managed install when this reconciliation
// first ships has no recorded release; its first boot counts as a change.
func TestFirstBootWithoutRecordedReleaseReconciles(t *testing.T) {
	site := newSupersedeSite(t, "1.3.2", "1.3.3")
	site.install(t, New(site.cfg, site.dir, site.store))

	manager, retired := site.boot("v0.136.5-beta.1")
	if len(retired) != 1 || managedActive(t, manager) {
		t.Fatalf("retired = %+v, active = %v; want the older managed install retired", retired, managedActive(t, manager))
	}
}

// Within one release an expert's choice stands, even an older version picked
// to avoid a bad bundled driver. The next release ends it.
func TestOverrideStaysWithinTheReleaseItWasMadeOn(t *testing.T) {
	site := newSupersedeSite(t, "1.0.0", "1.1.0")
	manager, _ := site.boot("v0.136.4-beta.1")
	site.install(t, manager)

	manager, retired := site.boot("v0.136.4-beta.1")
	if len(retired) != 0 || !managedActive(t, manager) {
		t.Fatalf("restart on the same release retired %+v; the downgrade must stay", retired)
	}
	manager, retired = site.boot("v0.136.5-beta.1")
	if len(retired) != 1 || managedActive(t, manager) {
		t.Fatalf("next release retired %+v; want the override ended", retired)
	}
}

func TestNewerManagedInstallAndUnbundledDriverStay(t *testing.T) {
	for _, tc := range []struct{ name, bundled string }{
		{"managed is newer than bundled", "1.0.0"},
		{"driver is not bundled", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			site := newSupersedeSite(t, "1.1.0", tc.bundled)
			manager, _ := site.boot("v0.136.4-beta.1")
			site.install(t, manager)

			manager, retired := site.boot("v0.136.5-beta.1")
			if len(retired) != 0 || !managedActive(t, manager) {
				t.Fatalf("retired = %+v; the managed install must keep running", retired)
			}
		})
	}
}
