---
"forty-two-watts": patch
---

Fix the RPi image's port 80 → 8080 redirect, which never worked.

The nftables rules file named its table `42w_redirect`. nftables identifiers
must match `[a-zA-Z_][a-zA-Z0-9_]*`, so the leading digit was tokenized as a
number and the entire file failed to parse. `42w-port-redirect.service` exited
1 on every boot and no redirect was ever installed, leaving the dashboard
reachable only at `http://42w.local:8080/` rather than the bare
`http://42w.local/` the README advertises.

Renamed the table to `ftw_redirect`. The rule logic was otherwise correct.
