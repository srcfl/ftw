---
"ftw": patch
---

Native migration keeps waiting when Core drops the API socket during restart, instead of treating that disconnect as a failed install and rolling back.
