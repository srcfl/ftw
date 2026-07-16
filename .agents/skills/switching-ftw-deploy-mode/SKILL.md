---
name: switching-ftw-deploy-mode
description: Use when the user wants to flip an FTW host between the official container image and a host-built development binary, in either direction. Detect and preserve legacy service, directory, and binary aliases rather than assuming them.
---

# Switching FTW deploy mode (official image ↔ development binary)

## Safety contract

Both modes use the existing Compose project and the same persistent `/app/data`
bind. Never create a second install directory, rename the Compose project, or
copy the data directory as part of this switch. Back up an override before
changing it, and use `docker compose up -d <main-service>` rather than
`down`.

The canonical layout is:

| Surface | Canonical | Supported legacy |
|---|---|---|
| Install directory | `~/ftw` | `~/forty-two-watts` |
| Compose service | `ftw` | `forty-two-watts` |
| Container binary | `/app/ftw` | `/app/forty-two-watts` |
| Official image | `ghcr.io/srcfl/ftw:latest` | mirrored `ghcr.io/frahlg/forty-two-watts:latest` |
| Dev binary | `~/ftw-dev/bin/ftw` | `~/ftw-dev/bin/forty-two-watts` |

## Required input

The SSH target (`user@host`) must come from the user. Do not guess or reuse a
host from unrelated context. The default development directory is
`~/ftw-dev`; confirm any different path before writing an override.

## Detect the installed layout first

Run a read-only probe:

```sh
ssh "$HOST" 'set -eu
  if [ -d "$HOME/ftw" ]; then dir="$HOME/ftw"
  elif [ -d "$HOME/forty-two-watts" ]; then dir="$HOME/forty-two-watts"
  else echo "no FTW Compose directory found" >&2; exit 1
  fi
  cd "$dir"
  services="$(docker compose config --services)"
  count="$(printf "%s\n" "$services" | grep -Ec "^(ftw|forty-two-watts)$" || true)"
  [ "$count" -eq 1 ] || { echo "expected exactly one FTW main service" >&2; exit 1; }
  service="$(printf "%s\n" "$services" | grep -E "^(ftw|forty-two-watts)$")"
  docker compose config "$service" | grep -q "/app/data" ||
    { echo "main service does not map persistent /app/data" >&2; exit 1; }
  cid="$(docker compose ps -q "$service")"
  printf "dir=%s service=%s container=%s\n" "$dir" "$service" "$cid"
  docker inspect "$cid" --format "image={{.Config.Image}}"
  ls -1 docker-compose.override.yml* 2>/dev/null || true'
```

Stop if both main service names own `/app/data`, neither does, or the data bind
is absent. Those layouts are ambiguous and must not be recreated automatically.

## Switch to the official image

Disable the development override by renaming it, then pull and recreate only the
detected main service:

```sh
ssh "$HOST" 'set -eu
  if [ -d "$HOME/ftw" ]; then cd "$HOME/ftw"; else cd "$HOME/forty-two-watts"; fi
  service="$(docker compose config --services | grep -E "^(ftw|forty-two-watts)$")"
  if [ -f docker-compose.override.yml ]; then
    mv docker-compose.override.yml "docker-compose.override.yml.dev-$(date +%Y%m%d-%H%M%S).bak"
  fi
  docker compose pull "$service"
  docker compose up -d "$service"
  cid="$(docker compose ps -q "$service")"
  docker inspect "$cid" --format "image={{.Config.Image}} version={{index .Config.Labels \"org.opencontainers.image.version\"}}"'
```

The effective image after removing the override must be the canonical Sourceful
image or its published compatibility mirror. If it is still a local,
hard-coded developer tag, stop: the base Compose file itself was customized and
needs an explicit reviewed migration. Do not silently replace the whole file.

## Switch to a development binary

First locate a canonical or legacy host binary and verify its architecture:

```sh
ssh "$HOST" 'set -eu
  for bin in "$HOME/ftw-dev/bin/ftw" "$HOME/ftw-dev/bin/forty-two-watts"; do
    if [ -x "$bin" ]; then file "$bin"; exit 0; fi
  done
  echo "no executable FTW dev binary found" >&2
  exit 1'
```

Prefer restoring the newest saved development override. It preserves the
user's actual service name and paths:

```sh
ssh "$HOST" 'set -eu
  if [ -d "$HOME/ftw" ]; then cd "$HOME/ftw"; else cd "$HOME/forty-two-watts"; fi
  service="$(docker compose config --services | grep -E "^(ftw|forty-two-watts)$")"
  saved="$(ls -1t docker-compose.override.yml.dev-*.bak 2>/dev/null | head -n1 || true)"
  [ -n "$saved" ] || {
    echo "no saved dev override; review service and host paths before creating one" >&2
    exit 1
  }
  cp "$saved" docker-compose.override.yml
  docker compose up -d "$service"'
```

Only create a new override after the user confirms the detected service, host
binary path, and container target (`/app/ftw` for canonical images,
`/app/forty-two-watts` for a legacy image). A wrong target can leave the
container running the bundled official binary and make the switch look
successful when it was not.

## Verification

Verify all three independently:

1. `docker compose ps <service>` reports one healthy/running container.
2. `docker inspect <container>` shows the intended image and bind mount.
3. The dashboard version/about field or startup log matches the intended
   official tag or development revision.

Do not treat a successful `docker compose up` alone as proof that the mounted
binary is running.
