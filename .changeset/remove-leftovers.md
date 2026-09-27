---
"ftw": patch
---

Pressing Keep on a driver draft as its countdown ends now waits for the expiry already running, and answers that no draft is running if the old driver was put back, instead of reporting "kept" for a draft that is gone. When the bundled Energyplan worker does not match Core, the error now says to run `ftw update` or reinstall the release, instead of pointing to an Update Center that no longer exists. Plans, history, charts and backups otherwise behave as before: this release removes storage, planner and backup code that no running site reached.
