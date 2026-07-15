package calendar

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/loadmodel"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/state"
)

// TestEndToEndRealCollaborators drives the calendar service's intent
// application against the REAL loadmodel.Service and loadpoint.Manager (no
// fakes), so a regression in the wiring — wrong profile constant, mis-plumbed
// loadpoint id — is caught. The CalDAV wire itself (REPORT/Expand/PUT/DELETE)
// is covered by the caldav_it-tagged integration test against a real CalDAV
// server; this is the CI-safe complement that proves the planner-side effects.
func TestEndToEndRealCollaborators(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer st.Close()

	loadSvc := loadmodel.NewService(st, nil, "", 5000)
	lpMgr := loadpoint.NewManager()
	lpMgr.Load([]loadpoint.Config{{ID: "garage", VehicleCapacityWh: 60000}})

	s := New(config.CalDAV{Enabled: true}, lpMgr, loadSvc, "garage")
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	// --- Away: the real load model switches to its away profile, and the
	// away-profile prediction is lower than home (planner conserves battery). ---
	homeBefore := loadSvc.PredictWith(now, loadmodel.ProfileHome)
	s.apply(Intents{Away: []Interval{{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}}}, now)
	if loadSvc.Profile() != loadmodel.ProfileAway {
		t.Fatalf("expected real load model to switch to away, got %q", loadSvc.Profile())
	}
	if awayPred := loadSvc.PredictWith(now, loadmodel.ProfileAway); !(awayPred < homeBefore) {
		t.Fatalf("away prediction (%.0f W) should be below home (%.0f W)", awayPred, homeBefore)
	}

	// Leaving the away window restores home on the real model.
	s.apply(Intents{}, now)
	if loadSvc.Profile() != loadmodel.ProfileHome {
		t.Fatalf("expected home profile after the away window, got %q", loadSvc.Profile())
	}

	// --- EV: the real loadpoint manager receives the target SoC + deadline,
	// which is exactly what the MPC loadpoint probe reads. ---
	dep := now.Add(3 * time.Hour)
	s.apply(Intents{EV: []EVDeadline{{LoadpointID: "garage", TargetSoCPct: 80, Departure: dep}}}, now)
	lpState, ok := lpMgr.State("garage")
	if !ok {
		t.Fatal("garage loadpoint not found")
	}
	if lpState.TargetSoCPct != 80 {
		t.Fatalf("loadpoint target SoC: want 80, got %v", lpState.TargetSoCPct)
	}
	if !lpState.TargetTime.Equal(dep) {
		t.Fatalf("loadpoint target time: want %v, got %v", dep, lpState.TargetTime)
	}
}
