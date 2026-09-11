package main

import (
	"math"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/loadpoint"
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

// The LAN dashboard splits house and car from every charger that is still
// reporting, including one that cannot take a command. The app snapshot has
// to do the same: hiding that draw in load_w is how the phone showed EV at
// 0 W and house at house+car.
func TestAppSnapshotSplitsEVFromHouseLoad(t *testing.T) {
	tel, ctrl := seedSite(t)
	tel.Update("easee", telemetry.DerEV, 2000, nil, nil)
	tel.RecordDriverSuccess("easee")

	site := &appSite{
		tel: tel, ctrl: ctrl, ctrlMu: &sync.Mutex{},
		revision: &control.Revision{}, started: time.Now(),
		siteMeterStale: time.Minute,
	}
	snap := site.Snapshot()

	if !snap.EVWKnown {
		t.Fatal("a site with a charger did not send field 10")
	}
	if snap.EVW != 2000 {
		t.Fatalf("ev = %v, want 2000", snap.EVW)
	}
	// grid 1200, battery +900, PV -3400, EV 2000 → house 1700.
	if snap.LoadW != 1700 {
		t.Fatalf("load = %v, want 1700 (house, not house+car)", snap.LoadW)
	}
}

func TestAppSnapshotCountsAFaultedChargersDraw(t *testing.T) {
	tel, ctrl := seedSite(t)
	tel.Update("easee", telemetry.DerEV, 2000, nil, nil)
	tel.RecordDriverSuccess("easee")
	tel.SetDriverDeviceFault("easee", true, "setpoint refused")

	site := &appSite{
		tel: tel, ctrl: ctrl, ctrlMu: &sync.Mutex{},
		revision: &control.Revision{}, started: time.Now(),
		siteMeterStale: time.Minute,
	}
	snap := site.Snapshot()

	if !snap.EVWKnown || snap.EVW != 2000 {
		t.Fatalf("ev = %v known=%v; a faulted charger is still drawing", snap.EVW, snap.EVWKnown)
	}
	if snap.LoadW != 1700 {
		t.Fatalf("load = %v; the car's draw landed in the house", snap.LoadW)
	}
}

func TestAppSnapshotIgnoresAnOfflineChargersDraw(t *testing.T) {
	tel, ctrl := seedSite(t)
	tel.Update("easee", telemetry.DerEV, 11400, nil, nil)
	tel.RecordDriverSuccess("easee")
	tel.DriverHealthMut("easee").SetOffline()

	site := &appSite{
		tel: tel, ctrl: ctrl, ctrlMu: &sync.Mutex{},
		revision: &control.Revision{}, started: time.Now(),
		siteMeterStale: time.Minute,
	}
	snap := site.Snapshot()

	if !snap.EVWKnown {
		t.Fatal("an idle-looking charger that exists must still be named")
	}
	if snap.EVW != 0 {
		t.Fatalf("ev = %v; an offline charger's last-known draw leaked", snap.EVW)
	}
	// grid 1200 - bat 900 - pv -3400 = 3700, no EV subtracted.
	if snap.LoadW != 1200-900+3400 {
		t.Fatalf("load = %v, want house without a live car", snap.LoadW)
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

// The port's charge-level path is the HTTP route's: SetCurrentSoC on the
// manager, refused while no car is plugged in, and read back from the
// manager's own state — within the half-permille the handler allows, because
// the manager re-anchors through the session's delivered energy.
func TestAppLoadpointsCorrectTheChargeLevelThroughTheManager(t *testing.T) {
	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{{
		ID: "garage", DriverName: "easee-cloud",
		VehicleCapacityWh: 60000, PluginSoC: 0.4,
	}})
	lp := &appLoadpoints{mgr: mgr}

	if lp.SetSoC("garage", 0.62) {
		t.Fatal("an unplugged car's level was set")
	}

	mgr.Observe("garage", true, 7400, 1200, true) // 1.2 kWh into the session
	if !lp.SetSoC("garage", 0.62) {
		t.Fatal("a plugged-in car's level was refused")
	}
	got, ok := lp.ObservedSoC("garage")
	if !ok || math.Abs(got-0.62) > 0.0005 {
		t.Fatalf("read back %v (known %v), want 0.62", got, ok)
	}
	if _, ok := lp.ObservedSoC("street"); ok {
		t.Fatal("a loadpoint the box does not have read back a level")
	}
}

// The port's PV-only path is the HTTP target route's SetSurplusOnly: the
// previous value comes back so the caller knows the direction, and the
// read-back is the manager's own flag.
func TestAppLoadpointsFlipSurplusOnlyThroughTheManager(t *testing.T) {
	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{{ID: "garage", DriverName: "easee-cloud", SurplusOnly: true}})
	lp := &appLoadpoints{mgr: mgr}

	prev, ok := lp.SetSurplusOnly("garage", false)
	if !ok || !prev {
		t.Fatalf("SetSurplusOnly = (%v, %v), want the previous true", prev, ok)
	}
	if v, ok := lp.ObservedSurplusOnly("garage"); !ok || v {
		t.Fatalf("read back %v (known %v), want off", v, ok)
	}

	prev, ok = lp.SetSurplusOnly("garage", true)
	if !ok || prev {
		t.Fatalf("SetSurplusOnly = (%v, %v), want the previous false", prev, ok)
	}
	if v, ok := lp.ObservedSurplusOnly("garage"); !ok || !v {
		t.Fatalf("read back %v (known %v), want on", v, ok)
	}

	if _, ok := lp.SetSurplusOnly("street", true); ok {
		t.Fatal("a loadpoint the box does not have took a flag")
	}
}
