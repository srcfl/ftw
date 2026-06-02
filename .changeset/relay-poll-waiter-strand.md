---
"forty-two-watts": patch
---

**Fix: help-a-friend hung after the session sat idle past one long-poll
cycle.** The relay's tunnel queue (`go/internal/tunnel/queue.go`) leaked a
waiter channel every time a host long-poll timed out: `Poll` returned
`ErrPollTimeout` (and on the context-cancel path too) without removing its
entry from `q.waiters[hostID]`. The next friend request was then handed by
`Enqueue` to that abandoned, never-read buffered channel and silently lost —
the request blocked until the friend's client gave up.

This bit the real flow because a friend naturally takes longer than one
poll-timeout (25 s in production) between approving the session and pasting
the `claude mcp add …` command, so the **first** MCP request reliably landed
after a poll had already stranded a waiter. Result: `initialize` (or
`tools/list`) hung and Claude Code reported the MCP server as unreachable,
even though approval and the grant exchange had succeeded.

`Poll` now deregisters its waiter on both the timeout and context-cancel
exits, and if a producer claimed the waiter in the same instant it delivers
that buffered request instead of dropping it. Regression tests cover the
timeout-strand, ctx-cancel-strand, and the producer/timeout handoff race.
