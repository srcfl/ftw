# Site test: FTW against a simulated house

`TestSite` runs the real `ftw` binary against a house from
[srcfl/device-simulator](https://github.com/srcfl/device-simulator): its
headless run (`simulator run`, docs/HEADLESS_RUN.md there) builds a Sungrow
hybrid whose battery follows FTW's EMS writes after 2 s, an SDM630 site
meter behind an IEC gG fuse on each phase, and OCPP 1.6 chargers with the
car on them. FTW talks to it over Modbus and OCPP as on a real site. Each
scenario starts a fresh house and a fresh `state.db`, drives the house and
FTW, and judges the run against fixed criteria from what flowed through the
fuse (the house's record) and what FTW said (its API, once a second). FTW
runs `planner_self` with the Core planner and SE3 prices.

The house lives in device-simulator; this directory keeps only what is
FTW's: the scenarios, FTW's configuration, the criteria and the pinned
simulator commit in [`devsim.version`](devsim.version).

## Run it

```bash
make siterig                          # the short set, two lanes
make siterig-full                     # every scenario
make siterig SITERIG_SET=s1-25,s9-25  # named scenarios
make siterig-coverage CATALOG=../reports/felkatalog/catalog.yaml
make siterig-judge DIR=/tmp/ftw-site-123  # judge a recorded run again
```

| Variable | Meaning |
|---|---|
| `FTW_SITE` | `short`, `full`, or names. Unset skips the test. |
| `FTW_SITE_LANES` | scenarios at once (`SITERIG_LANES`, default 2), each lane on its own 100 ports |
| `FTW_SITE_PORT_BASE` | first port on 127.0.0.1 (default 18700): the house on base+0..59, FTW on base+90 and +91 |
| `FTW_SITE_SHARD` | `k/n`: shard k of n, for several machines |
| `FTW_SITE_OUT` | run directories, `summary.md` and `summary.json` (default: a new temporary directory) |
| `FTW_SITE_DEVSIM_SRC` | a device-simulator checkout at the pinned commit; without it the test clones the pin into the user cache |
| `FTW_SITE_FTW_BIN`, `FTW_SITE_DEVSIM_BIN` | prebuilt binaries |
| `FTW_SITE_CATALOG` | the fault catalog, for `TestCatalogCoverage` |
| `FTW_SITE_JUDGE` | a results directory: `TestJudge` judges its runs again with the criteria as they are now, and rewrites their `result.json` and the summary |

Each run directory keeps `house/plant.jsonl` (the house once a second, with
every event and mark), `house/ocpp-<charger>.jsonl` (every OCPP frame the
charger read or wrote), `ftw.jsonl` (FTW's `/api/status` and
`/api/loadpoints` once a second), `ftw.log`, `house.log`, `config.yaml`,
`house.json` and `result.json`.

A run whose record has a hole over 15 s fails as "host paused": a sleeping
laptop stretches every wait and proves nothing.

## Where it runs

Not in GitHub Actions. `siterig-runner` ([runner/](runner/README.md)) runs
on a machine of its own, watches `master` and open pull requests, and posts
the result on the exact commit as the status `siterig/short` (pull requests
that touch control, loadpoint, OCPP or telemetry code), `siterig/full`
(each new `master` commit) or `siterig/random`. A beta needs
`siterig/full` on its commit.

## Known failures

A criterion named in a scenario's `known` map fails on master for the
finding it names. The run passes while it keeps failing, and fails once it
passes: a fix deletes its line in the same pull request.

## The fault catalog

Each scenario names the entries of FTW's field fault catalog it covers and
its sources (`provenance` in [catalog_test.go](catalog_test.go)).
`make siterig-coverage` lists, per area, which faults have a scenario.

## Scenarios

"Over the limit" is the worst phase above the fuse − 1 A, measured through
the fuse. A device's response time counts as 2 s, the meter and control
tick as 2 s more.

| Scenario | House and steps | Criteria | Short |
|---|---|---|---|
| `s0-25` Idle charger | 25 A, 3 A per phase, a charger with no car, heartbeat 60 s, 300 s | Home use `measured` in 95 % of samples; no OCPP disconnect | yes |
| `s0b-25` | as s0, Core's heartbeat | as s0 | |
| `s1-16`, `s1-25` Charge now | 3 A per phase, Charge now 16 A for 180 s | the room the fuse gives (25 A: 11 040 W) within 60 s; held 95 %; fuse holds; on 25 A no `fuse_limit` | yes |
| `s2-16` Charge now during export | 16 A, 1 A house, 7 kW PV, full battery: 9.1 A export per phase | car starts within 60 s (**known**: export counted as import); fuse holds | |
| `s3-16`, `s3-25` Load step | car and a 3 kW battery hold charging, then 13 A (16 A) or 21 A (25 A) per phase for 180 s | fuse holds; over the limit at most 7 s after the step | yes |
| `s4-25`, `s4b-25` Saved unlock | `surplus_unlock_bat_soc` 0.9 saved, not armed; Charge now 3p, and bare (s4b) | no stops; no offer drops to 0 W; full power over the last minute | |
| `s5-25`, `s5s-25` Surplus only | 25 A, 9.3 kW surplus, `auto`; s5 without steps, s5s with Easee's | no offer above 16 A; fuse holds; only 6–16 A; ≥ 4 140 W 90 % of the time (**known** in both) | |
| `s5t-25` Surplus with a goal | as s5s with a goal, 3.8 kW of export, a charger that switches phases | no offer above 16 A; fuse holds; ≥ 6 A on one phase 90 % of the time | |
| `s6-25` Inverter offline | 1.5 kW PV, the inverter silent for 300 s | Home use shown and marked estimated throughout; measured before and after; never estimated while every driver reads | |
| `s7-25` Restart | Charge now, 15 min, Core down 20 s, 420 s after | the day's Home use is kept; Home use back within 30 s; Charge now never below 4 140 W; full power within 420 s | |
| `s7s-25` Short restart | as s7, restarted after 60 s | as s7 without the day total, which needs the long run (10 % rule) | yes |
| `s8-60-25`, `s8-120-25` Slow charger | 15 A per phase, battery at its 5 % floor, profiles applied 60 or 120 s late | fuse holds; over the limit at most the delay + 7 s | 60 s |
| `s9-25` 3p to 1p | 10 A per phase, a 32 A charger that switches phases; Charge now 7 360 W on 3p, after 90 s on 1p | fuse holds; over the limit at most 3 s before and after the switch | |
| `s9r-25` 1p to 3p beside a loaded L2 | house 2/20/2 A, the 32 A charger; Charge now 7 360 W on 1p, after 90 s on 3p | as s9 | |
| `s10-25` Two chargers | `carport` 60 s late; cut 6 A and raise 16 A together | fuse holds; over the limit at most 3 s after the step | |
| `s11-25` Pause, then restart | a charger applying 5 s late; Charge now 16 A, 0 W for 25 s, then 6 A | an offer within 60 s of the restart; over the limit at most 3 s; fuse holds | |
| `s12-20` One-phase night | 3 × 20 A, house 1.6/1.7/2.3 A, a charger applying 40 s late, the field's step table on `auto`, a plan for 6.6 kWh by the first quarter hour 40 min out | fuse holds; planned charging never orders one phase; no order cut while every phase has room, unless the plan paused; 90 % of the plan by the ready time | |
| `s13-25` Socket reconnect | Charge now 16 A, the charger's connection gone for 10 s | Charge now goes on after the charger returns (**known**: #1788); no unplug seen | |
| `s15-25` One-phase charger, Charge now | 25 A, 3 A per phase, an Easee wired on L1 (6-16 A, steps to 3 680 W, no phase mode); Charge now on the phases `/api/loadpoints` reports, as the app sends it, 180 s | 16 A on L1 within 60 s; held 95 %; fuse holds; no `fuse_limit` | yes |
| `s15b-25`, `s15c-25` | as s15, phase mode `1p` and `auto` | as s15 | |
| `s15bat-16` One-phase charger, battery rising | 16 A, 3 A per phase, the s15 charger, battery at 50 % following its EMS 8 s late; PV 2.1 kW rising every 30 s (+2.5 kW, then 1.5 kW) so Core charges the battery in steps; Charge now 6 s after the second step, while the raise is on its way | the battery order rose over 1 150 W in the 20 s before (setup check); car charging within 10 s; fuse holds; over the limit at most 3 s | |
| `s15p-25` One-phase charger, planned | as s15, a plan for 600 Wh by the first quarter hour 15 min out | fuse holds; 90 % of the plan by the ready time; never called not following | |
| `s14-25` One-phase car | an 11 kW charger with a one-phase car, a plan it cannot meet, 20 min | 3.4–3.8 kW over the last 10 min; never called not following | |
