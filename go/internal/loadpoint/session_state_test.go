package loadpoint

import (
	"errors"
	"math"
	"testing"
)

type sessionMemory struct {
	data map[string]string
	fail bool
}

func (s *sessionMemory) LoadConfig(k string) (string, bool) { v, ok := s.data[k]; return v, ok }
func (s *sessionMemory) SaveConfig(k, v string) error {
	if s.fail {
		return errors.New("disk full")
	}
	s.data[k] = v
	return nil
}
func sessionManager(store SessionStore, id, driver string) *Manager {
	m := NewManager()
	m.Load([]Config{{ID: id, DriverName: driver, VehicleCapacityWh: 60000}})
	m.SetSessionStore(store)
	return m
}
func TestConfirmedSoCSurvivesRestartOnlyForSameHardwareSession(t *testing.T) {
	store := &sessionMemory{data: map[string]string{}}
	m := sessionManager(store, "garage", "charger")
	m.ObserveSession("garage", true, 4300, 9000, true, "easee:ABC", "connection-1")
	m.SetCurrentSoC("garage", .84)
	if s, _ := m.State("garage"); s.SoCRetention != "session" {
		t.Fatalf("not saved: %+v", s)
	}
	// Renaming the configured driver and loadpoint does not change hardware.
	m = sessionManager(store, "renamed", "renamed-driver")
	m.ObserveSession("renamed", true, 4300, 9600, true, "easee:ABC", "connection-1")
	if s, _ := m.State("renamed"); math.Abs(s.CurrentSoC-.85) > 1e-9 || s.SoCSource == "assumed" {
		t.Fatalf("same session failed to restore: %+v", s)
	}
	for _, tc := range []struct {
		name, device, session string
		wh                    float64
	}{
		{"new session", "easee:ABC", "connection-2", 9600},
		{"new charger", "easee:OTHER", "connection-1", 9600},
		{"no session", "easee:ABC", "", 9600},
		{"endpoint identity", "ep:192.168.1.40", "connection-1", 9600},
		{"counter reset", "easee:ABC", "connection-1", 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := sessionManager(store, "garage", "charger")
			m.ObserveSession("garage", true, 0, tc.wh, true, tc.device, tc.session)
			if s, _ := m.State("garage"); s.SoCSource != "assumed" {
				t.Fatalf("restored wrong car: %+v", s)
			}
		})
	}
}

func TestConfirmedSoCSurvivesFirstSessionProofWhenChargingStarts(t *testing.T) {
	store := &sessionMemory{data: map[string]string{}}
	m := sessionManager(store, "garage", "charger")
	m.ObserveSession("garage", true, 0, 0, true, "easee:A", "")
	m.SetCurrentSoC("garage", .12)
	if s, _ := m.State("garage"); s.SoCRetention != "unavailable" {
		t.Fatalf("unverified session was saved: %+v", s)
	}
	m.ObserveSession("garage", true, 4300, 600, true, "easee:A", "session-1")
	if s, _ := m.State("garage"); math.Abs(s.CurrentSoC-.13) > 1e-9 || s.SoCSource == "assumed" || s.SoCRetention != "session" {
		t.Fatalf("charging lost the level entered while waiting: %+v", s)
	}
	m = sessionManager(store, "garage", "charger")
	m.ObserveSession("garage", true, 4300, 600, true, "easee:A", "session-1")
	if s, _ := m.State("garage"); math.Abs(s.CurrentSoC-.13) > 1e-9 || s.SoCSource == "assumed" {
		t.Fatalf("verified level did not survive restart: %+v", s)
	}
}

func TestFirstSessionProofDoesNotCrossConnectionChanges(t *testing.T) {
	for _, change := range []string{"unplug", "hardware", "counter_regression"} {
		t.Run(change, func(t *testing.T) {
			m := sessionManager(&sessionMemory{data: map[string]string{}}, "garage", "charger")
			m.ObserveSession("garage", true, 0, 1000, true, "easee:A", "")
			m.SetCurrentSoC("garage", .12)
			device, wh := "easee:A", 1600.0
			switch change {
			case "unplug":
				m.ObserveSession("garage", false, 0, 1000, false, "easee:A", "")
			case "hardware":
				device = "easee:B"
			case "counter_regression":
				wh = 500
			}
			m.ObserveSession("garage", true, 4300, wh, true, device, "session-1")
			if s, _ := m.State("garage"); s.SoCSource != "assumed" {
				t.Fatalf("level followed %s: %+v", change, s)
			}
		})
	}
}

func TestUnseenReconnectResetsConfirmedSoC(t *testing.T) {
	store := &sessionMemory{data: map[string]string{}}
	m := sessionManager(store, "garage", "charger")
	m.ObserveSession("garage", true, 4300, 9000, true, "easee:ABC", "session-1")
	m.SetCurrentSoC("garage", .84)
	// A charger returns after an outage. No unplug sample reached core.
	m.ObserveSession("garage", true, 4300, 12000, true, "easee:ABC", "session-2")
	if s, _ := m.State("garage"); s.SoCSource != "assumed" {
		t.Fatalf("previous car level leaked: %+v", s)
	}
}
func TestObservedUnplugErasesConfirmedSoC(t *testing.T) {
	store := &sessionMemory{data: map[string]string{}}
	m := sessionManager(store, "garage", "charger")
	m.ObserveSession("garage", true, 4300, 9000, true, "easee:ABC", "session-1")
	m.SetCurrentSoC("garage", .84)
	m.ObserveSession("garage", false, 0, 0, false, "easee:ABC", "")
	m = sessionManager(store, "garage", "charger")
	m.ObserveSession("garage", true, 0, 9000, true, "easee:ABC", "session-1")
	if s, _ := m.State("garage"); s.SoCSource != "assumed" {
		t.Fatalf("unplug resurrected old level: %+v", s)
	}
}
func TestSessionSaveFailureIsVisibleAndRetryable(t *testing.T) {
	store := &sessionMemory{data: map[string]string{}, fail: true}
	m := sessionManager(store, "garage", "charger")
	m.ObserveSession("garage", true, 4300, 9000, true, "easee:ABC", "session-1")
	m.SetCurrentSoC("garage", .84)
	if s, _ := m.State("garage"); s.SoCRetention != "error" || math.Abs(s.CurrentSoC-.84) > 1e-9 {
		t.Fatalf("failed write hidden: %+v", s)
	}
	store.fail = false
	m.SetCurrentSoC("garage", .84)
	if s, _ := m.State("garage"); s.SoCRetention != "session" {
		t.Fatalf("retry failed: %+v", s)
	}
}
func TestUnknownSessionKeepsManualLevelOnlyInMemory(t *testing.T) {
	m := sessionManager(&sessionMemory{data: map[string]string{}}, "garage", "charger")
	m.Observe("garage", true, 4300, 9000, true)
	m.SetCurrentSoC("garage", .84)
	m.Observe("garage", true, 4300, 9600, true)
	if s, _ := m.State("garage"); s.SoCRetention != "unavailable" || math.Abs(s.CurrentSoC-.85) > 1e-9 {
		t.Fatalf("unsupported charger lost input: %+v", s)
	}
}

func TestCapacityChangeKeepsCurrentLevelAndConfidence(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		m := sessionManager(&sessionMemory{data: map[string]string{}}, "garage", "charger")
		m.ObserveSession("garage", true, 4300, 9000, true, "easee:ABC", "session-1")
		if confirmed {
			m.SetCurrentSoC("garage", .84)
		}
		before, _ := m.State("garage")
		m.Load([]Config{{ID: "garage", DriverName: "charger", VehicleCapacityWh: 100000}})
		m.ObserveSession("garage", true, 4300, 9000, true, "easee:ABC", "session-1")
		after, _ := m.State("garage")
		if math.Abs(after.CurrentSoC-before.CurrentSoC) > 1e-9 || after.SoCSource != before.SoCSource {
			t.Fatalf("capacity changed level or confidence: before=%+v after=%+v", before, after)
		}
		m.ObserveSession("garage", true, 4300, 10000, true, "easee:ABC", "session-1")
		after, _ = m.State("garage")
		if math.Abs(after.CurrentSoC-before.CurrentSoC-.01) > 1e-9 {
			t.Fatalf("new capacity not used: %+v", after)
		}
	}
}

func TestNewSessionAfterOutageDropsPriorCarCapacity(t *testing.T) {
	m := sessionManager(nil, "garage", "charger")
	m.ObserveSession("garage", true, 4300, 1000, true, "easee:ABC", "session-1")
	m.ApplyVehicleProfile("garage", "Old car", 100000)
	m.ObserveSession("garage", true, 4300, 2000, true, "easee:ABC", "session-2")
	if s, _ := m.State("garage"); s.VehicleName != "" || s.VehicleCapacityWh != 60000 {
		t.Fatalf("prior car leaked after an unseen reconnect: %+v", s)
	}
}

func TestFirstReadingUnpluggedTombstonesStoredSession(t *testing.T) {
	store := &sessionMemory{data: map[string]string{}}
	m := sessionManager(store, "garage", "charger")
	m.ObserveSession("garage", true, 4300, 1000, true, "easee:ABC", "session-1")
	m.SetCurrentSoC("garage", .84)
	m = sessionManager(store, "garage", "charger")
	m.ObserveSession("garage", false, 0, 0, false, "easee:ABC", "")
	m = sessionManager(store, "garage", "charger")
	m.ObserveSession("garage", true, 0, 1000, true, "easee:ABC", "session-1")
	if s, _ := m.State("garage"); s.SoCSource != "assumed" {
		t.Fatalf("cold unplug failed to clear stored session: %+v", s)
	}
}
