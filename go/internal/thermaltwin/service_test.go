package thermaltwin

import (
	"strings"
	"testing"
	"time"
)

type metric struct {
	v  float64
	at time.Time
}

type fakeTele map[string]metric

func (f fakeTele) LatestMetric(driver, name string) (float64, time.Time, bool) {
	m, ok := f[driver+"/"+name]
	return m.v, m.at, ok
}

type fakeStore map[string]string

func (f fakeStore) LoadConfig(key string) (string, bool) { v, ok := f[key]; return v, ok }
func (f fakeStore) SaveConfig(key, value string) error   { f[key] = value; return nil }

var t0 = time.Date(2026, 11, 10, 6, 0, 0, 0, time.UTC)

var livingRoom = Zone{Key: "wiser/room_temp_c", SensorDriver: "wiser", SensorMetric: "room_temp_c"}

func newTestService(tele fakeTele, store fakeStore, zones ...Zone) *Service {
	s := NewService(store, tele)
	s.Temp = func(time.Time) (float64, bool) { return 0, true }
	s.Zones = func() []Zone { return zones }
	s.PowerDrivers = func() []string { return []string{"wiser", "heishamon", "nibe"} }
	return s
}

// run ticks once a minute for the given minutes, letting update set this
// minute's readings first.
func run(s *Service, from time.Time, minutes int, update func(now time.Time)) time.Time {
	now := from
	for i := 0; i < minutes; i++ {
		update(now)
		s.Tick(now)
		now = now.Add(time.Minute)
	}
	return now
}

func zoneStatus(t *testing.T, s *Service, key string) ZoneStatus {
	t.Helper()
	for _, z := range s.Snapshot() {
		if z.Key == key {
			return z
		}
	}
	t.Fatalf("zone %s not in snapshot %+v", key, s.Snapshot())
	return ZoneStatus{}
}

func TestServiceLearnsAndPersistsAZone(t *testing.T) {
	tele, store := fakeTele{}, fakeStore{}
	s := newTestService(tele, store, livingRoom)
	h := &house{a: 1.0 / 40, b: 1.0 / 15, c: 0.05, tIn: 20}

	// Three whole windows plus the minute that closes the third.
	run(s, t0, 3*30+1, func(now time.Time) {
		h.step(0, 2, 1.0/60)
		tele["wiser/room_temp_c"] = metric{h.tIn, now}
		tele["heishamon/hp_power_w"] = metric{2000, now}
	})

	z := zoneStatus(t, s, livingRoom.Key)
	if z.Samples != 3 || z.Rejected != 0 {
		t.Fatalf("samples = %d, rejected = %d, want 3 and 0", z.Samples, z.Rejected)
	}
	if z.PowerSource != "heishamon" {
		t.Errorf("power source = %q, want the single driver reporting %s", z.PowerSource, HeatPowerMetric)
	}
	if z.Status != StatusLearning {
		t.Errorf("status = %s, want learning", z.Status)
	}

	js, ok := store[stateKeyRoot+livingRoom.Key]
	if !ok || !strings.Contains(js, FeatureHash()) {
		t.Fatalf("persisted state = %q", js)
	}
	restored := newTestService(fakeTele{}, store, livingRoom)
	restored.Tick(t0.Add(10 * time.Hour))
	if got := zoneStatus(t, restored, livingRoom.Key); got.Samples != 3 || got.PowerSource != "heishamon" {
		t.Errorf("restored = %+v, want the persisted twin", got)
	}
}

// State fitted to another form of the model cold starts instead of
// restoring coefficients that mean something else.
func TestServiceColdStartsOnForeignState(t *testing.T) {
	store := fakeStore{stateKeyRoot + livingRoom.Key: `{"schema_version":1,"feature_hash":"0000000000000000","model":{"samples":999}}`}
	s := newTestService(fakeTele{}, store, livingRoom)
	s.Tick(t0)
	if got := zoneStatus(t, s, livingRoom.Key); got.Samples != 0 {
		t.Errorf("samples = %d, want a cold start", got.Samples)
	}
}

// A sensor that stops reporting for part of a window yields no sample: an
// old reading is not the zone now.
func TestServiceSkipsAWindowWithAStaleSensor(t *testing.T) {
	tele := fakeTele{}
	s := newTestService(tele, fakeStore{}, livingRoom)
	run(s, t0, 31, func(now time.Time) {
		at := now
		if now.Sub(t0) >= 10*time.Minute {
			at = t0.Add(10 * time.Minute) // frozen from minute ten
		}
		tele["wiser/room_temp_c"] = metric{20, at}
		tele["heishamon/hp_power_w"] = metric{1500, now}
	})
	if got := zoneStatus(t, s, livingRoom.Key); got.Samples != 0 {
		t.Errorf("samples = %d, want 0 for a window with a stale sensor", got.Samples)
	}
}

// Two drivers reporting heat power and neither the sensor's own: the twin
// cannot tell which heats this zone, so it learns without heat input and
// says so, rather than guess.
func TestServiceRefusesAnAmbiguousPowerSource(t *testing.T) {
	tele := fakeTele{}
	s := newTestService(tele, fakeStore{}, livingRoom)
	run(s, t0, 31, func(now time.Time) {
		tele["wiser/room_temp_c"] = metric{20, now}
		tele["heishamon/hp_power_w"] = metric{1500, now}
		tele["nibe/hp_power_w"] = metric{900, now}
	})
	z := zoneStatus(t, s, livingRoom.Key)
	if z.PowerSource != "" || z.Status != StatusNoHeatInput {
		t.Errorf("zone = %+v, want no power source and no_heat_input", z)
	}
}

// The sensor's own driver reporting heat power wins over any other.
func TestServicePrefersTheSensorsOwnPower(t *testing.T) {
	tele := fakeTele{}
	pump := Zone{Key: "nibe/hp_indoor_temp_c", SensorDriver: "nibe", SensorMetric: "hp_indoor_temp_c"}
	s := newTestService(tele, fakeStore{}, pump)
	run(s, t0, 31, func(now time.Time) {
		tele["nibe/hp_indoor_temp_c"] = metric{20, now}
		tele["nibe/hp_power_w"] = metric{900, now}
		tele["heishamon/hp_power_w"] = metric{1500, now}
	})
	if z := zoneStatus(t, s, pump.Key); z.PowerSource != "nibe" {
		t.Errorf("power source = %q, want nibe", z.PowerSource)
	}
}

func TestServiceDropsARemovedZone(t *testing.T) {
	tele := fakeTele{}
	zones := []Zone{livingRoom}
	s := NewService(fakeStore{}, tele)
	s.Temp = func(time.Time) (float64, bool) { return 0, true }
	s.Zones = func() []Zone { return zones }
	s.Tick(t0)
	if len(s.Snapshot()) != 1 {
		t.Fatalf("snapshot = %+v", s.Snapshot())
	}
	zones = nil
	s.Tick(t0.Add(Window))
	if got := s.Snapshot(); len(got) != 0 {
		t.Errorf("snapshot = %+v after the zone was removed", got)
	}
}

// Without an outdoor temperature a window teaches nothing true, so it is
// skipped rather than learned against a guessed zero.
func TestServiceNeedsOutdoorTemperature(t *testing.T) {
	tele := fakeTele{}
	s := newTestService(tele, fakeStore{}, livingRoom)
	s.Temp = func(time.Time) (float64, bool) { return 0, false }
	run(s, t0, 31, func(now time.Time) {
		tele["wiser/room_temp_c"] = metric{20, now}
		tele["heishamon/hp_power_w"] = metric{1500, now}
	})
	if got := zoneStatus(t, s, livingRoom.Key); got.Samples != 0 {
		t.Errorf("samples = %d without outdoor temperature, want 0", got.Samples)
	}
}
