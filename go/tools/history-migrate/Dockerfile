# Separate offline tool. Never used by the normal Core image or release.
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build
ARG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends gcc-aarch64-linux-gnu g++-aarch64-linux-gnu gcc-x86-64-linux-gnu g++-x86-64-linux-gnu && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go /src/go
WORKDIR /src/go/tools/history-migrate
RUN case "$TARGETARCH" in arm64) export CC=aarch64-linux-gnu-gcc CXX=aarch64-linux-gnu-g++ ;; amd64) export CC=x86_64-linux-gnu-gcc CXX=x86_64-linux-gnu-g++ ;; *) exit 1 ;; esac; \
    CGO_ENABLED=1 GOOS=linux GOARCH="$TARGETARCH" go build -trimpath -o /out/ftw-history-migrate .
COPY go/tools/history-migrate/THIRD-PARTY-NOTICES.txt /out/THIRD-PARTY-NOTICES.txt
FROM debian:bookworm-slim AS runtime
RUN apt-get update && apt-get install -y --no-install-recommends libstdc++6 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/ /usr/local/bin/
COPY LICENSE NOTICE /usr/share/doc/ftw-history-migrate/
ENTRYPOINT ["/usr/local/bin/ftw-history-migrate"]

FROM scratch AS export
COPY --from=build /out/ /

FROM build AS test-build
ARG TARGETARCH
RUN case "$TARGETARCH" in arm64) export CC=aarch64-linux-gnu-gcc CXX=aarch64-linux-gnu-g++ ;; amd64) export CC=x86_64-linux-gnu-gcc CXX=x86_64-linux-gnu-g++ ;; *) exit 1 ;; esac; \
    CGO_ENABLED=1 GOOS=linux GOARCH="$TARGETARCH" go test -c -o /test/ftw-history-migrate.test .
FROM scratch AS test-export
COPY --from=test-build /test/ /
