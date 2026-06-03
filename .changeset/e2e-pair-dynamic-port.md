---
"forty-two-watts": patch
---

Fix the e2e pair-flow tests (`TestPairFlow`, `TestPairFlowThroughRelay`) so
they bind a dynamic API port instead of a hardcoded `:8080`. On a machine
where `:8080` is already taken (e.g. an OrbStack / docker control-plane
publishing `0.0.0.0:8080`), the test's main service couldn't bind, `waitForAPI`
silently latched onto the squatter, and the friend's request 404'd — a false
"grant broken" failure. The tests now use the same `freePort` helper
`stack_test.go` already relies on.
