package driverrepo

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
)

type betaPolicyFixture struct {
	manager    *Manager
	installed  state.DriverRepoInstall
	driver     config.Driver
	newManager func([]string) *Manager
}

func installBetaPolicyFixture(t *testing.T, repositoryIDs []string) betaPolicyFixture {
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
			_, _ = w.Write(fixture.driver)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	fixture.setVersion(server.URL, "1.2.2")
	fixture.manifest.Repository = "https://github.com/srcfl/device-drivers"
	entry := &fixture.manifest.Drivers[0]
	entry.ReadOnly, entry.Metadata.ReadOnly = true, true
	entry.Permissions = []string{"http.get", "http.post"}
	entry.Metadata.AuthPostPath = "/oauth/token"
	entry.Metadata.ConfigSecrets = []string{"refresh_token"}
	dir := t.TempDir()
	st, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	newManager := func(ids []string) *Manager {
		cfg := &config.DeviceRepository{}
		for _, id := range ids {
			cfg.Repositories = append(cfg.Repositories, config.DriverRepositorySource{
				ID: id, ManifestURL: "https://unrelated.invalid/manifest.json", AllowUnsigned: true,
			})
		}
		m := New(cfg, dir, st)
		// Keep the production alias calculation, replacing only the fixture
		// transport and signing key. No production private key is available.
		m.betaRepo.ManifestURL = server.URL + "/manifest.json"
		m.betaRepo.AllowInsecure = true
		m.betaRepo.TrustedKeys = map[string]string{"test": base64.StdEncoding.EncodeToString(public)}
		return m
	}
	m := newManager(repositoryIDs)
	installed, err := m.InstallChannel(context.Background(), "beta", "demo", "1.2.2")
	if err != nil {
		t.Fatal(err)
	}
	if installed.FTWSigned {
		t.Fatal("fixture key was incorrectly recorded as FTW's production key")
	}
	// Model the durable provenance that the production installer records for
	// its pinned key. Signature verification still runs with the fixture key.
	installed.FTWSigned = true
	installed, err = st.ActivateDriverRepoInstall(installed)
	if err != nil {
		t.Fatal(err)
	}
	return betaPolicyFixture{m, installed, config.Driver{Name: "demo", Lua: filepath.Join(m.ActiveDir(), "demo.lua")}, newManager}
}

func TestOfficialBetaPolicySurvivesRepositoryAliasChanges(t *testing.T) {
	beta := config.DefaultDriverRepositoryBetaID
	for _, tc := range []struct {
		name          string
		before, after []string
	}{
		{"collision_removed", []string{beta}, nil},
		{"collision_added", nil, []string{beta}},
		{"multiple_collisions_removed", []string{beta, beta + "-channel"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := installBetaPolicyFixture(t, tc.before)
			reloaded := f.newManager(tc.after)
			if reloaded.betaRepo.ID == f.installed.RepoID {
				t.Fatal("test did not change the generated beta alias")
			}
			// The current repository owning this alias may already have loaded
			// different, even unsigned metadata into the shared memory cache.
			unrelated := f.manager.manifests[f.installed.RepoID]
			unrelated.Drivers = append([]ManifestDriver(nil), unrelated.Drivers...)
			unrelated.Drivers[0].ReadOnly = false
			unrelated.Drivers[0].Metadata.ReadOnly = false
			unrelated.Drivers[0].ControlEnabled = true
			reloaded.manifests[f.installed.RepoID] = unrelated
			policy, err := reloaded.RuntimePolicy(f.driver)
			if err != nil || policy == nil || !policy.IsReadOnly() || policy.ArtifactSHA256 != f.installed.SHA256 ||
				policy.AuthPostPath != "/oauth/token" || len(policy.ConfigSecrets) != 1 || policy.ConfigSecrets[0] != "refresh_token" {
				t.Fatalf("beta policy after alias change = %+v, %v", policy, err)
			}
		})
	}
}

func TestOfficialBetaPolicyRejectsReplacedOrMissingManifest(t *testing.T) {
	for _, mode := range []string{"missing", "other_signer", "changed_payload"} {
		t.Run(mode, func(t *testing.T) {
			f := installBetaPolicyFixture(t, nil)
			cache := filepath.Join(f.manager.root, "cache", f.installed.RepoID+".json")
			if mode == "missing" {
				if err := os.Remove(cache); err != nil {
					t.Fatal(err)
				}
			} else {
				raw, err := os.ReadFile(cache)
				if err != nil {
					t.Fatal(err)
				}
				var envelope ManifestEnvelope
				if err := json.Unmarshal(raw, &envelope); err != nil {
					t.Fatal(err)
				}
				if mode == "other_signer" {
					_, other, err := ed25519.GenerateKey(rand.Reader)
					if err != nil {
						t.Fatal(err)
					}
					envelope.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(other, envelope.Payload))
				} else {
					envelope.Payload = json.RawMessage(`{"schema_version":1}`)
				}
				raw, err = json.Marshal(envelope)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(cache, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			// Even an earlier valid in-memory manifest cannot bypass a fresh
			// check against the recorded official trust identity.
			if policy, err := f.manager.RuntimePolicy(f.driver); err == nil || policy != nil {
				t.Fatalf("%s manifest yielded runtime policy %+v, %v", mode, policy, err)
			}
		})
	}
}
