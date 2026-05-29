---
"forty-two-watts": patch
---

fix(mpc): include planned EV loadpoint power when computing PV curtailment limit

`annotateCurtailment` previously only considered house load + battery charge when deciding how much PV can be safely absorbed locally before recommending `pv_limit_w`. When the planner had scheduled EV charging (`LoadpointW > 0`) in a negative-export-revenue slot, the limit would be too low and a curtailment-capable driver could starve the EV session the DP itself had chosen.

The fix adds `max(0, LoadpointW)` to the local-consumption total, matching the accounting already used for battery charging. Updated godoc, docs, and added regression test.

This only affects sites using both planner strategies that can produce export + a PV-curtailment-capable driver + configured loadpoints.