---
"forty-two-watts": minor
---

setup: identify scanned network devices by driver fingerprint, with hostname + an "Other devices" group

The setup wizard's network scan now tells you *what* each device is, not just
that a port is open. After the port scan, every scan hit is run past the driver
catalog: each driver whose declared protocol matches the open port gets to
answer "is this me?" via a new read-only `driver_fingerprint()` Lua lifecycle
function (read a known register / await a known MQTT topic). The first driver
that matches claims the host and reports the capabilities it actually detected
on *that* device.

- **New Lua lifecycle hook** `driver_fingerprint()` — strictly read-only
  (the probe path tears the VM down with a new `LuaDriver.Close()` that skips
  `driver_cleanup`, so a driver's failsafe writes never fire against an
  unidentified host on the LAN). Implemented for Ferroamp (MQTT),
  Pixii (Modbus SunSpec-on-holding), and SolarEdge — including separating
  modern HD-Wave (SunSpec on input registers) from K-series legacy (holding
  only), and detecting whether a revenue meter is actually wired in so the
  reported capabilities match the real install (`pv` vs `pv`+`meter`).
- **New endpoint** `POST /api/scan/identify` — takes scan hits, fingerprints
  each host against protocol-matching catalog drivers (parallel, bounded,
  short per-driver timeout), returns the matched driver + capabilities, or
  flags the host unidentified.
- **Auto-detects battery nameplate capacity** where the device exposes it:
  Pixii reports its SunSpec model-802 `WHRtg`, which the fingerprint returns and
  the wizard pre-fills into the battery-capacity field (operator confirms).
  (Ferroamp's extapi doesn't publish a nameplate figure, so it stays manual.)
- **Scan now resolves hostnames** — unicast reverse DNS, falling back to a
  reverse **mDNS** PTR query (most home-energy gear has no PTR record but does
  answer mDNS), shown on a line under the IP. The setup table gains a
  "Device & capabilities" column; identified devices are listed first, with two
  actions — **Configure** (review in the wizard) and **Add** (one-click, using
  the fingerprinted driver + detected settings; the row dims to "Added ✓" once
  in the config). Hosts no driver can claim (including auth-gated devices) are
  grouped under "Other devices", Configure-only.
- **Scans routed LANs too**, not just interface-attached subnets: the scanner
  reads the routing table (PF_ROUTE on darwin/BSD, `/proc/net/route` on Linux)
  and adds any RFC1918 /24../28 networks it finds, so a second LAN reached via a
  static route is swept automatically.
- **Extra Modbus scan ports** 503 and 1502 — proxied inverters commonly sit
  behind a TCP proxy here (a Pixii on 503, a SolarEdge via solaredge-proxy on
  1502, which multiplexes the inverter's single Modbus socket).
- SolarEdge fingerprinting is **holding-register only** (FC 0x03): that's the
  standard SunSpec map and the only one a solaredge-proxy answers. The driver
  never probes input registers during a scan — a timed-out FC 0x04 read wedges
  such a proxy's upstream socket for seconds. The **right** SolarEdge driver is
  picked from the SunSpec C_Model name (holding reg 40020): a K-series display
  unit (SE7K/10K/17K/25K) → `solaredge_legacy` (FC 0x10 curtail), everything
  else (e.g. an SE8K HD-Wave) → `solaredge_pv` (FC 0x06 curtail). The
  input-register `solaredge.lua` (+meter) stays manual-pick — input can't be
  safely probed.

No operator action required; drivers without a `driver_fingerprint` simply fall
into "Other devices" until one is added.
