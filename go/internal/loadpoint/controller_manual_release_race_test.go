package loadpoint

import (
	"context"
	"testing"
	"time"
)

func TestStaleAutoReleaseCannotDeleteNewPauseStartOrRetry(t *testing.T) {
	for _, action := range []string{"pause", "start", "same_request_retry"} {
		t.Run(action, func(t *testing.T) {
			m := NewManager()
			m.Load([]Config{{ID: "garage", DriverName: "charger"}})
			c := NewController(m, nil, nil, nil)
			c.SetManualHold("garage", ManualHold{PowerW: 4140, Persistent: true})
			old, _ := c.GetManualHold("garage", time.Now())
			next := old // The UI preserves StartedAt when editing the request.
			switch action {
			case "pause":
				next.PowerW = 0
			case "start":
				next.PowerW = 5520
			}
			entered, finishSave := make(chan struct{}), make(chan struct{})
			setDone, releaseDone := make(chan struct{}), make(chan bool, 1)
			clears := 0
			c.SetManualHoldSaver(func(_ string, _ ManualHold, cleared bool) {
				if cleared {
					clears++
					return
				}
				close(entered)
				<-finishSave
			})
			go func() { c.SetManualHold("garage", next); close(setDone) }()
			<-entered
			// A tick decided to release the old request just before this
			// operator change. It waits behind the new request's disk write.
			go func() { releaseDone <- c.releaseManualHoldIfCurrent("garage", old) }()
			select {
			case <-releaseDone:
				t.Fatal("auto-release overtook the explicit save")
			case <-time.After(20 * time.Millisecond):
			}
			close(finishSave)
			<-setDone
			if released := <-releaseDone; released {
				t.Fatal("stale tick removed the newer request")
			}
			current, ok := c.GetManualHold("garage", time.Now())
			if !ok || current.PowerW != next.PowerW || current.UpdatedAt == old.UpdatedAt || clears != 0 {
				t.Fatalf("new choice lost: current=%+v active=%v clears=%d", current, ok, clears)
			}
			if !c.releaseManualHoldIfCurrent("garage", current) || clears != 1 {
				t.Fatal("a current release decision must still clear and save")
			}
		})
	}
}

func TestExplicitRetryGetsAFullNewIdleTimeout(t *testing.T) {
	now := time.Now()
	cfg := chargeNowLoadpoint()
	sender := &fakeSender{}
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, RequestActive: false}}
	c := newTestController(t, []Config{cfg}, nil, samples, sender)
	c.SetManualHold(cfg.ID, ManualHold{PowerW: 4140, Persistent: true})
	c.Tick(context.Background(), now)
	c.Tick(context.Background(), now.Add(SessionCompletionTimeout-time.Second))
	old, ok := c.GetManualHold(cfg.ID, now)
	if !ok {
		t.Fatal("old request released before its timeout")
	}
	c.SetManualHold(cfg.ID, old)
	c.Tick(context.Background(), now.Add(SessionCompletionTimeout+time.Second))
	if _, ok := c.GetManualHold(cfg.ID, now); !ok {
		t.Fatal("retry inherited the old request's nearly expired idle timer")
	}
	c.Tick(context.Background(), now.Add(2*SessionCompletionTimeout+2*time.Second))
	if _, ok := c.GetManualHold(cfg.ID, now); ok {
		t.Fatal("retry never released after its own full idle timeout")
	}
}
