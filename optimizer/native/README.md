# Energyplan compiled worker

This directory contains the optional proprietary Sourceful Energyplan worker,
its license and third-party notices, and public integration checks. Rust source,
source tests and builds live in the private `srcfl/energyplan` repository.

The executables have a separate license in `bundle/LICENSE.txt`. It permits use
and redistribution of the unmodified workers with FTW, including commercial
FTW distributions. FTW's own source keeps its existing license. Other uses of
the worker require a separate license from Sourceful.

## Verify and run

No Rust toolchain, Python solver package, private repository access or network
service is needed. From the FTW repository root:

```sh
make native-solver-test
python3 optimizer/native/verify.py --host-binary
optimizer/native/bundle/ftw-solver-linux-arm64 --time-limit=100ms < requests.jsonl
```

The bundle contains static Linux ARM64 and Linux AMD64 workers and a macOS
ARM64 worker. The manifest pins their version, private source commit, sizes
and SHA-256 checksums. The verifier checks every bundled file, the host worker
handshake and the public source boundary. Keep the license and notices with
any copied or redistributed executable. Run only a verified bundle.

The worker reads optimizer protocol v1 JSON lines and stays alive across
requests. Core's existing `mpc.ExternalOptimizer` starts it through an absolute
path in `ExternalOptimizerConfig.Command`. Core validates all proposed plans
and keeps its Go fallback. Beta releases select Energyplan when `planner.engine`
is unset on a supported host. Set `planner.engine: energyplan` to select it
explicitly, or `core` to select Core DP. Stable and development
builds keep Core as the unset default; Windows has no bundled worker.

Energyplan uses the same downside PV forecast as Core. Small requests get a
500 ms solve budget; larger fleets and PV-control requests get 5 s.
Core applies PV uncertainty once when forming the downside horizon. This does
not add worker scenarios or extend the solve budget by itself.
The transport timeout is 7 s. After Core validates and publishes a plan, one
Core DP shadow runs with a 10 s limit when Core DP can represent the site.
Its result appears in
`dp_shadow`, tied to the same decision ID. It cannot change the active actions.
Both plans use Core's grid cost model, with a separate terminal-energy-adjusted
comparison. A failed comparison reports `rejected`, without a cost verdict.

Energyplan plans from the measured battery energy, including starts below the
reserve or above the charge limit. Each action must hold or reduce any existing
violation; after recovery the plan must stay within the configured limits.
Core independently checks that recovery and validates fallback plans too.
The compiled worker updates with Core.

Supported requests can contain zero, one or several batteries and EVs, with
each device's own physical limits and EV deadline. The worker supports the
four existing modes, negative tariffs and shared scenarios with CVaR. Request
limits are 512 slots, 64 total devices and 32 scenarios; bounded planning may
stop earlier. Thermal, commercial and recourse inputs return explicit errors.
A time limit can return a feasible plan with a remaining cost gap. An unknown
bound is null; without a feasible candidate the worker returns a budget error.
Core DP fallback cannot represent every fleet. In that case Core keeps the
previous plan for diagnosis and withholds execution until a new plan succeeds.
Selecting Core DP, including the stable/development default, requires positive
home-battery capacity. Energyplan permits sites without home storage.

Core only permits a planned PV generation cap when it verifies the loaded
driver and current telemetry for the site's complete PV control domain. A
restored diagnostic containing physical device maps or PV control stays an
archive until a new plan validates current inputs. It does not restore device
budgets or PV permission from saved JSON alone.

`make verify` includes the binary and integration checks. Go integration tests
can also use an absolute path supplied in `FTW_NATIVE_SOLVER`. Ordinary Go tests
skip these optional process tests when that variable is unset.

## Forecast primary, fallback and evaluation

A verified worker that advertises forecasting protocol v1 supplies the primary
PV and household-load forecast. The public request and response schemas are in
`bundle/forecast-v1.schema.json` and `bundle/forecast-v1.response.schema.json`.
Core calls a separate local worker with a 2 s deadline for each model update
and prediction. Prediction runs during replanning, outside the control and
dispatch locks.

One replan freezes the legacy forecast, weather, occupancy and model state
before it calls the worker. The returned PV and load values must match that
capture and cover the planner interval. Core selects each signal separately:
a valid Energyplan PV value can run with legacy load, or a valid Energyplan load
value can run with legacy PV. A missing, late, partial or invalid value falls
back to the matching legacy signal for that slot. The `champion` archive series
records the values used by the planner and names each signal's source. The
`legacy_shadow` series records the unchanged legacy forecast from the same
capture. Forecast work can change planner inputs, but it never sends a hardware
command; Core still validates the resulting plan before dispatch.

PV learning requires the site's location and qualified PV measurements, but no
panel angles or rated power. Core forms qualified PV and household-load labels
from complete 15-minute measurements. It excludes missing or unsafe evidence,
including PV intervals affected by commanded curtailment. Model updates run
outside dispatch and do not alter an issued forecast.

The latest Energyplan forecast state is stored locally in SQLite under
`forecast/energyplan_state_v1`. Core saves the complete update atomically before
it exposes the new state to planning, then loads it on restart. A change to the
model input configuration or stable hardware binding starts a new learning
revision and an empty model. A compatible Core or worker program upgrade keeps
the learning revision and can reuse the saved state. The issued-forecast
revision still records the exact Core version, worker bytes and pipeline policy,
so evaluation does not join results from different program builds.

Core saves each issued horizon with the same frozen weather, occupancy, model
state and site binding in a bounded 30-day local archive. Large model snapshots
are stored once by content hash and referenced by each issue. Row counts,
expanded sizes and compressed storage all have hard limits. Measured errors
calibrate forecast intervals by horizon and interval length. The worker's own
provisional ranges remain distinct from calibrated intervals.

To score an archive, run this from `go/`, preferably against a box backup:

```sh
go run ./cmd/ftw-forecast-evaluate -state /path/to/state.db
```

The command opens SQLite read-only and writes JSON. Optional `-since` and
`-until` take RFC3339 timestamps within a 30-day window. By default it compares
`champion` with `legacy_shadow` only where both came from the same frozen issue
and have the same truth. The report includes per-lead PV, load and net errors,
daylight PV errors, net energy errors over 1/3/6/12/24 hours, measured interval
coverage, and separate cold-start and provisional ranges. Missing, late or
censored truth cannot yield an accuracy or savings claim. This command is a
source tool; release archives do not include a separate evaluation executable.

`make native-solver-test` exercises both protocols and the Go forecast adapter
against the bundled host worker. Direct Go forecast tests use
`FTW_FORECAST_WORKER=/absolute/path/to/ftw-solver`.

## Update the bundle

Build and test a new version in the private repository. Copy only the complete
output of its binary packaging tool into `bundle/`. Before changing the pin,
verify that every target has the same declared worker version and forecast
protocol, and that the manifest pins the source commit, size and SHA-256 of each
binary. The integration checks must cover a partly elapsed first forecast
interval and a fresh model with no saved state. Then run
`make native-solver-test` and `make verify`. Submit the binaries, manifest,
license and notices together. Never add Rust source, Cargo files, source
archives or build tools to this public directory.
