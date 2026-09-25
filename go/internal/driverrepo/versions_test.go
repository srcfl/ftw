package driverrepo

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
)

func signedServer(t *testing.T, version string) (*signedFixture, string, string) {
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
	fixture.setVersion(server.URL, version)
	return fixture, server.URL, base64.StdEncoding.EncodeToString(public)
}

// The version picker is the one place an owner sees and changes a driver
// version, so it lists the beta channel beside stable, labelled, and one
// refresh checks both.
func TestVersionListShowsStableAndBetaFromOneRefresh(t *testing.T) {
	_, stableURL, stableKey := signedServer(t, "1.0.0")
	beta, betaURL, betaKey := signedServer(t, "1.1.0-beta.1")

	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager := New(&config.DeviceRepository{Enabled: true, Repositories: []config.DriverRepositorySource{{
		ID: "stable", ManifestURL: stableURL + "/manifest.json", Enabled: true, AllowInsecure: true,
		TrustedKeys: map[string]string{"test": stableKey},
	}}}, dir, store)
	manager.betaRepo = config.DriverRepositorySource{
		ID: config.DefaultDriverRepositoryBetaID, ManifestURL: betaURL + "/manifest.json", Enabled: true,
		AllowInsecure: true, TrustedKeys: map[string]string{"test": betaKey},
	}
	if err := manager.Refresh(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	versions, err := manager.AvailableVersions("demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 || versions[0].Driver.Version != "1.1.0-beta.1" || versions[0].Channel != "beta" ||
		versions[1].Driver.Version != "1.0.0" || versions[1].Channel != "stable" {
		t.Fatalf("versions = %+v; want beta 1.1.0-beta.1 then stable 1.0.0", versions)
	}

	// Promotion publishes the same file on both channels; it is one version.
	beta.setVersion(betaURL, "1.0.0")
	if err := manager.Refresh(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if versions, _ = manager.AvailableVersions("demo"); len(versions) != 1 || versions[0].Channel != "stable" {
		t.Fatalf("versions = %+v; want the promoted file listed once, as stable", versions)
	}

	// A beta version that stable has already passed is not offered.
	beta.setVersion(betaURL, "0.9.0")
	if err := manager.Refresh(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if versions, _ = manager.AvailableVersions("demo"); len(versions) != 1 || versions[0].Driver.Version != "1.0.0" {
		t.Fatalf("versions = %+v; want only stable 1.0.0", versions)
	}
}
