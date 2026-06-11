# Nova Core federation (opt-in)

forty-two-watts can federate its telemetry into [Sourceful Nova
Core](https://github.com/srcful/srcful-novacore) — the central backend
the ZAP gateway fleet reports into. Federation is strictly opt-in: when
disabled (default), forty-two-watts ships no data off-site.

## What federation gives you

- Per-site dashboards on Nova (grid, PV, battery, EV at 5-s cadence).
- Long-retention time-series in Nova's VictoriaMetrics (alongside the
  local long-format SQLite TS DB — the two are independent).
- Fleet views in the Sourceful portal.
- Future (PR 3+): control commands from Nova back to forty-two-watts
  for coordinated flexibility / VPP dispatch.

What federation does **not** do today:

- Does not change local operation. The control loop, MPC, watchdog,
  clamps, HA bridge — all unchanged.
- Does not replace Home Assistant. Run both; they're independent sinks.
- Does not send operator credentials or config contents. Only DER
  telemetry + registered device identities.

## Architecture at a glance

```
forty-two-watts                             Nova Core
┌──────────────────────┐                  ┌────────────────────────┐
│ telemetry.Store      │   MQTT (TCP/TLS) │ NATS MQTT adapter      │
│ (DerReadings)        │ ───────────────► │ :1883                  │
│   │                  │   ES256 JWT      │   │                    │
│   ▼                  │   (password)     │   ▼                    │
│ internal/nova        │                  │ topic-router           │
│  - Publisher         │                  │   │                    │
│  - adapter (legacy)  │                  │   ▼                    │
│  - ES256 identity    │                  │ metrics-bridge         │
│                      │                  │   │                    │
│ state.nova_ders      │   HTTPS (claim   │   ▼                    │
│ (Nova der_id cache)  │    + provision)  │ VictoriaMetrics        │
│                      │ ───────────────► │                        │
└──────────────────────┘                  └────────────────────────┘
```

Per-DER telemetry goes out once every `publish_interval_s` (default 5 s)
on `gateways/{gateway_serial}/devices/{hardware_id}/ders/{der_name}/telemetry/json/v1`.

## Data model alignment

forty-two-watts uses a physics-first, snake_case, site-convention
schema (see [docs/site-convention.md](site-convention.md)). Nova's
current wire format has several divergences from that schema — opposite
battery sign, mixed-case field names (`SoC_nom_fract`, `L1_V`,
`heatsink_C`), and a different DER type vocabulary (`solar`, `ev_port`).

We build forty-two-watts against its clean schema and translate at the
publisher boundary. The translation layer (`internal/nova/adapter.go`)
is a single file designed to be deleted once Nova's unified schema PR
lands; until then, `nova.schema_mode: legacy` (default) keeps
forty-two-watts compatible with every deployed ZAP gateway.

| Concept         | forty-two-watts (clean) | Nova legacy wire       |
|-----------------|-------------------------|------------------------|
| Battery W sign  | `+W` = charging (load)  | `−W` = charging        |
| Power           | `w`                     | `W`                    |
| Frequency       | `freq_hz`               | `Hz`                   |
| Phase voltage   | `l1_v` `l2_v` `l3_v`    | `L1_V` `L2_V` `L3_V`   |
| Temperature     | `temp_c`                | `heatsink_C`           |
| SoC (0..1)      | `soc`                   | `SoC_nom_fract`        |
| PV vocabulary   | `pv`                    | `solar`                |
| EV vocabulary   | `ev`                    | `ev_port`              |
| V2X vocabulary  | `v2x_charger`           | `v2x_charger`          |
| DC bus          | `dc_v` `dc_a`           | `V` `A` (battery)      |
| EV plug         | `connected`             | `plug_connected`       |
| EV vehicle SoC  | `vehicle_soc`           | `vehicle_soc_fract`    |
| V2X limits      | `charge_power_{min,max}_w`, `discharge_power_{min,max}_w` | `upper_limit_W`, `lower_limit_W` |
| V2X energy reqs | `ev_{min,max}_energy_req_wh` | `ev_{min,max}_energy_req_Wh` |
| Lifetime energy | `total_{import,export,charge,discharge,generation}_wh` | `total_..._Wh` (same, different case) |

To switch to the unified schema once it ships on Nova:

```yaml
nova:
  schema_mode: unified
```

## Identity mapping

| Nova field        | forty-two-watts source                                |
|-------------------|--------------------------------------------------------|
| `gateway_serial`  | Chosen at claim time (auto: `f42w-<12 hex>` if empty).|
| `hardware_id`     | `state.Device.device_id` verbatim (`make:serial`,      |
|                   |  `mac:…`, or `ep:…`).                                  |
| `der_name`        | `{driver_name}-{der_kind}` (e.g. `ferroamp-battery`). |
| `der_id`          | Server-generated `der-{uuid7}`; cached locally in     |
|                   |  `state.nova_ders` for diagnostics only.              |
| Gateway identity  | ES256 keypair at `cfg.Nova.KeyPath`                    |
|                   |  (default `<state_dir>/nova.key`).                    |

Topic levels are sanitized: any character outside `[a-zA-Z0-9_-]` is
replaced with `_` before going on the wire. Nova's MQTT adapter
translates `/` to `.` at the NATS-subject boundary; dots inside a
`hardware_id` (e.g. `ep:192.168.1.10:502`) would otherwise create
accidental extra subject levels.

## One-time setup: `forty-two-watts nova-claim`

Prerequisites:

1. A Nova organization and site (create via the Sourceful portal or
   `POST /organizations` + `POST /sites`).
2. A human operator JWT from Nova, and that operator's identity ID
   (`idt-…`). The JWT authorises the claim; it is **not** persisted by
   forty-two-watts.
3. forty-two-watts has been running long enough for each driver to
   register a device and emit at least one telemetry sample — that's
   how the CLI infers which DERs to provision. If you're adding a
   federation on a fresh install, start forty-two-watts once, let
   drivers connect, then run `nova-claim`.

Then:

```bash
export NOVA_OPERATOR_JWT=eyJhbGciOi...            # human identity JWT

./forty-two-watts nova-claim \
  --url=https://core.sourceful.energy \
  --org=org-019b952b-... \
  --site=sit-019b952b-... \
  --claimer=idt-019b952b-...                      # the human identity
```

What happens:

1. An ES256 keypair is generated at `<state-dir>/nova.key` if missing.
2. A `claimer_id|nonce|timestamp|gateway_id` message is signed with
   the private key. `POST /gateways/claim` is called with the pubkey,
   signature, and operator JWT. Nova verifies possession.
3. For each locally-registered device, the CLI infers which DER kinds
   the driver has emitted (from `ts_samples`) and calls
   `POST /devices/provision` to create the Device + DERs in one
   transaction. Returned `der-{uuid7}` IDs are cached in
   `state.nova_ders`.
4. The resulting `nova:` block (URL, gateway_serial, org_id, site_id,
   key_path, schema_mode) is written atomically into `config.yaml`.

Restart forty-two-watts to pick up the new `nova:` block. The
publisher starts and data begins flowing.

### Adding a driver after the initial claim

Add the driver to `config.yaml`, restart, let it register and emit
telemetry, then:

```bash
./forty-two-watts nova-claim --reconcile \
  --url=https://core.sourceful.energy \
  --site=sit-019b952b-...
```

`--reconcile` skips the claim step and only re-provisions devices; the
operator JWT is still required because `/devices/provision` is gated
on human identity.

The publisher will `WARN` once per unknown DER if it encounters a
device with no `nova_ders` entry. See logs for
`nova: DER not provisioned — run ...`.

## Runtime behavior

Once configured:

- A single paho MQTT client connects to `mqtt_host:mqtt_port`. Username
  is the gateway serial; password is a fresh ES256 JWT (10-min TTL)
  signed by the gateway key. The JWT is re-minted on every reconnect
  via paho's credentials-provider hook, so expired tokens never reach
  the broker.
- Every `publish_interval_s` seconds, the publisher snapshots every
  registered device × DER kind from `telemetry.Store`, assembles a
  clean `DerTelemetry`, translates per `schema_mode`, and publishes.
  QoS 0, not retained — same discipline as the HA bridge.
- The telemetry store, control loop, and TS DB are untouched. Nova is
  a strict read-side consumer.

## Config reference

```yaml
nova:
  enabled: true
  url: https://core.sourceful.energy        # core-api base
  mqtt_host: broker.sourceful.energy        # NATS MQTT adapter host
  mqtt_port: 1883
  mqtt_tls: false
  gateway_serial: f42w-abc123               # stable across restarts
  org_id: org-019b952b-...
  site_id: sit-019b952b-...
  key_path: /var/lib/forty-two-watts/nova.key
  schema_mode: legacy                       # "legacy" | "unified"
  publish_interval_s: 5
  reconcile_interval_h: 24                  # reserved for future use
```

All fields are validated at `config.Load`; an invalid `nova:` block
aborts startup rather than silently shipping a half-configured
publisher.

## Troubleshooting

**`nova: DER not provisioned`** — a device has telemetry locally but no
matching `nova_ders` row. Run `nova-claim --reconcile`.

**Connection lost / reconnect loop** — check that the clock on the
forty-two-watts host is within a few minutes of Nova's; ES256 JWTs
have short TTLs and auth-callout rejects stale tokens. Also verify
the gateway was claimed (`POST /gateways/<serial>` should return
your claimed record).

**Topics published but nothing shows in Nova** — the topic-router
silently drops telemetry for unknown `(hardware_id, der_name)` tuples
and caches the miss for an hour. Either re-run provisioning, or wait
out the cache.

**Sign looks wrong on battery charts** — confirm `schema_mode`. With
`legacy`, Nova's charts expect `−W` on charging; forty-two-watts flips
at the boundary. If you flipped to `unified` but Nova is still on the
legacy schema, battery signs will appear inverted.

## Design decisions

- **MQTT, not NATS WebSocket.** Nova's gateway ingress is MQTT via the
  built-in NATS adapter. ZAP gateways speak MQTT; so do we. NATS WS is
  for browser clients.
- **JWT-as-password over mTLS.** Matches ZAP fleet ergonomics and lets
  us refresh credentials without reconnecting from scratch.
- **Clean internal model, adapter at the boundary.** Renaming internal
  fields to match Nova's current hybrid-case convention would touch
  every driver, the HA bridge, the HTTP API, and every doc — for a
  schema we intend to deprecate. The adapter isolates the cost.
- **DER inference from telemetry history, not config.** Driver YAML
  does not currently declare DER kinds explicitly. Reading from
  `ts_samples` lets `nova-claim` work with zero additional config and
  naturally ignores drivers that haven't successfully connected yet.

## Future work

- **PR against novacore** — unified schema (snake_case, site-convention
  battery sign, DER vocabulary alignment). Once landed and ZAP firmware
  migrates, flip `schema_mode: unified` and delete `adapter.go`.
- **Control-topic subscription** — subscribe to
  `orgs/{org}/sites/{site}/devices/{device}/ders/{der}/control` so Nova
  can dispatch setpoints to forty-two-watts. Currently out of scope.
- **Nightly reconciliation** — auto-call `GET /sites/{site}/devices`
  and warn on drift. Today, drift detection is manual
  (`nova-claim --reconcile`).
