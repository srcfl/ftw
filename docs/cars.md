# Cars

A car belongs to its owner, not to the home. It visits chargers. The box
keeps the cars of its owners, so it needs no cloud to know them.

This is the contract. A box that lists `der.car` in `hello_ok.caps` keeps
cars as below; one without it treats each charger as its car, as before.

| Thing | What it is | What it owns |
|---|---|---|
| Car | A DER whose owner is a person | Id, name, make, VIN, capacity, its own charger's limits, level, goal, surplus-only, identifiers, level sources |
| Charger | A loadpoint at the site | Plug, power, phases, limits, charge now, boost, the site's battery rule |
| Charging session | One car on one charger, plug-in to unplug | Its id, the car or none, how the box knows it, energy delivered to that car |

The names are in `contract/registry.yaml`: `car_fields`,
`car_soc_sources`, `car_identified_by`, `charger_car_fields`, the `der.car`
capability, the scopes `ftw.cars.read` and `ftw.cars.write`, the car ops,
the refusals `E_NOT_FOUND` and `E_DUPLICATE`, and the `car.which` push kind.
The Go types are in [`go/protocol/cars.go`](../go/protocol/cars.go).

## Owners and ids

- Until owners are principals of their own, every phone with the owner role
  shares one list of cars. `ownerId` is reserved for the owner a car belongs
  to and stays empty until then.
- A car's id is 128 random bits as 32 lowercase hex digits. **The owner's app
  mints it**, and `car.add` requires it, so the car is the same on each box
  the owner has. Adding an id the box already has updates that car.
- The box mints ids only when it makes cars from its old settings, and marks
  those cars `boxMinted`. The app adopts such a car by sending
  `car.update` with `new_id`, or by adding a car with the same VIN.
- A VIN or an identifier that another car already has is refused with
  `E_DUPLICATE`.
- A removed car leaves a tombstone: `GET /api/cars` lists its id under
  `removed`, so an app copying lists between boxes does not bring it back.
  Adding the id again is the owner's word and brings it back.
- The app reads the box's list after each hello and copies a new or changed
  car to each of the owner's boxes. Every field travels except `sources`,
  which are the box's own: Home Assistant at home, the maker's service
  anywhere. A source is bound to a car by VIN, or by choice on that box.

## Who sees what

| Read | Who | What |
|---|---|---|
| `GET /api/cars` with `ftw.cars.read` | The owner role | Every field |
| `GET /api/cars` without it | Viewers, watches and agents | `id`, `name`, the level fields, `goal`, `surplusOnly`, and `redacted: true` |
| FTW Cloud's `car` class | Phones with `ftw.live.read` | Opaque `id`, the level fields, `goal`, `surplusOnly` |
| `cars.get` | Agents with `ftw.live.read` | The car class under the agent key ([agents.md](agents.md)) |

An agent asks for a car's goal or level with `car.goal.set` and
`car.soc.set` as `agent.request`, which waits for an owner, naming the car
by the id `cars.get` gave it. It never taps and never keeps the list.

On the box's own listener every device is the owner while the house
password is off. There `GET /api/cars` gives every field only to a request
that has proved itself as a protected one must: from a local address, from
the box itself, with the house password, or with `FTW_API_TOKEN` through a
public name. Any other request gets the shorter list with `redacted: true`,
so a box published under a public name does not hand out its owners' VINs
and charge tags.

`car.add`, `car.update` and `car.remove` need `ftw.cars.write`, which only
the owner role carries. They change no hardware, so they need no passkey
step-up. No car op waits for the dispatch gate: a stale meter must not stop
the tap that names a car.

## The level

A car's level is its last reading plus the energy delivered to that car
since, × 0.9 / capacity.

- **Sources** (`socSource`): `vehicle` is a reading from the car, from a
  vehicle driver or OCPP 2.0.1 `NotifyEVChargingNeeds`. `manual` is a level
  someone set. `assumed` is a known car's plug-in default.
- **A newer reading from the car beats a level someone set.** An older one
  does not.
- **A reading stays exact until energy flows.** The box marks the level as an
  estimate (`socEstimate`) once more than 1 kWh has reached the car since the
  reading. Time alone does not age it: 14 % with nothing delivered is still
  14 % hours later.
- **While the level is exact, `socPermille` is the car's own number** (or
  the person's). A number the car never said is never shown as the car's.
  The planner starts from the same number.
- **A level read before the car last left a charger is an estimate**, and it
  does not start the next session: the car may have driven. Nor does a
  reading more than an hour older than the plug-in. That session starts
  from the car's plug-in assumption, `assumed`, until the car reports again,
  as every session did before cars.
- An unplug counts once the charger has said so for 30 seconds, or names a
  new session. A cable that flaps for one reading keeps the session and its
  level.
- An assumed level is always an estimate.
- `socAgeS` is the seconds since the car reported the level or someone set
  it, counted at send, never from the box's wall clock. `energySinceWh` is
  what has flowed since; a V2X session makes it negative.
- An app can say "14 % · from the car, 9 min ago" or
  "≈ 22 % · from the car 16:31 + 6 kWh".
- **A visitor gets no level.** A car the box cannot name has none, and the
  box never guesses one.
- A car's charging state is live for five minutes (`VehicleMaxAge`). Only a
  live state wakes the car or tells the box a goal is done.

## Which car is on a charger

At each plug-in the box asks, in this order (`carIdentifiedBy`):

1. `charger`: the charger names the car by OCPP idTag, MAC or eMAID, and a
   car lists that identifier. An identifier no car lists makes the session a
   visitor's (`none`) only when some car lists identifiers: the charger has
   then said it is another car. While no car lists any, the identifier is no
   answer and the box goes on to rule 2. An OCPP charger sends an idTag at
   every start, often the same one for every car.
2. `car`: one of the car's sources saw its cable go in within a minute of
   the plug-in and does not say it is elsewhere (`at_site` false), or says
   it is charging at this site (`at_site` true) while this charger alone
   delivers. A source says so with `plugged` and `at_site` (or
   `site_distance_m`) in its `vehicle` emit, or with a charging state that
   needs a cable. The time that counts is when the source read the cable
   change: its `plugged_since_ms`, or else when the box first saw the source
   say so, if the source's reading before that was less than a minute
   older. A change seen between readings further apart has no time and does
   not count. A source that does not know where the car is counts only by
   that minute. Two cars saying so is no answer, and so is a cable time
   when another charger saw a plug-in within the same minute: the car could
   be on either. One source of a car that says it is elsewhere rules the
   car out. The fields are in
   [writing-a-driver.md](writing-a-driver.md#which-car-is-plugged-in).
3. `only`: exactly one car could be here: one car is not on another
   charger and has not been read elsewhere since the plug-in. The car's
   usual charger counts the same way: a car the box made from a charger's
   old settings has that charger as its usual one, and a plug-in there names
   it when no other car that could be here claims the charger, that is when
   each of them has a usual charger elsewhere. A car with none, as one an
   owner added since, could be on any charger, and the box asks. A car whose
   source read it within a minute after the plug-in and found `at_site`
   false or its cable out is not one that could be here; when that leaves
   no car, the session is a visitor's (`none`). A reading from before the
   plug-in does not count, however late it arrives. One car is on one charger at a time: rules 2 and 3
   skip a car another session has.
4. `tap`, after `asking`: with more than one car, the box waits a minute for
   rules 1 and 2, then raises `car.which` once per plug-in and says
   `asking`. A tap sends `session.car.set`. A tap, the charger or the car
   ends `asking`, and so does the unplug.
5. `none`: a visitor. A tap that picks none of the cars gives `none` too.

- The choice lives in Core and ends at unplug. It survives a restart when
  the charger's own session id, or its energy counter, shows the same
  plug-in. Until the charger has reported after a restart, the box keeps the
  session as it was.
- The charger's word counts from each transaction's start, and it may come
  before the box has seen the plug-in; it then waits up to a minute for it.
- **A tap names the plug-in.** `session` is the plug-in's id; a tap sends
  it, and the box refuses a tap for a plug-in that has ended
  (`E_PRECONDITION`, reason `session`).
- **A later, better rule may correct an automatic choice**, as when the car
  reports in after the box assumed the only car. Only the charger or the car
  corrects, never by a worse rule, and only with evidence newer than the
  choice. The box then sets `carCorrected` and `carCorrectedFrom` for the
  rest of the session, and logs it.
- **A tap is final once the box had answered by itself**: someone who taps
  over the charger's, the car's or the only car's answer knows better. A tap
  made while the box was asking can still be corrected by the charger or the
  car. A later tap replaces an earlier one.
- **Moving a session to another car moves its energy.** The new car gets the
  energy delivered since its own reading or since the plug-in, whichever is
  later. The car it had goes back to its level from before the session, or
  keeps a reading its own source made during it. The box plans again.

## The plan

The plan is made per session, from the session's car: its level, its
capacity, its goal and its surplus-only choice. Planned power is held to the
lower of the charger's maximum and the car's own (`maxW`), keeping the
charger's lowest step when that is above the car's maximum. The charge
limit the car reports caps the plan only while the level is the car's own
and still exact. The reserve that keeps a goal charging while no plan is
accepted holds to the same power and limit. `phases` is the owner's note:
`maxW` already counts the car's phases, and the controller follows the
charger's phase mode. A change of car or goal rolls the charger's deadline
before the replan, so that plan has the car's goal in it.

- **One goal per car**, used wherever the car is plugged in. `readyByMin` is
  in the box's time zone.
- **Surplus-only is the car's choice.** The site's battery rule
  (`surplus_unlock_bat_soc`) is the charger's: it rides
  `PUT /api/loadpoints/{id}/schedule` as before, and no car op carries it.
- **A session whose car is unknown** (`asking` or `none`) has a goal, a level
  and a surplus choice of its own, until unplug. Its capacity is the
  charger's `vehicle_capacity_wh`, so energy delivered still counts toward a
  goal, though the box shows no level. `car.goal.set` and
  `car.soc.set` with `charger_id` set them, and so do the charger routes;
  neither ever touches a car. Until someone sets a goal it charges from
  surplus only, with no level. Someone who sets a goal wants it met: the
  session no longer charges from surplus alone, and the plan and the reserve
  plan from empty, while the level stays unknown and is never shown as a
  number. The car stops when it is full. Clearing the goal brings
  surplus-only back. The design's "or the cheapest quarters" without a goal is not
  built: with neither a level nor a goal there is no amount of energy to
  place.

## Ops

All are `cmd` frames. Arguments follow the box's convention: snake_case, `id`
for what the op acts on, a level as a fraction from 0 to 1.

| Op | Args | Effect |
|---|---|---|
| `car.goal.set` | `id`, or `charger_id` for a session whose car is unknown; `target` (a fraction) and `ready_by_min`, with `days` and `finish_at_limit`; `clear`; `surplus_only` | `target` and `ready_by_min` replace the goal, `clear: true` removes it, `surplus_only` sets the choice. What is absent stays as it was |
| `car.soc.set` | `id` or `charger_id`; `soc` | Sets the level as `manual`, now |
| `session.car.set` | `id` (the charger), `session`, `car_id` | Names the car on the charger. An empty `car_id` makes the session a visitor's |
| `car.add` | `id`, which the app mints, and the car's fields without the level | Adds a car, or updates the one with that id |
| `car.update` | `id`, the fields that change; `new_id` for a `boxMinted` car | Changes a car |
| `car.remove` | `id` | Removes a car and keeps its tombstone. A session on it becomes a visitor's |

The box's own page speaks HTTP, so it sends the same ops through routes
that hand the same arguments to the same checks and answer with the same
code and reason:

| Route | Op |
|---|---|
| `POST /api/cars`, the op's arguments as the body | `car.add` |
| `PATCH /api/cars/{id}`, the fields that change | `car.update` |
| `DELETE /api/cars/{id}` | `car.remove` |
| `POST /api/loadpoints/{id}/car` with `{session, car_id}` | `session.car.set` |

The first three are served only on the box's own page: a phone sends the ops,
with their id and expiry, and the session refuses these routes. The tap is
actuation, and a phone is told to send `session.car.set`.

`cmd.result` carries `car`, the car's id, level, goal and surplus choice as
the box holds them after the op, or `charger`, the charger's car after
`session.car.set`; `car.soc.set` also reads the level back in `observed`, as
a fraction. A result rides lane 0, so the app reads the rest of a car from
`GET /api/cars`; in a session whose lane 0 is 256 B a full car does not fit,
and the result comes without `car` or `charger`. A box keeps at most 32
cars and remembers the last 64 it removed. A car has at most eight
identifiers and eight sources, of at most 64 characters each. Refusals after
the ack: `E_NOT_FOUND` for an unknown car or charger, `E_PRECONDITION` with
reason `session` for a plug-in that has ended or `elsewhere` for a tap
naming a car another charger has, `E_UNAVAILABLE` with reason `unplugged`
when nothing is plugged in, `E_DUPLICATE`, and `E_CAP_EXCEEDED` with reason
`cars` for a car past the 32. A box without cars answers `E_UNAVAILABLE`
with `subsystem: cars`.

## car.which

The box raises it once per plug-in, as above. The event names the charger,
the plug-in's `session` in its args, and at most four cars that are not on
another charger. The phone shows one button per car, with the names from its
own table, and "None of these", which sends `session.car.set` with an empty
`car_id`. With more than four cars it shows no buttons and opens FTW. The
phone refreshes its table when a car is added. The notification rule is on
by default: until someone taps, the car charges as a visitor.

## Where cars appear

- The box's own page shows each car on its overview, plugged in or not,
  with its level, source and age, its goal and the charger it is on. A
  charger's notice asks which car it is while the box asks, and Settings
  has a Cars tab that keeps the list.
- `GET /api/cars` lists the cars, as above, and `removed`.
  `GET /api/loadpoints` gives each charger `session`, `car_id`,
  `car_identified_by`, `car_corrected`, `car_corrected_from` and `choose`.
  HTTP routes spell each registry name in snake_case; a test holds the two
  spellings together.
- The watch's `now` gives each charger `carId`, `carIdentifiedBy`,
  `carCorrected`, and the car's `socSource`, `socAgeS` and `socEstimate`
  beside `socPermille`; while the box asks, also `session` and `choose`.
  To fit the 512 B frame, car ids there are their 16 bytes, the 32 hex
  digits decoded, and `carCorrectedFrom` is left out. A charger asking with
  four cars takes about 180 B; two chargers asking at once with four cars
  each do not fit, and the last is left out with TRUNC, as for any `now`.
- FTW Cloud's `lp` carries the same charger fields with opaque car ids, and
  its `car` class carries each car's level and goal and nothing that names
  it ([protocol-cloud.md](protocol-cloud.md)). An agent's `loadpoints.get`
  and `cars.get` are the same bodies under the agent key.
- The plan a phone reads or is pushed names each charger's car in
  `chargers[].carId`; a change of car replans, and the plan reaches the
  phone pending at once.

## One release of overlap

Apps already shipped know only chargers. For one release the charger routes
and ops act on the session's car:

- `GET /api/loadpoints` shows the car's level as `current_soc`;
- `POST /api/loadpoints/{id}/target` and `PUT /api/loadpoints/{id}/schedule`
  set that car's goal and surplus-only choice;
- `loadpoint.soc.set` and `POST /api/loadpoints/{id}/soc` set its level;
- `loadpoint.surplus_only.set` sets its surplus-only choice;
- `POST /api/loadpoints/{id}/vehicle` sets its capacity.

With nothing plugged in, they act on the car the charger last had, or the
only car. While the session's car is unknown they act on the session alone,
never on a car. So do edits of `vehicle_capacity_wh`, `plugin_soc` and
`surplus_only` in config.yaml, by hand or on the box's settings page: they
change the car the charger shows.

The old fields map so that a shipped app never shows an estimate as the
car's own number:

| The car | Legacy `soc_source` | Legacy `current_soc` | `vehicle_soc`, `vehicle_soc_age_s` | Cloud `lp.socAgeS` |
|---|---|---|---|---|
| `vehicle`, exact | `vehicle` | The car's number | The car's number and its age | Its age |
| `vehicle`, estimate | `inferred` | The estimate | The car's number and its age | null |
| `manual` | `inferred` | The level | absent | null |
| `assumed` | `assumed` | The assumption | absent | null |
| A visitor's | `unknown` | absent | absent | null, and `socPermille` null |

`completed` is no longer sent; an app keeps reading it from older boxes.

Goals map both ways between `goal` and the charger's `schedule`:

| `goal` | `schedule` |
|---|---|
| `days` 0 (once) | `recurring` false |
| `days` 127 (every day) | `recurring` true, `days` 0 |
| any other `days` | `recurring` true, the same `days` |
| `readyByMin` | `time_of_day_min`, and `time_of_day_min_utc` as today's UTC minute |
| `targetPermille` | `soc` as a fraction |
| `finishAtLimit` | `finish_at_vehicle_limit` |

## Moving what exists

On first start with no cars, the box makes them from what it has. Nothing
the owner set is lost.

- **Car 1** takes the loadpoint's `vehicle_capacity_wh` and `plugin_soc`,
  the goal from `loadpoint_schedule:<id>` (mapped as above) and surplus-only
  from `loadpoint_surplus_only:<id>`. A box with more loadpoints makes Car 2
  and on from each of the others. These cars are `boxMinted`, and the
  loadpoint each came from is its usual charger (rule 3).
- **Each profile** in `vehicles[]` becomes a car with its identifiers,
  capacity, target and surplus-only, and an edit to the profile in
  config.yaml reaches its car. One charger and one profile is one car:
  the profile gives Car 1 its name, identifiers and capacity, and Car 1
  keeps the goal and choices the charger showed.
- **Each vehicle driver** that reports a VIN binds to the car with that VIN.
  When there is one car, every vehicle driver binds to it, and gives it its
  VIN and maker when the car has none. A driver the owner takes off a car
  stays off, and so do the drivers of a car the owner removes.
- The loadpoint's own rows keep being written, so a box rolled back to a
  release before cars finds its goal and choices where they were.
