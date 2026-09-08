---
"ftw": patch
---

Use the running device's identity for energy counters. Wait for its known
serial at startup so a temporary MAC or endpoint alias cannot count the same
energy twice. Continue saving raw measurements while identity is pending.
