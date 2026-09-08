---
"ftw": patch
---

Start live collection after importing the catalog and energy accounting, then move large sample archives into DuckDB in the background. Resume verified progress after an interruption, preserve live writes, and report incomplete history until every source is checked. Release native database buffers only between active SQL connections.

Give Core startup more time and leave its image and data in place if readiness fails. Preserve the previous image ID before replacement. Check the updater before opening data, and stop with a clear message when an older installation needs to update its updater first.

Keep catalog ID allocation safe after an abrupt process exit, and defer raw-history retention until import finishes.
