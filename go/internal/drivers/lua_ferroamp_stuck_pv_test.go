package drivers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// newFerroampDriverWithConfigAndEnv mirrors newFerroampDriverWithConfig
// from lua_ferroamp_curtail_test.go but also returns the HostEnv so the
// caller can rewind millis() via env.Start for time-based tests.
func newFerroampDriverWithConfigAndEnv(t *testing.T, tel *telemetry.Store, mqtt *fakeMQTT, config map[string]any) (*LuaDriver, *HostEnv) {
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
	return d, env
}

// 2026-05-27 recovery automation. After the sticky-pplim brick we want
// the driver to self-heal: if the SSO reports DC bus voltage (paneler
// aktiva), zero PV current, no fault, and the relay closed for a long
// continuous stretch, auto-publish a pplim release. Operator opts in
// by setting `pplim_release_w`; without it, the watchdog only logs.
//
// The test pushes a stuck-signature SSO payload, rewinds the host's
// monotonic clock past STUCK_PV_AFTER_MS (10 min) so we don't burn
// wall-clock, polls, and asserts:
//   1. pplim arg=<release_w> is published exactly once
//   2. stuck_pv_recovery_count metric advances to 1
//   3. Cooldown: a subsequent poll does NOT re-publish (counter stays 1)
//   4. A poll AFTER the cooldown CAN re-publish (counter advances to 2)
//   5. Once stuck signature clears (ipv > 0), counter stops advancing
func TestFerroampStuckPVWatchdogAutoPublishesRelease(t *testing.T) {
	tel := telemetry.NewStore()
	mqtt := &fakeMQTT{}
	d, env := newFerroampDriverWithConfigAndEnv(t, tel, mqtt, map[string]any{
		"pplim_release_w": 15000,
	})

	 // see helper at the bottom of this file

	// Helper: push one stuck-signature SSO payload and one trivial ehub
	// so the poll has something fresh to consume each tick. (Ehub kept
	// minimal — we only care about the SSO branch here.)
	pushStuckSSO := func() {
		mqtt.Push("extapi/data/sso",
			`{"id":{"val":"PS00990-A04-S23091622"},`+
				`"upv":{"val":"500.0"},`+
				`"ipv":{"val":"0.0"},`+
				`"relaystatus":{"val":"1"},`+
				`"faultcode":{"val":"0"},`+
				`"udc":{"val":"499.0"}}`)
		mqtt.Push("extapi/data/ehub",
			`{"pext":{"L1":-100,"L2":-100,"L3":-100},"ppv":{"val":300},`+
				`"pbat":{"val":0},"gridfreq":{"val":50.0},`+
				`"ul":{"L1":230,"L2":230,"L3":230},`+
				`"iext":{"L1":-0.4,"L2":-0.4,"L3":-0.4}}`)
	}

	// Tick 1: stuck signature seen for the first time. Should NOT
	// trigger recovery yet (under STUCK_PV_AFTER_MS).
	pushStuckSSO()
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll-1: %v", err)
	}
	mark1 := len(mqtt.Published())
	for _, p := range mqtt.Published() {
		if strings.Contains(p.Payload, `"name":"pplim"`) {
			t.Fatalf("tick 1: must not publish pplim immediately on first stuck-signature poll, got %q", p.Payload)
		}
	}

	// Rewind host millis() by 11 min so the next tick sees a stretch
	// of stuck-signature longer than STUCK_PV_AFTER_MS (10 min).
	env.Start = env.Start.Add(-11 * time.Minute)

	// Tick 2: stuck for > 10 min → auto-publish pplim release.
	pushStuckSSO()
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll-2: %v", err)
	}
	want := `"name":"pplim","arg":15000`
	found := false
	for _, p := range mqtt.Published()[mark1:] {
		if strings.Contains(p.Payload, want) {
			found = true
		}
	}
	if !found {
		t.Fatalf("tick 2: expected a pplim arg=15000 release publish, got %v",
			payloadsAfter(mqtt, mark1))
	}
	checkMetricStuckPV(t, tel.FlushSamples(), "stuck_pv_recovery_count", 1)

	// Tick 3: still stuck, but cooldown < STUCK_PV_RECOVERY_COOLDOWN_MS
	// since the recovery. Must NOT re-publish.
	mark3 := len(mqtt.Published())
	pushStuckSSO()
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll-3: %v", err)
	}
	for _, p := range mqtt.Published()[mark3:] {
		if strings.Contains(p.Payload, `"name":"pplim"`) {
			t.Fatalf("tick 3: cooldown should prevent re-publish, got %q", p.Payload)
		}
	}

	// Rewind another 6 min so we're past the cooldown (5 min) AND
	// still well past STUCK_PV_AFTER_MS.
	env.Start = env.Start.Add(-6 * time.Minute)
	mark4 := len(mqtt.Published())
	pushStuckSSO()
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll-4: %v", err)
	}
	found = false
	for _, p := range mqtt.Published()[mark4:] {
		if strings.Contains(p.Payload, want) {
			found = true
		}
	}
	if !found {
		t.Fatalf("tick 4: cooldown expired, expected a second pplim release publish")
	}
	checkMetricStuckPV(t, tel.FlushSamples(), "stuck_pv_recovery_count", 2)
}

// Without operator opt-in (`pplim_release_w` == 0) the watchdog must
// only LOG the stuck condition, never publish a recovery payload —
// because we have no safe value to send. (Per PR #367: `pplim arg=0`
// is the very trap we're trying to recover from.)
func TestFerroampStuckPVWatchdogNoPublishWithoutPplimReleaseW(t *testing.T) {
	tel := telemetry.NewStore()
	mqtt := &fakeMQTT{}
	d, env := newFerroampDriverWithConfigAndEnv(t, tel, mqtt, nil)

	

	push := func() {
		mqtt.Push("extapi/data/sso",
			`{"id":{"val":"PS00990-A04-S23091622"},`+
				`"upv":{"val":"500.0"},"ipv":{"val":"0.0"},`+
				`"relaystatus":{"val":"1"},"faultcode":{"val":"0"},`+
				`"udc":{"val":"499.0"}}`)
		mqtt.Push("extapi/data/ehub",
			`{"pext":{"L1":-100,"L2":-100,"L3":-100},"ppv":{"val":300},`+
				`"pbat":{"val":0},"gridfreq":{"val":50.0},`+
				`"ul":{"L1":230,"L2":230,"L3":230},`+
				`"iext":{"L1":-0.4,"L2":-0.4,"L3":-0.4}}`)
	}

	push()
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll-1: %v", err)
	}
	env.Start = env.Start.Add(-11 * time.Minute)
	mark := len(mqtt.Published())
	push()
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll-2: %v", err)
	}
	for _, p := range mqtt.Published()[mark:] {
		if strings.Contains(p.Payload, `"name":"pplim"`) {
			t.Fatalf("must not auto-publish pplim without operator pplim_release_w opt-in, got %q", p.Payload)
		}
	}
	// Counter metric should still be emitted (at 0) so dashboards can
	// distinguish "feature inactive" from "feature missing".
	checkMetricStuckPV(t, tel.FlushSamples(), "stuck_pv_recovery_count", 0)
}

// When the stuck signature clears (ipv goes non-zero), the counter
// stops advancing on subsequent polls — i.e. the detection logic
// actually checks the live signal, not just elapsed time.
func TestFerroampStuckPVWatchdogResetsWhenSignatureClears(t *testing.T) {
	tel := telemetry.NewStore()
	mqtt := &fakeMQTT{}
	d, env := newFerroampDriverWithConfigAndEnv(t, tel, mqtt, map[string]any{
		"pplim_release_w": 15000,
	})
	

	pushStuck := func() {
		mqtt.Push("extapi/data/sso",
			`{"id":{"val":"PS00990-A04-S23091622"},"upv":{"val":"500.0"},`+
				`"ipv":{"val":"0.0"},"relaystatus":{"val":"1"},`+
				`"faultcode":{"val":"0"},"udc":{"val":"499.0"}}`)
		mqtt.Push("extapi/data/ehub",
			`{"pext":{"L1":0,"L2":0,"L3":0},"ppv":{"val":0},`+
				`"pbat":{"val":0},"gridfreq":{"val":50.0},`+
				`"ul":{"L1":230,"L2":230,"L3":230},`+
				`"iext":{"L1":0,"L2":0,"L3":0}}`)
	}
	pushHealthy := func() {
		mqtt.Push("extapi/data/sso",
			`{"id":{"val":"PS00990-A04-S23091622"},"upv":{"val":"480.0"},`+
				`"ipv":{"val":"5.0"},"relaystatus":{"val":"1"},`+
				`"faultcode":{"val":"0"},"udc":{"val":"478.0"}}`)
		mqtt.Push("extapi/data/ehub",
			`{"pext":{"L1":-100,"L2":-100,"L3":-100},"ppv":{"val":2400},`+
				`"pbat":{"val":0},"gridfreq":{"val":50.0},`+
				`"ul":{"L1":230,"L2":230,"L3":230},`+
				`"iext":{"L1":-0.4,"L2":-0.4,"L3":-0.4}}`)
	}

	// Establish "stuck" + trigger one recovery so counter is at 1.
	pushStuck()
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll-1: %v", err)
	}
	env.Start = env.Start.Add(-11 * time.Minute)
	pushStuck()
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll-2: %v", err)
	}
	checkMetricStuckPV(t, tel.FlushSamples(), "stuck_pv_recovery_count", 1)

	// Signature clears (PV recovered). Counter must NOT advance even
	// after another long stretch — because stuck_pv_since_ms is reset.
	env.Start = env.Start.Add(-20 * time.Minute)
	pushHealthy()
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll-3: %v", err)
	}
	checkMetricStuckPV(t, tel.FlushSamples(), "stuck_pv_recovery_count", 1)
}

// payloadsAfter returns the publish payloads at indices >= mark in
// the mqtt fake's Published log. Mirrors the helper in
// lua_ferroamp_curtail_test.go (publishedSinceMark) but in a
// MQTTMessage shape for easier diff-printing.
func payloadsAfter(mqtt *fakeMQTT, mark int) []MQTTMessage {
	pubs := mqtt.Published()
	if mark >= len(pubs) {
		return nil
	}
	out := make([]MQTTMessage, len(pubs)-mark)
	copy(out, pubs[mark:])
	return out
}

// checkMetricStuckPV asserts that `samples` contains at least one
// MetricSample for `name` whose value equals `want` (most recent
// wins). Local copy so this test file is independent of PR #372,
// which lands the shared `checkMetric` helper in
// lua_ferroamp_curtail_test.go.
func checkMetricStuckPV(t *testing.T, samples []telemetry.MetricSample, name string, want float64) {
	t.Helper()
	var got float64
	found := false
	for _, s := range samples {
		if s.Metric == name {
			got = s.Value
			found = true
		}
	}
	if !found {
		t.Fatalf("metric %q not emitted in samples %+v", name, samples)
	}
	if got != want {
		t.Errorf("metric %q = %v, want %v", name, got, want)
	}
}
