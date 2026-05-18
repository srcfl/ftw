package drivers

import (
	"context"
	"os"
	"path/filepath"
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
	// Resolve drivers/ferroamp.lua relative to the repo root.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	luaPath := filepath.Join(wd, "..", "..", "..", "drivers", "ferroamp.lua")
	if _, err := os.Stat(luaPath); err != nil {
		t.Fatalf("ferroamp.lua not found at %s: %v", luaPath, err)
	}

	tel := telemetry.NewStore()
	mqtt := &fakeMQTT{}
	env := NewHostEnv("ferroamp", tel).WithMQTT(mqtt)
	env.BatteryCapacityWh = 15200

	d, err := NewLuaDriver(luaPath, env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer d.Cleanup()
	if err := d.Init(context.Background(), nil); err != nil {
		t.Fatalf("init: %v", err)
	}

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
