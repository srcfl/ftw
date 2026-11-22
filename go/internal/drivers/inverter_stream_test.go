package drivers

import (
	"context"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// The Solis C&I drivers (50-125k-svk*) send the battery's AC stage on the
// inverter stream right after the battery reading. The host keeps it, and
// it reaches neither the DER readings nor history.
func TestHostAcceptsTheInverterStream(t *testing.T) {
	path := writeLua(t, `
function driver_init(config) end
function driver_poll()
    host.emit("battery", { W = 3600, SoC_nom_fract = 0.5, available_charge_W = 5000 })
    local err = host.emit("inverter", { W = 4000, available_import_W = 0, available_export_W = 5200, Hz = 50.01 })
    assert(err == nil, "inverter emit refused: " .. tostring(err))
    return 1000
end
`)
	tel := telemetry.NewStore()
	env := NewHostEnv("solis", tel)
	env.BatteryCapacityWh = 100000
	d, err := NewLuaDriver(path, env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer d.Cleanup()
	if err := d.Init(context.Background(), nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	tel.FlushSamples()
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	inv, ok := tel.LiveBatteryInverter("solis")
	if !ok || inv.W == nil || *inv.W != 4000 || inv.AvailableImportW == nil || *inv.AvailableImportW != 0 ||
		inv.AvailableExportW == nil || *inv.AvailableExportW != 5200 || !strings.Contains(string(inv.Data), `"Hz":50.01`) {
		t.Fatalf("inverter reading %+v %v", inv, ok)
	}
	if bat := tel.Get("solis", telemetry.DerBattery); bat == nil || bat.RawW != 3600 {
		t.Fatalf("battery reading %+v", bat)
	}
	for _, kind := range telemetry.AllDerTypes() {
		if kind != telemetry.DerBattery && tel.Get("solis", kind) != nil {
			t.Fatalf("the inverter stream became a %s reading", kind)
		}
	}
	for _, sample := range tel.FlushSamples() {
		if strings.HasPrefix(sample.Metric, "inverter") {
			t.Fatalf("the inverter stream reached history: %+v", sample)
		}
	}
}

// An inverter emit alone is no sign of life, so it never keeps a driver's
// held battery reading in dispatch. A poll whose reads failed drops it with
// the rest of the poll.
func TestInverterStreamFollowsThePollRules(t *testing.T) {
	tel := telemetry.NewStore()
	env := NewHostEnv("solis", tel)
	if err := env.emitTelemetry([]byte(`{"type":"inverter","W":4000,"available_import_W":0}`)); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if h := tel.DriverHealth("solis"); h != nil && h.LastSuccess != nil {
		t.Fatalf("an inverter emit counted as a sign of life: %+v", h)
	}
	if _, ok := tel.Inverter("solis"); !ok {
		t.Fatal("inverter reading not kept")
	}
	// One bad field is left out; the rest of the emit is kept.
	if err := env.emitTelemetry([]byte(`{"type":"inverter","W":"high","available_import_W":0}`)); err != nil {
		t.Fatalf("one bad field failed the emit: %v", err)
	}
	if r, _ := tel.Inverter("solis"); r.W != nil || r.AvailableImportW == nil || *r.AvailableImportW != 0 {
		t.Fatalf("bad power field: %+v", r)
	}

	tel = telemetry.NewStore()
	env = NewHostEnv("solis", tel)
	env.requiresFreshModbusRead = true
	env.beginPollEvidence()
	if err := env.emitTelemetry([]byte(`{"type":"inverter","W":4000,"available_import_W":0}`)); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if _, _, err := env.endPollEvidence(false); err != nil {
		t.Fatalf("end poll: %v", err)
	}
	if _, ok := tel.Inverter("solis"); ok {
		t.Fatal("an inverter reading from a failed poll was kept")
	}

	// A good poll lands both, held until it ends, as one poll.
	env.BatteryCapacityWh = 100000
	env.beginPollEvidence()
	env.recordPollModbusRead(nil)
	for _, blob := range []string{`{"type":"battery","W":3600,"SoC_nom_fract":0.5}`, `{"type":"inverter","W":4000,"available_import_W":0}`} {
		if err := env.emitTelemetry([]byte(blob)); err != nil {
			t.Fatalf("emit: %v", err)
		}
	}
	if _, ok := tel.Inverter("solis"); ok {
		t.Fatal("stored before the poll ended")
	}
	if _, _, err := env.endPollEvidence(true); err != nil {
		t.Fatalf("end poll: %v", err)
	}
	if inv, ok := tel.LiveBatteryInverter("solis"); !ok || inv.Poll == 0 {
		t.Fatalf("held poll did not pair: %+v %v", inv, ok)
	}
}

// A driver polled back to back that reads its inverter every other poll:
// the battery reading pairs with headroom only on the polls that read it,
// however close in time the polls are. A failed register (NaN) is left out
// and the rest of the emit kept.
func TestInverterStreamPairsByPoll(t *testing.T) {
	path := writeLua(t, `
local n = 0
function driver_init(config) end
function driver_poll()
    n = n + 1
    host.emit("battery", { W = 3600, SoC_nom_fract = 0.5 })
    if n % 2 == 0 then
        local err = host.emit("inverter", { W = 0/0, available_import_W = 0, rated_W = 1/0 })
        assert(err == nil, "inverter emit refused: " .. tostring(err))
    end
    return 1000
end
`)
	tel := telemetry.NewStore()
	env := NewHostEnv("solis", tel)
	env.BatteryCapacityWh = 100000
	d, err := NewLuaDriver(path, env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer d.Cleanup()
	if err := d.Init(context.Background(), nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	for n := 1; n <= 4; n++ {
		if _, err := d.Poll(context.Background()); err != nil {
			t.Fatalf("poll %d: %v", n, err)
		}
		inv, ok := tel.LiveBatteryInverter("solis")
		if ok != (n%2 == 0) {
			t.Fatalf("poll %d: paired %v", n, ok)
		}
		if ok && (inv.W != nil || inv.AvailableImportW == nil || *inv.AvailableImportW != 0 || strings.Contains(string(inv.Data), "rated_W")) {
			t.Fatalf("poll %d: %+v %s", n, inv, inv.Data)
		}
	}
}
