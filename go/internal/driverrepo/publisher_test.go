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
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
)

func TestSignedMonorepoPublicationRoundTrip(t *testing.T) {
	dir := t.TempDir()
	driversDir := filepath.Join(dir, "drivers")
	outputDir := filepath.Join(dir, "publication")
	if err := os.MkdirAll(driversDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(driversDir, "demo.lua"), testDriver("1.2.3"), 0o644); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewTLSServer(http.FileServer(http.Dir(outputDir)))
	defer server.Close()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publication, err := BuildPublication(PublicationOptions{
		DriversDir: driversDir, OutputDir: outputDir, BaseURL: server.URL,
		Repository: "https://github.com/srcfl/ftw", Commit: "abc123", KeyID: "test-2026-01",
		PrivateKey: private, ExpectedPublicKey: public, GeneratedAt: time.Unix(1_700_000_000, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(publication.Manifest.Drivers) != 1 || len(publication.ArtifactPaths) != 1 {
		t.Fatalf("publication = %+v", publication)
	}
	if !strings.Contains(filepath.Base(publication.ArtifactPaths[0]), "demo-v1.2.3-") {
		t.Fatalf("artifact name = %s", publication.ArtifactPaths[0])
	}

	rawManifest, err := os.ReadFile(publication.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	encodedPublic := base64.StdEncoding.EncodeToString(public)
	manifest, err := VerifyPublication(rawManifest, "test-2026-01", encodedPublic)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyPublicationArtifacts(manifest, outputDir); err != nil {
		t.Fatal(err)
	}

	store, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager := New(&config.DeviceRepository{Enabled: true, Repositories: []config.DriverRepositorySource{{
		ID: "official", Enabled: true, ManifestURL: server.URL + "/manifest.json",
		TrustedKeys: map[string]string{"test-2026-01": encodedPublic},
	}}}, filepath.Join(dir, "runtime"), store)
	manager.client = server.Client()
	if err := manager.Refresh(context.Background(), "official"); err != nil {
		t.Fatal(err)
	}
	installed, err := manager.Install(context.Background(), "official", "demo", "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(installed.InstalledPath); err != nil || !strings.Contains(string(raw), `version = "1.2.3"`) {
		t.Fatalf("installed artifact = %q, %v", raw, err)
	}

	if err := os.WriteFile(publication.ArtifactPaths[0], []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyPublicationArtifacts(manifest, outputDir); err == nil {
		t.Fatal("tampered publication artifact verified")
	}
}

func TestPublicDeviceDriversPythonContract(t *testing.T) {
	manifestPath := os.Getenv("FTW_DEVICE_DRIVERS_MANIFEST")
	artifactsDir := os.Getenv("FTW_DEVICE_DRIVERS_ARTIFACT_DIR")
	publicKey := os.Getenv("FTW_DEVICE_DRIVERS_PUBLIC_KEY")
	keyID := os.Getenv("FTW_DEVICE_DRIVERS_KEY_ID")
	if manifestPath == "" || artifactsDir == "" || publicKey == "" || keyID == "" {
		t.Skip("public device-driver cross-language fixture not configured")
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := VerifyPublication(raw, keyID, publicKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Drivers) != 61 {
		t.Fatalf("public driver count = %d, want 61", len(manifest.Drivers))
	}
	for _, driver := range manifest.Drivers {
		if !driver.ReadOnly || !driver.Metadata.ReadOnly || driver.ControlEnabled ||
			driver.SourceCommit != manifest.Commit {
			t.Fatalf("unsafe public driver entry = %+v", driver)
		}
	}
	if err := VerifyPublicationArtifacts(manifest, artifactsDir); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationRejectsWrongExpectedPublicKey(t *testing.T) {
	dir := t.TempDir()
	driversDir := filepath.Join(dir, "drivers")
	if err := os.MkdirAll(driversDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(driversDir, "demo.lua"), testDriver("1.0.0"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	wrongPublic, _, _ := ed25519.GenerateKey(rand.Reader)
	_, err := BuildPublication(PublicationOptions{
		DriversDir: driversDir, OutputDir: filepath.Join(dir, "publication"),
		BaseURL: "https://example.test/drivers-beta", Repository: "https://github.com/srcfl/ftw",
		KeyID: "test", PrivateKey: private, ExpectedPublicKey: wrongPublic,
	})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("wrong public key error = %v", err)
	}
}

func TestPublicationCarriesVerifiedVersionHistoryForward(t *testing.T) {
	dir := t.TempDir()
	driversDir := filepath.Join(dir, "drivers")
	if err := os.MkdirAll(driversDir, 0o750); err != nil {
		t.Fatal(err)
	}
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	build := func(version, output, previous string) Publication {
		t.Helper()
		if err := os.WriteFile(filepath.Join(driversDir, "demo.lua"), testDriver(version), 0o644); err != nil {
			t.Fatal(err)
		}
		publication, err := BuildPublication(PublicationOptions{
			DriversDir: driversDir, OutputDir: output,
			BaseURL:    "https://example.test/releases/download/drivers-beta",
			Repository: "https://github.com/srcfl/ftw", KeyID: "test",
			PrivateKey: private, ExpectedPublicKey: public, PreviousManifestPath: previous,
		})
		if err != nil {
			t.Fatal(err)
		}
		return publication
	}
	first := build("1.0.0", filepath.Join(dir, "first"), "")
	second := build("1.1.0", filepath.Join(dir, "second"), first.ManifestPath)
	if len(second.Manifest.History) != 1 || second.Manifest.History[0].Version != "1.0.0" {
		t.Fatalf("history = %+v", second.Manifest.History)
	}
}

func TestPublicationRejectsDangerousOutputDirectories(t *testing.T) {
	for _, path := range []string{"", ".", "..", string(filepath.Separator), filepath.Join(string(filepath.Separator), "tmp")} {
		if err := validatePublicationOutput(path); err == nil {
			t.Errorf("dangerous output directory %q accepted", path)
		}
	}
	for _, path := range []string{"dist", filepath.Join("..", "dist", "driver-repository"), filepath.Join(t.TempDir(), "publication")} {
		if err := validatePublicationOutput(path); err != nil {
			t.Errorf("safe output directory %q rejected: %v", path, err)
		}
	}
}
