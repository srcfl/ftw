# ADR 0009: An Apple Watch works without the phone

- Status: accepted. This repository has milestones 1, 2, 3, 5 and 6: the
  command log on the box, the registry names, the watch role with its
  enrolment, the exchange sessions, and the wire for FTW Cloud's one-shot
  read. The relay stopped on 17 October 2026, and its exchange socket went
  with it. [Through FTW Cloud](#through-ftw-cloud) (accepted by Fredrik on
  23 October 2026) moves the exchange onto FTW Cloud's one-shot request, where the
  box says `cmd.exchange` again.
- Date: 2026-10-14
- Decided by: Fredrik on 14 October 2026. Before the design: a watch controls
  the box directly as well as through the phone; an unlocked watch on the
  wrist counts as unlocked; each watch has its own key, which the owner can
  remove alone. On the design: yes to A–F below ("ja på alla"), and the watch
  must work fully on its own, for example on LTE with the phone away.
  Decisions 7–9 follow from that last answer and from his rule that FTW Cloud
  speeds things up and holds state while control never goes through it. The
  same day, after the timing test showed the gap, he set decision F's floor
  at 400 ms ("ja kör på ditt förslag ovan med tider").
- Pair: the exchange's wire, the relay route and the client changes are in
  [ftw-webapp `docs/watch-exchange.md`](https://github.com/srcfl/ftw-webapp/blob/main/docs/watch-exchange.md).
  Neither side works without the other.
- Builds on: [ADR 0006](0006-app-uplink.md), the relay as the only path that
  carries commands, and [ADR 0008](0008-ftw-cloud.md), FTW Cloud, which still
  never controls.

## Context

watchOS gives an ordinary app HTTPS through `URLSession` and nothing lower;
WebSocket and Network.framework are off limits
([TN3135](https://developer.apple.com/documentation/technotes/tn3135-low-level-networking-on-watchos)).
The relay and FTW Cloud's `/v1/app` both speak WebSocket. So the W1 watch app
hands everything to the phone, and nothing works when the phone is away.

Five facts shape the answer:

- Every session arrives on the one uplink, shares `MaxSessions = 4`, and a
  fifth evicts the oldest (`appuplink/session.go`). A watch session there
  would push a phone out.
- The command log lives in the handler (`cmds: newCmdLog()` in
  `appproto/handler.go`), and a handler lives as long as its session.
  ftw-webapp's `docs/protocol.md` and the comment on `CmdRetentionMs` say the
  box keeps a `cmdId` for 24 h. Today it keeps it for one session, so a retry
  in a new session would act again.
- `notValidAfterMs` counts in box uptime. A client learns the uptime from
  `hello_ok`, which travels on the bulk lane because its length varies.
- The roles are `owner` and `viewer`. Over a session a phone may mint viewer
  codes only; an owner code comes from the LAN, at home
  (`appLinkRoleAllowed` in `api/api_app_link.go`).
- FTW Cloud's app session is already one Noise IK handshake whose message 2
  carries the snapshot ([protocol-cloud.md](../protocol-cloud.md#app-session)).
  Only the WebSocket around it keeps a watch out.

## Decision

**An owner phone adds a watch, which is a device of its own and never holds
more than that phone. It sends commands to the box through a one-shot
exchange on the relay, and reads the home through a one-shot read at FTW
Cloud. Each step is one HTTPS request. The box's device list is the one place
to remove it.**

1. **An exchange socket.** `appuplink` opens
   `/r/<epoch>/<handle>/exchange` beside the uplink, with the same handle,
   epoch, rotation and backoff. Each message from the relay is
   `id(16) ‖ Noise message`, and each reply starts with the same id. The id is
   the relay's; Core copies it back and reads nothing else into it. The Noise
   prologue is `ftw.exchange.v1:` and the box's static key, so a stream
   handshake and an exchange handshake can never stand in for each other.
   Losing this socket changes nothing for the uplink, local control or the
   planner.

2. **Exchange sessions are a class of their own.** A table of at most 8,
   apart from the four stream sessions, so a watch never evicts a phone. A
   session ends after four exchanges, 60 s idle, or 3 minutes. Its handler
   takes `cmd` and `now`; `sub`, `api.req`, `hist.query`, `plan.get` and
   `price.get` get `E_UNKNOWN_OP`. It never streams and skips the plan push
   that follows an applied command on the stream.
   - The answer to message 1 is message 2. Its payload is `hello_ok` in the
     smallest bulk bucket it fits: proto, mode, clock, role, scopes, `rev`
     (the site's `controlRev`) and, while FTW Cloud is on, `cloud`. Every
     other frame is lane 0, 512 B.
   - The answer to `cmd` is two transport messages: `cmd.ack`, then
     `cmd.result`, or `tick` in its place if the result has not settled
     within 3 s. The same `cmd` again returns the stored outcome. The box
     answers a `cmd` no sooner than 400 ms after it arrived (decision F).
   - The answer to `now` is `now`, then `tick`: frozen fields 1–10, source
     states and ages, and each loadpoint's plug state, level, power and
     `controlRev`, flagged `TRUNC` if they do not fit. A read and a command
     have the same lengths.
   - At most 60 opening messages a minute; the box drops the rest unanswered.
     A stranger or a removed key gets no reply, as on the uplink.

3. **The command log belongs to the box.** It moves out of the handler, keyed
   by root device and `cmdId`, and keeps each entry 24 h of uptime, as
   protocol.md already promises. A watch's root device is the phone that added
   it, so one intent sent by both paths acts once. This fix stands on its own
   and lands first: today a phone's retry after a reconnect would act again.

4. **A watch is a device with role `watch`.**
   - Registry role `watch`: `ftw.live.read`, `ftw.plan.read`,
     `ftw.events.read` and `ftw.dispatch.write`.
   - `POST /api/app-link/pairing` accepts role `watch` only over a session,
     only from an owner, and only with `stepUp`.
   - The code is bound to the calling phone's device id and has its own slot,
     so minting it does not cancel a pending phone code.
   - The watch's `hello` carries the code in message 1. `Authorise` stores the
     watch's key with role `watch` and the phone as parent.
   - The live caller for a watch holds its role's scopes, minus any scope the
     parent does not hold at that moment. A watch never does more than its
     phone can do now.
   - Owner codes still come only from the LAN. A watch is not an owner.

5. **Removal runs from phone to watch, never back.** Removing a phone removes
   its watches and drops their sessions. Removing a watch touches nothing
   else. The LAN page and `GET /api/app-link/devices` list each watch under
   its phone. A watch holds no members scope, so it can neither add nor
   remove anyone.

6. **Capability `cmd.exchange`.** A box with the exchange socket and watch
   codes says so in `hello_ok`. Without it the phone offers no "Add watch".

7. **Reads go through FTW Cloud, in one request.**
   `POST https://cloud.ftw.energy/v1/app/once?home=<id>` with
   `content-type: application/octet-stream`. The body is IK message 1 to the
   service's Noise key, prologue `ftw.cloud.once.v1:` and the home id.
   - Message 1 carries one envelope with an empty `sig`: `read {have, hz}`,
     `token.get`, `push.register` or `push.unregister`. `have` names the
     `boot` and the highest `seq` per class the watch holds. `hz: 1` keeps
     the box's `watch` in force for 60 s, as a looking phone does. The
     service refuses owner messages such as `suspend`.
   - The service admits the watch as `/v1/app` admits a phone: its key in the
     roster, a roster that has not run out, no suspension. A refusal is 403
     with an empty body, before message 2.
   - Message 2 carries the answer: for `read`, the `snapshot` of
     `/v1/app` without the envelopes `have` already covers; for the others,
     the reply the app session gives. Envelopes that did not fit follow in the
     same body as transport messages, each after its length as u16 big-endian.
   - The service keeps nothing after the answer: no session, no Noise state.
   - Limits: one read at a time per device and 30 a minute; message 1 at most
     4 KiB; the answer within 10 s; 429 past a limit, 501 without a Noise key.
   - When FTW Cloud is off, the watch reads `now` through the exchange.
     Prices and the plan then wait for the phone or FTW Cloud.

   Reads through the exchange alone are worse: every glance would
   wake the box and cross the relay twice, and the plan and prices do
   not fit lane 0, so the bucket sizes would tell the relay which one was
   read. FTW Cloud already holds the same data, signed by the box, and a
   delaying or replaying service only ages it ([freshness](../protocol-cloud.md#freshness)).

8. **Complications, widgets and alerts without the phone.**
   - The watch app reads when it appears, after a command, and in a
     background refresh when watchOS grants one for an active complication.
     It keeps the verified envelopes and their anchors in the watch's App
     Group. Complications draw from that store with their age.
   - Widget push (watchOS 26). The watch registers its widget push token with
     `push.register {kind: widgets, app: watch}`. The service sends it the
     `widgets` push it sends phones, content changed and no data, when
     tomorrow's prices arrive and when a car is plugged in. The widget then
     reads `GET /v1/now` with the watch's own token. Tokens still read only
     `live`, `prices` and `hb`, so the car complication shows the app's last
     read with its age.
   - Alerts. The watch registers its APNs token with
     `push.register {kind: alert, app: watch}`. The service sends the watch
     the same payload it sends the phone; Apple shows one when a watch and its
     phone get identical payloads
     ([registerForRemoteNotifications](https://developer.apple.com/documentation/watchkit/wkapplication/registerforremotenotifications())).
     A notification service extension on the watch checks the box's signature
     and writes the text, as on the phone. Buttons run in the watch app and
     use the exchange.
   - `push.register` gains `app`, `iphone` or `watch`, so the service picks
     the APNs topic. Apple sees what it sees for phones, and no more.
   - The phone's Live Activity still shows on the watch when the phone is
     near. The watch gets none of its own.

9. **One identity in both doors.**
   - The watch's one key opens the exchange to the box and the read at FTW
     Cloud, as a phone's key does today.
   - The box lists the watch in its roster with role `watch` and the scopes
     the box derives. FTW Cloud holds no role table: its effective scope is
     those scopes intersected with the grant, as for any device.
   - Each watch has its own token (W plan decision 4, "rimligt"): `token.get`
     over the one-shot read, bound to the watch's roster entry, 7 days,
     renewed at each read.
   - Removing the watch on the box drops its exchange sessions at once, and
     its reads, token and push registrations at FTW Cloud within a second of
     the next roster arriving. If the box's uplink is down, FTW Cloud learns
     it when the box returns or when the roster runs out after 24 hours. An
     owner can suspend the watch at FTW Cloud from the phone until then.

## Through FTW Cloud

Accepted by Fredrik on 23 October 2026, after he asked to finish FTW Cloud. The
relay was stopped on 17 October 2026, and since the one-door decision of 15
October every phone reaches its box through FTW Cloud's tunnel. FTW Cloud
carries ciphertext and never chooses an action; that is the line this ADR
keeps, and the watch's commands now sit on the same side of it as the
phone's.

1. **The exchange rides the one-shot request.** A watch's exchange message
   goes to `POST /v1/app/once` as `x {d}`. The service sends the box
   `x {dev, id, d}` on the uplink and returns the box's `d` in message 2.
   Decisions 1 and 2 stand, apart from the socket: the sessions, their
   limits, the shapes and the 400 ms floor are the box's, unchanged. The
   wire is in [protocol-cloud.md](../protocol-cloud.md#exchange).
2. **The phone adds its watch by key.** Decision 4's code had to cross the
   service inside a handshake from a device the roster does not list yet.
   Instead the owner's phone sends the watch's public key over its own
   session, with the same step-up: `POST /api/app-link/pairing
   {"role": "watch", "key": …}`. The box lists the watch in its next
   roster. The code path is gone: a request for a watch without a key is
   refused, and a hello with a code is answered with silence.
3. **A watch has `tun`.** The roster gives a watch `tun` whenever it gives
   a phone `tun`, so the service admits its exchange with the readable copy
   off. The box answers a watch's key on the exchange alone.
4. **Reads stay as decision 7 says**, with one change: with the copy off a
   watch on its own shows what it holds and its age. Reading `now` through
   the exchange is not built.
5. **The exchange takes the charger's hold alone.** Decision B said a
   watch reads and charges. The watch role's `ftw.dispatch.write` reaches
   further, so the exchange answers any op but `loadpoint.hold` with
   `E_OP_NOT_GRANTED`.
6. **Replays end nothing, and a restart forgets.** A session proves itself
   with its first transport message before it writes anything, and a
   watch holds two. The command log lives in memory, so a box that
   restarted between a command and its retry would act again; the watch
   sees the uptime in `hello_ok` go back and does not resend.

The service learns that a watch of the home sent a request and when, and
when the box answered; the relay learned the same. One `cmdId` sent by the
phone and through FTW Cloud still acts once, because the command log keys
it by the phone that added the watch.

## What does not change

- Every exchange command takes the same `onCmd` path as the stream: expiry,
  scope, `rev`, dispatch gate, guards and read-back. Planner output never
  reaches hardware, and stale site-meter data still stops dispatch.
- FTW Cloud gains a read door and a device role. It sends the box nothing new
  and controls nothing.
- The relay stays blind. The paired doc lists what it reads and what it still
  knows.
- Local operation. The exchange and the read are remote doors; without the
  relay and FTW Cloud the box runs as before.
- Lane 0 keeps its fixed size. FTW Cloud stays unpadded, as ADR 0008
  decided.

## Accepted decisions

Fredrik answered yes to all six on 14 October 2026.

- **A. Transport.** A second socket from the box and an HTTPS route on the
  relay, with IK opened early, over a command in message 1, a mailbox the box
  polls, or a push through FTW Cloud.
- **B. What a watch may do.** Read, and charge. No site mode and no settings.
  Reads now include the plan and alerts, which standalone use needs.
- **C. Who adds a watch.** An owner phone, with one passkey step-up. A viewer
  cannot.
- **D. Removal.** Removing a phone removes its watches.
- **E. Order.** The watch tries the direct path first and the phone second.
- **F. Timing floor.** A refusal answers in milliseconds; an applied command
  waits up to 2 s for the EV save. The box answers no sooner than a fixed
  floor only if the timing test shows that gap through the relay.
  - Settled on 14 October 2026. The test showed the gap: with a 250 ms EV
    save, an applied charge answered in 252 ms and a refusal in under 2 ms
    through the relay. Fredrik chose a 400 ms floor. The box answers an
    exchange `cmd` no sooner than 400 ms after it arrived, applied or
    refused. A command that takes longer, an EV save of up to 2 s, answers
    when it is done and waits no more. Reads are not held.

## Milestones

Each step is one pull request with its own proof. A wire change lands in this
repository first, then in the repository that consumes it.

| # | Repository | What | Proof |
|---|---|---|---|
| 1 | ftw | The command log on the box (decision 3) | The same `cmdId` in a second session acts once; another device's same id is new; the table still prunes at 24 h |
| 2 | ftw-webapp, then ftw and ftw-app | Registry: role `watch`, capability `cmd.exchange` | Drift checks green in all three |
| 3 | ftw | Watch role and enrolment (decisions 4, 5, 9): the pairing route, parent, live scopes, removal by phone, watches in the roster | Role and removal tests below; the capability stays unsaid |
| 4 | ftw-webapp | The relay exchange: role, `/x/`, limits, README claim, Caddy log | Relay tests 1–5 in the paired doc |
| 5 | ftw | Exchange socket and sessions, `now`, `hello_ok` with `cloud`, `cmd.exchange` said | Replay, both paths, isolation, timing and vectors below; box, real relay and a Go client end to end |
| 6 | ftw | `cloudwire` and protocol-cloud.md: the one-shot read, `app` on `push.register`, role `watch` in the roster | Golden vectors for one read |
| 7 | ftw-cloud | `/v1/app/once`, watch push topics. Needs ADR 0008's token, `/v1/now` and widget push first | FTW Cloud tests below |
| 8 | ftw-app | The wire for watchOS: Noise, transport, frames, CBOR, rendezvous and FTW Cloud freshness | Swift replays the exchange and read vectors; `swift test` on the watchOS simulator |
| 9 | ftw-app | Add watch on the phone; direct commands on the watch, direct first, watch-minted `cmdId` | Device tests 6, 9, 10 and 11 in the paired doc |
| 10 | ftw-app | The watch on its own: FTW Cloud reads, `now` when FTW Cloud is off, complications, widget push, alerts | Device tests 7, 8 and 11 in the paired doc |

Step 1 needs nothing else. Steps 2 and 4 can run beside it.

## Consequences

- One more idle socket per box on the relay.
- Opening an exchange session costs the box up to four X25519 operations,
  bounded by the relay's limits and the 60 a minute here.
- The command log grows per household rather than per session, and still
  prunes at 24 h.
- A watch reads from FTW Cloud, so a household with FTW Cloud off gets
  commands and Now on the watch, but not prices, the plan or alerts.
- Four repositories change: this one, ftw-webapp (relay and registry),
  ftw-cloud (the read) and ftw-app (phone and watch).

## Tests

Core:

- **Command log.** As milestone 1.
- **Timing.** Time to answer for applied and refused commands, at the box and
  through the relay. The result settles decision F.
- **Replay.** A recorded message 1 draws a message 2 nobody else can use; a
  recorded `cmd` fails in its own session and returns the stored outcome in a
  fresh one.
- **Both paths.** One `cmdId` from the exchange and from the parent phone's
  stream acts once.
- **Role.** A watch may send `cmd` and `now`, and gets `E_UNKNOWN_OP` for
  `api.req`, `sub` and `hist.query` and `E_SCOPE_DENIED` for `site.mode.set`.
  Demoting its phone to viewer turns its next command into `E_SCOPE_DENIED`
  and narrows its scopes in the next roster.
- **Removal.** Removing the watch, or its phone, ends its sessions, silences
  its next message 1 and drops it from the next roster.
- **Isolation.** Eight exchange sessions leave four stream sessions running.
- **Shape.** `now` and `cmd` requests and answers have equal lengths for every
  op and outcome.
- **Vectors.** Swift and Go agree on the bytes of every step.

FTW Cloud:

- **Routes.** `/v1/app/once` is in `server.Routes`, and
  `TestNoRouteReadsAHome` covers it.
- **Admission.** The one-shot read admits exactly the devices `/v1/app`
  admits, run through the same table of cases.
- **Removal.** A watch the roster drops is refused within a second of that
  roster arriving.
- **Token.** A watch token reads `live`, `prices` and `hb`, and nothing else.
- **Alerts.** A watch and its phone get byte-identical alert payloads.
- **Memory.** After an answer, the service holds no session or Noise state for
  that read.
