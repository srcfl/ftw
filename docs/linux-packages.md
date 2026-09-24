# Linux packages

Native 0.x beta and stable releases provide `ftw-linux-arm64.tar.gz` for 64-bit
Raspberry Pi/Linux hosts and `ftw-linux-amd64.tar.gz` for x86-64 Linux hosts.
Each archive has a matching `.sha256` file. Existing
`forty-two-watts-linux-<arch>` download names remain aliases during the
transition. New releases do not build Windows packages. Existing published
assets remain available.

Install a package with [`scripts/install.sh`](../scripts/install.sh) from the
same published tag, as [native-beta.md](native-beta.md) shows. It verifies the
package, puts releases in slots under `/opt/ftw`, installs
`deploy/ftw-native.service` as the `ftw` service and puts the `ftw` command on
`PATH`. After that, `ftw update` downloads the next package into a slot and
the launcher switches to it ([ADR 0007](adr/0007-self-updating-binary.md)).
The web UI only shows the version. `install.sh --refresh` replaces the
launcher, the command and the unit when a release asks for it. Do not copy a
package into `/opt/ftw` by hand: the launcher expects the slot layout.

The installer is not the guided migration for an existing Docker, Home
Assistant or earlier native box. Those boxes stay on their current version
until the guided 0.x migration has been tested and published.
[`deploy/docker`](../deploy/docker) builds a local image from the same
package.

## Contents

Each archive holds one release: Core (`ftw`), `ftw-launcher`, `ftw-backup`,
`ftw-cli` (installed as the `ftw` command), web files, the pinned recovery
drivers, the compiled Energyplan bundle, license notices, an example config,
`state-schema.json`, `release-version.json` with the version, architecture
and state schema, `deploy/ftw-native.service` and the older direct-layout
`deploy/ftw.service`. A release slot holds the whole extracted package; keep
these files together.

Each package includes only the Energyplan executable for its Linux
architecture. Its manifest lists that executable and the shared schemas,
licenses and notices, with the original version, source commit and file
checksums. Other Linux architectures and macOS executables are excluded.
The full pinned bundle in the source checkout stays unchanged.

To verify and inspect a package by hand, download the archive and its
checksum from the same explicit release. For an arm64 host:

```sh
sha256sum -c ftw-linux-arm64.tar.gz.sha256
mkdir ftw-package
tar xzf ftw-linux-arm64.tar.gz -C ftw-package
cd ftw-package
./ftw -h
```

Use `amd64` instead on an x86-64 host. Extracting the archive does not install
or start anything.

## Build the same packages locally

From a clean checkout with Go, Python 3 and the driver-fetch tools installed:

```sh
make release VERSION="$(git describe --tags --exact-match)"
```

This builds both Linux architectures and writes the archives and checksums
under `release/`. `make release-linux-arm64` or `make release-linux-amd64`
builds one target. The package builder checks the binary architecture and
requires every pinned driver and runtime directory. It normalizes archive
timestamps and ownership so unchanged inputs produce the same package bytes.

Beta packages contain their full beta version. For stable,
[`native-release.yml`](../.github/workflows/native-release.yml) checks the
named beta's release receipt and package checksums, then rebuilds from that
beta's commit with the stable version. Stable promotion does not copy the beta
archives.
