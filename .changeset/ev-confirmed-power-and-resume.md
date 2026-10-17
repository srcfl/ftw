---
"ftw": patch
---

Retain fresh confirmations of change-only power samples in the bounded control buffer without changing source times or counting repeated confirmations as new physical samples. Require ten seconds of fresh, meaningful charging before ending EV resume tracking, so small startup pulses cannot cancel recovery.
