---
name: switching-ftw-deploy-mode
description: Use when the user wants to flip a forty-two-watts host between the official-image deploy and the dev-binary deploy, either direction. Triggers include "switch to the binary", "switch to docker", "run the raw binary on the pi", "reset my instance to the official version", "put my instance back on latest", "stop the binary, reload with docker-compose".
---

# Switching forty-two-watts deploy mode (official ↔ dev binary)

## Overview

A forty-two-watts host runs `docker compose` in both modes. The difference is a single override file:

| Mode | `docker-compose.override.yml` | Running binary |
|---|---|---|
| **Official** | absent (or `.bak`) | `/app/forty-two-watts` baked into `ghcr.io/frahlg/forty-two-watts:latest` |
| **Dev binary** | present | Host file (typically `~/ftw-dev/bin/forty-two-watts`) bind-mounted over `/app/forty-two-watts` |

Switching = enable/disable the override, then `docker compose up -d` to recreate. The state volume (`./data`) is shared across both modes — no data migration.

## Required inputs

Before running anything, confirm you have:

- **SSH target** — `user@host` (e.g. `pi@10.0.0.5`). If the user didn't provide it, ask. **Do not assume or hard-code a host.**
- **Compose directory** — default `~/forty-two-watts` on the host.
- **Dev-binary dir** (only for dev-mode switches) — default `~/ftw-dev` on the host. The override mounts `$DEV_DIR/bin/forty-two-watts` + `$DEV_DIR/drivers` + `$DEV_DIR/web`.

## Probe current state first

```sh
ssh "$HOST" "cd ~/forty-two-watts &&
  ls docker-compose.override.yml* 2>&1 | head -5
  docker inspect forty-two-watts --format '{{.Config.Image}} | version={{index .Config.Labels \"org.opencontainers.image.version\"}} | rev={{index .Config.Labels \"org.opencontainers.image.revision\"}}'
  docker exec forty-two-watts /app/forty-two-watts -version 2>/dev/null || true"
```

`docker-compose.override.yml` present → currently dev mode.
`.bak` variants only → currently official mode.

## Switching TO official (away from dev binary)

```sh
ssh "$HOST" "set -e
  cd ~/forty-two-watts
  # Disable override (rename, don't delete — user may want to switch back)
  if [ -f docker-compose.override.yml ]; then
    mv docker-compose.override.yml \"docker-compose.override.yml.bak-\$(date +%Y%m%d-%H%M%S)\"
  fi
  docker compose pull forty-two-watts
  docker compose up -d forty-two-watts
  docker inspect forty-two-watts --format 'version={{index .Config.Labels \"org.opencontainers.image.version\"}} rev={{index .Config.Labels \"org.opencontainers.image.revision\"}}'"
```

Verify the reported `version=` matches the latest GitHub release tag.

## Switching TO dev binary (away from official)

Pre-flight — user must have built the binary onto the host. Confirm:

```sh
ssh "$HOST" "ls -la ~/ftw-dev/bin/forty-two-watts && file ~/ftw-dev/bin/forty-two-watts"
```

If missing or wrong arch, STOP and tell the user to run `make build-arm64` + scp. Don't improvise.

Re-enable override + recreate:

```sh
ssh "$HOST" "set -e
  cd ~/forty-two-watts
  # Restore most recent backup if present, else write a fresh one
  latest_bak=\$(ls -1t docker-compose.override.yml.bak-* 2>/dev/null | head -1)
  if [ -n \"\$latest_bak\" ]; then
    cp \"\$latest_bak\" docker-compose.override.yml
  else
    cat > docker-compose.override.yml <<'YAML'
services:
  forty-two-watts:
    volumes:
      - /home/pi/ftw-dev/bin/forty-two-watts:/app/forty-two-watts:ro
      - /home/pi/ftw-dev/drivers:/app/drivers:ro
      - /home/pi/ftw-dev/web:/app/web:ro
YAML
  fi
  docker compose up -d forty-two-watts"
```

Verify the running binary reports the dev build's git-describe (e.g. `v0.43.0-20-g718acab`) — check via the web UI's About/version display, `docker logs forty-two-watts | head` at startup, or `docker exec forty-two-watts /app/forty-two-watts -version` if the binary supports it.

## Common mistakes

- **Hard-coding the host.** The target host is always a user-supplied parameter. Ask if missing.
- **Writing a fresh override with wrong paths.** If the user's dev dir isn't `/home/pi/ftw-dev`, the heredoc above is wrong — prefer restoring `.bak-*` over generating one.
- **Skipping `docker compose pull` when switching to official.** Without it, you'll run whatever stale `:latest` is in the local cache — possibly the exact version the user is trying to move off.
- **`docker compose down && up`.** Unnecessary churn — `up -d` alone recreates containers whose config changed.
- **Deleting the override instead of renaming.** Losing the override loses the record of which host paths were mounted; backup-rename is cheap.
- **Production hosts.** User memory pins this: "docker only in production". Don't switch a production host to dev-binary mode without explicit per-host confirmation.
