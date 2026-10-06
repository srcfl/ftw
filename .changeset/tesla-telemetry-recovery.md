---
"ftw": patch
---

Vehicle telemetry refresh no longer needs a fresh SoC to find its recipient. Core retains vehicle source time and saves a wake budget across restarts. The release includes Tesla BLE driver 0.2.5 for bounded recovery; charging commands keep their existing freshness checks. It also carries the merged Heishamon 0.8.0 and MyUplink 1.2.3 driver updates.
