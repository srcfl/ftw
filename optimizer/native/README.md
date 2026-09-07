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

Energyplan uses the same downside PV forecast as Core. The worker gets a 500 ms
solve budget and a 2 s transport timeout. After Core validates and publishes a
plan, one Core DP shadow runs with a 10 s limit. Its result appears in
`dp_shadow`, tied to the same decision ID. It cannot change the active actions.
Both plans use Core's grid cost model, with a separate terminal-energy-adjusted
comparison. A failed comparison reports `rejected`, without a cost verdict.

Energyplan plans from the measured battery energy, including starts below the
reserve or above the charge limit. Each action must hold or reduce any existing
violation; after recovery the plan must stay within the configured limits.
Core independently checks that recovery and validates fallback plans too.
The compiled worker updates with Core.

Supported requests contain one battery and at most one EV per site, with the
four existing modes, physical limits, negative tariffs and an EV deadline.
Unsupported scenarios, thermal/commercial models and multiple assets return
an error. A time limit can return a feasible plan with a remaining cost gap;
without a feasible candidate it returns a budget error. Core handles errors
through its existing fallback path.

`make verify` includes the binary and integration checks. Go integration tests
can also use an absolute path supplied in `FTW_NATIVE_SOLVER`. Ordinary Go tests
skip these optional process tests when that variable is unset.

## Forecast candidates and evaluation

Energyplan 0.2.0 also accepts forecasting protocol v1. The public request and
response schemas are in `bundle/forecast-v1.schema.json` and
`bundle/forecast-v1.response.schema.json`. Core runs a separate local process
with a 2 s deadline for each model update and prediction. Forecast failure
does not replace the active planner inputs. These PV and load models gather
evidence as candidates; Core does not promote them automatically.

PV requires the site's location and qualified PV measurements, but no panel
angles or rated power. Core saves each issued horizon with its available
weather, occupancy, model state and site binding in a bounded local archive.
Measured errors calibrate forecast intervals by horizon. The worker's own
provisional ranges remain distinct from calibrated intervals.

To score an archive, run this from `go/`, preferably against a box backup:

```sh
go run ./cmd/ftw-forecast-evaluate -state /path/to/state.db
```

The command opens SQLite read-only and writes JSON. Optional `-since` and
`-until` take RFC3339 timestamps within a 30-day window. The report compares
issued forecasts on matched outcomes, includes daylight PV errors and net
energy errors over 1/3/6/12/24 hours, and separates measured interval coverage
from cold-start and provisional ranges. Missing history cannot yield an
accuracy or savings claim. This command is a source tool; release archives
do not include a separate evaluation executable.

`make native-solver-test` exercises both protocols and the Go forecast adapter
against the bundled host worker. Direct Go forecast tests use
`FTW_FORECAST_WORKER=/absolute/path/to/ftw-solver`.

## Update the bundle

Build and test a new version in the private repository. Copy only the complete
output of its binary packaging tool into `bundle/`, then run
`make native-solver-test` and `make verify`. Submit the binaries, manifest,
license and notices together. Never add Rust source, Cargo files, source
archives or build tools to this public directory.
