# Try the 0.x beta

This guide is for people who test the 0.x beta on their own machine. You
run the host; FTW gives you a few commands for it
([ADR 0007](adr/0007-self-updating-binary.md), decisions 10–14). Install it
natively with systemd, or run it in Docker. Both run the same release
package. Report what you find in an issue that names the beta, for example
`v0.135.2-beta.1`.

## Before you start

- A spare 64-bit Linux host: a Raspberry Pi 4 or 5 with Raspberry Pi OS
  Bookworm 64-bit, Debian 12, or an x86_64 machine.
- No FTW on it already. An existing Docker 1.x, 2.x or 3.x site waits for
  the guided migration; do not install over it.
- Port 8080 free, `curl`, and `sudo`.
- FTW needs no MQTT broker of its own. Ferroamp and CTEK equipment runs
  one itself, and FTW connects to it. Pixii and Heishamon publish to a
  broker you choose: use the one in Home Assistant, or
  [install Mosquitto](#an-mqtt-broker).
- FTW controls the battery and charger you configure. Keep the equipment's
  own app at hand to take control back.

## Install

Use the installer from the same tag you install:

```bash
tag=v0.135.2-beta.1
curl -fsSLO "https://raw.githubusercontent.com/srcfl/ftw/${tag}/scripts/install.sh"
bash install.sh --fresh-host --tag "${tag}"
```

It checks the package's SHA-256, creates the `ftw` user, puts releases in
`/opt/ftw`, data in `/var/lib/ftw`, the `ftw` command in `/usr/local/bin`
and starts the `ftw` service. Open `http://<host>:8080/setup` to set up the
site.

To run it in Docker instead, see [Docker](#docker).

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

## An MQTT broker

Only Pixii and Heishamon need one. Mosquitto is about 1 MB and ships with
Debian and Raspberry Pi OS. It accepts only local connections until it is
told to listen on the network:

```bash
sudo apt install mosquitto
printf 'listener 1883\nallow_anonymous true\n' | sudo tee /etc/mosquitto/conf.d/lan.conf
sudo systemctl restart mosquitto
```

Point the device and the FTW driver at `<host>:1883`. Anonymous access
suits a trusted home network; otherwise add a `password_file`.

## Launcher and command updates

`ftw update` replaces Core, not the launcher, the `ftw` command or the
service definition. When a release notes changes to them, refresh them with
the installer from that release:

```bash
tag=v0.135.3-beta.1
curl -fsSLO "https://raw.githubusercontent.com/srcfl/ftw/${tag}/scripts/install.sh"
bash install.sh --refresh --tag "${tag}"
```

## Docker

Docker runs FTW as well as the native install does. Compose builds a small
local image from the same checksummed release package; nothing is compiled.
You need Docker Engine with Compose on Linux. Docker Desktop on macOS or
Windows keeps the network inside its VM and cannot reach the equipment.

```bash
mkdir -p ~/ftw && cd ~/ftw
base=https://raw.githubusercontent.com/srcfl/ftw/master/deploy/docker
curl -fsSLO "${base}/compose.yaml" -O "${base}/Dockerfile"
mkdir -p data && sudo chown 100:101 data
echo "FTW_VERSION=v0.135.2-beta.1" > .env
docker compose up -d --build
```

The two files stay the same between releases; `FTW_VERSION` chooses one. Open
`http://<host>:8080/setup`. Data lives in `~/ftw/data`.

```bash
docker compose ps                            # running and healthy
docker compose exec ftw ftw status           # the same status as a native install
docker compose exec ftw ftw support --output /app/data/support.zip   # lands in ~/ftw/data
docker compose logs --tail 100               # logs
docker compose restart                       # restart
```

To update, set the new version and rebuild. To go back, set the previous
version; its image is still on the host, so nothing is fetched. Going back
works while both releases read the same data, as on a native install.

```bash
sed -i 's/^FTW_VERSION=.*/FTW_VERSION=v0.135.3-beta.1/' .env
docker compose up -d --build
```

Docker has no automatic fallback. A release that does not start keeps
restarting until you set the previous version. Each version stays as an
image: `docker image ls ftw-local` lists them, and
`docker image rm ftw-local:<version>` frees the space of one you no longer
need.

Back up and restore the whole data directory while FTW is stopped:

```bash
docker compose stop
sudo tar -czf /media/usb/ftw/ftw-data-$(date +%F).tar.gz data
docker compose start

# restore
docker compose stop
sudo mv data data.before-restore
sudo tar -xzf /media/usb/ftw/ftw-data-<date>.tar.gz
docker compose start
```

To remove it, run `docker compose down`, remove the `ftw-local` images and
delete `~/ftw`.

## For an agent that runs the host

Give this section to an agent that operates FTW for you. FTW controls real
equipment, so the agent uses the same commands a person would and leaves
decisions about the site to you.

- Read the state with `ftw status`. Exit 0 means Core answers and is
  healthy; exit 1 means it is not, and stderr says why. Exit 2 is a usage
  error. The commands never prompt.
- Run `ftw update` only when the owner asked for it or set a schedule for
  it. Exit 0 means the box is current or updated. Exit 1 means a step
  failed; report `ftw status` and `journalctl -u ftw -n 200`, and do not
  repeat the update.
- Ask the owner before `ftw update --retry`, `ftw rollback`, a restore or
  the offline rollback in [When something goes wrong](#when-something-goes-wrong).
- Before an update the owner cares about, run
  `ftw backup --output-dir <a directory on another disk>`.
- Change settings in the web UI or the API. Do not edit files in
  `/var/lib/ftw` while the service runs. Never change or delete anything
  under `/opt/ftw`, never run `install.sh --fresh-host` on an installed box,
  and never remove `/var/lib/ftw`.
- Do not command the battery, inverter or charger through other apps or
  APIs while FTW runs. Stop FTW first with `sudo systemctl stop ftw`; a
  clean stop hands the equipment back to its own mode.
- For a report, write `ftw support` and open an issue that names the beta.
  The support file describes the site; share it only as the owner decides.
- The same operations are HTTP calls on `http://127.0.0.1:8080`; calls from
  the host itself need no token. `GET /api/health`, `GET /api/status` and
  `GET /api/version/check` read the state. `POST /api/version/update` with
  `{}` updates, or with `{"retry": true}` retries a failed release;
  `GET /api/version/update/status` follows it. `POST /api/version/binary-rollback`
  rolls back, `POST /api/backups` makes a backup and
  `POST /api/support/dump` returns the support file as a zip.
- In Docker, run `ftw` inside the container:
  `docker compose exec ftw ftw status`. `ftw update` and `ftw rollback` do
  not apply there. Change `FTW_VERSION` in `.env` and run
  `docker compose up -d --build` instead, and check
  `docker compose ps` for `healthy`.

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
