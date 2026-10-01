---
"ftw": patch
---

The Home Assistant bridge now connects when the broker comes up after FTW or starts to accept FTW's login. FTW retries a failed start every 5 seconds at first, backing off to once a minute, until it connects, the settings change or FTW stops. Before, the bridge stayed off until a restart or a settings save.
