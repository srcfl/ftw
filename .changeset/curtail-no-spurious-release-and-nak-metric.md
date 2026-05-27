---
"forty-two-watts": patch
---

**Defense-in-depth against the 2026-05-27 Ferroamp brick.** Two
independent changes that, combined with PR #367's driver-side hard
fail on `pplim arg=0`, eliminate every known trigger path:

- **Dispatcher**: `ComputePVCurtail` no longer emits a `curtail_disable`
  release simply because a previously-curtailed driver dropped out of
  the proportional allocation due to its own `|PV|` crashing to ~0
  (often a direct consequence of OUR curtail throttling that driver
  down). The release is now only sent when the curtail directive
  truly clears, or the driver is removed from `SupportsPVCurtail`, or
  the driver goes offline. Also: per-driver allocations rounding to
  `≤ 1 W` are suppressed entirely — never publish a near-zero
  `pplim` that some inverters treat as a hard "limit to 0 W" lock.

- **Ferroamp driver**: subscribes to `extapi/control/response`
  (was: `extapi/result` — wrong topic, never received anything),
  parses `{"status":"ack|nak", ...}` responses, and exposes
  cumulative `extapi_nak_count` + `extapi_ack_count` metrics. NAK
  responses are also logged as warnings with `transId` + `msg`
  fields. The 2026-05-27 brick was preceded by minutes of
  `nak: no available ESOs detected in system` that we couldn't see
  through ftw telemetry — now the operator can alert on any non-zero
  NAK rate.

Tests added:
- Four new dispatcher regressions in `control/pv_curtail_test.go`
  guarding the suppression / release semantics.
- One driver test in `drivers/lua_ferroamp_curtail_test.go`
  asserting NAK + ACK counter advancement.
