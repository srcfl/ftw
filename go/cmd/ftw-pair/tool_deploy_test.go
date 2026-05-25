package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDeployDriverHappyPath(t *testing.T) {
	repo := t.TempDir()
	driverDir := filepath.Join(repo, "drivers")
	cfgPath := filepath.Join(repo, "config.yaml")
	_ = os.MkdirAll(driverDir, 0o755)
	_ = os.WriteFile(cfgPath, []byte("drivers: []\n"), 0o644)

	var ticks atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ticks.Add(1)
		json.NewEncoder(w).Encode(map[string]any{
			"name":       "goodwe_xs",
			"status":     "ok",
			"tick_count": ticks.Load(),
		})
	}))
	defer upstream.Close()

	sc := NewScope(repo, t.TempDir())
	a := NewAudit()
	tool := NewDeployDriverTool(sc, a, upstream.URL, cfgPath)
	out, err := tool.Handle(context.Background(), map[string]any{
		"name":       "goodwe_xs",
		"lua_source": "-- minimal driver\nfunction driver_init() end\nfunction driver_poll() end\n",
		"config":     map[string]any{"capabilities": []string{"pv"}, "lua": "drivers/goodwe_xs.lua"},
	})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	res := out.(map[string]any)
	if res["status"] != "ok" {
		t.Fatalf("status: %v", res)
	}
	luaPath := filepath.Join(driverDir, "goodwe_xs.lua")
	if _, err := os.Stat(luaPath); err != nil {
		t.Fatalf("lua not written: %v", err)
	}
	cfgBytes, _ := os.ReadFile(cfgPath)
	var raw map[string]any
	_ = yaml.Unmarshal(cfgBytes, &raw)
	drs, _ := raw["drivers"].([]any)
	if len(drs) != 1 {
		t.Fatalf("expected drivers list entry, got %v", raw)
	}
}
