---
"ftw": patch
---

A cloud driver can name the WebSocket hosts it streams from in its `DRIVER` table as `ws_hosts`, the way it names `http_hosts`. When the driver's config already grants HTTP, FTW grants the WebSocket for those hosts, so a saved driver needs no `capabilities.websocket` entry; hosts the config lists are kept. A driver without HTTP gets no network access this way.

Opening a WebSocket now gives up after 5 s instead of 15 s, ends at once when the driver call is cancelled (a stand-down or a stop), and names the HTTP status when the server refuses the upgrade. The driver's poll and its commands wait for that dial.
