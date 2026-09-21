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

Use `amd64` instead on an x86-64 host. Extracting the archive does not install
or start the systemd service. Keep configuration and data outside the
directory replaced on update.

## First native installation

These steps target a fresh Debian 12 or Raspberry Pi OS Bookworm host with
systemd and a 64-bit OS. The package needs no Go toolchain or Docker engine.
Run `uname -m`: `aarch64` needs arm64; `x86_64` needs amd64. A 32-bit OS cannot
run these packages.

Install the download tools and the OS certificate store first. Core uses that
store for HTTPS services such as prices and driver downloads:

```sh
sudo apt-get update
sudo apt-get install -y ca-certificates curl
```

If FTW already runs on the host, stop here. A Compose, Home Assistant or older
native installation needs a verified backup and a planned handover. Do not
start a second Core against the same equipment. The older native template
used `/etc/ftw/config.yaml`; replacing its unit alone would start setup with a
new config. Keep its unit until the configuration, identity and other files
beside that config have joined the full backup and passed a restore check.

After verifying and extracting the package as above, run from `ftw-package`:

```sh
test ! -e /opt/ftw && test ! -e /var/lib/ftw
```

Both paths must be absent. Also check that port 8080 is free and no existing
FTW service is installed. Then create the account and copy the whole package:

```sh
sudo useradd --system --user-group --home-dir /var/lib/ftw --no-create-home ftw
sudo install -d -m 0755 /opt/ftw
sudo cp -a . /opt/ftw/
sudo chown -R root:root /opt/ftw
sudo install -m 0644 deploy/ftw.service /etc/systemd/system/ftw.service
sudo systemctl daemon-reload
sudo systemctl enable --now ftw
```

Do not copy `config.example.yaml` into the data directory. With no config,
Core opens setup at `http://<host>:8080/setup`. Finish setup in the browser.
Systemd creates `/var/lib/ftw` for the service account; Core keeps config,
identity, drivers and databases there so a full backup includes them together.
The example config remains a reference in the program directory.

Check the version at the bottom of the dashboard and the running process:

```sh
systemctl is-active ftw
curl --fail --silent --show-error http://127.0.0.1:8080/api/status
curl --fail --silent --show-error http://127.0.0.1:8080/api/health
sudo journalctl -u ftw -n 30 --no-pager
```

`/api/status` must report the selected release. `/api/health` becomes available
after setup; check its body for storage and device errors. A running service
does not prove that a meter is fresh or that a charger accepts commands.

## Manual update and recovery

Download, verify and extract the next explicit release into a new directory
before stopping FTW. Read that release's data-compatibility notes. Stop Core,
create and verify a full backup with `ftw-backup`, and copy it off the host.
The [backup guide](backup-and-restore.md#native-helper) gives the commands.

Keep the old `/opt/ftw` directory under a versioned name. Put the complete new
package at `/opt/ftw`, owned by root, then start the existing service and check
the version, health, saved goals and live measurements. Keep `/var/lib/ftw`
in place; do not copy example config over it or replace only the executable.

If startup fails, stop the service and inspect the logs. Before restoring the
old program, check whether the new version changed the data format. When it
did, restore the verified pre-update data with `ftw-backup` as well. Do not
start an older program against an unsupported data format. The native path
has no automatic rollback; retain both packages and the backup until the new
version has passed these checks.

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
