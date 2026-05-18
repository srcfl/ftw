# Safety and protective clamping

forty-two-watts runs unattended. A home battery pushing the wrong way
for thirty seconds can flip a fuse; a model learning from its own
clamped output can lock itself; a driver that went silent at 02:00
can leave another battery charging it from stale telemetry. This doc
catalogues every defensive mechanism in the stack: what each one
protects against, where it lives, and why removing it is a bad idea.

Site sign convention applies throughout: positive W = into the site,
negative W = out of the site. See
[site-convention.md](site-convention.md) for the full rule.

## 1. Layered defenses

Four independent layers, each handling a different failure class.
No single layer is sufficient; they compose.

| Layer | Guards against | Lives in |
|---|---|---|
| Watchdog | Silent drivers, stale telemetry | `go/internal/telemetry/store.go` + `go/cmd/forty-two-watts/main.go` |
| Dispatch clamps | Out-of-budget commands, oscillation | `go/internal/control/dispatch.go` + `go/internal/control/pi.go` |
| Model sanity envelopes | Wild RLS coefficients from bad samples | `go/internal/pvmodel/model.go` + `go/internal/battery/model.go` |
| Default mode | EMS offline, driver excluded | `drivers/*.lua` `driver_default_mode` |

The rest of this document walks each layer in turn.

## 2. Watchdog (telemetry staleness)

Every control cycle the telemetry store scans per-driver health and
transitions status based on how fresh the last successful read was.

```go
// go/internal/telemetry/store.go:309
func (s *Store) WatchdogScan(timeout time.Duration) []WatchdogTransition
```

For each driver in `s.health`:

- `stale := h.LastSuccess == nil || now.Sub(*h.LastSuccess) > timeout`
- stale and was online → `Status = StatusOffline`, emit transition with
  `Online: false`
- fresh and was offline → `Status = StatusOk`, reset `ConsecutiveErrors`,
  emit transition with `Online: true`

See [`store.go:309-326`](../go/internal/telemetry/store.go) and the
`WatchdogTransition` type just below it.

### Default timeout

`cfg.Site.WatchdogTimeoutS` (default **60s**, set in
[`config.go:249-250`](../go/internal/config/config.go)).

### Reaction to a new offline driver

The main loop drains the transitions once per tick and calls
`reg.SendDefault(ctx, name)` on each driver that just went offline.
That routes into the driver's Lua `driver_default_mode()` function —
Sungrow reverts to built-in self-consumption, Ferroamp returns to
auto. See [`main.go:489-500`](../go/cmd/forty-two-watts/main.go).

```go
// main.go
for _, tr := range tel.WatchdogScan(watchdogTimeout) {
    if !tr.Online {
        slog.Warn("driver telemetry stale — marking offline + reverting to autonomous",
            "name", tr.Name, "timeout", watchdogTimeout)
        _ = reg.SendDefault(ctx, tr.Name)
    } else {
        slog.Info("driver telemetry recovered — back online", "name", tr.Name)
    }
}
```

An offline driver is also excluded from dispatch — `ComputeDispatch`
filters batteries by `h.IsOnline()` at
[`dispatch.go:220-229`](../go/internal/control/dispatch.go).

### Site-meter staleness short-circuit

The site meter is checked separately, immediately after the per-driver
watchdog scan:

```go
// main.go:502-514
siteMeterStale := tel.IsStale(ctrl.SiteMeterDriver, telemetry.DerMeter, watchdogTimeout)
if siteMeterStale {
    slog.Warn("site meter telemetry stale — idling batteries this cycle",
        "driver", ctrl.SiteMeterDriver)
    for _, n := range reg.Names() {
        _ = reg.SendDefault(ctx, n)
    }
    continue
}
```

Every driver gets `SendDefault` and the rest of the cycle is skipped.
This prevents the worst-case failure where one battery tries to
"discharge into a load" that is actually another battery, because the
shared grid reading is minutes old and nobody sees the real picture.

### Recovery

Fully automatic. When telemetry resumes, the next scan flips status
back to `Ok`, the driver re-enters dispatch, and the PI controller
picks up where it left off. No operator action, no restart.

## 3. Fuse guard

Total site current must stay within the physical breaker rating, no
matter what the PI says.

```go
// go/internal/control/dispatch.go:436
func applyFuseGuard(targets []DispatchTarget, store *telemetry.Store, fuseMaxW float64) []DispatchTarget
```

The power budget is derived from config:

```go
// config.go:67
func (f Fuse) MaxPowerW() float64 {
    return f.MaxAmps * f.Voltage * float64(f.Phases)
}
```

Cycle-level check at [`dispatch.go:436-461`](../go/internal/control/dispatch.go):

1. Sum `|PV|` across every PV reading in the telemetry store.
2. Sum `|discharge target|` across every negative target.
3. If `totalPV + totalDischarge > fuseMaxW`, scale every negative
   target by `fuseMaxW / totalGeneration` and mark `Clamped = true`.

The scale is proportional so the per-battery distribution (from
`distributeProportional` / `distributeWeighted` / `distributePriority`)
is preserved while staying inside the breaker envelope.

Per-phase imbalance isn't modeled in `applyFuseGuard` directly, but
Sungrow and Ferroamp both emit per-phase data into
`DerReading.Data` — drivers that need per-phase guards can read that
JSON blob. The aggregate three-phase guard is the floor; per-phase
logic is opt-in on top.

### 3a. Reactive fuse-saver (PR #208)

`applyFuseGuard` only scales POSITIVE (charge) targets DOWN — it
prevents the EMS from making an existing overflow worse, but doesn't
help in the common "battery idle, surprise load" case because there's
no charge to shrink.

The reactive fuse-saver (`forceFuseDischarge`) closes that gap:

```go
// go/internal/control/dispatch.go
func forceFuseDischarge(
    targets []DispatchTarget,
    store *telemetry.Store,
    state *State,
    capacities map[string]float64,
    fuseMaxW float64,
) []DispatchTarget
```

Runs **after** `applyFuseGuard` on every dispatch cycle, in **every**
mode (idle, self_consumption, planner_*, holdoff window). Behaviour:

1. Recompute `predicted = currentGrid − currentBat + sumTarget` against
   the post-`applyFuseGuard` targets. `currentGrid` is the live meter
   and reflects ALL loads — planned, off-plan, manual_hold-injected,
   and unplanned spikes.
2. If `predicted > fuseMaxW`, allocate `overage = predicted − fuseMaxW`
   of additional discharge across online batteries proportionally to
   each battery's remaining headroom (`MaxDischargeW − current target
   magnitude`, gated on `SoC ≥ 5 %`).
3. Mark every modified target `Clamped = true` so the dispatch trace
   shows the fuse-saver fired.

Coverage extends to every code path that would normally short-circuit
`ComputeDispatch` to `nil`:

- **`ModeIdle`**: zeros are generated for every online battery, run
  through `forceFuseDischarge`. Idle mode + grid spike → battery is
  overridden to discharge.
- **Holdoff window**: same — fuse-saver overrides the 5 s holdoff
  because overflow can't wait.

Edge cases:

- All batteries empty (SoC < 5 %) → fuse-saver returns targets
  unchanged. Hardware breaker remains the next layer.
- All batteries already at `MaxDischargeW` → fuse-saver no-op (already
  doing all it can).
- `fuseMaxW = 0` → disabled (matches `applyFuseGuard`'s convention).

The 5 s control-tick is the floor. Sub-tick spikes (an oven turning
on between ticks) still rely on the hardware fuse for protection;
going faster requires pushing the dispatch loop down to ~1 s.

Surfaced by the manual_hold ramp test in PR #206's session: the EV
was pinned at ~5.5 kW while the home battery sat at 0 W per the
planner's idle slot, and `gridW` exceeded `fuseSafeMaxW` until the
operator stopped the test. The reactive fuse-saver eliminates that
class of incident at the dispatch level.

### 3b. Per-phase clamp (PR #208 follow-up)

The aggregate fuse-guard above (sections 3 + 3a) protects against
total grid power exceeding the breaker's combined rating. It does
**not** see per-phase imbalance: a 16 A 3Φ fuse has each phase trip
at 16 A, and a single phase can blow even when the three-phase
aggregate is well below `fuseMaxW`. This was the failure mode the
operator hit in PR #208's hardware test — the EV was at ~16 A 3Φ
balanced (3.6 kW per phase), but a single-phase Pixii battery
charging at 4.4 kW on L1 pushed that one phase past 16 A while the
aggregate stayed under fuse.

Both `applyFuseGuard` and `forceFuseDischarge` consult an additional
`perPhaseImportOverageW(store, state)` helper:

```go
// go/internal/control/dispatch.go
func perPhaseImportOverageW(store *telemetry.Store, state *State) float64
```

Reads `l1_a` / `l2_a` / `l3_a` from the meter driver's
`DerReading.Data` (Pixii, Ferroamp, Sungrow all emit these). Returns
the wattage by which the worst single phase exceeds
`state.SiteFuseAmps`, or 0 when within limits / data unavailable /
clamp disabled.

The dispatch logic then takes the larger of:

- aggregate overage = `predicted_grid − fuseMaxW`
- per-phase overage = `worst_phase_watts × 3` (balanced-3Φ assumption
  for the battery — total reduction needed to bring the worst phase
  back, accepting over-correction on the other phases)

…and uses that as the reduction/discharge target. The existing
charge-scaling and force-discharge code paths do not change; only
the input number is now per-phase aware.

**Configuration.** `state.SiteFuseAmps` and `state.SiteFuseVoltage`
are wired from `cfg.Fuse.MaxAmps` + `cfg.Fuse.Voltage` in `main.go`.
`SiteFuseAmps == 0` disables the per-phase clamp (back-compat for
sites without per-phase meter data and the test suite).

**Conservatism for 1Φ batteries.** A balanced 3Φ battery reduces
each phase by 1/3 of its total output, so `× 3` is exact. A
single-phase battery (Pixii Home, OCPP single-phase) on the
overloaded phase reduces it 1:1 — `× 3` over-corrects 3×, but that
direction is safe (less import on the overloaded phase, slight
over-export elsewhere that the aggregate guard catches next cycle).
A single-phase battery on a *different* phase from the overload
cannot help; this is a real limitation. Per-battery `phase`
configuration is a follow-up.

## 4. Dispatch min interval

`cfg.Site.MinDispatchIntervalS` (default **5s**, set in
[`config.go:261-262`](../go/internal/config/config.go)) caps how often
the PI controller is allowed to issue a new command. Stored in
`state.MinDispatchIntervalS` (see
[`dispatch.go:111`](../go/internal/control/dispatch.go)) and enforced
at the top of `ComputeDispatch`:

```go
// dispatch.go:187-192
if state.LastDispatch != nil {
    elapsed := time.Since(*state.LastDispatch).Seconds()
    if elapsed < float64(state.MinDispatchIntervalS) {
        return nil
    }
}
```

Reasons this exists:

- **Oscillation guard** — a PI that sends a new command every 250 ms
  will fight the battery's own internal loop.
- **Modbus saturation** — Sungrow over Modbus TCP can't field a
  command faster than a few Hz without queuing.
- **Settling time** — the site meter smoothing filter needs ~2–3 s
  to reflect the effect of the previous command.

## 5. PI anti-windup and slew rate

Two clamps live in or around the PI controller.

### Anti-windup on the integral

[`pi.go:41-58`](../go/internal/control/pi.go) — after each integral
update, the integral is clamped to `±IntegralLimit`:

```go
p.integral += p.Ki * err
if p.integral > p.IntegralLimit {
    p.integral = p.IntegralLimit
} else if p.integral < -p.IntegralLimit {
    p.integral = -p.IntegralLimit
}
```

Default integral limit is **3000 W** (`NewPI(0.5, 0.1, 3000, 10000)`
in [`dispatch.go:98`](../go/internal/control/dispatch.go)). Without
this, a pinned actuator (battery at saturation, fuse guard clamping,
driver offline) accumulates error forever; when control resumes, the
monstrous integral overshoots for minutes.

### Slew rate per driver

[`dispatch.go:286-298`](../go/internal/control/dispatch.go) — after
distribution but before the fuse guard, each driver's new target is
constrained relative to its previous target:

```go
if prev, ok := state.PrevTargets[raw[i].Driver]; ok {
    delta := raw[i].TargetW - prev
    if math.Abs(delta) > state.SlewRateW {
        // snap to prev ± SlewRateW
    }
}
```

Default `cfg.Site.SlewRateW` is **500 W** (per control interval, see
[`config.go:258-259`](../go/internal/config/config.go)). Prevents a
"charge to discharge in one step" command that would spike phase
currents and interact badly with the battery's own PI loop.

## 6. Battery cascade saturation curves

Per-SoC empirical envelopes of what each battery has actually
delivered. Live in
[`battery/model.go:193-206`](../go/internal/battery/model.go):

```go
func (m *Model) ClampToSaturation(target, soc float64) (clamped float64, wasClamped bool) {
    if target > 0 {
        max := interpolate(m.MaxChargeCurve, soc, 5000)
        if target > max { return max, true }
    } else if target < 0 {
        max := interpolate(m.MaxDischargeCurve, soc, 5000)
        if -target > max { return -max, true }
    }
    return target, false
}
```

Curves are populated by `updateSaturationCurves` on every `Update`
call ([`model.go:322`](../go/internal/battery/model.go)) — each
observation is bucketed to 5% SoC and the running max per bucket is
tracked. A slow decay factor (`SatDecay = 0.9999`) lets old peaks
fade so a one-off high reading doesn't pin the envelope forever.

### The self-reinforcing clamp bug (and the guard against it)

A small observation can lock a bucket: if the battery is clamped to
255 W, the observation is 255 W, and the curve records that as the
max. Next cycle the clamp is still 255 W, and forever. Fix lives in
`updateCurve` at
[`model.go:339-357`](../go/internal/battery/model.go):

```go
if value < MinSatSeedW {
    return curve  // don't seed a new bucket from a tiny observation
}
```

`MinSatSeedW = 1000` ([`model.go:28`](../go/internal/battery/model.go))
— new buckets need to see at least 1 kW before getting recorded.
Existing buckets can still grow from any observation; the guard is
purely at bucket-creation time.

### Confidence gating

Below `confidence < 0.5`, the cascade controller is bypassed entirely.
`Model.Confidence()` at
[`model.go:135-142`](../go/internal/battery/model.go) combines sample
count and residual-variance EMA. A cold-started or just-diverged
model can't produce a trustworthy saturation envelope, so the PI
raw target passes through directly to the slew + fuse guards —
both of which are static, quantifiable clamps.

## 7. PV twin sanity envelopes

The PV digital twin
[`pvmodel/model.go`](../go/internal/pvmodel/model.go) is a 7-feature
RLS regression. Three envelopes catch pathological samples:

### Input filter: reject impossible measurements

```go
// model.go:157-160
if m.RatedW > 0 && actualPVW > 1.2*m.RatedW {
    return false
}
```

An inverter restart, transient, or miswired sensor can report values
far above nameplate. Feeding them to RLS poisons β permanently.

### Cold-start guard: reject wild predictions

```go
// model.go:168-172
if m.RatedW > 0 && math.Abs(yHat) > 2*m.RatedW {
    return false
}
```

Before the MAE-based 10σ outlier filter has enough data
(`m.Samples > 50` at [`model.go:175`](../go/internal/pvmodel/model.go)),
a single bad sample can drive β large. If the predicted ŷ before
fitting is already > 2× rated, drop the sample — the next good one
lets β recover.

### Output cap: return the prior instead of the clipped wild value

```go
// model.go:138-143
if m.RatedW > 0 && y > 1.05*m.RatedW {
    return prior
}
```

At prediction time, if the learned model wants to output more than
**1.05 × rated**, fall back to the naive physics prior
`rated × (clearsky/1000) × (1-cloud)^1.5`.

The history here matters. The previous behaviour was a 1.3× cap that
just clipped — so a runaway model that wanted 50 kW on a 10 kW
system would report 13 kW confidently, every prediction, until
enough samples tamed β. Returning the prior instead means the
forecast degrades to "as good as before we had a twin" during the
bad period, and recovers when β does.

## 8. Default mode (`driver_default_mode`)

Every Lua driver exposes a `driver_default_mode()` function invoked
by `reg.SendDefault` ([`registry.go:255-267`](../go/internal/drivers/registry.go)).
It is the safe autonomous state the hardware falls back to when the
EMS is not in command of this device.

### Sungrow

[`drivers/sungrow.lua:380-383`](../drivers/sungrow.lua) — revert to
the built-in self-consumption mode:

```lua
function driver_default_mode()
    host.log("info", "Sungrow: watchdog → reverting to self-consumption")
    set_self_consumption()
end
```

`set_self_consumption` at
[`sungrow.lua:365-370`](../drivers/sungrow.lua) writes `0xCC` to
register `13050` (stop forced charge/discharge) and `0` to register
`13049` (self-consumption mode).

### Ferroamp

[`drivers/ferroamp.lua`](../drivers/ferroamp.lua) publishes the `auto`
command over MQTT:

```lua
local function publish_auto(trans_id)
    return host.mqtt_publish("extapi/control/request",
        string.format('{"transId":"%s","cmd":{"name":"auto"}}', trans_id))
end

function driver_default_mode()
    publish_auto("watchdog")
end

function driver_cleanup()
    pcall(publish_auto, "cleanup")
end
```

Same semantics: the hardware takes over and does its own
self-consumption logic until the EMS returns. `driver_cleanup` uses the
same fallback so a hot-reload, driver disable, or clean service stop
does not leave the EnergyHub in the last forced `charge` / `discharge`
reference.

Triggers for default-mode invocation:

- Watchdog transition to offline (see section 2)
- Site-meter stale short-circuit (every driver, see section 2)
- Driver shutdown / hot-reload

## 9. Failure-mode catalog

| Failure | Detection | Response |
|---|---|---|
| Driver MQTT silent | `LastSuccess > watchdog_timeout` in `WatchdogScan` | Mark offline + `SendDefault`; exclude from dispatch |
| Driver Modbus errors 3+ in a row | `RecordError` → `StatusDegraded` ([`store.go:98-105`](../go/internal/telemetry/store.go)) | Warn but keep using last known values |
| Site meter telemetry stale | `tel.IsStale` on `SiteMeterDriver` in main loop | Skip whole cycle, every driver → default mode |
| MPC plan stale (>30 min) | `MaxPlanAge` check in `GridTargetAt` ([`mpc/service.go:121-142`](../go/internal/mpc/service.go)) | Fall back to self-consumption with `grid_target=0`, set `state.PlanStale = true` |
| PV twin coefficients wild | `Predict` output > 1.05× rated | Return physics prior instead of β value |
| PV twin input wild | `actualPVW > 1.2 × rated` or `|yHat| > 2 × rated` | Reject sample from RLS fit |
| Battery saturation: commanded charge > envelope at SoC | Cascade controller checks `ClampToSaturation` | Reduce target to saturation envelope |
| Battery model diverging | `Confidence < 0.5` | Bypass cascade + inverse; use raw PI target |
| Commanded target changes too fast | `|delta| > SlewRateW` | Snap to prev ± `SlewRateW`, mark `Clamped` |
| Fuse budget exceeded | `totalPV + totalDischarge > MaxPowerW` | Scale all discharge proportionally |
| Controller integral saturates | Integral update would exceed `IntegralLimit` | Clamp to `±IntegralLimit` |

## 10. Things you should never bypass

- **The fuse guard.** Real fuses melt; modeled limits don't. Match
  `max_amps` to the physical fuse minus a safety margin. Turning it
  off or raising the rating past the breaker risks the whole house
  dark at 03:00.
- **The site sign convention.** Flip a sign at any layer above the
  driver boundary and you'll get the wrong answer everywhere. See
  [site-convention.md](site-convention.md).
- **The watchdog timeout.** Zero disables it. Very large values
  defeat it. 60 s is the default because it's a few control
  intervals — long enough to ride out a single missed publish, short
  enough that a genuinely dead driver is caught before it does
  damage.
- **Default mode implementations.** Every driver must have one. The
  safe state is what the hardware does when there is no EMS at all;
  losing that fallback means "EMS crashed at 02:00" becomes
  "batteries ran open-loop until morning."
