package telemetry

import (
	"reflect"
	"testing"
	"testing/synctest"
	"time"
)

func inverterOf(t *testing.T, data string) InverterReading {
	t.Helper()
	r, err := ParseInverterReading([]byte(data))
	if err != nil {
		t.Fatalf("parse %s: %v", data, err)
	}
	return r
}

func TestParseInverterReadingKeepsWhatTheDeviceReports(t *testing.T) {
	r := inverterOf(t, `{"W":-4200,"available_import_W":0,"available_export_W":3800,"Hz":50.01}`)
	if r.W == nil || *r.W != -4200 || r.AvailableImportW == nil || *r.AvailableImportW != 0 ||
		r.AvailableExportW == nil || *r.AvailableExportW != 3800 || string(r.Data) == "" {
		t.Fatalf("canonical names: %+v", r)
	}
	if r := inverterOf(t, `{"w":1200}`); r.W == nil || *r.W != 1200 || r.AvailableImportW != nil || r.AvailableExportW != nil {
		t.Fatalf("catalog spelling: %+v", r)
	}
	// The driver guide drops a power outside 0 to 100 kW; a missing field
	// stays missing, never 0.
	for _, data := range []string{
		`{"W":0,"available_import_W":-50,"available_export_W":100001}`,
		`{"W":0}`,
		`{"W":0,"available_import_W":null}`,
		`{"W":0,"available_import_W":"0"}`,
	} {
		if r, err := ParseInverterReading([]byte(data)); err == nil && (r.AvailableImportW != nil || r.AvailableExportW != nil) {
			t.Fatalf("%s kept headroom %+v", data, r)
		}
	}
	if _, err := ParseInverterReading([]byte(`[1]`)); err == nil {
		t.Fatal("not an object, accepted")
	}
}

// The inverter stream is the AC side of energy the battery and PV readings
// already carry. Storing it must change nothing that sums site power: the
// live and history balance, the per-type reads dispatch uses, and the
// samples that reach history.
func TestInverterReadingNeverChangesSitePower(t *testing.T) {
	seed := func() *Store {
		s := NewStore()
		for _, d := range []string{"meter", "hybrid"} {
			s.EnsureDriverHealth(d)
		}
		s.Update("meter", DerMeter, 1800, nil, []byte(`{"power_origin":"external_meter"}`))
		soc := 0.5
		s.Update("hybrid", DerBattery, 3600, &soc, nil)
		s.Update("hybrid", DerPV, -2400, nil, nil)
		s.RecordDriverSuccess("meter")
		s.RecordDriverSuccess("hybrid")
		s.FlushSamples()
		return s
	}
	plain, with := seed(), seed()
	with.UpdateInverter("hybrid", inverterOf(t, `{"W":-9000,"available_import_W":0,"available_export_W":0,"L1_W":-3000}`))

	opts := ForecastOptions{}
	if a, b := plain.LiveMeasurementNow("meter", opts), with.LiveMeasurementNow("meter", opts); a.LoadW != b.LoadW || a.PVW != b.PVW ||
		a.LoadKnown != b.LoadKnown || len(a.Sources) != len(b.Sources) || a.Reason != b.Reason {
		t.Fatalf("live balance moved: %+v vs %+v", a, b)
	}
	if a, b := plain.ForecastMeasurementNow("meter", opts), with.ForecastMeasurementNow("meter", opts); a.Valid != b.Valid ||
		a.LoadW != b.LoadW || len(a.Sources) != len(b.Sources) {
		t.Fatalf("history balance moved: %+v vs %+v", a, b)
	}
	for _, kind := range AllDerTypes() {
		if a, b := plain.OnlineRawWByType(kind), with.OnlineRawWByType(kind); !reflect.DeepEqual(a, b) {
			t.Fatalf("%s readings moved: %v vs %v", kind, a, b)
		}
		if a, b := len(plain.ReadingsByType(kind)), len(with.ReadingsByType(kind)); a != b {
			t.Fatalf("%s reading count %d vs %d", kind, a, b)
		}
	}
	if a, b := len(plain.ReadingsByDriver("hybrid")), len(with.ReadingsByDriver("hybrid")); a != b {
		t.Fatalf("driver readings %d vs %d", a, b)
	}
	if a, b := plain.SumOnlineHigherW(DerBattery, DerPV, DerEV, DerV2X), with.SumOnlineHigherW(DerBattery, DerPV, DerEV, DerV2X); a != b {
		t.Fatalf("site sum %v vs %v", a, b)
	}
	if got := with.FlushSamples(); len(got) != 0 {
		t.Fatalf("the inverter reading reached history: %+v", got)
	}
}

func TestLiveBatteryInverterComesWithTheBatteryReading(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		soc := 0.5
		poll := func(s *Store, inverter bool) {
			s.Update("hybrid", DerBattery, 3600, &soc, nil)
			if inverter {
				s.UpdateInverter("hybrid", inverterOf(t, `{"W":4000,"available_import_W":0}`))
			}
			s.RecordDriverSuccess("hybrid")
		}
		s := NewStore()
		if _, ok := s.LiveBatteryInverter("hybrid"); ok {
			t.Fatal("nothing emitted, yet live")
		}
		s.UpdateInverter("hybrid", inverterOf(t, `{"W":4000,"available_import_W":0}`))
		s.RecordDriverSuccess("hybrid")
		if _, ok := s.LiveBatteryInverter("hybrid"); ok {
			t.Fatal("live without a battery reading")
		}
		poll(s, true)
		r, ok := s.LiveBatteryInverter("hybrid")
		if !ok || r.AvailableImportW == nil || *r.AvailableImportW != 0 || r.Driver != "hybrid" {
			t.Fatalf("same poll: %+v %v", r, ok)
		}
		if got, ok := s.Inverter("hybrid"); !ok || got.UpdatedAt != r.UpdatedAt {
			t.Fatalf("latest reading %+v %v", got, ok)
		}

		// The next poll's inverter read fails: the battery reading moves on
		// and the old headroom no longer counts.
		time.Sleep(5 * time.Second)
		poll(s, false)
		if _, ok := s.LiveBatteryInverter("hybrid"); ok {
			t.Fatal("an inverter reading from an earlier poll counted")
		}
		if _, ok := s.Inverter("hybrid"); !ok {
			t.Fatal("the last known reading went missing from the view")
		}
		// The battery read fails while the inverter read answers: no pair.
		time.Sleep(5 * time.Second)
		s.UpdateInverter("hybrid", inverterOf(t, `{"W":4000,"available_import_W":0}`))
		if _, ok := s.LiveBatteryInverter("hybrid"); ok {
			t.Fatal("an inverter reading without this poll's battery reading counted")
		}

		poll(s, true)
		s.SetDriverDeviceFault("hybrid", true, "test")
		if _, ok := s.LiveBatteryInverter("hybrid"); ok {
			t.Fatal("live while the device reports a fault")
		}
		s.SetDriverDeviceFault("hybrid", false, "")
		poll(s, true)
		s.DriverHealthMut("hybrid").SetOffline()
		if _, ok := s.LiveBatteryInverter("hybrid"); ok {
			t.Fatal("live while the driver is offline")
		}
		s.Remove("hybrid")
		if _, ok := s.Inverter("hybrid"); ok {
			t.Fatal("a removed driver kept its inverter reading")
		}
	})
}
