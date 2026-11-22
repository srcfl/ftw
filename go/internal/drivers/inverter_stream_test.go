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
	if err := env.emitTelemetry([]byte(`{"type":"inverter","W":"high"}`)); err == nil {
		t.Fatal("a malformed inverter emit was accepted")
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
}
