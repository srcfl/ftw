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
	"strings"
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
	channel      *signedFixture
	channelURL   string
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

	site := &supersedeSite{dir: t.TempDir(), channel: fixture, channelURL: server.URL}
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
	manager.SetBundledDir(s.bundled)
	return manager, manager.RetireSupersededByBundled()
}

func (s *supersedeSite) install(t *testing.T, manager *Manager) {
	t.Helper()
	s.installVersion(t, manager, "")
}

// installVersion publishes version on the channel ("" keeps the current one)
// and installs it, as an owner picking it under Settings does.
func (s *supersedeSite) installVersion(t *testing.T, manager *Manager, version string) {
	t.Helper()
	if version != "" {
		s.channel.setVersion(s.channelURL, version)
	}
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

// An owner who goes back behind the release, say to avoid a bad bundled
// driver, keeps that choice across releases until they change it.
func TestChosenOlderVersionStaysUntilTheOwnerChangesIt(t *testing.T) {
	site := newSupersedeSite(t, "1.0.0", "1.1.0")
	manager, _ := site.boot("v0.136.4-beta.1")
	site.install(t, manager)

	for _, release := range []string{"v0.136.4-beta.1", "v0.136.5-beta.1", "v0.137.0-beta.1"} {
		manager, retired := site.boot(release)
		if len(retired) != 0 || !managedActive(t, manager) {
			t.Fatalf("%s retired %+v; the owner's older choice must stay", release, retired)
		}
	}

	// Choosing the release's own version again is early access, not a pin.
	site.installVersion(t, manager, "1.1.0")
	manager, retired := site.boot("v0.137.1-beta.1")
	if len(retired) != 1 || managedActive(t, manager) {
		t.Fatalf("retired = %+v; the release caught up with the owner's choice", retired)
	}
}

// Going back to the release's copy forgets the choice.
func TestUsingTheBundledCopyForgetsTheChoice(t *testing.T) {
	site := newSupersedeSite(t, "1.0.0", "1.1.0")
	manager, _ := site.boot("v0.136.4-beta.1")
	site.install(t, manager)
	if err := manager.Deactivate("drivers/demo.lua"); err != nil {
		t.Fatal(err)
	}
	if pinned, _ := site.store.LoadConfig(pinKey("drivers/demo.lua")); pinned != "" {
		t.Fatalf("pin %q survived going back to the bundled copy", pinned)
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

// device_repository.enabled: false turns off every remote fetch, the built-in
// beta channel included.
func TestBetaChannelRespectsTheOffSwitch(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager := New(&config.DeviceRepository{Enabled: false}, dir, store)
	if _, err := manager.ChannelCatalog(context.Background(), "beta"); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("ChannelCatalog with the repository off: %v", err)
	}
	if _, err := manager.InstallChannel(context.Background(), "beta", "easee_cloud", "1.3.3"); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("InstallChannel with the repository off: %v", err)
	}
}
