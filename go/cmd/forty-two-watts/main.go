// forty-two-watts — Home Energy Management System.
//
// Don't Panic 🐬
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/api"
	"github.com/frahlg/forty-two-watts/go/internal/battery"
	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/frahlg/forty-two-watts/go/internal/configreload"
	"github.com/frahlg/forty-two-watts/go/internal/control"
	"github.com/frahlg/forty-two-watts/go/internal/currency"
	"github.com/frahlg/forty-two-watts/go/internal/events"
	"github.com/frahlg/forty-two-watts/go/internal/notifications"
	"github.com/frahlg/forty-two-watts/go/internal/arp"
	"github.com/frahlg/forty-two-watts/go/internal/devtools"
	"github.com/frahlg/forty-two-watts/go/internal/drivers"
	"github.com/frahlg/forty-two-watts/go/internal/forecast"
	"github.com/frahlg/forty-two-watts/go/internal/ha"
	"github.com/frahlg/forty-two-watts/go/internal/loadmodel"
	"github.com/frahlg/forty-two-watts/go/internal/loadpoint"
	mqttcli "github.com/frahlg/forty-two-watts/go/internal/mqtt"
	modbuscli "github.com/frahlg/forty-two-watts/go/internal/modbus"
	"github.com/frahlg/forty-two-watts/go/internal/mpc"
	"github.com/frahlg/forty-two-watts/go/internal/nova"
	"github.com/frahlg/forty-two-watts/go/internal/ocpp"
	"github.com/frahlg/forty-two-watts/go/internal/priceforecast"
	"github.com/frahlg/forty-two-watts/go/internal/prices"
	"github.com/frahlg/forty-two-watts/go/internal/proxy"
	"github.com/frahlg/forty-two-watts/go/internal/pvmodel"
	"github.com/frahlg/forty-two-watts/go/internal/selftune"
	"github.com/frahlg/forty-two-watts/go/internal/selfupdate"
	"github.com/frahlg/forty-two-watts/go/internal/state"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// Version gets injected at build time via -ldflags. Defaults to "dev" for
// local runs.
var Version = "dev"

func main() {
	// Subcommand dispatch — a bare first non-flag argument selects one
	// of the bootstrap CLIs, e.g. `forty-two-watts nova-claim --url=…`.
	// Everything else is the long-running service.
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		switch os.Args[1] {
		case "nova-claim":
			// Shift os.Args so the subcommand's flag.FlagSet sees its own flags.
			runNovaClaim(os.Args[2:])
			return
		}
	}

	configPath := flag.String("config", "config.yaml", "Path to config.yaml")
	webDir := flag.String("web", "web", "Path to static web UI directory")
	driverDirFlag := flag.String("drivers", "", "Path to drivers directory (default: <config-dir>/drivers)")
	// Developer utility — seeds state.db with N days of synthetic history
	// so /api/energy/daily has something to render locally. Refuses to run
	// if the target DB already holds non-synthetic rows (prod-safety gate);
	// -backfill-force bypasses that check. Exits after seeding without
	// starting the service.
	backfillDays := flag.Int("backfill", 0, "DEV ONLY: seed N days of synthetic history into state.db then exit (0 disables)")
	backfillStep := flag.Duration("backfill-step", 5*time.Second, "DEV ONLY: backfill sample interval")
	backfillSeed := flag.Int64("backfill-seed", 0, "DEV ONLY: backfill rng seed (0 = random)")
	backfillForce := flag.Bool("backfill-force", false, "DEV ONLY: bypass the non-synthetic-data safety gate")
	flag.Parse()

	// Drivers default to a sibling of the config file (historical layout:
	// config.yaml + drivers/ + seed/ + state.db all under one dir). Docker
	// breaks that convention because /app/data is a host bind mount while
	// drivers are baked into the image at /app/drivers — the flag lets the
	// CMD point at the immutable image location.
	resolveDriverDir := func() string {
		if *driverDirFlag != "" {
			return *driverDirFlag
		}
		return filepath.Join(filepath.Dir(*configPath), "drivers")
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	slog.Info("forty-two-watts starting", "version", Version, "config", *configPath)

	// Route "drivers/<name>.lua" path resolution through the drivers dir
	// (from -drivers). Picked up by both the initial Load below and every
	// subsequent reload via the file watcher.
	config.DriversDirOverride = resolveDriverDir()

	// ---- Load config ----
	cfg, err := config.Load(*configPath)
	if err != nil {
		if isConfigMissing(err) {
			runBootstrap(*configPath, *webDir, resolveDriverDir())
			return
		}
		slog.Error("load config", "err", err)
		os.Exit(1)
	}
	slog.Info("config loaded", "site", cfg.Site.Name, "drivers", len(cfg.Drivers))

	// ---- Open persistent state (SQLite) ----
	statePath := "state.db"
	coldDir := "cold"
	if cfg.State != nil {
		if cfg.State.Path != "" { statePath = cfg.State.Path }
		if cfg.State.ColdDir != "" { coldDir = cfg.State.ColdDir }
	}
	st, err := state.Open(statePath)
	if err != nil {
		slog.Error("open state", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	// ---- Dev backfill (flag-gated, one-shot) ----
	// When -backfill N (N>0) is set, seed N days of synthetic history
	// into state.db and exit WITHOUT starting the service. Refuses if
	// the DB already holds non-synthetic rows unless -backfill-force.
	if *backfillDays > 0 {
		if err := devtools.Backfill(st, devtools.BackfillConfig{
			Days:  *backfillDays,
			Step:  *backfillStep,
			Seed:  *backfillSeed,
			Force: *backfillForce,
		}, slog.Default()); err != nil {
			slog.Error("backfill", "err", err)
			os.Exit(1)
		}
		return
	}

	if err := st.RecordEvent("startup"); err != nil {
		slog.Warn("failed to persist startup event", "err", err)
	}

	// ---- Restore EV charger password from state.db (not stored in YAML) ----
	if cfg.EVCharger != nil {
		if pw, ok := st.LoadConfig("ev_charger_password"); ok {
			cfg.EVCharger.Password = pw
		}
	}

	// ---- Telemetry store ----
	tel := telemetry.NewStore()

	// ---- Control state ----
	ctrl := control.NewState(cfg.Site.GridTargetW, cfg.Site.GridToleranceW, cfg.SiteMeterDriver())
	ctrl.SlewRateW = cfg.Site.SlewRateW
	ctrl.MinDispatchIntervalS = cfg.Site.MinDispatchIntervalS
	ctrl.InverterGroups = inverterGroupsFrom(cfg.Drivers)
	ctrl.SupportsPVCurtail = supportsPVCurtailFrom(cfg.Drivers)
	ctrl.DriverLimits = driverLimitsFrom(cfg.Drivers, cfg.Batteries)
	// Per-phase fuse params for the per-phase clamp inside applyFuseGuard
	// + forceFuseDischarge. Reads l1_a/l2_a/l3_a from the meter driver
	// when SiteFuseAmps > 0; otherwise the per-phase clamp is disabled.
	ctrl.SiteFuseAmps = cfg.Fuse.MaxAmps
	ctrl.SiteFuseVoltage = cfg.Fuse.Voltage
	ctrl.SiteFusePhases = cfg.Fuse.Phases
	// EffectiveSafetyMarginA distinguishes nil ("unset, use default")
	// from explicit 0 ("operator chose to disable"). The earlier
	// `<= 0 → default` shortcut clobbered the disable case.
	ctrl.SiteFuseSafetyA = cfg.Fuse.EffectiveSafetyMarginA()
	// Restore persisted mode + target if present. The planner variants
	// have to be listed too — without them the strategy the user picked in
	// the UI (planner_self / planner_cheap / planner_arbitrage) is silently
	// dropped on restart and the dashboard appears to forget the selection.
	if v, ok := st.LoadConfig("mode"); ok {
		switch control.Mode(v) {
		case control.ModeIdle, control.ModeSelfConsumption, control.ModePeakShaving,
			control.ModeCharge, control.ModePriority, control.ModeWeighted,
			control.ModePlannerSelf, control.ModePlannerCheap, control.ModePlannerArbitrage:
			ctrl.Mode = control.Mode(v)
		}
	}
	if v, ok := st.LoadConfig("grid_target_w"); ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			ctrl.SetGridTarget(f)
		}
	}
	if v, ok := st.LoadConfig("battery_covers_ev"); ok {
		ctrl.BatteryCoversEV = v == "true"
	}

	// ---- Driver capacities (site, for control + fuse guard) ----
	// Loadpoint drivers are filtered out — their battery_capacity_wh
	// is vehicle capacity, not site-battery capacity.
	capacities := driverCapacitiesFrom(cfg.Drivers, cfg.Loadpoints)
	warnIfEVHasBatteryCapacity(cfg.Drivers, cfg.Loadpoints)

	// ---- Battery models — restore from SQLite + ensure one per driver ----
	models := make(map[string]*battery.Model)
	if stored, err := st.LoadAllBatteryModels(); err == nil {
		for name, js := range stored {
			m := &battery.Model{}
			if err := json.Unmarshal([]byte(js), m); err == nil {
				models[name] = m
				slog.Info("restored battery model",
					"name", name, "τ", m.TimeConstantS(float64(cfg.Site.ControlIntervalS)),
					"gain", m.SteadyStateGain(), "samples", m.NSamples)
			}
		}
	}
	for _, d := range cfg.Drivers {
		if d.BatteryCapacityWh > 0 && models[d.Name] == nil {
			models[d.Name] = battery.New(d.Name)
		}
	}

	// ---- Self-tune coordinator ----
	selfTune := selftune.NewCoordinator()

	// ---- Restart signal ----
	// Closing restartCh from /api/restart drops the main control loop out
	// of its select, which returns from main() so every defer (HA Stop,
	// state.Close, http.Shutdown, …) runs in normal LIFO order. The
	// bottom-of-stack `os.Exit` defer below then translates exitCode 1
	// into a non-zero process exit so docker (`unless-stopped`) and
	// systemd (`Restart=on-failure`) bring the binary back up. SIGTERM /
	// SIGINT take the same return path with exitCode 0.
	restartCh := make(chan struct{})
	var restartOnce sync.Once
	exitCode := 0
	defer func() {
		if exitCode != 0 {
			os.Exit(exitCode)
		}
	}()

	// ---- Driver registry ----
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reg := drivers.NewRegistry(tel)
	reg.MQTTFactory = func(name string, c *config.MQTTConfig) (drivers.MQTTCap, error) {
		return mqttcli.Dial(c.Host, c.Port, c.Username, c.Password, "ftw-"+name)
	}
	reg.ModbusFactory = func(name string, c *config.ModbusConfig) (drivers.ModbusCap, error) {
		return modbuscli.Dial(c.Host, c.Port, c.UnitID)
	}
	reg.ARPLookup = arp.Lookup
	// Spawn initial drivers. config.Load has already joined relative Lua
	// paths with the config directory — nothing to resolve here.
	for _, d := range cfg.Drivers {
		if d.Disabled {
			slog.Info("driver skipped (disabled)", "name", d.Name)
			continue
		}
		if err := reg.Add(ctx, d); err != nil {
			slog.Warn("failed to spawn driver", "name", d.Name, "err", err)
		}
	}
	defer reg.ShutdownAll()

	// ---- Identity bootstrap ----
	// Drivers report make/serial inside driver_init via host.set_make / set_sn,
	// and we resolved endpoint+MAC at registry-Add time. Now we wait briefly
	// for those to populate, then register each device + run the one-shot
	// migration that re-keys legacy battery_models from driver-name to
	// device_id. Subsequent runs are no-ops.
	go func() {
		time.Sleep(3 * time.Second) // let driver_init finish + first SN be reported
		registerAllDevices(st, reg)
		if migrated, err := st.MigrateBatteryModelKeys(); err != nil {
			slog.Warn("battery model key migration failed", "err", err)
		} else if migrated > 0 {
			slog.Info("battery model keys migrated to device_id", "count", migrated)
		}
	}()

	// ---- Shared mutexes for API/control/models ----
	ctrlMu := &sync.Mutex{}
	capMu := &sync.RWMutex{}
	cfgMu := &sync.RWMutex{}
	modelsMu := &sync.Mutex{}

	// Pre-declare services that the hot-reload Applier needs to touch.
	// The Applier closure captures these by reference; they're assigned
	// further down when their packages are wired, and the Applier only
	// ever fires after `watcher.Start()` — by which point everything is
	// in place.
	var pvSvc *pvmodel.Service
	var forecastSvc *forecast.Service
	// Notifications: pre-declared so the hot-reload Applier can push
	// fresh config into the provider + rule engine. Constructed
	// unconditionally below so API handlers always have a live pointer.
	var notifProvider notifications.Provider
	var notifSvc *notifications.Service
	// Event bus: decouples core loops (control tick, API) from
	// cross-cutting subscribers (notifications today, audit/webhooks later).
	bus := events.NewBus()

	// ---- EV loadpoints ----
	// Manager is created early so the config hot-reload closure can
	// reference it; actual Load() runs against initial cfg below.
	// Phase 4 wires the first plugged-in loadpoint's state into the
	// MPC's DP so battery + EV are co-optimized in one DP.
	lpMgr := loadpoint.NewManager()
	if len(cfg.Loadpoints) > 0 {
		lpMgr.Load(buildLoadpointConfigs(cfg.Loadpoints))
		slog.Info("loadpoints configured", "count", len(cfg.Loadpoints))
	}

	// Forward-declared so the hot-reload closure below can push
	// capacity changes into the running planner. Assigned at line
	// ~450 after all its dependencies (pvSvc, loadSvc, priceFc) are
	// wired up. nil until that point — the reload closure guards.
	var mpcSvc *mpc.Service

	// Pre-declared so the hot-reload Applier can call (*ha.Bridge).Reload
	// when broker / credentials / publish interval change. Constructed
	// further down once the registry + control callbacks exist; the
	// Applier nil-guards against the bridge being disabled.
	var haBridge *ha.Bridge

	// deps is the API server's runtime dependency container. Forward-
	// declared as a *Deps so the hot-reload Applier closure can capture
	// it as a variable: the closure dereferences `deps` only after the
	// HTTP server has been wired up (deps is populated below), so
	// applier-time reads always observe the fully-constructed value.
	// Updating deps.HA from the applier (e.g. when an operator toggles
	// HA from disabled to enabled at runtime) keeps the API handler's
	// pointer in sync without forcing a process restart.
	var deps *api.Deps

	// ---- Config hot-reload watcher ----
	watcher, err := configreload.New(*configPath, cfgMu, cfg, ctrlMu, ctrl,
		func(newCfg, oldCfg *config.Config) {
			// Restore EV charger password from state.db (not in YAML).
			if newCfg.EVCharger != nil {
				if pw, ok := st.LoadConfig("ev_charger_password"); ok {
					newCfg.EVCharger.Password = pw
				}
			}
			// Driver paths are already resolved by config.Load; no extra
			// work needed here.
			reg.Reload(ctx, newCfg.Drivers)
			// Refresh capacities — mutate the existing map in place so
			// Deps.Capacities (a map header captured at init) sees the
			// update. Rebinding the local variable would orphan the
			// reference the api server still holds.
			capMu.Lock()
			for k := range capacities { delete(capacities, k) }
			for k, v := range driverCapacitiesFrom(newCfg.Drivers, newCfg.Loadpoints) {
				capacities[k] = v
			}
			capMu.Unlock()

			// Swap inverter-group tags (#143) and per-driver power
			// limits (#145) together. Taken under ctrlMu because
			// ComputeDispatch reads State.InverterGroups + .DriverLimits;
			// a bare replace would race with the control loop's 5 s tick.
			ctrlMu.Lock()
			ctrl.InverterGroups = inverterGroupsFrom(newCfg.Drivers)
			ctrl.SupportsPVCurtail = supportsPVCurtailFrom(newCfg.Drivers)
			ctrl.DriverLimits = driverLimitsFrom(newCfg.Drivers, newCfg.Batteries)
			// Fuse params + safety margin: previously startup-only.
			// Hot-reload them so operators can tune the per-phase margin
			// from the UI without restarting (e.g. raising it after the
			// inverter's own protection trips, lowering it to recover
			// last few hundred W of arbitrage headroom).
			ctrl.SiteFuseAmps = newCfg.Fuse.MaxAmps
			ctrl.SiteFuseVoltage = newCfg.Fuse.Voltage
			ctrl.SiteFusePhases = newCfg.Fuse.Phases
			// Mirror the startup-path default semantics — nil → 0.5,
			// explicit 0 → disabled. See EffectiveSafetyMarginA.
			ctrl.SiteFuseSafetyA = newCfg.Fuse.EffectiveSafetyMarginA()
			ctrlMu.Unlock()

			// Push the new pool totals into the planner so its next
			// replan uses the right CapacityWh / MaxChargeW /
			// MaxDischargeW. Without this the MPC keeps the snapshot
			// it took at buildMPC time; SoC % and terminal credit go
			// stale after an EV loadpoint is added/removed. Codex P1
			// on PR #121.
			if mpcSvc != nil {
				totalCap, maxChg, maxDis := aggregateBatteryLimits(newCfg, capacities)
				mpcSvc.UpdateCapacity(totalCap, maxChg, maxDis)
				slog.Info("mpc: capacity updated via hot-reload",
					"capacity_wh", totalCap, "max_charge_w", maxChg, "max_discharge_w", maxDis)
			}

			// Hot-reload EV loadpoints so operators can add / remove /
			// retune them without restarting. Manager preserves
			// observed state across reloads (plug status, session
			// anchor, current SoC estimate) — see loadpoint.Manager.Load.
			lpMgr.Load(buildLoadpointConfigs(newCfg.Loadpoints))

			// Notifications: rebuild the provider from fresh config
			// (handles the cold-start case where the initial config
			// had no notifications: block and notifProvider was nil),
			// wire it onto the service, then reset the rule-engine
			// per-outage latch. All calls are nil-safe.
			newProv := notifications.NewProvider(newCfg.Notifications)
			notifProvider = newProv
			var newPub notifications.Publisher
			if newProv != nil {
				newPub = newProv
			}
			notifSvc.SetPublisher(newPub)
			notifSvc.Reload(newCfg.Notifications)

			// Home Assistant: hot-reload broker / credentials / publish
			// interval / driver list. Bridge.Reload tears down the paho
			// client and re-publishes discovery so an operator changing
			// the broker IP from Settings sees HA reconnect within a
			// second — no process restart required.
			//
			// Three transitions to handle:
			//   running → running:  Bridge.Reload swaps connection.
			//   running → disabled: Stop the existing bridge.
			//   disabled → enabled: Start a fresh bridge (handles both
			//                       the "previously toggled off" case and
			//                       the "Start failed at boot, operator
			//                       fixed the broker" recovery path).
			haEnabled := newCfg.HomeAssistant != nil && newCfg.HomeAssistant.Enabled
			switch {
			case haBridge != nil && haEnabled:
				if err := haBridge.Reload(newCfg.HomeAssistant, reg.Names()); err != nil {
					slog.Warn("HA bridge reload failed", "err", err)
				} else {
					slog.Info("HA bridge reloaded", "broker", newCfg.HomeAssistant.Broker)
				}
			case haBridge != nil && !haEnabled:
				haBridge.Stop()
				haBridge = nil
				deps.HA = nil
				slog.Info("HA bridge stopped (disabled in config)")
			case haBridge == nil && haEnabled:
				if bridge, err := ha.Start(newCfg.HomeAssistant, tel, ctrl, ctrlMu, reg.Names(), haCallbacks(ctrl, ctrlMu, st)); err != nil {
					slog.Warn("HA bridge start failed", "err", err)
				} else {
					haBridge = bridge
					deps.HA = bridge
					slog.Info("HA bridge started", "broker", newCfg.HomeAssistant.Broker)
				}
			}

			// Weather diff → push live into the PV twin + forecast
			// fetcher without a process restart. Users adjust rated PV
			// + lat/lon from Settings and expect the change to take
			// effect right away.
			if newCfg.Weather != nil {
				oldLat, oldLon, oldRated := 0.0, 0.0, 0.0
				if oldCfg.Weather != nil {
					oldLat = oldCfg.Weather.Latitude
					oldLon = oldCfg.Weather.Longitude
					oldRated = oldCfg.Weather.PVRatedW
				}
				newRated := newCfg.Weather.PVRatedW
				if newRated > 0 && newRated != oldRated {
					if pvSvc != nil {
						pvSvc.SetRated(newRated)
					}
					if forecastSvc != nil {
						forecastSvc.RatedPVW = newRated
					}
				}
				newLat := newCfg.Weather.Latitude
				newLon := newCfg.Weather.Longitude
				if newLat != oldLat || newLon != oldLon {
					if pvSvc != nil {
						pvSvc.ClearSky = func(t time.Time) float64 { return forecast.ClearSkyW(newLat, newLon, t) }
					}
					if forecastSvc != nil {
						forecastSvc.Lat = newLat
						forecastSvc.Lon = newLon
					}
					slog.Info("weather location updated", "lat", newLat, "lon", newLon)
				}
			}
		})
	if err != nil {
		slog.Warn("could not start config watcher", "err", err)
	} else {
		watcher.Start()
		defer watcher.Stop()
	}

	// ---- Spot prices + weather forecast (optional, nil if not configured) ----
	// ---- FX rates (ECB, daily) — harmless to run even for SE-only users ----
	fxSvc := currency.New(st)
	fxSvc.Start(ctx)
	defer fxSvc.Stop()

	priceSvc := prices.FromConfig(cfg.Price, st, fxSvc)

	// ---- Price forecaster (fills in beyond day-ahead publication) ----
	zones := []string{"SE3"}
	if cfg.Price != nil && cfg.Price.Zone != "" {
		zones = []string{cfg.Price.Zone}
	}
	priceFc := priceforecast.NewService(st, zones)
	// Optional: seed from bundled CSV on first boot. Idempotent so safe
	// to call every boot — no-op once data is already in the store.
	seedPath := filepath.Join(filepath.Dir(*configPath), "seed", "prices.csv")
	if _, err := os.Stat(seedPath); err == nil {
		n, err := priceFc.SeedFromCSV(seedPath)
		if err != nil {
			slog.Warn("priceforecast seed failed", "path", seedPath, "err", err)
		} else if n > 0 {
			slog.Info("priceforecast seeded", "rows", n, "path", seedPath)
		}
	}
	priceFc.Start(ctx)
	defer priceFc.Stop()
	if priceSvc != nil {
		priceSvc.Start(ctx)
		defer priceSvc.Stop()
		slog.Info("price service started", "zone", priceSvc.Zone, "provider", priceSvc.Provider.Name())
	}

	// Sum rated PV from all drivers for the forecast estimator
	// Prefer explicit config; fall back to heuristic if unset.
	ratedPVW := 0.0
	if cfg.Weather != nil && cfg.Weather.PVRatedW > 0 {
		ratedPVW = cfg.Weather.PVRatedW
	} else {
		for _, d := range cfg.Drivers {
			if d.BatteryCapacityWh > 0 {
				ratedPVW += d.BatteryCapacityWh / 3
			}
		}
		if ratedPVW == 0 {
			ratedPVW = 10000
		}
	}
	forecastSvc = forecast.FromConfig(cfg.Weather, ratedPVW, st,
		"forty-two-watts/"+Version+" github.com/frahlg/forty-two-watts")
	if forecastSvc != nil {
		forecastSvc.Start(ctx)
		defer forecastSvc.Stop()
		slog.Info("forecast service started", "provider", forecastSvc.Provider.Name(),
			"lat", forecastSvc.Lat, "lon", forecastSvc.Lon, "rated_pv_w", ratedPVW)
	}

	// ---- Start PV digital twin (optional, requires weather config) ----
	// pvSvc is pre-declared above so the reload Applier can update it.
	if cfg.Weather != nil && cfg.Weather.Provider != "" && cfg.Weather.Provider != "none" {
		lat, lon := cfg.Weather.Latitude, cfg.Weather.Longitude
		clearSkyFn := func(t time.Time) float64 { return forecast.ClearSkyW(lat, lon, t) }
		cloudFn := func(t time.Time) (float64, bool) {
			// Look up nearest forecast row covering `t`.
			nowMs := t.UnixMilli()
			rows, err := st.LoadForecasts(nowMs-2*3600*1000, nowMs+2*3600*1000)
			if err != nil || len(rows) == 0 {
				return 0, false
			}
			for _, r := range rows {
				slotLen := r.SlotLenMin
				if slotLen <= 0 {
					slotLen = 60
				}
				end := r.SlotTsMs + int64(slotLen)*60*1000
				if nowMs >= r.SlotTsMs && nowMs < end && r.CloudCoverPct != nil {
					return *r.CloudCoverPct, true
				}
			}
			return 0, false
		}
		pvSvc = pvmodel.NewService(st, tel, clearSkyFn, cloudFn, ratedPVW)
		pvSvc.Start(ctx)
		defer pvSvc.Stop()
		slog.Info("pvmodel started", "rated_w", ratedPVW, "quality", pvSvc.Model().Quality())
	}

	// ---- Start load digital twin ----
	// Peak load proxy: use fuse power budget × 0.5 as a sane default
	// until user configures an explicit value. Users can override by
	// setting site.load_peak_w in config once we expose it.
	loadPeakW := cfg.Fuse.MaxPowerW() * 0.5
	if loadPeakW <= 0 {
		loadPeakW = 5000
	}
	loadSvc := loadmodel.NewService(st, tel, cfg.SiteMeterDriver(), loadPeakW)
	if cfg.Weather != nil && cfg.Weather.HeatingWPerDegC > 0 {
		m := loadSvc.Model()
		m.HeatingW_per_degC = cfg.Weather.HeatingWPerDegC
		// Apply without persisting raw overwrite — model is behind a sync,
		// so use the exposed setter. Simpler: push via reset+restore.
		// Just update the live field directly through a small helper.
		loadSvc.SetHeatingCoef(cfg.Weather.HeatingWPerDegC)
	}
	// Temperature source for heating-gain fit: same forecast cache.
	loadSvc.Temp = func(t time.Time) (float64, bool) {
		nowMs := t.UnixMilli()
		rows, err := st.LoadForecasts(nowMs-2*3600*1000, nowMs+2*3600*1000)
		if err != nil || len(rows) == 0 {
			return 0, false
		}
		for _, r := range rows {
			slotLen := r.SlotLenMin
			if slotLen <= 0 {
				slotLen = 60
			}
			end := r.SlotTsMs + int64(slotLen)*60*1000
			if nowMs >= r.SlotTsMs && nowMs < end && r.TempC != nil {
				return *r.TempC, true
			}
		}
		return 0, false
	}
	loadSvc.Start(ctx)
	defer loadSvc.Stop()
	slog.Info("loadmodel started", "peak_w", loadPeakW, "quality", loadSvc.Model().Quality())

	// ---- Start MPC planner (optional) ----
	mpcSvc = buildMPC(cfg, st, tel, capacities)
	if mpcSvc != nil {
		// Plumb the site fuse so the DP joint-plans battery + EV under
		// the fuse from the start (instead of producing plans that
		// dispatch later has to scale via the joint allocator).
		mpcSvc.FuseMaxW = cfg.Fuse.MaxPowerW()
		if pvSvc != nil {
			mpcSvc.PV = pvSvc.Predict
		}
		mpcSvc.Load = loadSvc.Predict
		mpcSvc.Price = priceFc.Predict
		mpcSvc.SiteMeter = cfg.SiteMeterDriver()
		// Wire the loadpoint probe so the DP extends its state space
		// when an EV is plugged in. Single-loadpoint for now: picks
		// the first plugged-in one.
		mpcSvc.Loadpoint = func(slotLenMin int) *mpc.LoadpointSpec {
			for _, st := range lpMgr.States() {
				if !st.PluggedIn {
					continue
				}
				// Schedule gate: only extend the DP with an EV-SoC
				// dimension when the operator has set BOTH a target SoC
				// and a future deadline. Without a schedule the DP
				// previously planned EV charging speculatively across
				// the full 48 h horizon — drawing battery + grid budget
				// against a target the operator never asked for. With
				// no schedule, EV is left to the loadpoint controller's
				// reactive surplus-only behaviour.
				if st.TargetSoCPct <= 0 || st.TargetTime.IsZero() ||
					!st.TargetTime.After(time.Now()) {
					continue
				}
				// Pull capacity off the configured loadpoint.
				var capWh float64 = 60000 // 60 kWh fallback
				for _, c := range cfg.Loadpoints {
					if c.ID == st.ID && c.VehicleCapacityWh > 0 {
						capWh = c.VehicleCapacityWh
						break
					}
				}
				// Prefer live DerVehicle SoC over the loadpoint manager's
				// inferred plugin-anchor + delivered-Wh estimate. The
				// inference is blind to BMS truth (Easee can't see the
				// car); when a vehicle driver such as TeslaBLEProxy is
				// online, its SoC reading is ground truth.
				//
				// Picker (rank + freshness + bounds + connection
				// evidence when delivering power) lives in
				// telemetry.PickBestVehicleForLoadpoint so api.go's
				// loadpoint decoration agrees with us on which vehicle
				// is "the one". Falls back to inferred SoC when nothing
				// usable online matches.
				initSoC := st.CurrentSoCPct
				socSource := "inferred"
				var vehicleChargeLimit float64 // 0 = unknown
				delivering := st.CurrentPowerW > 100.0
				if pick := telemetry.PickBestVehicleForLoadpoint(tel, delivering, time.Now()); pick.Driver != "" {
					initSoC = pick.SoCPct
					socSource = "vehicle:" + pick.Driver
					vehicleChargeLimit = pick.ChargeLimitPct
				}
				// Map target time → slot index using the DP's
				// actual slot length (hour-of-prices vs. 15-min
				// quarters vary by market). Anything past horizon
				// gets clamped by the DP itself; negative means
				// "no deadline".
				if slotLenMin <= 0 {
					slotLenMin = 60
				}
				targetSlot := -1
				if !st.TargetTime.IsZero() {
					delta := time.Until(st.TargetTime)
					if delta > 0 {
						targetSlot = int(delta / (time.Duration(slotLenMin) * time.Minute))
					}
				}
				// Operational ceiling: the lower of the user's target
				// and the vehicle-configured charge limit. The car
				// won't accept current beyond charge_limit_pct anyway,
				// so planning past it is wasted DP grid space. When
				// the limit is unknown, fall back to the deadline
				// target itself; never plan beyond what was requested.
				maxPct := st.TargetSoCPct
				if vehicleChargeLimit > 0 && vehicleChargeLimit < maxPct {
					maxPct = vehicleChargeLimit
				}
				// Guard against degenerate grids: if current SoC > maxPct
				// (already over target), grow the ceiling to current so
				// the DP can at least represent it (no charging will be
				// scheduled). The deadline penalty handles the rest.
				if initSoC > maxPct {
					maxPct = initSoC
				}
				slog.Debug("mpc: loadpoint spec",
					"id", st.ID, "soc_pct", initSoC, "soc_source", socSource,
					"target_pct", st.TargetSoCPct, "target_slot", targetSlot,
					"max_pct", maxPct, "vehicle_limit_pct", vehicleChargeLimit)
				return &mpc.LoadpointSpec{
					ID:              st.ID,
					CapacityWh:      capWh,
					Levels:          11,
					MinPct:          0,
					MaxPct:          maxPct,
					InitialSoCPct:   initSoC,
					PluggedIn:       true,
					TargetSoCPct:    st.TargetSoCPct,
					TargetSlotIdx:   targetSlot,
					MaxChargeW:      st.MaxChargeW,
					AllowedStepsW:   st.AllowedStepsW,
					ChargeEfficiency: 0.9,
					SurplusOnly:     st.SurplusOnly,
				}
			}
			return nil
		}
		if cfg.Price != nil {
			mpcSvc.ExportBonusOreKwh = cfg.Price.ExportBonusOreKwh
			mpcSvc.ExportFeeOreKwh = cfg.Price.ExportFeeOreKwh
			mpcSvc.ExportFloorOreKwh = cfg.Price.ExportFloorOreKwh
			mpcSvc.GridTariffOreKwh = cfg.Price.GridTariffOreKwh
			mpcSvc.VATPercent = cfg.Price.VATPercent
		}
		// Persist every replan's Diagnostic so operators can time-
		// travel. See docs/mpc-planner.md + planner_diagnostics
		// table in state/store.go.
		mpcSvc.SaveDiag = func(d *mpc.Diagnostic, reason string) error {
			js, err := json.Marshal(d)
			if err != nil {
				return err
			}
			return st.SaveDiagnostic(d.ComputedAtMs, reason, d.Zone,
				d.TotalCostOre, d.Horizon, string(js))
		}
		mpcSvc.Start(ctx)
		defer mpcSvc.Stop()
		// Inject plan → control.State. Both callbacks are wired:
		//   PlanTarget — legacy grid-target path (grid_target_w, mode str)
		//   SlotDirective — new energy-allocation path (Wh per slot)
		// State.UseEnergyDispatch picks which one is actually used when a
		// planner mode is active; see docs/plan-ems-contract.md.
		ctrl.PlanTarget = mpcSvc.SlotAt
		ctrl.SlotDirective = func(now time.Time) (control.SlotDirective, bool) {
			d, ok := mpcSvc.SlotDirectiveAt(now)
			if !ok {
				return control.SlotDirective{}, false
			}
			return control.SlotDirective{
				SlotStart:       d.SlotStart,
				SlotEnd:         d.SlotEnd,
				BatteryEnergyWh: d.BatteryEnergyWh,
				SoCTargetPct:    d.SoCTargetPct,
				Strategy:        string(d.Strategy),
				PVLimitW:        d.PVLimitW,
			}, true
		}
		// Default to the energy-allocation path. The plan is a
		// scheduler (decides WHEN each strategy applies); the EMS is
		// the regulator (decides HOW batteries react — from live
		// telemetry, not plan forecasts). See docs/plan-ems-contract.md.
		// `planner.legacy_dispatch: true` opts back to the old
		// PI-on-grid-target path for emergency rollback.
		//
		// Back-compat: honor the deprecated `use_energy_dispatch`
		// key when explicitly set. An operator who had
		// `use_energy_dispatch: false` in their config before v0.27.0
		// chose legacy on purpose — don't silently flip them.
		ctrl.UseEnergyDispatch = cfg.Planner == nil || !cfg.Planner.LegacyDispatch
		if cfg.Planner != nil && cfg.Planner.UseEnergyDispatch != nil {
			v := *cfg.Planner.UseEnergyDispatch
			slog.Warn("planner.use_energy_dispatch is deprecated — use planner.legacy_dispatch: "+
				"true to opt out of the energy path instead. Honored for this run.",
				"value", v)
			ctrl.UseEnergyDispatch = v
		}
		// If the restored control mode is a planner variant, push the
		// corresponding mpc.Mode so the plan is built with the strategy
		// the user actually picked — not whatever cfg.planner.mode says.
		if ctrl.Mode.IsPlannerMode() {
			var mm mpc.Mode
			switch ctrl.Mode {
			case control.ModePlannerSelf:
				mm = mpc.ModeSelfConsumption
			case control.ModePlannerCheap:
				mm = mpc.ModeCheapCharge
			case control.ModePlannerArbitrage:
				mm = mpc.ModeArbitrage
			}
			mpcSvc.SetMode(ctx, mm)
		}
		slog.Info("mpc planner started",
			"mode", mpcSvc.Defaults.Mode,
			"capacity_wh", mpcSvc.Defaults.CapacityWh,
			"horizon", mpcSvc.Horizon,
			"interval", mpcSvc.Interval,
			"pvtwin", pvSvc != nil)
		// Startup replan: the scheduled tick is up to mpcSvc.Interval
		// (15 min) away. Don't make the operator wait — fire one
		// immediately so /api/mpc/plan is populated as soon as
		// telemetry, prices, and forecasts have settled. Observe ctx
		// during the warm-up sleep so SIGTERM during startup doesn't
		// keep this goroutine alive past shutdown.
		go func() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second): // give drivers a moment to seed SoC
			}
			if ctx.Err() != nil {
				return
			}
			_ = mpcSvc.Replan(ctx)
			slog.Info("mpc: startup replan completed")
		}()
	}

	// ---- EV loadpoint controller ----
	// Phase 1 of the EV-arch refactor (issue #172): the per-tick EV
	// dispatch that used to live inline in the control loop is now
	// owned by loadpoint.Controller. Behaviour is identical — same
	// 5 s cadence, same energy-allocation contract, same snap. The
	// separation exists so follow-up PRs can give each loadpoint its
	// own goroutine + driver-declared cadence (#172 PR 2) and a
	// phase/step state machine (#172 PR 3) without disturbing
	// battery dispatch.
	//
	// Adapters here keep loadpoint independent of mpc/telemetry
	// (mpc already imports loadpoint — the cycle must go this way).
	var lpController *loadpoint.Controller
	if mpcSvc != nil {
		planAdapter := func(now time.Time) (loadpoint.Directive, bool) {
			d, ok := mpcSvc.SlotDirectiveAt(now)
			if !ok {
				return loadpoint.Directive{}, false
			}
			return loadpoint.Directive{
				SlotStart:         d.SlotStart,
				SlotEnd:           d.SlotEnd,
				LoadpointEnergyWh: d.LoadpointEnergyWh,
			}, true
		}
		telAdapter := func(driver string) (loadpoint.EVSample, bool) {
			r := tel.Get(driver, telemetry.DerEV)
			if r == nil {
				return loadpoint.EVSample{}, false
			}
			var d struct {
				Connected bool    `json:"connected"`
				SessionWh float64 `json:"session_wh"`
			}
			_ = json.Unmarshal(r.Data, &d)
			return loadpoint.EVSample{
				PowerW:    r.SmoothedW,
				SessionWh: d.SessionWh,
				Connected: d.Connected,
			}, true
		}
		lpController = loadpoint.NewController(lpMgr, planAdapter, telAdapter, reg.Send)
		// Wire the site fuse so the per-phase EV clamp and the
		// phase-split derivation can use the actual site voltage and
		// breaker rating instead of hard-coding 230 V × 16 A.
		lpController.SetSiteFuse(loadpoint.SiteFuse{
			MaxAmps:  cfg.Fuse.MaxAmps,
			Voltage:  cfg.Fuse.Voltage,
			PhaseCnt: cfg.Fuse.Phases,
		})
		// Wire the joint fuse-budget allocator: when battery + EV would
		// together bust the fuse, dispatch publishes a cap on EV W; the
		// loadpoint controller honours it so battery and EV cooperatively
		// share the budget instead of oscillating against the fuse guard.
		lpController.SetFuseEVMax(func() (float64, bool) {
			ctrlMu.Lock()
			defer ctrlMu.Unlock()
			if !ctrl.FuseSaturated {
				return 0, false
			}
			return ctrl.FuseEVMaxW, true
		})
		// Wire the matched-vehicle reader for auto-wake. When the
		// loadpoint is commanding power but the matched Tesla
		// reports `Stopped` / `Disconnected` / `Complete`, the
		// controller fires a charge_start command at the vehicle
		// driver — TeslaBLEProxy translates it to BLE and re-engages
		// the session. Without this, a long pause from the surplus
		// clamp (or arbitrage-mode planning) detaches Tesla and
		// nothing software-side can wake it.
		lpController.SetVehicleStatus(func(lpID string) (string, string, bool) {
			st, ok := lpMgr.State(lpID)
			if !ok || !st.PluggedIn {
				return "", "", false
			}
			delivering := st.CurrentPowerW > 100.0
			pick := telemetry.PickBestVehicleForLoadpoint(tel, delivering, time.Now())
			if pick.Driver == "" {
				return "", "", false
			}
			return pick.Driver, pick.ChargingState, true
		})

		// Wire the EV-available surplus computation for the
		// surplus_only clamp. We want the W of PV that exceeds house
		// load, regardless of how the home battery is currently
		// splitting it — otherwise on a sunny day with the home
		// battery absorbing all surplus the EV controller would see
		// gridW≈0 and conclude "no surplus", contradicting reality.
		//
		// Identity (using api.go's convention `loadW = gridW − batW
		// − pvW − evW`): pvSurplus = −pvW − loadW = −gridW + batW
		// + evW. We compute the right-hand form because the
		// telemetry store already publishes those three signals
		// directly. Returns (_, false) when the site meter is
		// missing — without it we can't bound grid import.
		lpController.SetSiteSurplusForEV(func() (float64, bool) {
			meterDriver := cfg.SiteMeterDriver()
			if meterDriver == "" {
				return 0, false
			}
			// Refuse to publish a surplus when the meter is stale —
			// last-known SmoothedW lingers indefinitely after a
			// driver crash, and trusting it would silently violate
			// the surplus_only "never import" promise. The watchdog
			// timeout matches what the control loop uses elsewhere
			// for site-meter staleness.
			watchdog := time.Duration(cfg.Site.WatchdogTimeoutS) * time.Second
			if watchdog <= 0 {
				watchdog = 60 * time.Second
			}
			if tel.IsStale(meterDriver, telemetry.DerMeter, watchdog) {
				return 0, false
			}
			meter := tel.Get(meterDriver, telemetry.DerMeter)
			if meter == nil {
				return 0, false
			}
			gridW := meter.SmoothedW
			// Sum battery only over drivers that are currently online.
			// A crashed battery driver leaves SmoothedW at last-known
			// (e.g. +5 kW from a sunny moment) which would inflate
			// surplus indefinitely.
			var batW float64
			for _, r := range tel.ReadingsByType(telemetry.DerBattery) {
				if h := tel.DriverHealth(r.Driver); h == nil || h.Status == telemetry.StatusOffline {
					continue
				}
				batW += r.SmoothedW
			}
			evW := tel.SumOnlineEVW()
			return -gridW + batW + evW, true
		})
	}

	// ---- Self-update checker ----
	// Probes the GitHub Releases API in the background; the UI reads the
	// cached result via /api/version/check. Gated behind FTW_SELFUPDATE_ENABLED
	// because the ftw-updater sidecar only exists in the docker-compose deploy.
	// Native / OS-image builds will ship their own update mechanism and set
	// their own gate (or leave this one off). Deps.SelfUpdate stays nil when
	// disabled, which makes every /api/version/* handler return 503 and the
	// UI hide the badge.
	var selfUpdater *selfupdate.Checker
	// Implicitly enable for dev binaries (Version=="dev") so `make dev`
	// users can click the version label and exercise the probe + modal
	// without setting FTW_SELFUPDATE_ENABLED=1. Production builds (real
	// vX.Y.Z stamped via -ldflags) still require the explicit env var
	// so the feature can't surprise an OS-image deploy.
	if envBool("FTW_SELFUPDATE_ENABLED") || Version == "dev" {
		// FTW_SELFUPDATE_CURRENT_VERSION overrides what the checker thinks
		// it's running so dev / QA can force update_available=true without
		// rebuilding with a fake -ldflags Version. Scoped to the checker
		// only — /api/status, User-Agent, HA discovery keep reporting the
		// real build version. Unset in production; logged loudly when set.
		current := Version
		if v, ok := os.LookupEnv("FTW_SELFUPDATE_CURRENT_VERSION"); ok && v != "" {
			current = v
			slog.Warn("selfupdate: CurrentVersion overridden for testing",
				"real_version", Version, "reported_version", current,
				"env", "FTW_SELFUPDATE_CURRENT_VERSION")
		}
		selfUpdater = selfupdate.New(selfupdate.Config{
			CurrentVersion: current,
			SocketPath:     envOr("FTW_UPDATER_SOCKET", "/run/ftw-update/sock"),
			StatusPath:     envOr("FTW_UPDATER_STATUS", "/run/ftw-update/state.json"),
			// Publish events.UpdateAvailable when a new release lands so
			// the notifications service (or any other subscriber) can act
			// without polling the checker directly.
			Bus: bus,
		}, st)
		selfUpdater.Start(ctx)
		slog.Info("selfupdate enabled", "socket", envOr("FTW_UPDATER_SOCKET", "/run/ftw-update/sock"))
	} else {
		slog.Info("selfupdate disabled — set FTW_SELFUPDATE_ENABLED=1 to turn on")
	}

	// ---- Start HTTP API ----
	// haBridge is forward-declared at the top of the file so the config
	// hot-reload closure can call Reload on it; the bridge instance gets
	// wired further down (HA is optional + depends on reg.Names()).
	deps = &api.Deps{
		Tel: tel, Ctrl: ctrl, CtrlMu: ctrlMu,
		State: st,
		CapMu: capMu, Capacities: capacities,
		CfgMu: cfgMu, Cfg: cfg, ConfigPath: *configPath,
		DriverDir: resolveDriverDir(),
		Models: models, ModelsMu: modelsMu,
		SelfTune: selfTune,
		DtS:        float64(cfg.Site.ControlIntervalS),
		SaveConfig: config.SaveAtomic,
		WebDir:     *webDir,
		ColdDir:    coldDir,
		// Snapshots live next to the rest of the persistent data so
		// docker-compose deploys only need one bind (./data). Derived
		// from the state.db path rather than the config path because
		// `state.db` is always in the main data volume; the config
		// can legitimately live elsewhere (e.g. mounted RO from /etc).
		SnapshotDir: filepath.Join(filepath.Dir(statePath), "snapshots"),
		Prices:     priceSvc,
		Forecast:   forecastSvc,
		MPC:        mpcSvc,
		PVModel:    pvSvc,
		LoadModel:  loadSvc,
		Loadpoints:    lpMgr,
		LoadpointCtrl: lpController,
		HA:            haBridge,
		Registry:   reg,
		Events:     bus,
		Notifications: notifSvc,
		SelfUpdate: selfUpdater,
		Restart: func(reqCtx context.Context) error {
			// Prefer the docker-compose sidecar path when wired up: the
			// updater container does docker compose up -d --force-recreate,
			// which is the same code path post-update restarts use, so
			// there's only one battle-tested escape hatch in production.
			if selfUpdater != nil {
				if err := selfUpdater.Trigger(reqCtx, "restart", ""); err == nil {
					slog.Info("restart: dispatched via updater sidecar")
					return nil
				} else {
					slog.Info("restart: sidecar unavailable, falling back to in-process exit", "err", err)
				}
			}
			// Fallback: drop the main control loop out of its select so
			// every defer (HA Stop, st.Close, http.Shutdown, …) runs
			// cleanly. The os.Exit(1) at the bottom of the defer stack
			// then makes docker (`unless-stopped`) and systemd
			// (`Restart=on-failure`) bring the binary back up.
			restartOnce.Do(func() {
				exitCode = 1
				close(restartCh)
			})
			return nil
		},
		Version:    Version,
	}
	srv := api.New(deps)
	// Dev-mode proxy: when FTW_PROXY_UPSTREAM is set (e.g.
	// http://192.168.1.139:8080), /api/* is forwarded to that instance so
	// the local UI renders live data without owning the control loop.
	// Unset / empty = proxy disabled, /api/* served locally as normal.
	// Read-only by default — writes (POST/PUT/…) come back as 403 so a
	// stray Save in the dev UI can't mutate the real instance. Set
	// FTW_PROXY_READONLY=0 if you explicitly need to exercise write paths.
	handler := srv.Handler()
	if up := os.Getenv("FTW_PROXY_UPSTREAM"); up != "" {
		u, err := url.Parse(up)
		if err != nil || u.Scheme == "" || u.Host == "" {
			slog.Error("FTW_PROXY_UPSTREAM invalid — must be like http://host:port", "value", up, "err", err)
			return
		}
		readOnly := true
		if v, ok := os.LookupEnv("FTW_PROXY_READONLY"); ok {
			switch strings.ToLower(v) {
			case "0", "false", "no", "off":
				readOnly = false
			}
		}
		handler = proxy.Wrap(handler, proxy.Config{Upstream: u, ReadOnly: readOnly})
		slog.Warn("proxy enabled — /api/* forwards upstream",
			"upstream", u.String(),
			"read_only", readOnly)
	}
	httpSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.API.Port),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		slog.Info("HTTP API listening", "addr", httpSrv.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http server", "err", err)
		}
	}()
	defer func() {
		shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	// ---- Notifications (always constructed so API + applier hold live refs) ----
	// Provider selection uses the strategy registry in internal/notifications;
	// only "ntfy" is registered today, but adding a new one is drop-in.
	notifProvider = notifications.NewProvider(cfg.Notifications)
	var notifPub notifications.Publisher
	if notifProvider != nil {
		notifPub = notifProvider
	}
	notifSvc = notifications.New(cfg.Notifications, notifPub, func(name string) (string, string, string, bool) {
		dev := st.LookupDeviceByDriverName(name)
		if dev == nil {
			return "", "", "", false
		}
		return dev.DeviceID, dev.Make, dev.Serial, true
	})
	// FuseReader: on each HealthTick the fuse_over_limit rule reads
	// the site meter's live per-phase currents from telemetry and
	// compares against cfg.Fuse.MaxAmps. Closes over cfg + cfgMu so
	// hot-reloaded fuse changes take effect without restart; closes
	// over tel so new metric emits are picked up immediately.
	notifSvc.SetFuseReader(func() (map[string]float64, float64, bool) {
		cfgMu.RLock()
		siteMeter := cfg.SiteMeterDriver()
		limitA := cfg.Fuse.MaxAmps
		cfgMu.RUnlock()
		if siteMeter == "" || limitA <= 0 {
			return nil, 0, false
		}
		amps := map[string]float64{}
		for _, phase := range []string{"l1", "l2", "l3"} {
			if v, _, ok := tel.LatestMetric(siteMeter, "meter_"+phase+"_a"); ok {
				amps[strings.ToUpper(phase)] = v
			}
		}
		if len(amps) == 0 {
			return nil, limitA, false
		}
		return amps, limitA, true
	})
	notifSvc.Subscribe(bus)
	// Persist every dispatch to state.notification_log via a bus
	// subscriber so the notifications package stays free of storage
	// logic. The UI reads this table through /api/notifications/history.
	bus.Subscribe(events.KindNotificationDispatched, func(e events.Event) {
		ev, ok := e.(events.NotificationDispatched)
		if !ok {
			return
		}
		if err := st.RecordNotification(state.NotificationEntry{
			TsMs:      ev.Time.UnixMilli(),
			EventType: ev.EventType,
			Driver:    ev.Driver,
			Title:     ev.Title,
			Body:      ev.Body,
			Priority:  ev.Priority,
			Status:    ev.Status,
			Error:     ev.Error,
		}); err != nil {
			slog.Warn("notification_log: record failed", "err", err)
		}
	})
	// Late-bind onto the Deps literal that was built earlier with a nil
	// notifSvc (the deps struct is assembled before this block runs).
	// Same pattern haBridge uses a few lines below.
	deps.Notifications = notifSvc
	if cfg.Notifications != nil && cfg.Notifications.Enabled {
		name := "ntfy"
		if notifProvider != nil {
			name = notifProvider.Name()
		}
		slog.Info("notifications enabled", "provider", name)
	}

	// ---- HA MQTT bridge (optional) ----
	if cfg.HomeAssistant != nil && cfg.HomeAssistant.Enabled {
		bridge, err := ha.Start(cfg.HomeAssistant, tel, ctrl, ctrlMu, reg.Names(), haCallbacks(ctrl, ctrlMu, st))
		if err != nil {
			slog.Warn("HA MQTT bridge failed to start", "err", err)
		} else {
			haBridge = bridge
			deps.HA = haBridge // late-binding for API
		}
	}
	// Stop deferred for whichever bridge instance is current at exit
	// time — Reload may have swapped haBridge mid-flight, so re-read here
	// rather than capturing the boot-time pointer.
	defer func() {
		if haBridge != nil {
			haBridge.Stop()
		}
	}()

	// ---- Nova Core federation (optional) ----
	// Publishes telemetry to Sourceful Nova Core's MQTT broker (NATS
	// MQTT adapter). Requires a one-time `forty-two-watts nova-claim`
	// bootstrap to register the gateway's ES256 key and provision
	// device/DER records under an org. When disabled or unconfigured,
	// this block is a no-op.
	if cfg.Nova != nil && cfg.Nova.Enabled {
		keyPath := cfg.Nova.KeyPath
		if keyPath == "" {
			keyPath = filepath.Join(filepath.Dir(statePath), "nova.key")
		}
		novaID, err := nova.LoadOrCreateIdentity(keyPath)
		if err != nil {
			slog.Warn("nova identity load failed — federation disabled", "err", err)
		} else if pub, err := nova.Start(cfg.Nova, novaID, st, tel); err != nil {
			slog.Warn("nova publisher failed to start", "err", err)
		} else if pub != nil {
			defer pub.Stop()
			slog.Info("nova federation enabled",
				"mqtt", fmt.Sprintf("%s:%d", cfg.Nova.MQTTHost, cfg.Nova.MQTTPort),
				"gateway_serial", cfg.Nova.GatewaySerial,
				"schema_mode", cfg.Nova.SchemaMode)
		}
	}

	// ---- OCPP 1.6J Central System (EV chargers) ----
	if cfg.OCPP != nil && cfg.OCPP.Enabled {
		ocppCfg := &ocpp.Config{
			Enabled:            cfg.OCPP.Enabled,
			Bind:               cfg.OCPP.Bind,
			Port:               cfg.OCPP.Port,
			Path:               cfg.OCPP.Path,
			Username:           cfg.OCPP.Username,
			Password:           cfg.OCPP.Password,
			HeartbeatIntervalS: cfg.OCPP.HeartbeatIntervalS,
		}
		ocppSrv, err := ocpp.Start(ctx, ocppCfg, tel)
		if err != nil {
			slog.Warn("OCPP central system failed to start", "err", err)
		} else {
			defer ocppSrv.Stop()
			// API surface for /api/ev_chargers etc. lands in Unit 5.
			_ = ocppSrv
		}
	}

	// ---- Background: Parquet rolloff (>14d → cold dir) ----
	go rolloffLoop(ctx, st, coldDir)

	// ---- Control loop ----
	controlInterval := time.Duration(cfg.Site.ControlIntervalS) * time.Second
	// fuseMaxW is recomputed per tick from ctrl.SiteFuse* under ctrlMu —
	// the configreload watcher updates those fields directly, so a
	// startup snapshot here would go stale on the first hot-reload.
	dtS := float64(cfg.Site.ControlIntervalS)

	// Graceful shutdown
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)

	ticker := time.NewTicker(controlInterval)
	defer ticker.Stop()
	var saveCount uint64
	// Track FuseSaturated edge so we replan once when the joint allocator
	// kicks in — the plan was made without knowledge of the live overage,
	// and a replan with current EV/PV/load state usually finds a feasible
	// schedule that doesn't fight the fuse.
	var prevFuseSaturated bool
	var lastFuseReplan time.Time
	const fuseReplanCooldown = 60 * time.Second
	// One-shot replan when the FIRST DerVehicle reading arrives. The
	// startup replan ran with whatever fallback SoC was available; once
	// the Tesla / vehicle driver gets ground truth from the car, the
	// plan should incorporate it (especially for EV target deadlines).
	var vehicleReplanFired bool
	for {
		select {
		case <-sigc:
			slog.Info("shutting down")
			if err := st.RecordEvent("shutdown"); err != nil {
				slog.Warn("failed to persist shutdown event", "err", err)
			}
			return
		case <-restartCh:
			slog.Info("restart requested via API — exiting cleanly so the supervisor brings us back")
			if err := st.RecordEvent("restart"); err != nil {
				slog.Warn("failed to persist restart event", "err", err)
			}
			return
		case <-ticker.C:
			nowMs := time.Now().UnixMilli()

			// ---- Continuous learning: feed (last_command, actual) per battery ----
			// Skip while self-tune is active — the override would corrupt RLS.
			if !selfTune.Status().Active {
				modelsMu.Lock()
				ctrlMu.Lock()
				lastTargets := append([]control.DispatchTarget{}, ctrl.LastTargets...)
				ctrlMu.Unlock()
				for _, t := range lastTargets {
					r := tel.Get(t.Driver, telemetry.DerBattery)
					if r == nil { continue }
					m, ok := models[t.Driver]
					if !ok { continue }
					soc := 0.5
					if r.SoC != nil { soc = *r.SoC }
					m.Update(t.TargetW, r.SmoothedW, soc, dtS, nowMs)
				}
				modelsMu.Unlock()
			}

			// ---- Self-tune tick ----
			if selfTune.Status().Active {
				modelsMu.Lock()
				selfTune.Tick(func(name string) (float64, float64, bool) {
					r := tel.Get(name, telemetry.DerBattery)
					if r == nil { return 0, 0, false }
					soc := 0.5
					if r.SoC != nil { soc = *r.SoC }
					return r.SmoothedW, soc, true
				}, models, dtS, nowMs)
				modelsMu.Unlock()
			}

			// ---- Watchdog: mark stale drivers offline, revert them to autonomous ----
			watchdogTimeout := time.Duration(cfg.Site.WatchdogTimeoutS) * time.Second
			if watchdogTimeout <= 0 { watchdogTimeout = 60 * time.Second }
			for _, tr := range tel.WatchdogScan(watchdogTimeout) {
				if !tr.Online {
					slog.Warn("driver telemetry stale — marking offline + reverting to autonomous",
						"name", tr.Name, "timeout", watchdogTimeout)
					_ = reg.SendDefault(ctx, tr.Name)
					bus.Publish(events.DriverLost{Driver: tr.Name, At: time.Now()})
				} else {
					slog.Info("driver telemetry recovered — back online", "name", tr.Name)
					bus.Publish(events.DriverRecovered{Driver: tr.Name, At: time.Now()})
				}
			}
			// Fire a HealthTick so subscribers that track user-level
			// thresholds (e.g. notifications) can evaluate their own
			// rules without the control loop knowing about them.
			bus.Publish(events.HealthTick{Health: tel.AllHealth(), Now: time.Now()})

			// ---- Safety: site meter stale → idle everything this cycle ----
			// Otherwise stale grid readings cause one battery to charge another.
			ctrlMu.Lock()
			siteMeterStale := tel.IsStale(ctrl.SiteMeterDriver, telemetry.DerMeter, watchdogTimeout)
			ctrlMu.Unlock()
			if siteMeterStale {
				slog.Warn("site meter telemetry stale — idling batteries this cycle",
					"driver", ctrl.SiteMeterDriver)
				for _, n := range reg.Names() {
					_ = reg.SendDefault(ctx, n)
				}
				continue
			}

			// ---- Compute dispatch ----
			capMu.RLock()
			capsSnap := make(map[string]float64, len(capacities))
			for k, v := range capacities { capsSnap[k] = v }
			capMu.RUnlock()

			ctrlMu.Lock()
			fuseMaxW := ctrl.SiteFuseAmps * ctrl.SiteFuseVoltage * float64(ctrl.SiteFusePhases)
			targets := control.ComputeDispatch(tel, ctrl, capsSnap, fuseMaxW)
			ctrlMu.Unlock()

			// ---- Self-tune override: force commanded battery, hold others at 0 ----
			finalTargets := targets
			if name, cmd, active := selfTune.CurrentCommand(); active {
				finalTargets = make([]control.DispatchTarget, 0, len(reg.Names()))
				for _, n := range reg.Names() {
					if n == name {
						finalTargets = append(finalTargets, control.DispatchTarget{Driver: n, TargetW: cmd})
					} else {
						finalTargets = append(finalTargets, control.DispatchTarget{Driver: n, TargetW: 0})
					}
				}
			}

			// ---- Dispatch to drivers ----
			for _, t := range finalTargets {
				payload, _ := json.Marshal(map[string]any{"action": "battery", "power_w": t.TargetW})
				if err := reg.Send(ctx, t.Driver, payload); err != nil {
					slog.Warn("driver send", "name", t.Driver, "err", err)
				}
			}

			// ---- PV curtailment dispatch ----
			// MPC's annotateCurtailment sets pv_limit_w on slots where
			// exporting more PV would lose money (negative spot, no
			// positive feed-in tariff). ComputePVCurtail picks the
			// drivers that opted in via supports_pv_curtail and emits
			// either a `curtail` command (limit > 0) or a one-shot
			// `curtail_disable` when a previously-curtailed driver
			// drops out of the active set.
			ctrlMu.Lock()
			curtailTargets := control.ComputePVCurtail(ctrl, tel)
			ctrlMu.Unlock()
			for _, c := range curtailTargets {
				var payload []byte
				if c.LimitW > 0 {
					payload, _ = json.Marshal(map[string]any{
						"action":  "curtail",
						"power_w": c.LimitW,
					})
				} else {
					payload, _ = json.Marshal(map[string]any{
						"action": "curtail_disable",
					})
				}
				if err := reg.Send(ctx, c.Driver, payload); err != nil {
					slog.Warn("pv curtail send", "name", c.Driver, "err", err)
				}
			}

			// ---- EV dispatch: per-loadpoint observe + command ----
			// The per-loadpoint state machine (observe → plan lookup
			// → snap → send) is owned by loadpoint.Controller so the
			// main tick stays a thin orchestrator. See issue #172.
			lpController.Tick(ctx, time.Now())

			// ---- Trigger MPC replan on fuse-saturation rising edge ----
			// The joint allocator (control.dispatch) just throttled
			// battery and EV to fit under the fuse. The plan was built
			// without seeing this overage, so let MPC redraw with current
			// state — usually it finds a slot allocation that doesn't
			// require both battery charge and full-bore EV simultaneously.
			ctrlMu.Lock()
			fuseSatNow := ctrl.FuseSaturated
			ctrlMu.Unlock()
			if mpcSvc != nil && fuseSatNow && !prevFuseSaturated && time.Since(lastFuseReplan) > fuseReplanCooldown {
				lastFuseReplan = time.Now()
				go mpcSvc.Replan(ctx)
				slog.Info("fuse-saturated → MPC replan triggered")
			}
			prevFuseSaturated = fuseSatNow

			// First-vehicle-SoC replan trigger: as soon as any
			// DerVehicle driver is online and reporting SoC, redo the
			// plan once with measured-truth instead of the pluginSoC
			// estimate the startup replan used.
			if mpcSvc != nil && !vehicleReplanFired {
				for _, vr := range tel.ReadingsByType(telemetry.DerVehicle) {
					if vr.SoC == nil {
						continue
					}
					if h := tel.DriverHealth(vr.Driver); h == nil || !h.IsOnline() {
						continue
					}
					vehicleReplanFired = true
					go mpcSvc.Replan(ctx)
					slog.Info("first vehicle SoC seen → MPC replan triggered",
						"driver", vr.Driver, "soc", *vr.SoC)
					break
				}
			}

			// ---- Record history snapshot ----
			recordHistory(st, tel, ctrl, nowMs)

			// ---- Flush per-driver metrics into long-format TS DB ----
			if samples := tel.FlushSamples(); len(samples) > 0 {
				stSamples := make([]state.Sample, len(samples))
				for i, sm := range samples {
					stSamples[i] = state.Sample{Driver: sm.Driver, Metric: sm.Metric, TsMs: sm.TsMs, Value: sm.Value}
				}
				if err := st.RecordSamples(stSamples); err != nil {
					slog.Warn("ts samples flush failed", "n", len(samples), "err", err)
				}
			}

			// ---- Periodic battery-model persistence (every 12 cycles ≈ 60s) ----
			saveCount++
			if saveCount%12 == 0 {
				modelsMu.Lock()
				for name, m := range models {
					if data, err := json.Marshal(m); err == nil {
						if err := st.SaveBatteryModel(name, string(data)); err != nil {
						slog.Warn("failed to persist battery model", "battery", name, "err", err)
					}
					}
				}
				modelsMu.Unlock()
			}
		}
	}
}

// rolloffLoop runs the SQLite → Parquet roll-off once per hour. Cheap when
// nothing is due (a single SELECT returns 0 rows); only does real work once
// data crosses the 14-day boundary into cold storage.
func rolloffLoop(ctx context.Context, st *state.Store, coldDir string) {
	tick := time.NewTicker(1 * time.Hour)
	defer tick.Stop()
	// Run once at startup so a fresh boot catches any backlog.
	doRolloff(ctx, st, coldDir)
	for {
		select {
		case <-ctx.Done(): return
		case <-tick.C: doRolloff(ctx, st, coldDir)
		}
	}
}

func doRolloff(ctx context.Context, st *state.Store, coldDir string) {
	rows, files, err := st.RolloffToParquet(ctx, coldDir)
	if err != nil {
		slog.Warn("parquet rolloff failed", "err", err)
	} else if rows > 0 {
		slog.Info("parquet rolloff", "rows", rows, "files", len(files))
	}
	// Planner diagnostics roll off on the same cadence but keep a
	// longer hot tier (30 d vs. the 14 d of ts_samples) — they're
	// sparse enough (~100/day) that the extra month in SQLite
	// costs < 60 MB and makes the time-travel UI snappy for
	// recent-incident debugging.
	dRows, dFiles, err := st.RolloffDiagnosticsToParquet(ctx, coldDir)
	if err != nil {
		slog.Warn("diagnostics parquet rolloff failed", "err", err)
		return
	}
	if dRows > 0 {
		slog.Info("diagnostics parquet rolloff",
			"rows", dRows, "files", len(dFiles))
	}
}

// registerAllDevices snapshots the identity HostEnv has gathered for each
// running driver and upserts a row in the devices table. Idempotent.
// Called periodically because some drivers (notably MQTT) only learn their
// serial after the first message from the device.
func registerAllDevices(st *state.Store, reg *drivers.Registry) {
	for _, name := range reg.Names() {
		env := reg.Env(name)
		if env == nil { continue }
		make, sn, mac, ep := env.FullIdentity()
		dev := state.Device{
			DriverName: name,
			Make:       make,
			Serial:     sn,
			MAC:        mac,
			Endpoint:   ep,
		}
		if id, err := st.RegisterDevice(dev); err == nil && id != "" {
			slog.Debug("device registered", "name", name, "device_id", id, "make", make, "sn", sn, "mac", mac)
		}
	}
}

// driverCapacitiesFrom builds the driver-name → battery-capacity map
// the MPC sums into Params.CapacityWh and the control layer uses for
// fuse-guard / peak-shave sizing.
//
// Critically: drivers that are referenced by a loadpoint entry are
// EV chargers, not home batteries — their `battery_capacity_wh`
// represents VEHICLE capacity and must NOT land in the MPC battery
// pool (doing so inflates SoC %, terminal-value credit, and the
// discharge-headroom DP arithmetic). Found live on Fredrik's Pi: his
// Easee entry had battery_capacity_wh=75000, which combined with
// Ferroamp (15.2 kWh) + Sungrow (9.6 kWh) gave a fantasy 99.8 kWh
// battery pool.
//
// Filtering here rather than at config-parse time means the vehicle
// capacity is still available for EV-side logic (loadpoint manager)
// without a schema migration.
func driverCapacitiesFrom(drivers []config.Driver, loadpoints []config.Loadpoint) map[string]float64 {
	evDrivers := make(map[string]struct{}, len(loadpoints))
	for _, lp := range loadpoints {
		// Only treat a loadpoint row as authoritative when it's
		// valid enough for loadpoint.Manager to actually load it.
		// An entry with an empty id is rejected by the manager (see
		// loadpoint.Manager.Load) — accepting it here would silently
		// drop a real battery from the MPC pool on nothing but
		// config noise.
		if lp.ID == "" || lp.DriverName == "" {
			continue
		}
		evDrivers[lp.DriverName] = struct{}{}
	}
	out := make(map[string]float64, len(drivers))
	for _, d := range drivers {
		if d.BatteryCapacityWh <= 0 {
			continue
		}
		if _, isEV := evDrivers[d.Name]; isEV {
			// Don't count EV vehicle capacity as battery capacity.
			// (Value remains in cfg.Drivers for any driver-side use.)
			continue
		}
		// Fallback detection: operators who haven't migrated to a
		// `loadpoints:` config block still have EV drivers with
		// `battery_capacity_wh` pointing at vehicle capacity. Match
		// on Lua filename prefix — narrow allowlist of known EV
		// charger drivers so we don't accidentally exclude a battery
		// driver whose name happens to share a substring.
		if isLikelyEVDriver(d.Lua) {
			continue
		}
		out[d.Name] = d.BatteryCapacityWh
	}
	return out
}

// driverLimitsFrom builds the driver-name → per-battery PowerLimits map
// used by control.State for per-battery charge/discharge caps (#145).
// Reads the drivers section first, then falls back to the batteries
// section for the same key — operators commonly set per-battery limits
// only under `batteries:` (the MPC reads them from there), and without
// this fallback the dispatcher silently uses the 5 kW MaxCommandW
// default while the planner schedules against the configured 9 kW.
// Drivers without limits in either place are omitted from the map.
func driverLimitsFrom(drivers []config.Driver, batteries map[string]config.Battery) map[string]control.PowerLimits {
	out := map[string]control.PowerLimits{}
	for _, d := range drivers {
		chg, dis := d.MaxChargeW, d.MaxDischargeW
		if b, ok := batteries[d.Name]; ok {
			if chg == 0 && b.MaxChargeW != nil && *b.MaxChargeW > 0 {
				chg = *b.MaxChargeW
			}
			if dis == 0 && b.MaxDischargeW != nil && *b.MaxDischargeW > 0 {
				dis = *b.MaxDischargeW
			}
		}
		if chg == 0 && dis == 0 {
			continue
		}
		out[d.Name] = control.PowerLimits{
			MaxChargeW:    chg,
			MaxDischargeW: dis,
		}
	}
	return out
}

// inverterGroupsFrom builds the driver-name → inverter-group map used by
// control.State for DC-local charge routing (see issue #143). Only
// drivers that set an explicit `inverter_group` make the map; untagged
// drivers inherit today's capacity-proportional behaviour.
//
// A PV-only driver and a battery driver on the same physical inverter
// should both set the same group (e.g. both `inverter_group: ferroamp`)
// so distributeProportional can link PV output to the co-located
// battery's charge target. Config-reload calls this again and swaps the
// map atomically in the control state.
func inverterGroupsFrom(drivers []config.Driver) map[string]string {
	out := map[string]string{}
	for _, d := range drivers {
		if d.InverterGroup == "" {
			continue
		}
		out[d.Name] = d.InverterGroup
	}
	return out
}

// supportsPVCurtailFrom builds the per-driver opt-in map used by
// ComputePVCurtail. Operators set `supports_pv_curtail: true` on
// each driver whose lua handles the `curtail` / `curtail_disable`
// actions (sungrow, ferroamp, deye, huawei, solis ship with it).
// Drivers not in the map are silently skipped by the curtail
// dispatcher — no risk of an EV charger receiving a curtail payload.
func supportsPVCurtailFrom(drivers []config.Driver) map[string]bool {
	out := map[string]bool{}
	for _, d := range drivers {
		if d.SupportsPVCurtail {
			out[d.Name] = true
		}
	}
	return out
}

// isLikelyEVDriver classifies a Lua path as pointing at an EV charger
// driver based on the filename prefix. Kept narrow + allowlist-style —
// a battery driver named "easy_battery.lua" would be wrongly excluded
// by a broader regex, so we only match EV chargers we actually ship.
func isLikelyEVDriver(luaPath string) bool {
	if luaPath == "" {
		return false
	}
	base := strings.ToLower(luaPath)
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(base, ".lua")
	for _, p := range []string{
		"easee",         // easee_cloud.lua, easee.lua
		"ocpp",          // ocpp_cp.lua, ocpp_csms.lua
		"ctek",          // ctek.lua, ctek_mqtt.lua, ctek_v2.lua
		"chargestorm",   // CTEK Chargestorm variants
		"tesla_vehicle", // tesla_vehicle.lua — DerVehicle emitter (BLE proxy)
		"vehicle",       // generic vehicle drivers — anything emitting DerVehicle should NOT be counted as a stationary battery
	} {
		if strings.HasPrefix(base, p) {
			return true
		}
	}
	return false
}

// warnIfEVHasBatteryCapacity surfaces operator mis-config where an EV
// driver's YAML entry still carries battery_capacity_wh. The value is
// now ignored for MPC battery-pool purposes, but we log at WARN so the
// operator moves it to the loadpoint's vehicle_capacity_wh (where it
// serves the DP's EV SoC inference) rather than leaving it as a
// silent no-op.
func warnIfEVHasBatteryCapacity(drivers []config.Driver, loadpoints []config.Loadpoint) {
	evDrivers := make(map[string]struct{}, len(loadpoints))
	for _, lp := range loadpoints {
		if lp.ID == "" || lp.DriverName == "" {
			continue
		}
		evDrivers[lp.DriverName] = struct{}{}
	}
	for _, d := range drivers {
		if d.BatteryCapacityWh <= 0 {
			continue
		}
		_, isEVByLoadpoint := evDrivers[d.Name]
		isEVByFilename := isLikelyEVDriver(d.Lua)
		if !isEVByLoadpoint && !isEVByFilename {
			continue
		}
		reason := "driver is referenced by a loadpoint"
		if !isEVByLoadpoint && isEVByFilename {
			reason = "driver's Lua filename matches a known EV charger"
		}
		slog.Warn(reason+" — battery_capacity_wh is being ignored for MPC "+
			"battery-pool sizing. Move the value to "+
			"loadpoints[].vehicle_capacity_wh to keep EV SoC inference "+
			"working.",
			"driver", d.Name,
			"lua", d.Lua,
			"battery_capacity_wh", d.BatteryCapacityWh)
	}
}

// buildLoadpointConfigs adapts YAML-facing config.Loadpoint entries
// into the internal loadpoint.Config shape. Shared between initial
// boot and the hot-reload watcher so the two paths can't drift.
func buildLoadpointConfigs(src []config.Loadpoint) []loadpoint.Config {
	out := make([]loadpoint.Config, 0, len(src))
	for _, lp := range src {
		out = append(out, loadpoint.Config{
			ID:                lp.ID,
			DriverName:        lp.DriverName,
			MinChargeW:        lp.MinChargeW,
			MaxChargeW:        lp.MaxChargeW,
			AllowedStepsW:     lp.AllowedStepsW,
			VehicleCapacityWh: lp.VehicleCapacityWh,
			PluginSoCPct:      lp.PluginSoCPct,
			PhaseMode:         lp.PhaseMode,
			PhaseSplitW:       lp.PhaseSplitW,
			MinPhaseHoldS:     lp.MinPhaseHoldS,
		})
	}
	return out
}

// aggregateBatteryLimits sums capacity + max charge/discharge across
// battery drivers the MPC should plan for, applying fuse-capacity
// clamps. Returned values are what buildMPC used to compute inline at
// startup — hoisted into a helper so the config-reload path can call
// it and push the new totals into an already-running mpc.Service.
func aggregateBatteryLimits(cfg *config.Config, capacities map[string]float64) (totalCap, maxChg, maxDis float64) {
	for _, d := range cfg.Drivers {
		cap := capacities[d.Name]
		if cap <= 0 {
			continue
		}
		totalCap += cap
		// Default max (de)charge = 0.5C unless overridden. Zero is a
		// legitimate one-sided constraint — `max_charge_w: 0` means
		// "forbid charging, allow discharge only" and mpc.Optimize's
		// action grid (`-MaxDischargeW…+MaxChargeW`) supports it.
		// Negative is always a config mistake.
		//
		// Only the *both-zero* case is treated as a config error (and
		// almost certainly is — it kills the planner's entire action
		// space while leaving the service running). We fall back to
		// default in that case and log a warning.
		defaultP := cap / 2
		chg := defaultP
		dis := defaultP
		if b, ok := cfg.Batteries[d.Name]; ok {
			bothZero := b.MaxChargeW != nil && *b.MaxChargeW == 0 &&
				b.MaxDischargeW != nil && *b.MaxDischargeW == 0
			if bothZero {
				slog.Warn("mpc: batteries.max_{charge,discharge}_w both 0 — treating as config error, using default 0.5C",
					"driver", d.Name, "default_w", defaultP)
			} else {
				if b.MaxChargeW != nil && *b.MaxChargeW >= 0 {
					chg = *b.MaxChargeW
				} else if b.MaxChargeW != nil {
					slog.Warn("mpc: ignoring negative batteries.max_charge_w; using default 0.5C",
						"driver", d.Name, "value", *b.MaxChargeW, "default_w", defaultP)
				}
				if b.MaxDischargeW != nil && *b.MaxDischargeW >= 0 {
					dis = *b.MaxDischargeW
				} else if b.MaxDischargeW != nil {
					slog.Warn("mpc: ignoring negative batteries.max_discharge_w; using default 0.5C",
						"driver", d.Name, "value", *b.MaxDischargeW, "default_w", defaultP)
				}
			}
		}
		maxChg += chg
		maxDis += dis
	}
	// Clamp aggregate charge/discharge to the grid fuse capacity. The
	// control loop's fuse guard enforces this per-tick anyway, but a
	// planner that schedules 45 kW of charge through a 16 A fuse (11 kW)
	// produces SoC projections that can never be realised — the optimiser
	// "charges" to 100% in the plan while the battery barely budges in
	// reality, and every downstream decision (when to discharge, when to
	// idle, what the total cost looks like) is based on that fantasy.
	// Cheaper to keep the plan feasible up-front.
	if fuseMaxW := cfg.Fuse.MaxPowerW(); fuseMaxW > 0 {
		if maxChg > fuseMaxW {
			slog.Info("mpc: clamping MaxChargeW to fuse capacity",
				"requested_w", maxChg, "fuse_w", fuseMaxW)
			maxChg = fuseMaxW
		}
		if maxDis > fuseMaxW {
			slog.Info("mpc: clamping MaxDischargeW to fuse capacity",
				"requested_w", maxDis, "fuse_w", fuseMaxW)
			maxDis = fuseMaxW
		}
	}
	return totalCap, maxChg, maxDis
}

// buildMPC constructs a planner from config. Returns nil if disabled,
// if prices aren't configured, or if there are no batteries with capacity.
func buildMPC(cfg *config.Config, st *state.Store, tel *telemetry.Store, capacities map[string]float64) *mpc.Service {
	if cfg.Planner == nil || !cfg.Planner.Enabled {
		return nil
	}
	if cfg.Price == nil || cfg.Price.Provider == "" || cfg.Price.Provider == "none" {
		slog.Warn("mpc requires price provider — skipping")
		return nil
	}
	totalCap, maxChg, maxDis := aggregateBatteryLimits(cfg, capacities)
	if totalCap <= 0 {
		slog.Warn("mpc: no battery capacity — skipping")
		return nil
	}
	pl := cfg.Planner
	zone := "SE3"
	if cfg.Price != nil && cfg.Price.Zone != "" {
		zone = cfg.Price.Zone
	}
	mode := mpc.Mode(pl.Mode)
	if mode == "" {
		mode = mpc.ModeSelfConsumption
	}
	socMin := pl.SoCMinPct
	if socMin <= 0 {
		socMin = 10
	}
	socMax := pl.SoCMaxPct
	if socMax <= 0 || socMax > 100 {
		socMax = 95
	}
	chgEff := pl.ChargeEfficiency
	if chgEff <= 0 {
		chgEff = 0.95
	}
	disEff := pl.DischargeEfficiency
	if disEff <= 0 {
		disEff = 0.95
	}
	params := mpc.Params{
		Mode:                mode,
		SoCLevels:           41,
		CapacityWh:          totalCap,
		SoCMinPct:           socMin,
		SoCMaxPct:           socMax,
		InitialSoCPct:       50,
		ActionLevels:        21,
		MaxChargeW:          maxChg,
		MaxDischargeW:       maxDis,
		ChargeEfficiency:    chgEff,
		DischargeEfficiency: disEff,
		ExportOrePerKWh:     pl.ExportOrePerKWh,
	}
	svc := mpc.New(st, tel, zone, params)
	svc.BaseLoad = pl.BaseLoadW
	if pl.HorizonHours > 0 {
		svc.Horizon = time.Duration(pl.HorizonHours) * time.Hour
	}
	if pl.IntervalMin > 0 {
		svc.Interval = time.Duration(pl.IntervalMin) * time.Minute
	}
	return svc
}

// isConfigMissing checks whether the error from config.Load indicates the
// config file does not exist (as opposed to a parse or validation error).
// config.Load wraps the os error with fmt.Errorf, so we use errors.Is to
// unwrap through the chain.
func isConfigMissing(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	return strings.Contains(err.Error(), "no such file")
}

func recordHistory(st *state.Store, tel *telemetry.Store, ctrl *control.State, nowMs int64) {
	gridW := 0.0
	if r := tel.Get(ctrl.SiteMeterDriver, telemetry.DerMeter); r != nil {
		gridW = r.SmoothedW
	}
	var pvW, batW, sumSoC float64
	var socCount int
	for _, r := range tel.ReadingsByType(telemetry.DerPV) { pvW += r.SmoothedW }
	for _, r := range tel.ReadingsByType(telemetry.DerBattery) {
		batW += r.SmoothedW
		if r.SoC != nil {
			sumSoC += *r.SoC
			socCount++
		}
	}
	avgSoC := 0.0
	if socCount > 0 { avgSoC = sumSoC / float64(socCount) }
	loadW := gridW - batW - pvW
	if loadW < 0 { loadW = 0 }

	// Per-driver detail packed into the JSON column. The schema is
	// schema-less by design — UI code reads what it understands and
	// ignores the rest, so drivers can add fields without a migration.
	perDriver := make(map[string]map[string]float64)
	for name, h := range tel.AllHealth() {
		row := map[string]float64{}
		if r := tel.Get(name, telemetry.DerBattery); r != nil {
			row["bat_w"] = r.SmoothedW
			if r.SoC != nil { row["soc"] = *r.SoC }
		}
		if r := tel.Get(name, telemetry.DerPV); r != nil {
			row["pv_w"] = r.SmoothedW
		}
		if r := tel.Get(name, telemetry.DerMeter); r != nil {
			row["meter_w"] = r.SmoothedW
		}
		// EV charge power: required for the live chart's EV series
		// (web/next-app.js reads `d.ev_w` per driver from /api/history).
		// Without it the chart's EV trace is always zero — the in-memory
		// /api/status DOES carry ev_w, but history rows never did until
		// this row was added.
		if r := tel.Get(name, telemetry.DerEV); r != nil {
			row["ev_w"] = r.SmoothedW
		}
		_ = h
		perDriver[name] = row
	}
	targets := make(map[string]float64)
	for _, t := range ctrl.LastTargets {
		targets[t.Driver] = t.TargetW
	}
	jsonBlob, _ := json.Marshal(map[string]any{
		"drivers": perDriver,
		"targets": targets,
	})
	if err := st.RecordHistory(state.HistoryPoint{
		TsMs: nowMs, GridW: gridW, PVW: pvW, BatW: batW, LoadW: loadW, BatSoC: avgSoC,
		JSON: string(jsonBlob),
	}); err != nil {
		slog.Warn("failed to persist history point", "err", err)
	}
}

// envOr returns the env var's value if it is set (even if empty, so an
// operator can explicitly blank a path to disable a feature — see
// docs/self-update.md on FTW_UPDATER_SOCKET=""). Returns def only when
// the variable is unset.
// haCallbacks builds the bridge's command-callback set. Extracted so
// the boot-time ha.Start path and the configreload "disabled → enabled"
// path can share the exact same wiring — drift between them would mean
// HA commands behave one way after boot and a different way after a
// hot-reload, which is the kind of silent skew that's hardest to debug.
func haCallbacks(ctrl *control.State, ctrlMu *sync.Mutex, st *state.Store) ha.CommandCallbacks {
	return ha.CommandCallbacks{
		SetMode: func(m string) error {
			ctrlMu.Lock()
			defer ctrlMu.Unlock()
			switch control.Mode(m) {
			case control.ModeIdle, control.ModeSelfConsumption, control.ModePeakShaving,
				control.ModeCharge, control.ModePriority, control.ModeWeighted:
				ctrl.Mode = control.Mode(m)
				return st.SaveConfig("mode", m)
			}
			return fmt.Errorf("unknown mode: %s", m)
		},
		SetGridTarget: func(w float64) error {
			ctrlMu.Lock()
			defer ctrlMu.Unlock()
			ctrl.SetGridTarget(w)
			return st.SaveConfig("grid_target_w", strconv.FormatFloat(w, 'f', 1, 64))
		},
		SetPeakLimit: func(w float64) error {
			ctrlMu.Lock()
			defer ctrlMu.Unlock()
			ctrl.PeakLimitW = w
			return nil
		},
		SetEVCharging: func(w float64, active bool) error {
			ctrlMu.Lock()
			defer ctrlMu.Unlock()
			if active {
				ctrl.EVChargingW = w
			} else {
				ctrl.EVChargingW = 0
			}
			return nil
		},
		SetBatteryCoversEV: func(enabled bool) error {
			ctrlMu.Lock()
			ctrl.BatteryCoversEV = enabled
			ctrlMu.Unlock()
			val := "false"
			if enabled {
				val = "true"
			}
			return st.SaveConfig("battery_covers_ev", val)
		},
	}
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

// envBool returns true iff the env var is set to a positive value
// (1/true/yes/on, case-insensitive). Unset or any other value = false.
func envBool(key string) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
