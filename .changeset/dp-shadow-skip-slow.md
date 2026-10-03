---
"ftw": patch
---

On a slow box such as a Raspberry Pi 4, the Core DP shadow no longer spends 10 s of CPU on almost every replan with a car plugged in, only to run out of time. After a shadow runs out of time, Core skips shadows of that size or larger for an hour and records each skip in the plan diagnostics, with the reason and the time of the next try. Battery-only shadows still run. The active plan and dispatch do not change.
