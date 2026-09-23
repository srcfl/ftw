package loadpoint

import (
	"context"
	"testing"
	"time"
)

func TestLostHardwareProofPausesOldStartButAllowsANewExplicitStart(t *testing.T) {
	cfg := holdLoadpoint()
	sender := &fakeSender{}
	sample := EVSample{Connected: true, RequestActive: true, DeviceID: "easee:A", SessionID: "session-1"}
	samples := map[string]EVSample{cfg.DriverName: sample}
	c := newTestController(t, []Config{cfg}, nil, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3})
	now := time.Now()
	tick := func(want float64) {
		t.Helper()
		c.Tick(context.Background(), now)
		now = now.Add(time.Second)
		if n := len(sender.calls); n == 0 || sender.calls[n-1].power != want {
			t.Fatalf("commands=%+v, want latest power %v", sender.calls, want)
		}
	}
	tick(0)
	c.SetManualHold(cfg.ID, ManualHold{PowerW: 4140, Persistent: true})
	tick(4140)
	sample.DeviceID, sample.SessionID = "", ""
	samples[cfg.DriverName] = sample
	tick(0)
	if state, _ := c.manager.State(cfg.ID); !state.ManualRestoreUnconfirmed {
		t.Fatal("lost proof did not explain the pause")
	}
	tick(0)
	if state, _ := c.manager.State(cfg.ID); !state.ManualRestoreUnconfirmed {
		t.Fatal("a later unknown sample cleared the confirmation requirement")
	}
	// This request belongs to the currently unknown socket, not the older
	// device. It works now and may acquire its first fresh hardware identity.
	c.SetManualHold(cfg.ID, ManualHold{PowerW: 4140, Persistent: true})
	tick(4140)
	sample.DeviceID, sample.SessionID = "easee:B", "session-2"
	samples[cfg.DriverName] = sample
	tick(4140)
	if state, _ := c.manager.State(cfg.ID); state.ManualRestoreUnconfirmed {
		t.Fatal("new explicit Start still asks for confirmation")
	}
}

func TestLostHardwareProofKeepsExplicitPauseConfirmed(t *testing.T) {
	m := sessionManager(nil, "garage", "charger")
	m.ObserveSession("garage", true, 0, 1000, false, "easee:A", "session-1")
	c := NewController(m, nil, nil, nil)
	c.SetManualHold("garage", ManualHold{Persistent: true})
	for _, device := range []string{"", "", "easee:B"} {
		m.ObserveSession("garage", true, 0, 1000, false, device, "")
		c.restoreManualHoldForSession("garage")
		if hold, ok := c.GetManualHold("garage", time.Now()); !ok || hold.PowerW != 0 {
			t.Fatalf("explicit pause lost: %+v %v", hold, ok)
		}
		if state, _ := m.State("garage"); state.ManualRestoreUnconfirmed {
			t.Fatal("explicit Pause unnecessarily asks for confirmation")
		}
	}
}

func TestManualSaveErrorKeepsCommandAndRetriesOnFreshReading(t *testing.T) {
	for _, action := range []string{"start", "clear", "before_identity"} {
		t.Run(action, func(t *testing.T) {
			store := &sessionMemory{data: map[string]string{}}
			m := sessionManager(store, "garage", "charger")
			if action != "before_identity" {
				m.ObserveSession("garage", true, 4300, 1000, true, "easee:A", "session-1")
			}
			c := NewController(m, nil, nil, nil)
			var saveErr error
			c.SetManualHoldSaver(func(id string, h ManualHold, cleared bool) {
				saveErr = m.PersistManualHold(id, h, cleared)
			})
			if action == "clear" {
				c.SetManualHold("garage", ManualHold{PowerW: 4140, Persistent: true})
				if saveErr != nil {
					t.Fatal(saveErr)
				}
			}
			store.fail = true
			if action == "clear" {
				c.ClearManualHold("garage")
			} else {
				c.SetManualHold("garage", ManualHold{PowerW: 5520, Persistent: true})
			}
			attempted := true
			if (saveErr != nil) != attempted {
				t.Fatalf("save error = %v, attempted = %v", saveErr, attempted)
			}
			if s, _ := m.State("garage"); s.ManualSaveError != attempted {
				t.Fatalf("save failure not reported: %+v", s)
			}
			// A later telemetry flush can fail too, including the first write
			// of an explicit request received before hardware was known.
			m.ObserveSession("garage", true, 4300, 1100, true, "easee:A", "session-1")
			if s, _ := m.State("garage"); !s.ManualSaveError {
				t.Fatal("retry failure not reported")
			}
			if h, ok := c.GetManualHold("garage", time.Now()); (action == "clear" && ok) ||
				(action != "clear" && (!ok || h.PowerW != 5520)) {
				t.Fatalf("disk failure changed the current command: %+v %v", h, ok)
			}
			// Reloading settings cannot hide an outstanding failed save.
			m.Load([]Config{{ID: "garage", DriverName: "charger", VehicleCapacityWh: 60000}})
			if s, _ := m.State("garage"); !s.ManualSaveError {
				t.Fatal("config reload hid the failed save")
			}
			store.fail = false
			m.ObserveSession("garage", true, 4300, 1200, true, "easee:A", "session-1")
			if s, _ := m.State("garage"); s.ManualSaveError {
				t.Fatal("successful retry did not clear the warning")
			}
			// Prove the retry reached storage rather than only clearing a flag.
			restarted := sessionManager(store, "garage", "charger")
			restarted.ObserveSession("garage", true, 4300, 1200, true, "easee:A", "session-1")
			h, status := restarted.RestoreManualHold("garage")
			if (action == "clear" && status != "none") ||
				(action != "clear" && (status != "restored" || h.PowerW != 5520)) {
				t.Fatalf("retried command did not survive restart: %+v %s", h, status)
			}
		})
	}
}

func TestManualHoldRestoreRequiresHardwareAndActiveSession(t *testing.T) {
	store := &sessionMemory{data: map[string]string{}}
	m := sessionManager(store, "garage", "charger")
	m.ObserveSession("garage", true, 4300, 1000, true, "easee:ABC", "session-1")
	if err := m.PersistManualHold("garage", ManualHold{PowerW: 4140, Persistent: true}, false); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		device, session, status string
		power                   float64
	}{
		{"easee:ABC", "session-1", "restored", 4140},
		{"easee:ABC", "session-2", "unconfirmed", 0},
		{"easee:ABC", "", "unconfirmed", 0},
		{"easee:OTHER", "session-1", "unconfirmed", 0},
	} {
		m := sessionManager(store, "garage", "charger")
		m.ObserveSession("garage", true, 0, 1000, true, tc.device, tc.session)
		h, status := m.RestoreManualHold("garage")
		if status != tc.status || h.PowerW != tc.power || !h.Persistent {
			t.Fatalf("%+v: got %+v %s", tc, h, status)
		}
	}
	if err := m.PersistManualHold("garage", ManualHold{PowerW: 0, Persistent: true}, false); err != nil {
		t.Fatal(err)
	}
	m = sessionManager(store, "garage", "charger")
	m.ObserveSession("garage", true, 0, 1000, true, "easee:ABC", "")
	if h, status := m.RestoreManualHold("garage"); status != "restored" || h.PowerW != 0 {
		t.Fatalf("pause failed: %+v %s", h, status)
	}
}

func TestManualHoldBeforeIdentityBindsOnlyAfterFreshReading(t *testing.T) {
	store := &sessionMemory{data: map[string]string{}}
	m := sessionManager(store, "garage", "charger")
	m.PersistManualHold("garage", ManualHold{PowerW: 4140, Persistent: true}, false)
	if h, status := sessionManager(store, "garage", "charger").RestoreManualHold("garage"); status != "unconfirmed" || !h.Persistent || h.PowerW != 0 {
		t.Fatalf("unidentified Start gained power after restart: %+v %s", h, status)
	}
	m.ObserveSession("garage", true, 4300, 1000, true, "easee:ABC", "session-1")
	m = sessionManager(store, "garage", "charger")
	m.ObserveSession("garage", true, 4300, 1100, true, "easee:ABC", "session-1")
	if h, status := m.RestoreManualHold("garage"); status != "restored" || h.PowerW != 4140 {
		t.Fatalf("explicit start was lost: %+v %s", h, status)
	}
}

func TestControllerPausesPendingRestoreAndHonoursExplicitOverride(t *testing.T) {
	store := &sessionMemory{data: map[string]string{}}
	m := sessionManager(store, "garage", "charger")
	m.ObserveSession("garage", true, 4300, 1000, true, "easee:ABC", "session-1")
	m.PersistManualHold("garage", ManualHold{PowerW: 4140, Persistent: true}, false)
	m = sessionManager(store, "garage", "charger")
	c := NewController(m, nil, nil, nil)
	c.restoreManualHoldForSession("garage")
	if h, ok := c.GetManualHold("garage", time.Now()); !ok || h.PowerW != 0 {
		t.Fatalf("unknown session could use automatic charging: %+v %v", h, ok)
	}
	if s, _ := m.State("garage"); !s.ManualRestoreUnconfirmed {
		t.Fatal("pending restore not visible")
	}
	c.markManualExplicit("garage")
	c.SetManualHold("garage", ManualHold{PowerW: 5520, Persistent: true})
	m.ObserveSession("garage", true, 4300, 1100, true, "easee:ABC", "session-1")
	c.restoreManualHoldForSession("garage")
	if h, ok := c.GetManualHold("garage", time.Now()); !ok || h.PowerW != 5520 {
		t.Fatalf("recovery overwrote explicit choice: %+v %v", h, ok)
	}
	if s, _ := m.State("garage"); s.ManualRestoreUnconfirmed {
		t.Fatal("explicit action did not confirm restore")
	}
}

func TestLegacyManualHoldNeedsConfirmationAndClearCannotResurrectIt(t *testing.T) {
	store := &sessionMemory{data: map[string]string{"loadpoint_manual_hold:garage": `{"PowerW":11000,"Persistent":true}`}}
	m := sessionManager(store, "garage", "charger")
	m.ObserveSession("garage", true, 4300, 1000, true, "easee:ABC", "session-1")
	if h, status := m.RestoreManualHold("garage"); status != "unconfirmed" || h.PowerW != 0 {
		t.Fatalf("legacy hold resumed: %+v %s", h, status)
	}
	m.PersistManualHold("garage", ManualHold{}, true)
	m = sessionManager(store, "garage", "charger")
	m.ObserveSession("garage", true, 4300, 1100, true, "easee:ABC", "session-1")
	if _, status := m.RestoreManualHold("garage"); status != "none" {
		t.Fatalf("cleared hold resurrected: %s", status)
	}
}

func TestRuntimeManualHoldCannotFollowChangedDeviceOrSession(t *testing.T) {
	for _, change := range []string{"hardware", "session", "remove_readd"} {
		t.Run(change, func(t *testing.T) {
			store := &sessionMemory{data: map[string]string{}}
			m := sessionManager(store, "garage", "charger")
			m.ObserveSession("garage", true, 4300, 1000, true, "easee:A", "session-1")
			c := NewController(m, nil, nil, nil)
			c.SetManualHold("garage", ManualHold{PowerW: 4140, Persistent: true})
			device, session := "easee:A", "session-1"
			switch change {
			case "hardware":
				device = "easee:B"
			case "session":
				session = "session-2"
			case "remove_readd":
				m.Load(nil)
				m.Load([]Config{{ID: "garage", DriverName: "charger", VehicleCapacityWh: 60000}})
			}
			m.ObserveSession("garage", true, 4300, 1100, true, device, session)
			c.restoreManualHoldForSession("garage")
			if h, ok := c.GetManualHold("garage", time.Now()); !ok || h.PowerW != 0 {
				t.Fatalf("old power followed %s: %+v %v", change, h, ok)
			}
			if s, _ := m.State("garage"); !s.ManualRestoreUnconfirmed {
				t.Fatal("changed session was not shown")
			}
			// The safe pause also survives another restart on that hardware.
			m = sessionManager(store, "garage", "charger")
			m.ObserveSession("garage", true, 0, 1100, true, device, session)
			if h, status := m.RestoreManualHold("garage"); status != "restored" || h.PowerW != 0 {
				t.Fatalf("pause was not durable: %+v %s", h, status)
			}
		})
	}
}

func TestExplicitStartCanAcquireFirstSessionProof(t *testing.T) {
	for _, initialDevice := range []string{"", "easee:A"} {
		m := sessionManager(nil, "garage", "charger")
		if initialDevice != "" {
			m.ObserveSession("garage", true, 0, 0, true, initialDevice, "")
		}
		c := NewController(m, nil, nil, nil)
		c.SetManualHold("garage", ManualHold{PowerW: 4140, Persistent: true})
		m.ObserveSession("garage", true, 4300, 100, true, "easee:A", "session-1")
		c.restoreManualHoldForSession("garage")
		if h, ok := c.GetManualHold("garage", time.Now()); !ok || h.PowerW != 4140 {
			t.Fatalf("explicit start before proof was lost: %+v %v", h, ok)
		}
	}
}

func TestFirstSessionIDAfterCounterResetCannotInheritManualStart(t *testing.T) {
	store := &sessionMemory{data: map[string]string{}}
	m := sessionManager(store, "garage", "charger")
	m.ObserveSession("garage", true, 0, 11000, true, "easee:A", "")
	c := NewController(m, nil, nil, nil)
	c.SetManualHold("garage", ManualHold{PowerW: 4140, Persistent: true})
	// The old session was paused and unverified. A fresh charging session
	// appears with less energy, after an unplug the box did not observe.
	m.ObserveSession("garage", true, 4300, 500, true, "easee:A", "session-2")
	c.restoreManualHoldForSession("garage")
	if h, ok := c.GetManualHold("garage", time.Now()); !ok || h.PowerW != 0 {
		t.Fatalf("new car inherited the previous Start: %+v %v", h, ok)
	}
	if s, _ := m.State("garage"); !s.ManualRestoreUnconfirmed {
		t.Fatal("changed session did not ask for confirmation")
	}
}

func TestClearBeforeTelemetrySurvivesAnotherImmediateRestart(t *testing.T) {
	store := &sessionMemory{data: map[string]string{}}
	m := sessionManager(store, "garage", "charger")
	m.ObserveSession("garage", true, 4300, 1000, true, "easee:A", "session-1")
	m.PersistManualHold("garage", ManualHold{PowerW: 4140, Persistent: true}, false)
	store.data["loadpoint_manual_hold:garage"] = `{"PowerW":11000,"Persistent":true}`
	m = sessionManager(store, "garage", "charger")
	c := NewController(m, nil, nil, nil)
	c.SetManualHoldSaver(func(id string, h ManualHold, cleared bool) {
		if err := m.PersistManualHold(id, h, cleared); err != nil {
			t.Error(err)
		}
	})
	c.ClearManualHold("garage")
	m = sessionManager(store, "garage", "charger")
	c = NewController(m, nil, nil, nil)
	c.restoreManualHoldForSession("garage")
	m.ObserveSession("garage", true, 4300, 1000, true, "easee:A", "session-1")
	c.restoreManualHoldForSession("garage")
	if _, ok := c.GetManualHold("garage", time.Now()); ok {
		t.Fatal("cleared hold returned after reboot before telemetry")
	}
	if _, status := m.RestoreManualHold("garage"); status != "none" {
		t.Fatalf("record survived explicit clear: %s", status)
	}
}

func TestConcurrentSetAndClearPersistInControllerOrder(t *testing.T) {
	m := sessionManager(nil, "garage", "charger")
	c := NewController(m, nil, nil, nil)
	entered, release, setDone, clearDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var order []float64
	c.SetManualHoldSaver(func(_ string, h ManualHold, cleared bool) {
		if !cleared {
			close(entered)
			<-release
			order = append(order, h.PowerW)
		} else {
			order = append(order, 0)
		}
	})
	go func() { c.SetManualHold("garage", ManualHold{PowerW: 4140, Persistent: true}); close(setDone) }()
	<-entered
	go func() { c.ClearManualHold("garage"); close(clearDone) }()
	select {
	case <-clearDone:
		t.Fatal("Clear overtook an earlier pending disk write")
	case <-time.After(20 * time.Millisecond):
	}
	if h, ok := c.GetManualHold("garage", time.Now()); !ok || h.PowerW != 4140 {
		t.Fatalf("read was blocked or changed before ordered clear: %+v %v", h, ok)
	}
	close(release)
	<-setDone
	<-clearDone
	if len(order) != 2 || order[0] != 4140 || order[1] != 0 {
		t.Fatalf("persist order: %v", order)
	}
	if _, ok := c.GetManualHold("garage", time.Now()); ok {
		t.Fatal("clear lost to earlier save")
	}
}

func TestManualStartWithoutHardwareCannotFollowAnotherSocket(t *testing.T) {
	cfg := holdLoadpoint()
	sender := &fakeSender{}
	sample := EVSample{Connected: true, RequestActive: true, ConnectionGeneration: 1}
	samples := map[string]EVSample{cfg.DriverName: sample}
	c := newTestController(t, []Config{cfg}, nil, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3})
	now := time.Now()
	tick := func(want float64) {
		t.Helper()
		c.Tick(context.Background(), now)
		now = now.Add(time.Second)
		if sender.calls[len(sender.calls)-1].power != want {
			t.Fatalf("commands=%+v, want %v", sender.calls, want)
		}
	}
	tick(0)
	c.SetManualHold(cfg.ID, ManualHold{PowerW: 4140, Persistent: true})
	tick(4140)
	sample.ConnectionGeneration = 2
	samples[cfg.DriverName] = sample
	tick(0)
	if st, _ := c.manager.State(cfg.ID); !st.ManualRestoreUnconfirmed {
		t.Fatal("new socket inherited unknown hardware Start")
	}
	c.SetManualHold(cfg.ID, ManualHold{PowerW: 4140, Persistent: true})
	tick(4140)
	sample.DeviceID = "easee:A"
	samples[cfg.DriverName] = sample
	tick(4140)
	if st, _ := c.manager.State(cfg.ID); st.ManualRestoreUnconfirmed {
		t.Fatal("first proof on same socket invalidated explicit Start")
	}
}
