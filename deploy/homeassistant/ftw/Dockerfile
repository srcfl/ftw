# Wraps the upstream multi-arch FTW image so it runs against Home Assistant
# Supervisor's /data volume instead of the image-native /app/data. Supervisor
# builds this locally on install using build.yaml's per-arch `build_from`
# (injected as BUILD_FROM); no add-on-side CI needed.
#
# Why a wrapper and not a plain `image:` reference in config.yaml:
# the upstream image bakes in WORKDIR /app/data, VOLUME /app/data, a non-root
# USER (100:101), a HEALTHCHECK, and a CMD that hardcodes /app/data paths. HA
# add-ons persist /data (owned by root) and cannot override CMD on a prebuilt
# `image:`. Rebasing here lets us repoint all of that at /data in one place.
#
# The upstream ENTRYPOINT is /app/ftw. Because we override CMD with FLAGS ONLY
# (not the binary path) and inherit ENTRYPOINT, the wrapper is unaffected by
# the binary name — the -config/-web/-drivers/-user-drivers flag interface is
# what matters and is unchanged.
ARG BUILD_FROM
# hadolint ignore=DL3006
FROM ${BUILD_FROM}

# Supervisor owns /data as root; run as root so the app can create
# config.yaml + state.db there. (The upstream image uses uid 100 for the
# standalone docker-compose deploy, which is not how add-ons persist.)
USER root
WORKDIR /data

# The app resolves relative state paths (state.db, cold/) against the CWD, so
# WORKDIR /data is load-bearing — it puts persistent state on the add-on
# volume. -user-drivers /data/drivers keeps the /app/drivers image bundle
# immutable while letting operators drop hot-reloadable Lua under
# /data/drivers, which survives add-on updates. Mirrors the upstream CMD.
CMD ["-config", "/data/config.yaml", "-web", "/app/web", "-drivers", "/app/drivers", "-user-drivers", "/data/drivers"]
