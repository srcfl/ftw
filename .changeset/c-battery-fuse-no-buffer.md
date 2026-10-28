---
"ftw": patch
---

When the home battery cuts its charge or discharge to protect the fuse, it now cuts only by the overage, so the grid settles on the limit instead of below it. It used to cut half of `safety_margin_a` more on top. Sites without a safety margin see no change.
