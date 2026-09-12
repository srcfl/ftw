# FTW roadmap

This roadmap is a delivery boundary, not a feature inventory. **NOW** contains
work already in implementation or acceptance. **NEXT** has a defined contract
and entry gates. **LATER** items have no delivery promise; each must satisfy its
promotion gate before it can move forward.

Operating law for what may land is [`AGENTS.md`](../AGENTS.md): the household
loop is the product, there is one live theme, and a LATER row is not
permission to open a PR against `master`.

The permanent rules do not move between lanes: core owns safety and dispatch,
the site sign convention is unchanged, and local operation never depends on an
optional service. Planner output remains untrusted input. A failed or stale
driver still receives its autonomous default. Home Assistant stays optional.

Feature work after NOW is ordered by return: the first track is the one that
changes what a household can do this week without a new tariff model, a new
protocol, or a second safety authority.

Status notes dated 2026-09-02 record what is implemented on `master` on that
date, checked against the code and its tests. They are evidence, not promises:
a track is done only when its exit evidence exists.

## NOW — close the P0 control and product loop

NOW is complete only when these four tracks are implemented, tested together,
and understandable from the local UI:

| Track | P0 outcome | Exit evidence | Status 2026-09-02 |
|---|---|---|---|
| Access boundary | One admission policy covers state-changing requests during setup, boot, normal API operation and local development. Trusted local access remains recoverable; non-local mutation fails closed. The separate site-controller identity remains read-only. | Positive and negative tests cover every lifecycle phase, origin/host handling, credential enforcement and local recovery. | Largely implemented ([#602](https://github.com/srcfl/ftw/pull/602) through [#995](https://github.com/srcfl/ftw/pull/995)): one `Authenticate` gate wraps setup, boot and the live mux; non-local mutation fails closed; loopback recovery works. Missing evidence only: the boot-phase listener has no admission test, the dev proxy's read-only default is untested, and no negative test shows the site-controller identity cannot admit a mutation. |
| Energy ledger and history | One durable ledger records import and export separately, with interval, source and quality/freshness attached. Daily and settlement-period views are derived from that record across hot, warm and cold history; control never offsets earlier import with later export. | Tier-boundary, restart, rolloff and reconciliation tests agree with the site sign convention and retain provenance. | Partial. The ledger itself is durable, directional and provenance-keyed, with tier-boundary and sign-convention tests. But the daily and settlement views still derive from the legacy history tables with no reconciliation test between the two accountings, the ledger has no cold tier (rows are deleted at two years, not archived), and the session receipt does not exist — no session table, no price column. |
| Battery-to-EV lease | Battery support for EV charging is an explicit local lease with a bounded site/loadpoint scope, start, expiry and observable state. The local UI treats a lease as a session: remaining energy, floor SoC, and a stop that the operator can see. Expiry, stale required telemetry or loss of the controlled session releases it; all normal SoC, fuse, power and slew limits still apply. | Tests cover grant, replacement, expiry, restart policy, stale-data release, optimizer interpretation and local operator priority. The UI shows remaining lease energy and the floor without a diagnostic page. | Partial. The control-side lease is complete and tested: bounded duration, sixteen observable stop reasons, stale-data release, restart preflight, planner wiring ([#871](https://github.com/srcfl/ftw/pull/871), [#970](https://github.com/srcfl/ftw/pull/970)). The lease has no energy budget — remaining time only — and its only UI sits behind the Advanced toggle, so both named exit lines are unmet. |
| Mobile and optimizer UX | The local experience works at narrow widths and explains current action, next planned action, fallback state, freshness and active battery-to-EV lease without requiring diagnostic knowledge. | Viewport checks and UI tests cover normal planning, optimizer unavailable/invalid, stale telemetry and lease expiry. | Partial. The explanation half shipped: plan brief, fallback and pause reasons in plain words, and the Ask why conversation ([#1004](https://github.com/srcfl/ftw/pull/1004), [#1010](https://github.com/srcfl/ftw/pull/1010), [#1037](https://github.com/srcfl/ftw/pull/1037)–[#1046](https://github.com/srcfl/ftw/pull/1046)). Narrow-width verification is still source-text assertions — no DOM-driven viewport tests, and the one headless 390 px smoke never runs in GitHub CI. The lease is absent from the normal mobile surface. |

The ledger track is also the session receipt. A finished ledger must be able
to answer how much energy a charge session used, from which source, and at
what recorded price when a tariff is present. Missing price hides the money
column. It does not invent a saving.

The active access-boundary and read-only site-controller work are inputs to
this lane, not parallel remote products. Their contracts must converge on one
rule: identity can establish who or what is speaking, but only core can admit a
mutation and validate its effect.

## NEXT — household charging policy

Entry gate: NOW is complete. These tracks run in the listed order. A later
track may start only when the earlier track has exit evidence, or when it
does not share control or UI state with an unfinished predecessor.

Each track is a core policy or an optimizer-contract field. None of them
puts Home Assistant, a cloud optimizer, or a vehicle OEM API on the control
path. Drivers remain the only hardware dialect. The optimizer may propose;
core still validates and dispatches.

| Order | Track | Outcome | Exit evidence | Status 2026-09-02 |
|---|---|---|---|---|
| 1 | Charge modes | A loadpoint has four named modes the operator can lock: Surplus, Min+surplus, Fast, Off. Surplus starts only when measured site surplus covers the charger minimum for the active phase count, with enable and disable thresholds in watts and minutes. Min+surplus holds the charger minimum and adds surplus on top. Fast may buy grid. Stale site-meter data turns Surplus into Off. It does not hold the last surplus. | Tests cover start/stop hysteresis, the 1-phase and 3-phase minima, stale-meter fail-closed, and mode lock vs optimizer suggestion. The UI names the modes without diagnostic copy. | Partial. Surplus hysteresis, the 1p/3p minima with a sticky day lock, and stale-meter fail-closed all exist ([#590](https://github.com/srcfl/ftw/pull/590)). The four named lockable modes do not: policy today is a surplus-only switch plus manual hold and schedule; Min+surplus is absent and the thresholds are hard-coded, not operator watts-and-minutes. |
| 2 | House reserve and discharge lock | Two SoC bands are first-class: the house reserve (surplus charges the battery first) and the car buffer (the EV may drain only above that band). When Fast or a grid-buy plan is active, core holds the battery so night energy hits the car instead of emptying the house. A lease from NOW may override the bands for one session. The optimizer consumes the bands. It does not invent them. | Tests cover reserve hold, buffer discharge, lock during grid-buy, lease override, and optimizer-unavailable fallback that still honours the reserve. | Partial. One configured floor (`soc_min`), the lease reserve and the battery-may-not-feed-EV clamp are enforced in dispatch and mirrored to the planner. The two first-class named bands are not modelled; the optimizer sees only the floor. |
| 3 | Freeze and hold | Core can command freeze-charge, freeze-export and hold as named intents. A driver that declares the capability executes the vendor hold. A driver that cannot freeze degrades to a quantified 0 W charge or discharge clamp, never a pretend hold. Idle 0 W is not freeze. | Driver capability tests for at least one hybrid that implements hold and one that degrades. Restart and stale-driver paths return to autonomous default, not a stuck freeze. | Not started, except the degrade half: idle mode holds every battery at a quantified 0 W and bounded manual holds auto-expire ([#817](https://github.com/srcfl/ftw/pull/817)). No freeze intents, no driver hold capability. |
| 4 | Fuse tree and phase scaling | Site limits are a tree: a child circuit has a parent, optional meter, and max current and/or max power. Before pausing a charge, core scales 3-phase to 1-phase when the charger can switch and the child still has headroom. New and live sessions share the tree; leftover-headroom-only is not enough. A stale circuit meter is over-limit, not a sum of children. | Tests cover nested circuits, metered vs summed children, 1p/3p before pause, live rebalance, and stale-meter fail-closed. The UI shows the tree and the active clip. | Partial. The single-fuse guard is mature — per-phase clamps, latching hysteresis, joint EV allocation — and 3p→1p happens before a surplus pause. No circuit tree exists: no children, parents, per-circuit meters or power caps. |
| 5 | Solar gate fallback | When the optimizer is unavailable, invalid or stale, core still allocates surplus. Below the house reserve, solar goes to the battery. The battery assists an EV only when live surplus clears a gate. This is the Go fallback, not a second planner. | Tests cover optimizer-down, optimizer-invalid, and surplus below/above the gate without emptying the house battery into the car. | Largely implemented, and overtaken by [#1030](https://github.com/srcfl/ftw/pull/1030): the Go DP is now the champion planner and the Python solver a measurement shadow. Solver failure falls back to the DP, a stale plan degrades to live self-consumption, and battery-assists-EV is gated — today on the per-loadpoint `surplus_unlock_bat_soc` threshold, because track 2's house reserve does not exist yet. |
| 6 | Charge as energy and deadline | A loadpoint goal is remaining energy or SoC, a ready-by time, optional weekday mask, and a strategy: cheapest slots or one continuous block. An optional late window moves the last minutes to just before leave. If the vehicle SoC is stale, the goal is kWh, never an invented percent. A session already drawing power pins the first planner slot to the measured watts so a replan does not cancel a human start. | Tests cover deadline, weekday mask, cheapest vs continuous, late window, stale SoC, and the t=0 pin. The local UI writes the loadpoint goal. | Partial. Ready-by deadline and the weekday mask shipped ([#869](https://github.com/srcfl/ftw/pull/869)); Charge now → target releases back to the plan ([#1007](https://github.com/srcfl/ftw/pull/1007)). The kWh goal, cheapest-vs-continuous strategy, late window, stale-SoC→kWh rule, t=0 pin are absent. |

Tracks 1–5 are dispatcher policy. They keep working if the optimizer is down.
Track 6 is an intent the optimizer may fill; the Go fallback must still produce
a continuous block when the sidecar is absent.

## NEXT — forecasts and plan quality

Entry gate: household charging policy tracks 1–3 have exit evidence, so a
better forecast cannot empty the house into the car.

| Order | Track | Outcome | Exit evidence | Status 2026-09-02 |
|---|---|---|---|---|
| 7 | Forecast beliefs | Load, PV and price forecasts stored for planning carry who produced them, when they were issued, and an uncertainty band. A six-hour-old PV curve is not treated as a meter. | Replay tests refuse to plan from a forecast missing issuer, issue time or freshness. The UI can say the plan used yesterday's weather. | Partial. Per-slot source and fetch-time provenance is recorded and replayed, and forecast trust persists as the k dial ([#968](https://github.com/srcfl/ftw/pull/968), [#1028](https://github.com/srcfl/ftw/pull/1028)). Provenance stays diagnostic: nothing refuses to plan on a missing issuer or freshness, issue time is explicitly out of scope in the schema, and no UI says which weather the plan used. |
| 8 | PV prior and percentiles | The optimizer request includes a physics or vendor PV prior plus p10/p50/p90 (or equivalent bands) and a bounded on-site residual only after that residual beats the prior on held-out days. Curtailment disables residual learning so the chop is not learned as low yield. | Contract tests for the new request fields. A site with no residual keeps the prior. A curtailed site does not shrink the prior. | Partial. A three-scenario band with learned per-slot relative PV error and a bounded on-site residual shipped ([#1026](https://github.com/srcfl/ftw/pull/1026)). There is no held-out skill gate before the residual is trusted, curtailment does not disable residual learning, and since [#1030](https://github.com/srcfl/ftw/pull/1030) the scenario block reaches only the measurement shadow. |
| 9 | Wear and improvement floor | Cycle cost, end-of-horizon battery value, and a minimum improvement to export or to swap slots are optimizer-contract fields. Displayed savings never include virtual wear. | Fixtures where a 0.1-unit spread no longer thrashes the battery, and the UI savings figure matches the tariff ledger, not the virtual cost. | Partial. End-of-horizon value and the arbitrage deadband are live, and displayed savings exclude virtual wear. `cycle_cost_ore_kwh` is declared on the wire but never populated by core, and there is no separate export-improvement floor. |

These tracks extend the versioned optimizer handshake with features. They do
not bump the contract version unless the request shape itself changes. They
do not write `config.yaml` from a learned model.

## NEXT — review the tariff and demand contract

FTW will review the tariff and demand model in text before C&I implementation
resumes. [Issue #866](https://github.com/srcfl/ftw/issues/866) is the shared
review thread. It starts from named, current utility tariffs and source
documents, not from an implementation branch.

The review must settle import and export direction, local civil time and DST,
complete and missing intervals, billing-cycle and peak rules, apparent-power
inputs, proven hardware limits, and the boundary between planning and live
control. Its fixtures must cover at least one real tariff and its failure
cases.

No tariff, demand, optimizer, control or tariff-UI implementation PR starts
until that written contract is accepted. The accepted result will be split
into small issues and one focused PR at a time.

After that contract, these follow in the same ROI order. They stay behind
the written tariff model because each one prices or clips against it:

| Track | Outcome |
|---|---|
| Peak and capacity | The planner sees a billing peak or capacity window. Live control still uses the fuse tree. Effective limit is at least the higher of the configured target and the peak already set this cycle. |
| Feed-in and curtail | Economic export pause and physical `max_export_w` are separate constraints. Stale price never curtails on economics. A DSO cap still binds. Drivers that can curtail report percent, not a boolean. |
| External events | A tariff plugin may emit a windowed event (saving session, free power, VPP stand-aside) as `{window, price, stand_aside, load_scale}`. Core only knows that object. Brand names stay in the plugin. |

## NEXT — the FTW app

The optional FTW Home Link was built, shipped and then removed whole; see
[ADR 0006](adr/0006-app-uplink.md). The remote lane is now one thing: the FTW
app at `app.ftw.energy`, talking to the box over its own protocol. Pairing or
relay availability must not change local control, setup, history or fallback
planning, and does not.

### Identity and pairing

- The box holds three secrets with three lifetimes: a Noise static X25519 key
  that never changes, a rotatable 32-byte rendezvous secret, and a single-use
  pairing code with a ten-minute life. None of them is in SQLite.
- Trust reaches the app optically. The QR code is a URL fragment carrying the
  static public key, the pairing code, a LAN hint and the rendezvous secret.
  A fragment is never sent in an HTTP request, so nothing the app trusts ever
  passes through a server.
- The box is not a WebAuthn relying party. It authenticates a phone by the
  pairing code in the first handshake message and afterwards by the app's
  pinned static key. The passkey lives in the app against `app.ftw.energy`,
  where it gates enrollment and privileged commands rather than reading.
- The machine identity in [`go/internal/gatewayidentity`](../go/internal/gatewayidentity)
  is unchanged and unrelated: hardware-protected P-256 where the hardware
  exists, a bound software key otherwise, and the same deterministic
  adjective-color-animal display name derived from the stable 18-hex gateway
  ID. It identifies the machine; it authorises nothing.
- Multi-site means several independently paired boxes. There is no central
  user-to-site directory. A consolidated multi-site view stays gated in LATER.

### Connection and authorization contract

The box holds one outbound WSS connection to `wss://relay.ftw.energy`, joining
under a handle derived per epoch from the rendezvous secret with HKDF-SHA256.
The handle rotates hourly, so the protocol exposes no stable per-box name or
DNS alias. The relay still sees source IP, timing and connection continuity,
which can correlate a household across the hour boundary.
An epoch correction from the relay is read as a clock correction and clamped;
it is never an order.

The relay forwards encrypted frames and holds no keys. Up to four phones share
one uplink; the relay broadcasts, and the box lets the AEAD decide which
session a frame belongs to, because asking the relay would require the relay to
know. Lane 0 frames are constant in length and cadence, because a
variable-length 1 Hz power stream leaks a household's load pattern through
perfect encryption.

Commands carry an expiry and preconditions, and core revalidates against fresh
state before acting. A queued command is never replayed silently. Site mode
changes go through `control.ApplyMode` from every door. Stale telemetry, local
limits and local operator actions remain authoritative.

The public `srcfl/device-drivers` release channel remains separate from
pairing and authentication.

The three items that were open here shipped in 2026:

- the on-box pairing surface is the Settings → FTW app tab, which mints the
  single-use code on demand and draws the QR with a fragment-only payload
  ([#828](https://github.com/srcfl/ftw/pull/828),
  [#994](https://github.com/srcfl/ftw/pull/994));
- per-device revocation is `DELETE /api/app-link/devices/{id}`, immediate at
  the box, so one lost phone no longer costs the household its pairing
  ([#831](https://github.com/srcfl/ftw/pull/831),
  [#880](https://github.com/srcfl/ftw/pull/880));
- the plan, history tiles, prices and EV commands ride the app protocol
  ([#829](https://github.com/srcfl/ftw/pull/829),
  [#836](https://github.com/srcfl/ftw/pull/836),
  [#871](https://github.com/srcfl/ftw/pull/871)). Push deliberately does not:
  it is Web Push with the relay as a blind dead-man courier
  ([#872](https://github.com/srcfl/ftw/pull/872)).

### Conditional Apple EnergyKit native companion

Apple EnergyKit is a conditional native companion initiative, never an FTW
core implementation. The base framework, electricity guidance, and EV/HVAC
load events require iOS/iPadOS 26 or later. Named load devices, EV
status/reasons/targets and Home presentation belong to the OS 27 beta line.
EnergyKit remains officially limited to the contiguous United States, so a
Sweden pilot is blocked by both region and the stability of the beta APIs.

The native app owns the entitlement, consent, venue mapping, guidance token and
offline event submission. For every venue, the person explicitly opts in to a
binding between their local passkey identity, the site-controller public key
and that venue. Person identity and site identity never collapse into one key.

Core owns a versioned, vendor-neutral venue/guidance/load-event flow and a
durable neutral EV event journal. Guidance is advisory input and passes the
same completeness, freshness and safety validation as every other planner
output. Adapter absence, denial or regional unavailability leaves FTW planning
and local operation unchanged.

This initiative cannot graduate until regional availability includes the
target site, the OS/API surface is stable, the neutral EV event journal is
durable, and the consent, retention and deletion model has passed privacy
review. See Apple's official [EnergyKit overview](https://developer.apple.com/energykit/)
and [EnergyKit updates](https://developer.apple.com/documentation/updates/energykit)
for the platform boundary.

## LATER — promote only from evidence

These are bounded follow-on directions, not scheduled commitments. Order
inside this table is still return: the first row is the one that should
be promoted first once its gate is met.

| Direction | Promotion gate |
|---|---|
| External grid constraints | A versioned constraint record has provenance, effective window, expiry, conflict handling and an audit trail; it can never weaken physical site limits. The record caps the root of the fuse tree. Household charging policy track 4 (fuse tree) has exit evidence. |
| Active heat | Neutral thermal capabilities, comfort bounds, a legionella or equivalent hygiene constraint, and a safe autonomous default are demonstrated before dispatch is enabled. The heat pump remains the thermodynamic owner. Core may request boost, dim or continuous run; it never writes a compressor setpoint. Household charging policy tracks 1–3 have exit evidence so heat cannot empty the house battery. |
| Excess-PV sinks | A priority list after the house battery (EV surplus loop, water, then other dump loads) is defined as eligible slots plus a measured-surplus dispatcher. The optimizer does not open-loop a sink. Active heat or a dump-load driver exists with a default-off fail. |
| Matter support ([matter.js](https://github.com/project-chip/matter.js)) | A TypeScript matter.js process runs as an optional module behind a narrow versioned contract — like the optimizer: independent failure and update semantics, and a safe unavailable state in which local control, planning and existing drivers are unchanged. Matter devices reach control only as capability-declared devices keyed by stable node identity, with autonomous defaults and sign conversion at that boundary. A Matter fabric may observe the site but never becomes a second commander, and fabric credentials stay out of SQLite. |
| OCPP gateway | The EV lease/action model and stable charger identity are proven locally, including disconnect and autonomous-default behavior. *Status 2026-09-02: effectively delivered — a built-in OCPP 1.6J + 2.0.1 server shipped with stable charger identity, disconnect handling, a hold-last-limit autonomous default and a current-limit-only action model ([#979](https://github.com/srcfl/ftw/pull/979), [#999](https://github.com/srcfl/ftw/pull/999), [#1015](https://github.com/srcfl/ftw/pull/1015)); the battery-to-EV lease is not yet exercised against an OCPP loadpoint in tests.* |
| OCPP forwarding | The gateway is proven. A tap can forward session and meter frames to one upstream CPO and block remote start, stop and profile so core stays the only commander. Upstream loss does not stop local charging. |
| Passive battery awareness | An EV-only site can read house-battery SoC without owning the inverter, and still refuse to pull the pack below the house reserve. *Status 2026-09-02: only a building block exists — opt-in read-only PV/battery ingest from the Zap ([#974](https://github.com/srcfl/ftw/pull/974)).* |
| Native widgets and richer multi-site views | The app protocol's read schema, per-site pairing and privacy budget are stable in production. |
| [Dashboard render budget](https://github.com/srcfl/ftw/issues/881) | A reference Raspberry Pi trace proves zero chart and particle work while hidden, measures visible-frame cost on the agreed fixture, and shows no visual or control regression in automated checks and human browser review. |
| [Energy-ledger write batching](https://github.com/srcfl/ftw/issues/882) | Exact ledger and cursor parity and rollback tests pass; Pi arm64 shows at least 2x speed on tmpfs, 20% on deployment SD, and 35% fewer allocations at 12 observations, with no case slower by more than 5%. |
| V2X automation | Bidirectional capability, metering, lease ownership, interlocks and fallback are proven for the complete local actuation path. *Status 2026-09-02: the policy envelope exists and collapses to 0 W on stale inputs, but dispatch is still manual operator commands; the automated path stays gated.* |
| General vehicle snapshot adapter | A minimal vendor-neutral snapshot has stable vehicle identity, freshness and consent semantics without becoming a second control path. Offline and guest vehicles remain the default path. A sleeping OEM API must not block surplus charging. |
| What-if and tariff compare | An offline tool can replay the ledger under another tariff. It never sits in the control loop. The tariff contract from issue #866 is accepted. |

## Later — Device Support package promotion

Device Support may later consume an exact `srcfl/device-drivers` commit for
another product or a higher support level. That work must not create a second
editable source or replace FTW's public default channel. Core will consume only
packages that pass its host contract, signature, compatibility, activation and
rollback checks.

The architectural decision for the FTW app lane is recorded in
[ADR 0005](adr/0005-outbound-site-link.md).
