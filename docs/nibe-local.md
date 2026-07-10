# NIBE S-series — Local REST API driver

`drivers/nibe_local.lua` reads a NIBE S-series heat pump (S735, S1255, S1155,
S320, …) directly over your LAN through the pump's **Local REST API**. It is
the on-prem twin of the MyUplink cloud driver (`drivers/myuplink.lua`): same
`hp_*` telemetry, no cloud account, no OAuth, no internet round-trip.

It is **read-only**. The pump is left in `aidMode: off` and the driver issues
no writes — it cannot actuate anything, so it cannot cause harm.

## Why the local API (vs. the cloud or raw Modbus)

The Local REST API returns, in one bulk request, ~980 data points — each with
its own metadata: modbus register, unit, **exact divisor**, and a writable
flag. That means:

- **Exact scaling.** A temperature with `divisor: 10` becomes °C precisely; a
  power with `divisor: 100` becomes kW precisely. No °C×10 guessing like the
  cloud driver has to do.
- **The whole register map, for life.** Every point lands in the long-format
  TS DB via `host.emit_metric`, queryable forever (see `docs/tsdb.md`). To keep
  a Pi-sized database bounded, headline metrics are sampled every minute while
  the bulk map records changes plus an hourly full snapshot.
- **No cloud dependency.** Works on an isolated LAN with no WAN.

## One-time setup on the pump

1. In the NIBE **myUplink** app, enable the **Local REST API** for the pump and
   note the generated **username** and **password**.
2. The app shows the API's certificate **fingerprint** ("fingeravtryck"). You
   will pin this. You can also read it yourself:

   ```bash
   openssl s_client -connect <pump-ip>:8443 -servername <pump-ip> </dev/null 2>/dev/null \
     | openssl x509 -noout -fingerprint -sha256
   ```

   Use the hex value (colons and case don't matter — they're normalised).
3. Leave the pump in **read-only** mode for this driver.

## Configuration

```yaml
drivers:
  - name: nibe
    lua: drivers/nibe_local.lua
    config:
      host: 192.168.1.180
      port: 8443                    # default
      username: <local-api-username>
      password: <local-api-password>   # masked via config_secrets
      # device_id: "06613225140002"    # optional; auto-detected if omitted
    capabilities:
      http:
        allowed_hosts: ["192.168.1.180:8443"]
        tls_pin_sha256: "<64-hex-char certificate fingerprint>"
```

### Why pin the certificate

The pump presents a **self-signed** certificate, which the system trust store
cannot validate. The wrong fix is to disable verification — that would accept
*any* certificate, letting a LAN man-in-the-middle present its own cert and
capture the Basic-auth password (Basic auth sends `base64(user:pass)` on every
request — reversible, not encryption).

Instead, `tls_pin_sha256` pins **exactly one** leaf certificate. A swapped cert
is rejected at the TLS handshake even if it chains to a real CA. This is the
same fingerprint-pinning approach the project uses for its DTLS/WebRTC identity.
Drivers without a pin keep standard system-root verification — pinning is
strictly opt-in and changes nothing for other HTTP drivers.

Keep the pump on a trusted LAN / IoT VLAN and never expose port 8443 to the
internet.

## Telemetry

Canonical headline metrics (stable names the heating dashboard + thermal twin
read). Their variable ids are **auto-selected per pump** from a built-in
profile map keyed by the device's `firmwareId`/model reported by
`GET /api/v1/devices` — the generic S-series default below is verified on the
S735 and, because the S-series shares the core register ids, covers the whole
family. To support a model that genuinely renumbers a headline, add an entry to
`PROFILES` in `drivers/nibe_local.lua`. You can always override any id per
instance via `param_power_id`, `param_hw_temp_id`, `param_indoor_temp_id`,
`param_outdoor_temp_id`, etc. in `config` (override > profile > default):

| Metric | Unit | Source (S735 id) |
|---|---|---|
| `hp_power_w` | W | Compressor power input (1801) |
| `hp_used_power_w` | W | Instantaneous used power (22130) |
| `hp_hw_top_temp_c` | °C | Hot water top BT7 (11) |
| `hp_outdoor_temp_c` | °C | Outdoor BT1 (4) |
| `hp_indoor_temp_c` | °C | Room BT50 (158) — absent if no room sensor |
| `hp_energy_consumed_kwh` | kWh | Tot. consumption (28393) |
| `hp_energy_produced_kwh` | kWh | Tot. production (28392) |
| `hp_degree_minutes` | DM | Degree minutes (781) |

Every other point auto-emits as `hp_<sanitized title>` with its unit, so the
UI can group temperatures / power / frequency / state automatically.

**Not-connected sensors.** An unconnected sensor reports a per-size sentinel
(`-32768` for s16, etc.) and the API still flags it `isOk: true`, so the driver
filters by variable size — a missing BT50 room sensor simply doesn't emit,
rather than logging −3276.8 °C.

## Site sign convention

A heat pump is a **load**. Its electrical draw is positive watts into the site
at the grid boundary — but this driver emits **diagnostics only**
(`host.emit_metric`), never `host.emit("meter"|"pv"|"battery")`. So it performs
no sign conversion and never double-counts against the real grid meter; the
thermal/load models consume `hp_power_w` etc. as twins.

## Troubleshooting

- **`tls pin mismatch`** in the logs → the pump's cert changed (firmware update
  / factory reset). Re-read the fingerprint (command above) and update
  `tls_pin_sha256`.
- **`host … not in allowed_hosts`** → add `"<ip>:8443"` to
  `capabilities.http.allowed_hosts`.
- **No metrics / `points poll failed`** → confirm the Local REST API is still
  enabled in myUplink and the username/password are current; the driver
  self-heals device detection on a 30 s backoff.
- **Verify by hand**:

  ```bash
  curl -sk -u '<user>:<pass>' https://<pump-ip>:8443/api/v1/devices
  ```

## Testing

A live integration test exercises the driver against a real pump
(`go/internal/drivers/nibe_local_test.go`, `TestNibeLocalLive`), skipped unless
`NIBE_LIVE=1`:

```bash
NIBE_LIVE=1 NIBE_HOST=192.168.1.180 NIBE_USER=… NIBE_PASS=… \
  NIBE_PIN=<fingerprint> go test ./go/internal/drivers/ -run TestNibeLocalLive -v
```

`TestNibeLocalEmitsTelemetry` is the hermetic version (fake pump) that runs in
CI.
