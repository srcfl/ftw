---
"forty-two-watts": patch
---

**Fix: relay landing page rejected every approval code as "Wrong code"
even when the friend typed the right one.** The `fmt.Fprintf` that
renders the landing HTML in `ftw-relay`'s `publicLanding` passed format
arguments in the wrong order, so the embedded JS `const TOKEN` was
populated with the token state (`"pending"`) instead of the actual
session token. The Activate button then POSTed to
`/h/pending/approve`; the relay couldn't find that token and returned
`403 Forbidden`, which the page surfaced as "Wrong code" regardless of
what was typed. As a side effect "From:" showed the token, "Intent:"
was empty, and "State:" showed the intent.

Argument order is now `as → intent → state → token`, matching the
positional verbs in `landingHTML`. A regression test
(`TestLandingPageTokenConstMatchesPath`) pins the JS const + each label
row so a future reshuffle can't silently regress the approve POST path.
