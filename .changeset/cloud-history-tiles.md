---
"ftw": patch
---

With FTW Cloud on and history in the owner's grant, the box sends FTW Cloud its last 48 hours of 5-minute history, so the iPhone app can draw a node's history while the box is offline. Each closed 12-hour tile is the same bytes the app gets from the box directly. The box adds each 5-minute bucket 30 seconds after it ends, and sends the tiles FTW Cloud lacks when it reconnects. It sends no history without NTP time, and none past the daily byte budget. The extra traffic is about 100 KB a day.
