---
"ftw": patch
---

Project statistics are part of FTW Cloud and nothing else. With FTW Cloud on, the box sends the hourly reports, which Sourceful links to your Cloud home for 48 hours so it can delete them, and the daily reports with no box id, as before. Without FTW Cloud it sends none, whatever an earlier release stored, and a statistics-only link from an earlier release ends. That includes a collector of your own set in `fleet_ping.endpoint`: without FTW Cloud it gets no daily report, and `fleet_ping.enabled` is no longer read. The "Share anonymous statistics" switch is gone; Settings → FTW app → Statistics says which case holds. Accepting the terms when you pair, on the box page, in the app or in the web app, is enough: the box no longer asks again, and the Overview no longer prompts for them. An assistant's access now lasts 90 days unless you pick another length, on the box page and for invitations.
