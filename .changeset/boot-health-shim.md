---
"ftw": patch
---

Answer health probes during slow boots.

The API port is now bound before the state DB opens, serving
`/api/health` with 200 `{"status":"starting"}` (and 503 elsewhere) until
the real mux takes over on the same listener. Previously a boot that ran
a one-time VACUUM or a full integrity check on a multi-GB database left
the port unbound for up to tens of minutes, so the Docker healthcheck
failed and the self-update sidecar judged the deploy failed and rolled
back in the middle of the compaction.
