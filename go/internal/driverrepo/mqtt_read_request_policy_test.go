package driverrepo

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/state"
)

// The read-request topics a read-only MQTT driver may publish to come from the
// signed manifest, as its sign-in paths do. Nothing outside the signed payload
// can add or widen one.
func TestSignedManifestCarriesMQTTReadRequestTopics(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &signedFixture{private: private}
	var tamper atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest.json":
			raw := fixture.envelope(t)
			if tamper.Load() {
				// Widen the topics after signing, leaving the signature.
				var envelope ManifestEnvelope
				if err := json.Unmarshal(raw, &envelope); err != nil {
					t.Error(err)
				}
				envelope.Payload = json.RawMessage(strings.Replace(string(envelope.Payload),
					`"R/+/keepalive"`, `"W/+/+"`, 1))
				raw, _ = json.Marshal(envelope)
			}
			_, _ = w.Write(raw)
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
	// The channel grants no write permission for read requests.
	fixture.manifest.Drivers[0].Permissions = []string{"mqtt.subscribe"}
	fixture.manifest.Drivers[0].Metadata.ReadOnly = true
	fixture.manifest.Drivers[0].Metadata.MQTTReadRequestTopics = []string{"R/+/keepalive"}
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
	if _, err := manager.Install(context.Background(), "ftw-official", "demo", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	driverCfg := config.Driver{Name: "demo", Lua: filepath.Join(manager.ActiveDir(), "demo.lua")}
	checkPolicy := func(m *Manager) {
		t.Helper()
		policy, err := m.RuntimePolicy(driverCfg)
		if err != nil || policy == nil || !policy.IsReadOnly() || policy.Permissions["mqtt.publish"] ||
			!policy.Permissions["mqtt.subscribe"] ||
			len(policy.MQTTReadRequestTopics) != 1 || policy.MQTTReadRequestTopics[0] != "R/+/keepalive" {
			t.Fatalf("signed read-request policy = %+v, %v", policy, err)
		}
		// The Lua constructor validates the policy; the grant must pass it.
		luaDriver, err := drivers.NewLuaDriverWithPolicy(driverCfg.Lua, drivers.NewHostEnv("demo", nil), policy)
		if err != nil {
			t.Fatal(err)
		}
		luaDriver.Cleanup()
	}
	checkPolicy(manager)

	// A manifest altered after signing is refused, and the verified policy
	// stays as it was.
	tamper.Store(true)
	if err := manager.Refresh(context.Background(), "ftw-official"); err == nil {
		t.Fatal("a manifest whose topics changed after signing was accepted")
	}
	checkPolicy(manager)
	tamper.Store(false)

	// A signed entry with a topic Core refuses yields a policy the Lua
	// constructor rejects, so the driver does not start.
	manager.mu.Lock()
	manifest := manager.manifests["ftw-official"]
	manifest.Drivers[0].Metadata.MQTTReadRequestTopics = []string{"R/#"}
	manager.manifests["ftw-official"] = manifest
	manager.mu.Unlock()
	policy, err := manager.RuntimePolicy(driverCfg)
	if err != nil || policy == nil {
		t.Fatalf("policy = %+v, %v", policy, err)
	}
	if _, err := drivers.NewLuaDriverWithPolicy(driverCfg.Lua, drivers.NewHostEnv("demo", nil), policy); err == nil {
		t.Fatal("a policy with a multi-level wildcard read request was accepted")
	}
}
