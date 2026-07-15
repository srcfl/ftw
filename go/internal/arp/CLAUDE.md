# arp — best-effort IPv4 → MAC resolution for hardware-stable device identity

## What it does

Resolves an IPv4 address to its MAC on the local L2 segment so Modbus-TCP / HTTP / LAN-MQTT devices get a stable `device_id` even when they don't expose a serial number in their protocol. Pure Go; no CGo; no raw sockets. Linux reads `/proc/net/arp`; macOS shells out to `/usr/sbin/arp`. Before every lookup, a single 50 ms TCP probe is sent to one of `{80, 502, 1883}` to nudge the kernel ARP cache — the SYN packet triggers ARP even if the connect itself fails.

## Key types

No public types — one package-level function and one test-stub `readFile`.

## Public API surface

- `Lookup(ipStr string) (mac string, ok bool)` — returns `("", false)` for non-IPv4 input, cross-VLAN hosts, or "(incomplete)" entries (`arp.go:28`).
- `readFile` (unexported, overridden in tests) — indirection over `os.ReadFile` so `lookupLinux` can be unit-tested against a fixture.

## How it talks to neighbors

`../drivers.Registry.ARPLookup` is set to `arp.Lookup` in `cmd/ftw/main.go:138`. At `Registry.Add` time, when a driver has an MQTT or Modbus config, the registry extracts the host string, calls `ARPLookup(host)`, and records the MAC on `HostEnv` via `SetMAC` (`drivers/registry.go:121-134`). That MAC then flows into `state.RegisterDevice` via `HostEnv.FullIdentity()`, producing a `mac:<addr>` device_id preferred over the `ep:<hash>` fallback. See `docs/site-convention.md` for how identity roots downstream state (battery models, history).

## What to read first

`arp.go` is one file. Read `Lookup` for the call path, then `lookupLinux` for the `/proc/net/arp` parse (flags field is ignored — "incomplete" presents as `00:00:00:00:00:00`, rejected at line 62) and `lookupDarwin` for the macOS `arp -n` parse including the single-digit-octet normalization at lines 84-86.

## What NOT to do

- **Do NOT expect cross-subnet resolution.** The kernel only ARPs within L2. If the device is behind a router, `Lookup` returns `("", false)` and that's intentional — device_id falls back to the endpoint hash (`drivers/registry.go:119-122` says "that's fine").
- **Do NOT call `Lookup` from a hot path.** The TCP nudge is 50 ms × up-to-3 ports worst case. It's meant to run once at driver-Add time, not every poll.
- **Do NOT add Windows support silently.** The current switch falls through to `("", false)` on non-linux/darwin (`arp.go:44-45`). A Windows implementation would need `GetIpNetTable` or `arp -a` parsing, and should be a new file.
- **Do NOT rely on the probe for connectivity checks.** The probe is silent on failure by design — a closed port 80 still resolves ARP because the kernel sends the SYN. Use the driver's own protocol for "is the device up?".
- **Do NOT normalize MACs elsewhere.** Both OS parsers already lowercase and zero-pad; keep consumers consuming the canonical `aa:bb:cc:dd:ee:ff` form.
