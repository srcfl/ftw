---
"ftw": patch
---

With FTW Cloud on, the box now sends the daily energy report: the date and that closed UTC day's six whole-kWh sums, with no box id. It goes straight to FTW Cloud at `/v1/energy`, which sees the box's address while the request is open and keeps neither the address nor the report. The report had waited for an independent relay that never came, so no box sent one and stats.ftw.energy showed no energy. Without FTW Cloud nothing is sent. Settings → FTW app → Statistics says where it goes.
