---
"forty-two-watts": minor
---

**Ferroamp self-healing watchdog for the sticky-pplim trap.** When the
SSO reports the post-incident signature — DC bus voltage > 200 V, zero
PV current, no fault, relay closed — continuously for ten minutes, the
driver now auto-publishes `pplim arg=<pplim_release_w>` to release
the lock. Operator opts in by setting `config.pplim_release_w > 0`;
without it, the watchdog logs a per-incident warning but does not
publish (we have no safe release value to send).

A five-minute cooldown between successive recoveries prevents command-
spam if the release doesn't take. A new `stuck_pv_recovery_count`
metric tracks lifetime recovery count so operators can alert on a
chronic condition.

Reuses the existing `pplim_release_w` field — same value, dual
purpose (dispatcher `curtail_disable` release AND watchdog
self-recovery).

Layered with [#367](https://github.com/frahlg/forty-two-watts/pull/367)
(driver hard-fail on `pplim arg=0`) and the dispatcher fix in the
parallel PR (`fix(curtail): no spurious release ...`) this is the
third and final layer of defense against the 2026-05-27 brick.
