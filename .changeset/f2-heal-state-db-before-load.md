---
"ftw": patch
---

Start again after state.db is damaged. When settings cannot be read because state.db is corrupt, FTW restores it from its last snapshot before it reads settings, instead of exiting on every start. When a restore brings back older settings, FTW runs them and reports the step back on /api/health instead of exiting.
