# FTW in Docker, built from the same release package the native installer
# uses. The package's SHA-256 is checked before it is unpacked. Nothing is
# compiled here, so a Raspberry Pi builds this in about a minute.
FROM debian:trixie-slim

ARG FTW_VERSION
# tzdata keeps zoneinfo for plan windows; curl fetches the package and runs
# the health check.
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates curl tzdata && \
    rm -rf /var/lib/apt/lists/*

RUN set -eu; \
    test -n "${FTW_VERSION}" || { echo "Set FTW_VERSION, for example v0.135.1-beta.1" >&2; exit 1; }; \
    pkg="ftw-linux-$(dpkg --print-architecture).tar.gz"; \
    url="https://github.com/srcfl/ftw/releases/download/${FTW_VERSION}"; \
    cd /tmp; \
    curl -fsSLO "${url}/${pkg}" -O "${url}/${pkg}.sha256"; \
    sha256sum -c "${pkg}.sha256"; \
    mkdir -p /app/data/drivers; \
    tar -xzf "${pkg}" -C /app; \
    rm "${pkg}" "${pkg}.sha256"; \
    ln -s /app/ftw-cli /usr/local/bin/ftw; \
    chown -R 100:101 /app/data

# `docker compose exec ftw ftw status` runs the same status, backup and
# support commands as a native install. Updates change FTW_VERSION instead.

# Numeric uid and gid 100:101, as in the 2.x image, so existing data
# directories keep working. HOME points into the data volume because no
# account exists.
ENV HOME=/app/data
USER 100:101
WORKDIR /app/data
EXPOSE 8080
ENTRYPOINT ["/app/ftw"]
CMD ["-config", "/app/data/config.yaml", "-web", "/app/web", "-drivers", "/app/drivers", "-user-drivers", "/app/data/drivers"]
