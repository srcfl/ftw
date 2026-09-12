# FTW product vision

FTW makes a home's energy equipment work together. It runs locally, plans
ahead, controls against current measurements and shows what actually happened.
Its default experience must be simple enough for a first-time user and good
enough to earn the trust of someone who builds their own energy system.

This is the product direction set by Fredrik. It guides design and review;
it does not claim that every outcome below has shipped. The
[roadmap](docs/roadmap.md) names the work and evidence needed to reach it.
The [architecture](docs/architecture.md) describes the running system.

## One system for mixed equipment

Mixed makes and generations are a main use case: for example, an older
SolarEdge inverter, a Pixii battery and a separate charger. Support must state
which models, firmware, measurements and commands have been verified. A
catalog entry alone is not proof that a device can be controlled.

FTW reasons from site physics: energy balance, storage capacity, conversion
losses, equipment response and physical limits. Models and state estimation
help it combine readings that arrive at different times and rates. They must
retain the difference between a measurement, an estimate and missing data.
Algorithms serve that result; their names are not a product promise.

The normal mode gives FTW authority to plan and control within the user's
goals and the site's limits. It does not ask for approval on each action.
Core validates every plan and command, including plans supplied by another
system. Fuse, equipment, SoC, freshness and other quantified safety limits
always apply. Stale required site-meter data stops dispatch.

## Trust through visible behaviour

The live view is a core product feature. It must feel local and fast, and
make these separate facts easy to follow:

- what the user, agent or planner requested;
- what Core accepted and why it changed or rejected a request;
- the command sent to a device and its response;
- the measured physical effect, its age and any delay or difference.

A sent command is not proof of a changed power flow. Smooth rendering must
not make old readings look live. The normal Flow experience should carry this
clarity; the user should not need a separate diagnostic tool to trust it.
Whether the existing Live and Flow views become one view is a design decision
to validate in the browser, not a requirement to keep two interfaces.

Show a failed integration and its effect in plain language. If battery
control is not working, planning and dispatch must stop relying on that
battery. Valid read-only telemetry may remain useful. Other devices may keep
working when the required measurements and safety conditions still hold.
Recovery must establish that control works again before relying on it.

## Useful from the first day

Discovery should obtain what the equipment can report. Ask only for what
FTW cannot establish reliably. The user must confirm the main fuse limit and
FTW needs a working site meter before active control.

Ask for battery energy capacity in kWh when it cannot be read. Battery and
inverter power ratings should not normally be required form fields: obtain
verified device limits where available and learn the usable response within
safe bounds. Advanced users may set limits explicitly. Do not treat an
observed power level as proof of an absolute hardware or installation limit.

For solar, the target is that "I have solar" is enough to start learning.
Installed kWp is an optional starting estimate. Approximate user input must
not permanently constrain a model when measurements support a better fit.
Panel drawings, orientations and engineering knowledge are not prerequisites.

STRÅNG, roof geometry and panel drawing are outside the selected Core scope.
They may serve a future optional extension if a concrete need warrants it;
the module boundary and delivery are not decided. Normal setup must work
without choosing an irradiance source, azimuth or panel layout.

Initial load and solar models must already support useful first-day planning.
On-site learning improves them as evidence arrives. Elapsed days alone do
not prove model quality; state uncertainty honestly and handle cold start.

Commissioning should include a short, controlled check of commands and
physical response, within known limits and with fresh site measurements.
Give the user a clear result showing what worked, what failed and what remains
unverified. A quick response check is not full-range hardware qualification.

## Defaults and user preferences

The default planner uses energy prices and charge/discharge efficiency.
**Battery wear cost defaults to zero.** This is a deliberate product choice.
Users may add a wear cost in settings; do not silently introduce one through
another penalty or hidden preference.

Keep three concepts separate:

- **Battery operating limits:** user-configured min/max SoC and equipment
  limits remain binding.
- **Forecast caution:** extra reserve in a grid-connected home reflects
  expected future demand, supply and uncertainty, and how much the user wants
  to trust the forecast. It is not another fixed SoC floor.
- **Explicit backup needs:** an off-grid or backup use case may need a stated
  reserve floor. This policy does not by itself establish off-grid support.

Explain forecast caution through its effect on the plan. Users should be able
to understand why FTW keeps energy for later without choosing model internals.
Keep useful expert controls available, but require a clear need for each
setting in the normal experience.

## Charging that fits daily life

Plugging in should normally be enough. A persistent schedule can say
"80% by 07:00 every weekday". FTW plans toward that need and reports when it
cannot meet it. Cloud vehicle access must not be a prerequisite for charging.

An offline car is a supported product use case. When its SoC is unknown, use
a stated default or estimate and distinguish it from a confirmed reading.
After plugging in, opening the webapp should expose the car's SoC slider
directly. Changing it needs no extra Save action; show the resulting plan
as soon as Core accepts the change and replans. Keep pending and rejected
changes visible.

"Charge now" takes one action. The rest of the site adapts within its limits.
The user should not have to reconfigure battery policy to charge the car.

Notifications are part of the charging product. Tell the user promptly when
charging fails or the goal is at risk, including when the app is closed.
Explain the problem and take the user to the relevant action. An estimated
SoC is not proof that the requested percentage was reached.

## Comfort and heat

The energy system serves household comfort. The first heat-pump phase reads
data to improve load forecasts and planning; it leaves comfort control with
the heat pump. Batteries and other flexible assets adapt around those needs.

Later, support bounded heat storage where the installation makes it useful:
for example, heating a buffer tank or hot water during cheap periods. That
requires known storage and temperature bounds, a safe control path and a
clear user goal. Do not make a detailed thermal model a setup requirement for
every home, or turn data-only support into active heat control by implication.

## External automation and agents

FTW works on its own and also serves as a local core for Home Assistant,
MQTT automation and agents. They can change goals, modes and charging
schedules, and propose plans. They use Core's admission and safety checks.

Temporary external control has a watchdog limit. It must be renewed; on
expiry FTW returns to its defined local automatic behaviour. Show who is
currently directing the action and why. A durable schedule or user goal
remains after its caller disconnects; it is not a control lease.

Agent first means a supported way to use FTW, not just agent-friendly source
code. An authorized agent needs structured access to measurements, history,
forecasts, plans, decisions, command results and actual outcomes for analysis.
It must be able to change schedules and goals, submit a proposed plan and
verify the result without operating the UI. Report freshness, provenance,
rejection reasons and the distinction between acceptance and physical effect.

Local access and cloud MCP access are part of the direction. Reuse the
webapp's encrypted session and relay where they fit; verify the fit before
adding another transport. Access requires the user's authorization and must
be revocable. The relay and escrow stay unable to read site data. An
authorized cloud agent is an endpoint that can read the data the user grants
it; do not describe that access as blind. The box keeps operating when the
agent, MCP service or network is unavailable.

This document adds no new protocol operation, credential or access grant.
New shared names belong in the contract registry and must ship with their
validation and matching clients.

## Show the value of FTW honestly

The intended main savings comparison is the same site with the same solar,
battery, efficiency, limits, tariff and household needs under ordinary
self-consumption control. It should estimate the benefit of FTW's coordination.
The current comparison against no solar and no battery describes broader site
value; it must not be presented as the incremental value of FTW.

Show actual grid cost and export revenue separately from the modelled
comparison. A fair comparison needs stated charging behaviour, comparable
starting stored energy, an account of ending stored energy and sufficient
measurement and price coverage. Show negative results and missing evidence.
Neither a forecast nor a replay is a measured alternative electricity bill.

## Keep the whole product simple

Installation, settings, local and remote UI, diagnostics, updates and recovery
are part of the product. Prefer fewer features that work together and fewer
decisions required from the user. Preserve useful expert access and flexible
Lua drivers. Removing a setting also requires handling its persisted state;
hiding it must not leave an old policy active without explanation.

Choose the design with the least total complexity. A smaller Core that needs
more services, contracts and deployment steps may make the product harder.
Require a concrete reason for a new module or general framework.

Scope exclusions will be decided as needs arise. There is no standing list
of banned protocols or features. A roadmap entry, green CI or available agent
capacity does not by itself establish priority. Fredrik sets the work; finish
coherent user outcomes and keep necessary maintenance moving.

## Ownership and contributions

Fredrik owns FTW's direction. Sourceful develops and maintains the product.
Agents work within the scope and authority given to them. Reviews should
provide an independent assessment and evidence; file-based reviewer lists
do not define product ownership.

External users contribute through issues: bugs, needs, hardware evidence and
suggestions. FTW does not accept external pull requests, including drivers,
documentation and website changes. Sourceful maintains implementation PRs.
Users may still inspect, run and adapt the software under its existing license.
This contribution policy changes no license or copyright attribution.

The operating rules live in [AGENTS.md](AGENTS.md), review routing in
[APPROVAL_POLICY.md](APPROVAL_POLICY.md), and contribution details in
[CONTRIBUTING.md](CONTRIBUTING.md).
