# Calendar removal

FTW no longer runs a CalDAV server or reads calendar events. The Calendar
settings tab, subscription feeds and port 5232 have been removed.

Before upgrading, move any future calendar charging events to the usual
loadpoint targets and ready-by schedules. Existing saved loadpoint goals
remain in place. Away events no longer change load forecasts; new forecasts
use the existing home default. Saved forecasts keep their original inputs.

Old configuration files still load. FTW ignores the `caldav` section and
logs a warning when it was enabled. Remove this section when you next edit
the file, and remove calendar accounts or subscriptions from your phone.

The upgrade keeps existing calendar objects and credentials in `state.db`.
Back up the database and the old config before upgrading if you may need to
return to an older release. The database backup includes those old tables;
a fresh database no longer creates them.
