package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
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
