package drivers

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// fakeMQTT is a minimal MQTTCap stub for unit tests. Tests push messages
// via Push(); the driver drains them through host.mqtt_messages() →
// PopMessages().
type fakeMQTT struct {
	mu     sync.Mutex
	queue  []MQTTMessage
	subs   []string
	pubs   []MQTTMessage
	closed bool
}

func (f *fakeMQTT) Subscribe(topic string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs = append(f.subs, topic)
	return nil
}

func (f *fakeMQTT) Publish(topic string, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pubs = append(f.pubs, MQTTMessage{Topic: topic, Payload: string(payload)})
	return nil
}

func (f *fakeMQTT) PopMessages() []MQTTMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.queue
	f.queue = nil
	return out
}

func (f *fakeMQTT) Close() error { f.closed = true; return nil }

func (f *fakeMQTT) Push(topic, payload string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, MQTTMessage{Topic: topic, Payload: payload})
}

func (f *fakeMQTT) Published() []MQTTMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]MQTTMessage, len(f.pubs))
	copy(out, f.pubs)
	return out
}

func newTestFerroampDriver(t *testing.T, tel *telemetry.Store, mqtt *fakeMQTT) (*LuaDriver, *HostEnv) {
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
	if err := d.Init(context.Background(), nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(d.Cleanup)
	return d, env
}

func newBundledMQTTDriver(t *testing.T, filename, name string, tel *telemetry.Store, mqtt *fakeMQTT, cfg map[string]any) *LuaDriver {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	luaPath := filepath.Join(wd, "..", "..", "..", "drivers", filename)
	if _, err := os.Stat(luaPath); err != nil {
		t.Fatalf("%s not found at %s: %v", filename, luaPath, err)
	}
	env := NewHostEnv(name, tel).WithMQTT(mqtt)
	d, err := NewLuaDriver(luaPath, env)
	if err != nil {
		t.Fatalf("load %s: %v", filename, err)
	}
	if err := d.Init(context.Background(), cfg); err != nil {
		t.Fatalf("init %s: %v", filename, err)
	}
	t.Cleanup(d.Cleanup)
	return d
}

func TestFerroampDC2V2XDriverTelemetryAndCommand(t *testing.T) {
	tel := telemetry.NewStore()
	mqtt := &fakeMQTT{}
	d := newBundledMQTTDriver(t, "ferroamp_dc2_v2x.lua", "dc2", tel, mqtt, nil)

	mqtt.Push("dc2/v2x/system", `{
		"timestamp": 1772745398,
		"Host": "dc2-v2x-test",
		"State": "discharging",
		"Control": "mqtt",
		"Power set": -25,
		"Charged energy": "1.250 kWh",
		"Discharged energy": "0.750 kWh"
	}`)
	mqtt.Push("dc2/v2x/pecc", `{
		"timestamp": 1772746264,
		"PECCStatus2": {
			"measuredVoltage": 300,
			"measuredCurrent": -12.5
		},
		"PECCLimits1": {
			"limitPowerMax": 20000
		},
		"PECCLimits3": {
			"limitDischargePowerMax": -15000
		}
	}`)
	mqtt.Push("dc2/v2x/secc", `{
		"VehicleStatus": {
			"batteryStateOfCharge": 64,
			"evConnectionState": "energyTransferAllowed"
		}
	}`)

	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	r := tel.Get("dc2", telemetry.DerV2X)
	if r == nil {
		t.Fatal("expected v2x telemetry")
	}
	if r.RawW != -3750 {
		t.Fatalf("v2x w = %v, want -3750", r.RawW)
	}
	if r.SoC == nil || *r.SoC != 0.64 {
		t.Fatalf("vehicle soc = %v, want 0.64", r.SoC)
	}
	if !strings.Contains(string(r.Data), `"session_charge_wh":1250`) || !strings.Contains(string(r.Data), `"session_discharge_wh":750`) {
		t.Fatalf("v2x data missing session energy: %s", string(r.Data))
	}

	if err := d.Command(context.Background(), []byte(`{"action":"v2x_set_power","power_w":-5000}`)); err != nil {
		t.Fatalf("command: %v", err)
	}
	pubs := mqtt.Published()
	if len(pubs) < 2 {
		t.Fatalf("unexpected publish: %+v", pubs)
	}
	assertDC2ControlPublish(t, pubs[len(pubs)-2], "dc2/ui/control/controller", "MQTT")
	assertDC2ControlPublish(t, pubs[len(pubs)-1], "dc2/ui/control/power", -33.333333333333336)
}

func assertDC2ControlPublish(t *testing.T, msg MQTTMessage, wantTopic string, wantValue any) {
	t.Helper()
	if msg.Topic != wantTopic {
		t.Fatalf("topic = %q, want %q; all msg=%+v", msg.Topic, wantTopic, msg)
	}
	var payload struct {
		Timestamp float64 `json:"timestamp"`
		Value     any     `json:"value"`
	}
	if err := json.Unmarshal([]byte(msg.Payload), &payload); err != nil {
		t.Fatalf("decode %s payload %q: %v", wantTopic, msg.Payload, err)
	}
	if payload.Timestamp <= 0 {
		t.Fatalf("timestamp = %v, want unix seconds", payload.Timestamp)
	}
	switch want := wantValue.(type) {
	case string:
		got, ok := payload.Value.(string)
		if !ok || got != want {
			t.Fatalf("%s value = %#v, want %q", wantTopic, payload.Value, want)
		}
	case float64:
		got, ok := payload.Value.(float64)
		if !ok || math.Abs(got-want) > 0.000001 {
			t.Fatalf("%s value = %#v, want %v", wantTopic, payload.Value, want)
		}
	default:
		t.Fatalf("unsupported wantValue type %T", wantValue)
	}
}

func TestAmbiboxV2XDriverTelemetryAndCommand(t *testing.T) {
	tel := telemetry.NewStore()
	mqtt := &fakeMQTT{}
	d := newBundledMQTTDriver(t, "ambibox_v2x.lua", "ambibox", tel, mqtt, nil)

	mqtt.Push("device/evCharger/0/powerAc", "-3200")
	mqtt.Push("device/evCharger/0/powerDc", "-3300")
	mqtt.Push("device/evCharger/0/soc", "58")
	mqtt.Push("device/evCharger/0/evConnected", "true")
	mqtt.Push("device/evCharger/0/dischargePowerMax", "11000")

	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	r := tel.Get("ambibox", telemetry.DerV2X)
	if r == nil {
		t.Fatal("expected v2x telemetry")
	}
	if r.RawW != -3200 {
		t.Fatalf("v2x w = %v, want -3200", r.RawW)
	}
	if r.SoC == nil || *r.SoC != 0.58 {
		t.Fatalf("vehicle soc = %v, want 0.58", r.SoC)
	}

	if err := d.Command(context.Background(), []byte(`{"action":"v2x_set_power","power_w":-2500}`)); err != nil {
		t.Fatalf("command: %v", err)
	}
	pubs := mqtt.Published()
	if len(pubs) == 0 || pubs[len(pubs)-1].Topic != "device/ess/0/targetPower" || pubs[len(pubs)-1].Payload != "-2500" {
		t.Fatalf("unexpected publish: %+v", pubs)
	}
}

func TestAmbiboxV2XDriverSoCOnlyUpdateKeepsPowerSnapshot(t *testing.T) {
	tel := telemetry.NewStore()
	mqtt := &fakeMQTT{}
	d := newBundledMQTTDriver(t, "ambibox_v2x.lua", "ambibox", tel, mqtt, nil)

	mqtt.Push("device/evCharger/0/soc", "58")
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll soc-only before power: %v", err)
	}
	if r := tel.Get("ambibox", telemetry.DerV2X); r != nil {
		t.Fatalf("expected no v2x telemetry before powerAc exists, got %+v", r)
	}

	mqtt.Push("device/evCharger/0/powerAc", "3200")
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll power: %v", err)
	}
	r := tel.Get("ambibox", telemetry.DerV2X)
	if r == nil || r.RawW != 3200 || r.SoC == nil || *r.SoC != 0.58 {
		t.Fatalf("initial v2x telemetry = %+v, want w=3200 soc=0.58", r)
	}

	mqtt.Push("device/evCharger/0/soc", "59")
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll soc-only after power: %v", err)
	}
	r = tel.Get("ambibox", telemetry.DerV2X)
	if r == nil || r.RawW != 3200 || r.SoC == nil || *r.SoC != 0.59 {
		t.Fatalf("soc-only v2x telemetry = %+v, want cached w=3200 soc=0.59", r)
	}
}

func TestAmbiboxV2XDriverDoesNotRefreshStalePowerSnapshot(t *testing.T) {
	tel := telemetry.NewStore()
	mqtt := &fakeMQTT{}
	d := newBundledMQTTDriver(t, "ambibox_v2x.lua", "ambibox", tel, mqtt, map[string]any{
		"telemetry_max_age_ms": 1,
	})

	mqtt.Push("device/evCharger/0/powerAc", "3200")
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll power: %v", err)
	}
	first := tel.Get("ambibox", telemetry.DerV2X)
	if first == nil || first.RawW != 3200 {
		t.Fatalf("initial v2x telemetry = %+v, want w=3200", first)
	}
	firstUpdatedAt := first.UpdatedAt

	time.Sleep(3 * time.Millisecond)
	mqtt.Push("device/evCharger/0/soc", "60")
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll stale soc-only: %v", err)
	}
	after := tel.Get("ambibox", telemetry.DerV2X)
	if after == nil || !after.UpdatedAt.Equal(firstUpdatedAt) {
		t.Fatalf("stale power should not be refreshed by soc-only update: before=%v after=%+v", firstUpdatedAt, after)
	}

	mqtt.Push("device/evCharger/0/powerAc", "")
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll retained delete: %v", err)
	}
	afterDelete := tel.Get("ambibox", telemetry.DerV2X)
	if afterDelete == nil || !afterDelete.UpdatedAt.Equal(firstUpdatedAt) {
		t.Fatalf("retained delete should not emit 0W telemetry: before=%v after=%+v", firstUpdatedAt, afterDelete)
	}
}

func TestAmbiboxV2XDriverClampsSetpointToLimits(t *testing.T) {
	tel := telemetry.NewStore()
	mqtt := &fakeMQTT{}
	d := newBundledMQTTDriver(t, "ambibox_v2x.lua", "ambibox", tel, mqtt, nil)

	mqtt.Push("device/evCharger/0/powerAc", "0")
	mqtt.Push("device/evCharger/0/chargePowerMax", "3200")
	mqtt.Push("device/evCharger/0/dischargePowerMax", "2500")
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll limits: %v", err)
	}

	if err := d.Command(context.Background(), []byte(`{"action":"v2x_set_power","power_w":5000}`)); err != nil {
		t.Fatalf("charge command: %v", err)
	}
	if err := d.Command(context.Background(), []byte(`{"action":"v2x_set_power","power_w":-5000}`)); err != nil {
		t.Fatalf("discharge command: %v", err)
	}
	pubs := mqtt.Published()
	if len(pubs) < 2 {
		t.Fatalf("expected two publishes, got %+v", pubs)
	}
	if pubs[len(pubs)-2].Payload != "3200" || pubs[len(pubs)-1].Payload != "-2500" {
		t.Fatalf("unexpected clamped publishes: %+v", pubs)
	}
}

func TestFerroampDC2V2XRequiresFreshPowerPairAndIgnoresControlEcho(t *testing.T) {
	tel := telemetry.NewStore()
	mqtt := &fakeMQTT{}
	d := newBundledMQTTDriver(t, "ferroamp_dc2_v2x.lua", "dc2", tel, mqtt, map[string]any{
		"telemetry_max_age_ms": 1,
	})

	mqtt.Push("dc2/connector/1/pe/measured_voltage", "400")
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll voltage-only: %v", err)
	}
	if r := tel.Get("dc2", telemetry.DerV2X); r != nil {
		t.Fatalf("expected no v2x telemetry from partial power pair, got %+v", r)
	}

	time.Sleep(3 * time.Millisecond)
	mqtt.Push("dc2/connector/1/pe/measured_current", "-10")
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll current with stale voltage: %v", err)
	}
	if r := tel.Get("dc2", telemetry.DerV2X); r != nil {
		t.Fatalf("expected no v2x telemetry from stale voltage/current pair, got %+v", r)
	}

	mqtt.Push("dc2/connector/1/pe/measured_voltage", "400")
	mqtt.Push("dc2/connector/1/pe/measured_current", "-10")
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll full pair: %v", err)
	}
	first := tel.Get("dc2", telemetry.DerV2X)
	if first == nil || first.RawW != -4000 {
		t.Fatalf("full pair v2x telemetry = %+v, want -4000W", first)
	}
	firstUpdatedAt := first.UpdatedAt

	mqtt.Push("dc2/ui/control", "-4.00")
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll control echo: %v", err)
	}
	after := tel.Get("dc2", telemetry.DerV2X)
	if after == nil || !after.UpdatedAt.Equal(firstUpdatedAt) {
		t.Fatalf("control echo should not refresh cached power: before=%v after=%+v", firstUpdatedAt, after)
	}
}

// Regression: ferroamp.lua used to cache the last MQTT payload per
// topic and emit from cache on every poll, regardless of how long
// ago the last message arrived. When the EnergyHub lost power on a
// fuse blow (real incident 2026-05-02) the driver re-emitted the
// last-seen pv_w/meter_w to four decimals identical for 30+ minutes
// while the inverter was physically dead. host.emit advanced
// LastSuccess on every tick, so the watchdog never flipped offline.
//
// The fix is two-part:
//  1. ferroamp.lua tracks per-topic arrival timestamps and stops
//     emitting once the cache exceeds STALE_AFTER_MS.
//  2. registry's runLoop bumps TickCount but NOT LastSuccess on a
//     bare poll-return — only host.emit advances LastSuccess.
//
// This test exercises both: it loads the real ferroamp.lua, feeds a
// few fresh messages, then stops the upstream and polls past the
// staleness threshold. After staleness, the driver must:
//   - NOT advance LastSuccess (watchdog will flip it offline)
//   - NOT push fresh meter/pv readings into the telemetry store
func TestFerroampDriverStopsEmittingWhenMQTTStalls(t *testing.T) {
	tel := telemetry.NewStore()
	mqtt := &fakeMQTT{}
	d, env := newTestFerroampDriver(t, tel, mqtt)

	// Feed one fresh ehub payload — keep the JSON minimal but
	// shape-compatible with extract_val (each field wraps its value
	// in a {"val": …} object). The pext.L1 etc. trigger a meter
	// emit; pext sums to -7000 W so the meter reading lands as
	// negative (export, site convention).
	ehub := `{"pext":{"L1":-2000,"L2":-2500,"L3":-2500},"ppv":{"val":7000},` +
		`"pbat":{"val":-1000},"gridfreq":{"val":50.0},` +
		`"ul":{"L1":230,"L2":230,"L3":230},` +
		`"iext":{"L1":-8.7,"L2":-10.9,"L3":-10.9}}`
	mqtt.Push("extapi/data/ehub", ehub)

	// First poll: driver consumes the fresh message, emits meter +
	// pv + battery → host.emit advances LastSuccess.
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll-1: %v", err)
	}
	h := tel.DriverHealth("ferroamp")
	if h == nil || h.LastSuccess == nil {
		t.Fatalf("expected LastSuccess after a fresh emit; got %+v", h)
	}
	freshMeter := tel.Get("ferroamp", telemetry.DerMeter)
	if freshMeter == nil || freshMeter.RawW == 0 {
		t.Fatalf("meter should have a non-zero reading after fresh ehub; got %+v", freshMeter)
	}

	// Snapshot what was last seen, then go silent. Wait long enough
	// to exceed STALE_AFTER_MS (30 s in the driver). Tests can't
	// burn 30 s of wall-clock, so we cheat: rewind the host's
	// monotonic Start so host.millis() reports a far-future value
	// without sleeping. host.millis() = (now - Start) in ms.
	env.Start = env.Start.Add(-2 * time.Minute) // -> millis() jumps by +120000

	// Snapshot LastSuccess before the stale poll so we can check it
	// doesn't advance.
	lastBefore := *h.LastSuccess

	// Drive several polls without feeding new MQTT data. The driver
	// must detect the stale cache, drop it, and skip emit. Sleep
	// 2 ms between polls so time.Now() in RecordSuccess can advance
	// — otherwise an accidental RecordSuccess could leave
	// LastSuccess unchanged at the same nanosecond and we'd miss a
	// regression.
	for i := 0; i < 3; i++ {
		time.Sleep(2 * time.Millisecond)
		if _, err := d.Poll(context.Background()); err != nil {
			t.Fatalf("poll stale-%d: %v", i, err)
		}
	}

	if !h.LastSuccess.Equal(lastBefore) {
		t.Errorf("LastSuccess advanced while cache was stale (before=%v after=%v)",
			lastBefore, *h.LastSuccess)
	}
}

func TestFerroampDriverFallsBackToEsoPbatWhenEhubPbatIsZero(t *testing.T) {
	tel := telemetry.NewStore()
	mqtt := &fakeMQTT{}
	d, _ := newTestFerroampDriver(t, tel, mqtt)

	ehub := `{"pext":{"L1":0,"L2":0,"L3":0},"ppv":{"val":0},"pbat":{"val":0}}`
	eso := `{"pbat":{"val":1200},"soc":{"val":29},"ubat":{"val":48},"ibat":{"val":25}}`
	mqtt.Push("extapi/data/ehub", ehub)
	mqtt.Push("extapi/data/eso", eso)

	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	bat := tel.Get("ferroamp", telemetry.DerBattery)
	if bat == nil {
		t.Fatal("expected battery reading")
	}
	if bat.RawW != -1200 {
		t.Fatalf("battery RawW = %v, want -1200 from ESO pbat fallback", bat.RawW)
	}
	if bat.SoC == nil || *bat.SoC != 0.29 {
		t.Fatalf("battery SoC = %v, want 0.29", bat.SoC)
	}
}

func TestFerroampDriverPrefersEsoCurrentForBatteryPower(t *testing.T) {
	tel := telemetry.NewStore()
	mqtt := &fakeMQTT{}
	d, _ := newTestFerroampDriver(t, tel, mqtt)

	ehub := `{"pext":{"L1":0,"L2":0,"L3":0},"ppv":{"val":0},"pbat":{"val":-300}}`
	eso := `{"pbat":{"val":-300},"soc":{"val":29},"ubat":{"val":308.8},"ibat":{"val":0}}`
	mqtt.Push("extapi/data/ehub", ehub)
	mqtt.Push("extapi/data/eso", eso)

	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	bat := tel.Get("ferroamp", telemetry.DerBattery)
	if bat == nil {
		t.Fatal("expected battery reading")
	}
	if bat.RawW != 0 {
		t.Fatalf("battery RawW = %v, want 0 from ESO ubat*ibat", bat.RawW)
	}
	gotA, _, ok := tel.LatestMetric("ferroamp", "battery_dc_a")
	if !ok {
		t.Fatal("missing battery_dc_a metric")
	}
	if gotA != 0 {
		t.Fatalf("battery_dc_a = %v, want 0", gotA)
	}
}

func TestFerroampDriverFallsBackToSsoPpvWhenEhubPpvIsZero(t *testing.T) {
	tel := telemetry.NewStore()
	mqtt := &fakeMQTT{}
	d, _ := newTestFerroampDriver(t, tel, mqtt)

	ehub := `{"pext":{"L1":0,"L2":0,"L3":0},"ppv":{"val":0},"pbat":{"val":0}}`
	sso := `{"upv":{"val":400},"ipv":{"val":7.75}}`
	mqtt.Push("extapi/data/ehub", ehub)
	mqtt.Push("extapi/data/sso", sso)

	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	pv := tel.Get("ferroamp", telemetry.DerPV)
	if pv == nil {
		t.Fatal("expected PV reading")
	}
	if pv.RawW != -3100 {
		t.Fatalf("PV RawW = %v, want -3100 from SSO voltage/current fallback", pv.RawW)
	}
}

func TestFerroampDriverEmitsStateRelayAndFaultDiagnostics(t *testing.T) {
	tel := telemetry.NewStore()
	mqtt := &fakeMQTT{}
	d, _ := newTestFerroampDriver(t, tel, mqtt)

	ehub := `{"pext":{"L1":0,"L2":0,"L3":0},"ppv":{"val":0},"pbat":{"val":0},"state":{"val":32816}}`
	eso := `{"pbat":{"val":0},"soc":{"val":29},"ubat":{"val":649},"ibat":{"val":0},` +
		`"udc":{"val":16},"relaystatus":{"val":1},"faultcode":{"val":0}}`
	sso := `{"upv":{"val":496.067},"ipv":{"val":0},"udc":{"val":495.344},` +
		`"relaystatus":{"val":1},"faultcode":{"val":2}}`
	mqtt.Push("extapi/data/ehub", ehub)
	mqtt.Push("extapi/data/eso", eso)
	mqtt.Push("extapi/data/sso", sso)

	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	assertMetric := func(name string, want float64) {
		t.Helper()
		got, _, ok := tel.LatestMetric("ferroamp", name)
		if !ok {
			t.Fatalf("missing metric %s", name)
		}
		if got != want {
			t.Fatalf("metric %s = %v, want %v", name, got, want)
		}
	}
	assertMetric("ehub_state", 32816)
	assertMetric("eso_relaystatus", 1)
	assertMetric("eso_faultcode", 0)
	assertMetric("eso_dc_link_v", 16)
	assertMetric("sso_relaystatus", 1)
	assertMetric("sso_faultcode", 2)
	assertMetric("sso_dc_link_v", 495.344)
	assertMetric("sso_pv_v", 496.067)
	assertMetric("sso_pv_a", 0)
}

// Live-system regression (2026-05-24, 2×ESO site): a single-slot
// `eso_data` cache made the driver report ONLY the most-recently-
// published ESO's ubat*ibat as battery power. With two ESOs publishing
// on extapi/data/eso the driver halved bat_w, the controller's grid-
// chase loop fed on the wrong number, and dispatch stuck ~190 W of
// grid import against a 0 W target. ehub.pbat aggregated correctly
// (~306 W) but the driver preferred ESO ubat*ibat (~153 W).
func TestFerroampDriverSumsBatteryPowerAcrossMultipleESOs(t *testing.T) {
	tel := telemetry.NewStore()
	mqtt := &fakeMQTT{}
	d, _ := newTestFerroampDriver(t, tel, mqtt)

	// Two ESOs each at 400 V × 0.375 A = 150 W → aggregate 300 W
	// discharging. ehub.pbat is intentionally inconsistent to prove
	// we prefer the summed per-ESO measurement.
	ehub := `{"pext":{"L1":0,"L2":0,"L3":0},"ppv":{"val":0},"pbat":{"val":999}}`
	eso1 := `{"id":{"val":"21030026"},"soc":{"val":50},"ubat":{"val":400},"ibat":{"val":0.375},` +
		`"wbatprod":{"val":3600000000},"wbatcons":{"val":7200000000},` +
		`"relaystatus":{"val":0},"faultcode":{"val":0},"udc":{"val":760}}`
	eso2 := `{"id":{"val":"23010216"},"soc":{"val":50},"ubat":{"val":400},"ibat":{"val":0.375},` +
		`"wbatprod":{"val":3600000000},"wbatcons":{"val":7200000000},` +
		`"relaystatus":{"val":0},"faultcode":{"val":0},"udc":{"val":760}}`
	mqtt.Push("extapi/data/ehub", ehub)
	mqtt.Push("extapi/data/eso", eso1)
	mqtt.Push("extapi/data/eso", eso2)

	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	bat := tel.Get("ferroamp", telemetry.DerBattery)
	if bat == nil {
		t.Fatal("expected battery reading")
	}
	if bat.RawW != -300 {
		t.Fatalf("battery RawW = %v, want -300 (sum 150+150 across both ESOs, discharging)", bat.RawW)
	}
	// Current sums (parallel strings), voltage averages.
	gotA, _, ok := tel.LatestMetric("ferroamp", "battery_dc_a")
	if !ok {
		t.Fatal("missing battery_dc_a metric")
	}
	if gotA != 0.75 {
		t.Fatalf("battery_dc_a = %v, want 0.75 (sum of both ibat)", gotA)
	}
	gotV, _, ok := tel.LatestMetric("ferroamp", "battery_dc_v")
	if !ok {
		t.Fatal("missing battery_dc_v metric")
	}
	if gotV != 400 {
		t.Fatalf("battery_dc_v = %v, want 400 (avg of equal ubats)", gotV)
	}
	// Wh counters sum across ESOs (Data carries the per-emit raw JSON).
	// Each ESO publishes 3.6e9 mJ produced + 7.2e9 mJ consumed; summing
	// over 2 ESOs and converting mJ → Wh gives 2 Wh produced + 4 Wh
	// consumed.
	if !strings.Contains(string(bat.Data), `"discharge_wh":2`) {
		t.Fatalf("battery Data missing discharge_wh=2: %s", string(bat.Data))
	}
	if !strings.Contains(string(bat.Data), `"charge_wh":4`) {
		t.Fatalf("battery Data missing charge_wh=4: %s", string(bat.Data))
	}
	// Diagnostic: operators must see how many ESOs the driver is summing.
	gotCount, _, ok := tel.LatestMetric("ferroamp", "eso_count")
	if !ok {
		t.Fatal("missing eso_count metric")
	}
	if gotCount != 2 {
		t.Fatalf("eso_count = %v, want 2", gotCount)
	}
}

// One ESO faulted in a 2-ESO cluster: the worst-of relay/fault metric
// must surface the fault rather than be averaged away by the healthy
// peer, and the aggregate must still include the healthy ESO's power.
func TestFerroampDriverSurfacesPerESOFaultsWorstOf(t *testing.T) {
	tel := telemetry.NewStore()
	mqtt := &fakeMQTT{}
	d, _ := newTestFerroampDriver(t, tel, mqtt)

	ehub := `{"pext":{"L1":0,"L2":0,"L3":0},"ppv":{"val":0},"pbat":{"val":0}}`
	healthy := `{"id":{"val":"AAA"},"soc":{"val":80},"ubat":{"val":400},"ibat":{"val":1.0},` +
		`"relaystatus":{"val":0},"faultcode":{"val":0},"udc":{"val":760}}`
	faulted := `{"id":{"val":"BBB"},"soc":{"val":80},"ubat":{"val":400},"ibat":{"val":0},` +
		`"relaystatus":{"val":1},"faultcode":{"val":42},"udc":{"val":760}}`
	mqtt.Push("extapi/data/ehub", ehub)
	mqtt.Push("extapi/data/eso", healthy)
	mqtt.Push("extapi/data/eso", faulted)

	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	gotFault, _, ok := tel.LatestMetric("ferroamp", "eso_faultcode")
	if !ok {
		t.Fatal("missing eso_faultcode")
	}
	if gotFault != 42 {
		t.Fatalf("eso_faultcode = %v, want 42 (worst-of so a single faulted ESO is visible)", gotFault)
	}
	gotRelay, _, ok := tel.LatestMetric("ferroamp", "eso_relaystatus")
	if !ok {
		t.Fatal("missing eso_relaystatus")
	}
	if gotRelay != 1 {
		t.Fatalf("eso_relaystatus = %v, want 1 (worst-of)", gotRelay)
	}
	// Healthy ESO still contributes its 400 W of discharge.
	bat := tel.Get("ferroamp", telemetry.DerBattery)
	if bat == nil || bat.RawW != -400 {
		t.Fatalf("battery RawW = %v, want -400 (healthy ESO 400×1.0 + faulted 400×0)", bat.RawW)
	}
}

// Live-system regression (2026-05-24): with power_w=0 the driver
// used to publish `auto`, which puts the EnergyHub in autonomous
// self-consumption. That silently overrode every planner slot that
// wanted the battery idle so PV surplus could export — Ferroamp
// kept covering local load and discharged ~500 W against target 0.
// power_w=0 must mean "hold the battery at 0 W, do not delegate".
func TestFerroampZeroBatteryCommandForcesIdleNotAuto(t *testing.T) {
	tel := telemetry.NewStore()
	mqtt := &fakeMQTT{}
	d, _ := newTestFerroampDriver(t, tel, mqtt)

	initial := len(mqtt.Published())
	if initial == 0 {
		t.Fatal("init should publish extapiversion + auto")
	}

	if err := d.Command(context.Background(), []byte(`{"action":"battery","power_w":0}`)); err != nil {
		t.Fatalf("zero command: %v", err)
	}
	pubs := mqtt.Published()
	if got := len(pubs); got != initial+1 {
		t.Fatalf("zero command publish count = %d, want %d (must transition out of init auto into forced idle)", got, initial+1)
	}
	last := pubs[len(pubs)-1].Payload
	if !strings.Contains(last, `"name":"discharge"`) || !strings.Contains(last, `"arg":0`) {
		t.Fatalf("zero command payload = %s, want discharge with arg=0 (force idle, not auto)", last)
	}
	if strings.Contains(last, `"name":"auto"`) {
		t.Fatalf("zero command payload = %s, must NOT publish auto (regression: auto lets EnergyHub self-consume)", last)
	}

	// Idempotent: once we're in forced idle, another power_w=0 must not
	// republish — that would churn MQTT on every dispatch tick.
	if err := d.Command(context.Background(), []byte(`{"action":"battery","power_w":0}`)); err != nil {
		t.Fatalf("second zero command: %v", err)
	}
	if got := len(mqtt.Published()); got != initial+1 {
		t.Fatalf("second zero command published %d new messages, want 0 (idempotent)", got-(initial+1))
	}

	if err := d.Command(context.Background(), []byte(`{"action":"battery","power_w":-1200}`)); err != nil {
		t.Fatalf("discharge command: %v", err)
	}
	pubs = mqtt.Published()
	if got := len(pubs); got != initial+2 {
		t.Fatalf("discharge publish count = %d, want %d", got, initial+2)
	}
	if !strings.Contains(pubs[len(pubs)-1].Payload, `"name":"discharge"`) {
		t.Fatalf("last payload = %s, want discharge command", pubs[len(pubs)-1].Payload)
	}

	// Back to zero from a real discharge: must publish forced idle again
	// (last_control_mode is now "discharge", not "idle").
	if err := d.Command(context.Background(), []byte(`{"action":"battery","power_w":0}`)); err != nil {
		t.Fatalf("release-to-idle command: %v", err)
	}
	pubs = mqtt.Published()
	if got := len(pubs); got != initial+3 {
		t.Fatalf("release-to-idle publish count = %d, want %d", got, initial+3)
	}
	last = pubs[len(pubs)-1].Payload
	if !strings.Contains(last, `"name":"discharge"`) || !strings.Contains(last, `"arg":0`) {
		t.Fatalf("release-to-idle payload = %s, want forced idle (discharge arg=0)", last)
	}
}

// Helpers for the multi-ESO scaling tests below.

// lastBatteryDispatch returns the most-recent control-request payload
// that targets the battery (charge/discharge/idle), skipping the init
// `auto` / `extapiversion` traffic.
func lastBatteryDispatch(t *testing.T, mqtt *fakeMQTT) string {
	t.Helper()
	pubs := mqtt.Published()
	for i := len(pubs) - 1; i >= 0; i-- {
		p := pubs[i].Payload
		if strings.Contains(p, `"name":"charge"`) ||
			strings.Contains(p, `"name":"discharge"`) {
			return p
		}
	}
	t.Fatalf("no battery dispatch payload in %d published messages", len(pubs))
	return ""
}

// esoPayload builds a minimal eso/data payload at the given SoC. udc /
// ubat / ibat are kept consistent with the existing multi-ESO tests so
// the battery emit path stays happy (otherwise n_eso never advances).
func esoPayload(id string, soc float64) string {
	return `{"id":{"val":"` + id + `"},"soc":{"val":` +
		strconv.FormatFloat(soc, 'f', -1, 64) +
		`},"ubat":{"val":400},"ibat":{"val":0.5},` +
		`"relaystatus":{"val":0},"faultcode":{"val":0},"udc":{"val":760}}`
}

func esoPayloadNoSoC(id string) string {
	return `{"id":{"val":"` + id + `"},"ubat":{"val":400},"ibat":{"val":0.5},` +
		`"relaystatus":{"val":0},"faultcode":{"val":0},"udc":{"val":760}}`
}

// Live-system regression (Stefan's 4-ESO site, 2026-05-26): the
// EnergyHub divides a dispatch setpoint evenly across all ESOs it knows
// about, including units pinned at their SoC floor that physically
// refuse to respond. Asking for 1.3 kW with 2 of 4 ESOs floored at min
// SoC delivered ~0.66 kW. The driver now pre-scales by N_total/N_capable.
func TestFerroampMultiESOScalingDoublesWhenHalfFloored(t *testing.T) {
	tel := telemetry.NewStore()
	mqtt := &fakeMQTT{}
	d, _ := newTestFerroampDriver(t, tel, mqtt)

	// 4 ESOs: two floored at 10% (below the 15% discharge floor), two
	// active at 50%. ehub.pbat is irrelevant — n_eso comes from the per-
	// ESO map.
	mqtt.Push("extapi/data/ehub", `{"pext":{"L1":0,"L2":0,"L3":0},"ppv":{"val":0},"pbat":{"val":0}}`)
	mqtt.Push("extapi/data/eso", esoPayload("A", 10))
	mqtt.Push("extapi/data/eso", esoPayload("B", 10))
	mqtt.Push("extapi/data/eso", esoPayload("C", 50))
	mqtt.Push("extapi/data/eso", esoPayload("D", 50))
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	if err := d.Command(context.Background(),
		[]byte(`{"action":"battery","power_w":-1300}`)); err != nil {
		t.Fatalf("discharge command: %v", err)
	}

	got := lastBatteryDispatch(t, mqtt)
	if !strings.Contains(got, `"name":"discharge"`) || !strings.Contains(got, `"arg":2600`) {
		t.Fatalf("dispatch payload = %s, want discharge arg=2600 (1300 * 4/2)", got)
	}

	// Diagnostic metric must reflect the 2.0x scale.
	if v, _, ok := tel.LatestMetric("ferroamp", "eso_dispatch_scale_x1000"); !ok || v != 2000 {
		t.Fatalf("eso_dispatch_scale_x1000 = %v, want 2000", v)
	}
	// Commanded target (pre-scale, site convention) must be the planner's request.
	if v, _, ok := tel.LatestMetric("ferroamp", "eso_dispatch_commanded_w"); !ok || v != -1300 {
		t.Fatalf("eso_dispatch_commanded_w = %v, want -1300", v)
	}
}

// The scale is capped to MAX_DISPATCH_SCALE (2.0×) so a transient "only
// 1 of 4 capable" snapshot can't quadruple the on-wire setpoint past
// the per-driver MaxChargeW / fuse-guard ceiling enforced upstream in
// dispatch.go. Deeper imbalance is left under-delivered (safe failure).
func TestFerroampMultiESOScalingCappedAtMax(t *testing.T) {
	tel := telemetry.NewStore()
	mqtt := &fakeMQTT{}
	d, _ := newTestFerroampDriver(t, tel, mqtt)

	mqtt.Push("extapi/data/ehub", `{"pext":{"L1":0,"L2":0,"L3":0},"ppv":{"val":0},"pbat":{"val":0}}`)
	mqtt.Push("extapi/data/eso", esoPayload("A", 10))
	mqtt.Push("extapi/data/eso", esoPayload("B", 10))
	mqtt.Push("extapi/data/eso", esoPayload("C", 10))
	mqtt.Push("extapi/data/eso", esoPayload("D", 50))
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	if err := d.Command(context.Background(),
		[]byte(`{"action":"battery","power_w":-1000}`)); err != nil {
		t.Fatalf("discharge command: %v", err)
	}

	got := lastBatteryDispatch(t, mqtt)
	// 4/1 = 4.0× uncapped; clamped to 2.0× → arg=2000.
	if !strings.Contains(got, `"arg":2000`) {
		t.Fatalf("dispatch payload = %s, want discharge arg=2000 (cap at 2.0x, not 4.0x)", got)
	}
	if v, _, ok := tel.LatestMetric("ferroamp", "eso_dispatch_scale_x1000"); !ok || v != 2000 {
		t.Fatalf("eso_dispatch_scale_x1000 = %v, want 2000 (clamped)", v)
	}
}

// Freshness gate: between polls a partial broker stall could leave
// last_eso_count high but last_eso_*_capable low, scaling all later
// commands. driver_command must refuse to scale when the per-ESO
// snapshot is older than STALE_AFTER_MS (30 s).
func TestFerroampMultiESOScalingRefusesStaleSnapshot(t *testing.T) {
	tel := telemetry.NewStore()
	mqtt := &fakeMQTT{}
	d, env := newTestFerroampDriver(t, tel, mqtt)

	mqtt.Push("extapi/data/ehub", `{"pext":{"L1":0,"L2":0,"L3":0},"ppv":{"val":0},"pbat":{"val":0}}`)
	mqtt.Push("extapi/data/eso", esoPayload("A", 10))
	mqtt.Push("extapi/data/eso", esoPayload("B", 50))
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	// Rewind the host clock by > STALE_AFTER_MS so the snapshot from
	// the poll above looks stale to driver_command.
	env.Start = env.Start.Add(-31 * time.Second)

	if err := d.Command(context.Background(),
		[]byte(`{"action":"battery","power_w":-1000}`)); err != nil {
		t.Fatalf("discharge command: %v", err)
	}

	got := lastBatteryDispatch(t, mqtt)
	if !strings.Contains(got, `"arg":1000`) {
		t.Fatalf("dispatch payload = %s, want raw arg=1000 (stale snapshot must not scale)", got)
	}
	if v, _, ok := tel.LatestMetric("ferroamp", "eso_dispatch_scale_x1000"); !ok || v != 1000 {
		t.Fatalf("eso_dispatch_scale_x1000 = %v, want 1000 (no scaling on stale data)", v)
	}
}

// All ESOs floored: capable==0 in the requested direction. We must
// idle instead of publishing a non-zero command nothing can fulfil —
// otherwise the planner sees no SoC progress, the loop sets a higher
// target, and we keep churning MQTT for nothing.
func TestFerroampMultiESOAllFlooredIdles(t *testing.T) {
	tel := telemetry.NewStore()
	mqtt := &fakeMQTT{}
	d, _ := newTestFerroampDriver(t, tel, mqtt)

	mqtt.Push("extapi/data/ehub", `{"pext":{"L1":0,"L2":0,"L3":0},"ppv":{"val":0},"pbat":{"val":0}}`)
	mqtt.Push("extapi/data/eso", esoPayload("A", 10))
	mqtt.Push("extapi/data/eso", esoPayload("B", 10))
	mqtt.Push("extapi/data/eso", esoPayload("C", 10))
	mqtt.Push("extapi/data/eso", esoPayload("D", 10))
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	if err := d.Command(context.Background(),
		[]byte(`{"action":"battery","power_w":-1500}`)); err != nil {
		t.Fatalf("discharge command: %v", err)
	}

	got := lastBatteryDispatch(t, mqtt)
	if !strings.Contains(got, `"name":"discharge"`) || !strings.Contains(got, `"arg":0`) {
		t.Fatalf("dispatch payload = %s, want forced idle (discharge arg=0) when all ESOs floored", got)
	}
	if v, _, ok := tel.LatestMetric("ferroamp", "eso_dispatch_scale_x1000"); !ok || v != 0 {
		t.Fatalf("eso_dispatch_scale_x1000 = %v, want 0 (no dispatch attempted)", v)
	}
}

// ESOs with a missing `soc` field must count as capable in both
// directions. The alternative (skip from capable counts but keep in
// n_eso) inflates the scale on partial telemetry and overdelivers.
func TestFerroampMultiESOMissingSocCountsCapable(t *testing.T) {
	tel := telemetry.NewStore()
	mqtt := &fakeMQTT{}
	d, _ := newTestFerroampDriver(t, tel, mqtt)

	mqtt.Push("extapi/data/ehub", `{"pext":{"L1":0,"L2":0,"L3":0},"ppv":{"val":0},"pbat":{"val":0}}`)
	mqtt.Push("extapi/data/eso", esoPayload("A", 50))
	mqtt.Push("extapi/data/eso", esoPayload("B", 50))
	mqtt.Push("extapi/data/eso", esoPayloadNoSoC("C"))
	mqtt.Push("extapi/data/eso", esoPayloadNoSoC("D"))
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	if err := d.Command(context.Background(),
		[]byte(`{"action":"battery","power_w":-1000}`)); err != nil {
		t.Fatalf("discharge command: %v", err)
	}

	got := lastBatteryDispatch(t, mqtt)
	if !strings.Contains(got, `"arg":1000`) {
		t.Fatalf("dispatch payload = %s, want raw arg=1000 (missing-soc ESOs must count as capable)", got)
	}
	if v, _, ok := tel.LatestMetric("ferroamp", "eso_dispatch_scale_x1000"); !ok || v != 1000 {
		t.Fatalf("eso_dispatch_scale_x1000 = %v, want 1000 (no scaling when all 4 are capable)", v)
	}
}

func TestFerroampMQTTCleanupReturnsToAuto(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	luaPath := filepath.Join(wd, "..", "..", "..", "drivers", "ferroamp.lua")
	if _, err := os.Stat(luaPath); err != nil {
		t.Fatalf("ferroamp.lua not found at %s: %v", luaPath, err)
	}

	mqtt := &fakeMQTT{}
	env := NewHostEnv("ferroamp", telemetry.NewStore()).WithMQTT(mqtt)
	d, err := NewLuaDriver(luaPath, env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := d.Init(context.Background(), nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := d.Command(context.Background(), []byte(`{"action":"battery","power_w":2500}`)); err != nil {
		t.Fatalf("command: %v", err)
	}

	d.Cleanup()

	pubs := mqtt.Published()
	if len(pubs) == 0 {
		t.Fatal("expected cleanup to publish auto command")
	}
	last := pubs[len(pubs)-1]
	if last.Topic != "extapi/control/request" {
		t.Fatalf("last publish topic = %q, want extapi/control/request", last.Topic)
	}
	want := `{"transId":"cleanup","cmd":{"name":"auto"}}`
	if last.Payload != want {
		t.Fatalf("last publish payload = %s, want %s", last.Payload, want)
	}
}
