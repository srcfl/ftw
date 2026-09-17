---
"ftw": patch
---
Keep EV session and battery-model disk writes outside the control loop. Confirm saved choices only after a durable write, keep pending writes bounded, and preserve manual Stop across queued updates and restart.
