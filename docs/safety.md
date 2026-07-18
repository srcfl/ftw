# Safety invariants

Core owns safety. Drivers translate hardware and the optimizer proposes plans,
but neither may bypass the final validation and dispatch path.

## Power boundary

All core values use the site convention: positive W flows into the site and
negative W flows out. Only a driver converts vendor signs. A sign fix above the
driver boundary is almost always hiding a driver bug. See
[site-convention.md](site-convention.md).

## Stale data

The telemetry watchdog runs every control tick. A driver with no fresh
successful reading within `site.watchdog_timeout_s` is marked offline and sent
its autonomous `driver_default_mode`.

The configured site meter is stricter: stale meter data stops all storage and
loadpoint dispatch. If the meter exposes per-phase current, every configured
phase must remain fresh as well because those readings are required to protect
the site fuse. Core sends connected loadpoints an explicit zero-power standdown
and reverts controllable drivers to autonomous mode once on the stale
transition, including a combined site-meter/battery owner. Telemetry
observation and schedule state continue advancing while actuation is blocked;
normal dispatch resumes only after the required readings recover.

Source and executable specification:

- `go/internal/telemetry`
- the control tick in `go/cmd/ftw/main.go`
- watchdog and stale-meter tests beside those packages

## Dispatch limits

Safety constraints are applied after the requested mode or planner target has
been computed. The final command path enforces:

- driver and aggregate charge/discharge capability;
- configured battery SoC floor and ceiling;
- whole-site import/export and fuse capacity;
- per-phase current when phase telemetry exists;
- dispatch interval and normal slew rate;
- charge-only reactive correction where a plan would otherwise create
  unintended import;
- EV/loadpoint fuse sharing and surplus-only promises.

The reactive fuse saver may bypass normal slew limiting because preventing a
fuse trip is the higher-priority quantified risk. It must still respect
available battery energy and hardware power capability.

The implementation and tests in `go/internal/control` are authoritative.
Planner-side limits in `go/internal/mpc` reduce infeasible plans but do not
replace runtime enforcement.

## Planner and model containment

Optimizer output is untrusted input. Core requires a complete, time-aligned,
finite trajectory within the request contract. Invalid, partial, late or
unavailable output is rejected and the Go fallback remains available.

Learned battery, PV and load models are advisory. Confidence gates and physical
sanity envelopes prevent a young or drifting model from overriding direct
measurement or configured bounds. Model saturation must be calculated from
physical capability, never from a previously clamped command; feeding a clamp
back into model capacity creates a self-reinforcing loss of authority.

## Driver default mode

Every controllable driver implements a safe autonomous mode appropriate to the
device, normally vendor self-consumption or cancellation of forced power. It is
used on watchdog transition, shutdown, driver removal and relevant reloads.
Default mode must not depend on core continuing to send commands.

## Clamp discipline

Do not add a “just in case” clamp. Each protection must name:

1. the measurable unsafe condition;
2. the physical/configured limit;
3. the safe reaction and recovery condition;
4. its interaction with higher-priority protections;
5. tests for both activation and non-activation.

Do not remove or reorder a protection based only on a simplified happy-path
simulation. Verify stale data, phase imbalance, empty/full assets, planner
failure and recovery.

## External systems

Home Assistant, CalDAV, notifications, cloud drivers, price/weather services
and Nova fail soft. Their network I/O is outside the control tick, and their
failure cannot disable local measurement or safety. Self-update snapshots state
before replacement and uses immutable version targets.
