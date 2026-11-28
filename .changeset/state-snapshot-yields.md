---
"ftw": patch
---

The daily recovery copy of the settings database no longer holds up live history writes. It used to copy each table in one large write, and on an SD card a history save could wait more than five seconds behind it, past its deadline; this happened two minutes after boot and once a day. The copy now writes short synced batches from one point in time and pauses between them, as a backup does. It also keeps the control journal's next id, so a restored copy never reuses one.
