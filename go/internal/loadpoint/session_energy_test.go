package loadpoint

import (
	"math"
	"testing"
	"time"
)

func TestSessionEnergyDelayedCounterDoesNotDoubleCount(t *testing.T) {
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	e := &sessionEnergy{}
	for sec := 0; sec <= 120; sec += 5 {
		at := start.Add(time.Duration(sec) * time.Second)
		sample := EVSample{PowerW: 6900, PowerAt: at, SessionWh: 1000, EnergyAt: start}
		if sec >= 110 {
			sample.SessionWh = 1000 + 6900*100.0/3600
			sample.EnergyAt = start.Add(100 * time.Second)
		}
		got := e.observe(sample, at)
		want := 1000 + 6900*float64(sec)/3600
		if math.Abs(got-want) > 1e-8 {
			t.Fatalf("t=%ds got %.6f Wh want %.6f", sec, got, want)
		}
	}
}

func TestSessionEnergyCachedPowerAndGapsDoNotInventCharge(t *testing.T) {
	at := time.Now()
	e := &sessionEnergy{}
	s := EVSample{PowerW: 6900, PowerAt: at, SessionWh: 1000, EnergyAt: at}
	e.observe(s, at)
	for sec := 5; sec <= 120; sec += 5 {
		if got := e.observe(s, at.Add(time.Duration(sec)*time.Second)); got != 1000 {
			t.Fatalf("cached power added energy: %v", got)
		}
	}
	s.PowerAt = at.Add(125 * time.Second)
	if got := e.observe(s, s.PowerAt); got != 1000 {
		t.Fatalf("gap added energy: %v", got)
	}
	s.PowerAt = s.PowerAt.Add(5 * time.Second)
	if got := e.observe(s, s.PowerAt); math.Abs(got-(1000+6900*5.0/3600)) > 1e-8 {
		t.Fatal(got)
	}
}

func TestSessionEnergySurvivesRestartBeforeCounterCatchup(t *testing.T) {
	store := &sessionMemory{data: map[string]string{}}
	m := sessionManager(store, "garage", "charger")
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	m.SetNowFn(func() time.Time { return at })
	s := EVSample{Connected: true, RequestActive: true, DeviceID: "easee:A", SessionID: "session-1", SessionWh: 1000, EnergyAt: at, PowerW: 6900, PowerAt: at}
	m.ObserveSample("garage", s)
	m.SetCurrentSoC("garage", .76)
	for i := 0; i < 24; i++ {
		at = at.Add(5 * time.Second)
		s.PowerAt = at
		m.ObserveSample("garage", s)
	}
	before, _ := m.State("garage")
	if math.Abs(before.CurrentSoC-(.76+230*.9/60000)) > 1e-8 {
		t.Fatal(before)
	}
	m = sessionManager(store, "garage", "charger")
	m.SetNowFn(func() time.Time { return at })
	m.ObserveSample("garage", s)
	after, _ := m.State("garage")
	if math.Abs(before.CurrentSoC-after.CurrentSoC) > 1e-8 || after.SoCRetention != "session" {
		t.Fatalf("before=%+v after=%+v", before, after)
	}
	s.SessionWh = 1230
	s.EnergyAt = at
	at = at.Add(5 * time.Second)
	s.PowerAt = at
	m.ObserveSample("garage", s)
	after, _ = m.State("garage")
	if math.Abs(after.CurrentSoC-(.76+6900*125.0/3600*.9/60000)) > 1e-8 {
		t.Fatalf("catch-up changed anchor: %+v", after)
	}
}

func TestMissingAndOutOfOrderCounterKeepConfirmedSession(t *testing.T) {
	store := &sessionMemory{data: map[string]string{}}
	m := sessionManager(store, "garage", "charger")
	at := time.Now().Add(-time.Hour)
	m.SetNowFn(func() time.Time { return at })
	s := EVSample{Connected: true, RequestActive: true, DeviceID: "easee:A", SessionID: "session-1", SessionWh: 1000, EnergyAt: at, PowerAt: at}
	m.ObserveSample("garage", s)
	m.SetCurrentSoC("garage", .76)
	at = at.Add(5 * time.Second)
	s.PowerAt = at
	s.SessionWhUnavailable = true
	s.SessionWh = 0
	m.ObserveSample("garage", s)
	st, _ := m.State("garage")
	if st.CurrentSoC != .76 || st.SoCRetention != "session" {
		t.Fatal(st)
	}
	s.SessionWhUnavailable = false
	s.SessionWh = 900
	s.EnergyAt = at.Add(-time.Minute)
	m.ObserveSample("garage", s)
	st, _ = m.State("garage")
	if st.CurrentSoC != .76 || st.SoCRetention != "session" {
		t.Fatal(st)
	}
	// A fresh lower counter is a real reset and must drop the previous anchor.
	s.EnergyAt = at
	m.ObserveSample("garage", s)
	st, _ = m.State("garage")
	if st.SoCSource != "assumed" {
		t.Fatal(st)
	}
}

func TestSessionEnergyOldCounterDoesNotLoseTrimmedPower(t *testing.T) {
	start := time.Now().Add(-4 * time.Hour)
	e := &sessionEnergy{}
	for sec := 0; sec <= 3*3600; sec += 5 {
		at := start.Add(time.Duration(sec) * time.Second)
		got := e.observe(EVSample{PowerW: 3600, PowerAt: at, SessionWh: 1000, EnergyAt: start}, at)
		if want := 1000 + float64(sec); math.Abs(got-want) > 1e-8 {
			t.Fatalf("at %ds got %v want %v", sec, got, want)
		}
	}
}

func TestSessionProgressPersistsBetweenMinuteBoundaries(t *testing.T) {
	store := &sessionMemory{data: map[string]string{}}
	m := sessionManager(store, "garage", "charger")
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	m.SetNowFn(func() time.Time { return at })
	s := EVSample{Connected: true, RequestActive: true, DeviceID: "easee:A", SessionID: "session-1", SessionWh: 1000, EnergyAt: at, PowerW: 6900, PowerAt: at}
	m.ObserveSample("garage", s)
	m.SetCurrentSoC("garage", .76)
	for i := 0; i < 17; i++ {
		at = at.Add(5 * time.Second)
		s.PowerAt = at
		m.ObserveSample("garage", s)
	}
	before, _ := m.State("garage")
	m = sessionManager(store, "garage", "charger")
	at = at.Add(3 * time.Second)
	m.SetNowFn(func() time.Time { return at })
	m.ObserveSample("garage", s)
	after, _ := m.State("garage")
	if before.CurrentSoC != after.CurrentSoC || after.SoCRetention != "session" {
		t.Fatalf("restart forgot measured progress: before=%+v after=%+v", before, after)
	}
}

func TestFirstCounterAfterConfirmationDoesNotAddPastEnergy(t *testing.T) {
	store := &sessionMemory{data: map[string]string{}}
	m := sessionManager(store, "garage", "charger")
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	m.SetNowFn(func() time.Time { return at })
	s := EVSample{Connected: true, RequestActive: true, DeviceID: "easee:A", SessionID: "session-1", SessionWhUnavailable: true, PowerAt: at, PowerW: 3600}
	m.ObserveSample("garage", s)
	m.SetCurrentSoC("garage", .76)
	for i := 0; i < 6; i++ {
		at = at.Add(5 * time.Second)
		s.PowerAt = at
		m.ObserveSample("garage", s)
	}
	s.SessionWhUnavailable = false
	s.SessionWh, s.EnergyAt = 1030, at
	m.ObserveSample("garage", s)
	before, _ := m.State("garage")
	if want := .76 + 30*.9/60000; math.Abs(before.CurrentSoC-want) > 1e-9 || before.SoCRetention != "session" {
		t.Fatalf("late counter changed user correction: %+v want %.8f", before, want)
	}
	m = sessionManager(store, "garage", "charger")
	m.SetNowFn(func() time.Time { return at })
	m.ObserveSample("garage", s)
	after, _ := m.State("garage")
	if before.CurrentSoC != after.CurrentSoC || after.SoCRetention != "session" {
		t.Fatalf("late counter was not saved: before=%+v after=%+v", before, after)
	}
}

func TestLateFirstCounterDoesNotConsumeCurrentSlotBudget(t *testing.T) {
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	c := &Controller{}
	cfg := Config{ID: "garage", DriverName: "easee"}
	for sec := 0; sec <= 120; sec += 5 {
		at := start.Add(time.Duration(sec) * time.Second)
		s := EVSample{Connected: true, DeviceID: "easee:A", SessionID: "session-1", PowerW: 3600, PowerAt: at, SessionWhUnavailable: true}
		if sec >= 90 {
			s.SessionWhUnavailable = false
			s.SessionWh, s.EnergyAt = 1060, start.Add(time.Minute)
		}
		c.observeEnergy(cfg, s, at)
		got, missing := c.energySince("garage", start, at)
		if math.Abs(got-float64(sec)) > 1e-9 || missing != 0 {
			t.Fatalf("t=%d counted %v Wh, missing %v seconds", sec, got, missing)
		}
	}
}
