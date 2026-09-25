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

// boot starts Core at release with the site's bundled drivers.
func (s *supersedeSite) boot(release string) (*Manager, []Superseded) {
	manager := NewWithHostVersion(s.cfg, s.dir, s.store, release)
	manager.SetBundledDir(s.bundled)
	return manager, manager.ApplyBundled()
}

// bundle replaces the release's own copy, as a Core update or rollback does.
func (s *supersedeSite) bundle(t *testing.T, version string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.bundled, "demo.lua"), testDriver(version), 0o644); err != nil {
		t.Fatal(err)
	}
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

// runsManaged reports whether paths resolve to the managed selection.
func runsManaged(t *testing.T, manager *Manager) bool {
	t.Helper()
	return exists(t, filepath.Join(manager.EffectiveDir(), "demo.lua"))
}

// selected reports whether the owner's selection is still recorded.
func selected(t *testing.T, manager *Manager) bool {
	t.Helper()
	return exists(t, filepath.Join(manager.ActiveDir(), "demo.lua"))
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	if _, err := os.Lstat(path); err == nil {
		return true
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return false
}

// On the home box easee_cloud 1.3.2 from the beta channel kept running over
// the bundled 1.3.3 in v0.136.4-beta.1. A release with a newer driver runs
// it, and the owner's selection is kept: a rollback, or a trial that falls
// back, runs it again under the older release.
func TestNewerReleaseDriverRunsWithoutLosingTheSelection(t *testing.T) {
	site := newSupersedeSite(t, "1.3.2", "1.3.1")
	manager, _ := site.boot("v0.136.3-beta.1")
	site.install(t, manager)
	if !runsManaged(t, manager) {
		t.Fatal("the early 1.3.2 must run over the release's 1.3.1")
	}

	site.bundle(t, "1.3.3")
	manager, superseded := site.boot("v0.136.4-beta.1")
	if len(superseded) != 1 || superseded[0].BundledVersion != "1.3.3" || runsManaged(t, manager) {
		t.Fatalf("superseded = %+v; the release's 1.3.3 must run", superseded)
	}
	if !selected(t, manager) {
		t.Fatal("the owner's selection was lost")
	}

	site.bundle(t, "1.3.1") // ftw rollback, or a failed trial
	manager, superseded = site.boot("v0.136.3-beta.1")
	if len(superseded) != 0 || !runsManaged(t, manager) {
		t.Fatalf("superseded = %+v; the older release must run the selection again", superseded)
	}
}

// A release that bundles the same version leaves the selection running: the
// same version is the same driver.
func TestSameVersionKeepsTheSelectionRunning(t *testing.T) {
	site := newSupersedeSite(t, "1.3.3", "1.3.1")
	manager, _ := site.boot("v0.136.3-beta.1")
	site.install(t, manager)
	site.bundle(t, "1.3.3")
	manager, superseded := site.boot("v0.136.4-beta.1")
	if len(superseded) != 0 || !runsManaged(t, manager) {
		t.Fatalf("superseded = %+v", superseded)
	}
}

// An owner who goes back behind the release, say to avoid a bad bundled
// driver, keeps that choice across releases until they change it.
func TestChosenOlderVersionStaysUntilTheOwnerChangesIt(t *testing.T) {
	site := newSupersedeSite(t, "1.0.0", "1.1.0")
	manager, _ := site.boot("v0.136.4-beta.1")
	site.install(t, manager)

	for _, release := range []string{"v0.136.4-beta.1", "v0.136.5-beta.1", "v0.137.0-beta.1"} {
		site.bundle(t, "1.1.0")
		manager, superseded := site.boot(release)
		if len(superseded) != 0 || !runsManaged(t, manager) {
			t.Fatalf("%s superseded %+v; the owner's older choice must stay", release, superseded)
		}
	}

	// Choosing the release's version again is not a choice to keep.
	site.installVersion(t, manager, "1.1.0")
	site.installVersion(t, manager, "1.0.9")
	manager, _ = site.boot("v0.137.1-beta.1")
	if !runsManaged(t, manager) {
		t.Fatal("1.0.9 went below the running 1.1.0; that is a choice")
	}
}

// Going back below the version that was running is a choice even when it is
// still newer than the release's copy: the owner rejected what ran.
func TestGoingBackBelowTheRunningVersionIsAChoice(t *testing.T) {
	site := newSupersedeSite(t, "1.3.3", "1.3.0")
	manager, _ := site.boot("v0.136.4-beta.1")
	site.install(t, manager)                 // early 1.3.3
	site.installVersion(t, manager, "1.3.2") // it misbehaved; back to 1.3.2

	site.bundle(t, "1.3.3") // the next release ships the rejected version
	manager, superseded := site.boot("v0.136.5-beta.1")
	if len(superseded) != 0 || !runsManaged(t, manager) {
		t.Fatalf("superseded = %+v; the owner's 1.3.2 must keep running", superseded)
	}
}

// A rollback undoes an activation and the choice that went with it, so the
// recovery of a failed install does not leave a mark of its own.
func TestRollbackRestoresTheChoiceItUndid(t *testing.T) {
	site := newSupersedeSite(t, "1.3.2", "1.3.3")
	manager, _ := site.boot("v0.136.4-beta.1")
	site.install(t, manager)                 // chosen 1.3.2 under the release's 1.3.3
	site.installVersion(t, manager, "1.3.4") // early 1.3.4 clears the choice
	if _, err := manager.Rollback("drivers/demo.lua"); err != nil {
		t.Fatal(err)
	}
	if pinned, _ := site.store.LoadConfig(pinKey("drivers/demo.lua")); pinned != "1.3.2" {
		t.Fatalf("pin after rollback = %q; want the undone choice 1.3.2 back", pinned)
	}
	manager, superseded := site.boot("v0.136.5-beta.1")
	if len(superseded) != 0 || !runsManaged(t, manager) {
		t.Fatalf("superseded = %+v", superseded)
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
	if runsManaged(t, manager) || selected(t, manager) {
		t.Fatal("the managed driver still runs after going back to the release's copy")
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

			manager, superseded := site.boot("v0.136.5-beta.1")
			if len(superseded) != 0 || !runsManaged(t, manager) {
				t.Fatalf("superseded = %+v; the managed install must keep running", superseded)
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
