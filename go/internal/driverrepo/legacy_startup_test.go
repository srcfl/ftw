package driverrepo

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
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

func TestManagedDriverStartupAfterRepositoryRemoval(t *testing.T) {
	for _, scenario := range []string{"legacy_v1", "control_v2_opt_in", "official_invalid_signature"} {
		t.Run(scenario, func(t *testing.T) {
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
					_, _ = w.Write(fixture.driver)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			fixture.setVersion(server.URL, "1.0.0")
			fixture.driver = append(fixture.driver, []byte(`
host.emit_metric("lua_loaded", 1)
local initialized = false
function driver_init(config)
  initialized = true
  host.emit_metric("initialized", 1)
  host.set_poll_interval(60000)
end
function driver_command() return true end
function driver_default_mode()
  if not initialized then error("default before init") end
  host.emit_metric("autonomous_default", 1)
end
`)...)
			sum := sha256.Sum256(fixture.driver)
			fixture.manifest.Drivers[0].SHA256 = hex.EncodeToString(sum[:])
			fixture.manifest.Drivers[0].ControlEnabled = true
			if scenario == "official_invalid_signature" {
				fixture.manifest.Repository = "https://github.com/srcfl/device-drivers"
			}
			dir := t.TempDir()
			st, err := state.Open(filepath.Join(dir, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			repo := config.DriverRepositorySource{
				ID: "custom-driver-source", ManifestURL: server.URL + "/manifest.json", AllowInsecure: true, Enabled: true,
				TrustedKeys: map[string]string{"test": base64.StdEncoding.EncodeToString(public)},
			}
			original := New(&config.DeviceRepository{Enabled: true, Repositories: []config.DriverRepositorySource{repo}}, dir, st)
			if err := original.Refresh(context.Background(), repo.ID); err != nil {
				t.Fatal(err)
			}
			installed, err := original.Install(context.Background(), repo.ID, "demo", "1.0.0")
			if err != nil {
				t.Fatal(err)
			}
			if installed.FTWSigned {
				t.Fatal("test signer unexpectedly counted as official")
			}
			if scenario == "official_invalid_signature" {
				// Model official provenance using a fixture signing key; then
				// corrupt only the saved signature, leaving Lua and hash intact.
				installed.FTWSigned = true
				if _, err := st.ActivateDriverRepoInstall(installed); err != nil {
					t.Fatal(err)
				}
				var envelope ManifestEnvelope
				if err := json.Unmarshal(fixture.envelope(t), &envelope); err != nil {
					t.Fatal(err)
				}
				envelope.Signature = base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
				raw, err := json.Marshal(envelope)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(original.root, "cache", repo.ID+".json"), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			// The next process has no configured source for the retained artifact.
			reloaded := New(nil, dir, st)
			reloaded.betaRepo.TrustedKeys = repo.TrustedKeys
			reloaded.betaRepo.AllowInsecure = true
			tel := telemetry.NewStore()
			registry := drivers.NewRegistry(tel)
			registry.RuntimePolicyResolver = reloaded.RuntimePolicy
			defer registry.ShutdownAll()
			cfg := config.Driver{Name: "demo", Lua: filepath.Join(reloaded.ActiveDir(), "demo.lua")}
			if scenario == "control_v2_opt_in" {
				cfg.Control = &config.DriverControlOptIn{Enabled: true, PackageID: "com.sourceful.driver.demo", Version: installed.Version, ArtifactSHA256: installed.SHA256}
			}
			err = registry.Add(context.Background(), cfg)
			if scenario == "legacy_v1" {
				if err != nil {
					t.Fatalf("legacy startup lost its autonomous default: %v", err)
				}
				for _, metric := range []string{"lua_loaded", "initialized", "autonomous_default"} {
					if value, _, ok := tel.LatestMetric(cfg.Name, metric); !ok || value != 1 {
						t.Errorf("%s = %v, present=%v", metric, value, ok)
					}
				}
			} else {
				if err == nil {
					t.Fatal("unverified managed control driver started")
				}
				if scenario == "official_invalid_signature" && !strings.Contains(err.Error(), "manifest signature verification failed") {
					t.Fatalf("wrong rejection: %v", err)
				}
				for _, metric := range []string{"lua_loaded", "initialized", "autonomous_default"} {
					if _, _, ok := tel.LatestMetric(cfg.Name, metric); ok {
						t.Errorf("rejected driver executed %s", metric)
					}
				}
			}
		})
	}
}
