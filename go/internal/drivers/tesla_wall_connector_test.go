package drivers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

func teslaWCDriverPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "testdata", "tesla_wall_connector.lua")
}

type teslaWCFake struct {
	versionHits int
	vitalsHits  int
	vitalsBody  []byte
}

func (f *teslaWCFake) handler(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/1/version":
		f.versionHits++
		_ = json.NewEncoder(w).Encode(map[string]string{
			"serial_number":    "TWC-SN-1",
			"part_number":      "1529455-02-J",
			"firmware_version": "22.36.1",
		})
	case "/api/1/vitals":
		f.vitalsHits++
		if len(f.vitalsBody) > 0 {
			_, _ = w.Write(f.vitalsBody)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"contactor_closed":  true,
			"vehicle_connected": true,
			"grid_v":            230.0,
			"vehicle_current_a": 16.0,
			"currentA_a":        16.1,
			"currentB_a":        16.0,
			"currentC_a":        16.2,
			"voltageA_v":        230.0,
			"voltageB_v":        229.5,
			"voltageC_v":        230.4,
			"session_energy_wh": 4200.0,
			"evse_state":        11,
		})
	default:
		http.Error(w, "unknown "+r.URL.Path, http.StatusNotFound)
	}
}

func TestTeslaWallConnectorInitPollAndReadonlyCommands(t *testing.T) {
	fake := &teslaWCFake{}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()

	tel := telemetry.NewStore()
	env := NewHostEnv("tesla-wc", tel).WithHTTP()
	d, err := NewLuaDriver(teslaWCDriverPath(t), env)
	if err != nil {
		t.Fatalf("load driver: %v", err)
	}
	defer d.Cleanup()

	if err := d.Init(context.Background(), map[string]any{
		"base_url": srv.URL,
	}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if fake.versionHits != 1 {
		t.Fatalf("version hits=%d, want 1", fake.versionHits)
	}

	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	reading := tel.Get("tesla-wc", telemetry.DerEV)
	if reading == nil {
		t.Fatal("no EV reading after poll")
	}
	// 230*16.1 + 229.5*16.0 + 230.4*16.2 = 3703 + 3672 + 3732.48 = 11107.48
	if reading.RawW < 11100 || reading.RawW > 11120 {
		t.Errorf("EV W=%v, want ~11107 (sum of V×A)", reading.RawW)
	}
	var extra map[string]any
	if err := json.Unmarshal(reading.Data, &extra); err != nil {
		t.Fatalf("decode EV data: %v", err)
	}
	if extra["connected"] != true {
		t.Errorf("connected=%v, want true", extra["connected"])
	}
	if extra["charging"] != true {
		t.Errorf("charging=%v, want true", extra["charging"])
	}
	if extra["session_wh"] != 4200.0 {
		t.Errorf("session_wh=%v, want 4200", extra["session_wh"])
	}

	if err := d.Command(context.Background(), []byte(`{"action":"ev_set_current","power_w":11040}`)); err != nil {
		t.Fatalf("ev_set_current should ack: %v", err)
	}
	if err := d.Command(context.Background(), []byte(`{"action":"ev_pause"}`)); err != nil {
		t.Fatalf("ev_pause should ack: %v", err)
	}
	if err := d.Command(context.Background(), []byte(`{"action":"ev_resume"}`)); err != nil {
		t.Fatalf("ev_resume should ack: %v", err)
	}
	if err := d.DefaultMode(); err != nil {
		t.Fatalf("default mode: %v", err)
	}
}

func TestTeslaWallConnectorRepairsBareNaN(t *testing.T) {
	fake := &teslaWCFake{vitalsBody: []byte(`{"vehicle_connected":false,"evse_state":1,"grid_v":nan,"session_energy_wh":0}`)}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()

	tel := telemetry.NewStore()
	env := NewHostEnv("tesla-wc", tel).WithHTTP()
	d, err := NewLuaDriver(teslaWCDriverPath(t), env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer d.Cleanup()
	if err := d.Init(context.Background(), map[string]any{"base_url": srv.URL}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if tel.Get("tesla-wc", telemetry.DerEV) == nil {
		t.Fatal("expected EV reading after repaired nan JSON")
	}
}

func TestTeslaWallConnectorHostConfig(t *testing.T) {
	fake := &teslaWCFake{}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	tel := telemetry.NewStore()
	env := NewHostEnv("tesla-wc", tel).WithHTTP()
	d, err := NewLuaDriver(teslaWCDriverPath(t), env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer d.Cleanup()
	if err := d.Init(context.Background(), map[string]any{"host": host}); err != nil {
		t.Fatalf("init with host: %v", err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if tel.Get("tesla-wc", telemetry.DerEV) == nil {
		t.Fatal("expected EV reading when configured with bare host")
	}
}

func teslaWCPoll(t *testing.T, fake *teslaWCFake, config map[string]any) *telemetry.DerReading {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	if config == nil {
		config = map[string]any{}
	}
	if _, ok := config["base_url"]; !ok {
		if _, ok := config["host"]; !ok {
			config["base_url"] = srv.URL
		}
	}
	tel := telemetry.NewStore()
	env := NewHostEnv("tesla-wc", tel).WithHTTP()
	d, err := NewLuaDriver(teslaWCDriverPath(t), env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	t.Cleanup(func() { d.Cleanup() })
	if err := d.Init(context.Background(), config); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	reading := tel.Get("tesla-wc", telemetry.DerEV)
	if reading == nil {
		t.Fatal("no EV reading after poll")
	}
	return reading
}

func teslaWCExtra(t *testing.T, reading *telemetry.DerReading) map[string]any {
	t.Helper()
	var extra map[string]any
	if err := json.Unmarshal(reading.Data, &extra); err != nil {
		t.Fatalf("decode EV data: %v", err)
	}
	return extra
}

func TestTeslaWallConnectorIgnoresGhostPhases(t *testing.T) {
	// Unused legs sit at ~1–3 V. Power must be L1 only, not 3× noise.
	fake := &teslaWCFake{vitalsBody: []byte(`{
		"contactor_closed": true,
		"vehicle_connected": true,
		"grid_v": 230,
		"vehicle_current_a": 16,
		"currentA_a": 16,
		"currentB_a": 0.2,
		"currentC_a": 0.1,
		"voltageA_v": 230,
		"voltageB_v": 2.4,
		"voltageC_v": 1.8,
		"session_energy_wh": 100,
		"evse_state": 11
	}`)}
	reading := teslaWCPoll(t, fake, nil)
	if reading.RawW < 3670 || reading.RawW > 3690 {
		t.Errorf("EV W=%v, want ~3680 (230×16, ignore ghost B/C)", reading.RawW)
	}
	extra := teslaWCExtra(t, reading)
	if extra["phases"] != 1.0 && extra["phases"] != float64(1) {
		// json numbers decode as float64
		if n, ok := extra["phases"].(float64); !ok || n != 1 {
			t.Errorf("phases=%v, want 1", extra["phases"])
		}
	}
}

func TestTeslaWallConnectorSplitPhase(t *testing.T) {
	fake := &teslaWCFake{vitalsBody: []byte(`{
		"contactor_closed": true,
		"vehicle_connected": true,
		"grid_v": 240,
		"vehicle_current_a": 32,
		"currentA_a": 32,
		"currentB_a": 32,
		"voltageA_v": 120,
		"voltageB_v": 120,
		"session_energy_wh": 50,
		"evse_state": 11
	}`)}
	reading := teslaWCPoll(t, fake, map[string]any{"split_phase": true})
	// grid_v × vehicle_current_a = 7680, not 120×32 + 120×32 = 7680
	// (same number here) — lock the intended formula by using a grid
	// voltage that would diverge from the per-leg sum.
	if reading.RawW < 7670 || reading.RawW > 7690 {
		t.Errorf("EV W=%v, want 7680 (grid_v × vehicle_current_a)", reading.RawW)
	}
}

func TestTeslaWallConnectorSplitPhaseUsesGridNotLegs(t *testing.T) {
	// Per-leg sum would be 120×40 + 120×40 = 9600; split-phase uses 240×32.
	fake := &teslaWCFake{vitalsBody: []byte(`{
		"contactor_closed": true,
		"vehicle_connected": true,
		"grid_v": 240,
		"vehicle_current_a": 32,
		"currentA_a": 40,
		"currentB_a": 40,
		"voltageA_v": 120,
		"voltageB_v": 120,
		"session_energy_wh": 50,
		"evse_state": 11
	}`)}
	reading := teslaWCPoll(t, fake, map[string]any{"split_phase": true})
	if reading.RawW < 7670 || reading.RawW > 7690 {
		t.Errorf("EV W=%v, want 7680 not per-leg 9600", reading.RawW)
	}
}

func TestTeslaWallConnectorIdleDisconnected(t *testing.T) {
	fake := &teslaWCFake{vitalsBody: []byte(`{
		"contactor_closed": false,
		"vehicle_connected": false,
		"grid_v": 230,
		"vehicle_current_a": 0,
		"currentA_a": 0,
		"currentB_a": 0,
		"currentC_a": 0,
		"voltageA_v": 230,
		"voltageB_v": 230,
		"voltageC_v": 230,
		"session_energy_wh": 0,
		"evse_state": 1
	}`)}
	reading := teslaWCPoll(t, fake, nil)
	if reading.RawW != 0 {
		t.Errorf("idle W=%v, want 0", reading.RawW)
	}
	extra := teslaWCExtra(t, reading)
	if extra["connected"] != false {
		t.Errorf("connected=%v, want false", extra["connected"])
	}
	if extra["charging"] != false {
		t.Errorf("charging=%v, want false", extra["charging"])
	}
}

func TestTeslaWallConnectorRepairsMissingBrace(t *testing.T) {
	fake := &teslaWCFake{vitalsBody: []byte(`{"vehicle_connected":true,"evse_state":2,"grid_v":230,"vehicle_current_a":0,"session_energy_wh":12`)}
	reading := teslaWCPoll(t, fake, nil)
	extra := teslaWCExtra(t, reading)
	if extra["connected"] != true {
		t.Errorf("connected=%v after repaired JSON, want true", extra["connected"])
	}
}

func TestTeslaWallConnectorVitalsFailureKeepsDriver(t *testing.T) {
	fake := &teslaWCFake{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/1/vitals" {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		fake.handler(w, r)
	}))
	defer srv.Close()

	tel := telemetry.NewStore()
	env := NewHostEnv("tesla-wc", tel).WithHTTP()
	d, err := NewLuaDriver(teslaWCDriverPath(t), env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer d.Cleanup()
	if err := d.Init(context.Background(), map[string]any{"base_url": srv.URL}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll must not fail the driver on a vitals miss: %v", err)
	}
	if tel.Get("tesla-wc", telemetry.DerEV) != nil {
		t.Error("no EV reading should be emitted on a failed vitals fetch")
	}
}

func TestTeslaWallConnectorPhaseFallbackToGrid(t *testing.T) {
	// No usable phase voltages → grid_v × vehicle_current_a.
	fake := &teslaWCFake{vitalsBody: []byte(`{
		"contactor_closed": true,
		"vehicle_connected": true,
		"grid_v": 230,
		"vehicle_current_a": 10,
		"voltageA_v": 2,
		"voltageB_v": 1,
		"voltageC_v": 0,
		"session_energy_wh": 1,
		"evse_state": 11
	}`)}
	reading := teslaWCPoll(t, fake, nil)
	if reading.RawW < 2290 || reading.RawW > 2310 {
		t.Errorf("EV W=%v, want 2300 fallback", reading.RawW)
	}
}
