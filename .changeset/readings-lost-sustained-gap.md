---
"ftw": patch
---

Stop one late poll from reading as "Lost control".

A home box with a Sungrow hybrid logged 341 "Lost control" alarms for its
battery in two weeks, each one 5 s journal sample long. The journal samples
at its timer's time, and the status page takes its clock before it waits
for the controller. A battery poll stored in between looked newer than the
clock, so Core called the reading stale, and a stale reading became lost
readings at once. A charger that said it was offline for one poll did the
same. Core now judges a device at the latest of its own clock and the
store's stamps on what it read. Readings count as lost once neither
measured power nor a new order has come for 30 s, or 2 min for a charger,
the same grace a new order gets. A reading over a minute old, or a driver
the watchdog gave up on, is still lost at once. Dispatch and the stale site
meter stop are unchanged.
