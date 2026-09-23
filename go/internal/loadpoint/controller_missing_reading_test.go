package loadpoint

import (
	"context"
	"testing"
	"time"
)

func TestMissingReadingDoesNotEndManualSession(t *testing.T) {
	now := time.Now()
	cfg := holdLoadpoint()
	samples := map[string]EVSample{}
	sender := &fakeSender{}
	c := newTestController(t, []Config{cfg}, &Directive{SlotStart: now, SlotEnd: now.Add(time.Hour)}, samples, sender)
	c.SetManualHold(cfg.ID, ManualHold{PowerW: 4140, PhaseMode: "3p", Persistent: true})
	c.Tick(context.Background(), now)
	if _, ok := c.GetManualHold(cfg.ID, now); !ok {
		t.Fatal("startup without a reading cleared the hold")
	}
	if len(sender.calls) != 0 {
		t.Fatal("sent a command without charger data")
	}
	samples[cfg.DriverName] = EVSample{Connected: true, RequestActive: true, PowerW: 4140, SessionWh: 6000}
	c.Tick(context.Background(), now.Add(time.Second))
	if len(sender.calls) != 1 || sender.calls[0].power != 4140 {
		t.Fatalf("confirmed connection did not restore manual charging: %+v", sender.calls)
	}
	c.manager.SetCurrentSoC(cfg.ID, .84)
	delete(samples, cfg.DriverName)
	c.TickWithDispatch(context.Background(), now.Add(2*time.Second), false)
	if _, ok := c.GetManualHold(cfg.ID, now); !ok {
		t.Fatal("missing reading during outage cleared the hold")
	}
	state, _ := c.manager.State(cfg.ID)
	if !state.PluggedIn || state.CurrentSoC < .83 {
		t.Fatalf("missing reading replaced the confirmed session: %+v", state)
	}
	samples[cfg.DriverName] = EVSample{Connected: false}
	c.Tick(context.Background(), now.Add(3*time.Second))
	if _, ok := c.GetManualHold(cfg.ID, now); ok {
		t.Fatal("a confirmed unplug must clear the hold")
	}
}
