---
"ftw": patch
---

Watch the control loop from outside. When no control tick has finished for 10 seconds (or three intervals, if longer), Core sends every driver its default mode, so a battery leaves a forced setpoint. After 60 seconds Core exits and the service restarts it. An update now becomes current only after its first control tick, and a crash in dispatch math no longer keeps the control lock held.
