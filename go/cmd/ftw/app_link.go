package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/api"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/appuplink"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
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
	a.ctrlMu.Unlock()

	snap := appproto.Snapshot{Mode: mode, ControlRev: rev}

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

	// grid = load + battery + pv, all site-signed. Rearranged, not
	// re-derived: a second formula here would be a second thing to keep in
	// step with docs/site-convention.md.
	snap.LoadW = snap.GridW - snap.BatteryW - snap.PVW -
		a.tel.SumOnlineEVW() - a.tel.SumOnlineV2XW()

	return snap
}

// appBoxInfo is what the box calls itself.
type appBoxInfo struct {
	id    string
	build string
	tz    string
}

func (a appBoxInfo) Identity() appproto.Identity {
	return appproto.Identity{ID: a.id, Build: a.build, TZ: a.tz}
}

// Boot returns nil because by the time the uplink is running the box can serve
// telemetry. A box still starting has not reached this code, so claiming
// otherwise would be a spinner that never resolves.
func (a appBoxInfo) Boot() *appproto.BootProgress { return nil }

// appModes applies a mode and reads it back.
//
// SetMode and ObservedMode are deliberately different questions: the first is
// what was asked for, the second is what the box is running. cmd.result
// reports the read-back, so a mode the box refused never appears as applied.
type appModes struct {
	ctrl   *control.State
	ctrlMu *sync.Mutex
	state  *state.Store
	mpc    *mpc.Service
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
			slog.Warn("app uplink could not persist the mode", "err", err)
		}
	}
	if mm, ok := control.PlannerMPCMode(m); ok && a.mpc != nil {
		a.mpc.SetMode(ctx, mm)
	}
	return nil
}

func (a *appModes) ObservedMode() (control.Mode, bool) {
	a.ctrlMu.Lock()
	defer a.ctrlMu.Unlock()
	return a.ctrl.Mode, true
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

// startAppLink connects the box to the app relay, if the site asked for it.
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
	ctrl *control.State,
	ctrlMu *sync.Mutex,
	revision *control.Revision,
	siteMeterStale time.Duration,
) (*appenroll.Identity, bool, error) {
	enabled := cfg != nil && cfg.AppLink != nil && cfg.AppLink.Enabled

	enroll, err := appenroll.LoadOrCreate(identityKeyPath)
	if err != nil {
		return nil, enabled, err
	}
	if !enabled {
		return enroll, false, nil
	}

	// The box's own zone, read from the host. The app renders plan slot times
	// in it, so a box in Stockholm and a phone abroad still agree on when
	// tonight's cheap hours are.
	tz := time.Now().Location().String()

	site := &appSite{
		tel: tel, ctrl: ctrl, ctrlMu: ctrlMu, revision: revision,
		started: processStarted, siteMeterStale: siteMeterStale,
	}
	info := appBoxInfo{id: boxID, build: build, tz: tz}
	modes := &appModes{ctrl: ctrl, ctrlMu: ctrlMu, state: st, mpc: planner}
	plans := &appPlans{planner: planner, ctrl: ctrl, ctrlMu: ctrlMu}

	uplink, err := appuplink.New(appuplink.Options{
		Enroll: enroll,
		Handler: func(sender appproto.Sender) (*appproto.Handler, error) {
			// One handler per app session. They share the ports above, which
			// are read-only apart from the mode, and the mode goes through
			// control's own validation.
			return appproto.New(appproto.Config{
				Clock:  appproto.SystemClock{StartedAt: site.started, Source: "ntp"},
				Site:   site,
				Info:   info,
				Modes:  modes,
				Plans:  plans,
				Codec:  appuplink.Codec(),
				Sender: sender,
				// The three frozen power fields point at the source whose
				// freshness governs them. The site meter is the only one the
				// box can name without knowing the site's hardware.
				SrcGrid:    siteMeterName(ctrl, ctrlMu),
				SrcPV:      siteMeterName(ctrl, ctrlMu),
				SrcBattery: siteMeterName(ctrl, ctrlMu),
			})
		},
	})
	if err != nil {
		return enroll, enabled, err
	}

	go func() {
		if err := uplink.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("app uplink stopped")
		}
	}()

	return enroll, true, nil
}

func siteMeterName(ctrl *control.State, ctrlMu *sync.Mutex) string {
	ctrlMu.Lock()
	defer ctrlMu.Unlock()
	return ctrl.SiteMeterDriver
}

// appEnrollForAPI hands the enroller to the API, or nothing.
//
// A typed nil in an interface is not nil, so a disabled app link would give
// the pairing routes something that passes their `== nil` check and then
// panics on first use. Returning an untyped nil is the difference between
// "pairing is off" and a crash on the one screen someone reaches for when
// nothing else works.
func appEnrollForAPI(enroll *appenroll.Identity, enabled bool) api.AppEnroller {
	if !enabled || enroll == nil {
		return nil
	}
	return enroll
}
