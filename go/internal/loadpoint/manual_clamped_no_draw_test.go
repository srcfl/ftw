package loadpoint

import (
	"testing"
	"time"
)

func TestPositiveClampDoesNotHideAcceptedZeroDraw(t *testing.T) {
	at := time.Date(2026, 10, 17, 0, 12, 0, 0, time.UTC)
	h := ManualHold{PowerW: 10350, StartedAt: at}
	st := State{Phases: 3, VoltageV: 230, CommandedKnown: true, CommandedW: 6900, CommandedReason: "fuse_limit", CommandedSinceMs: at.UnixMilli()}
	ch := ChargerReading{Known: true, UpdatedAt: at.Add(8 * time.Minute), LimitKnown: true, LimitA: 10, Reason: "limited by dynamic charger current"}
	m := ManualStatusFrom(h, true, st, ch, at.Add(8*time.Minute))
	if m.State != ManualNotDrawing || m.CommandedA != 10 || m.RequestedA != 15 || m.LimitReason != "fuse_limit" {
		t.Fatalf("positive clamp masks zero delivery: %+v", m)
	}
	st.CommandedW = 0
	ch.LimitA = 0
	if m := ManualStatusFrom(h, true, st, ch, at.Add(8*time.Minute)); m.State != ManualLimited {
		t.Fatalf("intentional zero must remain limited: %+v", m)
	}
}
