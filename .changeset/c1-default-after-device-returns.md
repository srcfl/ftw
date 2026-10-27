---
"ftw": patch
---

Let a battery back into control as soon as it reads again after an outage.
A device that refused commands because it was unreachable, even for less
than a minute, no longer stays out of dispatch for 5 min. On its first good
reading FTW sends its default mode again and takes it back into dispatch,
so FTW's next command reaches it at once. If FTW has no new command for it,
the device keeps whatever it last ran until the default takes hold. On a
Sungrow the resent default does nothing yet, so an old forced command can
still run until FTW sends a new one; that needs a driver fix. A device that
reads fine and keeps refusing still waits 5 min between tries.
