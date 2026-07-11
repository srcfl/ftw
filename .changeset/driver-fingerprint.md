---
"forty-two-watts": minor
---

feat(drivers): driver-level device fingerprinting + scan auto-detect

Drivers can now identify whether an arbitrary endpoint is one of their own
devices via a new optional Lua lifecycle hook, `driver_fingerprint()`. It
returns a deliberate tri-state — `true` (positive signature match), `false`
(talked to it, it's *not* mine), or `nil` (inconclusive / not supported) —
plus an optional `{make, model, serial, confidence}` identity hint. The
probe is passive: `driver_init`/`driver_cleanup` are never run, so a
fingerprint can't reconfigure the device (no Sungrow power-limit writes, no
SolarEdge curtail-register clears).

New `POST /api/drivers/fingerprint {host, port, protocol?, unit_id?}` takes
an open endpoint discovered by a network scan (e.g. port 502 or 80), runs
every catalog driver that speaks that protocol against it, and returns the
ranked matches (plus every candidate's verdict for transparency).
`GET /api/scan?fingerprint=1` folds this into discovery: each open Modbus
or HTTP host comes back annotated with the drivers that recognise it —
turning "port 502 is open on 10.0.0.7" into "that's a SolarEdge". The
default `/api/scan` response is unchanged.

Both Modbus (port 502) and HTTP (port 80) are fingerprintable. The hook
receives a `target` table (`host`, `port`, `protocol`, `base_url`) so HTTP
drivers can build their probe URL.

Ships signatures for four drivers:
- **SolarEdge** — SunSpec `"SunS"` marker + common-block manufacturer
  string on input registers, with serial extraction.
- **Pixii** — same SunSpec common block but on holding registers
  (manufacturer "Pixii"), with serial extraction.
- **Sungrow** — SH-hybrid device-type code.
- **Zap** (Sourceful) — HTTP `GET /api/devices` device-list signature,
  latching the P1 serial as identity.

Drivers without a `driver_fingerprint` hook simply report `unknown` and are
never false-positives.
