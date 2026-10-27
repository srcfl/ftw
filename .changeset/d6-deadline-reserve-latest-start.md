---
"ftw": patch
---

A saved car goal no longer starts the car at full power when no plan is in force, such as just after a restart or after one failed replan. Without a plan the car now waits at 0 W until its latest start: the time the rest of the goal takes at full power, plus one hour. A loadpoint whose minimum charge power is 0 W now also gets this reserve.
