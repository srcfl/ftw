---
"ftw": minor
---

FTW now asks each OCPP charger whether it can be steered, and says so. Shortly
after a charger connects, core reads its `SupportedFeatureProfiles` (1.6) or
`SmartChargingCtrlr.Available` (2.0.1), records the raw answer, and shows a
Control column on Settings → Chargers: "smart charging", "telemetry only" (with
a warning explaining the charger will meter but never plan), or "not reported"
for a charger that stayed silent. Also exposed as `steerable` and
`feature_profiles` on `GET /api/ocpp/chargers`. The verdict is advisory —
commands are still attempted, so a charger that under-reports its own
capabilities is never locked out of control.
