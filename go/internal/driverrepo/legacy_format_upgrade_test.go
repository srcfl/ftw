package driverrepo

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
	_ "modernc.org/sqlite"
)

type legacyDirectFixture struct {
	dir       string
	dbPath    string
	repo      config.DriverRepositorySource
	installed state.DriverRepoInstall
	signed    *signedFixture
}

func installLegacyDirectFixture(t *testing.T) legacyDirectFixture {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signed := &signedFixture{private: private}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest.json":
			_, _ = w.Write(signed.envelope(t))
		case "/demo.lua":
			signed.mu.Lock()
			defer signed.mu.Unlock()
			_, _ = w.Write(signed.driver)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	signed.setVersion(server.URL, "1.0.0")
	signed.mu.Lock()
	signed.manifest.Drivers[0].ReadOnly = true
	signed.manifest.Drivers[0].Metadata.ReadOnly = true
	signed.manifest.Drivers[0].Permissions = []string{"http.get"}
	signed.mu.Unlock()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	store, err := state.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	repo := config.DriverRepositorySource{
		ID: "legacy-direct", ManifestURL: server.URL + "/manifest.json", Enabled: true, AllowInsecure: true,
		TrustedKeys: map[string]string{"test": base64.StdEncoding.EncodeToString(public)},
	}
	manager := New(&config.DeviceRepository{Enabled: true, Repositories: []config.DriverRepositorySource{repo}}, dir, store)
	if err := manager.Refresh(context.Background(), repo.ID); err != nil {
		t.Fatal(err)
	}
	installed, err := manager.Install(context.Background(), repo.ID, "demo", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if installed.RepositoryFormat != config.DriverRepositoryFormatFTWManifestV1 {
		t.Fatalf("new direct install format = %q", installed.RepositoryFormat)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return legacyDirectFixture{dir: dir, dbPath: dbPath, repo: repo, installed: installed, signed: signed}
}

func (f legacyDirectFixture) legacyDriver() config.Driver {
	return config.Driver{Name: "demo", Lua: filepath.Join(f.dir, "driver-repository", "active", "demo.lua")}
}

func (f legacyDirectFixture) clearFormat(t *testing.T, mutate func(*sql.DB)) {
	t.Helper()
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(db)
	}
	if _, err := db.Exec(`UPDATE driver_repo_installs SET repository_format = '' WHERE installed_path = ?`, f.installed.InstalledPath); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func (f legacyDirectFixture) reopen(t *testing.T) (*state.Store, *Manager) {
	t.Helper()
	store, err := state.Open(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager := New(&config.DeviceRepository{Enabled: true, Repositories: []config.DriverRepositorySource{f.repo}}, f.dir, store)
	if _, err := manager.manifestFor(f.repo); err != nil {
		t.Fatal(err)
	}
	return store, manager
}

func assertUnrecordedFormat(t *testing.T, store *state.Store, path string) {
	t.Helper()
	installed, err := store.DriverRepoInstallByPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if installed.RepositoryFormat != "" {
		t.Fatalf("unverified legacy row recorded format %q", installed.RepositoryFormat)
	}
}

func TestLegacyDirectManifestFormatBackfillSurvivesRepositoryRemoval(t *testing.T) {
	f := installLegacyDirectFixture(t)
	f.clearFormat(t, nil)
	store, manager := f.reopen(t)

	policy, err := manager.RuntimePolicy(f.legacyDriver())
	if err != nil || policy == nil || !policy.IsReadOnly() {
		t.Fatalf("verified legacy direct policy = %+v, %v", policy, err)
	}
	installed, err := store.DriverRepoInstallByPath(f.installed.InstalledPath)
	if err != nil || installed.RepositoryFormat != config.DriverRepositoryFormatFTWManifestV1 {
		t.Fatalf("backfilled install = %+v, %v", installed, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	withoutSource, err := state.Open(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer withoutSource.Close()
	policy, err = New(nil, f.dir, withoutSource).RuntimePolicy(f.legacyDriver())
	if err != nil || policy != nil {
		t.Fatalf("recorded legacy driver after source removal = %+v, %v", policy, err)
	}
}

func TestLegacyDirectManifestFormatDoesNotTrustUnsignedCache(t *testing.T) {
	f := installLegacyDirectFixture(t)
	// New explicit installs may still use this setting. It must not turn an
	// unknown historical row into a trusted direct-manifest install.
	f.repo.AllowUnsigned = true
	f.clearFormat(t, nil)
	store, manager := f.reopen(t)
	raw := f.signed.envelope(t)
	var envelope ManifestEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manager.root, "cache", f.repo.ID+".json"), envelope.Payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if policy, err := manager.RuntimePolicy(f.legacyDriver()); err == nil || policy != nil || !strings.Contains(err.Error(), "verify older installed driver format") {
		t.Fatalf("unsigned cached manifest backfilled legacy row: %+v, %v", policy, err)
	}
	assertUnrecordedFormat(t, store, f.installed.InstalledPath)
}

func TestLegacyDirectManifestFormatDoesNotInferUnknownRows(t *testing.T) {
	for _, tc := range []struct {
		name   string
		want   string
		record func(t *testing.T, f legacyDirectFixture)
		cache  func(t *testing.T, f legacyDirectFixture, manager *Manager)
	}{
		{
			name: "different_driver",
			want: "absent from its verified manifest",
			record: func(t *testing.T, f legacyDirectFixture) {
				f.clearFormat(t, func(db *sql.DB) {
					if _, err := db.Exec(`UPDATE driver_repo_installs SET driver_id = 'other' WHERE installed_path = ?`, f.installed.InstalledPath); err != nil {
						t.Fatal(err)
					}
				})
			},
		},
		{
			name: "different_source",
			want: "source does not match",
			record: func(t *testing.T, f legacyDirectFixture) {
				f.clearFormat(t, func(db *sql.DB) {
					if _, err := db.Exec(`UPDATE driver_repo_installs SET repo_url = 'https://other.invalid/manifest.json' WHERE installed_path = ?`, f.installed.InstalledPath); err != nil {
						t.Fatal(err)
					}
				})
			},
		},
		{
			name:   "cache_missing",
			want:   "verify older installed driver format",
			record: func(t *testing.T, f legacyDirectFixture) { f.clearFormat(t, nil) },
			cache: func(t *testing.T, f legacyDirectFixture, manager *Manager) {
				if err := os.Remove(filepath.Join(manager.root, "cache", f.repo.ID+".json")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "malformed_cache",
			want:   "verify older installed driver format",
			record: func(t *testing.T, f legacyDirectFixture) { f.clearFormat(t, nil) },
			cache: func(t *testing.T, f legacyDirectFixture, manager *Manager) {
				if err := os.WriteFile(filepath.Join(manager.root, "cache", f.repo.ID+".json"), []byte("not a manifest"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "other_signer",
			want:   "verify older installed driver format",
			record: func(t *testing.T, f legacyDirectFixture) { f.clearFormat(t, nil) },
			cache: func(t *testing.T, f legacyDirectFixture, manager *Manager) {
				raw, err := os.ReadFile(filepath.Join(manager.root, "cache", f.repo.ID+".json"))
				if err != nil {
					t.Fatal(err)
				}
				var envelope ManifestEnvelope
				if err := json.Unmarshal(raw, &envelope); err != nil {
					t.Fatal(err)
				}
				_, other, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				envelope.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(other, envelope.Payload))
				raw, err = json.Marshal(envelope)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(manager.root, "cache", f.repo.ID+".json"), raw, 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "sourceful_package_entry",
			want:   "not a legacy direct-manifest artifact",
			record: func(t *testing.T, f legacyDirectFixture) { f.clearFormat(t, nil) },
			cache: func(t *testing.T, f legacyDirectFixture, manager *Manager) {
				f.signed.mu.Lock()
				f.signed.manifest.Drivers[0].PackageID = "com.sourceful.driver.demo"
				f.signed.mu.Unlock()
				if err := os.WriteFile(filepath.Join(manager.root, "cache", f.repo.ID+".json"), f.signed.envelope(t), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "v2_abi_entry",
			want:   "not a legacy direct-manifest artifact",
			record: func(t *testing.T, f legacyDirectFixture) { f.clearFormat(t, nil) },
			cache: func(t *testing.T, f legacyDirectFixture, manager *Manager) {
				f.signed.mu.Lock()
				f.signed.manifest.Drivers[0].RuntimeABI = sourcefulFTWABIV2
				f.signed.manifest.Drivers[0].HostAPIProfile = sourcefulFTWHostAPIProfileV2
				f.signed.mu.Unlock()
				if err := os.WriteFile(filepath.Join(manager.root, "cache", f.repo.ID+".json"), f.signed.envelope(t), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "memory_cache_cannot_override_bad_disk_cache",
			want:   "verify older installed driver format",
			record: func(t *testing.T, f legacyDirectFixture) { f.clearFormat(t, nil) },
			cache: func(t *testing.T, f legacyDirectFixture, manager *Manager) {
				if _, ok := manager.manifests[f.repo.ID]; !ok {
					t.Fatal("test did not retain a positive in-memory manifest")
				}
				if err := os.WriteFile(filepath.Join(manager.root, "cache", f.repo.ID+".json"), []byte("bad saved bytes"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := installLegacyDirectFixture(t)
			tc.record(t, f)
			store, manager := f.reopen(t)
			if tc.cache != nil {
				tc.cache(t, f, manager)
			}
			if policy, err := manager.RuntimePolicy(f.legacyDriver()); err == nil || policy != nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unknown legacy row policy = %+v, %v; want %q", policy, err, tc.want)
			}
			assertUnrecordedFormat(t, store, f.installed.InstalledPath)
		})
	}
}
