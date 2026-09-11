# Sourceful Zap

FTW reads Sourceful Zap as the **P1/HAN site meter**. That is the default
role. If Zap also lists an inverter, battery or charger, add that device in
FTW with its own driver when you can. Proxying those devices through an
ESP32 on Wi-Fi is slower and often fights the native protocol.

Some sites cannot talk to the inverter from FTW: Modbus is closed (no
SetApp), or Zap already owns the RS-485 bus. Then turn on a read under
Settings → Devices. The driver never writes.

The driver is telemetry-only. It talks to Zap's local API and stays up
without Sourceful cloud.

## Configuration

```yaml
drivers:
  - name: sourceful-zap
    lua: drivers/zap.lua
    is_site_meter: true
    capabilities:
      allow_unverified_local: true
      http:
        allowed_hosts: ["zap.local"]
    config:
      host: zap.local
      # read_pv: true        # opt-in; default is P1/HAN only
      # read_battery: true   # opt-in; telemetry only
```

The opt-in lets FTW use its own unauthenticated mDNS answer. Use the Zap's LAN
IP in both places when multicast does not cross the network.

With several meters, `meter_serial` pins the site meter; otherwise the first
P1/HAN device is preferred.

`read_pv` and `read_battery` are off unless you set them. Settings → Devices
exposes the same switches. Leave them off when a native driver already owns
that DER, or Combined will count it twice.

## Data and identity

The driver refreshes Zap devices without restarting and emits the selected
meter's power, phases, voltage/current/frequency and energy totals.

If Zap also lists a PV inverter, battery or charger that this driver is not
reading, it logs that and records an `other_resources` metric. Add the
matching native driver, or turn on the matching read.

The FTW device identity is based on Zap's gateway serial from `/api/crypto`,
with a lower-confidence meter serial fallback for older firmware.

Zap's per-DER `enabled` flag controls its Nova publishing, not whether local
FTW may read the P1/HAN meter.

## Safety

The driver converts every value to FTW's site convention and does not invent
zero when a required reading is absent. Silence lets the watchdog and
stale-site-meter guard act.

`driver_default_mode` performs no write because the driver is read-only.
Commands other than init/deinit fail closed.

## Verification

```bash
cd go
go test ./internal/drivers -run 'Zap|zap'
```

## Troubleshooting

- not found: confirm Zap is on Wi-Fi and reachable at
  `http://zap.local/api/system` from the FTW host;
- `.local` name does not resolve: with `allow_unverified_local` enabled, FTW
  asks the host's `avahi-daemon` where its socket is mounted, then queries the
  LAN. It needs to be on the same L2 segment as the device. That is the case
  with the Linux Compose topology
  (`network_mode: host`); under `docker-compose.macos.yml` the container is
  bridged and multicast does not reach the LAN, so configure the device by IP
  there. The log line naming the failure is `mDNS resolution failed`, and a
  successful lookup logs `via=avahi` or `via=multicast`; see
  [docs/operations.md](operations.md);
- no meter: inspect Zap's `/api/devices` and pin `meter_serial` when needed;
- inverter, battery or charger listed on Zap: add that device in FTW with
  its own driver, or turn on `read_pv` / `read_battery` when Zap is the only
  reader. Chargers have no Zap ingest path.
