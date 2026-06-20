---
"forty-two-watts": patch
---

MyUplink driver self-heals instead of needing a manual restart. NIBE/MyUplink
is touchy right after consent (token propagation / rate-limit), so the first
auth or device-detect can fail — previously the driver then idled forever on a
nil device_id and only came online after the operator hit Restart. driver_poll
now retries setup (auth + device detection) with a 30 s backoff, so it recovers
on its own within a poll or two.
