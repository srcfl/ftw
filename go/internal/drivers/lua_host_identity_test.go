package drivers

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// The Blixt L1 host API is srcfl/device-drivers' naming reference. These
// four calls change gateway state, so a driver cannot supply them itself:
// set_model, set_rated_w and set_warmup_s land on the host's identity,
// decode_string replaces a byte loop a dozen drivers hand-roll.
//
// No PROTOCOL is declared in these drivers: with PROTOCOL = "modbus" the
// host buffers every emit until a fresh Modbus read commits it, and a
// test driver with no bus would look silent.

const identityDriverSrc = `
DRIVER = { name = "identity", version = "1.0.0" }
function driver_init(config)
    host.set_make("Blixt")
    host.set_model("L1-9000")
    host.set_sn("SN-ID-1")
    host.set_rated_w(9600)
    return true
end
function driver_poll()
    host.emit("meter", { w = 700.0 })
    return 1000
end
function driver_command(a, v, c) return true end
function driver_default_mode() end
`

func TestSetModelAndRatedWWithMakeAndSerial(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "identity.lua")
	if err := os.WriteFile(path, []byte(identityDriverSrc), 0644); err != nil {
		t.Fatal(err)
	}
	tel := telemetry.NewStore()
	env := NewHostEnv("identity", tel)
	d, err := NewLuaDriver(path, env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	t.Cleanup(d.Cleanup)
	if err := d.Init(context.Background(), map[string]any{}); err != nil {
		t.Fatalf("init: %v", err)
	}

	mk, sn := env.Identity()
	if mk != "Blixt" || sn != "SN-ID-1" {
		t.Fatalf("make/sn regressed: %q %q", mk, sn)
	}
	if got := env.DeviceModel(); got != "L1-9000" {
		t.Errorf("model: got %q, want L1-9000", got)
	}
	if got := env.RatedPowerW(); got != 9600 {
		t.Errorf("rated power: got %v, want 9600", got)
	}

	// The nameplate must reach the payload Nova reads, without the
	// driver repeating it inside every emit table.
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	r := tel.Get("identity", telemetry.DerMeter)
	if r == nil {
		t.Fatal("no meter reading")
	}
	var fields map[string]any
	if err := json.Unmarshal(r.Data, &fields); err != nil {
		t.Fatalf("emit blob is not json: %v", err)
	}
	if fields["model"] != "L1-9000" {
		t.Errorf("model did not reach the emit blob: %v", fields["model"])
	}
	if fields["rated_power_w"] != float64(9600) {
		t.Errorf("rated_power_w did not reach the emit blob: %v", fields["rated_power_w"])
	}
}

// A value the driver emitted itself is a live reading and must win over
// the nameplate cached since driver_init.
func TestEmittedValueWinsOverStampedNameplate(t *testing.T) {
	const src = `
DRIVER = { name = "stampwin", version = "1.0.0" }
function driver_init(config)
    host.set_model("FromInit")
    host.set_rated_w(1000)
    return true
end
function driver_poll()
    host.emit("meter", { w = 1.0, model = "FromPoll", rated_power_w = 2000 })
    return 1000
end
function driver_command(a, v, c) return true end
function driver_default_mode() end
`
	tel := loadAndPoll(t, "stampwin", src, nil)
	r := tel.Get("stampwin", telemetry.DerMeter)
	if r == nil {
		t.Fatal("no meter reading")
	}
	var fields map[string]any
	if err := json.Unmarshal(r.Data, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["model"] != "FromPoll" {
		t.Errorf("model: got %v, want FromPoll", fields["model"])
	}
	if fields["rated_power_w"] != float64(2000) {
		t.Errorf("rated_power_w: got %v, want 2000", fields["rated_power_w"])
	}
}

// A host with no nameplate must leave the emit blob exactly as it was.
func TestNoNameplateLeavesEmitBlobAlone(t *testing.T) {
	const src = `
DRIVER = { name = "bare", version = "1.0.0" }
function driver_init(config) return true end
function driver_poll() host.emit("meter", { w = 5.0 }) return 1000 end
function driver_command(a, v, c) return true end
function driver_default_mode() end
`
	tel := loadAndPoll(t, "bare", src, nil)
	r := tel.Get("bare", telemetry.DerMeter)
	if r == nil {
		t.Fatal("no meter reading")
	}
	var fields map[string]any
	if err := json.Unmarshal(r.Data, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["model"]; ok {
		t.Error("model key invented for a driver that set none")
	}
	if _, ok := fields["rated_power_w"]; ok {
		t.Error("rated_power_w key invented for a driver that set none")
	}
}

func TestSetWarmupSDelaysTheFirstPoll(t *testing.T) {
	const src = `
DRIVER = { name = "warmup", version = "1.0.0" }
function driver_init(config)
    host.set_poll_interval(1000)
    host.set_warmup_s(30)
    return true
end
function driver_poll() host.emit("meter", { w = 0 }) return 1000 end
function driver_command(a, v, c) return true end
function driver_default_mode() end
`
	dir := t.TempDir()
	path := filepath.Join(dir, "warmup.lua")
	if err := os.WriteFile(path, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	env := NewHostEnv("warmup", telemetry.NewStore())
	d, err := NewLuaDriver(path, env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	t.Cleanup(d.Cleanup)
	if err := d.Init(context.Background(), map[string]any{}); err != nil {
		t.Fatalf("init: %v", err)
	}

	if got := env.Warmup(); got != 30*time.Second {
		t.Fatalf("warmup: got %v, want 30s", got)
	}
	if got := env.PollInterval(); got != time.Second {
		t.Fatalf("poll interval: got %v, want 1s", got)
	}
	// The run loop arms its first timer from FirstPollDelay. Time
	// already spent in driver_init counts against the window, so assert
	// a range rather than exactly 30s.
	first := env.FirstPollDelay()
	if first <= 25*time.Second || first > 30*time.Second {
		t.Errorf("first poll delay: got %v, want just under 30s", first)
	}
}

// Without a warmup, and with one shorter than the cadence, the first
// poll must still happen at the normal interval.
func TestFirstPollDelayFallsBackToPollInterval(t *testing.T) {
	env := NewHostEnv("cadence", telemetry.NewStore())
	env.setPollInterval(2000)
	if got := env.FirstPollDelay(); got != 2*time.Second {
		t.Errorf("no warmup: got %v, want 2s", got)
	}
	env.setWarmupS(1)
	if got := env.FirstPollDelay(); got != 2*time.Second {
		t.Errorf("short warmup: got %v, want the 2s cadence", got)
	}
	env.setWarmupS(0)
	if got := env.Warmup(); got != 0 {
		t.Errorf("cleared warmup: got %v, want 0", got)
	}
}

func TestDecodeString(t *testing.T) {
	const src = `
DRIVER = { name = "decode", version = "1.0.0" }
RESULTS = {}
function driver_init(config)
    -- "FTW" then "SN-42" padded with NUL and spaces, as a vendor would.
    local regs = { 0x4654, 0x5700, 0x534E, 0x2D34, 0x3220, 0x0000 }
    RESULTS.whole    = host.decode_string(regs)
    RESULTS.head     = host.decode_string(regs, 1, 2)
    RESULTS.tail     = host.decode_string(regs, 3, 4)
    RESULTS.one      = host.decode_string(regs, 1, 1)
    RESULTS.past_end = host.decode_string(regs, 3, 99)
    RESULTS.empty    = host.decode_string({ 0x0000, 0x2020 })
    host.set_sn(RESULTS.tail)
    return true
end
function driver_poll() host.emit("meter", { w = 0 }) return 1000 end
function driver_command(a, v, c) return true end
function driver_default_mode() end
`
	dir := t.TempDir()
	path := filepath.Join(dir, "decode.lua")
	if err := os.WriteFile(path, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	env := NewHostEnv("decode", telemetry.NewStore())
	d, err := NewLuaDriver(path, env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	t.Cleanup(d.Cleanup)
	if err := d.Init(context.Background(), map[string]any{}); err != nil {
		t.Fatalf("init: %v", err)
	}

	results := d.L.GetGlobal("RESULTS")
	get := func(key string) string {
		return d.L.GetField(results, key).String()
	}
	for _, tc := range []struct{ key, want string }{
		{"whole", "FTW\x00SN-42"}, // interior NUL is data, only the tail is padding
		{"head", "FTW"},           // trailing NUL stripped
		{"tail", "SN-42"},         // 1-based start, trailing space stripped
		{"one", "FT"},             // two characters per register
		{"past_end", "SN-42"},     // short table decodes what is there
		{"empty", ""},             // all padding decodes to nothing
	} {
		if got := get(tc.key); got != tc.want {
			t.Errorf("decode_string %s: got %q, want %q", tc.key, got, tc.want)
		}
	}
	if _, sn := env.Identity(); sn != "SN-42" {
		t.Errorf("decoded serial did not reach the host: %q", sn)
	}
}
