package drivers

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

func newTestHeishamonDriver(t *testing.T) (*LuaDriver, *fakeMQTT, *telemetry.Store) {
	t.Helper()
	tel := telemetry.NewStore()
	mqtt := &fakeMQTT{}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(wd, "..", "..", "..", "drivers", "heishamon.lua")
	d, err := NewLuaDriver(path, NewHostEnv("heishamon", tel).WithMQTT(mqtt))
	if err != nil {
		t.Fatalf("load heishamon.lua: %v", err)
	}
	if err := d.Init(context.Background(), nil); err != nil {
		d.Cleanup()
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(d.Cleanup)
	return d, mqtt, tel
}

func TestHeishamonEmitsMetricsWithoutFakeBattery(t *testing.T) {
	d, mqtt, tel := newTestHeishamonDriver(t)
	if len(mqtt.subs) != 1 || mqtt.subs[0] != "panasonic_heat_pump/#" {
		t.Fatalf("subscriptions = %v, want panasonic_heat_pump/#", mqtt.subs)
	}

	mqtt.Push("panasonic_heat_pump/main/Outside_Temp", "-4.5")
	mqtt.Push("panasonic_heat_pump/main/Main_Inlet_Temp", "31.2")
	mqtt.Push("panasonic_heat_pump/main/Main_Outlet_Temp", "35.7")
	mqtt.Push("panasonic_heat_pump/main/Main_Target_Temp", "36")
	mqtt.Push("panasonic_heat_pump/main/Z1_Heat_Request_Temp", "2")
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	wants := map[string]float64{
		"hp_outside_temp_c": -4.5,
		"hp_inlet_temp_c":   31.2,
		"hp_outlet_temp_c":  35.7,
		"hp_target_temp_c":  36,
		"hp_z1_heat_offset": 2,
	}
	for name, want := range wants {
		got, _, ok := tel.LatestMetric("heishamon", name)
		if !ok || got != want {
			t.Errorf("%s = %v (ok=%v), want %v", name, got, ok, want)
		}
	}
	if got := tel.Get("heishamon", telemetry.DerBattery); got != nil {
		t.Fatalf("metric-only heat-pump driver emitted fake battery telemetry: %+v", got)
	}
	if health := tel.DriverHealth("heishamon"); health == nil || !health.IsOnline() {
		t.Fatalf("metric emission must keep driver online, health=%+v", health)
	}
}

func TestHeishamonClampsOffsetAndDefaultModeResetsIt(t *testing.T) {
	d, mqtt, _ := newTestHeishamonDriver(t)
	if err := d.Command(context.Background(), []byte(`{"action":"set_heat_curve_offset","offset":99}`)); err != nil {
		t.Fatalf("command: %v", err)
	}
	if err := d.DefaultMode(); err != nil {
		t.Fatalf("default mode: %v", err)
	}

	pubs := mqtt.Published()
	if len(pubs) != 2 {
		t.Fatalf("published = %v, want clamp command and safe reset", pubs)
	}
	if pubs[0].Topic != "panasonic_heat_pump/set/Z1_Heat_Request_Temp" || pubs[0].Payload != "3" {
		t.Errorf("clamped publish = %+v, want offset 3", pubs[0])
	}
	if pubs[1].Payload != "0" {
		t.Errorf("default-mode publish = %+v, want safe offset 0", pubs[1])
	}
}
