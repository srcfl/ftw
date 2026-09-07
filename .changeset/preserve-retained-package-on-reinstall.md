---
"ftw": patch
---

Check retained package metadata before reinstalling a driver from an older
database. Reject a change to direct-manifest format while a package envelope
remains, and require signed legacy metadata when its format is unknown.
A rejected reinstall preserves the active artifact and rollback record.
