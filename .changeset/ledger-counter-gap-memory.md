---
"ftw": patch
---

Prevent history writes from exhausting database memory when several energy counters return after a long gap. Write each interval's five-minute buckets in one SQL statement while keeping history, samples, energy, cursor updates and retry receipts in the same transaction.
