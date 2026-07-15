# Device identity

Hardware-stable identity for every driver. Lets persistent state (battery
models, calibration history, RLS twin parameters) outlive cosmetic config
changes — rename a driver, remove + re-add it, swap the host running
FTW: the same physical inverter keeps its trained state.

## Why driver-name is not enough

Until v2.1.x, persistent state was keyed on the driver name from
`config.yaml` (`name: ferroamp`). Two operator actions broke this:

1. **Rename.** Changing `name: ferroamp` → `name: fa1` orphans the trained
   battery model — the new name has no history, the old row sits in
   `battery_models` referencing a driver that no longer exists.
2. **Remove + re-add.** Deleting and re-adding the same hardware in
   `config.yaml` wipes trained state even though the underlying inverter
   is unchanged.

Both scenarios should preserve trained state because the underlying
hardware is the same. The fix is to key persistent state on a
hardware-derived identifier (`device_id`) instead of the driver name.

## Identity hierarchy

Every driver is assigned a `device_id` at registry-Add time using the
highest-priority signal available. Resolution lives in
[`go/internal/state/devices.go:31`](../go/internal/state/devices.go)
(`ResolveDeviceID`):

1. **`make + ":" + serial`** — canonical, hardware-issued. Set from inside
   the Lua driver via `host.set_make("Sungrow")` and
   `host.set_sn("332407312")`. Examples: `sungrow:332407312`,
   `ferroamp:EH-12345-67890`. Never collides; survives every kind of
   reconfiguration.
2. **`"mac:" + arp-resolved MAC`** — L2-stable fallback for any TCP device
   on the same subnet. Used when the protocol doesn't expose a serial
   (e.g. Ferroamp's MQTT payload has no SN field). Example:
   `mac:64cfd94f7c54`.
3. **`"ep:" + endpoint`** — last-resort fallback when neither SN nor MAC
   is available (e.g. cloud MQTT brokers across VLANs). Example:
   `ep:mqtt://broker.example.com:1883`.

Normalization: `make` is lowercased + trimmed; `mac` is lowercased with
colons stripped; `serial` is trimmed but case-preserved. See
[`devices.go:32-34`](../go/internal/state/devices.go).

```go
// go/internal/state/devices.go:31
func ResolveDeviceID(make, serial, mac, endpoint string) string {
    make = strings.ToLower(strings.TrimSpace(make))
    serial = strings.TrimSpace(serial)
    mac = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(mac)), ":", "")
    if make != "" && serial != "" {
        return make + ":" + serial
    }
    if mac != "" {
        return "mac:" + mac
    }
    if endpoint != "" {
        return "ep:" + endpoint
    }
    return ""
}
```

## ARP behavior and the L2 caveat

The `arp` package ([`go/internal/arp/arp.go`](../go/internal/arp/arp.go))
exports one function:

```go
func Lookup(ipStr string) (mac string, ok bool)
```

Behavior:

- Nudges the kernel ARP cache by opening a 50 ms TCP probe to common
  ports (`80`, `502`, `1883`) — the SYN itself triggers ARP resolution,
  we don't care about the connection succeeding
  ([`arp.go:35-38`](../go/internal/arp/arp.go)).
- Linux: parses `/proc/net/arp` directly
  ([`arp.go:52`](../go/internal/arp/arp.go)).
- Darwin: shells out to `/usr/sbin/arp -n`
  ([`arp.go:69`](../go/internal/arp/arp.go)).
- Returns `("", false)` on cross-VLAN devices — the kernel's ARP table
  only contains entries for hosts on the same L2 segment. Cloud brokers,
  devices behind a different router, anything requiring an L3 hop: all
  invisible to ARP.

When ARP returns false the driver's `device_id` falls through to the
`ep:` tier.

## `devices` table schema

Created by `Store.migrate` in
[`go/internal/state/store.go:164`](../go/internal/state/store.go):

```sql
CREATE TABLE devices (
    device_id     TEXT PRIMARY KEY NOT NULL,
    driver_name   TEXT NOT NULL,
    make          TEXT,
    serial        TEXT,
    mac           TEXT,
    endpoint      TEXT,
    first_seen_ms INTEGER NOT NULL,
    last_seen_ms  INTEGER NOT NULL
);
CREATE INDEX idx_devices_name ON devices(driver_name);
```

## `RegisterDevice` and `LookupDeviceByDriverName`

`Store.RegisterDevice` upserts. The conflict-resolution clause uses
`COALESCE(NULLIF(...))` so already-known fields aren't overwritten with
empty strings — a late-arriving serial reported by
`driver_poll` won't erase a MAC we already have, and vice versa. See
[`devices.go:61-71`](../go/internal/state/devices.go):

```sql
ON CONFLICT (device_id) DO UPDATE SET
    driver_name  = excluded.driver_name,
    make         = COALESCE(NULLIF(excluded.make, ''), devices.make),
    serial       = COALESCE(NULLIF(excluded.serial, ''), devices.serial),
    mac          = COALESCE(NULLIF(excluded.mac, ''), devices.mac),
    endpoint     = COALESCE(NULLIF(excluded.endpoint, ''), devices.endpoint),
    last_seen_ms = excluded.last_seen_ms
```

`Store.LookupDeviceByDriverName(name)` returns the most recently-seen
device for a driver name (ordered by `last_seen_ms DESC LIMIT 1`). Used
by `batteryModelKey` to translate driver-name → device_id when saving
battery models. See
[`devices.go:79-91`](../go/internal/state/devices.go).

## Identity bootstrap flow

Wired in [`go/cmd/ftw/main.go:151-165`](../go/cmd/ftw/main.go)
and [`go/internal/drivers/registry.go:99-135`](../go/internal/drivers/registry.go):

1. **At registry-Add.** Endpoint is set immediately from the effective
   MQTT / Modbus config via `env.SetEndpoint("mqtt://host:port")` or
   `"modbus://host:port"`
   ([`registry.go:118`, `registry.go:131`](../go/internal/drivers/registry.go)).
2. **ARP lookup.** `r.ARPLookup(mq.Host)` is attempted; on success
   `env.SetMAC(mac)` records it. Cross-VLAN → silent miss
   ([`registry.go:121-123`](../go/internal/drivers/registry.go)).
3. **Driver `driver_init`.** The Lua driver calls `host.set_make(...)`
   unconditionally; `host.set_sn(...)` is called once the driver extracts
   the serial from its protocol. Sungrow reads SN from Modbus input
   registers 4990–4999
   ([`drivers/sungrow.lua:109-120`](../drivers/sungrow.lua)). Ferroamp
   does not currently extract a serial — it relies on MAC-tier identity.
4. **3-second grace period.** A background goroutine sleeps 3 s after
   `reg.Add` completes, then calls `registerAllDevices(st, reg)` which
   snapshots `env.FullIdentity()` for every running driver and upserts a
   row via `RegisterDevice`
   ([`main.go:157-165`](../go/cmd/ftw/main.go),
   [`main.go:607-623`](../go/cmd/ftw/main.go)).
5. **One-shot migration.** Immediately after, `MigrateBatteryModelKeys`
   runs once to re-key legacy `battery_models` rows.

The helper is idempotent by construction — the `COALESCE` upsert means
re-invocations never destroy earlier data. Currently it runs once, 3 s
after boot. Drivers whose SN only arrives after that window (e.g. an
MQTT broker that hasn't yet forwarded a "hello" message) will be stored
under a MAC- or endpoint-tier `device_id` until the next restart
upgrades them to canonical `make:serial`.

## `battery_models` migration

`Store.MigrateBatteryModelKeys` runs once at startup after
`RegisterDevice` has had time to populate. It scans `battery_models` for
rows whose key is a bare driver name (no `:`), looks up the matching
`device_id` via `LookupDeviceByDriverName`, and renames the row. Rows
already keyed on a `device_id` are left alone. Returns the count of
migrated rows. See
[`devices.go:116-148`](../go/internal/state/devices.go).

`SaveBatteryModel(name, json)` and `LoadAllBatteryModels()` translate
`driver_name` ↔ `device_id` internally so the rest of the system (API,
UI, model loader) continues to present models keyed by the friendly
driver name. The translation lives in
[`store.go:317-322`](../go/internal/state/store.go) (`batteryModelKey`).

### Deadlock-avoidance pattern

`LoadAllBatteryModels` reads the `devices` table FIRST into a map, THEN
opens the `battery_models` query. This matters because the store runs on
a single SQLite connection (`SetMaxOpenConns(1)`) and overlapping queries
on one connection deadlock. See
[`store.go:278-305`](../go/internal/state/store.go):

```go
// Deadlock note: SetMaxOpenConns(1) means we cannot run two queries at
// once. Pull all device rows BEFORE opening the battery_models query.
func (s *Store) LoadAllBatteryModels() (map[string]string, error) {
    // Phase 1: build device_id → driver_name reverse map.
    rev := make(map[string]string)
    if drows, err := s.db.Query(`SELECT device_id, driver_name FROM devices`); err == nil {
        for drows.Next() { /* ... */ }
        drows.Close()
    }
    // Phase 2: read battery_models, translating keys via rev.
    rows, err := s.db.Query(`SELECT name, json FROM battery_models`)
    ...
}
```

Legacy rows (bare driver name, no match in `devices` yet) pass through
unchanged so they stay visible during the cold-start window before
`MigrateBatteryModelKeys` runs.

## Operator-visible UI

- Settings → Devices lists every registered device with its `device_id`,
  make, serial, MAC, endpoint, and first/last-seen timestamps.
- Underlying endpoint: `GET /api/devices`, handled by
  [`api.go:777`](../go/internal/api/api.go) (`handleDevices`). Returns:
  ```json
  {
    "devices": [
      {
        "device_id": "sungrow:332407312",
        "driver_name": "sungrow",
        "make": "Sungrow",
        "serial": "332407312",
        "mac": "64cfd94f7c54",
        "endpoint": "modbus://192.168.1.50:502",
        "first_seen_ms": 1712000000000,
        "last_seen_ms":  1712003600000
      }
    ]
  }
  ```

## What survives a rename

| Action | What persists |
|---|---|
| Rename driver in `config.yaml` | Battery model, calibration history, device row |
| Remove + re-add driver | Same as above — `device_id` resolves the same way for the same hardware |
| Move the Zap to a new house | MAC / SN unchanged → identity unchanged → models stay |
| Replace the inverter with a new physical unit | Different SN/MAC → new `device_id` → trained state stays with the old `device_id` row (orphaned but not lost; the old row remains in `devices` with its `last_seen_ms` frozen at the last observation) |

## Future work

- Per-package `CLAUDE.md` for `arp` and `state` (separate documentation
  units cover these).
- The long-format TSDB currently interns `driver_name` as the identity
  (see `ts_drivers` table in
  [`store.go:138-141`](../go/internal/state/store.go)), not `device_id`.
  A follow-up migration can unify these so historical samples survive a
  driver rename without re-interning.
