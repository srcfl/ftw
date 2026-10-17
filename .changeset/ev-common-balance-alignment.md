---
"ftw": patch
---

Avoid false EV stops when the common balance revisits an older site observation after an observed EV stop/restart, and avoid a full battery discharge fallback when a fresh battery observation already precedes the latest site reading. Keep independent latest-site, phase, pending-order and unknown-history bounds.
