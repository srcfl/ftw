---
"ftw": patch
---

Send a battery its default mode again when it comes back from an outage.
A device that refused commands because it was unreachable no longer waits
5 min out of control. On its first good reading FTW resends the default and
lets one command through. Before, an inverter could keep its last forced
charge or discharge for up to 5 min after it came back. A device that reads
fine and keeps refusing still waits 5 min between tries.
