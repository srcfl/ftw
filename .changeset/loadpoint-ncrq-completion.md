---
"forty-two-watts": patch
---

loadpoint: detect CTEK NCRQ (car-side refusal) and stop allocating PV to a phantom EV sink

When a vehicle hits its onboard SoC target mid-session, the CTEK driver reports
`CHRG → NCRQ` ("No Charge Request") — the car has decided it's done, even
though the cable is still plugged in. Before this fix `classify_state` had no
branch for NCRQ, the loadpoint manager kept inferring a low SoC from the
session's plug-in anchor, and the MPC kept allocating multi-kW of PV surplus
to a sink that would never accept it. With a saturated home battery and no
other dump load, the surplus spilled to the grid — sometimes at negative spot.

The fix wires car-side refusal end-to-end:

- `drivers/ctek_hybrid.lua` — `classify_state` recognises `NCRQ` and emits a
  new `request_active` flag in the EV table (false on NCRQ, true otherwise).
- `internal/loadpoint` — `Manager.Observe` takes a `requestActive bool`. When
  the vehicle holds NCRQ past `NCRQCompletionThreshold` (90 s) on a session
  with a configured target, the inferred SoC pins to `targetSoCPct` and
  `SoCSource` becomes `"ncrq"`. The latch clears on plug-out only — a transient
  EVSE retry isn't enough to reopen the allocation.
- `cmd/forty-two-watts` — `telAdapter` parses `request_active` from
  `DerReading.Data`, defaulting to `true` so non-NCRQ-aware drivers (Easee,
  Zap, etc.) keep their existing behaviour.

The pinned SoC then flows naturally into `mpc.LoadpointSpec.InitialSoCPct`
on the next replan: `InitialSoCPct == TargetSoCPct` means the DP allocates
0 W to this loadpoint and the PV/battery curtail-vs-export trade-off no
longer competes against a fictional sink.
