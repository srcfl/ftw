---
"ftw": minor
---

A native install now has the `ftw` command on the machine: `ftw status`, `ftw update`, `ftw rollback`, `ftw backup` and `ftw support`. Updates on a native install are run there, by hand or from the owner's own automation; `ftw update` asks no questions, waits through the restart and exits 0 when the box is current. The web version panel on a native install shows the running version, a published release and the command, without update, rollback, channel or backup controls, and setup no longer offers an update. A native update that is slow to start is no longer reported as failed after five minutes. Starting a second Core on a port FTW already holds says that FTW is already running.
