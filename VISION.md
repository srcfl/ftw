# FTW product vision

FTW makes a home's energy equipment work together. It runs locally, plans
ahead, controls against current measurements and shows what actually happened.
Its default experience must be simple enough for a first-time user and good
enough to earn the trust of someone who builds their own energy system.

The product is a lean core: FTW Core and the Energy Planner. They are the
primitives an agent or another automation needs in order to work with the
home's energy system. Home Assistant, MQTT and a database the owner runs for
their own analysis sit on top. Core does not absorb those tools.

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

## Are you in control?

**Don't trust what you say. Trust what you read.**

FTW must verify control through fresh measurements. Sending a command, getting
an API success or reading back a setpoint does not prove a physical effect.
The user should be able to see whether the equipment did what they asked,
what limited it and what FTW can actually confirm.

Trace each request through Core's limits, the command sent, the device's
response and the measured result. Keep the time, source and unit of each
step.

Apply these levels to the device function FTW commands. Other measurements
provide context: solar normally contributes measured generation, not a control
verdict. It receives a control verdict when FTW issues curtailment. Household
load is the residual after measured flows; it is not another independent meter.

Each device keeps its own level. One may have Tier 2 while another is offline
with only a Tier 0 acknowledgement. Missing readings from another device
leave its effect in the unmeasured background, alongside household load;
they do not block this device's confirmation by themselves. That residual may
also contain generation when a battery or solar source stops reporting.
Require fresh readings from the device under test and its independent meter.
Use other flows as corrections only when their measurements support the same
before/after window. A changing residual can still prevent confirmation.

Show each level on its device in the overview, inside the energy-flow bubble.
Keep power and state of charge prominent. A tap opens the reason, request,
measurements and curves. Combined bubbles show mixed levels and keep alarms
visible; they never turn one device’s proof into a verdict for the whole group.
Losing measured confirmation on a previously verified device must raise a visible alarm after its normal response wait.
Starting a command, unplugging a car or returning control to a device must not
create a false loss alarm. Fresh measured proof clears the alarm.

Use three explicit levels:

- **Tier 0 — acknowledged:** the command was sent and acknowledged. This
  confirms communication, not a physical effect.
- **Tier 1 — device measured:** the device's own fresh power reading follows
  the sent command. A setpoint echo alone cannot reach this level.
- **Tier 2 — independently confirmed:** a separate physical meter shows the
  matching effect. For example, a new 1 kW charging command produces a 1 kW
  device reading and a corresponding 1 kW increase in site import.

Use time-aligned readings, short measurement windows, tolerances and response
waits to handle noise and different sample rates. Account for solar and other
loads. A simultaneous load change may prevent attribution; remain at Tier 1
until the evidence supports Tier 2. A steady absolute grid value is not proof
of a command's effect. Check the change against the baseline.

Sensor fusion must respect energy balance and retain each reading's source
and age. Two fields from the same sensor are not independent confirmation.
Neither an estimate nor house load derived from those same readings can act
as independent proof. Missing, stale or conflicting data lowers confidence;
never smooth it into a successful result. Confirmation expires when its
supporting readings stop being current.

Treat each command as a chance to observe the whole site. Keep a short window
before it and follow the measured curves afterwards. Subtract other measured
flows from grid power before comparing the change with the controlled device.
Changing solar need not prevent confirmation when its measured contribution
explains the grid change. Unknown household loads remain an uncertainty:
matching averages alone must not hide a load starting and stopping.

Use the fastest useful fresh measurements the sources safely provide. A short
burst around a command may help when the driver supports it, but must respect
device limits and vendor quotas, preserve command priority and end on its own.
Polling a cloud cache faster is not faster measurement. Never change a power
target just to create a test signal without the owner's explicit consent.

Keep success at each step separate. If a user asks for 11 kW, Core permits
5 kW and the meter shows 5 kW, the device follows the sent command but the
user's request remains limited. A charging goal needs its own completion
evidence. A matching reading also does not prove that FTW is the only
controller; report a changed setpoint without guessing who changed it.

Apply this standard to every control mode and supported device. Show waiting,
limits, failed commands, missing effect and recovery in the normal view, with
the known cause and a useful next step. Say when the cause is unknown. Keep
warnings current and clear them when new evidence shows recovery. Make the
same evidence available to authorized agents and support tools.

Traceability and robustness are product requirements: retain enough evidence to
explain what happened, and stop depending on an actuator that cannot deliver.
Recovery needs fresh proof of response. This is the product direction; each
implementation must state which devices, paths and physical outcomes it has
verified.

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

## Versions tell the truth

The version stays in 0.x until FTW is actually a 1.0. Real sites already run
it, and it is still an early project. The 1.x, 2.x and 3.x numbers came from
removals, not from a decision that the product was finished. The native line
continues the earlier 0.x counter at 0.131 so tags that were already published
stay unique. [ADR 0007](docs/adr/0007-self-updating-binary.md) records that
choice.

## Running it, updating it, and a later hosted service

A person who runs FTW on their own machine is using an early project that
already works. They should be able to follow a guide, take a backup and apply
an update. systemd and a container are both valid ways to run it. The project
documents those ways.

FTW's own work is the EMS and the Energy Planner. The owner operates the
machine it runs on: the service manager, when to update, backups kept off the
box and logs. They can do that by hand or through their own automation or
agent. The project gives them a few documented steps and does not take over
running their host.

Those steps are short commands on the machine, and the same operations are
available over the API. Each one runs without questions, shows its progress
and ends with a clear result, so it can be wrapped. The web UI shows the
running version, whether a newer release exists and the command that installs
it. On a native install it does not update, roll back or restore.

The update itself stays small and must be robust enough to run unattended. On
a native install, Core downloads the next verified release and swaps to it. A
new release that does not stay up falls back to the previous one without
operator action. A release that changes stored data first makes a verified
full backup. On a Docker install, the owner sets the new version and rebuilds
from the same release package. A
privileged sidecar whose job is to update Core is not part of the product. The
machinery around that sidecar is retired as sites leave it. The same ADR
records the shape.

A hosted service, in the spirit of the Sourceful Blixt gateway, is the later
path for a household that wants FTW to just work and does not want to operate
the box. It has not shipped. Until it exists, the project does not promise
individual support for every self-hosted site that can report only a version
number while releases still move quickly. Support that an agent can carry
out needs the user's authorization and access to that gateway. The box keeps
planning and controlling when the agent and the network are gone.

## Ownership and contributions

Fredrik owns FTW's direction. Sourceful develops and maintains the product.
Agents work within the scope and authority given to them. Reviews should
provide an independent assessment and evidence; file-based reviewer lists
do not define product ownership.

External contributions are welcome, including code, drivers, documentation
and website PRs. Prefer an issue that states the need and evidence. A broad
product or architecture proposal can start as a short Markdown PR; a concrete
fix can include code and tests. Hardware changes need relevant hardware
evidence before we claim working support.

Development is agentic first. Make problems, scope, test steps and results
clear enough for people and agents to assess and continue the work. The
submitter checks the result; agent output alone is not test evidence.
Sourceful reviews and maintains changes; Fredrik sets direction and priority.
An accepted issue or proposal does not promise implementation or delivery.
This policy changes no license, copyright attribution or release authority.

The operating rules live in [AGENTS.md](AGENTS.md), review routing in
[APPROVAL_POLICY.md](APPROVAL_POLICY.md), and contribution details in
[CONTRIBUTING.md](CONTRIBUTING.md).
