package loadpoint

import (
	"context"
	"testing"
	"time"
)

// bootController is one Core process over a state.db that outlives it.
func bootController(t *testing.T, store *sessionMemory, samples map[string]EVSample) (*Controller, *fakeSender) {
	t.Helper()
	m := NewManager()
	m.Load([]Config{holdLoadpoint()})
	m.SetSessionStore(store)
	sender := &fakeSender{}
	plan := PlanFunc(func(time.Time) (Directive, bool) { return Directive{}, false })
	tel := TelemetryFunc(func(driver string) (EVSample, bool) { s, ok := samples[driver]; return s, ok })
	c := NewController(m, plan, tel, sender.Send)
	c.SetSiteFuse(SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3})
	c.SetManualHoldSaver(func(id string, h ManualHold, cleared bool) {
		if err := m.PersistManualHold(id, h, cleared); err != nil {
			t.Error(err)
		}
	})
	return c, sender
}

func tickWants(t *testing.T, c *Controller, sender *fakeSender, now time.Time, power float64, unconfirmed bool) {
	t.Helper()
	c.Tick(context.Background(), now)
	if last, ok := lastSetCurrent(sender.calls); !ok || last.power != power {
		t.Fatalf("commands=%+v, want latest power %v", sender.calls, power)
	}
	if st, _ := c.manager.State("garage"); st.ManualRestoreUnconfirmed != unconfirmed {
		t.Fatalf("manual_restore_unconfirmed = %v, want %v", st.ManualRestoreUnconfirmed, unconfirmed)
	}
}

// Charge now ends on Stop, unplug or a full car. A restart is none of them,
// even when the charger reports its identity before its session.
func TestChargeNowWaitsForSessionAfterRestart(t *testing.T) {
	for _, reconnect := range []bool{false, true} {
		store := &sessionMemory{data: map[string]string{}}
		sample := EVSample{Connected: true, RequestActive: true, ConnectionGeneration: 1, DeviceID: "easee:A", SessionID: "session-1"}
		samples := map[string]EVSample{"easee": sample}
		c, sender := bootController(t, store, samples)
		now := time.Now()
		c.SetManualHold("garage", ManualHold{PowerW: 4140, Persistent: true})
		tickWants(t, c, sender, now, 4140, false)

		c, sender = bootController(t, store, samples)
		sample.SessionID = ""
		samples["easee"] = sample
		tickWants(t, c, sender, now.Add(20*time.Second), 0, true)
		if reconnect {
			// A new socket is not a new car; only the session can tell.
			sample.ConnectionGeneration = 2
			samples["easee"] = sample
			tickWants(t, c, sender, now.Add(21*time.Second), 0, true)
		}
		sample.SessionID = "session-1"
		samples["easee"] = sample
		tickWants(t, c, sender, now.Add(22*time.Second), 4140, false)
	}
}

// Most chargers, OCPP included, never report a session ID. The same
// hardware still plugged in resumes at once; an unplug still ends it.
func TestChargeNowWithoutSessionIDResumesAfterRestart(t *testing.T) {
	store := &sessionMemory{data: map[string]string{}}
	sample := EVSample{Connected: true, RequestActive: true, ConnectionGeneration: 1, DeviceID: "rigco:OCPPSIM02"}
	samples := map[string]EVSample{"easee": sample}
	c, sender := bootController(t, store, samples)
	now := time.Now()
	c.SetManualHold("garage", ManualHold{PowerW: 4140, Persistent: true})
	tickWants(t, c, sender, now, 4140, false)

	// After a restart the charger connects before it reports its cable.
	samples["easee"] = EVSample{ConnectionUnknown: true, ConnectionGeneration: 1}
	c, sender = bootController(t, store, samples)
	c.Tick(context.Background(), now.Add(20*time.Second))
	samples["easee"] = sample
	tickWants(t, c, sender, now.Add(21*time.Second), 4140, false)

	sample.Connected = false
	samples["easee"] = sample
	c.Tick(context.Background(), now.Add(22*time.Second))
	sample.Connected = true
	samples["easee"] = sample
	tickWants(t, c, sender, now.Add(23*time.Second), 0, false)
	if h, ok := c.GetManualHold("garage", now); ok {
		t.Fatalf("the next car inherited Charge now: %+v", h)
	}
	c, sender = bootController(t, store, samples)
	tickWants(t, c, sender, now.Add(30*time.Second), 0, false)
	if h, ok := c.GetManualHold("garage", now); ok {
		t.Fatalf("Charge now returned after unplug and restart: %+v", h)
	}
}

// A different session after a restart means the car changed while Core
// was down. It must not inherit the earlier Charge now.
func TestChargeNowDoesNotFollowAnotherSessionAfterRestart(t *testing.T) {
	store := &sessionMemory{data: map[string]string{}}
	sample := EVSample{Connected: true, RequestActive: true, DeviceID: "easee:A", SessionID: "session-1"}
	samples := map[string]EVSample{"easee": sample}
	c, sender := bootController(t, store, samples)
	now := time.Now()
	c.SetManualHold("garage", ManualHold{PowerW: 4140, Persistent: true})
	tickWants(t, c, sender, now, 4140, false)

	c, sender = bootController(t, store, samples)
	sample.SessionID = ""
	samples["easee"] = sample
	tickWants(t, c, sender, now.Add(20*time.Second), 0, true)
	sample.SessionID = "session-2"
	samples["easee"] = sample
	tickWants(t, c, sender, now.Add(21*time.Second), 0, true)
	tickWants(t, c, sender, now.Add(22*time.Second), 0, true)
}
