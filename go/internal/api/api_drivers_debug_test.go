package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// /api/drivers/test handler-level coverage. The probe path runs a real
// short-lived gopher-lua driver so each table case writes the Lua to a
// t.TempDir() and posts an absolute path (ResolveDriverPaths leaves
// absolute Lua untouched).

func TestSafeProbeName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"pixii", "pixii"},
		{"My Home/Battery", "My_Home_Battery"},
		{"  ferro-amp_01  ", "ferro-amp_01"},
		// Disallowed runes collapse to '_' which then gets trimmed,
		// leaving the empty-string fallback.
		{"!@#$", "driver"},
		// Cap at 48 runes.
		{strings.Repeat("a", 80), strings.Repeat("a", 48)},
		// Empty fallback.
		{"", "driver"},
	}
	for _, tc := range cases {
		if got := safeProbeName(tc.in); got != tc.want {
			t.Errorf("safeProbeName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHandleDriverTestRejectsBadInput(t *testing.T) {
	srv := New(&Deps{})
	cases := []struct {
		name     string
		body     string
		wantCode int
		wantErr  string
	}{
		{"invalid json", `not json`, 400, "invalid driver config"},
		{"missing lua", `{"name":"pixii"}`, 400, "missing driver lua path"},
		{"empty lua", `{"name":"pixii","lua":"   "}`, 400, "missing driver lua path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/drivers/test",
				strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)
			if rr.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body=%s)", rr.Code, tc.wantCode, rr.Body.String())
			}
			var resp struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(rr.Body.Bytes(), &resp)
			if !strings.Contains(resp.Error, tc.wantErr) {
				t.Errorf("error = %q, want substring %q", resp.Error, tc.wantErr)
			}
		})
	}
}

// MQTT/Modbus capabilities require their respective factories to be wired
// at startup. Posting a config that requests one without the factory must
// 503 — the alternative is constructing a registry with a nil factory and
// crashing on Add.
func TestHandleDriverTestRequiresMQTTFactory(t *testing.T) {
	srv := New(&Deps{}) // no DriverMQTTFactory
	body := `{"name":"probe","lua":"/tmp/x.lua","capabilities":{"mqtt":{"host":"mqtt.local"}}}`
	req := httptest.NewRequest(http.MethodPost, "/api/drivers/test",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 503 {
		t.Fatalf("status = %d, want 503 (body=%s)", rr.Code, rr.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if !strings.Contains(resp.Error, "mqtt probe unavailable") {
		t.Errorf("error = %q, want 'mqtt probe unavailable'", resp.Error)
	}
}

func TestHandleDriverTestRequiresModbusFactory(t *testing.T) {
	srv := New(&Deps{}) // no DriverModbusFactory
	body := `{"name":"probe","lua":"/tmp/x.lua","capabilities":{"modbus":{"host":"10.0.0.1"}}}`
	req := httptest.NewRequest(http.MethodPost, "/api/drivers/test",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 503 {
		t.Fatalf("status = %d, want 503 (body=%s)", rr.Code, rr.Body.String())
	}
	var resp struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if !strings.Contains(resp.Error, "modbus probe unavailable") {
		t.Errorf("error = %q, want 'modbus probe unavailable'", resp.Error)
	}
}

// Happy path: a tiny Lua driver that immediately emits a meter reading
// must produce ok=true with the reading + identity in the response.
// Exercises the full pipeline — config preserve, path resolve, registry
// lifecycle, telemetry poll loop, identity capture.
func TestHandleDriverTestRunsLuaProbe(t *testing.T) {
	dir := t.TempDir()
	luaPath := filepath.Join(dir, "probe_emit.lua")
	luaSrc := `
function driver_init(config)
    host.set_make("Acme")
    host.set_sn("SN-PROBE-1")
    host.set_poll_interval(50)
end
function driver_poll()
    host.emit("meter", { w = 1234 })
end
function driver_command(action, power_w, cmd) return false end
function driver_default_mode() end
function driver_cleanup() end
`
	if err := os.WriteFile(luaPath, []byte(luaSrc), 0o644); err != nil {
		t.Fatalf("write lua: %v", err)
	}

	srv := New(&Deps{ConfigPath: filepath.Join(dir, "config.yaml")})
	body, _ := json.Marshal(map[string]any{
		"name": "probe-meter",
		"lua":  luaPath, // absolute → ResolveDriverPaths leaves it untouched
	})
	req := httptest.NewRequest(http.MethodPost, "/api/drivers/test",
		strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	var resp driverProbeResp
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rr.Body.String())
	}
	if !resp.OK {
		t.Fatalf("probe.ok = false, error=%q (body=%s)", resp.Error, rr.Body.String())
	}
	if resp.Name != "probe-meter" {
		t.Errorf("display name = %q, want probe-meter", resp.Name)
	}
	if len(resp.Readings) == 0 {
		t.Fatalf("expected at least one reading, got none (body=%s)", rr.Body.String())
	}
	var foundMeter bool
	for _, rd := range resp.Readings {
		if rd.Type == "meter" && rd.RawW > 0 {
			foundMeter = true
		}
	}
	if !foundMeter {
		t.Errorf("expected a meter reading > 0, got %+v", resp.Readings)
	}
	if resp.Identity.Make != "Acme" || resp.Identity.SN != "SN-PROBE-1" {
		t.Errorf("identity = %+v, want make=Acme sn=SN-PROBE-1", resp.Identity)
	}
}

// A Lua path that doesn't exist must return ok=false at HTTP 200 with a
// useful error — the handler converts the registry Add failure into a
// structured probe response, not a 500.
func TestHandleDriverTestMissingLuaReturnsStructuredError(t *testing.T) {
	dir := t.TempDir()
	srv := New(&Deps{ConfigPath: filepath.Join(dir, "config.yaml")})
	body, _ := json.Marshal(map[string]any{
		"name": "probe-missing",
		"lua":  filepath.Join(dir, "does-not-exist.lua"),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/drivers/test",
		strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	var resp driverProbeResp
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.OK {
		t.Errorf("probe.ok = true, want false for missing lua")
	}
	if resp.Error == "" {
		t.Errorf("probe.error empty, want a message naming the missing file")
	}
}

// PreserveMaskedSecrets must restore a masked password from the live
// config so the probe runs against the real broker instead of the mask.
// Asserts: incoming "***MASKED***" is replaced by the existing value
// before the registry attempts to start the driver.
func TestHandleDriverTestRestoresMaskedSecrets(t *testing.T) {
	// Live config: one MQTT driver with the real password.
	live := &config.Config{
		Drivers: []config.Driver{{
			Name: "shellem",
			Lua:  "/tmp/shellem.lua",
			Capabilities: config.Capabilities{
				MQTT: &config.MQTTConfig{
					Host:     "mqtt.local",
					Username: "u",
					Password: "real-secret",
				},
			},
		}},
	}
	srv := New(&Deps{
		Cfg:   live,
		CfgMu: &sync.RWMutex{},
		// Factory absent on purpose — handler will 503 AFTER restoring
		// secrets (validation order in the handler), letting us inspect
		// the side effect without a live broker.
	})

	// Incoming form posts the masked sentinel.
	body, _ := json.Marshal(map[string]any{
		"name": "shellem",
		"lua":  "/tmp/shellem.lua",
		"capabilities": map[string]any{
			"mqtt": map[string]any{
				"host":     "mqtt.local",
				"username": "u",
				"password": "***MASKED***",
			},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/drivers/test",
		strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 503 {
		t.Fatalf("status = %d, want 503 (body=%s)", rr.Code, rr.Body.String())
	}
	// The live config must NOT have been mutated by the probe path.
	if live.Drivers[0].Capabilities.MQTT.Password != "real-secret" {
		t.Errorf("live config password mutated to %q (must remain 'real-secret')",
			live.Drivers[0].Capabilities.MQTT.Password)
	}
}

func TestHandleDriverTestUsesOriginalDriverSecretState(t *testing.T) {
	dir := t.TempDir()
	luaPath := filepath.Join(dir, "oauth_probe.lua")
	luaSrc := `
function driver_init(config)
    host.set_poll_interval(50)
    if config and config.refresh_token == "fresh-token" then
        host.emit_metric("used_fresh_token", 1)
        host.persist_secret("refresh_token", "rotated-token")
    else
        host.emit_metric("used_stale_token", 1)
    end
end
function driver_poll() end
function driver_command() end
function driver_default_mode() end
function driver_cleanup() end
`
	if err := os.WriteFile(luaPath, []byte(luaSrc), 0o644); err != nil {
		t.Fatalf("write lua: %v", err)
	}
	st, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SaveConfig(driverSecretStateKey("myuplink", "refresh_token"), "fresh-token"); err != nil {
		t.Fatalf("save secret override: %v", err)
	}

	live := &config.Config{Drivers: []config.Driver{{
		Name: "myuplink",
		Lua:  luaPath,
		Config: map[string]any{
			"refresh_token": "stale-token",
		},
	}}}
	srv := New(&Deps{
		Cfg:        live,
		CfgMu:      &sync.RWMutex{},
		ConfigPath: filepath.Join(dir, "config.yaml"),
		State:      st,
	})
	body, _ := json.Marshal(map[string]any{
		"name": "myuplink",
		"lua":  luaPath,
		"config": map[string]any{
			"refresh_token": "stale-token",
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/drivers/test", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	var resp driverProbeResp
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rr.Body.String())
	}
	if !resp.OK {
		t.Fatalf("probe.ok = false, error=%q (body=%s)", resp.Error, rr.Body.String())
	}
	if resp.Health == nil || !strings.HasPrefix(resp.Health.Name, "__test_myuplink_") {
		t.Fatalf("probe health name = %+v, want temporary myuplink probe", resp.Health)
	}
	var usedFresh, usedStale bool
	for _, m := range resp.Metrics {
		switch m.Name {
		case "used_fresh_token":
			usedFresh = m.Value == 1
		case "used_stale_token":
			usedStale = true
		}
	}
	if !usedFresh || usedStale {
		t.Fatalf("metrics = %+v, want fresh token metric only", resp.Metrics)
	}
	if got, ok := st.LoadConfig(driverSecretStateKey("myuplink", "refresh_token")); !ok || got != "rotated-token" {
		t.Fatalf("original driver secret = %q ok=%v, want rotated-token", got, ok)
	}
	if _, ok := st.LoadConfig(driverSecretStateKey(resp.Health.Name, "refresh_token")); ok {
		t.Fatalf("probe wrote secret under temporary name %q", resp.Health.Name)
	}
}

func writeOAuthProbeLua(t *testing.T, dir, name string) string {
	t.Helper()
	luaPath := filepath.Join(dir, name)
	luaSrc := `
function driver_init(config)
    host.set_poll_interval(50)
    local token = ""
    if config and config.refresh_token then
        token = config.refresh_token
    end
    host.emit_metric("token_" .. token, 1)
    host.persist_secret("refresh_token", "rotated-" .. token)
end
function driver_poll() end
function driver_command() end
function driver_default_mode() end
function driver_cleanup() end
`
	if err := os.WriteFile(luaPath, []byte(luaSrc), 0o644); err != nil {
		t.Fatalf("write lua: %v", err)
	}
	return luaPath
}

func metricToken(resp driverProbeResp) string {
	const prefix = "token_"
	for _, m := range resp.Metrics {
		if strings.HasPrefix(m.Name, prefix) && m.Value == 1 {
			return strings.TrimPrefix(m.Name, prefix)
		}
	}
	return ""
}

func TestHandleDriverTestDoesNotShareSecretsWithDifferentLua(t *testing.T) {
	dir := t.TempDir()
	liveLua := writeOAuthProbeLua(t, dir, "myuplink.lua")
	otherLua := writeOAuthProbeLua(t, dir, "other.lua")
	st, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SaveConfig(driverSecretStateKey("myuplink", "refresh_token"), "fresh-token"); err != nil {
		t.Fatalf("save secret override: %v", err)
	}

	live := &config.Config{Drivers: []config.Driver{{
		Name:   "myuplink",
		Lua:    liveLua,
		Config: map[string]any{"refresh_token": "stale-token"},
	}}}
	srv := New(&Deps{
		Cfg:        live,
		CfgMu:      &sync.RWMutex{},
		ConfigPath: filepath.Join(dir, "config.yaml"),
		State:      st,
	})
	body, _ := json.Marshal(map[string]any{
		"name":   "myuplink",
		"lua":    otherLua,
		"config": map[string]any{"refresh_token": "stale-token"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/drivers/test", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	var resp driverProbeResp
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rr.Body.String())
	}
	if !resp.OK {
		t.Fatalf("probe.ok = false, error=%q (body=%s)", resp.Error, rr.Body.String())
	}
	if got := metricToken(resp); got != "stale-token" {
		t.Fatalf("used token = %q, want posted stale-token, not the live driver's override", got)
	}
	if got, ok := st.LoadConfig(driverSecretStateKey("myuplink", "refresh_token")); !ok || got != "fresh-token" {
		t.Fatalf("live secret = %q ok=%v, want unchanged fresh-token", got, ok)
	}
}

// Settings posts the whole driver, credential_owner included; scripts may
// omit it. The probe must behave the same either way.
var probeCredentialOwnerCases = []struct {
	name  string
	owner string
}{
	{"without credential_owner", ""},
	{"with credential_owner", "credential-myuplink"},
}

func TestHandleDriverTestKeepsExplicitReauthToken(t *testing.T) {
	for _, tc := range probeCredentialOwnerCases {
		t.Run(tc.name, func(t *testing.T) { testHandleDriverTestKeepsExplicitReauthToken(t, tc.owner) })
	}
}

func testHandleDriverTestKeepsExplicitReauthToken(t *testing.T, owner string) {
	dir := t.TempDir()
	luaPath := writeOAuthProbeLua(t, dir, "oauth_probe.lua")
	st, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	secretOwner := "myuplink"
	if owner != "" {
		secretOwner = owner
	}
	if err := st.SaveConfig(driverSecretStateKey(secretOwner, "refresh_token"), "fresh-token"); err != nil {
		t.Fatalf("save secret override: %v", err)
	}

	live := &config.Config{Drivers: []config.Driver{{
		Name:            "myuplink",
		CredentialOwner: owner,
		Lua:             luaPath,
		Config:          map[string]any{"refresh_token": "stale-token"},
	}}}
	srv := New(&Deps{
		Cfg:        live,
		CfgMu:      &sync.RWMutex{},
		ConfigPath: filepath.Join(dir, "config.yaml"),
		State:      st,
	})
	payload := map[string]any{
		"name":   "myuplink",
		"lua":    luaPath,
		"config": map[string]any{"refresh_token": "new-account-token"},
	}
	if owner != "" {
		payload["credential_owner"] = owner
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/drivers/test", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	var resp driverProbeResp
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rr.Body.String())
	}
	if !resp.OK {
		t.Fatalf("probe.ok = false, error=%q (body=%s)", resp.Error, rr.Body.String())
	}
	if got := metricToken(resp); got != "new-account-token" {
		t.Fatalf("used token = %q, want explicit new-account-token", got)
	}
	if got, ok := st.LoadConfig(driverSecretStateKey(secretOwner, "refresh_token")); !ok || got != "fresh-token" {
		t.Fatalf("live secret = %q ok=%v, want unchanged fresh-token", got, ok)
	}
}

func TestConfiguredProbeLoopbackHostRequiresSameEnabledDriverAndURL(t *testing.T) {
	driver := config.Driver{
		Name: "audi-vag",
		Lua:  "/var/lib/ftw/drivers/vw_merged.lua",
		Config: map[string]any{
			"url": "http://127.0.0.1:8787",
		},
	}
	live := &config.Config{Drivers: []config.Driver{driver}}
	srv := New(&Deps{Cfg: live})

	if got := srv.configuredProbeLoopbackHost(driver); got != "127.0.0.1" {
		t.Fatalf("configured loopback host = %q, want 127.0.0.1", got)
	}

	changed := driver
	changed.Lua = "/var/lib/ftw/drivers/other.lua"
	if got := srv.configuredProbeLoopbackHost(changed); got != "" {
		t.Errorf("different Lua file was trusted: %q", got)
	}
	changed = driver
	changed.Config = map[string]any{"url": "http://127.0.0.1:8080"}
	if got := srv.configuredProbeLoopbackHost(changed); got != "" {
		t.Errorf("changed URL was trusted: %q", got)
	}
	changed = driver
	changed.Capabilities.HTTP = &config.HTTPCapability{AllowedHosts: []string{"127.0.0.1"}}
	if got := srv.configuredProbeLoopbackHost(changed); got != "" {
		t.Errorf("changed HTTP allowlist was trusted: %q", got)
	}
	live.Drivers[0].Disabled = true
	if got := srv.configuredProbeLoopbackHost(driver); got != "" {
		t.Errorf("disabled configured driver was trusted: %q", got)
	}
}

func TestRejectUnsafeProbeTargetsAllowsOnlyConfiguredLoopbackException(t *testing.T) {
	cfg := config.Driver{
		Config: map[string]any{"url": "http://127.0.0.1:8787"},
		Capabilities: config.Capabilities{
			HTTP: &config.HTTPCapability{AllowedHosts: []string{"127.0.0.1:8787"}},
		},
	}
	if err := rejectUnsafeProbeTargets(cfg, ""); err == nil {
		t.Fatal("unconfigured loopback URL was accepted")
	}
	if err := rejectUnsafeProbeTargets(cfg, "127.0.0.1"); err != nil {
		t.Fatalf("configured loopback URL was rejected: %v", err)
	}

	cfg.Config["url"] = "http://127.0.0.2:8787"
	if err := rejectUnsafeProbeTargets(cfg, "127.0.0.1"); err == nil {
		t.Fatal("different loopback URL was accepted")
	}
	cfg.Config["url"] = "http://169.254.169.254:8787"
	if err := rejectUnsafeProbeTargets(cfg, "127.0.0.1"); err == nil {
		t.Fatal("link-local URL was accepted by the loopback exception")
	}
	cfg.Config["url"] = "http://127.0.0.1:8787"
	cfg.MQTT = &config.MQTTConfig{Host: "127.0.0.1"}
	if err := rejectUnsafeProbeTargets(cfg, "127.0.0.1"); err == nil {
		t.Fatal("loopback MQTT target was accepted by the HTTP URL exception")
	}
}

func TestIsSensitiveKey(t *testing.T) {
	sensitive := []string{
		"password", "Password", "mqtt_password", "passwd", "client_secret",
		"refresh_token", "access_token", "api_key", "apikey", "private_key",
		"authorization", "Authorization", "credential", "credentials",
		"auth", "AUTH", "auth_header", "x-auth",
	}
	for _, k := range sensitive {
		if !isSensitiveKey(k) {
			t.Errorf("isSensitiveKey(%q) = false, want true", k)
		}
	}
	keep := []string{
		"host", "username", "name", "oauth", "oauth_client_id", "client_id",
		"port", "lua",
	}
	for _, k := range keep {
		if isSensitiveKey(k) {
			t.Errorf("isSensitiveKey(%q) = true, want false", k)
		}
	}
}

func TestRedactDumpLog(t *testing.T) {
	in := `HTTP 400: {"refresh_token":"RT-secret-value","access_token":"AT-secret-value"} Bearer eyJabc.def password=hunter2 dial 192.168.1.153:502 poll ok`
	got := redactDumpLog(in)
	for _, leak := range []string{"RT-secret-value", "AT-secret-value", "eyJabc.def", "hunter2", "192.168.1.153"} {
		if strings.Contains(got, leak) {
			t.Errorf("redactDumpLog leaked %q in %q", leak, got)
		}
	}
	if !strings.Contains(got, "HTTP 400") {
		t.Errorf("redactDumpLog dropped diagnostic context: %q", got)
	}
	if !strings.Contains(got, "poll ok") {
		t.Errorf("redactDumpLog dropped benign text: %q", got)
	}
}

func writeProbeRestartLua(t *testing.T, dir string) string {
	t.Helper()
	luaPath := filepath.Join(dir, "probe_restart.lua")
	luaSrc := `
function driver_init(config)
    host.set_poll_interval(50)
    if config and config.rotate_secret then
        host.persist_secret("refresh_token", config.persist_value)
    end
    host.emit_metric("probe_ready", 1)
    host.emit_metric("used_rotated_token", config.refresh_token == "rotated-token" and 1 or 0)
end
function driver_poll() end
function driver_command() end
function driver_default_mode() end
function driver_cleanup() end
`
	if err := os.WriteFile(luaPath, []byte(luaSrc), 0o644); err != nil {
		t.Fatalf("write lua: %v", err)
	}
	return luaPath
}

func TestHandleDriverTestRestartsRunningDriverAfterRefreshTokenRotation(t *testing.T) {
	for _, tc := range probeCredentialOwnerCases {
		t.Run(tc.name, func(t *testing.T) {
			testHandleDriverTestRestartsRunningDriverAfterRefreshTokenRotation(t, tc.owner != "")
		})
	}
}

func testHandleDriverTestRestartsRunningDriverAfterRefreshTokenRotation(t *testing.T, postOwner bool) {
	dir := t.TempDir()
	luaPath := writeProbeRestartLua(t, dir)

	st, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const secretKey = "refresh_token"
	if err := st.SaveConfig(driverSecretStateKey("credential-myuplink", secretKey), "fresh-token"); err != nil {
		t.Fatalf("save secret: %v", err)
	}

	tel := telemetry.NewStore()
	reg := drivers.NewRegistry(tel)
	reg.SecretOverride = func(driverName, key string) (string, bool) {
		return st.LoadConfig(driverSecretStateKey(driverName, key))
	}
	reg.SecretPersister = func(driverName, key, value string) error {
		return st.SaveConfig(driverSecretStateKey(driverName, key), value)
	}
	t.Cleanup(reg.ShutdownAll)

	liveDriver := config.Driver{
		Name:            "myuplink",
		CredentialOwner: "credential-myuplink",
		Lua:             luaPath,
		Config: map[string]any{
			"refresh_token": "stale-token",
		},
	}
	if err := reg.Add(context.Background(), liveDriver); err != nil {
		t.Fatalf("add live driver: %v", err)
	}

	before, ok := reg.ControlStatus("myuplink")
	if !ok {
		t.Fatal("live driver missing before probe")
	}

	live := &config.Config{Drivers: []config.Driver{liveDriver}}
	srv := New(&Deps{
		Cfg:        live,
		CfgMu:      &sync.RWMutex{},
		ConfigPath: filepath.Join(dir, "config.yaml"),
		State:      st,
		Registry:   reg,
	})

	payload := map[string]any{
		"name": "myuplink",
		"lua":  luaPath,
		"config": map[string]any{
			"refresh_token": "stale-token",
			"rotate_secret": true,
			"persist_value": "rotated-token",
		},
	}
	if postOwner {
		payload["credential_owner"] = liveDriver.CredentialOwner
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/drivers/test", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}

	if got, ok := st.LoadConfig(driverSecretStateKey("credential-myuplink", secretKey)); !ok || got != "rotated-token" {
		t.Fatalf("persisted secret = %q ok=%v, want rotated-token", got, ok)
	}

	after, ok := reg.ControlStatus("myuplink")
	if !ok {
		t.Fatal("live driver missing after probe")
	}
	if after.Generation <= before.Generation {
		t.Fatalf("generation = %d after probe, want greater than %d after rotated shared secret",
			after.Generation, before.Generation)
	}
	if got, _, ok := tel.LatestMetric("myuplink", "used_rotated_token"); !ok || got != 1 {
		t.Fatalf("restarted driver did not read the owner's new token: %v %v", got, ok)
	}
}

func TestHandleDriverTestDoesNotRestartRunningDriverWhenSecretUnchanged(t *testing.T) {
	dir := t.TempDir()
	luaPath := writeProbeRestartLua(t, dir)

	st, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const secretKey = "refresh_token"
	if err := st.SaveConfig(driverSecretStateKey("credential-myuplink", secretKey), "same-token"); err != nil {
		t.Fatalf("save secret: %v", err)
	}

	tel := telemetry.NewStore()
	reg := drivers.NewRegistry(tel)
	reg.SecretOverride = func(driverName, key string) (string, bool) {
		return st.LoadConfig(driverSecretStateKey(driverName, key))
	}
	reg.SecretPersister = func(driverName, key, value string) error {
		return st.SaveConfig(driverSecretStateKey(driverName, key), value)
	}
	t.Cleanup(reg.ShutdownAll)

	liveDriver := config.Driver{
		Name:            "myuplink",
		CredentialOwner: "credential-myuplink",
		Lua:             luaPath,
		Config: map[string]any{
			"refresh_token": "stale-token",
		},
	}
	if err := reg.Add(context.Background(), liveDriver); err != nil {
		t.Fatalf("add live driver: %v", err)
	}

	before, ok := reg.ControlStatus("myuplink")
	if !ok {
		t.Fatal("live driver missing before probe")
	}

	live := &config.Config{Drivers: []config.Driver{liveDriver}}
	srv := New(&Deps{
		Cfg:        live,
		CfgMu:      &sync.RWMutex{},
		ConfigPath: filepath.Join(dir, "config.yaml"),
		State:      st,
		Registry:   reg,
	})

	body, _ := json.Marshal(map[string]any{
		"name": "myuplink",
		"lua":  luaPath,
		"config": map[string]any{
			"refresh_token": "stale-token",
			"rotate_secret": true,
			"persist_value": "same-token",
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/drivers/test", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}

	after, ok := reg.ControlStatus("myuplink")
	if !ok {
		t.Fatal("live driver missing after probe")
	}
	if after.Generation != before.Generation {
		t.Fatalf("generation changed from %d to %d even though shared secret was unchanged",
			before.Generation, after.Generation)
	}
}
