# FTW roadmap

The [product vision](../VISION.md) sets the direction. This roadmap turns it
into user outcomes and acceptance evidence. It is not a list of shipped
features or permission to start every track at once.

Fredrik chooses the next bounded change. Check current code, tests, open PRs
and site evidence before claiming a gap or completion. Preserve established
behaviour while simplifying the product. Bug fixes, security, recovery and
necessary maintenance continue alongside product work.

## Current focus: a complete and trustworthy default experience

Make discovery, planning, control and feedback fit together for mixed hardware
and for both novice and expert users. Minimize setup and routine decisions.
Complete a user flow across Core and clients when needed, using paired PRs
for shared contracts.

Every row below is a target. Existing code contains parts of these flows;
the row is complete only when its evidence exists for the version under review.
This replaces older dated status snapshots. It does not reset completed work
or reopen closed issues.

| Outcome | Product requirement | Acceptance evidence |
|---|---|---|
| Simple setup and first-day value | Discover mixed equipment. Confirm the main fuse and site meter. Read battery capacity where possible and ask for kWh when needed. Power settings and solar kWp are optional where safe device information and learning allow. Provide useful initial load and PV forecasts. | A fresh install reaches useful automatic operation without panel drawings or expert settings. Missing data and wrong start estimates have tested behaviour. Record device identity, known limits and uncertainty. Validate on named hardware combinations as well as simulators. |
| A clear commissioning result | Check commands and measured response within known limits. Distinguish working telemetry from working control. Exclude failed control from both the plan and dispatch. | Show request, device response, measured effect and timing. Cover delayed response, refusal, disconnect, stale site data and recovery. A short commissioning test does not claim full hardware qualification. |
| Live control that earns trust | Keep the fast local feel. Make request, acceptance, command, response, physical effect and freshness visible in the normal Flow experience. | Browser review on desktop and mobile, timing measurements on a target box, and traces with different sampling rates. A pending command or old reading never appears as completed or fresh. Examine existing Live and Flow views before deciding their final layout. |
| Good automatic planning | Use site physics and charge/discharge efficiency. Default wear cost is zero; users may opt in. Keep hard SoC limits separate from forecast-based caution and explicit backup needs. | Cold-start and learned forecasts, stale inputs, multiple assets and unavailable optimizer paths have tests. Backtests use held-out periods and report uncertainty. Defaults and persisted settings agree across UI, Core and worker. Site runs establish practical benefit. |
| Reliable daily charging | Persistent weekday target and deadline. Offline-car estimates. Direct SoC slider after connection, no extra save, prompt replanning, one-action Charge now and notifications when action is needed. | Test from app intent to charger/car response and delivered energy, including missed-goal risk, absent vehicle cloud, unknown SoC, reconnect and restart. Test notifications with the app closed. Confirm physical charging separately from simulation. |
| Useful analysis and fair savings | Keep enough provenance to explain plans and outcomes. Main savings target compares with ordinary self-consumption on the same installation. | Actual cost reconciles with measured import/export and prices. Specify EV behaviour, initial and final stored-energy accounting, efficiency and coverage. Show missing and negative results. Label the current no-PV/no-battery comparison as total site value until replacement is verified. |
| External automation and agent access | Give authorized clients structured analysis data, schedule/goal changes and proposed-plan submission. Temporary external control expires; durable goals persist. Support local access and secure cloud MCP access. | Paired Core/client contract tests cover permissions, expiry, replay, rejection, revocation and reconnect. An agent can trace a request through to measured outcome. Prove local fallback when the caller disappears. Reuse the session/relay where suitable and verify that relay and escrow remain blind. Cloud MCP is a target, not a claim of a shipped endpoint. |
| Less configuration, reliable operation | Every normal setting serves a user need. Keep expert controls discoverable. Installation, updates, backup and recovery remain part of the finished experience. | Audit settings and feature use before removal. Test migration of stored choices so hidden settings cannot keep directing behaviour. Verify restart, upgrade and restore on a target box and review affected UI flows. |

Safety is part of each row. Core remains the only dispatch authority, every
plan is untrusted input, stale required site-meter data stops dispatch, and
failed devices receive their safe default where reachable.

## Heat: data first, bounded control later

First read heat-pump and heating data to improve load forecasts and planning.
Leave comfort control with the heat pump. The battery and other flexible
assets serve the household's needs.

A later control case is a buffer tank or hot-water store charged during cheap
periods. It needs known temperature and storage bounds, safe defaults, verified
hardware control and a user goal. Do not imply active heat support from a
telemetry-only driver or require every house to supply a thermal model.

## Client and repository responsibilities

| Repository | Responsibility in this direction |
|---|---|
| `srcfl/ftw` | Core safety, state, commissioning, dispatch, on-box UI, history, API and shared product direction. |
| `srcfl/energyplan` | Private solver and forecast implementation; defaults and model quality must match Core's contract. Only compiled artifacts and public integration metadata go to Core. |
| `srcfl/device-drivers` | Mixed-device support, stable identity, trustworthy readings, declared limits, structured command results and hardware evidence. |
| `srcfl/ftw-webapp` | Fast everyday UI, charging interaction, intent/result feedback, notifications and encrypted client/session contracts. |
| `srcfl/ftw-app` | The same product principles, with current Pair + Now verification gates preserved before expanding native scope. |
| `srcfl/ftw-web` | Explain the product and contribution route accurately. Separate available behaviour from product goals. |

## How work enters the roadmap

External users submit issues, not PRs. Sourceful implements selected work.
Acceptance of an issue does not invite an external implementation PR.

For each selected change, state the household need, the behaviour to change,
the existing work it touches and the evidence that will establish completion.
Keep details in the issue and PR. Do not create another feature inventory or
long-lived agent work plan beside this roadmap.

Decide exclusions as concrete needs arise. Existing proposals, including the
[tariff and demand discussion](https://github.com/srcfl/ftw/issues/866), remain
evidence to assess; they are neither blanket implementation bans nor delivery
promises. An older PR's title or approval is not proof it still fits current
code or direction. Coordinate changes with its author and preserve unique
work before any owner-authorized closure.
