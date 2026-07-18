---
"ftw": patch
---

Make Core and updater an atomic versioned control-plane pair. Release checks now require a shared manifest with both immutable image digests, Core stays unready until the matching updater is present, and paired updates retain and restore both previous image IDs on failure.
