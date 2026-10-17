package main

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/srcfl/ftw/go/internal/api"
	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/appuplink"
	"github.com/srcfl/ftw/go/internal/cars"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/localzone"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/prices"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"github.com/srcfl/ftw/go/protocol"
)

// The FTW app's uplink, wired to the box.
//
// Everything in this file is adapters: appproto asks narrow questions and this
// answers them from telemetry, control and the planner. No safety decision is
// made here. The dispatch gate stays in control, the mode change goes through
// control.ApplyMode, and the planner's output is never handed to hardware from
// this path.

// processStarted anchors every age the app is shown.
//
// Initialised at load, not when the uplink is wired, because a driver that
// last answered during startup would otherwise report a negative age — which
// appproto reads as "has never answered", and the freshness display is the one
// thing this protocol exists to get right.
var processStarted = time.Now()

// appSite is the box's live state as the message layer wants it.
type appSite struct {
	tel      *telemetry.Store
	ctrl     *control.State
	ctrlMu   *sync.Mutex
	revision *control.Revision
	started  time.Time
	// siteMeterStale is how long a site-meter reading stays current. It is
	// the watchdog timeout, so the app's freshness claim and the box's own
	// dispatch gate are answering from the same number.
	siteMeterStale time.Duration
}

func (a *appSite) Snapshot() appproto.Snapshot {
	a.ctrlMu.Lock()
	mode := a.ctrl.Mode
	siteMeterDriver := a.ctrl.SiteMeterDriver
	rev := a.revision.Observe(a.ctrl)
	fuseA, fusePhases := a.ctrl.SiteFuseAmps, a.ctrl.SiteFusePhases
	a.ctrlMu.Unlock()

	snap := appproto.Snapshot{Mode: mode, ControlRev: rev, FuseA: fuseA}

	uptimeMs := time.Since(a.started).Milliseconds()
	seen := map[string]bool{}
	addSource := func(driver string) {
		if driver == "" || seen[driver] {
			return
		}
		seen[driver] = true

		src := appproto.Source{ID: driver, Kind: "driver", Name: driver, LastOkUptimeMs: -1}
		health := a.tel.DriverHealth(driver)
		if health == nil {
			snap.Sources = append(snap.Sources, src)
			return
		}
		stale := a.siteMeterStale
		if health.WatchdogTimeoutOverride > 0 {
			// A cloud-polled inverter and a P1 meter are not comparable, so
			// the driver's own cadence decides when its reading goes stale
			// rather than one number for the whole site.
			stale = health.WatchdogTimeoutOverride
		}
		src.StaleAfterMs = stale.Milliseconds()
		src.Offline = !health.IsOnline()
		if health.LastSuccess != nil {
			src.LastOkUptimeMs = uptimeMs - time.Since(*health.LastSuccess).Milliseconds()
		}
		snap.Sources = append(snap.Sources, src)
	}

	if siteMeterDriver != "" {
		addSource(siteMeterDriver)
		if health := a.tel.DriverHealth(siteMeterDriver); health != nil && health.IsOnline() {
			if reading := a.tel.Get(siteMeterDriver, telemetry.DerMeter); reading != nil {
				snap.GridW = reading.SmoothedW
				snap.GridPhaseA = gridPhaseAmps(reading, fusePhases)
			}
		} else {
			// The invariant is that stale site-meter data stops dispatch.
			// Saying which source is responsible is what lets the app tell
			// the user why nothing is happening instead of looking broken.
			snap.DispatchBlockedBy = append(snap.DispatchBlockedBy, siteMeterDriver)
		}
	}

	for _, reading := range a.tel.ReadingsByType(telemetry.DerPV) {
		addSource(reading.Driver)
		if health := a.tel.DriverHealth(reading.Driver); health != nil && health.IsOnline() {
			// Site convention: positive watts flow into the site, so PV is
			// negative while generating. No sign conversion happens here —
			// the driver boundary already did it, and doing it twice is how
			// a solar panel starts consuming power.
			snap.PVW += reading.SmoothedW
		}
	}

	var socTotal float64
	var socCount int
	for _, reading := range a.tel.ReadingsByType(telemetry.DerBattery) {
		addSource(reading.Driver)
		health := a.tel.DriverHealth(reading.Driver)
		if health == nil || !health.IsOnline() {
			continue
		}
		snap.BatteryW += reading.SmoothedW
		if reading.SoC != nil {
			socTotal += *reading.SoC
			socCount++
		}
	}
	if socCount > 0 {
		snap.BatterySoCKnown = true
		// Already a fraction in [0,1] — telemetry stores battery SoC that
		// way, and the wire carries permille, converted at the edge of
		// appproto. Scaling here would put the battery at 5 % of its charge.
		snap.BatterySoC = socTotal / float64(socCount)
	}

	// EV chargers. Known means the site has one at all — an idle charger
	// is a real 0 W reading, a site without one sends no field 10 and the
	// app draws no EV node. Positive while charging: a charger consumes
	// like any other load. The watts come from SumOnlineEVW so a charger
	// that cannot take a command but is still drawing is counted here the
	// same way the LAN dashboard counts it — IsOnline() would hide that
	// draw in the house node.
	for _, reading := range a.tel.ReadingsByType(telemetry.DerEV) {
		addSource(reading.Driver)
		snap.EVWKnown = true
	}
	snap.EVW = a.tel.SumOnlineEVW()

	// grid = load + battery + pv + ev + v2x, all site-signed. Rearranged,
	// not re-derived: a second formula here would be a second thing to
	// keep in step with docs/site-convention.md. House load is never
	// negative; metering noise that would push it below zero is the car
	// or the battery, not the house consuming in reverse.
	snap.LoadW = snap.GridW - snap.BatteryW - snap.PVW -
		snap.EVW - a.tel.SumOnlineV2XW()
	if snap.LoadW < 0 {
		snap.LoadW = 0
	}

	return snap
}

// gridPhaseAmps reads the site meter's current on each of the site's phases
// and gives it the site convention's sign.
//
// Meters differ here. Some sign the current; many, Sungrow among them, give
// only its size. The power on the same phase already carries the site's
// sign, because the driver converted it, so where the meter reports that
// power its sign is the phase's direction. Otherwise the driver's own sign
// stands. This converts no vendor sign: it copies a site sign onto a size,
// and a current the driver already signed keeps its sign.
//
// Only the site's phases are read, in L1 to L3 order, as the fuse guard
// reads them: a one-phase site owns no L2, whatever the payload holds.
func gridPhaseAmps(r *telemetry.DerReading, phases int) [3]*float64 {
	var out [3]*float64
	if r == nil || len(r.Data) == 0 {
		return out
	}
	var d struct {
		L1A *float64 `json:"l1_a"`
		L2A *float64 `json:"l2_a"`
		L3A *float64 `json:"l3_a"`
		L1W *float64 `json:"l1_w"`
		L2W *float64 `json:"l2_w"`
		L3W *float64 `json:"l3_w"`
	}
	if err := json.Unmarshal(r.Data, &d); err != nil {
		return out
	}
	if phases <= 0 || phases > len(out) {
		phases = len(out)
	}
	amps := [3]*float64{d.L1A, d.L2A, d.L3A}
	watts := [3]*float64{d.L1W, d.L2W, d.L3W}
	finite := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
	for i := 0; i < phases; i++ {
		if amps[i] == nil || !finite(*amps[i]) {
			continue
		}
		a := *amps[i]
		if w := watts[i]; w != nil && finite(*w) && *w != 0 {
			a = math.Copysign(math.Abs(a), *w)
		}
		out[i] = &a
	}
	return out
}

// appBoxInfo is what the box calls itself.
type appBoxInfo struct {
	id    string
	build string
	tz    string
}

// newAppBoxInfo names the box's own zone as the host sets it. The app renders
// plan slot times in it, so a box in Stockholm and a phone abroad still agree
// on when tonight's cheap hours are.
func newAppBoxInfo(id, build string) appBoxInfo {
	return appBoxInfo{id: id, build: build, tz: localzone.Name()}
}

func (a appBoxInfo) Identity() appproto.Identity {
	return appproto.Identity{ID: a.id, Build: a.build, TZ: a.tz}
}

// Boot returns nil because by the time the uplink is running the box can serve
// telemetry. A box still starting has not reached this code, so claiming
// otherwise would be a spinner that never resolves.
func (a appBoxInfo) Boot() *appproto.BootProgress { return nil }

// appModes applies a mode and reads it back. Home Assistant's mode command
// uses the same SetMode, so the two doors cannot drift apart.
//
// SetMode and ObservedMode are deliberately different questions: the first is
// what was asked for, the second is what the box is running. cmd.result
// reports the read-back, so a mode the box refused never appears as applied.
type appModes struct {
	ctrl   *control.State
	ctrlMu *sync.Mutex
	state  *state.Store
	mpc    *mpc.Service
	prefs  *config.PlannerPrefs
}

func (a *appModes) SetMode(ctx context.Context, m control.Mode) error {
	a.ctrlMu.Lock()
	err := a.ctrl.ApplyMode(m)
	a.ctrlMu.Unlock()
	if err != nil {
		return err
	}

	if a.state != nil {
		if err := a.state.SaveConfig("mode", string(m)); err != nil {
			slog.Warn("could not persist the mode", "mode", m, "err", err)
		}
	}
	if a.prefs != nil {
		var save func(string, string) error
		if a.state != nil {
			save = a.state.SaveConfig
		}
		a.prefs.ApplyExportFromMode(string(m), save)
	}
	if mm, ok := control.PlannerMPCMode(m); ok && a.mpc != nil {
		// mpc.SetMode replans in the background and returns at once, so a
		// slow planner cannot make an applied mode change look
		// "unconfirmed", and a phone that drops its socket cannot abort
		// the replan. The plan push that follows reaches the app on its own.
		a.mpc.SetMode(mm)
	}
	return nil
}

func (a *appModes) ObservedMode() (control.Mode, bool) {
	a.ctrlMu.Lock()
	defer a.ctrlMu.Unlock()
	return a.ctrl.Mode, true
}

// appHistory serves the app's history tiles from the energy ledger.
//
// The ledger stores energy per flow per bucket; the wire wants mean power per
// signed series. The mapping below is the site convention and nothing else:
// positive watts flow into the site, so import minus export is the grid, and
// PV is generation negated. W = Wh × 3 600 000 / bucket, no other arithmetic.
type appHistory struct {
	st *state.Store
}

func (a *appHistory) Window(
	ctx context.Context, names []string, stepMs, fromMs, toMs int64,
) (map[string][]appproto.HistorySample, error) {
	type term struct {
		flow state.EnergyFlow
		sign float64
	}
	termsByName := map[string][]term{
		"grid_w":    {{state.FlowGridImport, 1}, {state.FlowGridExport, -1}},
		"pv_w":      {{state.FlowPVGeneration, -1}},
		"battery_w": {{state.FlowBatteryCharge, 1}, {state.FlowBatteryDischarge, -1}},
		"load_w":    {{state.FlowConsumerUse, 1}},
	}

	points, _, err := a.st.LoadEnergyHistoryContext(ctx, state.EnergyHistoryQuery{
		SinceMS:  fromMs,
		UntilMS:  toMs,
		BucketMS: stepMs,
		// One tile is at most 168 buckets; the ledger's own cap is 2000.
		Limit: 2000,
	})
	if err != nil {
		return nil, err
	}

	// A bucket exists on the wire only if some term reported it. A bucket
	// where nothing was metered stays absent and becomes MISSING, which is
	// the honest answer — zero would claim the house used no power at all.
	byName := make(map[string]map[int64]float64, len(names))
	for _, name := range names {
		if termsByName[name] != nil {
			byName[name] = map[int64]float64{}
		}
	}
	for _, p := range points {
		for _, name := range names {
			terms := termsByName[name]
			for _, t := range terms {
				if p.Flow != t.flow {
					continue
				}
				byName[name][p.BucketStartMS] += t.sign * p.EnergyWh * 3_600_000 / float64(p.BucketLenMS)
			}
		}
	}

	out := make(map[string][]appproto.HistorySample, len(byName))
	for name, byStart := range byName {
		series := make([]appproto.HistorySample, 0, len(byStart))
		for start, w := range byStart {
			series = append(series, appproto.HistorySample{StartMs: start, W: w})
		}
		out[name] = series
	}
	return out, nil
}

// appPrices serves the app's price window from the price service.
//
// Nothing is rounded here. The service holds minor units per kWh as floats and
// appproto turns them into integers at the wire, once — two roundings would be
// two answers to what 18.7 öre costs.
type appPrices struct {
	svc *prices.Service
}

// maxPriceSlotMin is the longest slot any provider publishes — the ENTSOE
// reader refuses anything longer. It is how far back a query has to look to
// catch the slot already running when the window opens.
const maxPriceSlotMin = 120

func (a *appPrices) Zone() string     { return a.svc.Zone }
func (a *appPrices) Currency() string { return a.svc.Currency }

func (a *appPrices) Slots(_ context.Context, fromMs, toMs int64) ([]appproto.PricePoint, error) {
	// The store selects on slot start, inclusive at both ends. Widening the
	// lower bound by one slot catches the slot in progress at fromMs — the
	// price right now, which a window starting at "now" would otherwise miss —
	// and toMs-1 keeps the upper end half-open, so a slot beginning exactly
	// where this window closes belongs to the next one.
	lookbackMs := (maxPriceSlotMin * time.Minute).Milliseconds()
	rows, err := a.svc.Load(fromMs-lookbackMs, toMs-1)
	if err != nil {
		return nil, err
	}

	out := make([]appproto.PricePoint, 0, len(rows))
	for _, r := range rows {
		if r.SlotLenMin <= 0 {
			// A slot with no length is a row nobody can draw or price.
			continue
		}
		slotEnd := time.UnixMilli(r.SlotTsMs).Add(time.Duration(r.SlotLenMin) * time.Minute)
		if slotEnd.UnixMilli() <= fromMs {
			continue // the lookback overshot: this slot ended before the window
		}
		out = append(out, appproto.PricePoint{
			StartMs:    r.SlotTsMs,
			LenMin:     r.SlotLenMin,
			SpotMinor:  r.SpotOreKwh,
			TotalMinor: r.TotalOreKwh,
		})
	}
	return out, nil
}

// appLoadpoints is the command lane's way to the box's EV charging: the same
// loadpoint controller the HTTP routes call, behind appproto's narrow
// questions. Boost carries the same replan nudge the HTTP enable route has —
// the cancel path needs none here because the controller's stopped hook
// already replans on every active-to-stopped transition, whichever door
// asked.
type appLoadpoints struct {
	mgr  *loadpoint.Manager
	ctrl *loadpoint.Controller
	mpc  *mpc.Service
	cars *cars.Runtime
}

func (a *appLoadpoints) WaitForPersistence(ctx context.Context) error {
	return a.mgr.WaitForPersistence(ctx)
}

func (a *appLoadpoints) Exists(id string) bool {
	_, ok := a.mgr.State(id)
	return ok
}

func (a *appLoadpoints) Hold(id string, h loadpoint.ManualHold) {
	a.ctrl.SetManualHold(id, h)
}

func (a *appLoadpoints) ClearHold(id string) {
	a.ctrl.ClearManualHold(id)
}

func (a *appLoadpoints) ObservedHold(id string, now time.Time) (loadpoint.ManualHold, bool) {
	return a.ctrl.GetManualHold(id, now)
}

func (a *appLoadpoints) HoldIfOwn(id string, h loadpoint.ManualHold, now time.Time) bool {
	return a.ctrl.SetManualHoldIfOwn(id, h, now)
}

func (a *appLoadpoints) ClearHoldIfOwn(id, setBy string, now time.Time) bool {
	return a.ctrl.ClearManualHoldIfOwn(id, setBy, now)
}

func (a *appLoadpoints) Boost(id string, lease loadpoint.BatteryBoostLease, now time.Time) error {
	if _, err := a.ctrl.EnableBatteryBoost(id, lease, now); err != nil {
		return err
	}
	if a.mpc != nil {
		// The same nudge the HTTP enable gives, for the same reason: a boost
		// changes what moves, and the planner should know now rather than at
		// its next scheduled run. Off this goroutine and unattached to the
		// session — a phone that drops its socket right after tapping must
		// not abort the planner mid-run.
		a.mpc.RequestReplan("loadpoint_battery_boost_enabled")
	}
	return nil
}

func (a *appLoadpoints) CancelBoost(id string, now time.Time) {
	a.ctrl.CancelBatteryBoost(id, now)
}

func (a *appLoadpoints) ObservedBoost(id string, now time.Time) loadpoint.BatteryBoostStatus {
	_, status := a.ctrl.BatteryBoost(id, now)
	return status
}

func (a *appLoadpoints) SetSoC(id string, soc float64) bool {
	if !a.mgr.SetCurrentSoC(id, soc) {
		return false
	}
	if a.mpc != nil {
		a.mpc.RequestReplan("loadpoint_soc_corrected")
	}
	return true
}

// Readings is every charger as the manager holds it. The car's level counts
// only while it is plugged in: unplugged, the manager's number is the last
// session's, and a watch would draw it as now. A visitor's car has none.
func (a *appLoadpoints) Readings() []appproto.LoadpointReading {
	states := a.mgr.States()
	out := make([]appproto.LoadpointReading, 0, len(states))
	for _, st := range states {
		r := appproto.LoadpointReading{
			ID: st.ID, Plugged: st.PluggedIn,
			SoC: st.CurrentSoC, SoCKnown: st.PluggedIn && !st.SoCUnknown,
			PowerW: st.CurrentPowerW,
		}
		if a.cars != nil {
			r.Car = a.cars.ChargerCar(st.ID)
			if v, ok := a.cars.Session(st.ID); ok && v.Source != "" && r.SoCKnown {
				// The level the car shows, and where it came from.
				r.SoC, r.SoCSource, r.SoCEstimate = v.Shown, v.Source, v.Estimate
				if !v.At.IsZero() {
					age := max(0, int64(time.Since(v.At)/time.Second))
					r.SoCAgeS = &age
				}
			}
		}
		out = append(out, r)
	}
	return out
}

func (a *appLoadpoints) ObservedSoC(id string) (float64, bool) {
	st, ok := a.mgr.State(id)
	return st.CurrentSoC, ok
}

func (a *appLoadpoints) SetSurplusOnly(id string, v bool) (bool, bool) {
	prev, ok := a.mgr.SetSurplusOnly(id, v)
	if !ok {
		return false, false
	}
	if a.mpc != nil {
		if prev && !v {
			slog.Info("loadpoint surplus_only disabled — requesting replan", "lp", id)
			a.mpc.RequestReplan("surplus_only_disabled")
		} else {
			a.mpc.RequestReplan("loadpoint_target_changed")
		}
	}
	return prev, true
}

func (a *appLoadpoints) ObservedSurplusOnly(id string) (bool, bool) {
	st, ok := a.mgr.State(id)
	return st.SurplusOnly, ok
}

// appPlans hands over the planner's current output.
type appPlans struct {
	planner *mpc.Service
	ctrl    *control.State
	ctrlMu  *sync.Mutex
	rev     uint64
	revMu   sync.Mutex
	lastGen int64
}

func (a *appPlans) Latest() *mpc.Plan {
	if a.planner == nil {
		return nil
	}
	return a.planner.Latest()
}

// Rev moves when the planner replans, so the app can tell a fresh plan from a
// redelivery of the same one. Derived from the plan's own timestamp because
// mpc has no revision of its own and inventing one there would be a field two
// packages had to keep in step.
func (a *appPlans) Rev() uint64 {
	plan := a.Latest()
	if plan == nil {
		return 0
	}

	a.revMu.Lock()
	defer a.revMu.Unlock()
	if plan.GeneratedAtMs != a.lastGen {
		a.lastGen = plan.GeneratedAtMs
		a.rev++
	}
	return a.rev
}

func (a *appPlans) CeilingW() *int64 {
	a.ctrlMu.Lock()
	defer a.ctrlMu.Unlock()
	if a.ctrl.PeakImportCeilingW <= 0 {
		return nil
	}
	ceiling := int64(a.ctrl.PeakImportCeilingW)
	return &ceiling
}

// appPlanChargers is each charger's share of the plan, read from the same plan
// snapshot and the same windows GET /api/loadpoints serves. Keyed by the
// charger, with the session's car while the box knows it.
type appPlanChargers struct {
	planner *mpc.Service
	mgr     *loadpoint.Manager
	ctrl    *loadpoint.Controller
}

func (a *appPlanChargers) PlanChargers(now time.Time) []appproto.PlanCharger {
	snap := a.planner.PlanSnapshot()
	// While a newer plan is being made the box goes on acting on the one it
	// has, so its windows stay, marked pending. The HTTP route empties them.
	acting := snap
	if snap.Pending && !snap.Unproven {
		acting.Outdated = false
	}
	states := a.mgr.States()
	out := make([]appproto.PlanCharger, 0, len(states))
	for _, st := range states {
		ch := appproto.PlanCharger{
			ID: st.ID, CarID: st.CarID, PluggedIn: st.PluggedIn,
			Pending: snap.Pending, Outdated: snap.Outdated,
			Windows: []appproto.PlanChargerWindow{},
			GoalRev: goalRev(st),
		}
		if a.ctrl != nil && !acting.Outdated {
			ch.GridDeferred = a.ctrl.GridDeferred(st.ID)
		}
		windows, totalWh := acting.LoadpointPlanWindows(st.ID, now, api.MaxPlanWindows)
		for _, w := range windows {
			ch.Windows = append(ch.Windows, appproto.PlanChargerWindow{
				StartMs: w.Start.UnixMilli(), EndMs: w.End.UnixMilli(), Wh: int64(math.Round(w.EnergyWh)),
			})
		}
		ch.TotalWh = int64(math.Round(totalWh))
		if snap.Plan != nil && !snap.Pending && !snap.Outdated {
			if wh, ok := snap.Plan.LoadpointShortfallWh[st.ID]; ok && !math.IsNaN(wh) && !math.IsInf(wh, 0) {
				short := int64(math.Round(max(0, wh)))
				ch.ShortfallWh = &short
			}
		}
		out = append(out, ch)
	}
	return out
}

// goalRev changes when a charger's goal, ready time, schedule or solar-only
// choice changes, wherever that was done, so a phone knows to read it again.
func goalRev(st loadpoint.State) uint32 {
	raw, _ := json.Marshal(struct {
		Schedule      loadpoint.Schedule
		TargetSoC     float64
		TargetTimeMs  int64
		SurplusOnly   bool
		FinishAtLimit bool
	}{st.Schedule, st.TargetSoC, st.TargetTime.UnixMilli(), st.SurplusOnly, st.FinishAtVehicleLimit})
	h := fnv.New32a()
	_, _ = h.Write(raw)
	return h.Sum32()
}

// startAppLink prepares app sessions for the FTW Cloud tunnel when enabled.
// Config defaults an omitted app_link section on.
//
// It returns the enrollment identity whether or not the uplink runs, because
// the QR code on the lid is minted from it and a box with the link switched
// off still has to be pairable before anyone can switch it on.
func startAppLink(
	ctx context.Context,
	cfg *config.Config,
	identityKeyPath string,
	boxID string,
	build string,
	st *state.Store,
	tel *telemetry.Store,
	planner *mpc.Service,
	lpMgr *loadpoint.Manager,
	lpCtrl *loadpoint.Controller,
	carsRT *cars.Runtime,
	priceSvc *prices.Service,
	ctrl *control.State,
	ctrlMu *sync.Mutex,
	prefs *config.PlannerPrefs,
	revision *control.Revision,
	siteMeterStale time.Duration,
	gateway *lateAPI,
	cloud *cloudSession,
	agents *agentPorts,
) (*appenroll.Identity, *appuplink.Uplink, bool, error) {
	enabled := cfg != nil && cfg.AppLink.On()

	enroll, err := appenroll.LoadOrCreate(identityKeyPath)
	if err != nil {
		return nil, nil, enabled, err
	}
	// Agent rows expire by the wall clock, and do nothing without one, nor
	// below the highest time the box has seen.
	agentWall.load(st)
	enroll.SetClock(agentClock)
	agents.key = agentKey(enroll)
	enroll.OnOwnerAdded(func() { agents.push(protocol.PushOwnerAdded, "") })
	if !enabled {
		return enroll, nil, false, nil
	}

	site := &appSite{
		tel: tel, ctrl: ctrl, ctrlMu: ctrlMu, revision: revision,
		started: processStarted, siteMeterStale: siteMeterStale,
	}
	info := newAppBoxInfo(boxID, build)
	modes := &appModes{ctrl: ctrl, ctrlMu: ctrlMu, state: st, mpc: planner, prefs: prefs}
	plans := &appPlans{planner: planner, ctrl: ctrl, ctrlMu: ctrlMu}

	// History rides on the energy ledger, so it exists exactly when state
	// does. The capability is advertised in the same breath: a capability the
	// box advertises but cannot answer is worse than one it never mentions.
	var history appproto.HistoryProvider
	caps := appproto.DefaultCaps()
	if st != nil {
		history = &appHistory{st: st}
		caps = append(caps,
			appproto.CapHistory5m,
			appproto.CapHistory1h,
			appproto.CapHistoryEtag,
		)
	}

	// EV charging rides on the loadpoint controller, so the two loadpoint
	// ops work exactly when the HTTP routes do. Without one the port stays
	// nil, the ops answer E_UNAVAILABLE, and der.ev stays unsaid so the app
	// hides the buttons rather than drawing dead ones.
	var loadpoints appproto.Loadpoints
	if lpMgr != nil && lpCtrl != nil {
		loadpoints = &appLoadpoints{mgr: lpMgr, ctrl: lpCtrl, mpc: planner, cars: carsRT}
		caps = append(caps, appproto.CapDerEv)
	}
	// Cars ride with the chargers (docs/cars.md). The box says der.car when
	// it keeps them, and the car ops answer.
	var carPort appproto.Cars
	if carsRT != nil && loadpoints != nil {
		carPort = carsRT
		caps = append(caps, appproto.CapDerCar)
	}

	// The plan reaches every streaming phone the moment the planner
	// replaces it, with each charger's windows, so a car plugged in shows
	// its plan without a read.
	var planChargers appproto.PlanChargers
	var planFeed *appproto.PlanFeed
	if planner != nil {
		planFeed = appproto.NewPlanFeed(appproto.PlanSettle)
		planner.OnPlanChange(planFeed.Changed)
		if lpMgr != nil {
			planChargers = &appPlanChargers{planner: planner, mgr: lpMgr, ctrl: lpCtrl}
		}
	}

	// Prices ride on the price service, and only when it has a zone and a
	// store to have fetched into. With either missing there is nothing to
	// serve, so the capability stays unsaid rather than advertised and empty.
	var priceReader appproto.PriceReader
	if priceSvc != nil && priceSvc.Zone != "" && priceSvc.Store != nil {
		priceReader = &appPrices{svc: priceSvc}
		caps = append(caps, appproto.CapPriceSpot)
	}

	// The box's own HTTP API, reachable over the session. Bound after this
	// returns, because the API server is built from dependencies that are not
	// assembled yet; until then it answers 503 and the app says the box is
	// starting, which is true.
	caps = append(caps, appproto.CapApiPassthrough)

	// One command log for the box. Sessions come and go with their sockets,
	// and a retry in the next session must meet the id the last one took.
	cmds := appproto.NewCmdLog()

	// Agents (docs/agents.md): an owner's grants to an assistant, Sourceful
	// support or the release check. They reach the box through FTW Cloud's
	// tunnel only, and every call meets its grant here.
	agents.access = &appuplink.AgentAccess{Enroll: enroll, Clock: agentClock, Push: agents.push,
		Holds: endAgentHolds(loadpoints)}
	agents.state = appproto.NewAgentState()
	if st != nil {
		agents.audit = newStateAudit(ctx, st)
	}
	caps = append(caps, appproto.CapAccessAgents)
	// One view for every agent session, so a charger's plug-in count is
	// the same whichever session reads it.
	views := agentViews{site: &cloudSite{site: site, api: gateway, cars: carsRT}, cars: carsRT}

	// handlerFor builds one handler for one session: a phone's stream, or a
	// watch's exchange. They share the ports above, which are read-only
	// apart from the mode and the command log, and the mode goes through
	// control's own validation.
	handlerFor := func() appuplink.HandlerBuilder {
		return func(
			sender appproto.Sender,
			caller apiauth.Caller,
			grants appproto.GrantReader,
		) (*appproto.Handler, error) {
			return appproto.New(appproto.Config{
				Clock:        appproto.SystemClock{StartedAt: site.started, Source: "ntp"},
				Site:         site,
				Info:         info,
				Modes:        modes,
				Plans:        plans,
				PlannerPrefs: gateway,
				History:      history,
				Prices:       priceReader,
				Loadpoints:   loadpoints,
				PlanChargers: planChargers,
				PlanFeed:     planFeed,
				Cars:         carPort,
				API:          gateway,
				Caller:       caller,
				Grants:       grants,
				Commands:     cmds,
				Access:       agents.access,
				Agents:       agents.state,
				Audit:        agents.audit,
				Logs:         ringLogs{ring: agents.logs},
				Redactor:     agents.redactor,
				Views:        views,
				AgentKey:     agents.key,
				ConfigDoc:    agents.configDoc,
				// FTW Cloud is asked per session: its link is bound after
				// this returns, and its grant moves while the box runs.
				Caps:   cloud.caps(caps),
				Cloud:  cloud.info,
				Codec:  appuplink.Codec(),
				Sender: sender,
				// The three frozen power fields point at the source whose
				// freshness governs them. The site meter is the only one the
				// box can name without knowing the site's hardware.
				SrcGrid:    siteMeterName(ctrl, ctrlMu),
				SrcPV:      siteMeterName(ctrl, ctrlMu),
				SrcBattery: siteMeterName(ctrl, ctrlMu),
			})
		}
	}

	uplink, err := appuplink.New(appuplink.Options{
		Enroll:   enroll,
		Handler:  handlerFor(),
		Arrivals: arrivalsFor(ctx, tel),
	})
	if err != nil {
		return enroll, nil, enabled, err
	}
	agents.access.Drop = func(key []byte) { uplink.DropSessionsByAppKey(key) }
	agents.boxPage = func(by string) (*appproto.Handler, error) {
		return handlerFor()(discardSender{}, boxPageCaller(by), boxPageGrant{})
	}
	go sweepAgents(ctx, agents)

	go func() {
		if err := uplink.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("app uplink stopped")
		}
	}()

	return enroll, uplink, true, nil
}

// arrivalsFor hears each finished driver poll until ctx ends, or nothing
// without a store.
func arrivalsFor(ctx context.Context, tel *telemetry.Store) <-chan struct{} {
	if tel == nil {
		return nil
	}
	arrivals, stop := tel.Arrivals()
	context.AfterFunc(ctx, stop)
	return arrivals
}

func siteMeterName(ctrl *control.State, ctrlMu *sync.Mutex) string {
	ctrlMu.Lock()
	defer ctrlMu.Unlock()
	return ctrl.SiteMeterDriver
}

// lateAPI is the box's HTTP API as the app session sees it.
//
// Late-bound because the uplink starts before the API server is assembled.
// Until it is bound every request answers 503, which the app already renders
// as "your box is starting, this takes a few minutes after an update" — an
// honest sentence rather than a silent hang.
//
// It is the same handler the LAN listener serves, trust boundary and all. The
// passthrough deliberately does not reach past it to the bare mux: one door
// with one set of checks, whichever side the request came in on.
type lateAPI struct {
	srv atomic.Pointer[api.Server]
}

func (l *lateAPI) bind(srv *api.Server) { l.srv.Store(srv) }

func (l *lateAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	srv := l.srv.Load()
	if srv == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"the box is still starting"}`))
		return
	}
	srv.ServeHTTP(w, r)
}

// Apply stores planner prefs through the API server, once it exists.
// Until then the command is refused: the session is up a moment before the
// server is bound, and a write in that window has nowhere to land.
func (l *lateAPI) Apply(safetyK *float64, export *string) (appproto.PlannerPrefsSnapshot, error) {
	srv := l.srv.Load()
	if srv == nil {
		return appproto.PlannerPrefsSnapshot{}, errors.New("the box is still starting")
	}
	return srv.ApplyPlannerPrefs(safetyK, export)
}

func (l *lateAPI) Route(r *http.Request) apiauth.RouteFacts {
	srv := l.srv.Load()
	if srv == nil {
		// No routes are registered yet, so nothing can be recognised as
		// actuation — and nothing can be served either, since every request
		// in this window meets the 503 above. The method still decides the
		// tier, mirroring api.Route's first rule, so a write is not waved
		// through in the seconds before the server exists.
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			return apiauth.RouteFacts{Tier: apiauth.TierRead}
		}
		return apiauth.RouteFacts{Tier: apiauth.TierConfigure}
	}
	return srv.Route(r)
}

// appLinkAPI is the API's view of enrollment: the identity plus, when the
// uplink runs, the ability to tear a revoked phone's sessions down at once
// rather than at its next reconnect.
type appLinkAPI struct {
	enroll *appenroll.Identity
	uplink *appuplink.Uplink
	// agents ends the holds and the state of agent rows a removal takes
	// with it. Nil while the app link is off.
	agents *agentPorts
	// st sweeps a revoked phone's push subscriptions. Nil in embeddings
	// without storage; the sweep is then vacuously done.
	st *state.Store
}

// MintPairingCode issues an owner's or a viewer's QR code.
//
// An invite is not a new cryptographic object: it is this same code, with a
// role the box remembers and stamps when the code is spent. The payload does
// not change shape, so the app's scanner needs to know nothing about sharing —
// a guest scans what an owner scans and learns what they are from hello_ok.
func (a *appLinkAPI) MintPairingCode(role string) ([]byte, time.Time, error) {
	ttl := appenroll.PairingTTL
	if role != apiauth.RoleOwner {
		ttl = appenroll.InviteTTL
	}
	code, expires, err := a.enroll.MintPairingCode(role, ttl)
	return code, expires, appLinkError(err)
}

// MintWatchCode issues a watch code bound to the owner phone that asked.
func (a *appLinkAPI) MintWatchCode(parentID string) ([]byte, time.Time, error) {
	code, expires, err := a.enroll.MintWatchCode(parentID, appenroll.WatchTTL)
	return code, expires, appLinkError(err)
}

func (a *appLinkAPI) MintSpokenCode(role string) (string, time.Time, error) {
	code, expires, err := a.enroll.MintSpokenCode(role)
	return code, expires, appLinkError(err)
}

func (a *appLinkAPI) EnrollmentURL(code []byte, lanHint string) (string, error) {
	return a.enroll.EnrollmentURL(code, lanHint)
}

// AuthorisedCount is how many phones are paired. A watch rides on its phone
// and is not counted, so the page's "2 phones paired" stays true.
func (a *appLinkAPI) AuthorisedCount() int {
	phones := 0
	for _, d := range a.enroll.Devices() {
		if d.Parent == "" {
			phones++
		}
	}
	return phones
}

func (a *appLinkAPI) Devices() []api.AppDevice {
	infos := a.enroll.Devices()
	out := make([]api.AppDevice, 0, len(infos))
	for _, d := range infos {
		out = append(out, api.AppDevice{
			ID: d.ID, AddedAtMs: d.AddedAtMs, LastSeenMs: d.LastSeenMs,
			Role: d.Role, LastOwner: d.LastOwner, Parent: d.Parent,
		})
	}
	return out
}

// SetDeviceRole changes what one phone may do, and leaves its session up.
//
// No session is dropped, deliberately. The epoch moved with the role, and both
// doors re-read the grant on every privileged request — so a demoted owner
// loses their writes at the next one and keeps the readings they still have
// every right to see. Tearing the session down would tell them their access
// was withdrawn, which for a demotion is not true and for a promotion is the
// opposite of true.
func (a *appLinkAPI) SetDeviceRole(id, role string) error {
	if err := a.enroll.SetRole(id, role); err != nil {
		return appLinkError(err)
	}
	// A phone that is no longer an owner vouches for no control: the holds
	// of the agents it approved end now.
	if role != apiauth.RoleOwner && a.agents != nil && a.agents.access != nil {
		for _, row := range a.agents.access.Agents() {
			if row.ApprovedBy == apiauth.KindApp+":"+id {
				a.agents.access.EndHolds(row.ID)
			}
		}
	}
	return nil
}

// RevokeDevice locks one device out, and a phone's watches with it. atTheBox
// is the API's word for the door the request came through, and it is the only
// thing that decides whether the last owner may go; Presence is enrollment's
// word for the same fact.
func (a *appLinkAPI) RevokeDevice(id string, atTheBox bool) error {
	agentRows := map[string]bool{}
	if a.agents != nil && a.agents.access != nil {
		for _, row := range a.agents.access.Agents() {
			agentRows[row.ID] = true
		}
	}
	removed, err := a.enroll.Revoke(id, appenroll.Presence(atTheBox))
	if err != nil {
		return appLinkError(err)
	}
	// An agent row this took, by its id or with the phone that approved it,
	// goes the way a revoke goes: its holds end, its state goes, and the
	// audit log says so.
	for _, gone := range removed {
		if agentRows[gone.ID] {
			a.agents.agentGone(gone.ID, "app-link")
		}
	}
	for _, gone := range removed {
		// Forgetting the key locks the next handshake out; dropping the
		// live sessions locks out the one running now. Both, or "remove"
		// quietly means "remove within the hour".
		if a.uplink != nil {
			a.uplink.DropSessionsByAppKey(gone.Key)
		}
		// A device that may no longer read the house may no longer be told
		// about it: its push subscriptions go with the grant, and the
		// Cloud roster's encrypted offline notices for them go on the resync.
		if a.st != nil {
			n, err := a.st.DeletePushSubscriptionsByDevice(gone.ID)
			switch {
			case err != nil:
				slog.Warn("could not sweep a revoked device's push subscriptions", "device", gone.ID, "err", err)
			case n > 0:
				slog.Info("swept a revoked device's push subscriptions", "device", gone.ID, "rows", n)
			}
		}
	}
	return nil
}

// appLinkError translates enrollment's refusals into the API's.
//
// Two vocabularies because the API package does not import appenroll — and
// because the refusals are the part a screen has to explain, so they are worth
// naming once on each side rather than passing a package's errors through.
func appLinkError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, appenroll.ErrUnknownDevice):
		return api.ErrUnknownAppDevice
	case errors.Is(err, appenroll.ErrLastOwnerProtected):
		return api.ErrLastAppOwnerProtected
	case errors.Is(err, appenroll.ErrUnknownRole):
		return api.ErrUnknownAppRole
	case errors.Is(err, appenroll.ErrWatchRole):
		return api.ErrWatchAppRole
	case errors.Is(err, appenroll.ErrWatchNeedsOwner):
		return api.ErrWatchNeedsOwner
	default:
		return err
	}
}

// appEnrollForAPI hands the enroller to the API, or nothing.
//
// A typed nil in an interface is not nil, so a disabled app link would give
// the pairing routes something that passes their `== nil` check and then
// panics on first use. Returning an untyped nil is the difference between
// "pairing is off" and a crash on the one screen someone reaches for when
// nothing else works.
func appEnrollForAPI(enroll *appenroll.Identity, uplink *appuplink.Uplink, st *state.Store, enabled bool, agents *agentPorts) api.AppEnroller {
	if !enabled || enroll == nil {
		return nil
	}
	return &appLinkAPI{enroll: enroll, uplink: uplink, st: st, agents: agents}
}
