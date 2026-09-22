---
"ftw": patch
---

Resume history archive work from saved progress, bound each maintenance turn, and let full backups pause maintenance. A stage that uses up its time budget stays pending and resumes, instead of being reported as a failure. A running archive query is cancelled with that same budget. Avoid repeated history scans and show archive and backup progress while preserving live writes and verified archive checks.
