package state

import (
	"context"
	"testing"
	"time"
)

// Plain history maintenance is what runs on every current box. It must keep
// planner snapshots to DiagnosticsRecentRetention; without it the table grew
// by about 30 MB a day and every backup copied it (#1403).
func TestPlainMaintenancePrunesOldPlannerDiagnostics(t *testing.T) {
	s := freshStore(t)
	now := time.Now()
	old := now.Add(-DiagnosticsRecentRetention - 24*time.Hour).UnixMilli()
	// More old rows than one delete batch, so the batches must repeat.
	for i := int64(0); i < 100; i++ {
		if err := s.SaveDiagnostic(old+i*60_000, "replan", "SE3", 1, 96, `{"old":true}`); err != nil {
			t.Fatal(err)
		}
	}
	recent := []int64{now.Add(-DiagnosticsRecentRetention + time.Hour).UnixMilli(), now.Add(-time.Hour).UnixMilli()}
	for _, ts := range recent {
		if err := s.SaveDiagnostic(ts, "replan", "SE3", 1, 96, `{"recent":true}`); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.MaintainPlainHistory(context.Background(), now); err != nil {
		t.Fatal(err)
	}

	kept, err := s.LoadDiagnosticsInRange(0, now.UnixMilli(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != len(recent) {
		t.Fatalf("kept %d snapshots, want the %d inside the retention window", len(kept), len(recent))
	}
	for _, d := range kept {
		if d.TsMs < now.Add(-DiagnosticsRecentRetention).UnixMilli() {
			t.Fatalf("snapshot at %d is older than the retention window", d.TsMs)
		}
	}
}
