---
"ftw": patch
---

Let the offline backup helper copy large histories without the live 100 ms pause that made a two-hour export deadline unreachable. Keep live backup yielding between copy batches, and refuse to start when the destination cannot hold the raw export, compressed archive and verification extract. The Raspberry Pi durable-goal latency requirement remains open in #1246.
