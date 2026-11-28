package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/api"
)

// The setup guide tests each device before a box has settings. The probe
// runs the driver, reports what it read, and writes nothing.
func TestBootstrapDriverTestRunsTheDriverWithoutSettings(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	lua := filepath.Join(dir, "probe.lua")
	src := "DRIVER = { id = \"probe\" }\n" +
		"function driver_init(config) host.set_poll_interval(50) end\n" +
		"function driver_poll() host.emit(\"meter\", { w = 1500 }) end\n" +
		"function driver_command() return false end\n" +
		"function driver_default_mode() end\n"
	if err := os.WriteFile(lua, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	handler := api.FirstRunDriverTest(firstRunProbeDeps(configPath, dir))
	post := func(body string) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, "/api/drivers/test", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	code, out := post(`{"name":"meter","lua":` + quoteJSON(lua) + `}`)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("probe = %d %v, want ok", code, out)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("a probe must not write settings: %v", err)
	}

	// The same guard as Settings: a probe never reaches the box itself.
	code, out = post(`{"name":"inverter","lua":` + quoteJSON(lua) + `,"capabilities":{"modbus":{"host":"127.0.0.1","port":502}}}`)
	if code != http.StatusBadRequest || !strings.Contains(out["error"].(string), "loopback") {
		t.Fatalf("loopback probe = %d %v, want 400", code, out)
	}
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
