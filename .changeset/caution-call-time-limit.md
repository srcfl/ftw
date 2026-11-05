---
"ftw": patch
---

The forecast caution plan is no longer lost to a late call. Core fixed each
worker call's time limit when planning started, so after a point plan the
caution call could ask the worker for more time than the call had left. A
worker that used its full time limit then answered after Core had given up,
and Core published the point plan instead. Each call's time limit now fits
the time left before that call's deadline, and Core skips a call that has too
little time left, as it already does near the end of a slot.
