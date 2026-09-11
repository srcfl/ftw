package loadpoint

import (
	"testing"
	"time"
)

func TestManualRestrictionSurvivesRestartWithoutHardwareIdentity(t *testing.T) {
	for _, device := range []string{"", "ep:ocpp://garage", "easee:ABC"} {
		for _, watts := range []float64{0, 4140} {
			store := &sessionMemory{data: map[string]string{}}
			m := sessionManager(store, "garage", "charger")
			m.ObserveSession("garage", true, 0, 1000, true, device, "")
			if err := m.PersistManualHold("garage", ManualHold{Persistent: true, PowerW: watts}, false); err != nil {
				t.Fatal(err)
			}
			restarted := sessionManager(store, "garage", "charger")
			restarted.ObserveSession("garage", true, 0, 1000, true, device, "")
			c := NewController(restarted, nil, nil, nil)
			c.restoreManualHoldForSession("garage")
			if h, ok := c.GetManualHold("garage", time.Now()); !ok || !h.Persistent || h.PowerW != 0 {
				t.Fatalf("%q/%v: restart lost restriction: %+v %v", device, watts, h, ok)
			}
			st, _ := restarted.State("garage")
			if st.ManualRestoreUnconfirmed != (watts > 0) {
				t.Fatalf("%q/%v: confirmation=%v", device, watts, st.ManualRestoreUnconfirmed)
			}
			if err := restarted.PersistManualHold("garage", ManualHold{}, true); err != nil {
				t.Fatal(err)
			}
			afterClear := sessionManager(store, "garage", "charger")
			afterClear.ObserveSession("garage", true, 0, 1000, true, device, "")
			if h, status := afterClear.RestoreManualHold("garage"); status != "none" || h.Persistent {
				t.Fatalf("clear did not survive restart: %+v %s", h, status)
			}
		}
	}
}

func TestNewPauseSupersedesClearBeforeHardwareArrives(t *testing.T) {
	store := &sessionMemory{data: map[string]string{}}
	m := sessionManager(store, "garage", "charger")
	if err := m.PersistManualHold("garage", ManualHold{}, true); err != nil {
		t.Fatal(err)
	}
	if err := m.PersistManualHold("garage", ManualHold{Persistent: true}, false); err != nil {
		t.Fatal(err)
	}
	restarted := sessionManager(store, "garage", "charger")
	if h, status := restarted.RestoreManualHold("garage"); status != "restored" || !h.Persistent || h.PowerW != 0 {
		t.Fatalf("old clear overwrote new pause: %+v %s", h, status)
	}
}
