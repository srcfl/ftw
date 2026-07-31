package drivers

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// srcfl/device-drivers is converting its catalog to the canonical
// @srcful/data-models emit keys and the Blixt L1 host function names, one
// driver at a time. A converted driver must run here before the older
// spellings are retired, otherwise a site loses telemetry the moment its
// driver is updated.

func loadAndPoll(t *testing.T, name, src string, prep func(*HostEnv)) *telemetry.Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name+".lua")
	if err := os.WriteFile(path, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	tel := telemetry.NewStore()
	env := NewHostEnv(name, tel)
	if prep != nil {
		prep(env)
	}
	d, err := NewLuaDriver(path, env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	t.Cleanup(d.Cleanup)
	if err := d.Init(context.Background(), map[string]any{}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	return tel
}

const canonicalDriverSrc = `
DRIVER = { name = "canonical", version = "1.0.0" }
function driver_init(config)
    host.set_make("CanonicalMaker")
    host.set_sn("SN-CANON")
    return true
end
function driver_poll()
    -- Canonical spellings only: no lowercase w, no soc.
    host.emit("meter", { W = 1234.0, Hz = 50.0, total_import_Wh = 42.0 })
    host.emit("battery", { W = -500.0, SoC_nom_fract = 0.87 })
    return 1000
end
function driver_command(action, value, ctx) return true end
function driver_default_mode() end
`

func TestCanonicalEmitKeysAreAccepted(t *testing.T) {
	tel := loadAndPoll(t, "canonical", canonicalDriverSrc, func(env *HostEnv) {
		env.BatteryCapacityWh = 9600
	})

	meter := tel.Get("canonical", telemetry.DerMeter)
	if meter == nil {
		t.Fatal("no meter reading: canonical W was not read")
	}
	if meter.RawW != 1234.0 {
		t.Errorf("meter W: got %v, want 1234", meter.RawW)
	}

	battery := tel.Get("canonical", telemetry.DerBattery)
	if battery == nil {
		t.Fatal("no battery reading: canonical W was not read")
	}
	if battery.RawW != -500.0 {
		t.Errorf("battery W: got %v, want -500", battery.RawW)
	}
	if battery.SoC == nil || *battery.SoC != 0.87 {
		t.Errorf("battery SoC: got %v, want 0.87 from SoC_nom_fract", battery.SoC)
	}
}

// The lowercase spelling must keep working unchanged. The point of accepting
// both is that neither side has to move first.
func TestLowercaseEmitKeysStillWork(t *testing.T) {
	const legacy = `
DRIVER = { name = "legacy", version = "1.0.0" }
function driver_init(config) host.set_make("Legacy") return true end
function driver_poll() host.emit("meter", { w = 99.0 }) return 1000 end
function driver_command(a, v, c) return true end
function driver_default_mode() end
`
	tel := loadAndPoll(t, "legacy", legacy, nil)
	got := tel.Get("legacy", telemetry.DerMeter)
	if got == nil || got.RawW != 99.0 {
		t.Errorf("lowercase w regressed: %+v", got)
	}
}

// A driver that sets both must not have the canonical value silently win over
// the one the driver actually meant as primary.
func TestLowercaseWinsWhenBothSpellingsArePresent(t *testing.T) {
	const both = `
DRIVER = { name = "both", version = "1.0.0" }
function driver_init(config) host.set_make("Both") return true end
function driver_poll() host.emit("meter", { w = 10.0, W = 20.0 }) return 1000 end
function driver_command(a, v, c) return true end
function driver_default_mode() end
`
	tel := loadAndPoll(t, "both", both, nil)
	got := tel.Get("both", telemetry.DerMeter)
	if got == nil || got.RawW != 10.0 {
		t.Errorf("lowercase w should win when both are set, got %+v", got)
	}
}

// write, write_registers and now_ms are the Blixt spellings of modbus_write,
// modbus_write_multi and millis. Both names must resolve to a function.
// No protocol is declared: this asserts the names exist, not that they write.
func TestCanonicalHostFunctionAliasesExist(t *testing.T) {
	const probe = `
DRIVER = { name = "probe", version = "1.0.0" }
function driver_init(config)
    local missing = {}
    for _, name in ipairs({"write", "write_registers", "now_ms",
                           "modbus_write", "modbus_write_multi", "millis"}) do
        if type(host[name]) ~= "function" then
            missing[#missing + 1] = name
        end
    end
    if #missing > 0 then
        error("host is missing: " .. table.concat(missing, ", "))
    end
    host.set_make("Probe")
    return true
end
function driver_poll()
    -- now_ms must return the same kind of value millis does.
    local a = host.now_ms()
    local b = host.millis()
    if type(a) ~= "number" or type(b) ~= "number" then
        error("now_ms/millis did not return a number")
    end
    host.emit("meter", { w = 0 })
    return 1000
end
function driver_command(a, v, c) return true end
function driver_default_mode() end
`
	loadAndPoll(t, "probe", probe, nil)
}
