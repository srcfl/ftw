---
"ftw": patch
---

Keep history queries and live commits responsive during archive building. Protect only file publication and bounded pruning against readers, and release archive write locks before retrying database contention.
