# Changelog

## 1.11.1

### Patch Changes

- fb9c05a: Show Home Assistant as enabled but disconnected when its MQTT bridge cannot start, and mark unsaved enable changes in Settings.

## 1.11.0

### Minor Changes

- 912c392: Add crash-safe local adoption and startup checks for the software Home Link identity.

### Patch Changes

- 6009831: Skip full history backups when the release uses the same database schema, reduce Core image downloads, and show live backup, download, restart, and health-check progress.
- 2a036d7: Stop stale driver readings from adding to dashboard energy history, and let Modbus drivers reconnect when a device comes online after Core starts.
- 2a036d7: Use the optimizer sidecar without a hidden Python process in Compose, keep its first failure visible when a development fallback also fails, and use the 30-second Core timeout throughout the backend.

## 1.10.5

### Patch Changes

- c563cb5: Add a Settings control that disables duplicate battery readings from multi-resource gateways such as Sourceful Zap.

## 1.10.4

### Patch Changes

- d518600: Show only installed drivers with a different signed stable or beta version in Update Center.

## 1.10.3

### Patch Changes

- 8f26151: Add a GoodWe register-profile picker to the Devices UI so operators can install and configure the GW8KN-ET/HK3000 beta without editing YAML.

## 1.10.2

### Patch Changes

- 94f8ac5: Show only configured drivers in Update Center, let operators try a signed beta for one driver without updating Core, offer stable updates only for newer versions, and move driver support status out of the Devices picker.

## 1.10.1

### Patch Changes

- afd9a81: Add the internal read-only Home Link identity, grant, relay-auth, and energy-ledger contracts.
- b4150d9: Reject unsafe Nova key files and persist first-boot identity atomically.
  Key storage now fails closed without Unix owner and link metadata, hard links,
  no-follow opens, file sync, and directory sync. It creates only the final key
  directory and requires its trusted parent to exist.

## 1.10.0

### Minor Changes

- 3985c82: Use `srcfl/device-drivers` as FTW's default signed driver source, bind its
  read-only manifest policy at runtime, and add serial and AES-GCM host support
  for public P1 drivers.
- 29731f1: Add a read-only Core storage check for SQLite page use and data-volume headroom.

### Patch Changes

- 7e85444: Start the independent Optimizer release line at 1.3.2 so existing 1.3.1 installations see component releases as upgrades.
- 9f98463: Sync the bundled ESPHome DSMR recovery driver with the signed 1.0.2 community asset so name-derived and delivered/returned object IDs keep per-phase meter data available offline.
- 3985c82: Keep Modbus drivers offline when a poll has a failed register read instead of accepting zero-filled telemetry as fresh data, and recover mute TCP sessions with a non-blocking reconnect cooldown.

## 1.9.1

### Patch Changes

- 3352ab0: Block in-app Core updates until the Compose optimizer sidecar is present and healthy. Legacy operator override files remain unchanged and the update error points to the migration guide.
- 3352ab0: Report the mathematical optimizer as degraded when its worker cannot answer a compatible handshake or the active plan uses the Go fallback, and show the fallback reason in the dashboard and system status.
- 3352ab0: Preserve planned PV export when live surplus replaces a later grid charge, honor the configured arbitrage spread, and limit early charging to the later grid-funded energy it can replace.

## 1.9.0

### Minor Changes

- 5d3ec43: Enforce signed read-only driver permissions in the host and add a safe public driver feedback flow.

  Local unsigned drivers remain offline-first and take priority over managed and bundled files. FTW now marks that source in inventory, keeps managed cache state unchanged, and documents the exact test, reload and rollback steps.

## 1.8.1

### Patch Changes

- ce90d5d: Pixii: exclude batteries from dispatch and MPC while SunSpec ChaSt reports `testing` (calibration), using the same device-fault path as Ferroamp Fault Mode. Telemetry and site-meter data stay live.

## 1.8.0

### Minor Changes

- cc7de6d: Add `observe_only` on battery drivers: keep structured telemetry and dashboard visibility while excluding the driver from dispatch, MPC, and watchdog commands. For sites where a retailer VPP owns battery actuation.

### Patch Changes

- bb2d644: Fix the battery self-tune "Run again" button so it reuses the batteries from the completed calibration when the results view no longer shows checkboxes.

## 1.7.0

### Minor Changes

- a49f248: Add the signed Device Support control v2 host with exact site pins, short leases, default-mode recovery, a restricted Lua API, Modbus-only permissions, and local command-result records. Existing bundled, local, and read-only v1 drivers keep their current runtime.

## 1.6.0

### Minor Changes

- eb92509: Report a bounded driver inventory to Nova when federation is enabled. Reports include loaded code hashes and package provenance, but no site config, device IDs, endpoints or credentials.

## 1.5.0

### Minor Changes

- 36ac4f6: Add a per-loadpoint Battery boost lease with a four-hour maximum, explicit home-battery reserve, optional EV target and departure, restart-safe persistence, API controls, and visible stop reasons in the Loadpoints UI.

  Battery boost automatically stops on expiry, reserve, unplug, operator holds, surplus-only policy, stale or unavailable meter/drivers, incompatible core modes, and fuse safety. The legacy site-wide `battery_covers_ev` control remains available for compatibility.

- adb70f2: Consume signed canonical Device Support driver indexes and FTW target packages while preserving explicit Core activation, offline recovery and automatic rollback. The first package gate is read-only; control packages remain blocked pending safety-lifecycle HIL acceptance.
- f7d2da9: Give the dashboard five focused Overview, Energy, Plan, History and More destinations, with a mobile bottom navigation and matching desktop tabs. Add a plain-language active-plan briefing for next actions, fallback or stale operation, forecast uncertainty, expected battery charge and active safety adjustments.
- 22ab07c: Add a versioned energy history ledger keyed by stable asset identity, with separate grid and battery directions, hardware-counter preference, marked integration fallback, data-quality provenance, bounded system/asset APIs, CSV export, and a read-only History view.

  Existing hot/warm/cold history and Parquet roll-off remain unchanged. XLSX export is deferred to a later phase to avoid adding a heavyweight runtime dependency.

  Five-minute ledger detail is retained for 30 days, then atomically rolled into honest hourly buckets and bounded to the two-year API horizon.

### Patch Changes

- bdc47c8: Publish a gated NOW/NEXT/LATER roadmap for the current P0 work, the optional outbound-only FTW Home Link, and evidence required before later product directions can advance.
- 630cc69: Consolidate the FTW logo into matching SVG, PNG, JPEG, and favicon assets, and use the scalable or square format appropriate for the dashboard, setup wizard, README, and Raspberry Pi Imager.
- 5d902a3: Stop EV charging and storage dispatch when the site meter or required per-phase fuse telemetry is stale, while preserving schedules and persistent manual holds for automatic recovery.
- 1d1e852: Harden the LAN/API trust boundary in setup and normal operation: remove wildcard CORS, block browser cross-site mutations and active reads, require JSON content types, and require an opt-in Bearer token for protected requests addressed through public or fully qualified hostnames. Existing loopback and private-LAN UI/API clients remain compatible, with documented remote-access onboarding and local recovery.
- eb6979d: Remove the dormant settlement-aware self-consumption path that could offset earlier import by exporting battery energy later in the same 15-minute window. Quarter-hour import and export remain separate observational totals and no longer have a control path that can net one against the other.

## 1.4.0

### Minor Changes

- 61a15f2: Add verified portable full backups with safe automatic-revert restore, make
  pre-update rollback points mandatory, and replace the updater dialog with an
  Update Center that versions Core, Optimizer, and signed Drivers independently.

  Legacy Compose migration now creates and verifies a full backup before any
  deployment change, health-gates the paired Core/updater phase, treats Optimizer
  as an optional independent phase, and refreshes Driver metadata without
  activating code.

## 1.3.1

### Patch Changes

- b04a4dd: Make update rollback backups complete and compressed, preserve the FTW file owner during restore, clear stale SQLite WAL files, health-check the restored service, and automatically recover the pre-rollback state if the selected backup cannot boot. Incomplete legacy snapshots are no longer restorable because they omitted history.

  Publish Raspberry Pi Imager metadata on stable version releases as well as the permanent installer channel, and harden the legacy Compose migration so it preserves custom project identities and updates or restores an existing optimizer container.

## 1.3.0

### Minor Changes

- ca37bef: Make the official core container a small static runtime and keep Python/CVXPY exclusively in the independently updatable optimizer image.

## 1.2.0

### Minor Changes

- 61c5250: Add Sourceful's keyless European spot-price API and make it the default provider for new FTW setups.
- 61c5250: Promote Sourceful Zap to a first-class production integration with official local-API identity, meter, PV, read-only battery and full firmware-schema V2X telemetry, hot device discovery, a safe local-control activation contract, setup guidance, and explicit Zap-anchored or standalone Sourceful Energy onboarding.

### Patch Changes

- 61c5250: Fix Easee auto phase switching around the 1-phase/3-phase minimum-current gap, and let cheap/arbitrage dispatch absorb unexpected live PV when it economically displaces a later grid-funded battery charge.
- 61c5250: Retire the unused relay, pairing, embedded OCPP and legacy-dashboard paths; reduce project documentation and make beta-to-stable promotion explicit for core and drivers.
- 61c5250: Fix Deye driver initialization by declaring its default SoC limits before they are read.

## 1.1.0

### Minor Changes

- 644d784: Add feature-negotiated commercial reserve, backup, robust forecast, degradation, and demand-charge constraints to optimizer protocol v1 for shared Sourceful Local EMS use.

## 1.0.0

### Major Changes

- 2801fca: Remove the retired Forty Two Watts relay, TURN/WebRTC owner portal, local
  passkey sessions, `ftw-pair` support tunnel, anonymous relay fleet reporter,
  and their API, UI, configuration, deployment and release surfaces. The FTW
  dashboard is now explicitly LAN-local; owner-operated VPN access remains a
  community path, while the planned managed remote experience is the optional
  Sourceful Energy app through outbound-only Novacore integration.

## 0.130.4

### Patch Changes

- ed59d81: Answer health probes during slow boots.

  The API port is now bound before the state DB opens, serving
  `/api/health` with 200 `{"status":"starting"}` (and 503 elsewhere) until
  the real mux takes over on the same listener. Previously a boot that ran
  a one-time VACUUM or a full integrity check on a multi-GB database left
  the port unbound for up to tens of minutes, so the Docker healthcheck
  failed and the self-update sidecar judged the deploy failed and rolled
  back in the middle of the compaction.

## 0.130.3

### Patch Changes

- 00a9da5: Yield between maintenance chunks so live writers are never starved.

  v0.130.2 bounded every maintenance transaction, but back-to-back chunks
  still starved the control loop during a large backlog migration: SQLite's
  busy handler retries without fairness, so the maintenance loop re-acquired
  the write lock before any waiting writer won its retry. Prune chunks,
  rolloff deletes, and diagnostics batch-deletes now pause (250/100 ms)
  between transactions, guaranteeing every waiting writer a window.

## 0.130.2

### Patch Changes

- 1b62749: Critical fix: bounded maintenance transactions (upgrade-lock incident).

  The first history prune after upgrading a long-running install (v0.129.0's
  Prune wiring) could hold SQLite's write lock for hours: the hot→warm
  aggregation used a correlated subquery that re-scanned the hot table once
  per 15-minute bucket, and the whole backlog ran as one transaction. Every
  control-loop write failed with SQLITE_BUSY until it finished.

  - Prune is now linear (single-pass bucket aggregation) and chunked: each
    transaction ages at most ~24 h of rows, so writers interleave. A 93-day
    backlog that previously locked the DB for 4+ hours now completes in
    seconds. Chunk and cutoff boundaries are bucket-aligned, which also fixes
    a pre-existing partial-bucket overwrite at the retention edge.
  - Prune logs its result (rows aged, chunks, duration) — maintenance is no
    longer silent.
  - Parquet rolloff deletes each day's rows right after that day's file is
    durable, in hour-sized transactions, instead of one giant end-of-run
    DELETE that could lose the race against live writers.
  - Planner-diagnostics retention in SQLite reduced 30 → 7 days (measured
    485 MB at 30 days on a real site; Parquet keeps everything), deletes are
    batched, and the table is excluded from state snapshots (snapshots shrink
    from ~470 MB to ~20 MB).

## 0.130.1

### Patch Changes

- d08c4cc: Automatically add and validate the independently updatable optimizer sidecar when upgrading safe existing Linux or macOS Compose installations, with rollback cleanup and fail-closed handling for operator-owned overrides.

## 0.130.0

### Minor Changes

- eba5722: Add independently updatable optimizer and Lua-driver modules with protocol negotiation, signed monorepo driver releases, content-addressed activation, selective restart, health verification, and rollback while retaining bundled and Go-DP safety fallbacks. Add explicit opt-in anonymous fleet statistics for active installations and public driver/component health.

### Patch Changes

- a4f0b33: Repair the setup wizard and pair-session component after the FTW rename, and add CI guards for browser JavaScript, release metadata, and stale Changeset package names.

## 0.129.0

### Minor Changes

- 8d61001: Add Sungrow PV curtail support, reset stale inverter control state when the driver loads, and surface hardware fault states such as thermal shutdowns in diagnostics.
- af005b9: Database hardening and richer history access.

  **Durability & SD-card resilience**

  - Parquet rolloff now fsyncs day files (and their directory) before the atomic rename — a power cut can no longer lose a day of rolled-off samples whose SQLite rows were already deleted.
  - The rolloff streams one UTC day at a time instead of buffering the whole backlog in memory (a multi-week backlog could OOM a Pi and then never complete).
  - Planner-diagnostics day files are merged instead of overwritten — the hourly rolloff previously discarded every earlier hour of the same day from cold storage.
  - One-time `VACUUM` at boot when a large share of state.db is freelist (reclaims the high-water mark left by big prunes), guarded by a free-disk check.
  - Truncating WAL checkpoint after every hourly rolloff; each control-loop tick (history + metrics) is now persisted in a single transaction, halving the WAL commit rate.

  **Faster history queries**

  - Downsampled `/api/history` and `/api/series` queries aggregate per bucket in SQL instead of fetching every raw row into Go (a month view used to materialize >1M rows per request), and buckets carry a min/max envelope so short power spikes stay visible zoomed out.

  **Richer history over REST**

  - `/api/series` windows older than 14 days transparently include the cold Parquet tier.
  - `/api/series` supports comma-separated multi-metric queries, absolute `since`/`until` bounds, and `format=csv` export.
  - `/api/series/catalog` reports each metric's display unit; units from `host.emit_metric` are now persisted across restarts.

  **Growth control**

  - New `state.cold_retention_days` config bounds the cold Parquet tier (default 0 = keep everything).
  - Low-disk warning (log + event feed) when free space drops below 500 MB.

### Patch Changes

- af005b9: Run the wide-history retention tiers during the hourly storage rolloff so old full-resolution dashboard rows are aggregated instead of growing `state.db` indefinitely.
- a4c695a: Decouple the Raspberry Pi installer image from application releases and reduce redundant CI work while retaining required validation.
- 3e1b98a: Fail closed for direct state-changing `p2pFetch` API calls on public relay origins so owner ceremony and control request bodies are never raw-fetched to the relay.

  Enable short TCP keepalive on Modbus TCP driver sockets to make stale inverter sessions recover faster after network/controller interruptions.

  Use MyUplink's known-working OAuth scope set by default and expose an override for installations that can use narrower read-only auth.

  Let `BatteryCoversEV` take effect in passive-arbitrage PV-surplus charge slots by allowing the planned-grid cap to back off charging and cover EV import while preserving deliberate grid-charge slots.

  Resolve failed remote-home P2P attempts to the actionable sign-in/setup gate instead of leaving the page on "Reaching your home..." forever.

## 0.128.1

### Patch Changes

- dcffc61: Add a one-command, rollback-safe migration from legacy Docker Compose and
  local-development images to the official Sourceful images. Include license
  and notice files in Docker build contexts so canonical main and updater images
  publish successfully.
- 43e2d1b: Repair self-updates from legacy or developer Compose layouts by applying a
  temporary canonical image override without rewriting the host deployment, and
  preserve automatic image and state rollback on that path. Add a bilingual
  legacy-upgrade runbook and refresh migration/operator documentation,
  fresh-clone simulator defaults, driver catalog coverage, release-image
  selection, and license payloads after the Sourceful transfer.

## 0.128.0

### Minor Changes

- 49fe04f: Replace the discretized Go MPC as the primary planner with a CVXPY mathematical optimizer using HiGHS for LP/MILP, CLARABEL for continuous convex fallback, scenario/CVaR forecast risk, independently constrained battery fleets, recoverable out-of-band SoC states, multiple jointly optimized EV loadpoints, Go-side trajectory validation, deterministic and historical replay tooling, an always-on diagnostic DP shadow, and automatic Go-DP emergency fallback.
- b10acac: Add an opt-in stochastic storage-recourse shadow planner and stateful champion/challenger evaluation without allowing the challenger to affect dispatch.
- c032cfa: Add an opt-in multistage stochastic optimizer shadow with hierarchical scenario trees, scenario reduction, move blocking, service-risk CVaR, a direct sparse HiGHS path, DPP CVXPY fallback, and guarded Progressive Hedging for large continuous ensembles.
- 1f0f7b4: Rename the project to FTW under the `srcfl` GitHub and GHCR namespaces, add
  canonical `ftw` binaries and fresh-install defaults, retain compatibility
  aliases for existing installations, and update active product documentation
  and branding for Sourceful stewardship.
- 49fe04f: Add persistent stable, beta, and edge update channels with immutable GHCR targets, snapshot-protected channel updates, a dashboard channel selector, automatic edge images from master, and an explicit beta prerelease workflow.

### Patch Changes

- 353c26d: Harden the Python optimizer worker with idle memory release, robust solver tolerances, same-forecast DP evaluation diagnostics, and safe repeated OCPP shutdown.

## 0.127.0

### Minor Changes

- 3cd15fe: Calendar-based planner constraints via a built-in CalDAV calendar (#498). 42W
  hosts its own in-process, LAN-only CalDAV server (pure-Go, `emersion/go-webdav`,
  MIT; objects persist in `state.db`), so no extra container is needed — it even
  works as a single-container Home Assistant add-on. Recurring events are expanded
  server-side. 42W reads a calendar you keep in your normal calendar app and turns
  events into planner intents:

  - An **Away** / **Vacation** event switches the load model to its away profile
    for that interval, so the planner conserves battery while the house is empty.
  - A **"Charge car 80%"** event (with your departure as the event time) sets the
    matching loadpoint's target SoC + deadline, which the MPC already honours.

  42W also **writes** read-only calendars you can subscribe to:

  - an EVSE usage history ("EV charged 12.3 kWh", one event per charge session);
  - the planner's forward-looking plan — upcoming battery charge/discharge
    windows — reconciled each cycle so it stays current without piling up.

  Both read-only calendars can be subscribed to in one tap via a `webcal://`
  link (a read-only `.ics` feed served on the CalDAV port).

  The feature is opt-in (`caldav.enabled`) and fail-soft — a calendar problem
  never blocks control — and stays entirely on your local network. Configure it
  under Settings → Calendar (mobile-friendly), which auto-manages the credential
  and shows it (with a QR) to add to a phone or desktop calendar app.

- 886a426: **The system now understands when a device is reachable but faulted, and stops
  relying on it.** Previously a driver that kept emitting telemetry was treated as
  healthy even if the device had gone into a fault state it couldn't act on — so
  the dispatcher kept commanding it and the MPC kept planning against it. A
  Ferroamp EnergyHub in _Fault Mode_ (relays open, can't charge/discharge) would
  silently turn the battery's commanded share into **grid import**, while a healthy
  second battery sat under-used.

  - **New host capability `host.set_device_fault(faulted, reason)`** lets a driver
    flag a device-level fault. It's orthogonal to the watchdog (which only catches
    "stopped emitting") — the driver keeps emitting, but `IsOnline()` returns false.
  - **Dispatch + MPC exclude a faulted driver** automatically (both already gate on
    `IsOnline()`), so the load is covered by the healthy battery instead of
    imported, and the plan stops counting capacity that isn't there.
  - **`/api/status`** shows the driver as `"fault"` with `device_fault_reason`;
    `/api/health` reports `drivers_faulted`; the transition is logged to `/api/logs`.
  - **The Ferroamp driver** now detects EnergyHub Fault Mode (`ehub.state` bit 15)
    and flags/clears the fault automatically.

- 8733f0f: feat(drivers): driver-level device fingerprinting + scan auto-detect

  Drivers can now identify whether an arbitrary endpoint is one of their own
  devices via a new optional Lua lifecycle hook, `driver_fingerprint()`. It
  returns a deliberate tri-state — `true` (positive signature match), `false`
  (talked to it, it's _not_ mine), or `nil` (inconclusive / not supported) —
  plus an optional `{make, model, serial, confidence}` identity hint. The
  probe is passive: `driver_init`/`driver_cleanup` are never run, so a
  fingerprint can't reconfigure the device (no Sungrow power-limit writes, no
  SolarEdge curtail-register clears).

  New `POST /api/drivers/fingerprint {host, port, protocol?, unit_id?}` takes
  an open endpoint discovered by a network scan (e.g. port 502 or 80), runs
  every catalog driver that speaks that protocol against it, and returns the
  ranked matches (plus every candidate's verdict for transparency).
  `GET /api/scan?fingerprint=1` folds this into discovery: each open Modbus
  or HTTP host comes back annotated with the drivers that recognise it —
  turning "port 502 is open on 10.0.0.7" into "that's a SolarEdge". The
  default `/api/scan` response is unchanged.

  Both Modbus (port 502) and HTTP (port 80) are fingerprintable. The hook
  receives a `target` table (`host`, `port`, `protocol`, `base_url`) so HTTP
  drivers can build their probe URL.

  Ships signatures for four drivers:

  - **SolarEdge** — SunSpec `"SunS"` marker + common-block manufacturer
    string on input registers, with serial extraction.
  - **Pixii** — same SunSpec common block but on holding registers
    (manufacturer "Pixii"), with serial extraction.
  - **Sungrow** — SH-hybrid device-type code.
  - **Zap** (Sourceful) — HTTP `GET /api/devices` device-list signature,
    latching the P1 serial as identity.

  Drivers without a `driver_fingerprint` hook simply report `unknown` and are
  never false-positives.

- 07a5ae5: Add a read-only ESPHome DSMR/P1 meter driver and hot-reload site-meter changes into control, MPC, and load-model services without restarting.

  The driver uses site-sign power, retries serial discovery for stable hardware identity, backs off after failures, and omits optional phase or lifetime-counter fields when their reads fail rather than publishing unsafe zeroes.

- d43ad0d: Expand the Home Assistant MQTT bridge with reliable availability, driver health, live diagnostics, plans, prices, forecasts, phase data, EV/vehicle data, and daily energy totals.

  - **Availability topic (LWT)**: HA entities now show as "Unavailable" when the EMS goes offline. The paho Last Will Testament publishes `offline` to `forty-two-watts/status` on unclean disconnect; a clean shutdown publishes it synchronously before disconnect.
  - **Per-driver binary_sensor**: One `<driver>_online` entity per driver so HA dashboards can alert on a stale driver (e.g. stuck Modbus device).
  - **`peak_limit_w` and `ev_charging_w`**: Both control setpoints are now published as `number` entities and writable from HA.
  - **`emit_metric` sensors**: Any scalar diagnostic emitted by a Lua driver is automatically discovered with its explicit unit preserved and a device class inferred when possible.
  - **MPC plan sensor**: A `plan_action` text sensor shows the current slot's action (charge / discharge / idle), with JSON attributes containing `battery_w`, `grid_w`, `soc_pct`, slot start/end times, and a full 24 h schedule array — so you can see what the EMS plans to do throughout the day directly in HA.
  - **Richer energy data**: Publish current price and forecasts, per-phase meter values, EV and vehicle telemetry, and daily import/export/PV/battery/load energy totals.

- 1789105: Add an experimental MQTT driver for Panasonic Aquarea heat pumps connected through HeishaMon, including live temperature metrics and safe heat-curve offset control.
- 2457429: Improve setup discovery with driver identification, reverse DNS and mDNS labels, small private routed-network scans, and common Modbus proxy ports 503 and 1502.

  The setup wizard shows the highest-confidence driver match and preselects it in the normal configuration form. Routed subnets retain their actual mask so a narrow route is never expanded into a broader scan.

- 5a05d6b: Drive the dashboard mode buttons from a server-side catalog. New
  `GET /api/modes` returns every selectable mode with its label, tooltip, and
  tier (primary / advanced / hidden), derived from `control.ModeCatalog`. The
  web dashboard now builds its Strategy buttons from that endpoint instead of a
  hand-maintained HTML list, so the UI, the `/api/mode` validator, and the Home
  Assistant discovery `select` all derive from the same canonical mode set and
  can't drift apart. Adding a new mode to the enum now surfaces everywhere by
  construction (a completeness test fails if the catalog omits one).

### Patch Changes

- 50a2242: main: classify EV / vehicle drivers via Lua DRIVER catalog instead of filename prefix sniffing

  `driverCapacitiesFrom` and `warnIfEVHasBatteryCapacity` previously fell back
  to a hard-coded filename-prefix allowlist (`easee`, `ocpp`, `ctek`,
  `chargestorm`, `tesla_vehicle`, `vehicle`) to decide whether a YAML driver
  entry pointed at an EV charger or vehicle telemetry source. That coupled
  the controller surface to vendor names and required code edits whenever a
  new EV-charger Lua driver was added.

  Each Lua driver already self-declares its kind via the `DRIVER = { …
capabilities = { … } }` table at the top of the file. EV chargers
  declare `"ev"`; vehicle telemetry drivers declare `"vehicle"`. This PR
  adds `drivers.IsEVOrVehicleDriver(catalog, luaPath)` which consults that
  self-declaration, and routes both pre-flight call sites through it. The
  filename sniffer is deleted.

  The catalog is parsed once at startup (text scan, no Lua VM) and re-
  scanned on every config hot-reload so a hot-edited driver's capability
  change is picked up without a service restart.

  No new schema. No operator action required — every EV/vehicle driver
  that shipped with the repo already declares the right capability.

- 5358f5e: Fix the Ferroamp battery under-charging (and spilling PV surplus to grid) on
  multi-ESO sites where one battery is saturated. The EnergyHub splits a charge
  setpoint evenly across all ESOs; a unit in CV taper near full — or held back by
  the EHub's own SoC balancing — absorbs almost none of its share, so the pack
  caps well below the commanded power (observed on Stefan's 4×ESO site: an 8.3 kW
  command delivered only 4.3 kW, the rest exported).

  Replaces the previous per-unit acceptance threshold (which flapped: a saturated
  ESO trickling ~170 W against a ~650 W share still read "charging", dropping the
  up-scale and re-spilling surplus) with a **delivery-ratio loop**: the driver
  measures how much of its last on-wire setpoint the pack actually absorbed
  (`eff = |delivered| / |on-wire|`, EMA-smoothed) and scales the next command by
  `1/eff`, bounded by `MAX_DISPATCH_SCALE` (2.0×). A saturated unit's trickle is
  simply summed in rather than voted on, so the command rises until the units
  that _can_ take more cover the deficit — converging on the commanded power
  regardless of which units under-pull, with no threshold to flap on. The
  SoC-capable count is retained only for the "every unit floored/ceilinged → idle"
  guard. New `eso_accept_eff_x1000` metric exposes the measured efficiency.
  Verified live: battery charge rose 4.3 kW → ~8 kW and the surplus stopped
  spilling.

- 77aa663: fix(battery): honor configured soc_max at the driver, not just the planner

  A battery's `soc_max` (and `soc_min`) only reached the planner, never the
  driver. The Ferroamp driver therefore applied its own built-in
  `CHARGE_CEIL_SOC` default of 0.95: once every ESO crossed ~95% it reported the
  pack "not charge-capable" and idled the charge command, so a site configured
  with `soc_max: 1.0` plateaued around 95–97% even while dispatch kept asking for
  charge.

  `config.WithBatterySoCBounds` now defaults each battery driver's
  `charge_ceil_soc` / `discharge_floor_soc` from the matching
  `batteries.<name>.soc_max` / `soc_min`, applied at both startup and config
  hot-reload. An explicit value in the driver's own `config:` block still wins,
  and the persisted config is never mutated (so a later `soc_max` change is not
  shadowed by a stale derived value). `soc_max` is now the single source of truth
  for a battery's usable SoC window.

- 76069a5: Fix plain local Docker builds so native `docker build .` does not depend on buildx-only platform args.
- 5c6e8ed: Fix docker pulls failing on slow connections at first boot and in-app update.

  `firstboot.sh` capped retries at 6 attempts (~90 s total) — shorter than a single
  pull on a 0.5 Mbps link. It now retries indefinitely with a 60 s gap; the sentinel
  is only written on success so a reboot is always a safe abort path.

  `ftw-updater` shared one 2-hour context across all 3 pull attempts, leaving almost
  no room for retries after a slow-but-failed download. Each attempt now gets its own
  independent 2-hour window, retries are unbounded, and `compose up -d` gets a
  separate 10-minute timeout since it only recreates an already-pulled container.

- cf0acb1: Use the locked site-meter driver snapshot when idling batteries after stale site-meter telemetry.

## 0.126.0

### Minor Changes

- 33d0608: Add a read-only NIBE S-series heat-pump driver (`drivers/nibe_local.lua`) that reads the pump's on-prem **Local REST API** directly over the LAN — no cloud, no OAuth, no internet round-trip. It auto-detects the device serial, pulls the full ~980-point register map in one bulk request, and emits every point into the long-format TS DB with **exact per-point divisor scaling** (the local API ships each point's unit + divisor, so no °C×10 heuristic). It reuses the same `hp_*` headline metrics as the MyUplink cloud driver (`hp_power_w`, `hp_hw_top_temp_c`, `hp_outdoor_temp_c`, `hp_used_power_w`, `hp_energy_consumed_kwh`, …), so the heating dashboard works with either source. The headline variable ids are auto-selected per pump from a built-in model/`firmwareId` profile map (generic S-series default, config-overridable), so one driver covers the whole S-series. Observe-only — it never writes to the pump.

  Each emitted signal also carries its source **Modbus register id**, surfaced as a **Register** column in the per-driver "all signals" detail view. This is plumbed through a new optional 4th argument to `host.emit_metric(name, value, unit, register)` (generic for any driver with addressable points; drivers that omit it are unaffected).

  Also adds **opt-in TLS certificate pinning** to the Lua HTTP host capability: `capabilities.http.tls_pin_sha256` pins an HTTPS endpoint's leaf certificate by SHA-256 fingerprint. This lets a driver reach a self-signed endpoint (the heat pump's local API) by trusting exactly one certificate — rejecting a swapped cert (LAN MITM) at the handshake — instead of disabling verification. Drivers without a pin are unaffected (standard system-root verification).

- 33d0608: Network scan now probes port 8443 (HTTPS) in addition to Modbus 502 / MQTT 1883 /
  HTTP 80. On-prem device APIs that only listen on HTTPS — notably the NIBE S-series
  heat pump's Local REST API — now show up in Settings → Scan and the setup wizard.
  Previously a NIBE pump was pingable but invisible to discovery because its API
  port wasn't probed.
- 33d0608: The heat-pump "all signals" detail view can now explain every signal.
  `host.emit_metric` gained an optional 5th `title` argument — the device's own
  human-readable point label — which threads through the telemetry snapshot and
  `/api/drivers/{name}` (as `title`) so the UI can show a plain-language line under
  each of the ~960 raw signals. The NIBE local driver passes each point's NIBE
  title (e.g. "Frånluft (BT20)"); other drivers are unaffected (the arg defaults
  to empty).

### Patch Changes

- e8b4bf4: Dashboard light-mode + heat-pump card fixes.

  The live power/energy **charts now render correctly in light mode**. The canvas chrome (axis labels, gridlines, zero/now/hover lines, both tooltips' background+border+text, and the neutral "Load" line + legend swatch) was hard-coded for the dark theme, so it went invisible or wrong on a light background. The charts now resolve the CSS theme tokens (`--fg`, `--fg-dim`, `--fg-muted`, `--line`, `--ink-raised`, `--accent-e`) into concrete canvas colors per draw — cached and re-read when `data-theme` changes. Saturated data-series hues are unchanged.

  The **heat-pump card now re-discovers newly-added drivers** without a manual reload. `heating.js` cached discovery once and never re-checked — and an empty result is truthy, so a site that discovered before its pump reported `hp_power_w` stayed blank. It now re-scans on first load and every 5 minutes, while steady-state polling still only touches already-known heat-pump drivers.

  The heat-pump **"all signals" detail view now shows a Register column** — each signal's source Modbus register id (read from the metric snapshot the driver reports). Signals with no Modbus mapping show "—".

  The **heat-pump card itself now themes in light mode**. It renders into a bare `.card`, whose base style uses the `--surface` token — which the light theme doesn't flip — so the card stayed dark on a light page while every other dashboard card themed correctly. It now uses the same `--ink-raised` palette as the rest.

## 0.125.0

### Minor Changes

- e2fa6d4: Migrate the Raspberry Pi SD-card image to Raspberry Pi OS Trixie with cloud-init, and publish a Raspberry Pi Imager repository JSON so Imager 2.0's customisation panel (hostname, SSH user/password, WiFi) works again.

  Point Raspberry Pi Imager at `https://github.com/frahlg/forty-two-watts/releases/latest/download/os_list.json` (App Options → Content Repository → Use custom file) to flash "Forty-Two Watts" with full per-flash customisation. The repository JSON is rebuilt and uploaded on every release with the new image's URL, sizes, and verified checksum.

  The on-image deploy directory moved from `/home/ftw/forty-two-watts` to `/opt/forty-two-watts` so a user-chosen account name can't orphan it. The `curl | install.sh` and manual docker-compose install paths are unaffected.

### Patch Changes

- af05294: Adopt the Apache License, Version 2.0. The project now ships a `LICENSE` file
  plus a `NOTICE` and `CONTRIBUTING.md` (DCO). Previously the license was only
  declared as "MIT" in the README; prior MIT-licensed contributions are
  acknowledged and preserved in `NOTICE`.
- 7e6e04d: EV Charger modal reworked. The controls are now split into three tabs —
  **PV charging** (surplus-only toggle), **Manual** (amp slider +
  Start/Stop), and **Scheduled** (current-SoC correction + target-SoC-by-
  deadline schedule) — so each charging mode has its own space. The
  current-SoC correction lives under Scheduled because it's a planning
  input. Both SoC inputs (current correction and Target SoC) are now
  0–100% sliders in whole-percent steps with a live mono % readout instead
  of free-text number fields. The "Let battery cover EV" toggle stays as a
  persistent footer. No backend changes.
- ddbba7a: Loadpoint SoC now anchors to the live vehicle BMS reading when a vehicle
  driver (e.g. Tesla via TeslaBLEProxy) is online and matched to the
  loadpoint. Previously the EV card showed the delivered-Wh _inferred_
  estimate labelled "(vehicle)", which drifts from the car's real SoC
  (chargers like Easee can't read the pack) — so an actively charging
  Tesla reading 31% could show e.g. 36% on the card. The control loop now
  re-anchors `current_soc_pct` to the paired vehicle's BMS SoC each tick
  (only when the reading is online, fresh, and not driver-flagged stale),
  so the dashboard, the planner's `InitialSoCPct`, and the MPC all agree on
  BMS ground truth. When the vehicle goes BLE-silent the estimate continues
  from the last known BMS value instead of snapping back to the plug-in
  guess.
- 24b1a5c: A manual EV charge hold ("Start" / amp slider) now auto-releases when the
  vehicle stops requesting current — e.g. it reaches its own charge limit
  and is full. Previously the hold pinned the wallbox at a fixed amperage
  and the loadpoint kept showing "charging" at 0 W until the operator
  pressed Stop. The controller now drops the hold after the vehicle has
  been not-requesting for SessionCompletionTimeout (90 s, debounced against
  brief ramp/handshake dips), falling back to automatic dispatch. Only
  applies to chargers that report "vehicle no longer requesting current"
  (e.g. Easee); chargers that can't distinguish that are unaffected.
- 0d548e6: Fix the RPi image's port 80 → 8080 redirect, which never worked.

  The nftables rules file named its table `42w_redirect`. nftables identifiers
  must match `[a-zA-Z_][a-zA-Z0-9_]*`, so the leading digit was tokenized as a
  number and the entire file failed to parse. `42w-port-redirect.service` exited
  1 on every boot and no redirect was ever installed, leaving the dashboard
  reachable only at `http://42w.local:8080/` rather than the bare
  `http://42w.local/` the README advertises.

  Renamed the table to `ftw_redirect`. The rule logic was otherwise correct.

## 0.124.4

### Patch Changes

- aeac725: MyUplink driver self-heals instead of needing a manual restart. NIBE/MyUplink
  is touchy right after consent (token propagation / rate-limit), so the first
  auth or device-detect can fail — previously the driver then idled forever on a
  nil device_id and only came online after the operator hit Restart. driver_poll
  now retries setup (auth + device detection) with a 30 s backoff, so it recovers
  on its own within a poll or two.

## 0.124.3

### Patch Changes

- 0db6692: Set the car's SoC from the EV Charger modal. Clicking the EV planet now
  shows a "State of charge" section: while the car is plugged in it offers
  an inline % field + Set (re-anchors the inferred SoC via
  POST /api/loadpoints/{id}/soc and replans); unplugged it shows the current
  value with a hint. Previously this lived only in the advanced loadpoints
  panel — now it's where the SoC is naturally looked for.

## 0.124.2

### Patch Changes

- 6e321b3: Driver cards no longer show phantom battery-SoC / PV / meter "0" rows for
  telemetry-only drivers. A driver that emits only scalar metrics (e.g. the
  MyUplink heat pump) has no meter/pv/battery DER reading, but the driver
  card fell through to the meter/pv/battery layout and rendered 0 W / 0 %
  with an empty SoC bar. Such drivers now render a compact "telemetry only"
  body (status + ticks + errors); open Diagnose / the Heat pump card for
  their signals.

## 0.124.1

### Patch Changes

- 33ceac5: Manually correct a vehicle's State of Charge from the UI. The loadpoint
  card (advanced mode) now shows an inline ✎ next to the SoC while the car
  is plugged in — click it, type the real %, and it re-anchors the inferred
  SoC via the existing POST /api/loadpoints/{id}/soc (then triggers an MPC
  replan). Useful when there's no vehicle BMS reading and the inferred SoC
  has drifted.

## 0.124.0

### Minor Changes

- 20375bf: Rich heat-pump telemetry. The MyUplink driver now captures **every** device
  point (not just four), emitting each as a metric with its unit. A new
  click-through **detail view** on the Heat pump dashboard card groups all
  signals by unit class (temperatures / power / frequency / percent / electrical
  / counters & degree-minutes / state), each with its current value and a 24h
  sparkline. `host.emit_metric` gains an optional `unit` argument (carried into
  the live snapshot for UI grouping); metric emission now also registers a driver
  health success, so a read-only telemetry driver no longer shows as offline.

## 0.123.3

### Patch Changes

- d851d8a: Clearer MyUplink onboarding. The device card now shows numbered setup
  steps with a link to the MyUplink developer portal, and renders **Client
  Identifier** and **Client Secret** together using the exact same labels as
  the portal (instead of a separate "Client ID" field and a distant
  "Secrets" section), so the two values can't be swapped. The OAuth-managed
  refresh_token no longer appears as a hand-editable secret field.

## 0.123.2

### Patch Changes

- b58891a: Fix read-only telemetry drivers showing "offline" with "last success:
  never" despite live data. A driver that only calls `host.emit_metric`
  (e.g. the MyUplink heat-pump driver) never recorded a health success, so
  the watchdog flipped it offline even while it polled and emitted metrics
  fine. `emit_metric` now records a health success like the structured
  `host.emit` does. The dispatch stale-meter guard is unaffected — it keys
  on per-reading (DerMeter) freshness, not driver-level last-success.

## 0.123.1

### Patch Changes

- 9483205: Fix MyUplink "connects but finds nothing": the driver read the wrong JSON
  keys from the MyUplink API. Device auto-detection used `systems.objects`
  but the real `/v2/systems/me` response keys the array as `systems`, so no
  device was ever found. Also read the points unit from `parameterUnit`
  (the real field name) instead of `unit`, so kW→W conversion works.

## 0.123.0

### Minor Changes

- 48f972b: MyUplink heat-pump driver now uses the authorization-code OAuth flow the
  MyUplink developer portal actually supports, fixing the `invalid_client`
  startup failure (the old `client_credentials` grant is not offered for portal
  apps). A new in-app consent flow (Settings → Devices → "Connect to MyUplink")
  handles the one-time browser sign-in, stores the refresh token as a masked
  secret, and keeps it fresh — the driver runs `grant_type=refresh_token` at
  runtime and persists Azure-B2C-rotated tokens via the new `host.persist_secret`
  capability so they survive restarts. A manual fallback (paste the redirected
  URL) completes the consent on origins where the auto-callback can't reach the
  Pi (relay/home route, or an http LAN callback the portal rejects) — the Pi
  exchanges the code over its own outbound HTTPS, so no inbound callback is
  needed.

  A new **Heat pump** dashboard card surfaces the driver's telemetry (compressor
  power + hot-water / indoor / outdoor temperatures, with a 24h power sparkline).

  Note: the driver has no `mode` field — it is read-only telemetry for one
  physical pump.

## 0.122.0

### Minor Changes

- c85ad70: Make owner remote access reachable AND fast. The relay now publishes shared
  ICE/TURN configuration (new `GET /signal/ice`, new `-ice-stun`/`-turn-url`/
  `-turn-secret` flags) so hard-NAT/CGNAT owners can reach their Pi over a
  TURN-relayed DTLS channel, while owner API writes stay on the fail-closed P2P
  transport.

  Connection setup is also much snappier. The browser no longer arms its retry
  cooldown on a transient cold-load (directory-not-yet-decrypted) race — the
  single biggest contributor to the old multi-second "Reaching your home" stall —
  and both ends now POST their offer/answer as soon as a usable candidate set is
  gathered (host + server-reflexive, plus a relay candidate when TURN is
  configured so symmetric-NAT owners still traverse) instead of waiting for full
  ICE gathering. The Pi caches the relay ICE config and refreshes it hourly
  instead of refetching it on every offer, retries use exponential backoff, and a
  transient `/signal/ice` failure reuses the last good config instead of silently
  dropping TURN.

## 0.121.0

### Minor Changes

- 941a81f: Add a Tesla-style manual charge control to the EV charging modal: an amp
  slider (range = the loadpoint's min/max charge current) plus Start / Stop.
  Start pins a **persistent** manual hold at the chosen amperage that now
  **overrides `surplus_only`** — when the operator explicitly asks to charge,
  we honour it and import from the grid if PV is short. Stop releases the hold
  and drops straight back to automatic charging (PV-surplus-only when that
  toggle is on).

  Behaviour change: previously `surplus_only` clamped any manual hold down to
  the available PV surplus (a manual "Start" with no sun did nothing). A manual
  hold now takes priority over surplus; the per-phase fuse clamp remains the one
  guard a manual hold can never override. Persistent holds carry no time expiry
  and are auto-released when the vehicle unplugs. `POST .../manual_hold` now
  accepts `hold_s: 0` (or omitted) to mean a persistent hold; `hold_s > 0` is
  still the bounded diagnostic hold. `GET /api/loadpoints` gains `phases`,
  `voltage_v`, `manual_active`, and `manual_charge_w` so the UI can render the
  amp slider and reflect the current override.

  The now-redundant Start / Pause / Resume footer buttons in the EV modal are
  removed — the amp slider's Start/Stop supersedes them (Start is strictly more
  capable: it pins a chosen amperage instead of always MaxChargeW). The
  `POST /api/ev/command` endpoint is retained for Home Assistant / scripts.

- caf07c3: Add a read-only MyUplink heat-pump telemetry driver (`drivers/myuplink.lua`).
  Authenticates to the MyUplink Cloud REST API v2 (OAuth2 client_credentials,
  READSYSTEM scope) and emits compressor power and hot-water / indoor / outdoor
  temperatures into the time-series DB. Observe-only — no control. Configure the
  Client ID in Settings → Devices; the Client Secret is stored as a masked
  config secret.
- 6ec93f8: Add manual V2X pilot support with experimental Ferroamp DC2 and Ambibox MQTT drivers, signed `v2x_charger` telemetry, `/api/v2x/command`, V2X policy readback, dashboard controls, and V2X-aware load/status accounting.

### Patch Changes

- c76ac32: Fix surplus-only EV charging never starting on a "dumb" charger (CTEK and
  other AC wallboxes that don't report the vehicle's BMS SoC). In
  self-consumption mode the home-battery PI absorbs all PV surplus, so the EV
  loadpoint sees nothing to claim. `EVSurplusOnlyReserveW` is supposed to hold
  back export headroom for the EV, but `SurplusReserveW` only reserved a
  bootstrap floor for a plugged-but-not-drawing EV when the vehicle's SoC was
  known and below its limit — a dumb charger reports no SoC, so the reserve was
  0, the battery took everything, and the EV could never bootstrap (chicken-and-
  egg, observed live on Stefan's CTEK).

  `SurplusReserveW` now reserves the loadpoint's `MinChargeW` (or the ramp
  headroom) for a plugged, surplus-only, not-drawing EV **unless the car is known
  to be full** (SoC known and at/above its charge limit). This prioritises PV
  into the EV ahead of the home battery, as intended. Trade-off: a finished-but-
  still-plugged car on a dumb charger holds the reserve (exports rather than
  charging the home battery) until unplugged — surfacing the charger's own "done"
  state into the loadpoint would let us skip that case too (follow-up). Smart
  chargers/paired vehicles are unaffected: a car known to be full still reserves
  nothing.

- 6ec93f8: Fix dashboard stalls on late-day loads by aggregating the `/api/status`
  energy-today totals in SQLite instead of loading every history sample
  since midnight into Go on every 2-second status poll.
- 28080b5: Fix Modbus drivers getting stuck after a device goes mute on a long-lived
  session. The reconnect classifier (`isTransportError`) recognised closed-socket
  errors but not `simonvetter/modbus`'s own deadline sentinel `ErrRequestTimedOut`
  ("request timed out") — a plain string-typed value that is neither a `net.Error`
  nor wraps `syscall.ETIMEDOUT`. When a device kept the TCP socket `ESTABLISHED`
  but stopped answering requests on it, every read/write timed out and the wrapper
  reused the dead socket forever instead of redialing.

  Seen in the field on a CTEK Chargestorm CSOS charger: 2907 consecutive
  charge-limit writes timed out over ~43 h, so the controller could never set the
  EV charge current and the loadpoint never followed PV surplus — while a fresh
  connection to the same charger read and wrote the register instantly. The
  classifier now treats the timeout sentinel as a transport error, so the next
  call tears down the wedged socket and dials a fresh one.

- 6ec93f8: Add a global `site.troubleshooting_mode` for incident diagnostics. The mode exposes its state in `/api/status`, logs dispatch-decision snapshots without changing control behavior, and passes a reserved troubleshooting flag into Lua drivers. Pixii now uses that flag to emit calibration/control status and setpoint readback metrics, while still supporting its legacy per-driver troubleshooting flag. Invalid Pixii SoC values now omit `soc` from the battery emit instead of dropping the whole battery reading.
- bc9e473: fix(loadpoint): resume PV surplus after a self-induced charger stop (NCRQ)

  When a surplus_only loadpoint was paused below its 3-phase floor on a sub-floor
  PV dip, the charger reported the vehicle "not requesting current" (NCRQ). That
  stop is self-induced — we withheld power, the car didn't decline — but it was
  counted toward session completion. After the 90 s timeout the loadpoint latched
  the session "complete" and the planner stopped offering PV surplus for the rest
  of the day, so the charger never restarted when the sun returned (the home
  battery soaked the surplus instead).

  Two complementary fixes, both generic across EV-charger drivers:

  - The controller now tells the loadpoint manager when it is withholding power
    (`SetSurplusWithheld`). A "not requesting current" report during a
    self-induced pause no longer advances the session-completion timer; a genuine
    vehicle-side refusal once power is offered again still completes as before.
  - Chargers with no vehicle-API binding (e.g. a bare CTEK) couldn't be woken
    from NCRQ because the wake path required a bound vehicle driver. The
    controller now drives the wake off the charger's own connector state: when
    surplus recovers and we offer power but the charger is still in a
    self-induced NCRQ, it cycles the contactor (ev_pause → ev_resume) to make the
    vehicle renegotiate, throttled to once per cooldown.

- 6ec93f8: Internal: add the `thermal` package — site-level asset contract types
  (`TemperatureBand`, `MarginalPrice`, `DecideIntent`) for the upcoming heat-pump
  workstream. Not yet wired into control or MPC, so there is no user-visible
  behavior change; it lands as scaffolding the thermal-store model will consume.

## 0.120.9

### Patch Changes

- 4af0caf: Add a UI-controlled global troubleshooting mode for incident diagnostics. It exposes its state in `/api/status`, logs dispatch-decision snapshots without changing control behavior, and passes a reserved troubleshooting flag into Lua drivers. Pixii now uses that flag to emit calibration/control status and setpoint readback metrics, while still supporting its legacy per-driver troubleshooting flag. Invalid Pixii SoC values now omit `soc` from the battery emit instead of dropping the whole battery reading.
- a0ad92a: Prevent surplus-only EV loadpoints from using home battery discharge as synthetic PV surplus, even when "Let battery cover EV" is enabled, and surface the policy in planner diagnostics.

## 0.120.8

### Patch Changes

- 6238d2d: Settings → Planner: the Mode dropdown is gone — it edited a config field that the runtime strategy (set from the dashboard Plan card) overrides, so it showed stale values and confused operators. In its place a read-only "Active strategy" row shows what the planner is actually running. The "PV forecast safety (k)" help text now explains the real mechanism (plans against forecast − k·σ; higher k holds more reserve and charges earlier, it never forces charging), with a live σ/hedge readout under the field.
- 9e5635a: Reaching your dashboard over Tailscale no longer shows the passkey sign-in
  gate. The browser's LAN-origin detection treated Tailscale's CGNAT addresses
  (100.64.0.0/10, RFC 6598) as a public/relay origin, so it waited for a P2P
  channel that a direct connection never opens and fell back to the sign-in gate
  — while the same Pi reached over zerotier (192.168.0.0/16) sailed straight
  through. The CGNAT range is now recognised as a direct-LAN origin in all three
  copies of the check (`p2p.js`, `next-app.js`, `owner-access/owner-fetch.js`),
  matching `p2p.js`'s own `isDirectLAN`. Direct IP access — LAN, zerotier, or
  Tailscale — reaches the dashboard without a passkey prompt.

  The Pi-side LAN-presence check (`isLANClientSource`) now recognises the same
  CGNAT range, so owner-admin actions (manage passkeys, mint the setup PIN,
  bootstrap the first passkey) work over Tailscale exactly as over an RFC1918 LAN.
  An overlay you joined to your Pi is an explicit, authenticated owner decision —
  genuine LAN presence — while the relay path stays excluded by the X-FTW-Tunnel
  marker and the loopback check, so the friend pair-flow still cannot reach
  owner-admin.

## 0.120.7

### Patch Changes

- 05932a3: Fix Home Assistant logging "Invalid option for select.forty_two_watts_mode"
  for the planner modes. The MQTT discovery for the Mode `select` only
  advertised six modes, but the bridge publishes the live mode as state — and
  the default UI choices (`planner_passive_arbitrage` / `planner_arbitrage`)
  weren't in the advertised list, so HA rejected them every cycle. The discovery
  options and the API mode validator now both derive from a single
  `control.AllModes` source of truth, so all ten modes are advertised and the
  two lists can't drift again.

  Also fixes the matching command path: selecting a planner mode from the Home
  Assistant dropdown previously returned "unknown mode" because the HA `SetMode`
  callback (and the boot-time mode restore) carried their own hand-maintained
  mode lists that omitted the planner modes. Both now validate through
  `control.IsValidMode`, and the HA setter mirrors the full `/api/mode`
  side-effects (battery manual-hold clear, PI reset, MPC strategy propagation)
  via a shared `control.PlannerMPCMode` mapping — so a planner mode picked in HA
  behaves identically to one picked in the web UI.

- 9f4eab0: Fix surplus-only EV charging never starting on 3-phase-only chargers (e.g.
  CTEK Chargestorm — 3Φ, 6 A minimum, no phase-switch register). The surplus
  controller's 1Φ fallback assumed every charger can trickle on a single phase
  (true for Easee, false for CTEK): on any day the PV forecast couldn't sustain
  the 3Φ minimum it locked the loadpoint to 1Φ for the day and handed the
  charger a ~1380 W offer, which a 3Φ-only unit can only answer by writing 0 A —
  so it never charged. `pickSurplusSteps` now keeps the 3Φ-only step set and
  never commits the day-long 1Φ lock when the loadpoint is pinned to
  `phase_mode: "3p"`, and `resolvePhaseMode` no longer lets a stale 1Φ lock
  override an explicit `"3p"`. A 3Φ-only charger now pauses cleanly below the
  ~4.1 kW floor and charges in 3Φ steps above it. Configure such chargers with
  `phase_mode: "3p"`, `min_charge_w: 4140`, and 3Φ-only `allowed_steps_w`.

## 0.120.6

### Patch Changes

- Fix self-update snapshots after Remote Access passkeys have been enrolled, and route more public-home dashboard reads through the strict owner transport.

  The state snapshotter now copies SQLite tables in foreign-key dependency order, so remembered browser keys in `trusted_device_pubkeys` no longer make pre-update snapshots fail. Dashboard, update, diagnostics, notification, pair, HA, EV, system, and loadpoint reads used by the public home route now use `ownerFetch`, preventing raw `/api/*` calls from hitting the relay and producing 403 noise while the P2P channel is opening.

## 0.120.5

### Patch Changes

- Fix Remote Access browser persistence and Home Assistant mode discovery.

  Remote owner sessions now last 30 days, and the legacy passkey login page stores
  the browser's remembered device key after a successful passkey login so reloads
  and bookmarks can reuse silent sign-in. The public home loader also explains the
  current synced-passkey limitation when a browser cannot recover the encrypted
  home directory from WebAuthn PRF.

  Home Assistant MQTT discovery now advertises every mode the service can publish,
  including planner modes, and MQTT mode commands accept the same planner values as
  the HTTP API.

## 0.120.4

### Patch Changes

- Fix first-device Remote Access setup for normal Pi releases: the Pi-side tunnel now allows the claim-key-gated enroll start/finish bridge by default, while all other owner API calls remain P2P-only. The tunnel also returns stable `FTW_*` diagnostic codes when it refuses a request.

## 0.120.3

### Patch Changes

- Add Remote Access support diagnostics: stable `FTW_*` error codes on relay/Pi setup failures, clearer passkey enroll/login troubleshooting details, and a browser-local reset button for stale remote routing cache.

## 0.120.2

### Patch Changes

- Fix Remote Access first-passkey setup so official installs use the public relay
  after opt-in, only show a setup QR once the relay has accepted the live
  invitation, and surface clearer Safari/passkey setup errors.

## 0.120.1

### Patch Changes

- f89bfe6: Fix the Raspberry Pi SD-card image release build so pi-gen does not OOM on Docker's apt repository during export.

## 0.120.0

### Minor Changes

- 0174f14: Multi-tenant home-route client + Pi instance descriptor — still behind the relay's `-multi-tenant` flag (NOT yet live in production).

  Completes the browser + Pi half of the multi-tenant home route (the relay half shipped in v0.118.x):

  - **Web:** a PUBLIC landing for anonymous visitors (brand + passkey button only — no instance data); passkey sign-in that derives the directory key from the WebAuthn **PRF** extension, fetches + AES-GCM-decrypts the per-wallet directory blob from the relay, verifies each entry's Pi signature, and routes to the chosen instance's **own** Pi. Identity is pinned **first-key-wins** per `(origin, site_id)` and the relay's `/api/identity` is **never trusted on the public route** (anti-MITM); the Ed25519 directory write key is generated once and synced inside the encrypted blob.
  - **Pi:** `GET /api/owner-access/instance-descriptor` (owner-authed, served over the P2P channel) returns the Pi's signed `{site_id, pi_pubkey, label}` so the browser can build + verify its directory entry; first enrollment seeds the encrypted directory blob.
  - The single-tenant / LAN sign-in flow is **untouched** — the multi-tenant path is additive and only active on the public home route.

  Codex-reviewed: two anti-MITM findings (relay-identity TOFU on the public route; pin-overwrite) found and fixed. Cross-language interop is locked by tests: JS-signed blob PUTs verify in Go, and Go-signed descriptors verify in the browser.

  Cutover (flipping `-multi-tenant` on the relay + deploying this web bundle as `-home-web`) still needs the WebAuthn-PRF determinism device test on real synced devices + live browser validation. See `docs/superpowers/specs/2026-06-05-multi-tenant-home-route-design.md`.

- 0174f14: Multi-tenant onboarding bootstrap on a high-entropy `bootstrap_id` — behind the relay's `-multi-tenant` flag (default OFF, not yet live in production).

  A first-time user with no device key can enroll their first passkey on `home.*` without a prior P2P channel. On the LAN the box mints a 6-digit PIN **and** a high-entropy `bootstrap_id` (≥32 bytes CSPRNG, base64url); the raw secret travels only in the onboarding link's `#fragment` (QR or tap). The relay keys its blind, TTL'd bootstrap store on `claim_key = hex(sha256(bootstrap_id))` — **never** the PIN — so it never holds a guessable secret. The browser derives the same `claim_key`, claims the box's signed descriptor back, verifies its Pi signature, and enrolls through the relay's single enroll-forward (`?claim_key` relay gate + `?pin` validated by the box, 5-try burn). The publish carries `ts_ms` (±30s replay guard) and the enroll-forward is atomic single-use (a 200 finish consumes the window).

  Reworked from the earlier `sha256(PIN)`-keyed store (a Codex audit found the ~10⁶ PIN space brute-forceable offline). A second Codex re-audit then found two more issues, both now closed:

  - **Ceremony-bound, body-bound possession proof** (closes a relay-visible-PIN HIGH **and** a device_pubkey-substitution HIGH). The relay forwards `?pin` to the box, so a compromised relay would see the 6-digit PIN. The browser now also sends `bootstrap_proof = hex(HMAC-SHA256(key=bootstrap_id, msg=ceremony_token + "|" + hex(sha256(finish_body))))` on enroll/finish; the box validates it (constant-time) on the tunneled path before saving any device. Binding a hash of the **exact finish body** authenticates the whole payload — including the top-level `device_pubkey` (the C4 silent-login key), the WebAuthn attestation, and the friendly name — so a MITM relay can no longer swap `device_pubkey` for its own key (which would have made the relay-controlled key a trusted device and let device-PoP mint `ftw_owner` for the relay). The relay holds only `sha256(bootstrap_id)`, so it can neither forge a proof for its own `ceremony_token` nor reuse the user's single-use one, and it cannot recompute the HMAC after tampering with the body. A relay that captured the PIN still cannot run its own WebAuthn enrollment nor substitute a key. An untunneled LAN finish requires no proof.
  - **Single-use before side effects** (closes a concurrent-double-finish BLOCKER). The relay now atomically RESERVES the bootstrap (test-and-set) BEFORE forwarding enroll/finish to the box — a concurrent second finish loses the latch and is refused before its enroll can reach the box. A box 200 burns the window; a box rejection releases it for retry. As a source-of-truth backstop, the box re-checks the zero-device window at finish time and refuses if any device already exists.

  Cross-language interop is locked by tests (Go-signed bootstrap descriptor verifies in the browser `verifyEntry`; the browser's `claim_key` and the body-bound `bootstrap_proof` derivations match the box/relay byte for byte, against Go- and openssl-verified vectors). The relay↔box enroll path is now end-to-end: a full-stack e2e drives the real `ftw-relay` enroll-forward host into the real box enroll handler — software-attested enroll/finish with a valid proof (200), wrong/missing proof (403), a MITM relay that swaps `device_pubkey` in the forwarded body (403, no device saved), concurrent double-finish (exactly one 200 + one 403 via the relay reservation), the box zero-device recheck (403 once a device exists), and the C2 fail-closed cases. Still gated by `-multi-tenant` (default OFF); `home.*` stays a 404 in production.

- 0174f14: Route public dashboard price, history, savings, plan, and loadpoint reads through the strict owner P2P transport so they render on the relay-backed home route.

  Remember multiple browser-local device keys for a synced passkey, so Safari on Mac can silent-login after an iPhone-enrolled passkey has already pinned its own key.

  Add a Settings Access tab for the opt-in remote access switch, passkey management, remembered browser keys, and active session revocation.

  Make `remote_access.enabled` the only remote-access opt-in; `FTW_REMOTE_ACCESS_ENABLED` no longer enables remote access by itself.

  Keep the home-route sign-in gate retrying while the owner P2P channel is still opening, and give Chrome/WebRTC a longer handshake window before retrying.

  Clear stale browser-local `ftw.p2p=off` toggles from older builds on the public home route, where strict owner traffic must use the P2P channel.

  Generate or rotate to a persistent high-entropy owner `site_id`; guessable `site:<name>` routing is not preserved.

  Stop bootstrapping fresh public browsers to `site:Home`; without a cached directory the home route now shows the setup/sign-in landing instead of guessing a site.

  Stop serving the dashboard app bundle from the relay in multi-tenant mode. The relay now serves only a small remote loader/login allowlist; after the browser decrypts its directory, static app GETs are routed to the selected Pi while owner APIs remain P2P-only.

  Publish `ftw-relay-web.tar.gz` as a minimal relay bootstrap asset so relay deploys no longer copy the Pi dashboard `web/` bundle.

  Cache Pi-routed static dashboard assets privately in the browser and pause advanced-panel polling until Advanced is visible, improving repeat-load and remote-route responsiveness.

## 0.119.0

### Minor Changes

- b9ab35b: Multi-tenant home-route client + Pi instance descriptor — still behind the relay's `-multi-tenant` flag (NOT yet live in production).

  Completes the browser + Pi half of the multi-tenant home route (the relay half shipped in v0.118.x):

  - **Web:** a PUBLIC landing for anonymous visitors (brand + passkey button only — no instance data); passkey sign-in that derives the directory key from the WebAuthn **PRF** extension, fetches + AES-GCM-decrypts the per-wallet directory blob from the relay, verifies each entry's Pi signature, and routes to the chosen instance's **own** Pi. Identity is pinned **first-key-wins** per `(origin, site_id)` and the relay's `/api/identity` is **never trusted on the public route** (anti-MITM); the Ed25519 directory write key is generated once and synced inside the encrypted blob.
  - **Pi:** `GET /api/owner-access/instance-descriptor` (owner-authed, served over the P2P channel) returns the Pi's signed `{site_id, pi_pubkey, label}` so the browser can build + verify its directory entry; first enrollment seeds the encrypted directory blob.
  - The single-tenant / LAN sign-in flow is **untouched** — the multi-tenant path is additive and only active on the public home route.

  Codex-reviewed: two anti-MITM findings (relay-identity TOFU on the public route; pin-overwrite) found and fixed. Cross-language interop is locked by tests: JS-signed blob PUTs verify in Go, and Go-signed descriptors verify in the browser.

  Cutover (flipping `-multi-tenant` on the relay + deploying this web bundle as `-home-web`) still needs the WebAuthn-PRF determinism device test on real synced devices + live browser validation. See `docs/superpowers/specs/2026-06-05-multi-tenant-home-route-design.md`.

## 0.118.1

### Patch Changes

- 7382139: Harden the (still dormant, behind `-multi-tenant`) relay wallet-blob endpoints.

  - **Writer authentication on `PUT /wallet/{user_handle}/blob`** (closes a Codex HIGH from the v0.118.0 foundation). Each PUT now carries an Ed25519 `write_pub` + `sig`; the relay TOFU-pins the write key on the first write and rejects any later write whose key differs or whose signature fails to verify over a canonical `handle|version|nonce|sha256(ciphertext)` message. A `userHandle`-knower without the owner's passkey-derived write key can no longer overwrite or take over a blob. Wallet blobs are no longer time-GC'd (eviction would drop the pin and reopen a squat window).
  - **Route gating:** the `/wallet/*` and `/signal/{site_id}/identity` routes are now registered ONLY in multi-tenant mode, and the single-tenant home-host catch-all reserves those paths (404) — so with the flag off the endpoints add no surface (a plain 404, not a 503 or a public-key answer).

  Still dormant: `-multi-tenant` defaults off and the production home route stays disabled. The remaining cutover blocker is the WebAuthn-PRF determinism device test. See `docs/superpowers/specs/2026-06-05-multi-tenant-home-route-design.md`.

## 0.118.0

### Minor Changes

- b0e20da: Relay multi-tenant home-route foundation — behind `-multi-tenant` (default OFF, NOT active in production).

  Server-side groundwork for `home.fortytwowatts.com` to become a public multi-tenant front door (anonymous visitor → landing; a signed-in wallet → its own Pi) instead of a single pinned instance. Adds: a BLIND per-wallet encrypted-directory store (`WalletBlobStore` — opaque ciphertext the relay never decrypts, durable, bounded, version-guarded), the `GET/PUT /wallet/{user_handle}/blob` endpoints, a per-site `GET /signal/{site_id}/identity` public-key read, and a fail-closed `-multi-tenant` mode that serves ONLY the relay-disk landing/shell (never forwards to a Pi), forces `-require-device-key` on, and requires `-home-web`.

  Dormant scaffolding: the flag defaults off, the multi-tenant routes aren't registered unless it is passed, and the production home route stays disabled. Cutover is gated on a WebAuthn-PRF determinism device test and adding write-authentication to the blob PUT (see `docs/superpowers/specs/2026-06-05-multi-tenant-home-route-design.md`). No change to existing single-tenant behaviour.

### Patch Changes

- 1de3026: Clean up stale root documentation, archive retired planning artifacts, and move the legacy home-ems deploy helper out of the active scripts directory.

## 0.117.0

### Minor Changes

- dff16b5: Hardened relay + device-key remote access: no anonymous path to a home Pi.

  The relay now serves the sign-in shell (`-home-web`) and `/api/identity` (from its
  pinned `-home-pubkey`) **itself** — an anonymous internet visitor never causes the
  relay to contact the Pi. To even open a signaling channel, the browser must prove a
  **device-key**: the relay issues a single-use nonce, the browser signs it with a
  non-extractable ECDSA P-256 key (WebCrypto, IndexedDB, minted at LAN enrollment),
  and the relay verifies it against the device-pubkeys the Pi publishes on
  `/me/register` — anything else is 403'd and the Pi is never woken. The same
  device-key mints the owner session **silently** over the channel (device-PoP), so a
  returning device signs in with no Face ID; step-up still requires a passkey, and
  revoking a device drops its key on both Pi and relay. The gate UI now conveys the
  posture (direct + end-to-end + relay-blind + "this device is remembered"). LAN-first
  enrollment; see `docs/superpowers/specs/2026-06-05-hardened-relay-device-key-design.md`.

- dff16b5: Home route: a real sign-in **gate** + inline passkey login (the dashboard IS the
  door). When you open `home.fortytwowatts.com` and aren't signed in, the dashboard
  is fully covered by a clean sign-in card ("Reaching your home…" → "Sign in with
  your passkey") instead of the empty dashboard chrome — which previously rendered
  "No devices configured / run the setup wizard" to logged-out visitors and falsely
  read as an unconfigured instance. The passkey ceremony runs in place over the same
  strict P2P channel (`ownerFetch` / FIX-B) — no redirect to
  `/owner-access/login.html`. Never shown on the LAN (bypass) or once signed in; the
  "no devices" prompt is suppressed while logged out. No owner DATA is ever served
  unauthenticated — the gate is purely the lock's UI.

  Also: the transport indicator is now purely informational (it explains direct vs
  relayed vs connecting) rather than a click-to-toggle that, on the P2P-only route,
  just broke the channel. Part of the #438 seamless-UX layer (device-key silent
  re-auth still to come).

## 0.116.0

### Minor Changes

- 1dc6f35: Arbitrage cycle threshold: a new planner knob, **Min arbitrage spread
  (öre/kWh)** (`planner.min_arbitrage_spread_ore_kwh`), stops the battery
  cycling for marginal gains. The planner won't cycle for grid arbitrage
  unless the price gain beats this many öre/kWh on top of round-trip losses.
  It applies only to the arbitrage modes (`planner_arbitrage` /
  `planner_passive_arbitrage`) — self-consumption is never affected — and
  biases the planner's decision only, so the savings statistics stay on real
  spot economics. Default 0 = off. Configurable from the Planner settings tab.
- ed3cfc2: Capability-aware battery reallocation: when one battery can't move in the
  demanded direction this cycle (e.g. a Ferroamp ESO floored at its discharge
  SoC limit), the dispatcher now hands its share to a capable sibling instead
  of leaking it to the grid. Drivers signal this with two optional battery-emit
  fields, `discharge_capable` / `charge_capable`; absent → assumed capable, so
  every existing driver is unaffected. The Ferroamp driver reports both from its
  per-ESO floor/ceiling counts. Symmetric for charge (a full battery is excluded
  from the charge split).
- b586f09: Local docker E2E harness — tier 2: container-side P2P + passkey proof.

  Adds an automated, fully-in-docker browser test that drives the real home route
  through the relay and proves the WebRTC DataChannel forms **directly**
  container-to-container (where there is no NAT, unlike a Mac-host browser):

  - A headless-Chromium (Playwright) container on the tier-1 bridge net
    (`docker-compose.e2e-tier2.yml`, profile `tier2`) enrolls and logs in with a
    passkey via a **CDP virtual WebAuthn authenticator** (unattended), asserts
    `window.ftwP2P.state()` reaches `direct` and that the selected ICE candidate
    pair is host/srflx (never `relay`), then makes one authenticated owner API
    call (`/api/status`) over `p2pFetch`.
  - New `make e2e-docker-tier2` target brings the stack up, runs the test, and
    exits non-zero on failure.
  - New `FTW_P2P_STUN` env knob on the main app: unset keeps the production STUN
    set; `none`/`off` gathers host candidates only (correct + fast on a closed
    shared-L2 network like the docker bridge); a comma-separated list overrides
    the default. No behaviour change when unset.

  The harness runs the Pi with `FTW_OWNER_ACCESS_RPID=home.fortytwowatts.localhost`
  so the WebAuthn origin check passes against the `*.localhost` secure-context home
  host. Docs: `docs/local-e2e-docker.md`.

- b586f09: Owner remote access: reach your own dashboard from anywhere via a single URL
  (`home.fortytwowatts.com`) + a passkey — over a **direct, end-to-end-encrypted
  browser↔Pi WebRTC DataChannel**. OPT-IN, default OFF (`remote_access.enabled` /
  `FTW_REMOTE_ACCESS_ENABLED`); the Pi makes no outbound connection unless you turn
  it on.

  The relay is a **blind signaling rendezvous** — it brokers the connection and
  serves the static shell, but owner traffic and the session cookie exist only as
  DTLS-encrypted DataChannel frames and never traverse it in cleartext. Hardening
  that shipped with it:

  - **Signed DTLS-fingerprint handshake** (ES256 over the site identity key): the
    browser TOFU-pins the Pi's key from `/api/identity` and verifies every answer,
    so a relay that swaps the fingerprint can't MITM the channel (fail-closed).
  - **Fail-closed gate**: an unauthenticated remote request can never reach owner
    data or control endpoints; the relay forwards only `GET` static assets +
    `/api/identity`, strips the owner cookie, and the Pi's tunnel marker blocks any
    LAN-bypass on a tunnelled request. The LAN enrollment PIN is LAN-only.
  - **Operator-pinned home site** (`-home-pubkey`): the public home host refuses to
    run trust-on-first-use, so a racing attacker can't claim it across relay
    restarts.
  - **Blind TURN** (optional) as a ciphertext-only fallback for hard-NAT/CGNAT
    peers — costs zero trustlessness.
  - DoS-resilience on the relay: per-source-IP signaling throttle (Cloudflare-aware
    via `-trust-cf-ip`), nonce-keyed signaling mailbox, fast unauth-peer reap, and
    principal-bound poll secrets.

### Patch Changes

- b586f09: Dashboard: route every state-changing owner/CONTROL `/api/*` call through the
  fail-closed strict transport (FIX-B).

  The owner-access ceremony pages already rode `ownerFetch` (strict P2P, fail-closed
  503 on a public / `/me/<site>` origin with no DataChannel, raw relay fetch ONLY on
  a genuine LAN). This extends the SAME behaviour to the dashboard's classic scripts

  - web components so a state-changing call's body + owner session can never traverse
    the untrusted relay in cleartext on the public home route:

  * `p2p.js` now exposes `window.ownerFetch = p2pFetchStrict` — the dashboard's one
    shared, fail-closed entry point (not a fork; the identical function the ceremony
    pages use).
  * Converted the remaining bare state-changing calls: config save / restart
    (`settings.js`), self-tune start/cancel (`models.js`), load-twin profile switch
    - PV/load twin reset (`twins.js`), MPC replan (`plan.js`), EV-charger probe /
      Tesla verify / driver test (`settings/tabs/devices.js`), battery + PV manual-hold
      install/clear, pair start/abort, notification test, self-update trigger, and the
      update-badge snapshot-delete + skip/unskip/rollback/update POSTs.
  * Read-only GETs stay plain (no body; the relay strips the owner cookie on the
    P2P-only route, so they can't leak).
  * New web tests: a static guard that fails if any public-route module bare-fetches
    a state-changing `/api` call, plus an `ownerFetch` wiring test. The tier-2 docker
    e2e gains a `window.ownerFetch` fail-closed step + a relay-leak tripwire.

- b586f09: State: skip the boot integrity check when the DB is already known-good so a large
  `state.db` restarts in seconds instead of minutes.

  `heal.go`'s boot-time `PRAGMA quick_check` scans the entire file, which on a
  multi-GB `state.db` is minutes of disk I/O on a Pi — and it ran on every boot,
  making a restart look like a hang. Now a persistent `<db>.clean` "verified-good"
  marker is armed by `Open` after a successful open, and `openChecked` skips the
  boot check whenever it is present. On a 1.3 GB field DB the integrity gate went
  from >5 min to ~40 ms (measured).

  The marker is deliberately NOT a clean-shutdown flag: it persists across both
  clean shutdowns and crashes (a crash doesn't corrupt a WAL database, so it must
  not force a slow re-scan), so fast restarts never depend on how the process
  exited. The only thing that removes it is `Store.VerifyInBackground` finding real
  corruption — that runs the full check off the startup hot path after the app is
  already serving, and on failure removes the marker so the next boot runs the full
  check and heals from snapshot. At-rest SD-card rot is therefore still caught,
  recovery just takes the next boot instead of blocking this one. The background
  scan is cancellable (`Close` interrupts it via `sqlite3_interrupt`) so a shutdown
  isn't blocked by an in-flight multi-minute scan. Startup phases are now timed in
  the logs (`integrity gate complete elapsed=…`, `migrations complete elapsed=…`)
  so a slow phase is visible instead of a silent gap. See `docs/fast-restart.md`.

## 0.115.0

### Minor Changes

- dcacd18: Owner remote-access + relay hardening (pre-release security pass) — closes the
  home.\* exposure and the issues a multi-agent audit surfaced around it.

  **Security**

  - **Authenticated relay registration.** `POST /me/register` is now ES256-signed:
    the Pi signs `(site_id, host_id, ts)` with its self-sovereign site identity and
    the relay verifies it, pins the key per site (trust-on-first-use, or an
    operator-provisioned `-home-pubkey` for the internet-exposed home host), and
    refuses a conflicting key or a stale timestamp. Previously any internet client
    could repoint a site's tunnel mapping to a host it controlled (owner-session
    theft + dashboard MITM).
  - **No friend-flow owner escalation.** The friend pair-flow reverse-proxies from
    loopback and must never inherit owner authority. Owner-credential management
    (enroll an additional passkey, list/delete devices) and pairing control
    (`/api/pair/start`, `/api/pair/abort`) now require a real passkey session or a
    genuine private-range LAN source — never the loopback bypass — so a temporary
    friend grant can't be escalated into a permanent owner passkey, and a friend
    can't lock the owner out by deleting their passkeys. The PIN-less LAN bootstrap
    and the enrollment-PIN endpoint were already source-checked; this extends the
    same discipline to the post-bootstrap credential surface. The owner-remote gate
    continues to key off the unforgeable tunnel marker, regression-guarded by the
    end-to-end `TestOwnerGateThroughRelay`.
  - **Mandatory home-key pin.** The relay refuses to run a public home host
    (`-home-host`/`-home-site`) without `-home-pubkey`, so the internet-exposed home
    route is never left in trust-on-first-use mode (claimable by a racer after a
    relay restart); `-home-allow-tofu` is an explicit testing-only override.
  - **Correct passkey RP-ID default.** The WebAuthn RP-ID now defaults to
    `home.fortytwowatts.com` (the origin the owner visits) instead of the relay
    host, so a deploy that forgets the env var no longer enrolls passkeys bound to
    the wrong, unusable origin (a one-way door).
  - **Bounded request bodies.** Every relay request body is capped (with a tighter
    ceiling on the small unauthenticated control endpoints), and the Pi's WebAuthn
    finish handlers bound the attestation/assertion body, closing memory-exhaustion
    vectors on the public JSON surfaces.
  - **Relay reflected-XSS fix.** The pair-session landing page now charset-validates
    the routing token on registration and JSON-encodes it into the page, so a token
    planted via the open `POST /tunnel/register` can't break out of the page.
  - **On-host liveness.** Loopback `GET /api/health` probes (deploy/CI/docker
    HEALTHCHECK) are exempt from the gate without exposing health detail remotely.

  **Robustness**

  - Relay caps tunneled body size, bounds each forwarded request with a timeout so a
    dead-but-registered host fails fast instead of pinning a goroutine, and GCs
    expired/revoked pair tokens.
  - `home.*` now serves a calm, self-contained **offline page** (with auto-retry)
    when the Pi is offline or hasn't checked in recently, instead of a raw timeout.

  **Onboarding & UX**

  - A persistent **"Run setup wizard"** control in the dashboard (re-run setup
    without a fresh install), an in-UI **"Show enrollment PIN"** affordance on the
    LAN (with copy + live countdown) so first-passkey enrollment isn't a dead end,
    and `/setup?step=N` deep-links now navigate to the requested step.
  - **EV charger setup fix:** the provider dropdown is populated and the
    username/field-id mismatch that made the whole EV section non-functional is
    corrected.

  **Further hardening (independent review)**

  - Owner-credential surfaces fully closed to the friend pair-flow: the P2P offer
    (`/api/p2p/offer`) now uses the strict authorizer (a friend could otherwise
    open a WebRTC DataChannel that outlives the grant), and the ftw-pair
    friend-proxy refuses to forward `/api/pair/*` and `/api/owner-access/*` (so a
    friend can't forge the owner's pair-card or probe owner-access).
  - Relay/Pi memory bounded against unauthenticated floods: the token registry
    clamps TTL + caps live tokens, the owner registry caps TOFU sites and GCs
    stale ones (never the pinned home site), the tunnel queue no longer leaks
    waiter-map entries per polled host_id, and the Pi caps in-flight WebAuthn
    ceremonies and the landing-page hit counter.
  - Deploy docs corrected for the shipped RP-ID cutover (now
    `home.fortytwowatts.com`), the ES256-signed `/me/register`, and the
    `-home-pubkey` requirement — the runbook previously instructed the wrong,
    one-way-door RP-ID.
  - The friend-proxy denylist now normalizes (decodes + cleans) the path before
    matching, so `/api/%70air/status`-style percent-encoding can't smuggle a
    request past it; the relay's token registry evicts the oldest _unapproved_
    token at capacity (a flood can't lock out real pair sessions) and the tunnel
    queue caps total parked long-poll waiters against an unauthenticated flood.

  Note for operators: upgrade the relay and the Pi together — the hardened relay
  requires the signed registration the updated Pi sends.

### Patch Changes

- 36c2404: Fix dashboard stalls on late-day loads by aggregating the `/api/status`
  energy-today totals in SQLite instead of loading every history sample
  since midnight into Go on every 2-second status poll.
- f6aca48: Dashboard: fix the 5–10 s first-load stall and collapse duplicate request
  storms — three related changes to how the live dashboard talks to the backend.

  **P2P no longer stalls the first paint.** The Phase 5 P2P transport
  (`window.p2pFetch`) awaited the WebRTC handshake on the first request, so the
  first `/api/status` poll — which gates the whole live render — blocked on the
  8 s `CONNECT_TIMEOUT_MS` before falling back to plain `fetch`. Two fixes:
  (1) `p2pFetch` is now non-blocking — it uses the DataChannel only once it's open
  and otherwise serves the request over the relay immediately while connecting in
  the background, so no request ever waits on the handshake (on the relay path
  either); (2) P2P is skipped entirely on a direct-LAN connection — detected by
  host (`isDirectLAN`: localhost, private/CGNAT IP, single-label or `*.local`
  name), not by the pathname. The bare-host relay (e.g. `home.fortytwowatts.com`)
  is a public FQDN reached through the relay, so it is correctly treated as a
  remote context and keeps P2P — the earlier `apiBase() === ""` gate wrongly
  disabled it there. On a direct-LAN visit the transport indicator stays hidden
  instead of showing an un-toggleable "Relay" badge.

  **Live 24 h history is deduped.** `/api/history?range=24h&points=288` was
  fetched on boot, the 1-min poll, and every (undebounced) window resize, so a
  first-load layout resize storm fanned out into many identical requests. A small
  in-flight-coalescing + 15 s-TTL cache (`fetchHistory`, mirroring
  `ftw-history-card`'s `dailyFetchCache`) now shares one payload across those
  triggers; the periodic poll forces a fresh sample.

  **Notification-history badge is deduped.** `<ftw-notif-history>` now shares one
  in-flight request and a short-TTL cache for `/api/notifications/history` across
  the badge poll and modal open, collapsing transient bursts to a single request.
  The modal's manual Refresh button forces a fresh fetch, and non-OK responses are
  never cached.

- 64a6fe7: Relay: authenticate the host long-poll with a per-host **poll token** — closes
  the `host_id`-race flagged during the owner-access hardening review.

  `POST /me/register` (ES256-signed) and `POST /tunnel/register` now return a poll
  token that the Pi / pair sidecar must present (header `X-FTW-Poll`) on
  `GET /tunnel/{host_id}/next` and `POST /tunnel/{host_id}/response/{req_id}`. The
  relay verifies it constant-time and rejects unknown-host / wrong-token polls. So
  a caller that merely learns a host's `host_id` can no longer poll for (and steal)
  its tunneled traffic — which carries the owner's session cookie — and an
  unregistered `host_id` can't create long-poll waiters at all. Tokens are minted
  on the verified registration, refresh on re-registration (so they survive a relay
  restart re-mint), and are GC'd after going unused.

  Operators: upgrade the relay and the Pi together — the hardened relay requires
  the token the updated Pi/sidecar sends.

## 0.114.0

### Minor Changes

- 2a67660: Owner remote access — **LAN-PIN first enrollment**. A short-lived 6-digit PIN,
  readable only on the Pi's local network (`GET /api/owner-access/enroll-pin` —
  `403` over the relay) and printed to the Pi's console, authorizes the very
  first passkey enrollment through the relay origin. This resolves the deadlock
  between the WebAuthn RP-ID origin requirement (the first passkey must be
  created at `relay.fortytwowatts.com`) and the bootstrap hardening that blocks
  un-authenticated first-enrollment over the tunnel. `enroll.html` gains an
  optional PIN field. Once one passkey exists the PIN path is inert (further
  enrollment requires a logged-in session).
- 7efc9b3: Owner remote access — single-home **`home.fortytwowatts.com`** cutover. The relay
  gains `-home-host` / `-home-site` flags that forward a bare host (e.g.
  `home.fortytwowatts.com`) verbatim to the single owner Pi, so the dashboard loads
  at the clean root URL with working absolute asset paths (no `/me/<site_id>`
  prefix). The Pi auth-gate is refined to keep static assets (CSS/JS/images) public
  so the login page renders styled, while `/api/*` and the dashboard HTML shell stay
  gated.
- 14f964f: Owner remote access — passkey foundation (home route, Phases 1–3):

  - **Safe floor:** a per-process unforgeable tunnel marker excludes relay-tunnelled
    (remote) requests from LAN-bypass, and a global auth-gate wraps the whole mux —
    remote hits now require a passkey session, while genuine LAN/loopback stays
    frictionless. First-enrollment bootstrap is denied over the tunnel (LAN-only).
  - **Identity spine:** every Pi generates an always-on self-sovereign ES256 identity
    on first boot (Nova reuses it when federation is enabled); `GET /api/identity`
    exposes the public key; the owner's WebAuthn identity is now a stable opaque
    wallet handle decoupled from the mutable site name (rename-safe).
  - **Usernameless login:** discoverable resident-key passkeys + Conditional-UI
    autofill (no username — just Face ID / Touch ID / Windows Hello) with a button
    fallback, plus a backup-passkey recovery nudge.

- ab238ee: Home route Phase 5: **direct browser↔Pi P2P transport**. The dashboard at
  home.fortytwowatts.com now opens a direct, DTLS-end-to-end-encrypted WebRTC
  DataChannel to the Pi and routes its live `/api/status` poll over it, bypassing
  the relay on the data path. A `Direct / Relay` indicator in the header shows
  which transport is live; if the DataChannel can't open (hard NAT, no STUN
  reachability) it falls back to the relay fetch invisibly.

  - **Signaling rides the existing authenticated owner tunnel** — `POST
/api/p2p/offer` is owner-gated, so only an authenticated owner can open a
    channel. No relay changes.
  - **Pi side**: `p2p.Manager` answers SDP offers and serves the channel with a
    `Bridge` over the local API mux; pure Go (`pion/webrtc/v4`, no CGo), with
    PeerConnection lifecycle reaping and a connection cap.
  - The DataChannel carries the existing `tunnel.TunneledRequest/Response`
    frames, so the Pi's mux is unchanged. The data plane is ciphertext even over
    a future TURN relay — closing the "cloud sees plaintext" gap for P2P-routed
    traffic. STUN-only for now; TURN deferred.

- e0eb84c: Owner remote access: **persist sessions** to `state.db` (a new `owner_sessions`
  table) so a Pi restart no longer signs you out — the in-memory session map is
  restored on boot. And the owner-access landing now **manages passkeys** when
  signed in: list your enrolled passkeys, remove (revoke) one, or add a device.
- 3702a27: **Relay: the 4-digit code is now a one-time exchange for a session grant,
  not a standing password.** Previously, once a pair session was approved,
  anyone who got hold of the `/h/<token>/…` URL had full access for the
  rest of the TTL — and for MCP that means powerful tools
  (`run_command`, `modbus_write`, `deploy_driver`, `write_file`). A
  forwarded or leaked-from-history URL was effectively a host handover.

  Now, accepting the code mints a high-entropy session grant (32 bytes,
  CSPRNG). It is handed to the friend exactly once:

  - **MCP**: the landing page prints
    `claude mcp add ftw-friend --transport http <url>/h/<token>/mcp --header "Authorization: Bearer <grant>"`.
    `/h/<token>/mcp` now requires that Bearer grant.
  - **Browser/dashboard**: approval sets an `HttpOnly; Secure;
SameSite=Strict` `ftw_grant` cookie scoped to the session path;
    `/h/<token>/web/…` now requires it.

  A leaked-but-already-active URL is useless without the grant — the
  recipient lands back on the code-entry page and doesn't have the
  out-of-band 4-digit code (5 wrong tries still locks it). The grant is
  validated constant-time, never forwarded to the host, and expires with
  the session. `POST /h/<token>/approve` now responds `200 {"grant":"…"}`
  instead of `204`.

  Works on the existing path-based routes — no subdomains or new domain
  required (the browser-dashboard _rendering_ fix and any subdomain work
  remain deferred; see `docs/goals/relay-subdomain-sessions.md`).

### Patch Changes

- 623a998: Fix the e2e pair-flow tests (`TestPairFlow`, `TestPairFlowThroughRelay`) so
  they bind a dynamic API port instead of a hardcoded `:8080`. On a machine
  where `:8080` is already taken (e.g. an OrbStack / docker control-plane
  publishing `0.0.0.0:8080`), the test's main service couldn't bind, `waitForAPI`
  silently latched onto the squatter, and the friend's request 404'd — a false
  "grant broken" failure. The tests now use the same `freePort` helper
  `stack_test.go` already relies on.
- 5aa164f: Home route Phase 5 groundwork: add the CI-verifiable P2P transport core
  (`go/internal/p2p`). A `Bridge` reads `tunnel.TunneledRequest` JSON frames off
  an open WebRTC `DataChannel`, replays each against the local HTTP handler, and
  writes back a `ResponseFrame` — the same tunnel protocol the relay long-poll
  uses, so the Pi's mux is unchanged. Proven by an in-process pion↔pion loopback
  test (DTLS DataChannel, no browser/network). Pure-Go (`pion/webrtc/v4`, no
  CGo). Not yet wired to any user-facing surface — the relay signaling endpoints
  and browser `p2pClient` are later slices that need a browser harness.
- c333139: Persist the WebAuthn `BackupEligible` / `BackupState` credential flags on enrolled
  passkeys and restore them at login. Without this, go-webauthn rejected logins from
  synced / backed-up passkeys (iCloud Keychain, Google Password Manager) with
  "BackupEligible flag inconsistency during login validation" — the stored credential
  reported BE=false while the live assertion reported BE=true. Existing flag-less
  credentials must be re-enrolled.
- e91bbea: Fix the owner-access sign-in page throwing "OperationError: A request is
  already pending." The page started a Conditional-UI (autofill) WebAuthn
  ceremony on load and a second one on the "Sign in with passkey" button click
  without cancelling the first — browsers allow only one credential request at a
  time (a password manager like Bitwarden grabbing the autofill slot makes the
  collision near-certain). The page now tracks an `AbortController` and aborts any
  in-flight ceremony before starting the next, so the button and autofill no
  longer collide.
- 5a6d1be: Owner remote access: add a real server-side **sign out**. The `ftw_owner`
  session cookie is HttpOnly, so the landing page's old client-side
  `document.cookie` clear never actually logged you out — the session stayed alive
  on the Pi. New `POST /api/owner-access/logout` revokes the session both in
  memory and in the persisted store and expires the cookie; the landing's
  Sign-out button now calls it. `whoami` also returns `can_sign_out` (false on
  LAN-bypass) so the dashboard can show a Sign-out control only on a real remote
  session.
- 75f4579: Owner remote access hardening (security review): (1) deleting a passkey now
  revokes its active sessions immediately, so revoking a lost device actually logs
  it out instead of leaving its session alive until the 24 h TTL; (2) the LAN
  bootstrap enrollment PIN is burned after 5 wrong guesses, so its 6-digit space
  can't be brute-forced within the 10-minute window.
- 2d7f3f1: ui: dashboard banner when the database auto-recovered from corruption

  The dashboard now reads the `storage` field from `GET /api/health` (added in
  the two-tier storage work) and shows a dismissible amber banner when
  `state.db` or `cache.db` was found corrupt and healed at boot — e.g. "cache.db
  was corrupt — rebuilt empty, re-fetching" or "state.db … restored from last
  snapshot". Closes the loop so DB corruption is visible at a glance instead of
  only in the logs.

- 59e33aa: Fix a relay tunnel **poll-waiter** bug: a timed-out long-poll left a dead waiter
  channel in the queue, so a later request was handed off to that dead poller and
  **silently dropped — hanging the caller forever** (the remote dashboard would
  "just load" once a host had idled long enough to accumulate dead waiters).
  Timed-out and cancelled waiters are now removed from the queue, and the channel
  is drained first so a request handed off in the race window is never lost.

## 0.113.0

### Minor Changes

- bba0d1a: prices: implement the ENTSO-E day-ahead XML parser

  The `entsoe` provider previously returned `"entsoe: XML parser not yet
implemented"` for every fetch — selecting it in Settings → Price (or as a
  fallback when elprisetjustnu fails) silently produced no prices at all.

  It now decodes the A44 `Publication_MarketDocument` (TimeSeries > Period >
  Point), handling both PT60M and PT15M resolutions and the sparse
  carry-forward representation, and converts EUR/MWh to the configured
  currency per kWh via the existing FX converter (ballpark 11.5 SEK/EUR when
  rates aren't wired). A day the auction hasn't published yet returns no
  rows, mirroring the elprisetjustnu path so the hourly scheduler just
  retries.

- 285cca0: state: resilient two-tier storage with auto-heal

  Disposable, re-fetchable data (spot prices, weather forecasts) now lives in a
  separate `cache.db`, isolated from the precious `state.db` (trained models,
  energy history, device identity). At boot each database runs `PRAGMA
quick_check`: a corrupt `cache.db` is quarantined and rebuilt empty
  (re-fetched within the hour); a corrupt `state.db` is restored from a daily
  recovery snapshot, or quarantined and started fresh if none exists. Every
  recovery is surfaced on `GET /api/health` under `storage`, so DB corruption is
  never a silent, blank-dashboard failure again. Existing installs migrate
  automatically — `prices`/`forecasts` move from `state.db` to `cache.db` on
  first boot.

### Patch Changes

- e046eb0: ui(flow): drop the "· charging/discharging" suffix on battery nodes

  The battery node in the energy-flow view showed `target −83 W · discharging`,
  which overflowed the node circle. The suffix is now removed — the live W value
  and SoC% already convey direction — so the label reads just `target −83 W`.

## 0.112.0

### Minor Changes

- 3e24a6e: **Settings UI: expose `pv_forecast_safety_k` on the Planner tab.** The
  downside-PV safety factor (v0.111.0) was config-only; it now has a "PV forecast
  safety (k)" field under Settings → Planner (default 1.0, with inline help).
  Operators can dial it down to 0 to use the full battery, or up to keep more
  reserve on uncertain days, without editing config.yaml.

### Patch Changes

- c359527: **planner_arbitrage: the battery now reactively covers a sudden load on a
  charge-from-PV-surplus slot.** Previously, when the DP planned to "absorb PV
  surplus" this slot (a charge slot with `PlannedGridW ≈ 0` — charge from PV,
  not buy from the grid) and a large unforecast load came in, the battery sat
  idle at 0 W while the house imported the deficit from the grid, waiting for the
  slow reactive replan (60 s+ cooldown) to catch up. The existing PlannedGridW
  soft cap correctly _backed the charge off_ toward available PV, but floored at
  0 and never flipped to discharge, so the battery never supported the load.

  The soft cap's back-off may now go **negative (discharge)** on a
  charge-from-PV-surplus slot, driving projected grid back toward the plan's
  `PlannedGridW` (~0) — i.e. the battery covers the load the moment PV can't,
  instead of importing. This is the charge-side mirror of the existing
  discharge-slot cover-load carve-out.

  Three dispatch rails were aligned through a single `coverLoadChargeSlot`
  predicate so the discharge isn't undone downstream: the soft-cap floor,
  `planHasNonDischargeIntent` (so `noSelfDischarge` doesn't re-clamp it), and the
  plan/exec sign floor (so it isn't treated as a sign mismatch).

  **The same cover-load behaviour now also applies to `planner_arbitrage`
  _idle_ slots.** An idle slot (the DP planned neither charge nor discharge,
  expecting PV to cover the load) used to stay on the energy path — which yields
  a 0 W target and can't react — so a forecast miss left the site importing while
  the battery sat idle. Idle `planner_arbitrage` slots now fall through to the
  reactive PI / grid=0 path, the same one `planner_passive_arbitrage` idle slots
  already use: the battery discharges to cover a live import, and the existing
  live-export gate still prevents it from reactively absorbing a live PV surplus
  (the DP's idle choice is honoured on the charge side).

  Scope is deliberately narrow and safe:

  - Only `planner_arbitrage` (mirroring the existing `planner_passive_arbitrage`
    behaviour). `planner_cheap` idle slots keep the non-discharge block — only
    deliberate discharge slots are exempt there.
  - Charge slots: a deliberate grid-charge (`PlannedGridW` ≥ the 100 W import
    band) still floors at 0; only charge-from-PV-surplus and idle slots flip to
    reactive cover-load.
  - Normal sunny charge-from-surplus operation is unchanged (the cap only fires
    on a live import divergence; absorbing surplus is untouched).
  - The SoC floor, fuse guard, and slew limiter still bound the discharge.

  Does not change PV forecasting or any planner mode other than
  `planner_arbitrage`.

- 49a3046: loadpoint: detect CTEK NCRQ (car-side refusal) and stop allocating PV to a phantom EV sink

  When a vehicle hits its onboard SoC target mid-session, the CTEK driver reports
  `CHRG → NCRQ` ("No Charge Request") — the car has decided it's done, even
  though the cable is still plugged in. Before this fix `classify_state` had no
  branch for NCRQ, the loadpoint manager kept inferring a low SoC from the
  session's plug-in anchor, and the MPC kept allocating multi-kW of PV surplus
  to a sink that would never accept it. With a saturated home battery and no
  other dump load, the surplus spilled to the grid — sometimes at negative spot.

  The fix wires car-side refusal end-to-end:

  - `drivers/ctek_hybrid.lua` — `classify_state` recognises `NCRQ` and emits a
    new `request_active` flag in the EV table (false on NCRQ, true otherwise).
  - `internal/loadpoint` — `Manager.Observe` takes a `requestActive bool`. When
    the vehicle holds NCRQ past `NCRQCompletionThreshold` (90 s) on a session
    with a configured target, the inferred SoC pins to `targetSoCPct` and
    `SoCSource` becomes `"ncrq"`. The latch clears on plug-out only — a transient
    EVSE retry isn't enough to reopen the allocation.
  - `cmd/forty-two-watts` — `telAdapter` parses `request_active` from
    `DerReading.Data`, defaulting to `true` so non-NCRQ-aware drivers (Easee,
    Zap, etc.) keep their existing behaviour.

  The pinned SoC then flows naturally into `mpc.LoadpointSpec.InitialSoCPct`
  on the next replan: `InitialSoCPct == TargetSoCPct` means the DP allocates
  0 W to this loadpoint and the PV/battery curtail-vs-export trade-off no
  longer competes against a fictional sink.

- e0ba0bb: **A charge schedule now overrides the surplus 1-phase forecast lock.** On a
  cloudy day the surplus_only logic can pin a loadpoint to 1-phase for the whole
  day (`surplusLockedTo1P`, the "today's PV can't sustain 3Φ" verdict). That lock
  is sticky and was applied even when the operator had set a **deadline-driven
  charge schedule** that needs 3-phase grid power — so an "11 kW by 13:00"
  schedule was silently throttled to ~3.7 kW (1-phase) and could miss its target.

  Phase selection now puts an **active schedule first**: when a schedule SoC
  target is set, the charger is given the operator's explicit phase pin or
  `auto` (never forced to `1p` by the surplus optimisation), so a scheduled
  charge can use 3-phase. With no schedule, the surplus 1-phase lock and the
  30-minute near-term dwell verdict behave exactly as before. The precedence
  lives in a single pure `resolvePhaseMode` helper with a table-driven test.

## 0.111.0

### Minor Changes

- a129137: **Replace the SoC safety floor with downside-PV planning.** The MPC's forecast-
  risk reserve was a `soc_safety_floor_pct` (default 25 %) — a soft cost penalty
  that kept SoC above a percentage on PV-surplus slots. A percentage is the wrong
  unit (25 % of a 5 kWh battery and a 40 kWh battery hedge wildly different
  absolute risk), it couldn't be set low or disabled (`0` was forced back to
  25 %), and as a separate penalty it could fight legitimate "run down now, refill
  cheap later" decisions.

  The planner now instead optimises against **downside PV**: `PV_plan = forecast −
k·σ`, where σ is the live PV forecast-error std (the pvmodel residual std) and
  `k = pv_forecast_safety_k` (default 1.0; `0` disables the hedge). The DP no
  longer runs the battery down betting on PV that may not arrive, so a reserve
  _emerges from the live forecast uncertainty itself_ — large on variable cloudy
  days, ~zero on clear days, and naturally inert in winter / no-sun (so passive
  runs its charge-cheap / discharge-for-self-consumption loop down to the hardware
  floor). No separate magic floor; the robustness comes from the economics.

  **Config:** new `pv_forecast_safety_k` (pointer; unset → 1.0, explicit `0` →
  no hedge). `soc_safety_floor_pct` and `safety_floor_penalty_ore_kwh_hour` are
  deprecated — still parsed so existing config loads, but ignored with a warning.
  Remove them and set `pv_forecast_safety_k` instead.

## 0.110.0

### Minor Changes

- 34335cf: **Document and support running off a Mac mini or a generic Linux server.**
  The Docker stack already ran on any Linux box via `docker-compose.yml`,
  but that file uses `network_mode: host` — a Linux-kernel feature that, on
  macOS, binds to the Docker Desktop VM rather than the Mac, leaving the
  dashboard unreachable and silently breaking device discovery.

  - **`docker-compose.macos.yml`** — a self-contained macOS compose file
    that swaps host networking for bridge networking with published ports
    (`8080`, `1883`). The app reaches the embedded broker by service name
    (`mosquitto:1883`), and the `ftw-updater` sidecar is wired to the
    macOS file so the in-app Update/Restart buttons recreate the right
    containers.
  - **`scripts/install-macos.sh`** — one-shot macOS installer: verifies
    Docker Desktop is up, lays out `~/forty-two-watts`, fetches the macOS
    compose file + broker config, and brings the stack up. The Linux
    installer now bails early with a pointer when run on macOS.
  - **`docs/deploy-platforms.md`** — new guide covering both the generic
    Linux server path (Ubuntu/NUC/VM: install, `ufw`, device-identity
    caveats) and the Mac mini path (Docker Desktop networking caveats:
    point MQTT at `mosquitto`, use explicit driver IPs since mDNS/broadcast
    don't cross the VM boundary, keep-it-running tips). `docker-compose.yml`
    and `operations.md` now cross-reference it.

- f6935e4: **Add `site.max_export_w` — an opt-in site export ceiling below the
  physical fuse.** Some inverters trip into a protective fault on
  _sustained_ grid export well under the breaker rating: the Ferroamp
  EnergyHub faults to state `0x8030` after ~8 kW of continuous midday
  export (battery discharge stacked on PV surplus) and only recovers as PV
  wanes — losing hours of solar. Recurred daily on a live
  `planner_arbitrage` site whose plan discharged the battery into the
  morning price peak while PV was already exporting; grid voltage and
  frequency were both in spec at every trip, ruling out a normal grid
  protection.

  `max_export_w` (W, magnitude; `0` = disabled, the default) is enforced
  on two layers:

  - **Dispatch** — the fuse guard's export side now scales battery
    discharge against `min(fuse − margin, max_export_w)` via the new
    `(*State).effectiveExportCeilingW`, mirroring the import-side
    `effectiveImportCeilingW` / `peak_import_ceiling_w`. Hot-reloadable.
  - **MPC** — every plan slot's export limit becomes
    `min(FuseMaxW, max_export_w)` (`clampSlotGridLimits`), so the DP never
    _schedules_ a discharge that would over-export — fixing the root cause
    rather than only clamping at execution time. Applied at startup
    (parity with the existing per-slot fuse plumbing).

  Off by default; existing sites are unaffected until they set the knob.
  The full-battery, PV-only over-export case still needs PV curtailment —
  the discharge clamp can only scale battery action, not PV.

- b4d3db6: **Savings: baseline now includes EV charging priced at the day's average,
  so EMS-scheduled EV laddning shows up as savings instead of zeroing out.**
  Previously the `BaselineCostOre` returned by `state.DailyCostBreakdown`
  (and surfaced by `/api/savings/daily` as `baseline_cost_ore`) was
  `Σ slot ( house_load_w × spot_total )`, where `house_load_w` was
  explicitly the meter reading minus EV (see
  `main.go`'s `loadW := gridW − batW − pvW − evW`). Two consequences:

  1. When the EMS scheduled EV charging onto a near-zero spot hour, that
     energy contributed ~0 to baseline but the matching grid import still
     went into `ActualCostOre`. Saved-tal looked flat or even negative.
  2. When the EV was charged on a higher-priced hour (cold-start, no
     override), actual rose while baseline didn't move — the metric was
     systematically biased toward "lost" whenever the EV was active.

  The breakdown now treats EV separately:

  - `BaselineHouseOre` keeps the slot-by-slot house pricing (unchanged
    behaviour for the EV-less case).
  - `BaselineEvOre = EVWh × AvgImportOreKwh / 1000` prices the day's EV
    energy at the day's time-weighted average spot. Interpretation: "a
    dumb charger with no timing awareness would have paid the day's avg
    per kWh". Smart scheduling onto cheap hours then surfaces as savings;
    charging on a peak shows up as a penalty. Symmetric.
  - `BaselineCostOre = BaselineHouseOre + BaselineEvOre` (sum exposed for
    back-compat).
  - `EVWh` is derived per history sample as
    `grid_w − bat_w − pv_w − load_w` (clamped non-negative), the inverse
    of `main.go`'s identity. No schema change.

  The `/api/savings/daily` response gains `ev_wh`, `baseline_house_ore`,
  and `baseline_ev_ore` fields so the UI can render the EV share of
  savings separately. Existing fields (`baseline_cost_ore`, `saved_ore`,
  `flat_cost_ore`) keep their names; their values now incorporate the EV
  term.

  Historical days will re-render with the new baseline once a process
  restart clears the savings cache; volume columns are unchanged.

### Patch Changes

- 8df5c11: Fix backend safety edge cases around driver default timeouts, stale site-meter fallback, loadpoint surplus-only persistence, and MPC idle action selection with asymmetric power limits.
- 1cca922: **Easee driver: pause+resume the contactor on a live phase flip so 1Φ→3Φ
  actually takes effect.** The Easee only latches its phase count when a session
  (re)starts — writing `phaseMode=3` while a session is actively charging at 1Φ
  leaves the contactor on a single phase, so a loadpoint that crossed from 1Φ to
  3Φ (e.g. a schedule ramping to 11 kW) stayed throttled to ~3.7 kW. Field-
  confirmed: only a manual pause+resume flipped it.

  The driver now pauses charging before writing the new `phaseMode` on a real
  mid-session flip (`last_sent_phases` already set); the existing auto-resume
  (offer > 0 while paused) re-closes the contactor on the new phase count. The
  first command of a session is unaffected (no live contactor to recycle).

- 32c238e: **surplus_only EV charging: smooth the step setpoint so the EV and home
  battery stop fighting over the same PV surplus.** The surplus*only setpoint
  magnitude tracked the \_instant* surplus and snapped to an `allowed_steps_w`
  step every 5 s tick. Because `surplusW = −gridW + batW + evW` counts the home
  battery's current charge power as EV-available, a single-tick wobble (the
  battery briefly backing off, a cloud edge, a load twitch) ratcheted the EV up
  a step it couldn't hold — it collapsed the next tick, and the repeated
  multi-kW load swing whipsawed the home battery's reactive PI into integrator
  windup, so the battery stopped delivering its planned discharge (an EV↔battery
  limit cycle; observed live as `ev_w` swinging 0–4.7 kW and the battery
  under-delivering to ~4% of plan).

  The step setpoint now uses **asymmetric smoothing**: down-steps still track the
  instant surplus (the no-import promise is unchanged), but an **up-step is gated
  on the rolling average** — the EV only climbs to a higher step when the smoothed
  surplus sustains it. This breaks the limit cycle: the EV ramps up only on a
  genuine surplus rise and the home battery's PI stays stable. Pause/resume
  hysteresis and the no-import guarantee are untouched.

- 990457e: fix(mpc): include planned EV loadpoint power when computing PV curtailment limit

  `annotateCurtailment` previously only considered house load + battery charge when deciding how much PV can be safely absorbed locally before recommending `pv_limit_w`. When the planner had scheduled EV charging (`LoadpointW > 0`) in a negative-export-revenue slot, the limit would be too low and a curtailment-capable driver could starve the EV session the DP itself had chosen.

  The fix adds `max(0, LoadpointW)` to the local-consumption total, matching the accounting already used for battery charging. Updated godoc, docs, and added regression test.

  This only affects sites using both planner strategies that can produce export + a PV-curtailment-capable driver + configured loadpoints.

- 4f2e204: Fix dashboard UI state regressions around settings edits, notification history, history cakes, and the Plan Today horizon.
- 9f10e91: fix(loadpoint): reactive per-phase fuse clamp for the EV charger

  The site-level fuse guard only protects the three-phase _total_ — a single
  phase can still trip from house-load imbalance (a vacuum, kettle or oven on
  one leg) stacked on top of the EV's per-phase draw, which forced manual
  ramp-downs in the Tesla app. The loadpoint now reads the site meter's live
  per-phase currents (`meter_l1_a/l2_a/l3_a`) and reactively caps the EV's
  `max_amps_per_phase`: the worst phase drops by the full overage the instant
  it nears the breaker, and recovers at 1 A/tick once there is headroom
  (fast-down / slow-up servo, deadband below the limit). Pure, table-tested
  `nextFusePhaseCapA`; clamp disabled cleanly when per-phase telemetry is
  absent.

## 0.109.0

### Minor Changes

- af6435c: **Relay: the 4-digit code is now a one-time exchange for a session grant,
  not a standing password.** Previously, once a pair session was approved,
  anyone who got hold of the `/h/<token>/…` URL had full access for the
  rest of the TTL — and for MCP that means powerful tools
  (`run_command`, `modbus_write`, `deploy_driver`, `write_file`). A
  forwarded or leaked-from-history URL was effectively a host handover.

  Now, accepting the code mints a high-entropy session grant (32 bytes,
  CSPRNG). It is handed to the friend exactly once:

  - **MCP**: the landing page prints
    `claude mcp add ftw-friend --transport http <url>/h/<token>/mcp --header "Authorization: Bearer <grant>"`.
    `/h/<token>/mcp` now requires that Bearer grant.
  - **Browser/dashboard**: approval sets an `HttpOnly; Secure;
SameSite=Strict` `ftw_grant` cookie scoped to the session path;
    `/h/<token>/web/…` now requires it.

  A leaked-but-already-active URL is useless without the grant — the
  recipient lands back on the code-entry page and doesn't have the
  out-of-band 4-digit code (5 wrong tries still locks it). The grant is
  validated constant-time, never forwarded to the host, and expires with
  the session. `POST /h/<token>/approve` now responds `200 {"grant":"…"}`
  instead of `204`.

  Works on the existing path-based routes — no subdomains or new domain
  required (the browser-dashboard _rendering_ fix and any subdomain work
  remain deferred; see `docs/goals/relay-subdomain-sessions.md`).

### Patch Changes

- ce92b4a: **Fix: relay landing page rejected every approval code as "Wrong code"
  even when the friend typed the right one.** The `fmt.Fprintf` that
  renders the landing HTML in `ftw-relay`'s `publicLanding` passed format
  arguments in the wrong order, so the embedded JS `const TOKEN` was
  populated with the token state (`"pending"`) instead of the actual
  session token. The Activate button then POSTed to
  `/h/pending/approve`; the relay couldn't find that token and returned
  `403 Forbidden`, which the page surfaced as "Wrong code" regardless of
  what was typed. As a side effect "From:" showed the token, "Intent:"
  was empty, and "State:" showed the intent.

  Argument order is now `as → intent → state → token`, matching the
  positional verbs in `landingHTML`. A regression test
  (`TestLandingPageTokenConstMatchesPath`) pins the JS const + each label
  row so a future reshuffle can't silently regress the approve POST path.

## 0.108.2

### Patch Changes

- 0779ff2: **Hardening: cover-load and passive-arbitrage-idle carve-outs now reset stale
  energy-path bookkeeping on every tick they fire**, mirroring what
  `preparePlannerSelf` already does for `planner_self`.

  Without this, `slotDelivered` / `lastTickTs` / `currentDirective` could
  carry leftover state from a prior energy-path tick into the carve-out
  window. A subsequent transition back to the energy path within the same
  15-min slot (e.g., a mid-slot replan flipping the slot's intent, or an
  operator mode-hop) would then read those stale values and miscompute
  `remainingWh`. Same forward-transition risk that `planner_self` has
  guarded against since PR #131.

  No new behaviour, no signal change in the steady-state cover-load reactive
  path — purely defence-in-depth for plan-refinement / mode-transition
  scenarios. Two regression tests pin the bookkeeping reset for both the
  `planner_arbitrage` cover-load and the `planner_passive_arbitrage` idle
  carve-outs.

## 0.108.1

### Patch Changes

- 1160393: fix(pvmodel): MPC now consumes the unanchored structural PV predictor so the rolling residual correction (PR #381) is not applied twice. Previously `mpcSvc.PV` was wired to `pvSvc.Predict`, which already folds in the live-vs-model now-anchor; combined with `PVResidualCorrect` the planner saw the structural-vs-live bias subtracted twice and could plan as if PV was ~0 W on a sunny day with a heavy downward residual. A new `pvmodel.Service.PredictStructural` returns the RLS-only prediction; the anchored `Predict` is kept for UI overlays and dispatch's live-reading path.

## 0.108.0

### Minor Changes

- b887541: **PV twin now applies a short-horizon residual correction on top of the
  structural RLS prediction.** The RLS model's forgetting factor (~3h
  half-life @ 60s cadence) is tuned to learn site orientation, shading
  and slow soiling drift; it does not respond fast enough to "today's
  persistent NWP bias" — e.g. when measured cloud cover is heavier than
  the forecast assumed for the last 90 minutes, structural predictions
  stay biased high while RLS chews through the samples needed to adapt.

  The new layer keeps a 2-hour rolling buffer of (predicted_at_t,
  actual_at_t) pairs, computes the mean residual, and applies it as an
  additive bias to MPC slot predictions, fading linearly over a 2 h
  horizon (full correction ≤ 30 min, zero by 120 min). Beyond 2 h the
  structural model is again the best estimate — weather fronts roll in,
  time-of-day shifts, and the residual is no longer relevant.

  Gates (`go/internal/pvmodel/residual.go`):

  - ≥ 20 samples in the 2 h window before any correction applies.
  - `|mean residual|` ≥ 25 W → otherwise treated as "no bias detected".
  - `std / |mean|` ≤ 1.0 → variance-dominated streams are skipped.
  - `dt ≤ 0` (past slot) → factor = 0.

  Wiring: `pvmodel.Service.ResidualCorrect` is plumbed into
  `mpc.Service.PVResidualCorrect` (new optional hook). The planner calls
  the corrector on the slot midpoint inside `buildSlots`, after the twin
  prediction and before `selectPlannerPVW` blends with the NWP forecast.
  A nil hook is a hard no-op, so existing wiring without the corrector
  is unchanged.

  **PV only**: load is multimodal (appliances cycling) and a rolling-mean
  correction can chase the noise. Variance gate would catch it most of
  the time, but risk/reward is poor without dedicated diagnostics.
  Revisit when load observability lands.

  Diagnostics exposed via `GET /api/pvmodel`:
  `pv_residual_correction_w` (the value the planner would apply 15 min
  out), `pv_residual_sample_count`, `pv_residual_mean_w`,
  `pv_residual_std_w`, `pv_residual_window_minutes`.

- 2ff3d09: feat(mpc): tighter replan triggers + twin-driven replan signal

  Tightens the reactive replan thresholds (PV 500→250 Wh, load 400→200 Wh,
  half-life 15→8 min, cooldown 60→30 s) and adds a third trigger that fires
  when the PV or load twin's CURRENT prediction has shifted materially (RMSE

  > 250 W PV / 200 W load over the next 16 slots) from the prediction the
  > active plan was built on.

  The twin already self-corrects every cycle through RLS; the planner only
  consumed its output every 15 min. The new signal closes that gap without
  waiting for the integral-of-error to accumulate. Replanning is ~100 ms on
  a Pi 4 (51 × 21 × 193 DP cells, sub-1 % CPU) — being stingy was the wrong
  default.

### Patch Changes

- 55fb0c3: **Codex review follow-ups for v0.107.0** — fixes 2 P1 and 2 P2 review
  findings on the dispatch / loadmodel changes shipped in v0.107.0.

  **P1: Heating coefficient survives restarts.** `main.go` had been calling
  `loadSvc.SetHeatingCoef(cfg.Weather.HeatingWPerDegC)` at startup, which
  unconditionally overwrote any value persisted from previous training.
  After every binary update the adaptive fit was thrown away. New
  `SeedHeatingCoef(w)` only writes the value when the model has no samples
  yet — operator config is the cold-start prior, observation drives the
  value once learning has begun. `SetHeatingCoef` remains for explicit
  operator overrides.

  **P1: Cover-load carve-out actually chases grid=0.** The PR #378
  carve-out only set `useEnergyPath = false`; in production `main.go` wires
  both `SlotDirective` and `PlanTarget`, so the code fell into the legacy
  `!useEnergyPath` branch and called `SetGridTarget(plannedImportW)` —
  chasing the planned positive import instead of grid=0. Result: cover-
  load slot with a 1.7 kW planned import would back the battery off all
  the way to idle instead of covering live load. Fixed by forcing
  `SetGridTarget(0)` for carve-out slots and skipping the legacy
  PlanTarget block when a carve-out predicate fires.

  **P2: Live-export gate predicate tightened.** `passiveArbitrageIdleSlot`
  used `dir.BatteryEnergyWh <= idleWhGate`, which is true for _any_
  negative-energy slot (planned discharge). Tightened to
  `|BatteryEnergyWh| ≤ idleWhGate` so the predicate names what it does
  (true idle only). The planned-discharge case is now folded into
  `coverLoadDischargeSlot`, which was also extended to cover
  `planner_passive_arbitrage` (not just `planner_arbitrage`), and the
  live-export gate now fires on either predicate.

  **P2: SlotDeliveryStats catches sign mismatches.** Planned `-425 Wh`
  discharge vs actual `+425 Wh` charge would have scored `|actual| /
|planned| = 1.0` = "on target" — the largest possible miss, invisible
  on `/api/status`. New `SignMismatchCount` field fires when planned and
  actual have opposite signs (and both exceed the idle cutoff). The
  magnitude over/under counters then only fire on same-sign cases,
  keeping their semantics clean.

## 0.107.0

### Minor Changes

- adf3f86: **Fix: `planner_arbitrage` cover-load discharge slots now chase the live
  zero-grid line instead of rigidly running the planned discharge power.**
  When the DP picks a discharge slot to _offset expensive import_ (rather
  than to _export at peak price_), the energy-allocation path used to lock
  the battery at `remainingWh × 3600 / remainingS` regardless of live
  conditions — exporting at spot price any forecast-load undershoot and
  under-covering any forecast-load overshoot. The EMS now routes these
  slots through reactive PI on grid=0, the same path
  `planner_passive_arbitrage` non-charge slots and `planner_self`
  participant slots already use.

  Detection: `PlannedGridW > -100 W` (no significant planned export) AND
  `BatteryEnergyWh < -50 Wh` (discharge planned). Peak-export slots
  (`PlannedGridW < -100 W`) stay on the energy path — extra export there is
  bonus revenue at the price the DP picked the slot for. Charge slots
  stay on the energy path so deliberate grid-charge intent is honoured.

  Found 2026-05-28: plan estimated baseload 1.7 kW for a slot that scheduled
  the battery to be empty by 23:30. Real load was 0.9 kW; battery sat at
  -1.7 kW exporting 800 W at spot. Then load surged to 3.2 kW and the
  battery stayed at -1.7 kW, forcing 0.5 kW import. Both directions are
  now reactive — the slot's Wh budget guides where the battery is
  generally headed, the meter decides the instantaneous power.

- fdbf53c: **Load model now adapts the heating coefficient online from measurements.**
  Previously `HeatingW_per_degC` was operator-set and never moved — if the
  value drifted from reality (or the house turned out not to track outdoor
  temperature at all), forecasts silently inflated cold-day load and the
  MPC made decisions on phantom heating draw.

  The fit runs as one-parameter SGD on the prediction residual:
  `coef ← coef + α · err / deltaT`. Gated on `bucket.Samples ≥
MinTrustSamples` (residual derives the slope from the bucket baseline)
  and on `deltaT ≥ 3 °C` (warm samples and near-reference samples have no
  heating signal to extract). Clamped to `[0, 1500] W/°C`.

  The fit runs **before** the outlier filter so a wildly stale coefficient
  can recover — without that ordering, every cold sample under a wrong
  coef looks like an outlier vs the warm-day MAE and nothing could ever
  pull the value down.

  Operator config (`Planner.HeatingWPerDegC` / `SetHeatingCoef`) still
  seeds the initial estimate and is re-applied on
  `POST /api/loadmodel/reset`. From there observation drives the value.
  Households whose load is temperature-independent (district heating,
  solar-gain-dominated shoulder seasons, well-insulated homes) converge
  toward 0 W/°C.

  Found 2026-05-28 on site .40: planner predicted 2782 W load for a sunny
  May afternoon (actual 504 W). Root cause was the un-adapted heating
  term — `300 W/°C × (18 − 11.4 °C) = 1980 W` of phantom load applied
  without seasonal / solar-gain awareness. The dispatcher fix in #375
  prevents the _symptom_; this change addresses the _cause_.

- c1cbda7: feat(diagnostics): per-slot Wh delivery tracking for reactive dispatch paths

  Adds an independent per-slot Wh accumulator that runs on every dispatch
  tick regardless of which execution path was taken (planner_self, planner
  passive_arbitrage idle slots, the planner_arbitrage cover-load carve-out
  from PR #378, manual modes, plain self_consumption). At slot rollover
  the actual fleet delivery is compared against the plan's
  `BatteryEnergyWh`; ratios outside [0.5, 1.5] are logged and bump
  `SlotDeliveryStats.OverDeliveryCount` / `UnderDeliveryCount`, surfaced
  on `/api/status`. Idle slots (|planned| ≤ 50 Wh) are skipped — ratio
  against ~0 is meaningless.

  Pure observability — no dispatch decision reads the counters and no
  hard Wh cap is applied to reactive paths. The point is to measure
  first, decide on enforcement later.

### Patch Changes

- bdf2352: **Fix: `planner_passive_arbitrage` no longer absorbs live PV surplus into the
  battery on a plan-idle slot.** When the DP picked idle for a slot
  (`battery_w = 0`) and live conditions turn out to have more PV (or less
  load) than the forecast assumed, the dispatcher now holds the battery at
  0 and lets the surplus export — rather than collapsing to
  self-consumption and chasing `grid = 0` by ramping the charge up.

  The DP picks idle slots deliberately, often to preserve export revenue
  at the current spot when future PV is plentiful and future prices are
  lower. The old behaviour reactively swallowed that surplus because the
  fallback path was symmetric with self-consumption ("balance to zero"),
  which discards the DP's intent. The gate is the mirror of
  `plannerSelfExportSurplusGate`, but triggered on the **live** baseline
  grid (`grid_meter − Σ battery_w`) rather than the plan's forecasted
  grid — for the slot we're already in, live measurements override the
  (possibly-stale) forecast.

  Reactive discharge on live import is unchanged: a passive-arbitrage
  idle slot with the meter importing still allows the battery to cover
  the load. The change is one-sided — block reactive charging when the
  meter shows export potential the forecast missed.

  Found 2026-05-28 on a sunny May afternoon with a wildly over-estimated
  load forecast: planner expected ~2.8 kW load vs. actual ~0.5 kW, picked
  idle on net-≈0 forecast, and the dispatcher charged 2.6 kW into the
  battery despite high current spot (160 öre), low future spot (95 öre),
  and abundant forecast PV in upcoming slots.

## 0.106.0

### Minor Changes

- 9638c78: **Ferroamp self-healing watchdog for the sticky-pplim trap.** When the
  SSO reports the post-incident signature — DC bus voltage > 200 V, zero
  PV current, no fault, relay closed — continuously for ten minutes, the
  driver now auto-publishes `pplim arg=<pplim_release_w>` to release
  the lock. Operator opts in by setting `config.pplim_release_w > 0`;
  without it, the watchdog logs a per-incident warning but does not
  publish (we have no safe release value to send).

  A five-minute cooldown between successive recoveries prevents command-
  spam if the release doesn't take. A new `stuck_pv_recovery_count`
  metric tracks lifetime recovery count so operators can alert on a
  chronic condition.

  Reuses the existing `pplim_release_w` field — same value, dual
  purpose (dispatcher `curtail_disable` release AND watchdog
  self-recovery).

  Layered with [#367](https://github.com/frahlg/forty-two-watts/pull/367)
  (driver hard-fail on `pplim arg=0`) and the dispatcher fix in the
  parallel PR (`fix(curtail): no spurious release ...`) this is the
  third and final layer of defense against the 2026-05-27 brick.

### Patch Changes

- 312e9ba: **Defense-in-depth against the 2026-05-27 Ferroamp brick.** Two
  independent changes that, combined with PR #367's driver-side hard
  fail on `pplim arg=0`, eliminate every known trigger path:

  - **Dispatcher**: `ComputePVCurtail` no longer emits a `curtail_disable`
    release simply because a previously-curtailed driver dropped out of
    the proportional allocation due to its own `|PV|` crashing to ~0
    (often a direct consequence of OUR curtail throttling that driver
    down). The release is now only sent when the curtail directive
    truly clears, or the driver is removed from `SupportsPVCurtail`, or
    the driver goes offline. Also: per-driver allocations rounding to
    `≤ 1 W` are suppressed entirely — never publish a near-zero
    `pplim` that some inverters treat as a hard "limit to 0 W" lock.

  - **Ferroamp driver**: subscribes to `extapi/control/response`
    (was: `extapi/result` — wrong topic, never received anything),
    parses `{"status":"ack|nak", ...}` responses, and exposes
    cumulative `extapi_nak_count` + `extapi_ack_count` metrics. NAK
    responses are also logged as warnings with `transId` + `msg`
    fields. The 2026-05-27 brick was preceded by minutes of
    `nak: no available ESOs detected in system` that we couldn't see
    through ftw telemetry — now the operator can alert on any non-zero
    NAK rate.

  Tests added:

  - Four new dispatcher regressions in `control/pv_curtail_test.go`
    guarding the suppression / release semantics.
  - One driver test in `drivers/lua_ferroamp_curtail_test.go`
    asserting NAK + ACK counter advancement.

- 322ffe2: **Ferroamp safety fix:** the Lua driver now refuses to publish
  `pplim arg=0` from any `curtail` / `curtail_disable` path.

  Ferroamp's extapi treats `{"cmd":{"name":"pplim","arg":0}}` as
  "limit PV output to 0 W" — same wire bytes as a naive release would
  have, opposite semantics. The inverter sticks at 0 W PV until the
  operator clears pplim from the Ferroamp portal or power-cycles the
  EnergyHub. On 2026-05-27 this fired against a live SE4 site after the
  dispatcher's proportional curtail allocation gave a 0-share to
  Ferroamp; recovery required a 30+ minute outage and a portal-side
  reset.

  Changes:

  - `curtail` with `power_w <= 0` is now a logged no-op (was: published
    `pplim arg=0`).
  - `curtail_disable` is a logged no-op by default (was: published
    `pplim arg=0`). To restore automatic release, set
    `config.pplim_release_w` on the driver to the inverter's nominal
    max (e.g. `15000` for a 15 kW SSO). The driver then publishes
    `pplim arg=<release_w>` which Ferroamp accepts as "raise the limit".
  - New unit tests guard the wire payload against any regression that
    reintroduces `pplim arg=0`.
  - Docs in `docs/configuration.md` describe the trap and the new
    config field.

  Operators with `supports_pv_curtail: true` on Ferroamp **should** add
  `config.pplim_release_w: <SSO-rated-watts>` to keep curtailment
  auto-releasing. Without it, curtail still engages correctly, but
  release becomes a portal action.

## 0.105.0

### Minor Changes

- c206f4c: **Friend-types-code redesign of the pair-approval flow.** v0.104.0 shipped the code on both sides (dashboard + friend's landing page) and required the operator to type it back in — confusing UX, and the cross-origin POST from the LAN dashboard to the public relay was blocked by CORS so the Allow button silently did nothing.

  New flow:

  - Dashboard displays the 4-digit code along with the URL for the operator to share. "Copy code" and "Copy URL + code" buttons make the bundle easy to send in one Signal/SMS message.
  - The relay's landing page **no longer shows the code**. It shows an input field. The friend types the code they received separately from the host.
  - POST happens same-origin (browser → relay), no CORS surprises.
  - On success, the page reveals the dashboard URL + the `claude mcp add` one-liner.

  Security model is unchanged in substance — possession of (URL + code) activates the session — but the flow now matches the operator's mental model (share both, friend types code). The host no longer has to be live at connect-time to approve.

  Tests adjusted: relay landing-page test now asserts the code is **NOT** present in the served HTML; component source-hygiene tests assert the operator-side input field is gone. 31 node-tests + Go test suite all green.

## 0.104.0

### Minor Changes

- 8e2c08f: **Pair-card v2 with real relay presence + voice-channel approval.** When the friend opens the relay URL, the dashboard now surfaces the full URL with a Copy button, the 4-digit voice-channel approval code in big numbers, and an inline Allow form that POSTs the typed code straight to the relay's `/h/<token>/approve` once the operator hears the matching digits from their friend on voice. The misleading "0 clients connected" counter is replaced with a live presence indicator (live / active / idle / pending / dead) driven by a new `GET /tunnel/sessions/<token>/info` endpoint on the relay that tracks landing-page hits + last-tunneled-request timestamps; ftw-pair polls it every heartbeat and forwards the snapshot to `/api/pair/status`.

  The friend-message template is rewritten for the URL flow — no more `curl install-ftw-connect.sh` references, no more old binary install path. Operator-facing security: if the friend reads back a code that doesn't match the one shown on the dashboard, the validator refuses to approve and warns "leaked URL".

  Pure render helpers split into `web/components/ftw-pair-card-render.js` and covered by 42 `node --test` cases (state-machine snapshots, golden-string assertions on the friend message, source-hygiene checks that catch regressions where someone re-introduces `ftw-connect` references). Run with `npm test` from the repo root.

### Patch Changes

- cf93ada: Internal groundwork for owner remote access via passkey: adds the `trusted_devices` table in state.db with full CRUD (`SaveTrustedDevice`, `LoadTrustedDevices`, `LookupTrustedDevice`, `UpdateTrustedDeviceSignCount`, `DeleteTrustedDevice`) and pulls in `github.com/go-webauthn/webauthn` as a direct dependency. No user-visible surface yet — the host endpoints, relay `/me/<site-id>` routing, and enrollment/login UIs land in follow-up commits on this branch.

## 0.103.0

### Minor Changes

- 8841201: Disable the passive-arbitrage PV-charge bonus by default (was 30 öre/kWh).

  The bonus credited each kWh of battery charge fed from live PV surplus,
  intended to break ties when the DP saw "store PV now" and "export PV
  now, reimport later" as economically equivalent. In practice the import
  tariff + VAT asymmetry already makes storage strictly preferred under
  typical retail pricing, so the bonus was redundant.

  The redundancy is harmless on flat-price days, but on days with future
  negative-price hours the bonus pulled morning battery charging forward
  to the point where no SoC headroom remained when the negative-price
  window arrived — forcing PV export against negative prices instead of
  absorbing the (paid-to-consume) energy into the battery.

  Behavior change: operators who relied on the bonus can re-enable it
  explicitly via `planner.pv_charge_bonus_ore_kwh` in `config.yaml`.
  The previous fallback that silently reinstated 30 öre/kWh when the
  value was set to 0 has also been removed — 0 now means 0.

- 476a13c: Expose `CHARGE_CEIL_SOC` and `DISCHARGE_FLOOR_SOC` in the Ferroamp Lua
  driver as operator-tunable YAML config fields.

  ```yaml
  - name: ferroamp
    config:
      charge_ceil_soc: 1.0 # default 0.95 — charge all the way to 100%
      discharge_floor_soc: 0.05 # default 0.15 — discharge down to 5%
  ```

  Both fields are optional and default to the existing constants, so
  existing configurations behave identically. Out-of-range or
  non-numeric values are logged as warnings and the default is kept. To
  actually reach 100 % SoC the operator must also raise
  `planner.soc_max_pct` — the planner cap and the driver cap are two
  independent layers.

- bfc1504: **Replace `ftw-connect` with a URL on `relay.fortytwowatts.com`.** Friend opens a browser to `/h/<6-word-token>`, sees a 4-digit code, reads it to the host on voice, host clicks Allow on the dashboard. Then both Claude Code (`--transport http https://relay.../h/<token>/mcp`) and the web dashboard (`/h/<token>/web/`) work for the rest of the TTL.

  Under the hood: new `ftw-relay` HTTPS request-response relay (linux/amd64 + linux/arm64 release assets), new `internal/tunnel` long-poll protocol, rewired `ftw-pair` host loop. Deletes `ftw-connect`, `ftw-subetha`, `internal/subetha`, the curl installer script, and the old `docs/subetha-deploy.md` runbook. Operator deploys the new relay per `docs/relay-deploy.md` (Cloudflare Origin Cert + systemd, ~15 min).

  Known temporary regression: the dashboard's "friend connected" counter always shows 0 until a follow-up PR wires it through a new relay-side sessions endpoint.

### Patch Changes

- 7b95ce9: Switch the release pipeline from semantic-release to Changesets.

  - `.changeset/*.md` files drive the next version bump + CHANGELOG entry.
  - A "Version Packages" PR opens automatically when changesets accumulate
    on master; merging it cuts the tag and runs the binaries / docker /
    rpi-image / Discord jobs unchanged.
  - PRs to master are now gated on the `changeset-check` workflow — add a
    changeset with `npx changeset`, or apply the `no-changeset` label for
    pure docs / CI / chore PRs.
  - Hitchhiker codename header preserved via `scripts/apply-codename.cjs`.

## [0.8.0](https://github.com/frahlg/forty-two-watts/compare/v0.7.0...v0.8.0) (2026-04-16)

### Features

- **drivers/zap:** aggregate PV from attached inverters ([fb8ca88](https://github.com/frahlg/forty-two-watts/commit/fb8ca8869bea4cac079f68fd9d66a96e7428aac3))
- **drivers:** add Sourceful Zap meter driver ([f1877cc](https://github.com/frahlg/forty-two-watts/commit/f1877cc5b6abdfc7634fbfb07ccdedc927342144))

### Bug Fixes

- key local-vs-cloud HTTP on connection_defaults.host ([5b30477](https://github.com/frahlg/forty-two-watts/commit/5b3047711d7410ef68dff75280a4f1f262a4a55b)), closes [#76](https://github.com/frahlg/forty-two-watts/issues/76)

## [0.7.0](https://github.com/frahlg/forty-two-watts/compare/v0.6.1...v0.7.0) (2026-04-16)

### Features

- **drivers:** align Solis + Deye control with Zap reference ([#74](https://github.com/frahlg/forty-two-watts/issues/74)) ([281f4df](https://github.com/frahlg/forty-two-watts/commit/281f4dfc8027acfedb9ac8ea7ad6fba290ee30c0))

## [0.6.1](https://github.com/frahlg/forty-two-watts/compare/v0.6.0...v0.6.1) (2026-04-16)

### Bug Fixes

- add HTTP capability support for catalog drivers + clarify grid tariff label ([#75](https://github.com/frahlg/forty-two-watts/issues/75)) ([d4cc95e](https://github.com/frahlg/forty-two-watts/commit/d4cc95e21df5853af82f0f11fd69d762a96f353e))

## [0.6.0](https://github.com/frahlg/forty-two-watts/compare/v0.5.2...v0.6.0) (2026-04-16)

### Features

- EV driver UI + lifecycle controls + creds visibility ([#73](https://github.com/frahlg/forty-two-watts/issues/73)) ([52a482a](https://github.com/frahlg/forty-two-watts/commit/52a482a81701ec0e9da2bdfa94e06ca03f5fa21b))

### Bug Fixes

- 3 P1 + 1 P2 from Codex + UI cleanup ([48e0d28](https://github.com/frahlg/forty-two-watts/commit/48e0d2865beac703805765ab238058565f1e91e7))

### UI

- move EV credentials to Devices tab, remove EV Charger tab ([7cd2d9f](https://github.com/frahlg/forty-two-watts/commit/7cd2d9f3af4a547cf9370c29614607b764d9e59f))

## [0.5.3](https://github.com/frahlg/forty-two-watts/compare/v0.5.2...v0.5.3) (2026-04-16)

### Bug Fixes

- 3 P1 + 1 P2 from Codex + UI cleanup ([48e0d28](https://github.com/frahlg/forty-two-watts/commit/48e0d2865beac703805765ab238058565f1e91e7))

### UI

- move EV credentials to Devices tab, remove EV Charger tab ([7cd2d9f](https://github.com/frahlg/forty-two-watts/commit/7cd2d9f3af4a547cf9370c29614607b764d9e59f))

## [0.5.2](https://github.com/frahlg/forty-two-watts/compare/v0.5.1...v0.5.2) (2026-04-16)

### Bug Fixes

- 4 wizard review bugs — path traversal, /setup route, scan API, skip validation ([#70](https://github.com/frahlg/forty-two-watts/issues/70)) ([f691015](https://github.com/frahlg/forty-two-watts/commit/f691015fe154f59e4ce24914674ea924184f556a))

## [0.5.1](https://github.com/frahlg/forty-two-watts/compare/v0.5.0...v0.5.1) (2026-04-16)

### Bug Fixes

- prevent driver paths from accumulating "../" on each config save ([790429f](https://github.com/frahlg/forty-two-watts/commit/790429f79b56281e5fe5875cc6c51e2d3e05572e))

## [0.5.0](https://github.com/frahlg/forty-two-watts/compare/v0.4.0...v0.5.0) (2026-04-16)

### Features

- add setup wizard frontend (web/setup.html + web/setup.js) ([#66](https://github.com/frahlg/forty-two-watts/issues/66)) ([bc1a285](https://github.com/frahlg/forty-two-watts/commit/bc1a2850e8f15c2d1d6d483be6ed627df7b76f5b))
- bootstrap mode + network scanner for onboarding wizard ([#67](https://github.com/frahlg/forty-two-watts/issues/67)) ([267cef4](https://github.com/frahlg/forty-two-watts/commit/267cef42481ee8515abe0ef26ebb5721650d414e))
- wizard dashboard trigger + driver catalog enrichment ([#68](https://github.com/frahlg/forty-two-watts/issues/68)) ([78c83cf](https://github.com/frahlg/forty-two-watts/commit/78c83cf207bf0664e17dabca6c988fdb6f0e5e81))

## [0.4.0](https://github.com/frahlg/forty-two-watts/compare/v0.3.0...v0.4.0) (2026-04-16)

### Features

- config/UI improvements — kWh display, secure EV password, planner tab ([#65](https://github.com/frahlg/forty-two-watts/issues/65)) ([35ab03d](https://github.com/frahlg/forty-two-watts/commit/35ab03d7b5f63ffcc471bf28e1409d761bf0f7d2))
- Easee Cloud driver + host.http_get/post for Lua drivers ([#56](https://github.com/frahlg/forty-two-watts/issues/56)) ([4cdc942](https://github.com/frahlg/forty-two-watts/commit/4cdc9421590385e8f00301925d590f6fb093ebaf))
- EV charger config + credential masking in API responses ([#58](https://github.com/frahlg/forty-two-watts/issues/58)) ([c22cb80](https://github.com/frahlg/forty-two-watts/commit/c22cb805af960bcafc353846f62e2406fc791e17))

### Bug Fixes

- 5 Go-side P1 bugs from Codex review ([#46](https://github.com/frahlg/forty-two-watts/issues/46)) ([0cd2885](https://github.com/frahlg/forty-two-watts/commit/0cd2885bdb79d6a4c3116bb4930ec785cea8f944))
- 5 Go-side P1 bugs from Codex review ([#47](https://github.com/frahlg/forty-two-watts/issues/47)) ([4f2eaf6](https://github.com/frahlg/forty-two-watts/commit/4f2eaf69f626caddf2bae456ac047301f9a36840))
- address P2 review comments across control, MPC, drivers, and UI ([#64](https://github.com/frahlg/forty-two-watts/issues/64)) ([fcafa88](https://github.com/frahlg/forty-two-watts/commit/fcafa88f12c714a1930342dd9f28ea07d18440c2))
- **ci:** disable @semantic-release/github PR annotation features ([4020d46](https://github.com/frahlg/forty-two-watts/commit/4020d4606e0f81924cca5d0e06f4ab743bf8f1d5)), closes [#32](https://github.com/frahlg/forty-two-watts/issues/32) [#33](https://github.com/frahlg/forty-two-watts/issues/33) [#34](https://github.com/frahlg/forty-two-watts/issues/34) [#35](https://github.com/frahlg/forty-two-watts/issues/35) [#36](https://github.com/frahlg/forty-two-watts/issues/36) [#39](https://github.com/frahlg/forty-two-watts/issues/39)
- **ci:** switch semantic-release to conventionalcommits preset ([7e0bb89](https://github.com/frahlg/forty-two-watts/commit/7e0bb895f7a8f8271033336899bed8639e772dc4))
- **ci:** upgrade GitHub Actions to Node.js 24 (drop deprecated Node 20) ([4005bd8](https://github.com/frahlg/forty-two-watts/commit/4005bd8b982c091bff4dcd428cebbe1a08447242))
- Lua driver Command() reading wrong field — Sungrow ignored targets ([9237156](https://github.com/frahlg/forty-two-watts/commit/923715691d55c9dc5c3058b72271d00a72d9c93a))
- populate EV Charger tab from driver config when ev_charger is empty ([5e6b116](https://github.com/frahlg/forty-two-watts/commit/5e6b11676bc972a2c983d39a345a3b5f8dbc77dc))
- remove dead evSlider event listeners that crash app.js ([8ae76c7](https://github.com/frahlg/forty-two-watts/commit/8ae76c710b4ca2d15eb71399211849c4ce03a4bb))
- replace wonky Catmull-Rom spline with simple linear forecast ([abea431](https://github.com/frahlg/forty-two-watts/commit/abea431d7895504116600384c6a92e9577675607))
- show '...' instead of stale v0.1.0 while JS loads version ([dc65065](https://github.com/frahlg/forty-two-watts/commit/dc65065784cad8c018f64338284b5f4b6441ac22))
- **solaredge_pv:** read SunSpec scale factors every poll, not cached ([#38](https://github.com/frahlg/forty-two-watts/issues/38)) ([26f8793](https://github.com/frahlg/forty-two-watts/commit/26f8793f22888dc11d29fd157b10b4340da34c8d))

### Drivers

- add Eastron SDM630 Lua driver ([#18](https://github.com/frahlg/forty-two-watts/issues/18)) ([d5ad806](https://github.com/frahlg/forty-two-watts/commit/d5ad8066377371eb63f320969d153ece50d1266a))
- add Ferroamp Modbus driver (alt transport to ferroamp.lua) ([#31](https://github.com/frahlg/forty-two-watts/issues/31)) ([03b802c](https://github.com/frahlg/forty-two-watts/commit/03b802cefcd1f4d2e07ad05f493ca5643585ed0c))
- fix 9 P1 bugs flagged by Codex review ([#44](https://github.com/frahlg/forty-two-watts/issues/44)) ([b20e485](https://github.com/frahlg/forty-two-watts/commit/b20e485f5fa0a5a20d3a4e83d49410528f81ea1e))
- port Deye SUN-SG hybrid inverter to 42W v2.1 Lua host ([#29](https://github.com/frahlg/forty-two-watts/issues/29)) ([df8fbc0](https://github.com/frahlg/forty-two-watts/commit/df8fbc006375dfc2a3abeb2bc8ec0f01f3e1d0e1))
- port Fronius GEN24 (SunSpec) to Lua ([#19](https://github.com/frahlg/forty-two-watts/issues/19)) ([c1fc875](https://github.com/frahlg/forty-two-watts/commit/c1fc87559b404aa0429ed8ca0a71539e634cb59d))
- port Fronius Smart Meter (SunSpec Modbus, read-only) ([#24](https://github.com/frahlg/forty-two-watts/issues/24)) ([575895c](https://github.com/frahlg/forty-two-watts/commit/575895c7469283bd139deb481e601068045f7519))
- port GoodWe hybrid inverter (ET-Plus / EH) to Lua v2.1 ([#28](https://github.com/frahlg/forty-two-watts/issues/28)) ([e43d2d9](https://github.com/frahlg/forty-two-watts/commit/e43d2d92ef1a7fd26c65b839944bc8d98fa4915a))
- port Growatt hybrid inverter driver (read-only) ([#20](https://github.com/frahlg/forty-two-watts/issues/20)) ([92524ac](https://github.com/frahlg/forty-two-watts/commit/92524acdd890507873a6d5f54b3b6d4335b8e610))
- port Huawei SUN2000 hybrid inverter ([#15](https://github.com/frahlg/forty-two-watts/issues/15)) ([09a8855](https://github.com/frahlg/forty-two-watts/commit/09a88558d0ae17c7e6bdd26387c663badb55e37b))
- port Kostal Plenticore / Piko IQ (Lua, read-only) ([#21](https://github.com/frahlg/forty-two-watts/issues/21)) ([bdeca96](https://github.com/frahlg/forty-two-watts/commit/bdeca96e6c3e05cfe968e20ceb298221f2be5c84))
- port Pixii PowerShaper battery driver to v2.1 Lua host ([#22](https://github.com/frahlg/forty-two-watts/issues/22)) ([70a96d1](https://github.com/frahlg/forty-two-watts/commit/70a96d1120b2aab2cb12ef49688fe3cb204789e3))
- port SMA hybrid inverter Lua driver ([#23](https://github.com/frahlg/forty-two-watts/issues/23)) ([dd34555](https://github.com/frahlg/forty-two-watts/commit/dd3455577c7a3adebad252f81d40b81d3b982350))
- port Sofar HYD-ES/HYD-EP from hugin to Lua v2.1 ([#26](https://github.com/frahlg/forty-two-watts/issues/26)) ([14f6131](https://github.com/frahlg/forty-two-watts/commit/14f6131952b033381a5501f76265714a2b985f1c))
- port SolarEdge SunSpec inverter + meter to Lua (read-only) ([#30](https://github.com/frahlg/forty-two-watts/issues/30)) ([1007e63](https://github.com/frahlg/forty-two-watts/commit/1007e63f9d1908f3210d9b80037e4a6e05e3fa78))
- port Solis hybrid inverter ([#27](https://github.com/frahlg/forty-two-watts/issues/27)) ([98b2a50](https://github.com/frahlg/forty-two-watts/commit/98b2a50ccf59c45130de951dd22db4fc17a67a1a))
- port Victron Energy GX Modbus driver ([#25](https://github.com/frahlg/forty-two-watts/issues/25)) ([ad71db2](https://github.com/frahlg/forty-two-watts/commit/ad71db269438e7aa6e11c632ba1db10897db81be))

### UI

- add status bar with driver health indicators ([b048d60](https://github.com/frahlg/forty-two-watts/commit/b048d60a57049385c498cc4e592ee049a3a05809))
- EV status card + Easee control commands ([#59](https://github.com/frahlg/forty-two-watts/issues/59)) ([b03749a](https://github.com/frahlg/forty-two-watts/commit/b03749ac9ae670447a201e65ed4a57e0db4e99d8))
- fix summary cards grid for 7 cards + raise side-by-side breakpoint ([6e19973](https://github.com/frahlg/forty-two-watts/commit/6e1997312df8ca5b889000d286d0b0782059b701))
- inline target on hover + driver card + collapsible model cards ([de88f43](https://github.com/frahlg/forty-two-watts/commit/de88f4326e5aa5587b623cde76371c0f410eff27))
- legend wrap + nice-tick y-axis + cleaner chart labels ([#33](https://github.com/frahlg/forty-two-watts/issues/33)) ([aeb1d1c](https://github.com/frahlg/forty-two-watts/commit/aeb1d1cb2ab6d69984cdcd424cb6c3da7d775407))
- remove manual EV charging slider ([063174c](https://github.com/frahlg/forty-two-watts/commit/063174cc259d46185da34bad827c16994a3c6e33))
- show mode band in plan chart + grid target on status card ([877e0bd](https://github.com/frahlg/forty-two-watts/commit/877e0bde83964ddb26ce4894ab0adc446fd7801b))
- smooth Catmull-Rom spline for forecast + 15min forecast zone ([dba51a5](https://github.com/frahlg/forty-two-watts/commit/dba51a54c26e6329a4eca850b81b4a22974efcfd))

### Control loop

- fold live DerEV readings into the EV clamp ([#36](https://github.com/frahlg/forty-two-watts/issues/36)) ([5d57d68](https://github.com/frahlg/forty-two-watts/commit/5d57d68c50e6a417b45695bd3ccf551e8566277a))
- slew-rate anchors on actual battery power, not stale command ([#41](https://github.com/frahlg/forty-two-watts/issues/41)) ([4f73f19](https://github.com/frahlg/forty-two-watts/commit/4f73f19abfb6e322a4934d9e9bb46b645afd1352))

### MPC planner

- fall back to forecast when learned PV twin collapses ([#39](https://github.com/frahlg/forty-two-watts/issues/39)) ([f3062ac](https://github.com/frahlg/forty-two-watts/commit/f3062acdd54206de8287b0a9af3862a13cb13105))
- log optimize params + ems_mode per action for plan chart ([9e8c14b](https://github.com/frahlg/forty-two-watts/commit/9e8c14bd388b869091c2315bd4a42def648bf987))
- value SoC at import−export spread in self-consumption modes ([#40](https://github.com/frahlg/forty-two-watts/issues/40)) ([a90d525](https://github.com/frahlg/forty-two-watts/commit/a90d5259209ca9fd8094927b060f62633dd3b5d0))

### Telemetry

- add DerEV type for EV charger readings ([#34](https://github.com/frahlg/forty-two-watts/issues/34)) ([65c9e2c](https://github.com/frahlg/forty-two-watts/commit/65c9e2c23b5f3eb7cb55fd952be7e724b2270e17))

### TSDB

- long-format SQLite (14d) + Parquet rolloff for older ([c53c964](https://github.com/frahlg/forty-two-watts/commit/c53c964e825c896fc0cf760a21ee7b0e29421d2f))

### Safety

- watchdog marks stale drivers offline + reverts to autonomous ([519196c](https://github.com/frahlg/forty-two-watts/commit/519196c01255db3947774bb8a267961b755d261e))

## v0.4.0-alpha (2026-04-16)

First public alpha. Running in production on real hardware but API and config format may still change. See the full changelog below or the [README](README.md) for what the system can do.

### Highlights

- **19 Lua drivers** — Sungrow, Solis, Huawei, Deye, SMA, Fronius, SolarEdge, Kostal, GoodWe, Growatt, Sofar, Victron, Ferroamp (MQTT + Modbus), Pixii, Eastron SDM630, Fronius Smart Meter, Easee Cloud
- **MPC planner** — 48h dynamic programming with three strategies (self-consumption, cheap charging, arbitrage)
- **EV charging** — Easee Cloud integration + OCPP 1.6J Central System
- **Digital twins** — self-learning PV, load, and price models
- **Pure Go + Lua** — single static binary, no Rust, no WASM, no CGo
- **Web dashboard** with real-time power flow, planner visualization, and full config UI
- **Home Assistant** MQTT autodiscovery

---

## Auto-generated changelog (internal)

## [2.3.0](https://github.com/frahlg/forty-two-watts/compare/v2.2.6...v2.3.0) (2026-04-16)

### Features

- config/UI improvements — kWh display, secure EV password, planner tab ([#65](https://github.com/frahlg/forty-two-watts/issues/65)) ([35ab03d](https://github.com/frahlg/forty-two-watts/commit/35ab03d7b5f63ffcc471bf28e1409d761bf0f7d2))

## [2.2.6](https://github.com/frahlg/forty-two-watts/compare/v2.2.5...v2.2.6) (2026-04-16)

### Bug Fixes

- populate EV Charger tab from driver config when ev_charger is empty ([5e6b116](https://github.com/frahlg/forty-two-watts/commit/5e6b11676bc972a2c983d39a345a3b5f8dbc77dc))

## [2.2.5](https://github.com/frahlg/forty-two-watts/compare/v2.2.4...v2.2.5) (2026-04-16)

### Bug Fixes

- address P2 review comments across control, MPC, drivers, and UI ([#64](https://github.com/frahlg/forty-two-watts/issues/64)) ([fcafa88](https://github.com/frahlg/forty-two-watts/commit/fcafa88f12c714a1930342dd9f28ea07d18440c2))

## [2.2.4](https://github.com/frahlg/forty-two-watts/compare/v2.2.3...v2.2.4) (2026-04-16)

### Bug Fixes

- replace wonky Catmull-Rom spline with simple linear forecast ([abea431](https://github.com/frahlg/forty-two-watts/commit/abea431d7895504116600384c6a92e9577675607))

### UI

- add status bar with driver health indicators ([b048d60](https://github.com/frahlg/forty-two-watts/commit/b048d60a57049385c498cc4e592ee049a3a05809))
- smooth Catmull-Rom spline for forecast + 15min forecast zone ([dba51a5](https://github.com/frahlg/forty-two-watts/commit/dba51a54c26e6329a4eca850b81b4a22974efcfd))

## [2.2.3](https://github.com/frahlg/forty-two-watts/compare/v2.2.2...v2.2.3) (2026-04-16)

### Bug Fixes

- remove dead evSlider event listeners that crash app.js ([8ae76c7](https://github.com/frahlg/forty-two-watts/commit/8ae76c710b4ca2d15eb71399211849c4ce03a4bb))

### UI

- fix summary cards grid for 7 cards + raise side-by-side breakpoint ([6e19973](https://github.com/frahlg/forty-two-watts/commit/6e1997312df8ca5b889000d286d0b0782059b701))

## [2.2.2](https://github.com/frahlg/forty-two-watts/compare/v2.2.1...v2.2.2) (2026-04-16)

### Bug Fixes

- show '...' instead of stale v0.1.0 while JS loads version ([dc65065](https://github.com/frahlg/forty-two-watts/commit/dc65065784cad8c018f64338284b5f4b6441ac22))

## [2.2.1](https://github.com/frahlg/forty-two-watts/compare/v2.2.0...v2.2.1) (2026-04-16)

### Bug Fixes

- **ci:** disable @semantic-release/github PR annotation features ([4020d46](https://github.com/frahlg/forty-two-watts/commit/4020d4606e0f81924cca5d0e06f4ab743bf8f1d5)), closes [#32](https://github.com/frahlg/forty-two-watts/issues/32) [#33](https://github.com/frahlg/forty-two-watts/issues/33) [#34](https://github.com/frahlg/forty-two-watts/issues/34) [#35](https://github.com/frahlg/forty-two-watts/issues/35) [#36](https://github.com/frahlg/forty-two-watts/issues/36) [#39](https://github.com/frahlg/forty-two-watts/issues/39)
- **ci:** switch semantic-release to conventionalcommits preset ([7e0bb89](https://github.com/frahlg/forty-two-watts/commit/7e0bb895f7a8f8271033336899bed8639e772dc4))
- **ci:** upgrade GitHub Actions to Node.js 24 (drop deprecated Node 20) ([4005bd8](https://github.com/frahlg/forty-two-watts/commit/4005bd8b982c091bff4dcd428cebbe1a08447242))

### UI

- remove manual EV charging slider ([063174c](https://github.com/frahlg/forty-two-watts/commit/063174cc259d46185da34bad827c16994a3c6e33))

# [2.2.0](https://github.com/frahlg/forty-two-watts/compare/v2.1.0...v2.2.0) (2026-04-16)

### Features

- EV charger config + credential masking in API responses ([#58](https://github.com/frahlg/forty-two-watts/issues/58)) ([c22cb80](https://github.com/frahlg/forty-two-watts/commit/c22cb805af960bcafc353846f62e2406fc791e17))

# [2.1.0](https://github.com/frahlg/forty-two-watts/compare/v2.0.1...v2.1.0) (2026-04-16)

### Features

- Easee Cloud driver + host.http_get/post for Lua drivers ([#56](https://github.com/frahlg/forty-two-watts/issues/56)) ([4cdc942](https://github.com/frahlg/forty-two-watts/commit/4cdc9421590385e8f00301925d590f6fb093ebaf))

## [2.0.1](https://github.com/frahlg/forty-two-watts/compare/v2.0.0...v2.0.1) (2026-04-16)

### Bug Fixes

- 5 Go-side P1 bugs from Codex review ([#46](https://github.com/frahlg/forty-two-watts/issues/46)) ([0cd2885](https://github.com/frahlg/forty-two-watts/commit/0cd2885bdb79d6a4c3116bb4930ec785cea8f944))
- 5 Go-side P1 bugs from Codex review ([#47](https://github.com/frahlg/forty-two-watts/issues/47)) ([4f2eaf6](https://github.com/frahlg/forty-two-watts/commit/4f2eaf69f626caddf2bae456ac047301f9a36840))
- **solaredge_pv:** read SunSpec scale factors every poll, not cached ([#38](https://github.com/frahlg/forty-two-watts/issues/38)) ([26f8793](https://github.com/frahlg/forty-two-watts/commit/26f8793f22888dc11d29fd157b10b4340da34c8d))
