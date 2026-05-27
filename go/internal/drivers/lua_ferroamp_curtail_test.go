package drivers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// Regression for the 2026-05-27 incident: the dispatcher's
// curtail_disable path used to translate to `pplim arg=0` on Ferroamp's
// extapi. Ferroamp treats that as "limit PV output to 0 W" — same wire
// bytes as a release would have, opposite semantics — and the inverter
// then sticks at 0 W PV until the operator clears pplim from the
// Ferroamp portal or power-cycles the EnergyHub.
//
// The defensive contract enforced by these tests:
//  1. A `curtail` action with power_w <= 0 MUST NOT publish anything
//     (or at minimum, must not publish a pplim with arg <= 0).
//  2. A `curtail_disable` action with the default config (no
//     pplim_release_w set) MUST NOT publish anything — leaving whatever
//     pplim Ferroamp last received in place; operator clears via portal.
//  3. A `curtail_disable` action with config.pplim_release_w = N MUST
//     publish exactly `pplim arg=N` so a properly-configured operator
//     gets the auto-release behaviour back.

// newFerroampDriverWithConfig is a parameterised copy of
// newTestFerroampDriver (lua_mqtt_stale_test.go) that lets the test
// pass a YAML-style config block into driver_init.
func newFerroampDriverWithConfig(t *testing.T, tel *telemetry.Store, mqtt *fakeMQTT, config map[string]any) *LuaDriver {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	luaPath := filepath.Join(wd, "..", "..", "..", "drivers", "ferroamp.lua")
	if _, err := os.Stat(luaPath); err != nil {
		t.Fatalf("ferroamp.lua not found at %s: %v", luaPath, err)
	}
	env := NewHostEnv("ferroamp", tel).WithMQTT(mqtt)
	env.BatteryCapacityWh = 15200
	d, err := NewLuaDriver(luaPath, env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := d.Init(context.Background(), config); err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(d.Cleanup)
	return d
}

// publishedSinceMark returns the publish payloads that arrived after
// the caller-provided mark index. Used to ignore init-time chatter
// (extapiversion + auto) and inspect only what a specific command
// produced.
func publishedSinceMark(mqtt *fakeMQTT, mark int) []string {
	pubs := mqtt.Published()
	if mark >= len(pubs) {
		return nil
	}
	out := make([]string, 0, len(pubs)-mark)
	for _, p := range pubs[mark:] {
		out = append(out, p.Payload)
	}
	return out
}

func TestFerroampCurtailWithZeroWattsDoesNotPublishPplimZero(t *testing.T) {
	mqtt := &fakeMQTT{}
	d := newFerroampDriverWithConfig(t, telemetry.NewStore(), mqtt, nil)
	mark := len(mqtt.Published()) // skip init chatter

	if err := d.Command(context.Background(),
		[]byte(`{"action":"curtail","power_w":0}`)); err != nil {
		t.Fatalf("curtail 0 W: %v", err)
	}

	for _, p := range publishedSinceMark(mqtt, mark) {
		if strings.Contains(p, `"pplim"`) && strings.Contains(p, `"arg":0`) {
			t.Errorf("curtail 0 W must NOT publish pplim arg=0 (sticky lock on Ferroamp firmware) — got %q", p)
		}
	}
}

func TestFerroampCurtailWithNegativeWattsDoesNotPublishPplimZero(t *testing.T) {
	mqtt := &fakeMQTT{}
	d := newFerroampDriverWithConfig(t, telemetry.NewStore(), mqtt, nil)
	mark := len(mqtt.Published())

	if err := d.Command(context.Background(),
		[]byte(`{"action":"curtail","power_w":-50}`)); err != nil {
		t.Fatalf("curtail -50 W: %v", err)
	}

	for _, p := range publishedSinceMark(mqtt, mark) {
		if strings.Contains(p, `"pplim"`) && strings.Contains(p, `"arg":0`) {
			t.Errorf("curtail -50 W (math.abs → 50) must not collapse to pplim arg=0; got %q", p)
		}
	}
}

func TestFerroampCurtailDisableDefaultSkipsPublish(t *testing.T) {
	mqtt := &fakeMQTT{}
	d := newFerroampDriverWithConfig(t, telemetry.NewStore(), mqtt, nil)
	mark := len(mqtt.Published())

	if err := d.Command(context.Background(),
		[]byte(`{"action":"curtail_disable"}`)); err != nil {
		t.Fatalf("curtail_disable: %v", err)
	}

	for _, p := range publishedSinceMark(mqtt, mark) {
		if strings.Contains(p, `"pplim"`) {
			t.Errorf("curtail_disable with default config must not publish pplim (would trip the sticky-zero trap if arg=0); got %q", p)
		}
	}
}

func TestFerroampCurtailDisableWithReleaseWattsPublishesRelease(t *testing.T) {
	mqtt := &fakeMQTT{}
	d := newFerroampDriverWithConfig(t, telemetry.NewStore(), mqtt, map[string]any{
		"pplim_release_w": 15000,
	})
	mark := len(mqtt.Published())

	if err := d.Command(context.Background(),
		[]byte(`{"action":"curtail_disable"}`)); err != nil {
		t.Fatalf("curtail_disable: %v", err)
	}

	want := `"pplim","arg":15000`
	found := false
	for _, p := range publishedSinceMark(mqtt, mark) {
		if strings.Contains(p, want) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("curtail_disable with pplim_release_w=15000 must publish pplim arg=15000; got %v",
			publishedSinceMark(mqtt, mark))
	}
}

func TestFerroampCurtailNormalWattsPublishesPplim(t *testing.T) {
	mqtt := &fakeMQTT{}
	d := newFerroampDriverWithConfig(t, telemetry.NewStore(), mqtt, nil)
	mark := len(mqtt.Published())

	if err := d.Command(context.Background(),
		[]byte(`{"action":"curtail","power_w":1500}`)); err != nil {
		t.Fatalf("curtail 1500 W: %v", err)
	}

	want := `"pplim","arg":1500`
	found := false
	for _, p := range publishedSinceMark(mqtt, mark) {
		if strings.Contains(p, want) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("curtail 1500 W must publish pplim arg=1500; got %v",
			publishedSinceMark(mqtt, mark))
	}
}
