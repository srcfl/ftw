package loadpoint

import (
	"testing"
	"time"
)

// The manual-hold saver must fire for operator (Persistent) holds so they can
// be persisted and survive reboot/fw-update, and must fire "cleared" on Clear
// — but NOT persist ephemeral timed holds, and NOT hammer the saver when
// clearing a loadpoint that had no hold (ClearManualHold runs every unplugged
// tick). Stefan 2026-06-11.
func TestManualHoldSaverPersistsOperatorHolds(t *testing.T) {
	c := &Controller{}
	type ev struct {
		id      string
		cleared bool
		power   float64
	}
	var got []ev
	c.SetManualHoldSaver(func(id string, h ManualHold, cleared bool) {
		got = append(got, ev{id, cleared, h.PowerW})
	})

	// 1. Persistent operator hold → saved (cleared=false).
	c.SetManualHold("garage", ManualHold{PowerW: 6210, Persistent: true})
	// 2. Clear → saved (cleared=true).
	c.ClearManualHold("garage")
	// 3. Clear again with no hold → must NOT call saver (no spam on unplugged ticks).
	c.ClearManualHold("garage")
	// 4. Timed (non-persistent) hold → persisted as cleared (ephemeral, must not survive).
	c.SetManualHold("garage", ManualHold{PowerW: 3000, ExpiresAt: time.Now().Add(time.Minute)})

	want := []ev{
		{"garage", false, 6210}, // persistent saved
		{"garage", true, 0},     // clear saved
		// (no entry for the no-op clear)
		{"garage", true, 0}, // timed hold → cleared sentinel
	}
	if len(got) != len(want) {
		t.Fatalf("saver calls = %d (%+v), want %d (%+v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
