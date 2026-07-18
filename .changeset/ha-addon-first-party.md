---
"ftw": minor
---

Add a first-party Home Assistant OS / Supervised add-on under
`deploy/homeassistant/`, versioned in lockstep with releases, replacing the
stale community add-on (`erikarenhill/ha-addon-forty-two-watts`) that was
pinned to `ghcr.io/frahlg/forty-two-watts:v1.3.0`.

The project was restructured under Sourceful: the canonical image is now
`ghcr.io/srcfl/ftw` (the old path survives only as a byte-identical mirror),
the runtime binary is `/app/ftw`, and the MPC optimizer runs as a separate
`ftw-optimizer` sidecar. A hand-maintained external wrapper can't follow that
drift.

The in-tree add-on targets the canonical `srcfl/ftw` (pinned `v1.4.0`),
rebases it onto Supervisor's `/data` volume, restores the `-user-drivers`
overlay so hot-reload Lua drivers persist across updates, and uses host
networking (Modbus TCP / LAN MQTT / mDNS). README gains an install section;
the full analysis, storage-growth guidance, and remaining packaging
limitations (optimizer sidecar in a single-container add-on, `/data` sizing,
distribution/mirror automation) are in `docs/ha-addon.md`.
