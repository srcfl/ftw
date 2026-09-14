---
"ftw": patch
---

Copy numeric DuckDB history in larger bounded batches to reduce migration time on Raspberry Pi storage. Keep full readback checks and resume interrupted conversions from the saved row cursor.

Keep state read-only until the verified history is selected, so an interrupted converter cannot change its own source through a SQLite checkpoint. Treat absent and empty SQLite WAL files alike while still checking every nonempty WAL.
