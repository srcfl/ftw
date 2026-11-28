package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	handler, _ := api.FirstRunProbes(firstRunProbeDeps(configPath, dir))
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

// No password exists on a new box, so the probe runs only the box's own
// driver files. Anything else is refused before it is read.
func TestBootstrapDriverTestRunsOnlyTheBoxesDrivers(t *testing.T) {
	dir := t.TempDir()
	elsewhere := t.TempDir()
	outside := filepath.Join(elsewhere, "outside.lua")
	if err := os.WriteFile(outside, []byte("DRIVER = { id = \"x\" }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	handler, _ := api.FirstRunProbes(firstRunProbeDeps(filepath.Join(dir, "config.yaml"), dir))
	for _, lua := range []string{"/etc/hosts", outside, filepath.Join(dir, "..", filepath.Base(elsewhere), "outside.lua"),
		"drivers/../../../../../../etc/hosts", filepath.Join(dir, "missing.lua")} {
		req := httptest.NewRequest(http.MethodPost, "/api/drivers/test", strings.NewReader(`{"name":"x","lua":`+quoteJSON(lua)+`}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "one of this box's drivers") {
			t.Errorf("%s: %d %s, want 400", lua, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "localhost") {
			t.Errorf("%s: the reply echoes the file: %s", lua, rec.Body.String())
		}
	}
}

// One probe or scan at a time: a second caller is told to wait, and the
// next one after the first ends runs.
func TestBootstrapProbesRunOneAtATime(t *testing.T) {
	dir := t.TempDir()
	write := func(name, poll string) string {
		path := filepath.Join(dir, name)
		src := "DRIVER = { id = \"p\" }\n" +
			"function driver_init(config) host.set_poll_interval(50) end\n" +
			"function driver_poll() " + poll + " end\n" +
			"function driver_command() return false end\n" +
			"function driver_default_mode() end\n"
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	silent := write("silent.lua", "")
	meter := write("meter.lua", "host.emit(\"meter\", { w = 1 })")
	driverTest, scan := api.FirstRunProbes(firstRunProbeDeps(filepath.Join(dir, "config.yaml"), dir))
	post := func(ctx context.Context, h http.HandlerFunc, path, lua string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"name":"p","lua":`+quoteJSON(lua)+`}`)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// A device that never answers holds the probe until its request ends.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); post(ctx, driverTest, "/api/drivers/test", silent) }()
	busy := false
	for i := 0; i < 100 && !busy; i++ {
		time.Sleep(20 * time.Millisecond)
		busy = post(context.Background(), driverTest, "/api/drivers/test", meter).Code == http.StatusTooManyRequests
	}
	if !busy {
		t.Fatal("a second test ran beside the first")
	}
	// The scan waits its turn too; it is refused before it touches the LAN.
	if rec := post(context.Background(), scan, "/api/scan?fingerprint=1", ""); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("scan beside a test = %d, want 429", rec.Code)
	}
	cancel()
	<-done
	if rec := post(context.Background(), driverTest, "/api/drivers/test", meter); rec.Code != http.StatusOK {
		t.Fatalf("after the first ended = %d %s, want 200", rec.Code, rec.Body.String())
	}
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
