---
"ftw": patch
---

Update the compiled planner to reject a PV maximum without a minimum control capability instead of silently ignoring the maximum. Valid requests keep their existing validation and planning behavior.
