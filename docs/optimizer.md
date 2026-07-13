# Mathematical optimizer

The primary MPC engine is a local Python worker built with CVXPY. HiGHS solves
linear and mixed-integer models; CLARABEL is available for continuous convex
models. The Go host remains responsible for forecasts, configuration, safety
validation, plan persistence, and dispatch.

## Process boundary

The host starts one long-lived worker:

```text
Go MPC service
  -> versioned JSON planning snapshot
  -> Python / CVXPY model
  -> HiGHS or CLARABEL
  -> candidate plan
  -> Go physics and policy validator
  -> active plan cache
```

The protocol is JSON Lines on stdin/stdout. It is deliberately local and has no
network listener. Worker logs go to stderr. A request includes no credentials or
hardware endpoints.

`schema_version` is mandatory. A worker response must carry the same
`request_id`; mismatched, malformed, late, non-finite, or infeasible responses
are rejected as a unit.

## Model

Every slot uses the site convention:

```text
grid = house load + PV + curtailment
     + storage charge - storage discharge
     + flexible loads + thermal loads
```

Positive grid power is import. PV is negative. Storage charging and EV/thermal
loads are positive.

The model supports:

- any number of storage resources with individual energy, power, efficiency,
  terminal value, cycle cost, deadline, and reserve constraints;
- any number of flexible loads with continuous or enumerated power steps;
- bidirectional resources through storage contracts, including V2X reserve and
  departure targets;
- first-order thermal states with comfort bands, heat loss, outside-temperature
  forecasts, continuous power, or discrete compressor stages;
- PV curtailment as a decision variable;
- site import/export limits;
- multiple load/PV scenarios with shared champion schedules;
- an opt-in storage-only recourse shadow with a shared executable prefix;
- expected cost plus configurable CVaR tail risk.

If live storage energy starts outside a configured operating band, the initial
violation is treated as a recoverable state rather than an invalid request. It
may never worsen, recovery is prioritized before economics, and the bound
becomes hard as soon as the trajectory returns inside it. Physical bounds of
zero and full capacity remain hard throughout.

The Go integration sends every online home battery as a separate storage
resource, including its live SoC and configured charge/discharge limits. The
existing aggregate fleet safety limit is distributed proportionally across the
online devices. Dispatch still receives one aggregate battery target because
the control layer already allocates that target across the fleet.

Battery and grid direction binaries are introduced only when required by the
economics, such as negative import prices or an import/export price inversion.
Discrete EV and thermal steps always make the strict model a MILP. This keeps
ordinary battery-only planning convex without allowing artificial simultaneous
charge/discharge cycles in the edge cases where losses are profitable.

## Objective priority

Planning is lexicographic:

1. Minimize normalized storage-bound recovery, deadline shortfall, and
   comfort-band violation.
2. Lock that service optimum.
3. Minimize expected import cost minus export revenue, plus CVaR risk and cycle
   cost, minus terminal storage value and configured PV preference.

This means an expensive but feasible EV deadline or comfort requirement wins
over energy cost. When a requirement is physically infeasible, the plan returns
the smallest possible shortfall instead of returning no plan.

## Forecast risk

When the PV twin exposes a residual standard deviation, Go sends three shared-
decision scenarios:

| Scenario | Probability | PV |
|---|---:|---|
| base | 0.60 | current forecast |
| downside | 0.25 | `max(0, forecast - k*sigma)` |
| upside | 0.15 | `forecast + k*sigma` during daylight |

The default CVaR weight is 0.15 at alpha 0.90. Setting
`optimizer_cvar_weight: 0` explicitly disables tail-risk cost. If the Python
engine fails, the Go-DP fallback receives the previous downside-only forecast,
so an optimizer outage does not weaken the existing PV reserve behavior.

## Solver cascade

- HiGHS is primary for LP and MILP.
- CLARABEL can be selected or used after a HiGHS failure only when the model has
  no integer variables.
- The in-process Go DP is the final operational fallback.

When Python produces a valid active plan, the Go DP also evaluates the
downside-PV fallback input as a diagnostic shadow. The shadow never reaches
dispatch. `plan.dp_shadow` records its horizon cost, first action, mean/max
battery-power difference, and direction-disagreement count. If the Python
worker or Go validator fails, the same DP path stops being a shadow and becomes
the active fallback for that replan.

The active plan exposes engine, backend, formulation, status, objective,
service slack, solve time, MIP gap, scenario count, and fallback reason under
`plan.solver`. The same metadata is persisted in planner diagnostics.

## Go validation

Before activation, Go independently replays:

- slot identity and duration;
- finite numeric values;
- aggregate and per-device battery power and energy transitions with
  charge/discharge efficiency;
- every loadpoint's allowed step and energy/SoC transition;
- PV curtailment bounds;
- site power balance;
- planner-mode policy;
- fuse/import/export limits;
- raw per-slot and total cost.

Validation uses tight numerical tolerances only. It does not clamp or repair a
solver result; an invalid plan falls back in full.

Every accepted Python plan records two non-dispatching Go-DP references. The
`dp_shadow` uses downside PV and remains the conservative fallback comparison.
`dp_evaluation_shadow` uses the exact same base forecast as Python; use this
field for planned-cost and first-action comparisons between engines.

With `optimizer_recourse_shadow: true`, the worker also solves a storage
challenger. `optimizer_challenger_policy: recourse` keeps the two-stage
reference. `multistage` constructs a hierarchical scenario tree from observed
net-power and PV/load history, reduces large ensembles with weighted
net-energy/PV trajectory medoids,
and ties far-horizon actions into move blocks. Decisions become scenario-
specific only after their tree node branches; the first slot is shared by
default. Service-risk CVaR is minimized before expected economic cost. The same
worker process solves champion and
challenger sequentially. A stateful evaluator then advances both policies over
identical realized house load and PV with independent virtual storage energy.
The accumulated raw and terminal-energy-valued cost, SoC, and clamp counts are stored under
`shadow_evaluation`. Flexible EV/thermal contracts pause this challenger until
their counterfactual state is modeled equivalently.
Both policies carry an explicit `policy_version`; changing it or the planner
mode starts a new score run instead of mixing incompatible references.

## Challenger hierarchy

The active champion uses one shared 48-hour asset schedule across all PV
scenarios and adds CVaR risk cost. That is deliberately conservative, but it can
reserve energy twice: once through all-scenario feasibility and again through
CVaR. The storage-recourse experiment remains the optimistic two-stage
reference. The `storage-multistage-v1` shadow is the production candidate: it
uses a hierarchical information tree, 15-minute near-horizon decisions,
30-60-minute move blocks farther out, scenario reduction, and lexicographic
service CVaR. Since MPC replans every 15 minutes, only the common first action
is executable.

Promote a multistage formulation only after rolling-origin replay shows lower realized
cost without more mode, grid-limit, or SoC violations. Tune scenario
probabilities, PV spreads, CVaR weight, and terminal energy price against
forecast residuals from each installation rather than one fixed global value.
The same-input DP evaluation shadow and two-stage recourse lower bound remain
references for those experiments.

The normal continuous battery model is assembled directly as a sparse HiGHS LP.
The CVXPY reference and fallback is DPP-compliant and retained once per
warm-worker topology, so repeated fallback solves reuse canonicalization. For
eligible continuous arbitrage ensembles above the decomposition threshold,
`auto` may use quadratic Progressive Hedging with a reported
non-anticipativity residual. Discrete or otherwise ineligible models are
reduced to the threshold and solved exactly by HiGHS through CVXPY.

The compact extensive form creates charge, discharge, and curtailment actions
once per information node and move block instead of copying them per scenario
and joining the copies with equalities. With ordinary tariffs `auto` is an LP;
negative import prices or export compensation above import cost activate the
corresponding charge/discharge or meter-flow binary guard.

The wider method assessment and promotion gate live in
[optimizer-method-review.md](optimizer-method-review.md).

The direct `highspy` path is parity-tested against the CVXPY model. On an ARM64
Pi, eight stored live snapshots measured 125,072 KiB peak RSS and 0.97 s warm
median, down from 223,096 KiB and 1.14 s for the cached CVXPY extensive form;
its cold model solve fell from 24.8 s to 0.81 s. CVXPY remains loaded because it
also runs the champion and handles exact binary guards, but the large cached
multistage graph is avoided. The worker idle timeout still releases all Python
and solver memory between planning bursts.

## Configuration

```yaml
planner:
  enabled: true
  engine: python                 # default; dp is emergency rollback
  optimizer_solver: HIGHS
  optimizer_formulation: auto    # auto | milp | relaxed
  optimizer_timeout_s: 30
  optimizer_idle_timeout_s: 120  # stop the warm worker after two idle minutes
  optimizer_mip_rel_gap: 0.005
  optimizer_cvar_weight: 0.15
  optimizer_cvar_alpha: 0.90
  optimizer_recourse_shadow: false
  optimizer_recourse_non_anticipative_slots: 1
  optimizer_challenger_policy: multistage
  optimizer_multistage:
    scenario_limit: 12
    branch_interval_slots: 4
    branch_horizon_slots: 48
    max_branching: 2
    near_horizon_slots: 16
    mid_horizon_slots: 96
    mid_block_slots: 2
    far_block_slots: 4
    service_cvar_weight: 1.0
    service_cvar_alpha: 0.95
    economic_cvar_weight: 0
    decomposition_threshold: 20
    decomposition_method: auto
```

`optimizer_command` may point at a different Python executable. It is an
executable path, not a shell command. `optimizer_dir` overrides the module
directory. The corresponding environment overrides are
`FTW_OPTIMIZER_PYTHON` and `FTW_OPTIMIZER_DIR`.

`optimizer_idle_timeout_s` defaults to 120 seconds. The worker stays warm
during reactive planning bursts and exits after that much idle time, releasing
CVXPY and solver memory until the next replan. Set a longer value when cold
Python startup latency matters more than resident memory.

The official container includes the pinned primary solver packages. Native
release archives include the Python package; install it with:

```bash
python3 -m venv optimizer/.venv
optimizer/.venv/bin/pip install ./optimizer
```

Then set `optimizer_command` to `optimizer/.venv/bin/python`, or export
`FTW_OPTIMIZER_PYTHON`.

## Replay

Every successful mathematical plan stores its exact versioned request as
`optimizer_input` in the diagnostic snapshot. Replay it without touching live
dispatch:

```bash
ftw-optimizer-replay diagnostic.json --solver HIGHS
ftw-optimizer-replay diagnostic.json --solver CLARABEL --formulation relaxed
```

The replay output contains the candidate plan and solver metadata. It does not
write state or send commands.

### Historical backtest

`ftw-optimizer-backtest` exports a stratified sample of persisted planner
diagnostics using read-only HTTP GETs, then solves the resulting dataset fully
offline:

```bash
ftw-optimizer-backtest export \
  --api-base http://energy-host:8080 \
  --days 30 --samples 200 --output /tmp/mpc-backtest.jsonl

ftw-optimizer-backtest run \
  --input /tmp/mpc-backtest.jsonl \
  --output /tmp/mpc-backtest-report.json \
  --max-import-w 11040 --max-export-w 11040 \
  --min-arbitrage-spread-ore-kwh 30
```

An optional `--realized-csv` accepts `timeseries_15m.csv` from
`GET /api/research/load/dump`. It reprices each first-slot battery decision
against actual PV, household load, EV/V2X power, and prices, deduplicating
multiple reactive replans in the same bucket.

The forecast-horizon comparison is not realized savings: horizons overlap and
use the forecasts preserved in each diagnostic. The realized first-slot view
is a one-step counterfactual and does not reproduce inner-loop dispatch
feedback. Legacy snapshots with an active loadpoint are skipped because the old
diagnostic schema did not persist the complete vehicle contract. Historical
scenario distributions were also not persisted, so CVaR is disabled for these
replays. These limitations are emitted in every report.

## Development

```bash
make optimizer-install
make optimizer-test
make test
```

`make test` runs Python model tests and the Go-to-worker integration test. The
48-hour scenario test covers 192 slots, three PV scenarios, storage dynamics,
CVaR, and site limits.
