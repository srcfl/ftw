---
"ftw": patch
---

When the home battery cuts its charge or discharge to protect a fuse phase, it now cuts only by the overage, so the phase lands on the fuse less `safety_margin_a`. It used to cut half the safety margin more on top. Sites without a safety margin see no change.
