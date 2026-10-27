package cars

import (
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
)

// A car merged from a vehicles[] profile with target_soc keeps a one-off
// goal's deadline across a plug-in.
func TestProfileTargetKeepsOneOffDeadline(t *testing.T) {
	h := newHarness(t, homeConfig(), nil, nil)
	h.start(Profile{ID: "ioniq", Name: "Ioniq", CapacityWh: 58000, Identifiers: []string{"RFID-9"}, TargetSoC: .9})
	if c := h.only(); c.Name != "Ioniq" {
		t.Fatalf("not merged: %+v", c)
	}
	// One-off goal: 80 % by 07:00 tomorrow.
	if ok, err := h.mgr.SetScheduleChecked("easee", loadpoint.NewSchedule(.8, 420, false, 0, false)); !ok || err != nil {
		t.Fatal(ok, err)
	}
	h.tick(time.Second)
	before := h.state("easee")
	if before.TargetTime.IsZero() || before.TargetSoC != .8 {
		t.Fatalf("goal not applied before plug-in: %.2f by %s", before.TargetSoC, before.TargetTime)
	}
	h.plug("easee", "s1")
	for i := 0; i < 3; i++ {
		h.tick(5 * time.Second)
	}
	after := h.state("easee")
	if after.TargetTime.IsZero() || after.TargetSoC != .8 {
		t.Fatalf("plug-in erased the one-off goal: before %.2f by %s, after %.2f by %s",
			before.TargetSoC, before.TargetTime, after.TargetSoC, after.TargetTime)
	}
}

// Without a goal, the profile target still applies to the session.
func TestProfileTargetAppliesWithoutGoal(t *testing.T) {
	h := newHarness(t, homeConfig(), nil, nil)
	h.start(Profile{ID: "ioniq", Name: "Ioniq", CapacityWh: 58000, Identifiers: []string{"RFID-9"}, TargetSoC: .9})
	h.tick(time.Second)
	h.plug("easee", "s1")
	h.tick(5 * time.Second)
	if st := h.state("easee"); st.TargetSoC != .9 || !st.TargetTime.IsZero() {
		t.Fatalf("profile target not applied: %.2f by %s", st.TargetSoC, st.TargetTime)
	}
}
