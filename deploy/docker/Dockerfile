# FTW in Docker, built from the same release package the native installer
# uses. The package's SHA-256 is checked before it is unpacked. Nothing is
# compiled here, so a Raspberry Pi builds this in about a minute.
FROM debian:trixie-slim

ARG FTW_VERSION
# FTW_RELEASE_MIRROR serves packages at /download/<tag>/, as GitHub does and
# as Core's variable of the same name reads them. It exists to test releases
# that are not published; leave it unset.
ARG FTW_RELEASE_MIRROR
# tzdata keeps zoneinfo for plan windows; curl fetches the package and runs
# the health check.
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates curl tzdata && \
    rm -rf /var/lib/apt/lists/*

# /app/image keeps the package for the launcher. /app holds it unpacked:
# the launcher, the ftw command and, for balena, Core itself.
RUN set -eu; \
    test -n "${FTW_VERSION}" || { echo "Set FTW_VERSION to an exact published new 0.x tag" >&2; exit 1; }; \
    pkg="ftw-linux-$(dpkg --print-architecture).tar.gz"; \
    url="${FTW_RELEASE_MIRROR:-https://github.com/srcfl/ftw/releases}/download/${FTW_VERSION}"; \
    mkdir -p /app/image /app/data/drivers /opt/ftw; \
    cd /app/image; \
    curl -fsSLO "${url}/${pkg}" -O "${url}/${pkg}.sha256"; \
    sha256sum -c "${pkg}.sha256"; \
    tar -xzf "${pkg}" -C /app; \
    cp /app/release-version.json /app/image/; \
    ln -s /app/ftw-cli /usr/local/bin/ftw; \
    chown -R 100:101 /app/data /opt/ftw

# `docker compose exec ftw ftw status` runs the same status, update, rollback,
# backup and support commands as a native install.

# The new, separate data directory belongs to uid:gid 100:101. This does not
# make data from an older FTW compatible. HOME points into the data volume
# because no account exists.
ENV HOME=/app/data
USER 100:101
WORKDIR /app/data
EXPOSE 8080
# Core runs through ftw-launcher from release slots in /opt/ftw, as on a
# native install; compose.yaml keeps them in a volume. The first start
# installs the image's release there. After that the launcher starts the
# release the box updated itself to, and a newer image's release once as a
# trial. Docker's restart policy restarts the launcher as systemd does.
ENTRYPOINT ["/app/ftw-launcher", "-root", "/opt/ftw", "-config", "/app/data/config.yaml", "-user-drivers", "/app/data/drivers", "-image", "/app/image", "run"]
