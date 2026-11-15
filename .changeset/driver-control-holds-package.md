---
"ftw": patch
---

Internal: operator driver-control holds moved from the API into their own
package so a later planner can share the same lock and expiry, and the unused
thermal policy draft (`DecideIntent`) is removed. No behaviour change.
