---
"forty-two-watts": patch
---

Fix a relay tunnel **poll-waiter** bug: a timed-out long-poll left a dead waiter
channel in the queue, so a later request was handed off to that dead poller and
**silently dropped — hanging the caller forever** (the remote dashboard would
"just load" once a host had idled long enough to accumulate dead waiters).
Timed-out and cancelled waiters are now removed from the queue, and the channel
is drained first so a request handed off in the race window is never lost.
