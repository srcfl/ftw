# FTW Cloud protocol

Proposed with [ADR 0008](adr/0008-ftw-cloud.md). Nothing here has shipped.
This document is normative: "must" is a requirement on the box, the service
or the phone. The bytes live in `go/protocol/cloudwire`, with golden vectors
in its `testdata`: `cloud-vectors.json` for the uplink,
`cloud-app-vectors.json` for one whole app session, its
[tunnel](#tunnel) messages and a pairing proof, and
`cloud-once-vectors.json` for one [one-shot read](#one-shot-read),
`cloud-once-x-vectors.json` for one request of the [exchange](#exchange)
through the service, and
`cloud-shard-vectors.json` for which hosts a client may follow and how it
reads a restart's spread ([Shards and restarts](#shards-and-restarts)), and
`cloud-tile-vectors.json` for the three shapes of a [tile](#tiles-ftwhistoryread)
and how they lay back together.
`srcfl/ftw-cloud`, the phone and the watch replay them. `cloudwire.View` is the phone's
[freshness](#freshness) rules in Go, and the cases listed there are its tests.

The app protocol, which runs inside Noise between a phone and its box, is
specified in
[srcfl/ftw-webapp docs/protocol.md](https://github.com/srcfl/ftw-webapp/blob/main/docs/protocol.md).
FTW Cloud reuses its frame layout, CBOR rules, field encoding and tile format.
It adds the `cloud.link` capability and one optional field to the app
protocol's `hello_ok`, `cloud` (see
[Session information](#session-information)), and changes nothing else.
Assistants, Sourceful support and the release check reach the box as agents
through the [tunnel](#tunnel); [Agents](agents.md) specifies their roles,
scopes, approval and messages. Lane 0 and the relay stay as they are. The Go types
for both live in `go/protocol`, under Apache-2.0
([#1588](https://github.com/srcfl/ftw/pull/1588)).

## Parties and connections

- **Box:** FTW Core linked to FTW Cloud. A box links when it first shows a
  pairing code, an owner lets an agent in or an owner turns the readable copy
  on, and stays linked while any phone or agent may reach it (see
  [Link](#link)).
- **Service:** `ftwcloudd`, at `cloud.ftw.energy`.
- **Phone:** the iPhone app, paired with the box over Noise as today.
- **Extension:** a widget, notification service extension or Live Activity
  on that phone.
- **Watch:** an Apple Watch that an owner's phone added as a device of its
  own ([ADR 0009](adr/0009-watch-exchange.md)). watchOS gives it HTTPS and
  nothing lower, so it reads in one request.

| Connection | Opened by | Transport | What protects it |
|---|---|---|---|
| Uplink | Box | `wss://cloud.ftw.energy/v1/box` | TLS 1.3, a handshake bound to the TLS session on both sides, every box message signed |
| App session | Phone | `wss://cloud.ftw.energy/v1/app?home=<id>` | TLS 1.3, then Noise IK to the service's static key. Inside it, the phone's own Noise session with its box rides as [`tun`](#tunnel) |
| One-shot read | Watch | `POST https://cloud.ftw.energy/v1/app/once?home=<id>` | TLS 1.3, then Noise IK to the service's static key, inside one request. Inside it, a watch's [exchange](#exchange) with its box rides as `x` |
| Extension read | Extension | `GET https://cloud.ftw.energy/v1/now` | TLS and an extension token; the data inside is box-signed |
| Push | Service | APNs | Box-signed payloads where they carry data |

**Uplink rules.**

- The endpoint is a compile-time constant in Core, as the relay's is.
- TLS 1.3 is the minimum. The dialer uses no proxy, whatever the environment
  says.
- No proxy may end TLS in front of the service, because that breaks the
  handshake binding. A plain TCP load balancer is fine.
- Reconnects back off with full jitter from 0.5 s to 60 s. The attempt
  counter resets after 60 s connected. Two closes are not failures: 4421
  names the instance that holds the home, and 4503 asks the box to come back
  within a spread (see [Shards and restarts](#shards-and-restarts)).
- The service sends a WebSocket ping every 15 s. The box's read deadline is
  45 s and its write deadline 10 s.
- WebSocket compression is off.
- A box keeps one uplink. A second one from the same home replaces the first.

## Frames

Each WebSocket binary message carries exactly one frame in the app
protocol's layout:

```
offset  field    type     value on FTW Cloud
0       ver      u8       1
1       lane     u8       1, always
2       flags    u8       0
3       rsvd     u8       0
4       len      u16 BE   payload bytes
6       payload  u8[len]  CBOR envelope
```

- **No padding.** In the app protocol, lane 1 pads to bulk buckets of 1, 4 or
  16 KiB. FTW Cloud uses lane 1 without the buckets: the message is exactly
  `6 + len` bytes. Lane 0 never appears on an FTW Cloud connection.
- A receiver closes the connection on a lane 0 frame, a nonzero flag, bytes
  after `len`, or a text message.
- Inside the app session the same frames ride as Noise transport messages,
  one frame per message, also without padding.

The app protocol pads lane 0 to a fixed size and cadence because the relay
must learn nothing from the stream. The service reads the content, so padding
would hide only size and timing from the network, at about three times the
data. The network sees when messages go and how large they are, which shows
which class changed and when. The privacy notice says so (ADR 0008,
decision 5).

## QUIC

The uplink and the app session also run over QUIC (RFC 9000) on UDP 443 at
`cloud.ftw.energy`, with the same certificate as TCP. The messages and every
rule in this document stay the same; only the framing below changes. The
WebSocket stays too, and a client falls back to it when UDP does not get
through. The one-shot read and the extension read are plain HTTPS, which the
service also serves over HTTP/3 (RFC 9114); they need nothing here.
`go/protocol/cloudwire/quic.go` holds the names and the codec, and
`cloud-quic-vectors.json` the bytes.

| ALPN | Connection | WebSocket it stands for |
|---|---|---|
| `ftw-cloud-box/1` | Uplink | `/v1/box` |
| `ftw-cloud-app/1` | App session | `/v1/app?home=<id>` |
| `h3` | HTTP/3 | — |

The service offers only the ALPNs it serves. With the sessions turned off it
offers `h3` alone, so a client's QUIC handshake fails at once and it takes
the WebSocket.

- **TLS 1.3 inside QUIC.** `ekm` is the exporter of that TLS session, with the
  label and length of [Handshake](#handshake), and a box binds `hello` to it
  as it does over TCP. Nothing may end QUIC in front of the service; a load
  balancer that forwards UDP packets is fine.
- **Message.** One WebSocket binary message becomes one message: its length
  as a QUIC variable-length integer (RFC 9000 §16), then its bytes. The size
  limits of the WebSocket apply. A length over them closes the connection
  with 4400.
- **Control stream.** The client opens bidirectional stream 0 and sends the
  *target* first, as one message: the path and query the WebSocket would name,
  such as `/v1/app?home=<id>`, in ASCII. The service checks it as it checks
  the WebSocket's URL. After it, every message of the connection goes on this
  stream, both ways, except those below.
- **Bulk stream.** A sender may open one unidirectional stream and send on it,
  in order, the `tun` messages whose `d` carries a lane 1 frame of the
  phone's session: snapshots, history and answers too large for lane 0. It
  opens it the first time it has bulk to send. The box already tells bulk
  from lane 0 for its pacing (see [Backpressure](#backpressure-and-limits)).
  The service sends a `tun` that came on the box's bulk stream on the phone's
  bulk stream. A lost bulk packet then never holds up a command, its answer
  or a tick. A phone sends everything on the control stream.
- **Datagrams (RFC 9221).** The box sends `hb`, and `live` without `sources`,
  as one QUIC DATAGRAM each when it fits, and otherwise on the control stream.
  The service sends those same envelopes to a phone the same way, each as one
  Noise transport message. A datagram is never sent again, and the newest
  wins. The [freshness](#freshness) rules already cover one that is lost or
  late: a phone sees the gap through `hb.liveSeq` and ages what it holds
  instead of showing it as live. A `live` that carries `sources` goes on the
  control stream, because a lost table would leave sources stale until the
  next full `live`. Nothing else goes as a datagram, and tunnel traffic never
  does: lane 0 carries commands, their answers and deltas that must arrive,
  and two paths for lane 0 would show the service which frames are which.
  - The service closes the uplink with 4400 on a datagram that is not `hb`
    or a `live` without `sources`. It keeps an `hb` or `live`, on either
    path, only when its `seq` is above the newest of its type on this
    connection, and drops a late one, with one exception: a late `live`
    whose `srcRev` is above the table it holds still gives its table, and
    nothing else. A phone does the same (Freshness case 17), so a table
    overtaken by a datagram is not lost.
  - Each end offers datagrams in its transport parameters. A sender whose
    peer does not uses the control stream.
  - Until a phone's first transport message arrives, the service sends it
    everything on the control stream, behind message 2. quic-go and other
    stacks send datagrams ahead of stream data, so an early `hb` could
    otherwise reach a phone before the message 2 it needs to read it.
- **Order.** Messages on one stream arrive in order; across streams and
  datagrams they may not. Noise transport carries its own counter, one for
  all three paths, so a burst on the bulk stream can overtake a control
  message by a few hundred. A receiver of a session that can travel over
  QUIC keeps a window of at least 1024 (`appwire.ReplayWindow`), so the app
  session and the tunnel take messages out of order. It drops a datagram
  that falls outside the window or repeats; the same on a stream ends the
  session, as over the WebSocket. The service's rule to forward a home's
  messages in the order it received them holds per stream.
- **Streams.** A peer that opens a second bidirectional stream, or a second
  unidirectional one, breaks the protocol. The service grants one of each.
- **Closing.** A close code becomes the QUIC application error code, with
  the WebSocket close reason as the reason phrase. 1000 closes normally.
- **Liveness.** QUIC stands in for the WebSocket ping. The client sends a QUIC
  PING after 15 s with nothing to send, which keeps a NAT's UDP mapping
  open; many drop one after 30 s. Both ends close a connection silent for
  45 s, as the WebSocket's read deadline does.
- **Migration.** A client may move its connection to a new address (RFC 9000
  §9): a phone from Wi-Fi to cellular, or a box whose NAT rebinds. The
  service validates the new path and follows it. The Noise sessions inside
  never notice: no handshake, no new `sid`.

### Racing the WebSocket

A client starts QUIC first. If the QUIC handshake has not finished after
250 ms, it also dials the WebSocket, takes the first that opens and closes
the other. It remembers which won on each network for a day and starts the
next time with that one, giving the other the same 250 ms; after a day QUIC
goes first again, so a network that blocked UDP once is tried again. A QUIC
dial that loses to the WebSocket goes on for up to 3 s, only to learn: if it
opens, QUIC goes first next time, so one lost packet does not keep QUIC
second for a day. The session stays on the WebSocket either way.

A QUIC handshake that finishes proves less than a session that works. A
QUIC connection that ends before the first session message (the `challenge`
for a box, message 2 for a phone), or that the path drops within 60 s of
it, counts as QUIC failing on that network: the client starts there with
the WebSocket for a day, and a learning dial in that day does not move QUIC
first. A close the service sends on purpose, with a code from 1000 up, and a
stateless reset after a restart, do not count. A box names its network by
its local address; a phone by its path's interfaces and gateways, kept as a
hash. A client
says which carries it, by the registry's `cloud_transports`: `quic` or
`websocket`. Core shows it in `GET /api/cloud`; the app shows it beside the
carrier. The carrier stays `tunnel` or `cloud` over both.

### Early data

A client that holds a session ticket from the service may send 0-RTT data. It
may put there only the target and, on the app session, Noise message 1, with
`now` and the box session's message 1 in `now.tun`. Nothing else can go
there: every later message is a Noise transport message, which the client
can encrypt only once message 2 has come back. So no `cmd` or `api.req` ever
rides in early data. A box puts only its target there, and the service's
`challenge` comes back in its first flight.

The network can replay early data. These rules make a replay change nothing:

- **A phone's session waits for the handshake.** The service reads a phone's
  message 1 from early data but acts on it only once the QUIC handshake has
  finished, which the sender of a replay cannot do. Until then nothing is
  forwarded, opened or answered: no session is replaced, no pairing code
  spent, no `watch` sent. This holds for the first copy too, which matters
  when the network dropped the original and the phone dialled again.
- **A box's target is all it sends early.** The service may answer it with a
  `challenge` before the handshake ends. A challenge changes nothing, and
  only the box can answer it with a `hello`.
- **For a day.** A ticket allows early data for 24 hours after the service
  issued it, and resumption without early data after that. A restart draws
  new ticket keys, so every client makes a full handshake once.
- **The box holds a tunnel session as pending** until its first transport
  message decrypts (see [Tunnel](#tunnel)), so a `now.tun` takes no slot
  and spends no code, whoever sent it.
- **HTTP/3 requests wait for the handshake**, since no plain route has said
  which of its requests survive a replay.

A client whose early data the service rejects sends it again once the
handshake is done.

## Envelope and signature

```
Envelope = { t: text, b: bstr, sig: bstr }
```

- CBOR, core deterministic encoding, text keys. A map with a repeated key is
  refused.
- An empty list or map is `[]` or `{}`, never null. A number the box does not
  know is `null`. Of the box's fields, only `wallMs`, `sources` and
  `fieldSources` are ever left out.
- `b` is the body, itself deterministic CBOR, carried as a byte string so that
  the signature covers exact bytes and the service can forward them unchanged.
- `sig` is ES256 (ECDSA on P-256 with SHA-256), 64 bytes, `r ‖ s`
  big-endian.
- **Signed input:** the bytes `ftw.cloud.v1\0` (12 ASCII bytes and a zero
  byte), then the deterministic CBOR encoding of the array `[home, t, b]`,
  where `home` is the text home id.
- The box signs with `cloud.key`, every envelope but `tun`. The service signs
  `hello_ok` and `purged` with its service key, over the box's home id. Every
  other envelope the service sends, `tun` in both directions, and every
  envelope between phone and service has an empty `sig`. A `tun` needs none:
  its `d` is Noise ciphertext that the phone and the box authenticate.
- A receiver verifies over the bytes it received, never over a re-encoding.
- The box's public key reaches phones over Noise, beside the tables of opaque
  ids, the home id and the box's current `boot`, in the
  [session information](#session-information) every role receives. It never
  comes from the service.
- Receivers ignore unknown keys in a body, as in the app protocol. The service
  drops and counts an unknown `t` from the box, so a newer box works with an
  older service. An unknown `t` from the service closes the uplink.
- The service forwards a home's messages in the order it received them.

## Keys and ids

| Name | What it is |
|---|---|
| `cloud.key` | ES256 (P-256) key the box makes when it links (see [Link](#link)). The box never backs it up |
| `pk` | Its public key as a 65-byte uncompressed SEC1 point |
| `home` | Lowercase base32, no padding, of the first 16 bytes of SHA-256(`ftw.cloud.home.v1` ‖ `pk`). 26 characters |
| Service key | P-256. Core carries up to two public halves, so the key can rotate across a release |
| Service Noise key | X25519 static key. The app carries up to two |
| `grantKey` | 32 random bytes in the grant record. A new grant draws a new one |
| Opaque source id | First 8 bytes of HMAC-SHA256(`grantKey`, `src\0` ‖ driver name), base64url without padding |
| Opaque loadpoint id | First 8 bytes of HMAC-SHA256(`grantKey`, `lp\0` ‖ loadpoint id), base64url without padding |
| Opaque car id | First 8 bytes of HMAC-SHA256(`grantKey`, `car\0` ‖ car id), base64url without padding. The car id is the same on each of the owner's boxes; the opaque id is not, so the service cannot follow a car from one home to another |

A home id comes only from the key that signed `hello` on this connection,
never from a body field.

### Rotating the service keys

The service key signs every `hello_ok` and every downlink message. A box
trusts only the public halves its build carries (`cloudlink.ServiceKeys`).
Release builds carry one: the key `cloud.ftw.energy` holds in Secrets
Manager as `ftw-cloud/service-key`. To rotate it:

1. Make the new key where only the service can read it, beside the old one.
   Put its public half into Core next to the old one, so a release carries
   both.
2. Ship that release and wait for boxes to update. Until the switch, every
   box checks the service against the old key.
3. Switch the service to the new key. Boxes on the new release accept it.
   A box on an older release refuses the service and keeps running on its
   own until it updates.
4. Drop the old public half in a later release.

If the key leaks, ship a release with only the new key at once. To stand in
for the service, an attacker would also need the TLS certificate for
`cloud.ftw.energy`.

The service's Noise key rotates the same way through the app, which carries
up to two and tries the next when the service refuses message 1.

## Common body fields

Every body the box sends carries these four fields. The class tables below do
not repeat them.

| Field | Type | Meaning |
|---|---|---|
| `seq` | uint | Starts at 1 and rises by 1 per message, per boot, across reconnects |
| `boot` | bstr, 16 B | Random, drawn at each Core start |
| `uptimeMs` | uint | Box monotonic time since start, at send |
| `wallMs` | uint | Box wall time at send. Present only while NTP has synced the box clock |

Other wall-time fields, such as `readyByMs` and `t0Ms`, come from the box's
clock. While the box reports `clock: "none"`, the phone shows them as
unverified.

## Handshake

Both sides bind the handshake to the TLS session with its exporter value
(RFC 8446 §7.5), label `EXPERIMENTAL-ftw-cloud-v1`, empty context, 32 bytes,
called `ekm` below. A proxy that ends TLS in the middle holds two TLS sessions
with two different values, so neither side's check passes through it.

1. **Service → box: `challenge`** `{v: 1, nonce: bstr32}`, unsigned.
2. **Box → service: `hello`**, signed. The service computes `home` from `pk`,
   checks the signature, that `nonce` is its own and that `ekm` equals its own
   exporter value, or closes.

   | Field | Type | Meaning |
   |---|---|---|
   | `v` | uint | 1 |
   | `pk` | bstr, 65 B | The box's public key |
   | `nonce` | bstr, 32 B | Echo of the challenge |
   | `ekm` | bstr, 32 B | The box's exporter value for this TLS session |
   | `boxNonce` | bstr, 32 B | Random, for the service to sign |
   | `build` | text | Core version |
   | `clock` | text | `ntp` or `none` |
   | `caps` | [text] | The classes this box sends under the grant at connect, `roster` included, and `tun` when the box takes the [tunnel](#tunnel) |
   | `rosterSeq` | uint | The box's current roster number |
   | `grantId` | bstr, 16 B or empty | The grant at connect. Empty while the readable copy is off or a revoke waits for its receipt |

3. **Service → box: `hello_ok`**, signed by the service key over the box's
   home id.

   | Field | Type | Meaning |
   |---|---|---|
   | `boxNonce` | bstr, 32 B | Echo |
   | `ekm` | bstr, 32 B | The service's exporter value for this TLS session |
   | `have` | map text→text | Tile id to etag, for the closed tiles the service holds |
   | `rosterSeq` | uint | The roster number the service holds |
   | `statsPolicy` | text, optional | The statistics policy this service is approved to ingest. Missing means no statistics; the box sends none |

   `caps` and `grantId` describe the connection as it starts. The readable
   copy can turn on or off while the link stays up, so the service takes
   what it may hold from the newest roster's grant, never from `hello`, and
   enforces neither per connection; only `tun` in `caps` decides whether it
   sends the box `tun`.

   The box checks the signature against a service key it carries, that
   `boxNonce` is its own and that `ekm` equals its own exporter value. If any
   check fails, the box closes and backs off. **It sends nothing but `hello`
   before this check passes.** A proxy that reads `hello` learns the box's
   public key and grant id, and cannot use them.
4. **Box → service:** a `roster`, at every connect (see [Roster](#roster));
   then one full message of each class the grant allows; then the trailing
   tile whole and the closed tiles missing from `have` (see
   [`tiles`](#tiles-ftwhistoryread)). While the link waits for a purge, the
   box sends only `bye` instead (see [Revocation](#revocation-and-deletion)).
5. After that the box sends on change, by the cadence rules.

## Classes

The box sends these. Each class belongs to one scope, and the service
forwards a class only to devices whose effective scope includes it. These
tables are the allowlist: a field the box sends must have a row here, and a
test fails on any field without one. `cloudwire.Fields` holds the same
tables, and `TestFieldsMatchTheProtocolDocument` compares the two. It reads
the rows under each class heading, each bold shape name and each numbered
handshake step, so keep that layout. Driver names and device capabilities
have no rows. Source kinds and counts, health counts and the statistics `has`
list still show what kinds of equipment a site has.

### `live`: `ftw.live.read`

| Field | Type | Meaning |
|---|---|---|
| `fields` | map text→int | Frozen field ids 1–6 and 10, and the extra fields 11–14 when the site meter gives phases, keyed and encoded as the app protocol's `delta` does: field 1 is the mode index, power in W, phase currents and the main fuse in tenths of an ampere |
| `fieldSources` | map text→[text] | The sources each field's freshness follows. Sent only with `sources` |
| `sources` | map text→Source | The source table. Sent whenever `srcRev` changes, and in every full message |
| `srcRev` | uint | Rises by 1, per boot, each time the source table changes |
| `ages` | map text→uint | For each source in state `live` or `lagging`, the age of its newest reading at send, in ms of box uptime |
| `dispatchBlockedBy` | [text] | Sources whose staleness stops dispatch |

**Source**

| Field | Type | Meaning |
|---|---|---|
| `kind` | text | As in the app protocol |
| `state` | text | `live`, `lagging`, `stale`, `down` or `never`, as the box grades it |
| `lastOkMs` | int | Box uptime at the last good reading, when the table was sent |
| `staleAfterMs` | uint | How long a reading from this source stays current |

### `lp`: `ftw.live.read`

| Field | Type | Meaning |
|---|---|---|
| `controlRev` | uint | The box's control revision, as a `cmd` guard needs it |
| `loadpoints` | [Loadpoint] | One per loadpoint |
| `feedback` | [Feedback] | One per device function FTW commands |

**Loadpoint**

| Field | Type | Meaning |
|---|---|---|
| `id` | text | Opaque loadpoint id |
| `src` | text | Opaque source id of the charger, whose freshness the loadpoint follows |
| `plugged` | bool | A car is connected |
| `socPermille` | uint or null | Car charge level, null when unplugged and for a visitor's car, whose level nobody knows |
| `socSource` | text | `vehicle` (the car's own number, exact), `inferred` (an estimate, or a level someone set), `assumed`, or `unknown` (a visitor's car); empty when unplugged |
| `socAgeS` | uint or null | Seconds since the car last reported the level, at send. Null unless `socSource` is `vehicle` |
| `targetPermille` | uint or null | The level to reach |
| `readyByMs` | int or null | Wall time the car should be ready |
| `recurring` | bool | The target comes from a weekly schedule |
| `powerW` | int | Charging power, positive while charging |
| `sessionWh` | uint | Energy delivered this session |
| `session` | text | This plug-in's id, the same across a restart of the box; empty when unplugged. A box from before cars sends the number of plug-ins since it started, which names one plug-in only with `boot` |
| `manual` | Manual or null | Manual charging state, null when none |
| `commandedW` | int or null | Power last commanded |
| `commandedReason` | text | Stable code for that command |
| `surplusOnly` | bool | Charging from surplus solar only |
| `gridDeferred` | bool | Grid charging deferred by the plan |
| `planPending` | bool | A replan is due |
| `boost` | bool | Battery boost is on |
| `nextStartMs` | int or null | Wall time the next planned charge starts |
| `nextEndMs` | int or null | Wall time it ends |
| `nextWh` | uint or null | Energy it plans to deliver |
| `carId` | text, optional | Opaque car id of the car on the charger. Absent while nothing is plugged in or the box does not know which car it is |
| `carIdentifiedBy` | text, optional | How the box knows: `charger`, `car`, `only`, `tap`, `asking` or `none` ([docs/cars.md](cars.md)) |
| `carCorrected` | bool, optional | A better rule replaced the car the box named first |
| `carCorrectedFrom` | text, optional | Opaque car id of the car the box named before that |
| `choose` | [text], optional | While `asking`: opaque ids of the cars a tap may pick, at most four, none on another charger |

With cars ([docs/cars.md](cars.md)), the level fields of a Loadpoint
(`socPermille`, `socSource`, `socAgeS`, `targetPermille`, `readyByMs`,
`recurring`, `surplusOnly`) are views of the session's car, kept for apps
that know only chargers. The join is `carId` to the `car` class's `id`, and
where the two disagree the `car` class wins. The box fills the views from
the car's own: `socPermille` as the car's; `socSource` `vehicle` for the
car's exact reading, with its `socAgeS`, `assumed` for an assumed level,
`unknown` with no level, and `inferred` otherwise; `targetPermille` and
`recurring` (`days` not 0) from its goal; `surplusOnly` as the car's; and
`readyByMs` as the next ready time of that goal. A box drops the views two
releases after the one that first sends `car`, and from then lists
`lp.bare` in `hello.caps`: its Loadpoints carry no level fields. A box from
before cars never lists it.

**Manual**

| Field | Type | Meaning |
|---|---|---|
| `state` | text | The manual state |
| `sinceAgeMs` | uint | Time in that state, at send |
| `requestedW` | int or null | Power the user asked for |
| `commandedW` | int or null | Power commanded for it; null when none or zero |
| `reason` | text | Why the hold is limited, as a stable code |

**Feedback**

| Field | Type | Meaning |
|---|---|---|
| `kind` | text | The device function, such as battery or charger |
| `src` | text | Opaque source id |
| `status` | text | `following`, `waiting`, `limited`, `not_following`, `no_contact` or `not_controlled` |
| `reason` | text | Stable code |
| `severity` | text | As on the box |
| `evidence` | text | `none`, `accepted`, `measured` or `confirmed` |
| `fresh` | bool | The readings behind the status are fresh |
| `actualW` | int or null | Measured power |
| `ageMs` | uint or null | Age of that reading by box uptime, at send |

The change test ignores `socAgeS`, `sinceAgeMs` and `ageMs`; the phone adds
the time since the message. It compares measured values with a deadband,
because a charging car moves its power at every reading:

- `powerW` and `actualW` count as changed when they move by more than
  max(100 W, 5 % of the larger value), or `actualW` appears or goes;
- `sessionWh` when it moves by 100 Wh or more;
- `socPermille` when it moves by 10 or more, or appears or goes.

Any other difference counts as a change. The full `lp` every 15 minutes
carries exact values, and live field 10 carries the site's charging power at
every period. Without the deadband, a simulated house with a car charging at
11 kW sent 12.7 MB a day.

### `car`: `ftw.live.read`

The level and goal of each of the owners' cars on this box
([docs/cars.md](cars.md)). Nothing here names a car: the phone reads the
name, maker, VIN, identifiers and sources from the box over Noise.

| Field | Type | Meaning |
|---|---|---|
| `cars` | [Car] | One per car the box keeps |

**Car**

| Field | Type | Meaning |
|---|---|---|
| `id` | text | Opaque car id |
| `socPermille` | uint or null | The car's level, null when the box has none. The car's or a person's own number while `socEstimate` is false |
| `socSource` | text | `vehicle`, `manual` or `assumed`; empty without a level |
| `socAgeS` | uint or null | Seconds since the car reported the level or someone set it, at send |
| `energySinceWh` | int | Energy delivered to the car since then; negative when a V2X session took energy out |
| `socEstimate` | bool | The box counts the level as an estimate |
| `chargeLimitPermille` | uint or null | The charge limit the car reported with its level |
| `goal` | CarGoal or null | What the car should reach and when |
| `surplusOnly` | bool | The car charges from surplus PV only |

**CarGoal**

| Field | Type | Meaning |
|---|---|---|
| `targetPermille` | uint | The level to reach |
| `readyByMin` | uint | Ready time in minutes after midnight in the box's time zone |
| `days` | uint | ISO weekday mask, Monday in bit 0. 0 is once, at the next ready time, and 127 every day |
| `finishAtLimit` | bool | Charge on until the car stops at its own limit |

The box sends `car` when a car's level, source, goal or preference changes,
at most once per period like `lp`, and in full every 15 minutes and after a
`resync`, as `lp`. The change test ignores `socAgeS`, counts
`energySinceWh` as changed at 100 Wh, as `lp` counts `sessionWh`, and
`socPermille` at 10 or when it appears or goes, as `lp` counts its level, so
an estimate that rises with a charging car does not send the class at every
reading. Whether the level is an estimate counts at once. The box lists
`car` in `hello.caps` while it keeps cars and the grant has
`ftw.live.read`. A service that does not know the class drops it, as for any
unknown `t`.

### `prices`: `ftw.live.read`

| Field | Type | Meaning |
|---|---|---|
| `zone` | text | Price zone |
| `currency` | text | ISO 4217 code |
| `t0Ms` | int | Wall time the first slot starts: the first stored slot from the start of today on the box's clock |
| `slotMs` | uint | Slot length. Where stored slots differ in length, the box repeats a longer slot's price across `slotMs` slots |
| `spotMinor` | [int or null] | Market price, minor units per kWh |
| `totalMinor` | [int or null] | What the household pays, tariff and VAT included |

`totalMinor` shows the household's tariff, and the privacy notice says so. The
phone sees how far the slots reach. The window runs from the start of today to
the end of tomorrow. Because it starts at midnight and not now, its hash moves
only when new prices land. The box looks at prices once a minute and sends
`prices` only when a hash of `t0Ms`, `slotMs`, `spotMinor` and `totalMinor`
changes. A gap between stored slots is `null`.

### `plan`: `ftw.plan.read`

| Field | Type | Meaning |
|---|---|---|
| `decisionId` | text | The plan's id. The box sends `plan` only when it changes |
| `genMs` | int | Wall time the plan was made |
| `stale` | bool | The planner could not run |
| `fallback` | bool | Core's Go fallback made this plan, not the solver |
| `t0Ms` | int | Wall time the first slot starts |
| `slotMs` | uint | Slot length |
| `batteryW` | [int or null] | Battery power per slot, site convention |
| `gridW` | [int or null] | Grid power per slot |
| `socPermille` | [uint or null] | Planned battery level at slot end |
| `evW` | [int or null] | Planned charging power |
| `pvLimitW` | [int or null] | Planned solar limit, null where none |
| `priceMinor` | [int or null] | Import price, minor units per kWh |
| `reason` | [text] | Stable code per slot |

The grid ceiling the plan defends is not sent.

### `health`: `ftw.health.read`

| Field | Type | Meaning |
|---|---|---|
| `ok` | uint | Drivers working |
| `degraded` | uint | Drivers working with faults |
| `faulted` | uint | Drivers failing |
| `offline` | uint | Drivers not answering |

### `event`: `ftw.events.read`

| Field | Type | Meaning |
|---|---|---|
| `eventId` | text | Unique per event |
| `ts` | int, optional | The box's wall time of the event, in ms. Present only while NTP has synced the box clock. It can be earlier than `wallMs` when the event waited for the uplink. The phone dates the alert by the earlier of this and the send time |
| `kind` | text | A kind from `contract/push-catalogue.yaml` |
| `severity` | text | As on the box |
| `args` | map text→(text or int) | The catalogue's placeholders, except names |
| `src` | text | Opaque source id, when the event concerns a device |
| `lp` | text | Opaque loadpoint id, when the event concerns a loadpoint |
| `cars` | [text], optional | Opaque car ids, for `car.which`: the cars the phone offers one button each, in the box's order. At most four; with more cars it is absent and the phone opens FTW. `args.session` is the plug-in's `session`, which the tap names |

No argument carries a driver, device or car name. Where the catalogue text
has a name placeholder, such as `{name}` in `driver.offline`, the box sends
`src` and the phone fills in the name from its table. `car.which` names its
cars the same way. The box sends an event only
when its own rules (enabled, cooldown) allow. Delivery is best effort.

### `tiles`: `ftw.history.read`

The box's 5-minute history of the last 48 hours, so a phone can draw it while
the box cannot answer. The body is the app protocol's `hist.chunk` body:

| Field | Type | Meaning |
|---|---|---|
| `tileId` | text | `5m/1/<index>`: the 12-hour tile, at stride 1, as the app protocol names it |
| `etag` | text | FNV-1a of `data`, as the app protocol computes it |
| `res` | text | `5m` |
| `startMs` | int | Wall time of the first point. The tile's start, except in an append |
| `stepMs` | uint | 300,000 |
| `series` | [text] | `grid_w`, `pv_w`, `battery_w` and `load_w`, in that order and no other: the series the app asks for over Noise |
| `data` | bstr | int32 LE, one block per series, the app protocol's missing value where the box has no record |
| `partial` | bool | The tile is still filling |

A body is one of three shapes. `cloudwire.Tiles.Check` holds them, and
`cloud-tile-vectors.json` has one of each. A tiles envelope is at most 4 KiB
(`TileEnvelopeMax`); a whole tile is about 2.6 KB.

- **Closed:** `partial` false, `startMs` the tile's start, all 144 points.
  It has the same tile id, etag and bytes as the box serves over Noise for a
  query of those series at 5 minutes.
- **Whole:** `partial` true, `startMs` the tile's start, all 144 points, with
  the missing value from the first bucket that has not closed. It replaces
  every piece of that tile the receiver holds.
- **Append:** `partial` true, `startMs` a later bucket, and only the buckets
  that closed since the last piece of that tile: `data` holds as many points
  per series as fit. The receiver lays it over what it holds.

The box counts a bucket closed 30 s after it ends, when the history writer
has committed it. Then:

- It sends an append for the trailing tile, or the tile whole when no piece
  of it has gone on this connection or after 11 appends, so the service holds
  at most 12 pieces of it and the box rereads it once an hour.
- When the tile's last bucket closes it sends the closed tile instead, and
  nothing for the next tile until a bucket of it closes.
- Once an hour it packs the closed tiles of the last 48 hours again and
  sends any whose etag changed, as when the ledger repaired a gap.
- At each connect, after the full state, it sends the trailing tile whole
  and every closed tile of the last 48 hours that `hello_ok.have` lacks at
  that etag, newest first. `resync {what: [tiles]}` sends the tiles it names,
  or all of them when it names none, whatever `have` says.
- When `ftw.history.read` leaves the grant it forgets what it sent, since the
  service drops what it held; when it comes back, the box sends every tile
  again, as at connect.

The box reads its ledger at most once per tick, one tile at a time, so a
reading never waits long behind a pass. It sends tiles only while NTP has
synced its clock, since tiles are dated by wall time, and only under a grant
with `ftw.history.read`, which it checks again before each one. They count
against the byte budget: about 100 KB a day.

The service keeps each tile until 48 hours after it ends, so up to 60 hours
of history, and drops them all when the roster runs out. A phone reads them
only while the roster is in force, so with the box away that is 18 to 24
hours after the box was last heard. The service sends the box
`resync {what: [tiles]}`, at most once per 5 minutes, when a roster gives
history back and when room frees after it dropped a tile for lack of it.

`cloudwire.Lay` puts what a receiver holds back together: a closed tile as
it is, and the trailing tile from its newest whole piece with each later
append laid over it, in the order the box sent them, by `wallMs` and then
`seq`, since `seq` starts again when the box does. A later closed tile
replaces an earlier one, never an older one a later. The trailing tile stays
partial, and its etag is never offered as held.

### `hb`: every device in the roster

| Field | Type | Meaning |
|---|---|---|
| `clock` | text | `ntp` or `none` |
| `srcRev` | uint | The source table revision now in force |
| `liveSeq` | uint | `seq` of the newest `live` the box has sent |
| `lpSeq` | uint | `seq` of the newest `lp` the box has sent, 0 if none |
| `ages` | map text→uint | As in `live`, at the time of this `hb` |

### Service-only classes

The service never forwards these to a phone.

**`roster`**

| Field | Type | Meaning |
|---|---|---|
| `rosterSeq` | uint | Persisted on the box. Rises by 1 at each pair, role change, revoke, grant change or pairing code shown |
| `ttlS` | uint | 86,400. Counted from when the service received the roster |
| `devices` | [Device] | Every device paired with the box |
| `grant` | Grant | What the owner granted |
| `pending` | [Pending] | One phone QR code and up to 16 agent invitations |
| `noise` | bstr, 32 B | The box's X25519 Noise static key, the one in its pairing QR code. FTW Cloud's `keys` part opens an agent's session to it, and trusts it only as signed here |

| `webpush` | [WebPushRow], optional | Box-encrypted browser offline notices. Service only; no browser key leaves the box |

**WebPushRow**

| Field | Type | Meaning |
|---|---|---|
| `id` | text | Subscription id, scoped to this home |
| `dev` | text | Current roster device id |
| `epoch` | uint | Its current grant epoch |
| `endpoint` | text | HTTPS address at a supported browser push vendor |
| `ct` | bstr | RFC 8291 encrypted box.unreachable payload |
| `auth` | text | Box-signed VAPID Authorization, renewed within six hours |

At most 16 rows and 32 KiB of encoded rows fit in one roster. Each ciphertext
has at most 1,024 bytes, endpoint 2,048 bytes and Authorization 512 bytes.

The service sends these only after the box has been away ten minutes, using
the same timer and update grace as APNs. It checks the current device, epoch,
events scope and suspension before each send. It needs no readable-copy grant.
The rows never reach another phone. It stores only a sent marker per outage
and subscription; purge removes it. Removed subscriptions leave the next roster.
A deploy drains and starts no offline alert.

**Device**

| Field | Type | Meaning |
|---|---|---|
| `id` | text | The box's device id |
| `key` | bstr, 32 B | The device's X25519 Noise static key |
| `role` | text | `owner`, `viewer` or `watch`; for an agent row `agent`, `support` or `release` |
| `scopes` | [text] | Scopes the box derives from the role. For a watch, only those its phone's role also carries now. Empty for an agent row: an agent reads the box, never the copy. The service holds no role table |
| `epoch` | uint | The device's epoch on the box |
| `tun` | bool | The device may open a session to the box through the service's doors. True for a phone, false for a watch and for an agent row. It needs no grant |
| `agent` | map, optional | Only on an agent row: its grant, below. See [Agents](agents.md) |

**Agent**

| Field | Type | Meaning |
|---|---|---|
| `scopes` | [text] | The grant's scopes, within what its approving owner holds now |
| `ops` | [text] | The ops the grant allows |
| `caps` | map | `holdS`, `maxW` and `perMin`, below |
| `expiresAtMs` | int | Wall clock. The service stops admitting the row then by its own clock, without waiting for a roster |
| `principal` | map | `client`, `clientId` and `ticket`, below |

**Who may open an agent's session.** Only the service's own `keys` part, the
one place that holds agent keys, opens a session for an agent row, and only
for a call through MCP that the agent's OAuth token allows. The service must
never admit an agent row on `/v1/app`, `/v1/app/once` or the relay, whatever
its `tun` says, and an agent row never reads the readable copy. It may
tighten the caps but never loosen them. The box checks everything again on
every call.

**Caps**

| Field | Type | Meaning |
|---|---|---|
| `holdS` | uint | The longest hold, in seconds; 0 without `ftw.dispatch.hold` |
| `maxW` | uint | The largest power a hold may set, in W |
| `perMin` | uint | `cmd` and `agent.request` per rolling minute |

**Principal**

| Field | Type | Meaning |
|---|---|---|
| `client` | text | The client's name, from FTW Cloud's consent page |
| `clientId` | text | The client id |
| `ticket` | text | The support ticket, for `support` |

**Grant**

| Field | Type | Meaning |
|---|---|---|
| `id` | bstr, 16 B or empty | Grant id. Empty while the readable copy is off |
| `scopes` | [text] | A subset of `cloud_scopes`. Empty while the copy is off or being withdrawn |
| `purposes` | [text] | A subset of `cloud_purposes`: `serve` while the copy is on, and `stats` while the owner contributes to statistics. Empty while the copy is off or being withdrawn |
| `statsId` | bstr, 16 B, optional | Random id for the separately accepted statistics policy; omitted when off |
| `statsPolicy` | text, optional | Exact policy the owner accepted |
| `termsPolicy` | text, optional | Cloud terms accepted by the owner; the current Cloud policy includes statistics |
| `statsSinceMs` | uint, optional | Consent start in UTC milliseconds; only later closed hours may contribute |
| `controller` | text | The data controller: `sourceful` unless a partner pairing flow sets another. No such flow exists yet |

A grant has three shapes. With an id and `serve`, the readable copy is on.
With an id and no purposes, the owner has withdrawn that copy: the service
deletes what it holds under the grant and answers `purged {what: "copy"}`,
each time it receives such a roster. With no id, there is no copy.

**Pending**

| Field | Type | Meaning |
|---|---|---|
| `hash` | bstr, 32 B | Phone pairing hash below, or the agent invitation verifier below |
| `kind` | tstr, optional | Absent for a phone; `agent` admits only the pending-agent invitation path |
| `ttlS` | uint | Seconds the code has left, counted from when the service received the roster |

For a phone, the hash is SHA-256(`ftw.cloud.pair.v1\0` ‖ home id ‖ pairing code).
Only a phone QR code is listed. A spoken code has 40 bits, and its hash would let
the service find it offline; it pairs over the relay or the home network.

A new iPhone may exchange a short code directly with the box on the home
network through `POST /api/app-link/pairing` with `kind: "claim"`. The box
replaces that code with a full 128-bit enrollment token, keeping its role and
expiry, and returns the same v3 link as the QR. Only the full token's hash
enters the roster above. The exchange keeps the pairing route's local-address,
house-password and browser-origin checks and refuses app sessions. See
[Pair with one iPhone](pairing.md) for the steps.

**`stats`**

Signed box to service only. Never forwarded to phones or agents. The box
sends at most one report per closed UTC hour, after fresh consent to
`ftw-project-stats-2026-10-v3`. Older policies send nothing.

| Field | Type | Meaning |
|---|---|---|
| `statsId` | bstr, 16 B | Must match the current signed grant |
| `policy` | text | Exact statistics policy version |
| `hourMs` | uint | Closed UTC hour start, no older than 24 hours and not before consent |
| `metrics` | map(text, uint) | Allowlisted values, rounded to 10; missing means unknown |

Keys: `battery_capacity_wh`, `flexible_power_w`, `pv_rated_w`,
`grid_import_wh`, `grid_export_wh`, `pv_generation_wh`, `battery_charge_wh`,
`battery_discharge_wh`, `vehicle_charge_wh`, `vehicle_discharge_wh`. Each
value is at most 1,000,000. Flexible power sums configured battery and charger
limits, with shared inverter groups counted once; it does not prove physical
control. Energy is recorded ledger energy and may cover only part of an hour.
No names, hardware ids, location, prices or live readings leave this path.

The service upserts by home and hour, keeps these consented rows for 48 hours,
and deletes them when it receives statistics withdrawal or a home purge.
Only cells with at least 20 homes and no home above 30 percent of the sum
are public. The public view counts reporting installations, never people.
Partner-controlled sites do not send.

**`notice`**, whenever the box is linked, with the readable copy on or off.

| Field | Type | Meaning |
|---|---|---|
| `kind` | text | One of `agent.added`, `agent.request`, `agent.expiring`, `approval.key.set`, `owner.added` |
| `client` | text, optional | On `agent.added` only, while the readable copy is on and its grant has `ftw.events.read`: the client's name as FTW Cloud's consent page showed it: at most 65 characters (the page keeps 64 and marks a cut name with "…"), and no control or format character. Absent otherwise |

A security notice: someone gained access, asks for a change, is about to
lose access, or can now approve access. The service checks that the body
holds nothing beyond the header, `kind` and `client` (`cloudwire.ReadNotice`)
and sends each owner device registered for alerts the notice as it arrived,
in an `alert` (see [Push](#push)). The phone words it from its catalogue. A
notice of these kinds never goes as an `event`, so an owner gets one alert,
not two; a phone shows an event of these kinds as the generic line. A notice
states a lasting fact, so Apple may hold it and the phone shows it for a day
(`NoticeMaxAgeMs`), where an event lasts 10 minutes. The box keeps at most 16
notices for the uplink and counts any it drops (`noticesDropped` in
`GET /api/cloud`). The name in `client` is not new to the service: the
roster's agent row carries it whatever the copy (`principal.client`); the
copy decides only whether it also reaches Apple. See [Agents](agents.md).

**`bye`** `{why, purge}`. `why` is `revoked`, `shutdown` or `update`.
`purge: true` asks the service to delete the home. `why: "update"` holds
back `box.unreachable` until the box returns or an hour passes.

## Cadence

- The box keeps the latest value per class and sends a class only when its
  content changed. The change test ignores `lastOkMs`, as the app protocol's
  source comparison does, and the age fields named under `live` and `lp`.
  `lp` adds the deadband described there.
- Each class goes at most once per period: 5 s by default, 1 s while a
  `watch` is in force. `lp` goes at most every 2 s until a Pi 4 bench shows
  what composing it costs. The box reads its state no faster than a message
  could go.
- Two messages skip the period. The box looks at its source table every
  second, and a `live` whose table changed goes then, with `sources` and
  `fieldSources`. An `hb` goes when it is due.
- While a `watch` is in force, a driver poll that changed `live` sends it at
  once if the last `live` left a period ago or more, and otherwise when that
  period ends. So the copy costs what it did, and a reading from a device
  read once a second waits for nothing.
- `plan` goes only when `decisionId` changes, `prices` only when its hash
  changes, `tiles` when a bucket closes, and `event` at once.
- Every 15 minutes, and after a `resync`, the box sends a full `live` with
  `sources` and `fieldSources`, a full `lp` and a full `car`.
- **Heartbeat.** The box sends `hb` whenever neither `live` nor `hb` has left
  for H, where H = clamp(the smallest `staleAfterMs` among sources ÷ 2,
  2 s, 15 s), and 15 s when there are no sources. It sends no `hb` while a
  changed `live` or `lp` waits for its period, so an `hb` never vouches for
  values the box has not sent.

## Service to box

The service may send the box only these. Anything else, a `tun` frame over
`MaxTunFrame` (16,664 bytes), any other frame over 4 KiB, a text message or
a lane 0 frame closes the uplink. A fuzz test covers it.

| `t` | Body | Rule |
|---|---|---|
| `challenge` | `{v, nonce}` | First message. Unsigned |
| `hello_ok` | See Handshake | Signed by the service key |
| `watch` | `{hz: 1}` | Raises the period to 1 s for 60 s. While any phone of the home looks, the service sends it at once, every 50 s after that, and again when the box connects. Without it the box returns to 5 s. The box ignores any other `hz` |
| `resync` | `{what, tiles}` | `what` holds `full`, `tiles` or `roster`; `tiles` lists tile ids, and none means every tile of the last 48 hours. The box honours at most one per 5 minutes and ignores the rest. The service sends one for tiles when a roster gives history back, and when room frees after it dropped a tile |
| `purged` | `{atMs, what, grantId, statsId}` | Signed by the service key. `what` is `home`, `copy` or `stats`. Sent once every deletion it names is done |
| `tun` | `{dev, sid, d}` | A phone's own session with the box. See [Tunnel](#tunnel). Only to a box that lists `tun` in `hello.caps` |

The box acts on none of these beyond what the table says. It takes no time,
config, update or instruction from the service. What a `tun` carries is the
phone's, encrypted to the box's static key; the box decrypts it and decides
as it decides for the same session over the relay.

## Backpressure and limits

- One writer per socket. While a write blocks, the newest value per class
  replaces the one waiting. Events wait in a ring of 32; when it is full the
  oldest drops and the box counts the drop.
- A write blocked for 10 s closes the socket.
- A reconnect sends full state and the missing tiles. It replays no old
  `live`, `lp` or `event`.
- **Byte budget.** The box counts the bytes it sent over a rolling 24 hours
  of uptime. Past 20 MB, four times the measured 5 MB, it sends only `event`,
  `notice`, `roster`, `stats.withdraw` and `bye` until the count falls. It sends no
  `hb` then, so data on phones ages to stale rather than looking current.
  `GET /api/cloud` shows the count and whether the budget is reached.
- **`tun` has its own queues and budget.** `tun` waits for the socket apart
  from the readable copy, in two queues. A `tun` carrying a lane 0 frame (a
  command, its answer, a tick) goes first and is never held; bulk is paced
  to nine tenths of the service's [tunnel limits](#tunnel), so the service
  never drops for this box, and waits its turn. The queues hold 64 and 256;
  the newest is dropped when one is full. They do not count against the
  20 MB, so a phone at work never stops `hb`. The box counts them against
  250 MB per 24 hours of uptime, and past that sends no `tun` until the count
  falls.

## Roster

- The box sends a roster at every connect, on every change within one control
  tick, and every 6 hours. Each one renews `ttlS`, so a box that returns after
  20 hours offline does not have 4 hours left.
- If `hello_ok.rosterSeq` is above the box's own, the box moves its number past
  the service's and sends a new roster. A box whose state went back in time
  therefore cannot undo revokes the service already holds.
- The service accepts a roster whose `rosterSeq` is at least the one it
  holds. An equal number renews `ttlS`. A lower one is ignored.
- When `ttlS` runs out with no newer roster, the service closes the home's
  sessions with 4403 and refuses new sessions and token reads until a roster
  arrives.
- **Effective scope** of a device is its `scopes` intersected with
  `grant.scopes`. It is empty while the readable copy is off, and a device
  with `tun` still opens a session for its [tunnel](#tunnel).
- A watch has `tun` whenever a phone may have it. For a watch it opens the
  [exchange](#exchange) and nothing else: the box answers a watch's key on
  the stream with silence.
- A device that leaves the roster loses its sessions (4403), tokens and APNs
  registrations within a second of the service receiving the roster, which
  needs the uplink to be up. A device whose `epoch`, `role` or effective scope
  changes has its sessions closed with 4409, so it reconnects and learns its
  new access. A new `epoch` also drops its tokens and APNs registrations.
- The service learns devices only from the roster. It never adds one.

## App session

1. The phone opens `wss://cloud.ftw.energy/v1/app?home=<id>` over TLS 1.3,
   with no `Origin` header (the web app's is `https://app.ftw.energy`, the one
   origin the service accepts), and runs `Noise_IK_25519_ChaChaPoly_SHA256` as
   initiator. The responder's static key is a service Noise key built into
   the app. The prologue is `ftw.cloud.v1:` followed by the home id, which the
   phone learned from the box (see [Session information](#session-information)).
   The query only says which home to look in: the prologue binds it, so a
   query that names another home fails the handshake.
2. Handshake message 1 carries an unsigned `now` envelope, `{hz, pair, tun}`:
   `hz` is 1 while the phone is looking at the home, 0 while it is not. `pair`
   is only for a new phone; see
   [Pairing through the service](#pairing-through-the-service). `tun`, `{sid,
   d}`, is optional and carries message 1 of the phone's session with its
   box. The service forwards it as soon as it admits the phone, so that
   session is up one round trip after TLS.
3. The service admits the phone only if the static key that message 1 proved
   is in the home's roster, with `tun` or while the readable copy is on, the
   roster has not run out, and no owner has suspended the device; or if
   `pair` matches a pending entry. Every refusal closes with 4403 before
   message 2.
4. Message 2 carries an unsigned `snapshot` envelope, below.
5. After that the service forwards each new box envelope the device may read,
   unchanged and in the order the box sent them, one unpadded lane 1 frame per
   Noise transport message. It takes the snapshot and starts the stream under
   one lock, so no envelope falls between them.

**`snapshot`**

| Field | Type | Meaning |
|---|---|---|
| `envelopes` | [Envelope] | Box envelopes in rising `seq`, each the box's own CBOR map, unchanged: for each class the device may read but `tiles`, the newest one the service holds; the newest `live` that carried `sources`; and the newest `hb`. Empty after an hour without the box, and while the readable copy is off. A phone asks for tiles with `tiles.get` |
| `lastSeenMs` | int | Service wall time when it last heard from the box |
| `online` | bool | The box's uplink is up now |
| `more` | uint | How many envelopes did not fit in message 2. They follow it as ordinary frames, in rising `seq` |

The phone accepts the envelopes that follow as it accepts any forwarded
envelope. An envelope too large for one Noise message is not forwarded; the
phone sees the gap through the next `hb`.

**Watch.** While at least one phone of a home looks, the service keeps the
box's `watch` in force: at once, every 50 s, inside the box's 60 s, and when
the box connects. When no phone looks, it stops, and the box returns to 5 s
within 60 s.

**Phone to service.** These use the envelope with an empty `sig`. The
service acts on these and ignores other types; a reply has the type named in
the table.

| `t` | Who | Effect |
|---|---|---|
| `watch` | Any device | `{hz}`: 1 when the phone looks at the home, 0 when it looks away. It holds until the next `watch` or the end of the session |
| `tun` | A device with `tun` | `{sid, d}`. The phone's session with its box. See [Tunnel](#tunnel). The service answers `tun` the same way |
| `tiles.get` | Devices with `ftw.history.read` | `{have}`, tile id to etag of the closed tiles the phone holds. The service answers with the `tiles` envelopes the phone lacks: each closed tile it holds that `have` does not name at that etag, and every piece of the trailing tile, oldest tile first and each tile's pieces in the order the box sent them. Then `tiles.end {n}`, where `n` counts them. It answers this while the box is away too, and a device without the scope gets `tiles.end {n: 0}` |
| `token.get` | Any device | Answered with `token {token, expiresInS}`. A new token replaces the device's old one |
| `push.register` | Any device | `{apnsToken, environment, kind, activityId, app}`. One registration per APNs token; a Live Activity and its push-to-start each register their own. `app` is `iphone`, the default when absent, or `watch`, and picks the APNs topic |
| `push.unregister` | Any device | `{apnsToken}` |
| `suspend` | Owner | `{deviceId}`. Suspends a device |
| `unsuspend` | Owner | `{deviceId}`. Lifts a suspension |

A suspension can only narrow access, never beyond what the roster allows. The
service ignores one from a viewer and one that names a device outside the
roster. Suspending a device closes its sessions with 4403. A suspension holds
until an owner lifts it, or the roster drops the device or changes its
`epoch`, and it works while the box is offline, until the roster runs out.
Anything the service says itself, such as `lastSeenMs`, carries no box
signature, and the phone never shows it as a reading.

**Close codes.** The service ends a session with one of these. A refusal says
nothing about which check failed.

| Code | Meaning | The phone |
|---|---|---|
| 4400 | The phone broke the protocol | Retries with backoff |
| 4403 | Refused: no such home, a device not in the roster, a suspended device, a roster that ran out, a device that may neither read nor open a tunnel, a purged home, or a handshake to another service key or home | Before message 2, tries the app's other service key once, at once. Otherwise asks again slowly |
| 4408 | A newer session from the same device took over | Does not retry |
| 4409 | The roster changed what this device may read | Reconnects at once, once; then retries with backoff |
| 4429 | The home has as many sessions as the service allows, as at the relay | Retries with backoff |
| 1013 | The phone fell too far behind | Retries with backoff and gets a fresh snapshot |
| 4421 | Another instance holds the home. The reason is its host | Follows once and keeps the host for the home (see [Shards and restarts](#shards-and-restarts)) |
| 4503 | The service is about to stop. The reason is the spread in milliseconds | Redials at a random moment within the spread, without backoff |

- **Backoff** is full jitter from 0.5 s to 60 s, as on the uplink. It also
  covers a lost socket.
- **Slowly** is a random delay between half and all of min(15 s × 2ⁿ, 5 min),
  where n counts the refusals in a row: 7.5 to 15 s at first, at most 5 min.
  A refusal changes only when the roster or grant does, or when a service
  that restarted has heard from the box again.
- The phone dials at once, whatever it waits for, when the app comes to the
  front or the network changes.

**Limits.** Per home, 16 sessions. Per device, one session; a newer one
replaces the older with 4408. Per phone, 256 envelopes queued, past which the
session closes with 1013. A `tun` message larger than `MaxTunFrame` and its
Noise overhead, any other phone message larger than 4 KiB, a text message,
or more than 20 messages other than `tun` in a burst and then 2 a second
closes the session with 4400. `tun` has limits of its own (see
[Tunnel](#tunnel)). Message 1 must arrive within 10 s. Pings and deadlines
are the uplink's. A restarted service reads the rosters it holds back from
its registry, so it admits a home's phones before the box has connected
again (see [Service state](#service-state)).

### Session information

Every role learns what it needs to read FTW Cloud from the box, over its own
Noise session, in the app protocol's `hello_ok.cloud`.

| Field | Type | Meaning |
|---|---|---|
| `pk` | bstr, 65 B | The box's `cloud.key` public key. Every box envelope from the service must verify with it |
| `home` | text | The home id, cut from `pk` |
| `boot` | bstr, 16 B | The uplink's current `boot`. It counts from the same Core start as `hello_ok.clock.uptimeMs`, which anchors rule U |
| `sources` | map text→text | Opaque source id to driver name |
| `loadpoints` | map text→text | Opaque loadpoint id to loadpoint id |
| `cars` | map text→text, optional | Opaque car id to car id. Absent from a box without cars |

- The box sends `cloud` while it is linked. It leaves it out when it holds
  no `cloud.key`, while the link waits for a purge, and in floor mode.
  Without it the phone reaches the box through the relay only.
- `sources`, `loadpoints` and `cars` are cut with the grant's `grantKey`, so
  they are empty while the readable copy is off.
- The tables name the drivers and loadpoints the box knows at `hello`. One
  added later has no name on the phone until its next session with the box.
- The box lists `cloud.link` in `hello_ok.caps` when its build can run the
  uplink. A build that names no service does not.
- It also lists `cloud.tun` when its link carries the phones' own sessions
  as [`tun`](#tunnel). A phone sends its box session through the service
  only to a box that said so at its last hello, or to one it has never
  reached when a v3 QR named the home. Otherwise it uses the relay.

## Tunnel

The phone's own session with its box, `ftw.session.v1` (Noise IK to the
box's static key, as the app protocol specifies), rides inside the app
session. Its frames and buckets do not change, so the service sees when a
message goes, not what it says or how large a command is. Its timing does:
lane 0 goes as readings land, not on the relay's one-second pulse.

### Lane 0 through the tunnel

- When a driver finishes a poll, the box sends each visible session one
  frame: a `delta` of what changed, or a `tick` when nothing did. It waits
  5 ms for polls that finish with it, so they go in one frame, and sends at
  most five such frames a second on average, three at once.
- When nothing has gone for the subscribed period, 1 s while the phone shows
  the stream and 5 s while it is hidden, the box sends a `tick`, or a `delta`
  if something changed. A hidden session gets only these.
- A source table too large for the bucket goes in a snapshot on bulk, sent
  in line with lane 0 so no reading sent after it reaches the phone first.
  Every lane 0 frame keeps its bucket.
- Past 60 % of the tunnel's 24-hour budget a session gets the beat alone,
  and past 90 % no readings, so handshakes, command answers and ends still
  go.
- Over the relay nothing changes: one frame per pulse, every 1 s or 5 s.

**What the service learns.** It cannot read a frame, and every lane 0 frame
is the same size. While a phone watches, a frame goes on every finished
poll, changed or not, so their timing shows when the box reads its devices
and never which reading moved, by how much or which way. A snapshot is
larger, and goes when the control revision or the source table changes.
With the copy off, the poll rhythm is new to the service, for the minutes
someone watches. With the copy on, the service reads in `live` the values of
the fields the grant lists, and the rhythm adds when the box read them.
While no phone has a session nothing goes, and a hidden session sends every
5 s whatever happens.

### `tun`

| Field | Type | Meaning |
|---|---|---|
| `dev` | text | The roster device id whose session this is |
| `sid` | uint | The phone's number for one session with the box, below 2³², drawn at random for each new session so that the late end of an older one never ends it |
| `d` | bstr | One message of that session, byte for byte, or empty to end it |

On the app session the phone and the service send `tun {sid, d}`, with no
`dev`. On the uplink the service and the box send `tun {dev, sid, d}`.

- **The service names the device.** It sets `dev` from the roster entry its
  own handshake with the phone matched, and ignores a `dev` a phone adds. It
  never forwards a phone's `tun` to another home or under another device.
- **The box checks it again.** A session starts with handshake message 1 of
  the box session under a new `sid`. The box runs the handshake and checks
  the phone exactly as it checks one over the relay: a known key, or a live
  pairing code in the payload. If the key the handshake proved is not the
  device `dev` names, the box drops the message and answers nothing. A `d`
  for a `(dev, sid)` the box does not hold that is not message 1 gets an empty
  `d` back, so the phone starts again.
- **A session proves itself first.** The service sees every message 1 and
  could replay one under a new `sid`. So the box answers a message 1 but
  holds the session as pending: it takes no device's place, stamps nothing
  and spends no pairing code until its first transport message decrypts. A
  replay never gets that far. The same holds over the relay.
- **One session per device.** A session that proves itself, through the
  service or the relay, ends the device's older one.
- **Ends.** An empty `d` ends `(dev, sid)`. An end for a session a newer
  one replaced looks the same as any other, so a phone ignores an end for
  any `sid` but its current one and never restarts on it; if it did, it
  would fight its own newer session. An end for its current `sid` means the
  box dropped it, and the phone starts again with backoff. The box sends an
  end when it drops a session; the service sends one to the box when the phone's app session
  ends, and one to the phone when the box's uplink ends or is replaced by a
  newer one, when the box does not take `tun`, or when the phone names a
  session the box does not hold.
  Every tunnel session ends with the uplink that carried it.
- **What the service can do.** Drop or delay a `tun`. It cannot read or forge
  `d`, and Noise transport refuses a replay. Every `cmd` carries its own
  expiry.
- **Order.** The service forwards a device's `tun` in the order it received
  them.
- **Limits.** `d` is at most 16,408 bytes, one transport message around the
  largest bulk bucket; a phone that sends more is closed with 4400. Per home
  and per direction the service passes 240 messages in a burst and then 60 a
  second, and 4 MiB in a burst and then 512 KiB a second, the limits the
  relay puts on a box's uplink. Past them it drops `tun` and counts the drop;
  at twice them it closes the box's uplink, or the phone's session. `tun`
  does not count against the uplink's own message rate. A box handshake
  through the tunnel costs the box up to four X25519 operations, so the box
  answers at most 12 a minute per device and 240 in all.

### Agent invitations through the service

An owner creates a single-use agent invitation through the signed app session
or the password-protected box page. The roster carries its verifier with
`kind: "agent"`, for at most 24 hours. The verifier is
SHA-256(`ftw.agent.invite.v1\0` ‖ home id ‖ `\0` ‖ 32 random secret bytes).
No code, label or recipient name enters the roster.

This entry never admits a phone through `/v1/app`. OAuth gives each recipient
its own pending key in `ftwkeysd`. `store.For(AgentInvite)` opens only a tunnel,
within the agent-session quota, with no access to the readable copy. Opening
or losing this tunnel does not spend the verifier. The keys service's separate
`open.invite` reads only pending keys and accepts only a typed claim with its
own signed offer for that key. Ordinary `open` still needs a kept key.

Noise message 1 carries `ftw.agent.invite.v1\0` followed by strict CBOR
`{secret, offer: {o, kid, sig}}`. Core checks the code, current owner, expiry,
offer signature and the Noise static key, then checks them again after the
first transport frame proves the key. It atomically saves the consumed
invitation and the ordinary agent row. Rights and limits intersect the
owner's invitation and the recipient's request. A retry must match that
invitation, key and signed offer; it cannot revive a revoked agent.

Core reads its devices and pending entries in one snapshot. Its next signed
roster removes the invitation hint and lists the new agent. Only this roster
allows OAuth to keep the key and issue its normal PKCE authorization code.
The code's claim deadline and the agent's access expiry are separate. The
box must be online to claim a code. The owner-facing six-character code is a
short-lived Cloud alias for the strong proof; it never enters a roster or
replaces the Core verifier. See [Agent invitations](agents.md#agent-invitations).

### Pairing through the service

A new phone is in no roster, so the service would refuse it. While the box
shows a QR code, its roster lists the code's hash in `pending`. The phone
scans the code, computes the same hash and puts it in `now.pair`. The service
admits the handshake once for that hash, removes the entry and names the
device by its device id: the first 8 characters of its static key in
base64url, which is how the box names it too. The device may then send only
`tun`. The box decides as over the relay: the code rides inside handshake
message 1, encrypted to the box, so the service never sees it. When the
roster that lists the new device arrives, the session takes that entry
without closing. If the pairing fails, the session ends when the entry's
`ttlS` runs out.

**The QR, v3.** A phone that has never reached the box needs the home id
before any session with it. Pairing payload `v3` is `v2` with a sixth segment:

```
https://app.ftw.energy/p#v3.<box_noise_pub>.<pairing_code>.<lan_hint>.<rendezvous_secret>.<home>
```

`<home>` is base64url, without padding, of the home id's 26 ASCII
characters, like the hint. The other five segments are `v2`'s, byte for byte,
and everything stays in the fragment. The box shows `v2` until one constant,
`appenroll.QRNamesHome`, turns `v3` on. It stays off until the apps parse
`v3` in builds people have: ftw-app's `Enrollment.swift` and `Enrollment.kt`
and ftw-webapp's `enrollment.ts` each take exactly five `v2` segments today,
so an earlier `v3` would stop pairing on every shipped app. Until then a new
phone learns the home id from `hello_ok.cloud` on its first session with the
box, which the box sends from its first pairing on.

### Link

- A box links when it first shows a QR pairing code, when a device that may
  open a tunnel is paired, when an owner lets an agent in, or when an owner
  turns the readable copy on. It makes `cloud.key` then, and the home id with
  it. An agent row has no tunnel, but FTW Cloud learns of it only from the
  roster, and an owner with no phone grants one on the box page
  ([agents.md](agents.md)).
- A linked box sends its roster and takes `tun`. It sends no readings until
  the owner turns the readable copy on.
- The link ends when no paired device may open a tunnel, no agent row is
  left, no code is shown and the readable copy is off. The box then sends
  `bye {why: "revoked", purge: true}` and deletes `cloud.key` on the
  `purged {what: "home"}` receipt. The next link has a new home id.
- A build that names no service never links.
- For every linked home the service sees a stable home id, the box's address
  and when traffic goes, with the readable copy off too.

## One-shot read

A watch cannot open a WebSocket, so it reads with one HTTPS request
([ADR 0009](adr/0009-watch-exchange.md), decision 7). The service admits it
exactly as `/v1/app` admits a phone and keeps nothing after the answer.

1. The watch posts to `https://cloud.ftw.energy/v1/app/once?home=<id>` over
   TLS 1.3, with `content-type: application/octet-stream` and no `Origin`
   header. The body is Noise IK message 1 and nothing else: the watch is the
   initiator and a service Noise key the responder, as on the app session.
   The prologue is `ftw.cloud.once.v1:` followed by the home id, so a
   one-shot handshake and an app session handshake never stand in for each
   other.
2. Message 1 carries one unsigned envelope: `read`, `token.get`,
   `push.register`, `push.unregister` or `x` (see [Exchange](#exchange)).
   The service refuses any other type, an owner's `suspend` among them,
   with 400 before message 2.
3. The service admits the watch by the rules of the app session: its key in
   the home's roster, a roster that has not run out, no suspension, and a
   non-empty effective scope, so the readable copy must be on. For `x` the
   roster entry's `tun` stands in for the effective scope, as it does for a
   phone's tunnel, so commands work with the copy off. A refusal is 403 with
   an empty body, before message 2, and does not say which check failed.
4. The answer is 200 with `content-type: application/octet-stream`. Its body
   is message 2 and then, for a `read`, the envelopes that did not fit, as
   transport messages. Each message, message 2 included, comes after its
   length as u16 big endian.
5. The service keeps no session and no Noise state after the answer.

**`read`**

| Field | Type | Meaning |
|---|---|---|
| `have` | map | `{boot, seq}`: the `boot` the watch's envelopes come from, and `seq`, class to the highest `seq` it holds from that boot. Empty means nothing |
| `hz` | uint | 1 keeps the box's `watch` in force for 60 s, as a looking phone does. 0 leaves it as it is |

Message 2 answers a `read` with the app session's `snapshot`, without the box
envelopes `have` covers: one from the same `boot` with a `seq` no higher than
the one the watch holds for its class. `more` counts the envelopes that follow
message 2. A `token.get` gets `token {token, expiresInS}` in message 2, and
`push.register` and `push.unregister` get an empty payload, once the service
has stored the change.

**Limits.** One read at a time per device and 30 a minute; message 1 at most
4 KiB; the whole answer within 10 s. Past a limit the service answers 429, a
longer message 1 gets 400, and a service without a Noise key answers 501.

With the readable copy off, a watch on its own shows what it holds and the
age of it, and prices and the plan wait for the phone.

## Exchange

A watch's commands go to its box through the box's exchange
([ADR 0009](adr/0009-watch-exchange.md), decisions 1 and 2): short Noise
sessions, `ftw.exchange.v1`, IK to the box's static key, which live on the
box and answer one request at a time. The relay that first carried them is
gone (17 October 2026), so each request rides one
[one-shot request](#one-shot-read) to the service, and the service hands it
to the box on the uplink. It reads neither the request nor the answer.

1. The watch posts a one-shot request whose message 1 carries `x {d}`. `d`
   is one exchange message: handshake message 1 with the watch's `hello`
   (608 bytes), or a request of one lane 0 frame, `cmd` or `now` (536).
2. The service admits the watch as step 3 of the one-shot read says, with
   `tun` in place of the effective scope, and only a roster entry with role
   `watch`.
3. It sends `x {dev, id, d}` down the uplink: `dev` the roster id its own
   handshake proved, `id` a number of its own for this request. It never
   takes either from the watch.
4. The box answers `x {dev, id, d}` with the same `dev` and `id`. `d` is the
   exchange's answer: message 2 with `hello_ok` (1,072 bytes), or two lane
   0 frames (1,072), `cmd.ack` and `cmd.result`, or `now` and a `tick`. An
   empty `d` is the box's silence: a stranger, a key that is not the device
   `dev` names, a spent session, or a message it could not read.
5. The service answers 200 with message 2 carrying `x {d}`, the box's `d`
   unchanged, and keeps nothing.

**`x`**

| Field | Type | Meaning |
|---|---|---|
| `dev` | text | The roster device id of the watch. On the uplink only; the service sets it |
| `id` | uint | The service's number for one request, below 2³², which the box copies into its answer. On the uplink only |
| `d` | bstr | One exchange message, or empty for no answer |

- **No answer.** The service answers `x` with an empty `d` at once when the
  box is not connected, did not say `x` in `hello.caps`, or has 8 requests
  open; and after 5 s without the box's answer. The watch cannot tell
  these apart, and does not need to: an exchange session that got no
  answer is spent, and a command that got none is sent again under its
  `cmdId`, which the box's command log answers with the stored outcome,
  unless the box restarted meanwhile (below).
- **A restart forgets.** The command log lives in the box's memory. A box
  that restarted between a command and its retry no longer knows the
  `cmdId`, and would act again. `hello_ok.clock.uptimeMs` says so: a new
  session's uptime below the one the command was sent in means a restart.
  The watch then sends the command no more, and says no answer came, which
  may mean it ran.
- **The box checks it again.** `dev` must name a watch of this box; one
  that does not costs the box nothing, not even a Diffie-Hellman. The box
  runs the handshake and admits the watch by its key, exactly as ADR 0009
  says, and answers only if the key the handshake proved is the device
  `dev` names. A request is tried only against that device's sessions.
- **Replays end nothing.** A new session stamps nothing and writes nothing
  until its first transport message decrypts. A watch holds at most 2
  sessions and the box 8; past either, the oldest session that has not
  proved itself goes first, never the newcomer. The box takes 12 handshakes
  a minute from one watch and 60 in all.
- **What the service can do.** Drop, delay or replay a request. It cannot
  read or forge `d`. A replayed message 1 opens a session nobody can use
  and ends no working one; a replayed request fails in its session's
  transport, and a replayed `cmd` in a new session meets the command log.
  Every `cmd` carries its own expiry, in box uptime.
- **What a watch may do.** The exchange takes `cmd` with `loadpoint.hold`
  alone, which Charge now, Pause and Back to the plan send, and `now`. Any
  other op is answered `E_OP_NOT_GRANTED` before it runs, whatever the
  watch role's scopes reach (ADR 0009, decision B).
- **The box's share.** `x` answers count against the tunnel's 24-hour
  budget and against their own tenth of it, and stop once the tunnel has
  spent nine tenths, so phones keep the last of it. The box takes 120
  requests a minute. A request whose answer could not leave is dropped
  unanswered and runs nothing.
- **What the service learns.** That a watch of this home sent a request
  and when, and when the answer came. Every request and every answer after
  the handshake has one length, and the box answers a `cmd` no sooner than
  400 ms after it arrived, applied or refused, so the service cannot tell a
  read from a command, or a refusal from a success.
- **Limits.** The one-shot read's: one request at a time per device and 30
  a minute. A command takes two (the handshake and the `cmd`) and a third
  when its result had not settled. `d` is at most 2,048 bytes either way.
- **Enrolment.** An owner's phone adds a watch over its own box session,
  with a passkey step-up, by the watch's public key:
  `POST /api/app-link/pairing {"role": "watch", "key": "<base64url>"}`. The
  box lists it in the next roster, so its first request is admitted like
  any other. No code crosses the service, and a request for a watch
  without a key is refused.

## Extension token

- The home id, a dot, and 32 random bytes in base64url. The service stores
  the SHA-256 of the random bytes, never the token, and refuses a token whose
  home is not the one its row is bound to. The home id lets any instance tell
  which shard holds the token. To the extension the token is opaque.
- Bound to one roster entry and valid for 7 days. The app renews it at each
  foreground and background refresh.
- It dies when the roster drops the device, the device's epoch changes, an
  owner suspends the device, or the grant ends.
- `GET /v1/now` with `Authorization: Bearer <token>` returns CBOR
  `{envelopes, lastSeenMs}`: the newest `live`, the newest `live` with
  `sources`, `prices` and `hb`, as the box signed them, in rising `seq`.
- A token never reads `lp`, `plan`, `event`, `tiles` or `health`. A test
  proves it.
- An extension checks signatures with the box key, and ages data with the
  anchor, that the app stored in its App Group.

## Push

| Type | Payload | Phone behaviour |
|---|---|---|
| `alert` | `aps` with the generic title "FTW" and `mutable-content: 1`, plus `home` and, in its `event` member, the box's `event` or `notice` envelope in base64 | The extension checks the signature and drops an already seen `(boot, seq)`. If the event is anchored and at most 10 minutes old, or the notice at most a day, it renders text from the app's catalogue copy and picks the category, and so the buttons, from the verified `kind`. A notice reads as an event of its kind, with `client` as its one argument. Otherwise it shows a generic line with no buttons |
| `widgets` | Content changed, no data | Widgets fetch with their token |
| `liveactivity` | `home`, the `lp` envelope and the newest `hb` envelope, in base64 | The view shows numbers only if both signatures check, the `hb` covers that `lp`, and the charger source grades `live` by the freshness rules; otherwise "Open FTW". `staleDate` is set to when that grade would lapse |
| `box.unreachable` | Generic, from the service | Sent after 600 s without the box's socket. No box signature, no arguments, no buttons |

A watch registers with `app: watch`, and the service sends it to the watch
app's APNs topic. A watch and its phone get byte-identical alert payloads, so
Apple shows one alert when both are near.

Each push goes only to devices whose effective scope covers what it carries:
an event's `alert` needs `ftw.events.read`, and `widgets` and `liveactivity`
need `ftw.live.read`. Viewers therefore get no alerts. `box.unreachable` goes
where an event would. A notice's `alert` goes to every owner device
registered for `alert`, with the readable copy on or off, and to no viewer
and no watch token. An owner may therefore register for `alert` with the copy
off; until the copy carries events, it gets notices only. Notices have their
own hourly cap per home, apart from events, so neither crowds out the other. The service sends `widgets` when tomorrow's
prices arrive and when a car is plugged in. It derives no category, collapse
id or priority from an event's kind. How often a Live Activity can refresh
within Apple's budget is the iOS plan's M7 spike.

**What Apple sees.** Every alert payload carries the home id, the event kind,
its arguments and the box's `boot`, `seq` and send time. A notice's carries
the home id, the notice kind and the same header, and on `agent.added` the
client's name only while the copy carries events. A Live Activity
payload carries the whole `lp` envelope: charge level, power, ready time and
energy delivered. ADR 0008 lists this as a risk.

## Freshness

Carrier state `cloud` labels data read through the service. Source states
come from the box, and the rules below can only make them worse.

**Assumption.** The phone's own clock is close to true time. A phone clock
that runs behind lets a delaying service hide that much delay.

**Accepting a message.** The phone accepts a box envelope only if its
signature verifies with the box key for that home. It keeps the highest `seq`
accepted per class and per boot, and drops a message of that class whose
`seq` is not higher. It processes the envelopes of a snapshot in rising
`seq`. A message from another boot replaces the current boot only if it is
anchored and its send time is later than the newest message the phone holds.
When the boot changes, the phone drops the source table, field values and
loadpoints it held, and shows none until that boot's full `live` arrives. The
phone checks the signature, decodes the body and checks `seq` before it
changes anything, so a message it drops leaves what it holds as it was.

**Anchors.** The phone trusts a send time only by one of two rules.

- **Rule N.** The message has `wallMs`. Send time = the earlier of `wallMs`
  and the phone's time of arrival, so a fast box clock cannot make data look
  fresher. If the phone accepts a message of a boot whose `wallMs` is more
  than 2 s ahead of its arrival, it stops using rule N for that boot.
- **Rule U.** During a Noise session with the box, the phone records phone
  time minus box uptime for each message from the box that carries
  `uptimeMs`, `hello_ok.clock` among them, under the boot that
  `hello_ok.cloud.boot` names. It keeps the smallest offset per boot, and
  keeps it across app starts. Send time = offset + `uptimeMs`. Box clocks
  drift, so when rule U gives the send time, the phone adds 0.01 % of the time
  since the offset was last learned to every age. It drops the anchor
  24 hours after it was last learned.
- When both apply, the phone uses the earlier send time. When neither does,
  the data is unanchored: it shows as "cloud, age unverified", at best
  `stale`, and never `live`.

**State.** The phone holds, for the current boot, the source table with its
`srcRev`, the newest `live` and `lp` with their `seq`, and the newest anchored
`live` or `hb`, called M below. Let t = phone time − M's send time, at least
zero.

1. If M names a `srcRev` newer than the table the phone holds, every source is
   `stale` until that table arrives.
2. If M is an `hb` whose `liveSeq` is above the `seq` of the `live` the phone
   holds, every field is `stale` until that `live` arrives. The same holds
   for `lpSeq` and the loadpoint and feedback values in `lp`.
3. Otherwise, for each source with box state B, `staleAfterMs` S and age A in
   M's `ages`, the reading's age now is A + t. Graded as the box grades it,
   that is `live` up to S, `lagging` up to 2S, `stale` up to 6S and `down`
   beyond. A source missing from `ages` keeps B.
4. The effective state is the worse of B and that grade, in the order `live`,
   `lagging`, `stale`, `down`, `never`. A state the phone does not know ranks
   as `never`.
5. A field follows the worst of its sources in `fieldSources`. A field with no
   source, such as the mode, is `live` while t ≤ 2H and `stale` after. A
   loadpoint follows its charger source in `src`. A source the table lacks
   counts as `stale`.

The service cannot sign, so it cannot make a source look fresher. It can only
delay, drop or replay, and each of those ages the data or marks it stale.

**Cases.** `cloudwire.View` runs each case below as a test, except case 15,
which belongs to the notification extension. The phone's port replays them.
In all of them the phone uses rule N with 100 ms in transit unless stated.

1. **A quiet live source stays live.** One source, S = 10 s, read every
   4.5 s, so H = 5 s. The box sends a full `live` with `srcRev` 1, then only
   `hb` every 5 s for 120 s, each with an age of at most 4.5 s. The age now
   never passes 9.6 s, and the source shows `live` throughout.
2. **A dead uplink ages on time.** As case 1, but the socket dies after an
   `hb` whose age for the source is 2 s. The source shows `live` until t = 8 s,
   `lagging` until 18 s, `stale` until 58 s and `down` after.
3. **A replay ages out.** The service replays case 1's first `live` and
   `hb` an hour later to a phone that has seen nothing from that boot. Their
   send time is an hour old, so the source shows `down`. A phone that holds a
   higher `seq` for those classes drops them.
4. **A withheld table goes stale.** The box moves the source to `lagging` with
   `srcRev` 2, and the service drops that `live`. The next `hb` names
   `srcRev` 2. The phone holds revision 1, so the source shows `stale` until
   revision 2 arrives.
5. **No anchor, never live.** The box reports `clock: "none"`, and the phone
   holds no rule U offset for that boot, or none at all. The data shows as
   "cloud, age unverified" and never `live`.
6. **A fast box clock loses rule N.** A message arrives with `wallMs` 3 s
   ahead of the phone clock. Rule N stops for that boot, and without rule U
   the data is unanchored, as in case 5.
7. **A small clock lead gains nothing.** A message arrives with `wallMs` 1.5 s
   ahead of the phone clock. Its send time is the arrival time, not `wallMs`.
8. **A changed byte is dropped.** The service flips one byte of `b`. The
   signature fails and the phone drops the message.
9. **A dropped `live` goes stale.** The box sends `live` with `seq` 40, the
   service drops it, and the next `hb` names `liveSeq` 40. The phone holds
   `seq` 31, so every field shows `stale` until `seq` 40 arrives.
10. **A held-back `lp` goes stale.** As case 9, for `lp` and `lpSeq`. The car
    view shows `stale` and no charge level as current.
11. **The byte budget runs out.** The box stops sending `live` and `hb`. The
    data ages as in case 2.
12. **A reboot clears the old table.** The phone holds boot X's table with
    `srcRev` 9. Boot Y's `hb` arrives, anchored and later, with `srcRev` 3,
    and boot Y's full `live` is dropped. The phone drops boot X's table and
    values, and shows no sources and no fields until boot Y's full `live`
    arrives.
13. **Rule U drift counts against freshness.** The offset was learned 10 hours
    ago. Every age gains 3.6 s, and at 24 hours the anchor lapses.
14. **A snapshot with mixed `seq`.** Message 2 holds `prices` with `seq` 5 and
    `live` with `seq` 3000. The phone, which held `live` 2990 and no `prices`,
    accepts both.
15. **An old alert shows the generic line.** The service replays a signed
    event from 2 hours ago to a phone that never saw it. The extension shows
    the generic line with no buttons.
16. **The box's state wins.** The box reports a source as `down`, and t = 1 s.
    The source shows `down`.
17. **A late table still counts.** The box changes the table: a `live` with
    `seq` 2 and `srcRev` 2 goes on the stream, and a `live` without a table,
    `seq` 3 and `srcRev` 2, overtakes it as a datagram. The source shows
    `stale` (rule 1) until the late `live` arrives; the phone then takes its
    table, keeps `seq` 3's values, and the source shows `live`.

## Grant on the box

The grant lives in the box's state store under `cloud/grant_v1`, not in the
config document, so a whole-document config write cannot reset it. It holds
the grant id, scopes, purposes, a fingerprint of `cloud.key`, `grantKey`, the
granting device or door, the version of the consent text the owner accepted,
the time or 0 when the clock was not set, and the data controller. With
statistics on, it also holds the `statsId` and the version of the statistics
consent text.

- A missing record, or a fingerprint that does not match `cloud.key`, reads
  as **off**, and `GET /api/cloud` says why. This is the opposite of
  `app_link` and `fleet_ping`, where a missing section reads as on.
- The box never backs up `cloud.key`, and a restore never brings it back:
  restore moves the whole data directory aside. A restored box, in place or
  on new hardware, has FTW Cloud off until an owner turns it on again, under
  a new home id and a new `grantKey`. The old home at the service ages out as
  under [Silence](#revocation-and-deletion).
- A grant works with the relay off.
- A grant uses the link's `cloud.key` and home id, and makes the link when
  there is none. Withdrawing it keeps both.

| Route | Who may call it |
|---|---|
| `GET /api/cloud` | An owner's session, or a client on the LAN. State, scopes, purposes, bytes sent in the last 24 hours of uptime, whether the byte budget is reached |
| `GET /api/cloud/stats-policy` | The same. Exact fields, purpose and group rules for a fresh consent |
| `GET /api/cloud/preview` | The same. The exact bytes that leave the house |
| `POST /api/cloud/grant {scopes, policy}` | An owner's session, the box itself, or a direct private LAN client. The optional house password, when on, requires a LAN login as Settings does. On a grant already on, it changes the scopes and keeps the key, home id and `grantKey` |
| `POST /api/cloud/stats {policy}` | The same gate, as a separate consent. 409 while the grant is off or a revoke waits for its receipt |
| `DELETE /api/cloud/grant`, `DELETE /api/cloud/stats` | An owner's session, or any client on the LAN, because narrowing access is safe |

A viewer's session gets 403 on every route, and so does a request with
forwarding headers. No route asks for a step-up: the box cannot verify the
app's, so the grant screen claims no more than an owner's session.

The house password is not a prerequisite for consent. With it off, a client
that can reach the box on a private network may turn Cloud or statistics on,
as it may save settings and pair an owner. This includes private VPN peers.
Public clients, public hostnames, forwarding headers and browser cross-site
writes do not gain access. CGNAT is not a private network for this choice.

- Turning FTW Cloud on pushes `cloud.granted` to all owners, and turning it
  off pushes `cloud.revoked`. A change of scopes, and turning statistics on
  or off, push nothing. Every change writes an audit line on the box. A grant,
  a change of scopes and a statistics change each send a new roster.
- `POST /api/cloud/stats` draws a `statsId` the first time and keeps it while
  statistics stay on under the same policy. A new policy draws a new id.
  Turning statistics on again after a withdrawal draws a new one.
- `DELETE /api/cloud/stats` removes `stats` from the purposes at once. The
  box forgets its stats id and sends a roster without statistics metadata.
  Cloud deletes the rows when it receives that signed roster.

## Revocation and deletion

- **Device revoke** on the box: a roster without the device. See Roster.
- **Grant revoke:** the box stops sending readings and sends a roster whose
  grant keeps its id and has no scopes or purposes. The service deletes what
  it holds under that grant (the live copy, events, tiles, extension tokens
  and statistics rows) and answers `purged {what: "copy", grantId}`, which it
  sends again for every such roster. The box shows the revoke as pending
  until a valid receipt arrives, then deletes the grant and shows the receipt
  time. The link, the home id and the tunnel stay.
- **Link end:** the box sends `bye {why: "revoked", purge: true}`. The
  service deletes the home: registry row, tokens, APNs registrations, live
  copy, events, tiles and statistics rows. It keeps a tombstone of the home
  id until it has delivered `purged {what: "home"}`, which it sends once all
  of that is gone, statistics rows included. Until then the box keeps
  `cloud.key` and sends only `hello` and `bye` on each connect. With a valid
  `purged` it deletes the key. A box of an earlier release still ends its
  grant this way.
- **Statistics withdrawal:** a signed roster without the statistics consent
  deletes the home's unpublished rows before accepting another report. If
  the box is offline, this happens on reconnect.
- **Silence:** after an hour without the box's socket, the service serves only
  `lastSeenMs`, no readings, and the tiles, each until 48 hours after its
  end or until the roster runs out. A tile is dated history and never reads
  as now. When the roster runs out, it closes sessions
  and refuses reads. After 30 days it drops tokens and APNs registrations.
  After 90 days without a valid roster it deletes the home as in a purge.
- **A new home** that sends no roster within 24 hours is deleted.

## Shards and restarts

Every home lives on exactly one instance of the service, its shard. Its box,
phones, watch and extensions all meet there, so nothing about a home crosses
between instances: no shared pub/sub and no shared storage, and readings stay
off disk. `cloud.ftw.energy` is the default host. With one shard it is the
only host and nothing below happens.

**Wrong shard.** An instance that gets a connection or a request for a home
it does not hold names the host that does:

| Where | When the service knows the home | Answer |
|---|---|---|
| `/v1/box` | After `hello`, from the key that signed it, before the registry is touched | Close 4421, reason the host |
| `/v1/app` | From `?home=`, before message 1 is read | Close 4421, reason the host |
| `/v1/app/once` | From `?home=`, before the body is read | 421, the host in `Ftw-Shard`, empty body |
| `/v1/now` | From the home id in the token | 421, the host in `Ftw-Shard`, empty body |

A client follows a wrong-shard answer only to `cloud.ftw.energy` itself or
one DNS label of lowercase letters, digits and hyphens directly under it,
such as `s2.cloud.ftw.energy` (`cloudwire.ShardHostOK`). No port, userinfo,
uppercase or trailing dot; `cloud-shard-vectors.json` holds the cases every
client must decide alike. A phone that follows `Ftw-Shard` on `/v1/now`
carries its bearer token there. The redirect carries
no trust: the box still checks the TLS certificate for that host and
`hello_ok` against the service keys it was built with, and the phone still
runs Noise IK to the service Noise key it carries.

- The client keeps the host for that home. The box stores it with its link,
  so a restart dials the shard first.
- It follows once. A second wrong-shard answer before it is connected again
  sends it back to `cloud.ftw.energy` after backoff, where it may follow once
  more.
- After 3 dials in a row that fail at a kept host over at least 30 s, the
  client forgets it and starts again at `cloud.ftw.energy`. The window
  outlasts a shard's restart, so a restart does not send its clients to
  the default host.
- A box lists `shard` in `hello.caps` when it follows 4421 and spreads its
  redial after 4503. A second shard holds homes only once their boxes list
  it.

**Directory.** Each instance holds the same directory: the shards' hosts, a
shard for each of 256 buckets of homes, and single homes moved on their own.
A home's bucket is the first byte of SHA-256 of `ftw.cloud.shard\0` and the
home id. Moving homes copies their registry rows to the new shard, then
updates the directory on every instance; the tool for it comes with the
second shard. Their clients follow the redirect at their next connection,
and extension tokens keep working, because the rows hold their hashes and
each token names its home.

Assistants' MCP and OAuth clients belong to other parties and follow neither
4421 nor 421. `/mcp` and `/oauth/*` therefore stay on `cloud.ftw.energy`.
Once there is a second shard, that instance forwards each call to the shard
that holds the home; the client never sees another host.

**Restart.** An instance that is about to stop, for a deploy or a move, stops
accepting connections and closes every socket with 4503. The reason is a
spread in milliseconds, in decimal, which the service sets from how many
sockets it holds and how fast the next instance can take them. The client
redials at a uniformly random moment within the spread, from 1 s to 60 s,
and does not count the close as a failed attempt. A missing or unreadable
reason means 5 s. A 4503 that comes before `hello_ok`, or before message 2
on `/v1/app`, is a failed attempt: the backoff grows, so a service that
sends every client away at once cannot hold them in a loop. The service
sends no `box.unreachable` for boxes it closed this way.

A command in flight when the service stops may be lost with the socket,
before or after the box acted on it. A phone treats a command that got no
answer as unknown, not failed: it shows the box's state once the session is
back, and sends the command again only if the state says it was not
applied.

## Service state

Per home, the service keeps in memory the newest envelope of each class, the
newest `live` that carried `sources`, 48 hours of tiles and a one-hour ring of
events. For tiles that is at most five closed tiles and 16 pieces of the
trailing tile, each as the box signed it, and at most 64 KiB per home; a
tile that fails `cloudwire.Tiles.Check` or is over 4 KiB is dropped and
counted. All homes together hold at most a fixed number of tile bytes, past
which new tiles are dropped and counted. `have` in `hello_ok` names the
closed tiles held. It writes no live readings to disk; the statistics files hold the
only readings it stores. Its registry holds the home id, the box key, the
roster, suspensions, the current and deleted `statsId`s, APNs registrations
and token hashes.

A restarted service holds the registry but no readings. At start it reads
each home's signed roster back from the registry, checks its signature with
the home's box key, and admits phones by it, counting `ttlS` from when the
roster first arrived. It also starts the wait for `box.unreachable` again for
each home, from when the box was last seen, so a restart neither forgets nor
repeats one. Without a box, a phone's `tun` gets an empty `d` back.

A rebuilt host starts with an empty registry. Boxes send their rosters again
and phones register again at their next start. Suspensions are lost, so an
owner must suspend again.

## Registry names

These names live in the contract registry. srcfl/ftw-webapp added them there
first ([#81](https://github.com/srcfl/ftw-webapp/pull/81),
[#88](https://github.com/srcfl/ftw-webapp/pull/88) and, for agents,
[#90](https://github.com/srcfl/ftw-webapp/pull/90)), and paired pull requests
copied them here and to the app.

| Name | Kind | Goes into |
|---|---|---|
| `ftw.events.read` | Scope: events behind notifications | `registry.yaml`, `scopes` |
| `cloud_scopes` | List: `ftw.live.read`, `ftw.history.read`, `ftw.plan.read`, `ftw.health.read`, `ftw.events.read` | `registry.yaml`, new block |
| `cloud_purposes` | List: `serve`, `stats` | `registry.yaml`, new block |
| `cloud.link` | Capability: the box can run the FTW Cloud uplink. Absent means the app uses the relay only and offers no FTW Cloud | `registry.yaml`, `capabilities` |
| `cloud` | Carrier state, between `relay` and `cache` | `registry.yaml`, `carrier_states` |
| `cloud.tun` | Capability: the box takes its app session through the service as `tun`. Absent means the phone uses the relay, even when it knows the home id | `registry.yaml`, `capabilities` |
| `tunnel` | Carrier state, between `relay` and `cloud`: the box's own session, carried by the service | `registry.yaml`, `carrier_states` |
| `cloud.granted`, `cloud.revoked` | Push kinds, sent to every owner when FTW Cloud is turned on or off | `push-catalogue.yaml` |
| `access.agents` | Capability: the box takes agents and answers the `agent.*` messages. See [Agents](agents.md) | `registry.yaml`, `capabilities` |
| `agent`, `support`, `release` | Roles for agent rows | `registry.yaml`, `roles` |
| `ftw.logs.read`, `ftw.config.read`, `ftw.diag.run`, `ftw.dispatch.hold`, `ftw.goals.request`, `ftw.update.request` | Agent scopes | `registry.yaml`, `scopes` |
| `agent` on each op | The scope that opens an op to an agent | `registry.yaml`, `ops` |
| `agent_args` | What an agent may put in each op, with a label for each argument | `registry.yaml`, new block |
| `reads` | The reads an agent may send and the scope each needs | `registry.yaml`, new block |
| `E_GRANT_EXPIRED`, `E_OP_NOT_GRANTED`, `E_CAP_EXCEEDED`, `E_RATE_LIMITED`, `E_NEEDS_OWNER`, `E_BAD_SIGNATURE`, `E_NO_APPROVAL_KEY`, `E_NOT_FOUND`, `E_CLOCK_SKEW` | Agents' refusals | `registry.yaml`, `errors` |
| `agent.added`, `agent.request`, `agent.expiring`, `approval.key.set`, `owner.added` | Push kinds, sent to every owner | `push-catalogue.yaml` |
| `car.which` | Push kind: the box asks which car is on a charger ([docs/cars.md](cars.md)) | `push-catalogue.yaml` |
| `car_soc_sources`, `car_identified_by` | Lists: where a car's level comes from, and how the box knows which car is on a charger | `registry.yaml`, new blocks |

Class names, field names, close codes and the signing tags are not registry
names. They live here and in `go/protocol`, held by golden vectors in both
repositories.

Role names are registry names. `go/protocol` generates them as `Role*`
constants, so `srcfl/ftw-cloud` need not spell them by hand.

## Numbers in one place

| What | Value |
|---|---|
| Reconnect backoff, box and phone | 0.5 s to 60 s, full jitter |
| Redial after 4503 | Uniform within the reason's spread, 1 s to 60 s; 5 s without one; backoff when it comes before `hello_ok` or message 2 |
| Wrong-shard redirects followed | One per connection; a kept host is forgotten after 3 failed dials over at least 30 s |
| Shard buckets | 256 |
| Phone retry after 4403 | Half to all of min(15 s × 2ⁿ, 5 min) |
| Ping, read deadline, write deadline | 15 s, 45 s, 10 s, on both sockets |
| Period per class | 5 s; 1 s while watched; `lp` at least 2 s |
| Source table change sent within | 1 s |
| `lp` deadband | Power more than max(100 W, 5 %); `sessionWh` 100 Wh; `socPermille` 10 |
| `prices` looked at | Every minute |
| Box's `watch` lasts | 60 s |
| Service renews the box's `watch` | Every 50 s while a phone looks |
| Phone's `watch` lasts | Until the next `watch` or the end of the session |
| Heartbeat H | clamp(smallest `staleAfterMs` ÷ 2, 2 s, 15 s) |
| Full state | Every 15 minutes |
| `resync` honoured | At most one per 5 minutes |
| Event ring on the box | 32 |
| Byte budget | 20 MB per 24 hours of uptime |
| Largest frame from the service to the box | 4 KiB; a `tun` frame 16,664 bytes |
| Largest message from a phone | 4 KiB; a `tun` message 16,664 bytes and Noise's 24 |
| `tun` per home and direction | 240 messages, then 60 a second; 4 MiB, then 512 KiB a second |
| `tun` queues and budget on the box | 64 lane 0, 256 bulk paced to 90 % of the limits; 250 MB per 24 hours of uptime |
| Lane 0 through the tunnel | On each finished poll, after 5 ms; at most 5 frames a second, 3 at once; a beat after 1 s quiet, 5 s while hidden; pushes stop at 60 % of the tunnel budget, beats at 90 % |
| `live` while watched | On each poll that changed it, at most once per period |
| Box handshakes through the tunnel | 12 a minute per device, 240 in all |
| Pending pairing entry | One QR code, for its own ten minutes; admits one handshake |
| Pending agent invitations | At most 16; claim window at most 24 hours; Core consumes each code after Noise proof |
| Box session up after TLS | One round trip: its message 1 rides in `now.tun` |
| Sessions | 16 per home; one per device |
| Envelopes queued per phone | 256 |
| Phone messages | 20 in a burst, then 2 a second |
| App session message 1 | Within 10 s |
| One-shot reads | One at a time per device, 30 a minute; message 1 at most 4 KiB; answered within 10 s |
| Exchange through the service | Answered within 5 s or with an empty `d`; 8 open per box; `d` at most 2,048 bytes |
| Exchange at the box | 8 sessions, 2 per watch; 12 handshakes a minute per watch, 60 in all; 120 requests a minute; `x` a tenth of the tunnel budget, stopped at nine tenths |
| Roster `ttlS`, renewal | 24 hours, every 6 hours |
| Extension token | 7 days |
| Alert age for buttons | 10 minutes |
| `box.unreachable` after | 600 s |
| Readings served after silence | 1 hour; tiles until 48 hours after their end, or until the roster runs out |
| Tiles | `5m`, 12-hour tiles, the last 48 hours: at most 5 closed and 16 pieces of the trailing one, 64 KiB, per home; 4 KiB an envelope |
| Bucket counted closed | 30 s after it ends |
| Trailing tile sent whole | At each connect, after 11 appends, and when its first bucket closes |
| Closed tiles packed again | Every hour, sent when the etag changed |
| Sessions closed | When the roster runs out |
| Tokens and APNs dropped after | 30 days |
| Home deleted after | 90 days without a roster; 24 hours for a new home without one |
| Statistics rows kept | 48 hours from the hour they describe |
| Rule N clock lead allowed | 2 s |
| Rule U drift margin, lifetime | 0.01 % of the time since learned; 24 hours |


### Cloud terms and local statistics, policy version 3

`ftw-cloud-2026-10-v3` includes `ftw-project-stats-2026-10-v3` for every Cloud
home. `Grant.termsPolicy` carries the owner-accepted terms. Once the operator
approves collection, Cloud admits only that bundled grant or a statistics-only
roster. Old terms are not silently upgraded; they need an owner answer.
Restoring a roster after restart applies the same rule.

A statistics-only roster names the statistics policy as its terms policy,
contains the stats purpose alone and has no read scopes, devices, pending
codes, Noise key or browser subscriptions. It grants no remote access. Core
also rejects tunnels locally in that mode and emits no live or event data.
Collection stays off until the signed C8 review; the default operator setting
is empty. Withdrawal and retention continue while it is paused.

Since 24 October 2026 a box without Cloud no longer starts a statistics-only
roster: its statistics are the two anonymous daily reports, through the
Oblivious HTTP relay (ADR 0008, "Anonymous daily reports and one statistics
answer"). Statistics-only rosters already running keep working until the
owner turns statistics off.
