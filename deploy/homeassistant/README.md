# FTW — Home Assistant add-on repository

This directory is a self-contained [Home Assistant add-on
repository](https://developers.home-assistant.io/docs/add-ons/repository) for
**FTW**, kept inside the main repo so it stays in lockstep with the code it
wraps.

```
deploy/homeassistant/
├── repository.yaml          # add-on repository descriptor
└── ftw/
    ├── config.yaml          # add-on manifest (arch, ports, host_network, map)
    ├── build.yaml           # per-arch base image (pinned upstream tag)
    ├── Dockerfile           # rebases upstream image onto Supervisor's /data
    ├── DOCS.md              # user-facing docs shown in the HA UI
    └── README.md            # this add-on's short description
```

## Why it lives here

The previous community add-on
([`erikarenhill/ha-addon-forty-two-watts`](https://github.com/erikarenhill/ha-addon-forty-two-watts))
pinned `ghcr.io/frahlg/forty-two-watts:v1.3.0` and stopped tracking releases.
The project was restructured under Sourceful: the canonical image is now
`ghcr.io/srcfl/ftw` (the old path survives only as a byte-identical mirror),
the binary was renamed `/app/ftw`, and the optimizer runs as a separate
`ftw-optimizer` sidecar container. A hand-maintained external wrapper drifts
against those changes.

Full analysis — what still works, the optimizer single-container limitation,
storage growth, and the distribution plan — is in
[`docs/ha-addon.md`](../../docs/ha-addon.md). Versioning the add-on next to the
code lets each release bump the pinned image tag and the add-on `version:` in
the same commit.

## Consuming it

A HA add-on repository must have `repository.yaml` at its **git root**, so the
Supervisor cannot add the monorepo root directly. Either:

- **Publish/mirror** this `deploy/homeassistant/` subtree to a dedicated
  add-on repo (e.g. `srcfl/ha-addons`) — ideally from CI on each release — and
  point users at that repo URL; or
- **Local test:** copy `deploy/homeassistant/` onto the HA host under
  `/addons` (Supervised) or `/root/addons`, then reload local add-ons.
