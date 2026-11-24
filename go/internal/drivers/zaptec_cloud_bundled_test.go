package drivers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// bundledZaptecCloudPath resolves the bundled drivers/zaptec_cloud.lua, the
// file a release ships, from the repo root.
func bundledZaptecCloudPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "drivers", "zaptec_cloud.lua")
}

// TestZaptecCloudControlsThroughFTWHost runs the bundled Zaptec Cloud driver in
// Core's Lua host against a fake Zaptec API. It pins the exact writes: the
// current goes to maxChargeCurrent, a pause is 0 A plus command 506, a resume
// offers 6 A and sends 507, and the stand-down after FTW wrote is 0 A plus 506
// once. Every request names FTW in its User-Agent.
func TestZaptecCloudControlsThroughFTWHost(t *testing.T) {
	path := bundledZaptecCloudPath(t)
	var mu sync.Mutex
	var posts []string
	state := `[{"StateId":710,"ValueAsString":"3"},{"StateId":718,"ValueAsString":"0"},{"StateId":712,"ValueAsString":"0"},
{"StateId":507,"ValueAsString":"15.9"},{"StateId":508,"ValueAsString":"16.1"},{"StateId":509,"ValueAsString":"15.8"},
{"StateId":510,"ValueAsString":"16"},{"StateId":513,"ValueAsString":"11000"},{"StateId":553,"ValueAsString":"2.5"},
{"StateId":100,"ValueAsString":"{\"ProductVariant\":\"Go2\"}"}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "FTW-zaptec_cloud/") {
			t.Errorf("%s %s without FTW User-Agent: %q", r.Method, r.URL.Path, r.Header.Get("User-Agent"))
		}
		switch {
		case r.URL.Path == "/oauth/token":
			_, _ = w.Write([]byte(`{"access_token":"tok","refresh_token":"ref","expires_in":3600}`))
		case r.URL.Path == "/api/chargers" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"Data":[{"Id":"c-1","SerialNo":"ZAP1"}]}`))
		case r.URL.Path == "/api/chargers/c-1/state":
			_, _ = w.Write([]byte(state))
		case r.Method == http.MethodPost:
			if r.Header.Get("Authorization") != "Bearer tok" {
				t.Errorf("POST %s without bearer token", r.URL.Path)
			}
			mu.Lock()
			posts = append(posts, r.URL.Path+" "+string(body))
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	tel := telemetry.NewStore()
	d, err := NewLuaDriver(path, NewHostEnv("zaptec", tel).WithHTTP())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer d.Cleanup()
	if err := d.Init(context.Background(), map[string]any{"email": "o@example.invalid", "password": "pw", "base_url": srv.URL}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	r := tel.Get("zaptec", telemetry.DerEV)
	if r == nil {
		t.Fatal("no ev reading")
	}
	raw, _ := json.Marshal(r)
	t.Logf("ev reading: %s", raw)
	if r.RawW != 11000 {
		t.Errorf("EV W=%v, want 11000 (charging, positive)", r.RawW)
	}
	cmd := func(m map[string]any) error {
		b, _ := json.Marshal(m)
		return d.Command(context.Background(), b)
	}
	if err := cmd(map[string]any{"action": "ev_set_current", "power_w": 11040, "phase_mode": "3p", "voltage": 230, "max_amps_per_phase": 13.5}); err != nil {
		t.Fatalf("set current: %v", err)
	}
	if err := cmd(map[string]any{"action": "ev_pause"}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := cmd(map[string]any{"action": "ev_resume"}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := d.DefaultMode(); err != nil {
		t.Fatalf("default mode: %v", err)
	}
	if err := d.DefaultMode(); err != nil {
		t.Fatalf("second default mode: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, p := range posts {
		t.Logf("POST %s", p)
	}
	want := []string{
		`/api/chargers/c-1/update {"maxChargeCurrent":13.5}`,
		`/api/chargers/c-1/update {"maxChargeCurrent":0}`,
		`/api/chargers/c-1/sendCommand/506 `,
		`/api/chargers/c-1/update {"maxChargeCurrent":6}`,
		`/api/chargers/c-1/sendCommand/507 `,
		`/api/chargers/c-1/update {"maxChargeCurrent":0}`,
		`/api/chargers/c-1/sendCommand/506 `,
	}
	got := posts
	if len(got) != len(want) {
		t.Fatalf("posts = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("post %d = %q, want %q", i, got[i], want[i])
		}
	}
}
