# Energyplan contract

This is the public contract between FTW Core and Energyplan. Core owns this
document in `docs/energyplan-contract.md`. Energyplan keeps a byte-identical
copy at the same path. Change both in paired PRs. It covers the current
optimizer and forecast protocols, both version 1; it adds no wire fields or
runtime capabilities.

This document defines responsibilities and result meanings. The public Go
types, forecast JSON schemas and process tests define the detailed wire shape.
When code and this contract disagree, record and test the gap; do not silently
change a field's meaning. Private algorithms and benchmark evidence stay in
Energyplan.

## Responsibility

| Core owns | Energyplan owns |
|---|---|
| Measurements, stable device identity, user goals, prices, weather and qualified training records | Model updates and forecasts from the supplied records |
| A frozen planning snapshot, executable device limits and time remaining | A candidate plan, its model cost, status and any valid cost bound |
| Local storage of goals, model state, forecasts and decision evidence | Input and candidate validation before replying |
| Independent plan validation, fallback, live safety and driver commands | Bounded computation and explicit failure when no valid result is available |

Energyplan receives data and returns data. It does not read live devices,
write Core state or command hardware. A plan never grants control authority.
Core must still stop dispatch on stale required site-meter data and apply live
device, fuse and mode limits. Solver success does not prove physical execution.

## Transport and compatibility

The local worker reads one JSON object per line from stdin and writes one
JSON reply per line to stdout. Diagnostics belong on stderr. Core serializes
calls to each warm process and matches replies to requests. It kills a worker
whose I/O exceeds the call deadline; a later call may start a fresh process.

The handshake identifies the worker, its version, protocol range and features.
Protocol compatibility and supported features govern use, not matching Core
and Energyplan release numbers. Current features are `champion`,
`forecast_reset`, `partial_slots`, `demand_charges`, `ev_duty`,
`charging_periods`, `published_prices`, `hourly_import_caps` and
`demand_elapsed_hours`; the worker also reports `forecast_protocol_version`.
Core negotiates `charging_periods` before sending its optional fields, and
`hourly_import_caps` before sending `hourly_import_caps`.
Core negotiates `demand_elapsed_hours` before sending a demand hour that
starts before the first slot or ended by its execution start.

New optional request fields need capability negotiation or a paired update
that preserves deployed clients. Unknown optimizer request fields are errors.
Do not remove a required field or reuse it for a new meaning under an unchanged
contract. Workers with `published_prices` use each supplied price directly and
accept but ignore the retired `confidence` field from older clients. Core omits
that field after negotiation and retains `confidence: 1` for older workers.
Forecast uncertainty belongs in load/PV inputs, not in published-price blending.

Optimizer frames have a 2 MiB request limit and a 16 MiB reply limit. Forecast
clients apply a stricter 2 MiB limit to both directions and a 1 MiB model-state
limit. Limits include framing where the adapter counts it. Valid asset and
slot counts do not guarantee the model will fit its compute budget.

## Planning input

One request contains `schema_version`, `request_id`, settings, contiguous
slots and assets keyed by unique IDs. IDs and slot boundaries must survive
the reply unchanged. Inputs must be finite and physically valid. Unsupported
models must fail explicitly; the worker must not ignore a requested constraint.

| Quantity | Meaning |
|---|---|
| `start_ms`, `len_min` | UTC start and positive duration; 1–512 slots, each at most 1,440 minutes |
| `execution_start_ms` | Optional start inside the first slot only; energy and cost use the remaining time |
| `load_w`, `pv_w` | Household load is positive; PV generation is negative |
| Battery and grid W | Positive means charging or importing; negative means discharging or exporting |
| Energy and efficiency | Stored energy is Wh; efficiency is a fraction in `(0, 1]` |
| `max_import_w`, `max_export_w` | Positive limit magnitudes; zero on this wire means unspecified, not a zero-power constraint |
| `price_per_kwh`, `spot_per_kwh` | Import price and export-price input in one consistent cost unit per kWh |
| `target_slot` | Zero-based EV deadline slot, evaluated at its end; `-1` means no deadline |

Legacy response names ending in `_ore` retain the caller's cost unit.
`soc_pct` and `initial_soc_pct` carry percentages on the wire; Core converts
them to fractions. Do not infer new units from an old field name.

The worker supports up to 64 batteries and EVs combined and up to 32 shared
forecast scenarios. Current FTW sends one deterministic horizon with its PV
downside already applied. Energyplan must not add that margin a second time.
Worker model support does not prove Core can execute every supported case.

Physical capacity, device power, grid limits, allowed EV steps and mode rules
are hard limits. Measured battery energy outside an operating band may recover
without worsening the existing violation, for each battery and for the
batteries' total, whose band is the sum of their bands (Core applies the same
SoC limits to every battery). After recovery it must stay inside the band.
Physical zero and capacity remain hard limits throughout. Default wear cost
is zero; charge/discharge efficiency remains in the physical model.

Multiple batteries keep their own limits. Current execution requires a common
charge/discharge direction in each slot. Demand-charge windows use explicit
UTC intervals and elapsed import from Core; Core owns local calendar and
daylight-saving conversion.

`hourly_import_caps` carries the owner's hourly import limit as a hard
limit. Each entry is `{start_ms, end_ms, max_mean_w, elapsed_import_wh}`:
one local clock hour as UTC milliseconds, the mean import the hour may hold
in W, and the import in Wh the hour has already used before the plan starts
(0 for later hours). Core sends one entry per clock hour that overlaps the
plan, from the first slot's execution start to the horizon end; a
daylight-saving day has 23 or 25. The hour in progress may start before the
first slot and the last may end after the horizon; an entry outside the plan
is ignored. Entries must not overlap, last 1 minute to 25 hours, and number
at most 512. Import is `max(0, grid)` per pulse state: a duty slot imports
`duty * max(0, grid on)` plus `(1 - duty) * max(0, grid off)`, at the cut
points between the cars' duties. In every scenario, the plan's import inside
an hour from execution start must not exceed
`max_mean_w * hours - elapsed_import_wh`. In an hour where every car is off
it may instead reach what the house alone imports there,
`max(0, load_w + pv_w)` per slot with the battery idle, if that is more: Core
cannot cut the house, but its live limit stops a car before the house.
`max_mean_w` may be 0: the hour then has room for the house alone. The worker
is stricter in one case: while a car pulses into solar surplus and the
battery charges, it counts the car's on-step import plus the whole charge.
The cap wins over EV goals, whose miss shows in
`flex_shortfall_wh`. The plan reports each hour in
`plan.hourly_import_caps` as `{start_ms, end_ms, import_wh, max_wh}` on the
published forecast, `import_wh` including the elapsed part.

`demand_charges` carries peak-power tariffs as
`{id, price_per_kw, top_n, already_kw, floor_kw, hours}`, each hour
`{start_ms, end_ms, elapsed_import_kwh, weight, group}` with weight 1 by
default. An hour's sample is its weight times its mean import in kW: the
import before the first slot's execution start, `elapsed_import_kwh`, plus the
plan's import per pulse state from then or `start_ms`, whichever is later, to
`end_ms`, over the hour's length. A group is one sample, the largest of its
hours'; an ungrouped hour and each `already_kw` value are one sample each. The
bill is `price_per_kw * max(floor_kw, mean of the top_n largest samples)`, or
of all samples if there are fewer. Hours must end inside the horizon, not
overlap, last 1 minute to 25 hours and number at most 512 per charge. An hour
that ended by execution start must share its group with an hour that overlaps
the plan. Core makes each local day a group. With `demand_elapsed_hours` it
sends the hour in progress from its start, puts the completed peak of each day
the plan reaches in that day's group and sends each other completed day as one
`already_kw` value, so each local day is one sample. Without the feature Core
sends only hours from the first slot on and every completed day in
`already_kw`. The plan reports `plan.demand_cost` and, per charge in
`plan.demand_charges`, `billed_kw`, `cost` and each hour's unweighted
`mean_kw`, in ascending start order.

Thermal, commercial and recourse inputs are unsupported. Protocol v1 has no
per-phase electrical model, future connection windows or phase-switch schedule.
Total watts cannot certify phase safety. Those extensions need an explicit
capability and a Core validation/dispatch mapping before use.

## EV energy and charging

Physical limits always win. Planning gives deadline energy priority over cost,
handles deadlines in order, then considers energy reserves and charging-period
preferences before cost. A compute limit may stop that search early; it does
not establish the greatest reachable energy or the cheapest plan.

`allowed_steps_w` lists instantaneous AC charging steps. With `ev_duty`:

- `flex_power_w` is mean AC watts over the executable part of the slot.
- `flex_max_power_w` is the allowed on-step; zero means off. Mean power must
  lie between zero and that step.
- Stored EV energy increases by `mean_W * hours * charge_efficiency`.
- Core applies the on-step from the start of the executable part for its
  on-time, the energy budget at the on-step, and on until the metered energy
  is spent if that comes later, then pauses. An off-time under 20 s at the
  slot's end is not sent. Validation must cover joint on/off pulse states, grid limits, surplus-only
  charging, battery-to-EV rules and the corresponding costs.

For example, 1,000 Wh at efficiency 1 over a remaining 15 minutes requires
4,000 W mean. With steps `[0, 6000]`, the on-step is 6,000 W for ten minutes.
A 5,000 W available import margin cannot admit that pulse merely because the
mean fits. This assumes no other load, generation or battery support.

Without on-step metadata, mean power must itself be an allowed full-slot step.
An unmet goal may return a valid partial plan. `flex_shortfall_wh` records
`max(0, target - replayed_energy_at_deadline)` per EV, not horizon-end energy.
Core recomputes shortfall. Missing energy must not relax physical limits or
appear as a saving. Shortfall under a time limit does not prove the goal was
physically impossible.

`charging_periods` adds minimum-period and start-cost preferences plus observed
initial charging duration. These are preferences, not safety limits. A final
top-up may be short. Stale or unknown connection evidence cannot establish
an uninterrupted run; start preferences do not enter the electricity bill.

A need of at most 1 Wh counts as met. With `charging_periods`, a need below
one minimum run rounds to the nearest whole run. One run is the smallest
allowed step for `min_charge_seconds` at `charge_efficiency`. Up to half a run
counts as met; more becomes one full run, capped at `max_energy_wh`. A car
charging at plan start keeps its exact target. Core counts a departure miss
by the same rule. `flex_shortfall_wh` still measures the requested target.

## Result, cost and failure

`ok: true` means the worker returned a candidate. Core checks response identity,
time alignment, asset maps, finite values, energy trajectories, pulse states,
physical limits, mode rules and recomputed cost before publishing it. The same
physical validation applies to a Core fallback. Runtime clamps remain active.

Slot `cost_ore` and `total_cost_ore` describe energy import/export cost for the
published horizon. Demand cost and charging preferences have separate fields.
`solver.objective_ore` may also include terminal value, wear, preferences or
scenario risk. It is not a bill or measured saving. Savings claims need a fair
replay with the same goals, measurements and initial/final stored energy.

| Status or error | Meaning |
|---|---|
| `optimal` | A valid global cost interval closed within numerical tolerances for the stated model and objectives |
| `gap_satisfied` | The search met its configured gap target; a nonzero gap remains |
| `feasible` | A validated candidate exists; optimality may be unproved and the bound unknown |
| `infeasible` | The modeled physical problem has no feasible plan; a failed heuristic or expired budget cannot establish this |
| `deadline_exceeded` | A resource limit left no usable result; it says nothing about physical feasibility |
| `invalid_request` | Malformed or unsupported input |
| `numerical_failure`, `internal_error`, `cancelled` | Computation failed or was cancelled; no false success or impossibility claim |

For minimization, a reported bound must satisfy
`lower_bound <= optimum <= candidate objective` for the same feasible set and
objective. Unknown bounds and gaps remain null. When executed pulse cost and
the optimized cost model differ, report `feasible` and omit the cost bound.
A plan from a search that left out part of the model reports `feasible` with
a null bound. Core validates physical execution; it does not independently
prove optimality.

Core makes no worker call when less than 500 ms of the first slot remain. It
allows 500 ms for small plans, 1.5 s with an active car, and 5 s when
slots × (3 × batteries + 2 × active cars) is at least 965, or when the request
carries PV curtailment, demand charges or hourly import caps. Each call's
`time_limit_s` is the smallest of that budget, 5.6 s and the time left before
that call's deadline once its request is prepared, less a reply margin of
max(5% of that time, 30 ms). A later call of the same planning attempt, such as
the forecast-caution call after the point call, can get less, and Core sends
no request with less than 450 ms before its deadline. Core starts the worker
with `--time-limit=5s`. The transport timeout is 7 s. Near the first slot's end
Core keeps 50 ms for validation and publication and a reply margin of
max(5% of the remaining time after that, 30 ms).
These are compute and call budgets, not a proven wall-time SLA. Measure
queueing, startup, decoding, solving, replay, encoding and Core validation
separately when changing them.

A warm worker may keep one previous plan as a candidate. Before reuse it must
match asset identities, advance time, rebuild energy from current measurements,
reprice all actions and replay current physical limits and scenarios. New tail
slots need validation too. Cached costs, bounds and optimal status never carry
over. A candidate must compete with fresh planning on deadline energy, charging
preferences and cost. `solver.warm_start_valid`, when present, means a cached
candidate passed this check; it does not mean the final plan used it. Restart
may discard this optional cache without losing stored user goals.

Replies may carry optional diagnostics. `solver.source`, `solver.fallbacks`,
`error.fallbacks` and `error.model_variables` are worker-defined, opaque
numeric codes; each `fallbacks` is an array of them. `solver.model_variables`
is an integer; Core rejects a reply whose value is not one. Core never
accepts, rejects or ranks a plan by the values of these diagnostics.

On failure Core may use its validated fallback only where it can represent
the site. Otherwise an old plan may remain as diagnostic history; retention
does not grant permission to execute stale or unvalidated actions. Restart
and failure must preserve stored goals and local safety.

## Forecasts and learning state

Forecast requests use `op: "forecast"`, `version: 1` and `action` set to
`update`, `predict` or `reset`. Core checks the echoed operation, action,
request ID, site ID, configuration revision and origin time. Public schemas
define the payloads. Forecast work runs in a separate local worker, outside
control and dispatch locks.

Training records describe complete UTC quarters. Prediction intervals may
cover only the remaining part of a quarter. Observations and weather must
have been available at `origin_ms`; future truth must not enter training or
evaluation. Updates accept at most 4,096 observations; predictions at most
512 intervals. Missing or stale observations are not zero measurements.

This protocol uses generation-positive `pv_available_w` and PV estimates.
Core converts that magnitude at the forecast adapter to planning's negative
`pv_w`. This is a distinct model field, not a change to telemetry signs.
Household-load labels exclude separately modeled EV and battery energy so
planning cannot count it twice.

Predictions state whether a signal is known, its quality, coverage and
uncertainty. Provisional model bounds are not calibrated quantiles. Missing,
late, incomplete or rejected predictions trigger per-signal Core fallback;
current Core also keeps legacy load during Energyplan load cold start.
When a week of paired, scored errors shows one source clearly better for a
signal, Core uses that source instead of the quality label.
Core records which signal supplied each planner input.

Energyplan returns complete opaque model state. Core validates and stores an
update atomically before exposing it to planning, and restores it on restart.
A site/configuration revision prevents reuse with a different input identity.
A compatible program upgrade need not erase learning. Reset requires the
advertised capability and must preserve the other signal. Issued forecasts
retain their origin and revision for evaluation against later measurements.

## Evidence and maintenance

Public definitions live in FTW's `go/internal/mpc/external_optimizer.go`,
`go/internal/optimizercontract`, `go/internal/energyforecast` and
`optimizer/native/bundle/forecast-v1*.schema.json`. Runtime and packaging
instructions live in `optimizer/native/README.md`.

A contract change needs paired request/response and rejection tests. Cover
partial first slots, zero-limit semantics, pulse overlap and shortfall,
battery recovery, wrong identities, timeout/restart, unknown forecast values
and state round trips where affected. An incompatible payload change must not
ship with an older bundled worker.

Run Energyplan's `make verify`; for a bundle or integration change also run
FTW's `make native-solver-test` and `make verify` against the exact binaries.
Ship only compiled workers, checksums, source identity, public schemas, license
and notices to FTW. Synthetic tests establish only their stated scope.
Target-box timing, held-out forecast accuracy and physical charging each need
their own evidence.

Keep both copies byte-identical. From the workspace root, check with
`cmp FTW/docs/energyplan-contract.md energyplan/docs/energyplan-contract.md`.
Keep evaluation reports and implementation plans outside this contract.
