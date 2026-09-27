---
"ftw": patch
---

Self-tune now sends its step commands only to the batteries FTW controls. Chargers, meters, PV inverters and telemetry-only batteries no longer receive battery commands during a run, so an EV no longer stops charging and those drivers are no longer marked faulty for five minutes after the tune. Each step still passes the fuse guard, the fuse-saver and the battery's power and SoC limits, so a self-tune can no longer push the site past its main fuse. Starting a self-tune now refuses any name that is not a controllable battery, and any battery named more than once.
