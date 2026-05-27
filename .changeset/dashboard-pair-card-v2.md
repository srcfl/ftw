---
"forty-two-watts": minor
---

**Pair-card v2 with real relay presence + voice-channel approval.** When the friend opens the relay URL, the dashboard now surfaces the full URL with a Copy button, the 4-digit voice-channel approval code in big numbers, and an inline Allow form that POSTs the typed code straight to the relay's `/h/<token>/approve` once the operator hears the matching digits from their friend on voice. The misleading "0 clients connected" counter is replaced with a live presence indicator (live / active / idle / pending / dead) driven by a new `GET /tunnel/sessions/<token>/info` endpoint on the relay that tracks landing-page hits + last-tunneled-request timestamps; ftw-pair polls it every heartbeat and forwards the snapshot to `/api/pair/status`.

The friend-message template is rewritten for the URL flow — no more `curl install-ftw-connect.sh` references, no more old binary install path. Operator-facing security: if the friend reads back a code that doesn't match the one shown on the dashboard, the validator refuses to approve and warns "leaked URL".

Pure render helpers split into `web/components/ftw-pair-card-render.js` and covered by 42 `node --test` cases (state-machine snapshots, golden-string assertions on the friend message, source-hygiene checks that catch regressions where someone re-introduces `ftw-connect` references). Run with `npm test` from the repo root.
