package main

import (
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func seedSite(t *testing.T) (*telemetry.Store, *control.State) {
	t.Helper()

	tel := telemetry.NewStore()
	for _, name := range []string{"meter", "solar", "battery", "dead-solar"} {
		tel.EnsureDriverHealth(name)
	}
	soc := 0.62
	tel.Update("meter", telemetry.DerMeter, 1200, nil, nil)
	// Negative while generating. FTW's convention is that positive watts flow
	// into the site, so PV is never positive.
	tel.Update("solar", telemetry.DerPV, -3400, nil, nil)
	tel.Update("battery", telemetry.DerBattery, 900, &soc, nil)
	tel.Update("dead-solar", telemetry.DerPV, -5000, nil, nil)
	for _, name := range []string{"meter", "solar", "battery"} {
		tel.RecordDriverSuccess(name)
	}
	tel.DriverHealthMut("dead-solar").SetOffline()

	return tel, &control.State{Mode: control.ModeSelfConsumption, SiteMeterDriver: "meter"}
}

// The signs are the invariant. A PV field that arrived positive would make
// every sentence the app writes about solar the opposite of the truth, and no
// test in the app can catch it because the app believes what it is sent.
func TestAppSnapshotKeepsTheSiteSignConvention(t *testing.T) {
	tel, ctrl := seedSite(t)
	site := &appSite{
		tel: tel, ctrl: ctrl, ctrlMu: &sync.Mutex{},
		revision: &control.Revision{}, started: time.Now(),
		siteMeterStale: time.Minute,
	}

	snap := site.Snapshot()

	if snap.GridW != 1200 {
		t.Fatalf("grid = %v, want 1200 drawing from the grid", snap.GridW)
	}
	if snap.PVW != -3400 {
		t.Fatalf("pv = %v; PV is never positive", snap.PVW)
	}
	if snap.BatteryW != 900 {
		t.Fatalf("battery = %v, want 900 charging", snap.BatteryW)
	}
	// grid = load + battery + pv, rearranged.
	if snap.LoadW != 1200-900+3400 {
		t.Fatalf("load = %v, want %v", snap.LoadW, 1200-900+3400)
	}
	if !snap.BatterySoCKnown || snap.BatterySoC != 0.62 {
		t.Fatalf("soc = %v (known %v), want the fraction 0.62", snap.BatterySoC, snap.BatterySoCKnown)
	}
}

// An offline driver contributes nothing. Adding its last reading would show a
// number that is not happening as though it were.
func TestAppSnapshotIgnoresAnOfflineDriversReading(t *testing.T) {
	tel, ctrl := seedSite(t)
	site := &appSite{
		tel: tel, ctrl: ctrl, ctrlMu: &sync.Mutex{},
		revision: &control.Revision{}, started: time.Now(),
		siteMeterStale: time.Minute,
	}

	snap := site.Snapshot()

	if snap.PVW != -3400 {
		t.Fatalf("pv = %v; the offline array was counted", snap.PVW)
	}

	// It still appears as a source, marked offline. The app needs to say the
	// array went quiet, which it cannot do if the box stops mentioning it.
	var found bool
	for _, src := range snap.Sources {
		if src.ID == "dead-solar" {
			found = true
			if !src.Offline {
				t.Fatal("the offline array is not marked offline")
			}
		}
	}
	if !found {
		t.Fatal("the offline array vanished from the source list")
	}
}

// Stale site-meter data stops dispatch. Naming the source is what lets the app
// say why nothing is happening instead of looking broken.
func TestAnOfflineSiteMeterBlocksDispatch(t *testing.T) {
	tel, ctrl := seedSite(t)
	tel.DriverHealthMut("meter").SetOffline()

	site := &appSite{
		tel: tel, ctrl: ctrl, ctrlMu: &sync.Mutex{},
		revision: &control.Revision{}, started: time.Now(),
		siteMeterStale: time.Minute,
	}

	snap := site.Snapshot()
	if len(snap.DispatchBlockedBy) != 1 || snap.DispatchBlockedBy[0] != "meter" {
		t.Fatalf("dispatchBlockedBy = %v, want the site meter", snap.DispatchBlockedBy)
	}
}

// The revision has to move when the mode does, or the app's expect.rev check
// passes everything and reads like a conflict check that is not one.
func TestAppSnapshotRevisionFollowsTheControlState(t *testing.T) {
	tel, ctrl := seedSite(t)
	site := &appSite{
		tel: tel, ctrl: ctrl, ctrlMu: &sync.Mutex{},
		revision: &control.Revision{}, started: time.Now(),
		siteMeterStale: time.Minute,
	}

	before := site.Snapshot().ControlRev
	if same := site.Snapshot().ControlRev; same != before {
		t.Fatalf("the revision moved on an unchanged site: %d → %d", before, same)
	}

	ctrl.Mode = control.ModePeakShaving
	if after := site.Snapshot().ControlRev; after <= before {
		t.Fatalf("the revision is %d after a mode change, was %d", after, before)
	}
}

// A mode change is three things. Arriving with two of them has already cost a
// night's battery, which is why every door goes through control.ApplyMode.
func TestAppSetModeGoesThroughApplyMode(t *testing.T) {
	_, ctrl := seedSite(t)
	ctrl.PI = &control.PIController{}
	ctrl.SetBatteryManualHold(control.BatteryManualHold{
		Driver: "battery", PowerW: -2000, ExpiresAt: time.Now().Add(time.Hour),
	})

	modes := &appModes{ctrl: ctrl, ctrlMu: &sync.Mutex{}}
	if err := modes.SetMode(t.Context(), control.ModePeakShaving); err != nil {
		t.Fatalf("SetMode: %v", err)
	}

	if ctrl.Mode != control.ModePeakShaving {
		t.Fatalf("mode = %q", ctrl.Mode)
	}
	if _, active := ctrl.GetBatteryManualHold(time.Now()); active {
		t.Fatal("the manual hold survived a mode change")
	}
}

func TestAppSetModeRefusesAModeTheBoxDoesNotHave(t *testing.T) {
	_, ctrl := seedSite(t)
	modes := &appModes{ctrl: ctrl, ctrlMu: &sync.Mutex{}}

	if err := modes.SetMode(t.Context(), control.Mode("turbo")); err == nil {
		t.Fatal("an unknown mode was accepted")
	}
	if ctrl.Mode != control.ModeSelfConsumption {
		t.Fatalf("a refused mode still changed the state to %q", ctrl.Mode)
	}
}
