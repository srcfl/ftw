---
"ftw": patch
---

Return history storage to SQLite and Parquet. Store goals and charging state in a separate database, preserve older summaries, verify archives before pruning, and bound history reads and backup writes. DuckDB beta installations use a separate offline converter that keeps their original history files. Block image-only rollback to an older history format. Send known EV safety stops before session persistence.
