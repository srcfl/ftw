---
"ftw": patch
---

Vehicle telemetry refresh no longer needs a fresh SoC to find its recipient. Core retains vehicle source time and saves a wake budget across restarts. Tesla BLE recovery requires the paired Tesla driver update; charging commands keep their existing freshness checks.
