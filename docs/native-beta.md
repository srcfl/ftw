# Try the native beta

This guide is for people who test the native 0.x beta on their own
machine. You run the host; FTW gives you a few commands for it
([ADR 0007](adr/0007-self-updating-binary.md), decisions 10–14). Report
what you find in an issue that names the beta, for example
`v0.135.0-beta.1`.

## Before you start

- A spare 64-bit Linux host with systemd: a Raspberry Pi 4 or 5 with
  Raspberry Pi OS Bookworm 64-bit, Debian 12, or an x86_64 machine.
- No FTW on it already. An existing Docker 1.x, 2.x or 3.x site waits for
  the guided migration; do not install over it.
- Port 8080 free, `curl`, and `sudo`.
- Equipment that talks MQTT, such as Ferroamp, needs a broker on the host:
  `sudo apt install mosquitto`.
- FTW controls the battery and charger you configure. Keep the equipment's
  own app at hand to take control back.

## Install

Use the installer from the same tag you install:

```bash
tag=v0.135.0-beta.1
curl -fsSLO "https://raw.githubusercontent.com/srcfl/ftw/${tag}/scripts/install.sh"
bash install.sh --fresh-host --tag "${tag}"
```

It checks the package's SHA-256, creates the `ftw` user, puts releases in
`/opt/ftw`, data in `/var/lib/ftw`, the `ftw` command in `/usr/local/bin`
and starts the `ftw` service. Open `http://<host>:8080/setup` to set up the
site.

## Everyday commands

```bash
ftw status                                   # version, releases, last update, disk, health
ftw update                                   # install the next release on the saved channel
ftw rollback                                 # return to the previous release
ftw backup --output-dir /media/usb/ftw       # make a verified backup and copy it off the disk
ftw support                                  # write the redacted support file for a report
journalctl -u ftw -n 100                     # logs
sudo systemctl restart ftw                   # restart
```

`ftw update` asks nothing, so a timer or an agent can run it. It exits 0
when the box is current or the update succeeded, and 1 when a step failed.

## When something goes wrong

**A new release does not start or keeps stopping.** FTW goes back by
itself. A release that does not become ready returns to the previous one at
once. A release that became ready but stops without a clean shutdown three
times within ten minutes, during its first hour, does too. `ftw status`
then shows `Failed:`, and `ftw update` skips that release until a newer one
is published. `ftw update --retry` tries it again.

**Core does not start at all**, and `ftw` cannot reach it:

```bash
sudo -u ftw /opt/ftw/ftw-launcher -root /opt/ftw rollback
sudo systemctl restart ftw
```

The next start runs the previous release once and keeps it if it becomes
ready. This works only when that release reads the same data.

**The data needs to come back from a full backup.** Restore is offline:

```bash
sudo systemctl stop ftw
tag=$(sudo -u ftw /opt/ftw/ftw-launcher -root /opt/ftw status | sed 's/.*"current":"\([^"]*\)".*/\1/')
sudo install -o ftw -g ftw -m 0600 /media/usb/ftw/ftw-full-backup-<time>.ftwbak /var/tmp/ftw-restore.ftwbak
sudo -u ftw /opt/ftw/releases/${tag}/ftw-backup restore -archive /var/tmp/ftw-restore.ftwbak -data /var/lib/ftw -yes
sudo systemctl start ftw
ftw status
```

`restore` keeps the replaced data, including earlier backups made on the
box, in `/var/lib/ftw/.ftw-pre-restore-<time>` and prints its path. To undo it, stop the service and run
`ftw-backup revert -data /var/lib/ftw -safety <that path> -yes` the same way.

## Launcher and command updates

`ftw update` replaces Core, not the launcher, the `ftw` command or the
service definition. When a release notes changes to them, refresh them with
the installer from that release:

```bash
tag=v0.135.0-beta.2
curl -fsSLO "https://raw.githubusercontent.com/srcfl/ftw/${tag}/scripts/install.sh"
bash install.sh --refresh --tag "${tag}"
```

## Report

Open an issue at <https://github.com/srcfl/ftw/issues> that names the beta
and says what you did, what happened and what you expected. `ftw support`
writes a zip with logs and settings. Core removes secrets from it, but it
still describes your site, so share it privately if you prefer.

## Remove FTW

```bash
sudo systemctl disable --now ftw
sudo rm -rf /opt/ftw /etc/systemd/system/ftw.service /usr/local/bin/ftw
sudo systemctl daemon-reload
```

Data stays in `/var/lib/ftw` until you remove it with
`sudo rm -rf /var/lib/ftw` and `sudo userdel ftw`.
