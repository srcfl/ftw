---
"ftw": minor
---

FTW in Docker now updates itself as a native install does. The container runs `ftw-launcher` with its releases in a volume of their own, so **Update FTW** on the box page and in the app, `ftw update` and `ftw rollback` work in Docker, and a release that does not become ready falls back to the one before it. A rebuild with a newer `FTW_VERSION` still updates, through the same trial; an older one changes nothing.

An existing Docker project moves over once: take `Dockerfile` and `compose.yaml` from this release and rebuild. Its `data` stays. Run `ftw update` and `ftw rollback` with `docker compose run --rm --entrypoint ftw ftw …`, since Core's restart ends commands started with `exec`. Balena deployments keep running Core from the image.
