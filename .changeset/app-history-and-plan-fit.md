---
"ftw": patch
---

The FTW app gets history, and the plan no longer kills the session. History tiles are served from the energy ledger over the app protocol — same tile geometry, ids and etags as the app computes, so unchanged tiles are never resent; mean power per bucket, MISSING where nothing was metered, retention gaps named instead of served as silence. The plan answer is truncated to the slots that fit the largest bulk bucket: the optimizer's 193 slots encode past 16 kB, and the encode error used to drop the whole session — which the app experienced as the box hanging up whenever the plan view opened.
