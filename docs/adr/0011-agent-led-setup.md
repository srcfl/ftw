# ADR 0011: Set up a home through an authorized agent

- Status: implemented for beta review; novice and hardware trials remain.
- Date: 2026-10-18
- Owner: Fredrik. Requested complete setup through MCP on 18 October 2026.

## Decision

FTW is an energy management primitive for people, agents and other clients.
An owner should be able to connect a new box to an assistant and complete its
setup in that conversation. The box owns configuration, drafts, validation,
measurement and control. The assistant asks questions and proposes changes.
The same setup service must serve MCP and the box page.

Normal status, history and control keep their direct tools. Setup and deep
troubleshooting are distinct workflows; neither should add a long interview
to a request to read today's energy use.

## From a new box to a running home

1. **Connect and claim.** The user powers the box and connects its network.
   The box page proves local access and offers either the current wizard or
   setup with an assistant. Assistant setup starts an empty, passive system
   without device commands. The user claims the box and approves the agent
   through the existing owner pairing path. Cloud remains opt-in. A public
   directory entry may start this flow, but cannot claim a box for its user.
2. **Read what is known.** The agent reads setup state, supported configuration
   fields, the driver catalogue and current measurements. It offers discovery
   through an explicit discovery action; a read must not scan the network.
   It separates a device it found from a device the owner chose to add.
3. **Ask the next useful question.** Ask for site name, country, price area,
   currency, location and time zone; then main fuse, phase count and voltage.
   Ask what the home has: site meter, solar, battery, cars, chargers, heating
   and other equipment. Read capacities and limits from equipment where the
   driver supports them. Ask only for missing facts and household choices.
4. **Build a durable draft.** Store answers and proposed config on the box.
   Return its revision, outstanding questions and validation results after
   each edit. A new conversation can resume it. Apply patches to the current
   typed config, preserving fields and saved secrets that the caller did not
   change. A stale revision must conflict rather than overwrite another edit.
   Return the next question, missing facts, field sources and units as
   structured data. A schema of field types alone is not a question plan.
   Keep new credentials in the draft when a client edits a masked list again.
5. **Check and save.** Validate with Core's existing config checks. Show the
   proposed changes, assumptions, effects and required restarts. The owner
   confirms the proposed configuration in the conversation. Commit the exact
   checked revision through the same durable save and reload path as Settings.
   Commit a receipt in the same transaction. If the reply is lost, a retry
   must report the saved revision rather than ask the user to save it again.
6. **Verify the installation.** Start with measurement. Require a fresh site
   meter, confirmed fuse and phase mapping before active dispatch. A driver
   that reports data has not thereby proved control. Offer bounded control
   tests only with the user's agreement; report command, reply and measured
   effect separately. A failed test leaves that device out of active control.
7. **Choose everyday goals.** Explain useful choices for this equipment, then
   save battery policy, reserves, export limits, charge goals and schedules.
   Core keeps every existing safety check. Finish with what works, what is
   still estimated and what needs attention. Losing the assistant never
   removes saved goals or stops local operation.

Use small groups of questions, allow "I don't know", and explain how to find
the fuse rating and phase count. Distinguish saved, connected, measuring,
tested and active control. A device failure must not force the user to
discard working parts of the installation. A conflict should show which
fields changed and allow answers to be carried forward.

The workflow covers the full supported config, including driver connection
fields, price and grid tariffs, weather, battery windows, vehicles and
loadpoints, OCPP, notifications, Home Assistant and other integrations. A
generated schema and live driver metadata describe what this build accepts;
the Cloud service must not keep a second list of config fields. Credentials
that need a vendor sign-in use that vendor's sign-in flow. Do not ask users to
paste tokens when a secure sign-in flow exists.

## Contract and authority

Provide setup tools for status/schema, draft edits, validation and commit.
Read tools explain missing setup without changing anything. Mutation needs
an owner-approved setup grant, distinct from diagnostic read access. Keep the
agent's role and identity; never turn an agent into an owner internally.
Check expiry and revocation on every operation, including just before commit.
An agent cannot grant itself access or accept Cloud terms for its owner.

The box binds a checked draft to its base config revision and content digest.
Commit must name both. It cannot substitute a different patch after the user
has reviewed the result. Bound messages and drafts, reject unknown fields,
preserve exact numbers, and audit the caller and changed field paths without
logging credentials. A reply must distinguish saved, applied, restart needed
and verified on hardware.

First-run bootstrap now offers assistant setup alongside the wizard. It stores
an empty measurement-only configuration and a private house-password hash in
one published SQLite settings file, then restarts. The owner signs in from
another device on the LAN, accepts Cloud terms in Access unless pairing the
app already did, and approves the setup grant there.

`setup_begin` requests diagnostic and setup access together. The box supplies
the complete typed configuration schema, supported modes, runtime time zone,
driver catalogue, outstanding questions, version and commissioning status.
`setup_discover` scans only after confirmation. `setup_test` uses private draft
credentials and blocks equipment writes. It reads an existing driver's health
instead of forking a running OAuth token family. `setup_connect` binds MyUplink
browser consent to the exact draft and grant. Other vendors use their driver
catalogue fields. A driver that needs an undeclared authentication or read
subscription write may require a driver update before it works in measurement
mode; do not present that as a verified connection.

Save equipment in measurement mode first. Read fresh measurements and the
running meter's identity. While control remains blocked, use the ordinary
`goal_request` and `car_goal_request` tools to choose mode, planning style and
car departure goals. The owner approves these in local Access or the FTW app;
Check requests and access refreshes the local list. Read both request receipts
and active values. These goals use their existing state and never replay old
setup answers over later choices. Confirm the host time zone before local
HH:MM targets; a wrong host zone needs a host change and Core restart.

The final draft records the reviewed connection digest, equipment and live
meter identity. Enabling control requires a fresh grid-boundary meter with
usable phase currents for the configured fuse, and unchanged meter connection
settings. Changing measurement mode requires a restart. Saved mode and active
mode appear separately. Measurement mode blocks driver writes even in init,
poll and cleanup; a changed meter identity blocks new dispatch while allowing
Core's safe stop/default paths. Hardware response still needs a measured test.

## Useful forecasts before learning

Ask optional questions that can improve a forecast: known annual household
electricity use, whether that figure includes EV charging or electric heat,
heating type, usual occupancy, and known solar nameplate capacity. A user who
does not know these values must still be able to finish setup. Roof geometry
is optional expert input, never a prerequisite.
Ask whether an annual figure means purchased electricity or total use:
solar self-consumption makes those different quantities.

Use existing weather, solar scale and heating inputs where they fit. A load
prior derived from annual energy must exclude separately forecast EV use and
must not count heating twice. Store the answers, unit, source, time and
assumptions beside the resulting prior. Return uncertainty and label it as an
estimate. Do not add synthetic samples to measured history or increase a
model's sample count. Measured days progressively replace the prior through
the model's existing learning weights. Changing a prior must preserve learned
history and must not change an equipment safety limit.

If no separate heating estimate exists, an annual whole-house estimate stays
a coarse prior. Its unlearned share already includes average heating; only
the measured share adds the learned weather-sensitive heat term.

Missing solar capacity stays unknown until the owner supplies an estimate or
measurement supports one. An observed production peak is not a verified
inverter limit. More detailed solar priors can follow measured comparison;
they do not justify sending new hardware commands.

## Share access with a code

An owner can create a single-use code for another person, choose a lifetime
and choose full technical read access or additional setup/control rights.
Technical read access covers the same complete diagnostic API as the owner's
MCP grant, including energy logs, source and bug evidence. Sharing does not
copy an owner's token, private key or role.

The recipient connects their own MCP client and redeems the code. The box
binds the resulting grant to that client's own key. An interrupted or repeated
claim must not create a second grant. The owner sees who claimed the code,
what rights they received, when access ends and how to revoke it. Revoking a
grant ends its open sessions; cancelling an unclaimed code prevents later use.
The code itself has a short claim window, distinct from the granted access's
lifetime. Store only a verifier, limit failed attempts, and never log the code.
The owner creates an invitation; the recipient selects "I have an invitation"
when connecting their own client. This is distinct from today's flow where
the owner types a code that their assistant generated. Show the owner's label,
the client and claim time. Possession of a code does not verify a person's
real identity. Treat expired, cancelled, already used and network failure as
different states with a clear next step.

Cloud may carry a claim but cannot widen the owner's selected rights. A code
cannot name another home or mint owner access. Setup cannot issue a sharing
code merely because an assistant has permission to edit equipment settings.
Test expiry, cancellation, simultaneous claims, retries, guessed codes,
revocation, narrower grants and a client that loses its original connection.

## Entry through ChatGPT and other clients

The remote MCP endpoint remains client-neutral. Its tool descriptions tell
clients when to start setup, resume a draft, use normal tools or enter deep
troubleshooting. Return source links and running versions in troubleshooting.
No model account or OpenAI API key belongs on the box for this workflow.

A public ChatGPT plugin is a distribution step after the workflow works.
OpenAI's current submission docs require a verified publisher, working OAuth,
product/support/privacy/terms URLs, review cases, a demo and reviewer access
for MCP review. Prepare those against a test home with simulated equipment.
Directory review and approval remain external; a working MCP server does not
mean FTW is listed. Other clients can use the same MCP without that listing.

Source: [OpenAI plugin submission](https://developers.openai.com/plugins/deploy/submission).

## Delivery and acceptance

Ship in separate beta increments so the current full diagnostic read release
does not wait for a new setup architecture:

- A shared setup model, complete schema, durable drafts, conflict checks and
  validation; test a blank config, an existing home, partial answers, unknown
  fields, secret preservation, failed saves and restart/resume.
- The paired Core/Cloud setup contract and explicit setup grant; test real
  OAuth and Noise sessions, revocation, narrowed grants and older boxes.
- Passive first-run claim and the full conversation through saved config;
  test a truly empty data directory, interrupted pairing, no Cloud consent,
  a lost client and restart. Keep the existing wizard usable.
- Owner-issued sharing codes and redemption from another MCP client; test
  the recipient's independent key and complete revocation through a real
  OAuth/Noise session before describing sharing as shipped.
- Optional forecast priors; test energy accounting, no invented samples,
  learned-data preservation and measured replacement of the prior.
- Commissioning and directory packaging; verify actual supported hardware
  before claiming physical setup or control works. Run a complete novice
  trial from a new box through ChatGPT and through another MCP client.

No beta or directory description may claim the later steps work on the
strength of an earlier increment's tests.
