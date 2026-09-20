---
"ftw": patch
---

Keep dashboard rollups within their source range so a large recent history cannot make every retention attempt time out. Read summaries outside the live writer lock, preserve late rows on retry, and report a bucket that cannot fit its write budget instead of retrying it for hours.
