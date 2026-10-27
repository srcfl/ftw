---
"ftw": patch
---

Keep OCPP charger commands flowing when a charger's boot or session message triggers slow work, such as a device registry write. That work now runs after the charger's lock is released, and a charging limit command always returns by its deadline, so one stalled write can no longer block fuse cuts to every charger and then the control loop. Each charger also holds at most one unanswered charging limit, so a stall no longer leaves a backlog of old limits to replay ahead of the current one.
