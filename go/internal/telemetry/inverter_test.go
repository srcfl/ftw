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
	// The driver guide drops a power outside 0 to 100 kW. A missing,
	// null or misspelt field stays missing, never 0, and Blixt's exact
	// names are the only ones read.
	for _, data := range []string{
		`{"W":0,"available_import_W":-50,"available_export_W":100001}`,
		`{"W":0}`,
		`{"W":0,"available_import_W":null,"available_export_W":null}`,
		`{"W":0,"available_import_W":"0","available_export_W":true}`,
		`{"W":0,"available_import_w":0,"Available_Export_W":0}`,
		`{"W":0,"available_import_W":1e999}`,
	} {
		if r := inverterOf(t, data); r.AvailableImportW != nil || r.AvailableExportW != nil {
			t.Fatalf("%s kept headroom %+v", data, r)
		}
	}
	// One bad field never costs the others.
	for _, data := range []string{`{"W":"high","available_import_W":0}`, `{"W":1e999,"available_import_W":0}`, `{"W":null,"available_import_W":0}`} {
		if r := inverterOf(t, data); r.W != nil || r.AvailableImportW == nil || *r.AvailableImportW != 0 {
			t.Fatalf("%s: %+v", data, r)
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
	with.UpdateInverter("hybrid", inverterOf(t, `{"W":-9000,"available_import_W":0,"available_export_W":0,"L1_W":-3000}`), 1)

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

// The pair is the driver poll, not a time window: a driver polled every
// second whose inverter read fails leaves a reading a second old, and that
// one never pairs with the new battery reading.
func TestLiveBatteryInverterComesWithTheBatteryReading(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		soc := 0.5
		headroom := inverterOf(t, `{"W":4000,"available_import_W":0}`)
		poll := func(s *Store, n uint64, battery, inverter bool) {
			time.Sleep(time.Second)
			if battery {
				s.UpdateFromPoll("hybrid", DerBattery, 3600, &soc, nil, n)
			}
			if inverter {
				s.UpdateInverter("hybrid", headroom, n)
			}
			s.RecordDriverSuccess("hybrid")
		}
		s := NewStore()
		if _, ok := s.LiveBatteryInverter("hybrid"); ok {
			t.Fatal("nothing emitted, yet live")
		}
		poll(s, 1, false, true)
		if _, ok := s.LiveBatteryInverter("hybrid"); ok {
			t.Fatal("live without a battery reading")
		}
		if _, ok := s.LiveInverter("hybrid"); !ok {
			t.Fatal("the view lost a live driver's reading")
		}
		poll(s, 2, true, true)
		r, ok := s.LiveBatteryInverter("hybrid")
		if !ok || r.AvailableImportW == nil || *r.AvailableImportW != 0 || r.Driver != "hybrid" || r.Poll != 2 {
			t.Fatalf("same poll: %+v %v", r, ok)
		}
		// The next poll's inverter read fails, a second later.
		poll(s, 3, true, false)
		if _, ok := s.LiveBatteryInverter("hybrid"); ok {
			t.Fatal("an inverter reading from an earlier poll counted")
		}
		if _, ok := s.LiveInverter("hybrid"); !ok {
			t.Fatal("the last known reading went missing from the view")
		}
		// The battery read fails while the inverter read answers.
		poll(s, 4, false, true)
		if _, ok := s.LiveBatteryInverter("hybrid"); ok {
			t.Fatal("an inverter reading without this poll's battery reading counted")
		}
		// A driver that reads its inverter every other poll pairs on those
		// polls only.
		for n := uint64(5); n < 11; n++ {
			poll(s, n, true, n%2 == 0)
			if _, ok := s.LiveBatteryInverter("hybrid"); ok != (n%2 == 0) {
				t.Fatalf("poll %d: paired %v", n, ok)
			}
		}
		// Readings from outside a poll never pair.
		poll(s, 0, true, true)
		if _, ok := s.LiveBatteryInverter("hybrid"); ok {
			t.Fatal("readings from outside a poll paired")
		}

		poll(s, 12, true, true)
		s.SetDriverDeviceFault("hybrid", true, "test")
		if _, ok := s.LiveBatteryInverter("hybrid"); ok {
			t.Fatal("live while the device reports a fault")
		}
		if _, ok := s.LiveInverter("hybrid"); ok {
			t.Fatal("the view shows a faulted device's reading as live")
		}
		s.SetDriverDeviceFault("hybrid", false, "")
		poll(s, 13, true, true)
		s.DriverHealthMut("hybrid").SetOffline()
		if _, ok := s.LiveBatteryInverter("hybrid"); ok {
			t.Fatal("live while the driver is offline")
		}
		if _, ok := s.LiveInverter("hybrid"); ok {
			t.Fatal("the view shows an offline driver's reading as live")
		}
		if _, ok := s.Inverter("hybrid"); !ok {
			t.Fatal("the last known reading went missing")
		}
		s.Remove("hybrid")
		if _, ok := s.Inverter("hybrid"); ok {
			t.Fatal("a removed driver kept its inverter reading")
		}
	})
}
