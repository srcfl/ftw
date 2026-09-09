---
"ftw": patch
---

After a verified DuckDB history import, hide the import UI on later boots and drop the leftover SQLite history tables and imported Parquet files. DuckDB keeps history; SQLite keeps configuration. A failed or interrupted import still keeps the original sources. Full backups still export portable SQLite history so restore and older Core can read it.
