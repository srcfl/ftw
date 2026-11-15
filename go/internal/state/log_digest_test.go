package state

import (
	"context"
	"testing"
)

// The digest keeps a week of warnings in a few rows. Two flushes of the
// same message in the same hour must add up, keep the earliest first
// sighting and the latest text, and stay one row.
func TestLogDigestAddsUpRepeatsWithinAnHour(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	const hour = int64(3_600_000) * 500_000
	first := LogDigest{HourMs: hour, Level: "WARN", Driver: "pixii", Pattern: "write failed reg=#",
		Msg: "write failed reg=40", Attrs: "err=timeout", Count: 3, FirstMs: hour + 1_000, LastMs: hour + 9_000}
	if err := st.AddLogDigest(ctx, []LogDigest{first}); err != nil {
		t.Fatal(err)
	}
	later := first
	later.Msg, later.Attrs, later.Count = "write failed reg=41", "err=reset", 2
	later.FirstMs, later.LastMs = hour+60_000, hour+70_000
	other := LogDigest{HourMs: hour + 3_600_000, Level: "ERROR", Pattern: "optimizer: deadline",
		Msg: "optimizer: deadline", Count: 1, FirstMs: hour + 3_700_000, LastMs: hour + 3_700_000}
	if err := st.AddLogDigest(ctx, []LogDigest{later, other}); err != nil {
		t.Fatal(err)
	}

	got, more, err := st.LogDigestBetween(ctx, hour, hour+2*3_600_000, 100)
	if err != nil || more {
		t.Fatalf("LogDigestBetween: more=%v err=%v", more, err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(got), got)
	}
	w := got[0]
	if w.Count != 5 || w.FirstMs != hour+1_000 || w.LastMs != hour+70_000 {
		t.Errorf("repeat row = count %d first %d last %d, want 5, %d, %d", w.Count, w.FirstMs, w.LastMs, hour+1_000, hour+70_000)
	}
	if w.Msg != "write failed reg=41" || w.Attrs != "err=reset" {
		t.Errorf("repeat row keeps %q %q, want the latest text", w.Msg, w.Attrs)
	}
	if got[1].Level != "ERROR" || got[1].Driver != "" {
		t.Errorf("second row = %+v, want the core error", got[1])
	}

	// A window that ends before the second hour leaves it out.
	got, _, err = st.LogDigestBetween(ctx, hour, hour+3_599_999, 100)
	if err != nil || len(got) != 1 {
		t.Fatalf("first hour only: %+v err=%v", got, err)
	}
	// The limit is honoured and says more matched.
	got, more, err = st.LogDigestBetween(ctx, hour, hour+2*3_600_000, 1)
	if err != nil || len(got) != 1 || !more {
		t.Fatalf("limit 1: %d rows more=%v err=%v", len(got), more, err)
	}

	n, err := st.PruneLogDigest(ctx, hour+3_600_000)
	if err != nil || n != 1 {
		t.Fatalf("prune: n=%d err=%v, want the first hour gone", n, err)
	}
	got, _, _ = st.LogDigestBetween(ctx, 0, hour+10*3_600_000, 100)
	if len(got) != 1 || got[0].Level != "ERROR" {
		t.Fatalf("after prune: %+v", got)
	}
}

// The week view counts fallback plans from the stored diagnostics without
// handing an 85 kB blob per replan to Go.
func TestPlannerReplansReadSolverFields(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.SaveDiagnostic(1_000, "scheduled", "SE3", 12.5, 96,
		`{"solver":{"engine":"energyplan","backend":"milp","status":"optimal"}}`); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveDiagnostic(2_000, "soc_drift", "SE3", 14, 96,
		`{"solver":{"engine":"core","backend":"dp","status":"fallback","fallback":true,"fallback_reason":"optimizer timed out"}}`); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveDiagnostic(3_000, "scheduled", "SE3", 9, 96, `{}`); err != nil {
		t.Fatal(err)
	}
	got, err := st.PlannerReplans(ctx, 0, 2_500, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d replans, want 2 inside the window: %+v", len(got), got)
	}
	if got[0].TsMs != 1_000 || got[1].TsMs != 2_000 {
		t.Fatalf("replans not oldest first: %+v", got)
	}
	if got[0].Engine != "energyplan" || got[0].Fallback {
		t.Errorf("optimizer plan = %+v", got[0])
	}
	fb := got[1]
	if !fb.Fallback || fb.Engine != "core" || fb.Backend != "dp" || fb.FallbackReason != "optimizer timed out" || fb.Reason != "soc_drift" {
		t.Errorf("fallback plan = %+v", fb)
	}
	if fb.TotalCostOre != 14 || fb.HorizonSlots != 96 || fb.Zone != "SE3" {
		t.Errorf("fallback summary = %+v", fb.DiagnosticSummary)
	}
	// A plan without solver data reads as empty, not as an error.
	all, err := st.PlannerReplans(ctx, 0, 10_000, 0)
	if err != nil || len(all) != 3 || all[2].Engine != "" || all[2].Fallback {
		t.Fatalf("plan without solver: %+v err=%v", all, err)
	}
	if lim, err := st.PlannerReplans(ctx, 0, 10_000, 1); err != nil || len(lim) != 1 || lim[0].TsMs != 3_000 {
		t.Fatalf("limit keeps the newest: %+v err=%v", lim, err)
	}
}
