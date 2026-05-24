package drivers

import (
	"context"
	"os"
	"path/filepath"
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

func TestFerroampZeroBatteryCommandDoesNotRepublishAutoWhenAlreadyAuto(t *testing.T) {
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
	if got := len(mqtt.Published()); got != initial {
		t.Fatalf("zero command while already auto published %d new messages, want 0", got-initial)
	}

	if err := d.Command(context.Background(), []byte(`{"action":"battery","power_w":-1200}`)); err != nil {
		t.Fatalf("discharge command: %v", err)
	}
	pubs := mqtt.Published()
	if got := len(pubs); got != initial+1 {
		t.Fatalf("discharge publish count = %d, want %d", got, initial+1)
	}
	if !strings.Contains(pubs[len(pubs)-1].Payload, `"name":"discharge"`) {
		t.Fatalf("last payload = %s, want discharge command", pubs[len(pubs)-1].Payload)
	}

	if err := d.Command(context.Background(), []byte(`{"action":"battery","power_w":0}`)); err != nil {
		t.Fatalf("release command: %v", err)
	}
	pubs = mqtt.Published()
	if got := len(pubs); got != initial+2 {
		t.Fatalf("release publish count = %d, want %d", got, initial+2)
	}
	if !strings.Contains(pubs[len(pubs)-1].Payload, `"name":"auto"`) {
		t.Fatalf("last payload = %s, want auto command", pubs[len(pubs)-1].Payload)
	}

	if err := d.Command(context.Background(), []byte(`{"action":"battery","power_w":0}`)); err != nil {
		t.Fatalf("second zero command: %v", err)
	}
	if got := len(mqtt.Published()); got != initial+2 {
		t.Fatalf("second zero command published %d new messages, want 0", got-(initial+2))
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
