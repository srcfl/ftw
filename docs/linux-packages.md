# Linux packages

New beta and stable releases provide `ftw-linux-arm64.tar.gz` for 64-bit
Raspberry Pi/Linux hosts and `ftw-linux-amd64.tar.gz` for x86-64 Linux hosts.
Each archive has a matching `.sha256` file. New releases do not build Windows
packages. Existing published assets remain available.

The archive contains Core, `ftw-backup`, web files, the pinned recovery
drivers, the compiled Energyplan bundle, license notices, an example config
and `deploy/ftw.service`. Keep these files together when changing versions.
Existing Linux download names remain aliases during the transition.

Download the archive and its checksum from the same explicit release. For
an arm64 host, verify and inspect the package with:

```sh
sha256sum -c ftw-linux-arm64.tar.gz.sha256
mkdir ftw-package
tar xzf ftw-linux-arm64.tar.gz -C ftw-package
cd ftw-package
./ftw -h
```

Use `amd64` instead on an x86-64 host. The systemd file is a service template:
it expects a service account and program, config and data directories to be
set up first. Extracting the archive does not install or start that service.
Keep live configuration and data outside the directory replaced on update.

Native builds leave the container updater disabled. Do not set
`FTW_SELFUPDATE_ENABLED=1`: it enables the existing container update path,
not a native package installer. Stop Core and take a verified backup before
replacing an existing installation; changing the program alone does not undo
a data-format change. Follow the [backup procedure](backup-and-restore.md) for recovery.

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

Beta packages contain their full beta version. Stable packages currently
build from the selected stable tag. The shared package builder does not yet
change stable promotion into a copy of beta binary artifacts.
