---
"forty-two-watts": patch
---

Internal: add the `thermal` package — site-level asset contract types
(`TemperatureBand`, `MarginalPrice`, `DecideIntent`) for the upcoming heat-pump
workstream. Not yet wired into control or MPC, so there is no user-visible
behavior change; it lands as scaffolding the thermal-store model will consume.
