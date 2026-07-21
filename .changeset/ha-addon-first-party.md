---
"ftw": minor
---

Add a first-party, all-in-one Home Assistant OS / Supervised add-on under
`deploy/homeassistant/`, versioned in lockstep with releases, replacing the
stale community add-on (`erikarenhill/ha-addon-forty-two-watts`).

FTW now ships as several containers (core, Python/CVXPY optimizer, updater,
Mosquitto). A HA add-on is single-container, so the add-on **bundles core +
optimizer into one image**: since the two already talk over a Unix socket
(`FTW_OPTIMIZER_TRANSPORT=auto`), the Dockerfile bases on the optimizer image
and copies in the fully static Go core, with a `tini`-supervised `run.sh`
running both. This gives the full CVXPY optimizer with no extra setup — a real
"install one add-on and it works" trial. The updater is dropped (Supervisor
owns updates) and Mosquitto is left to the HA Mosquitto add-on.

Targets canonical `ghcr.io/srcfl/ftw:v1.4.0` + `ghcr.io/srcfl/ftw-optimizer:v1.3.1`
(they version independently), rebases onto Supervisor's `/data`, restores the
`-user-drivers` overlay, and uses host networking (Modbus TCP / LAN MQTT /
mDNS). The full analysis, storage-growth guidance, and remaining items
(CI-validated tag pairing, optional slim variant, options schema) are in
`docs/ha-addon.md`.
