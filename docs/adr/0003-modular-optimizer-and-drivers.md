# ADR 0003: Modular optimizer and driver delivery

- Status: accepted
- Date: 2026-07-16

## Decision

FTW core remains the only safety and dispatch authority. The mathematical
optimizer and Lua drivers may be versioned, delivered, updated, health-checked,
and rolled back independently from core.

The optimizer speaks `optimizer_protocol_version = 1` over JSON Lines on a
Unix socket. Core validates every returned candidate with the canonical Go
validator before it can reach dispatch. Socket absence, timeout, malformed
output, or incompatibility falls back first to the bundled Python worker during
the migration window and always to the Go DP planner.

Lua drivers continue to run inside core's gopher-lua VM and capability sandbox;
they do not become privileged per-driver containers. The initial host contract
is `driver_host_api = 1`. Repository manifests declare an inclusive
`host_api_min` / `host_api_max`, are signed with Ed25519, and hash every Lua
artifact. Refresh is read-only. Installation and activation are explicit,
content-addressed, atomic, and retain the previous artifact for rollback.

Independent component versions are not required to match the core version.
Compatibility is determined by protocol/host-API ranges. Site sign convention,
hardware-stable `device_id`, clamping rules, and driver capability grants are
unchanged.

## Rollout

The bundled Python optimizer and bundled Lua drivers remain recovery snapshots
for at least two stable releases after modular delivery reaches stable. Driver
updates are manual in the first release. Core slimming happens only after edge
and beta telemetry demonstrates reliable sidecar fallback and rollback.
