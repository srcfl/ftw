# Install and update FTW

**FTW 2.x and 3.x will receive no further updates. All new development and
releases use the new 0.x line. Move to it now to follow the latest fixes and
features. Do not install 3.x beta or use it as an intermediate upgrade.**

The new line starts at `v0.131.0-beta.1`. Older 0.x releases, up to 0.130.x,
belong to the retired line too. The lower version number is deliberate.
Choosing `beta` in an old installation does not move it to the new line.

You can switch now with a new setup and separate data. The guided migration
that preserves old settings, history, identity and goals is not ready yet.
If you need those data moved before switching, get help for your exact
installation; do not copy old databases into a new install.

Use this guide for both people and agents. [Svenska](setup-guide/update-sv.md).
The owner runs the host; FTW supplies the commands. Native systemd and Docker
on 64-bit Linux use the same release package.

## Choose your path

| What runs now | How to switch or update |
|---|---|
| Old Docker: 0.x up to 0.130.x, 1.x or 2.x, including Forty Two Watts images | [Switch from an older FTW](#coming-from-an-older-ftw). Use a new SD card/host, or separate Docker project and data. |
| Docker 3.x, including beta | The same switch. No intermediate release and no further 3.x updates. |
| The old ready-made FTW Raspberry Pi image | It runs old Docker. Use a second card with Raspberry Pi OS Lite 64-bit; keep the old card. |
| New 0.x native with launcher/release slots | [Update the native box](#update-a-native-box). Do not reinstall. |
| New 0.x Docker built from the release package | [Change `FTW_VERSION` and rebuild](#docker). `ftw update` does not apply. |
| Older direct native service without release slots | Identify its unit and data paths first. Use a separate host/card, or a checked migration for that site. The [native migration pilot](self-update.md#pilot-an-older-native-systemd-site) is not a general installer. |
| Home Assistant app on the old line | Run new FTW on another Linux host. Stop the old app and its automatic start/watchdog before new Core controls the equipment. The app does not provide new 0.x yet. |
| Custom build, manual binary, macOS or Windows | Identify the process and data first. The release install paths here require 64-bit Linux. |

### Find out what is running

Run these read-only checks on the FTW host, through SSH if needed:

```bash
sudo docker ps -a --format 'table {{.ID}}\t{{.Names}}\t{{.Image}}\t{{.Status}}'
sudo docker compose ls -a
systemctl list-units --type=service --all --no-pager | grep -Ei 'ftw|forty|docker'
systemctl list-unit-files --type=service --no-pager | grep -Ei 'ftw|forty'
```

Record the version and dashboard address too. A missing command or permission
error does not prove that no other install exists. Check other hosts, custom
start scripts and Home Assistant if used. A stored Docker image is not a
running Core; inspect containers and their restart rules. A service called
`ftw`, or `ftw status` answering from port 8080, does not identify its layout.

## Before you start

- A 64-bit Linux host: a Raspberry Pi 4 or 5 with Raspberry Pi OS Lite
  64-bit (Bookworm or Trixie), Debian 12 or 13, or an x86_64 machine.
- Running any older FTW already, perhaps from the Raspberry Pi image?
  Read [Coming from an older FTW](#coming-from-an-older-ftw) first.
- Port 8080 free, `curl`, and `sudo`.
- FTW needs no MQTT broker of its own. Ferroamp and CTEK equipment runs
  one itself, and FTW connects to it. Pixii and Heishamon publish to a
  broker you choose: use the one in Home Assistant, or
  [install Mosquitto](#an-mqtt-broker).
- FTW controls the battery and charger you configure. Keep the equipment's
  own app at hand to take control back.

## Install

Choose an exact published new 0.x beta from [Releases](https://github.com/srcfl/ftw/releases).
Check that it includes the Linux package for your architecture and its SHA-256
file. Do not use `releases/latest`: it still points to the retired 2.x line.
Replace `v0.X.Y-beta.N` below with that tag and use its own installer.
`--fresh-host` confirms that this host has no existing FTW site, including
stopped containers or data in a custom directory:

```bash
tag=v0.X.Y-beta.N
curl -fsSLO "https://raw.githubusercontent.com/srcfl/ftw/${tag}/scripts/install.sh"
bash install.sh --fresh-host --tag "${tag}"
```

It checks the package's SHA-256, creates the `ftw` user, puts releases in
`/opt/ftw`, data in `/var/lib/ftw`, the `ftw` command in `/usr/local/bin`
and starts the `ftw` service. Open `http://<host>:8080/setup` to set up the
site.

To run it in Docker instead, see [Docker](#docker).

## Coming from an older FTW

Move directly to the new line using one of the paths below. Keep the old
installation and its data for recovery; the new setup does not import them.
Before switching, save device addresses, goals and schedules, and make a
verified full backup using the tools your installed version supports. Keep
that backup off the host. Not every old version has the same backup UI.
Calendar support is gone: use loadpoint targets and ready-by schedules.

**Only one Core may control the equipment.** An old Core can keep controlling
when it fails to bind port 8080 and has no working web page. A single visible
dashboard is not proof that only one Core runs. Stop old Core before starting
new Core, and stop new Core before returning to the old install.

### Raspberry Pi: use a second SD card

1. Keep the old card intact. Write Raspberry Pi OS Lite **64-bit** to a **new**
   card, as in the [setup guide](setup-guide/README.md). Choose a login name
   other than `ftw`; the installer creates that service account.
2. Shut the Pi down, swap cards, start it and follow [Install](#install).
   Set up the site again at `http://<host>:8080/setup`.
3. If the old card supplied MQTT, provide a broker for the new setup before
   connecting the devices. The old card's broker does not move with FTW.
4. Follow [Verify the switch](#verify-the-switch).

To return, shut down and put the old card back. Also stop any new FTW you ran
on another machine. Data collected by the new setup stays on the new card.

### Same Linux host: use a separate Docker project

1. Identify the old project's directory, Compose files, overrides, name and
   data mounts. Common paths are `/opt/ftw` on the old Pi image, `~/ftw` and
   `~/forty-two-watts`; use the path found on this host. Save its Compose files
   and `.env` with the backup so recovery uses the same old version.
2. Check whether that project also supplies Mosquitto or another needed
   service. Arrange continued MQTT before stopping the whole project.
3. Stop the old project with its actual files and project name. For a standard
   project, run `docker compose down` from its directory (with `sudo` if
   required). Do not add `-v`, delete data, or stop Docker as a whole.
4. Check systemd, timers and custom scripts that could recreate or start old
   Core or its updater at boot. Disable only the confirmed old start path.
   Docker's container restart policy is only one possible start path.
5. Follow [Docker](#docker) in a new directory with separate empty data,
   normally `~/ftw-local`. If that directory already exists, inspect it first;
   do not overwrite an earlier test or attach the old data directory.
6. Set up the site and follow [Verify the switch](#verify-the-switch).

To return, stop the new project first, then start the saved old project with
its original files and version pin. Restore only the start rules you disabled.
Each installation keeps its own data.

The native installer refuses an existing site. Do not remove its checks or
old files to make `--fresh-host` pass. Home Assistant users need another Linux
host for the new line; the old app is not an intermediate upgrade. Stop its
Core, Start on boot and Watchdog before the new site takes control. Keep a
Home Assistant backup and the old app's data for recovery. The MQTT integration
with a separate FTW host still works; see [Home Assistant](ha-integration.md).

## Verify the switch

Check the expected running version, healthy devices, advancing measurements,
current plan and history without write failures. Confirm that old Core and
its updater are stopped and cannot start again automatically. Check all hosts
that could control the equipment; port 8080 alone is not a check for this.

Plan a reboot when the site can tolerate the interruption, then repeat these
checks. If you have not checked after reboot, say so. Service health does not
prove physical charging or battery response; verify those on the equipment.

## Everyday commands

These commands are for the new **native** installation with release slots.

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

## Update a native box

Run the commands on the box. From another computer, connect with
`ssh <user>@<box-address>` first. The native web UI shows the version and
update notice; it has no Update button.

```bash
ftw status                    # check the running version, saved channel and health
ftw update                    # download, check, stage and restart Core
ftw status                    # confirm the running version and health afterward
journalctl -u ftw --since "10 minutes ago" --no-pager
```

`ftw update` follows the saved channel. Use `ftw update --channel beta` if
you mean to switch this site to beta. Publishing a beta does not update a
box by itself. Do not rerun the installer for a routine Core update.

The command shows each step and waits until Core is ready. Check that its
`Now running` line and the final `ftw status` name the expected version,
that the devices are healthy, and that history has no write failures. Then
open the local dashboard and check that device readings keep changing and
the plan is current. A healthy service alone does not prove that a car is
charging or a battery follows a command.

`Previous:` names the release available through `ftw rollback`. If the
update fails, follow [When something goes wrong](#when-something-goes-wrong)
before retrying. Updates to the launcher, command or service use the separate
[refresh step](#launcher-and-command-updates) only when release notes ask for it.

## Drivers

Drivers come with the release: `ftw update` and `ftw rollback` move them
with Core, and `ftw status` lists the version each device runs. To try
another signed version of one driver, open the device's Versions under
Settings › Devices. The release's own copy comes first and is the way back.
"Check for new versions" reads the signed channels, beta included, and
"What changed" opens a version's history. A newer version you pick runs
until a release brings a newer one. An older version you pick stays across
updates until you change it. See [Driver source and signed releases](device-repository.md).

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
the installer from that release. Replace the placeholder with its exact tag:

```bash
tag=v0.X.Y-beta.N
curl -fsSLO "https://raw.githubusercontent.com/srcfl/ftw/${tag}/scripts/install.sh"
bash install.sh --refresh --tag "${tag}"
```

## Docker

Docker runs FTW as well as the native install does. Compose builds a small
local image from the same checksummed release package; nothing is compiled.
You need Docker Engine with Compose on Linux. Docker Desktop on macOS or
Windows has a different network setup; this Linux host-network recipe does
not establish LAN device access there. Choose an exact published new 0.x tag
as described under [Install](#install). Replace the placeholder below. These
steps create a new site; use an empty directory, not an existing install.

```bash
tag=v0.X.Y-beta.N
mkdir -p ~/ftw-local && cd ~/ftw-local
base="https://raw.githubusercontent.com/srcfl/ftw/${tag}/deploy/docker"
curl -fsSLO "${base}/compose.yaml" -O "${base}/Dockerfile"
mkdir -p data && sudo chown 100:101 data
printf 'FTW_VERSION=%s\n' "$tag" > .env
docker compose up -d --build
```

The two files stay the same between releases; `FTW_VERSION` chooses one. Open
`http://<host>:8080/setup`. Data lives in `~/ftw-local/data`. If Docker
answers "permission denied", put `sudo` in front of `docker`.

```bash
docker compose ps                            # running and healthy
docker compose exec ftw ftw status           # the same status as a native install
docker compose exec ftw ftw support --output /app/data/support.zip   # lands in ~/ftw-local/data
docker compose logs --tail 100               # logs
docker compose restart                       # restart
```

To update, set the new version and rebuild. To go back, set the previous
version. If its image is still on the host, Compose can reuse it. Going back
works while both releases read the same data, as on a native install.

```bash
# Edit only FTW_VERSION in .env to the chosen exact release; keep other entries.
docker compose up -d --build
docker compose ps
docker compose exec ftw ftw status
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
sudo mv data data.before-restore-$(date +%F-%H%M)
sudo tar -xzf /media/usb/ftw/ftw-data-<date>.tar.gz
docker compose start
```

To remove it, run `docker compose down`, remove the `ftw-local` images and
delete `~/ftw-local`.

## For an agent that runs the host

Give this section to an agent that operates FTW for you. FTW controls real
equipment, so the agent uses the same commands a person would and leaves
decisions about the site to you.

- Identify the host, running version, process/service or Compose project,
  data paths, restart rules and target release before changing anything. Use
  [Choose your path](#choose-your-path), including for older 0.x versions.
- Distinguish an update within the new line, a new setup with separate data,
  and a migration preserving data. Do not recommend or publish 2.x or 3.x,
  even if an old UI or a `latest` alias offers one.
- A diagnosis is read-only. Once the owner has authorized a specific update,
  carry it through without asking again for the same action.
- For a switch, confirm an off-host backup, preserve MQTT and stop the old
  control/start paths. Report the checks in [Verify the switch](#verify-the-switch),
  including anything not tested, before calling the switch complete.
- Read the state with `ftw status`. Exit 0 means Core answers and is
  healthy; exit 1 means it is not, and stderr says why. Exit 2 is a usage
  error. The commands never prompt.
- Run `ftw update` only when the owner asked for it or set a schedule for
  it. Exit 0 means the box is current or updated. Exit 1 means a step
  failed; report `ftw status` and `journalctl -u ftw -n 200`, and do not
  repeat the update.
- Use `ftw update --retry`, rollback or restore only within the owner's
  authorized recovery plan. If that authority is missing, ask before changing
  the site. Do not retry a failed update blindly.
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
  `GET /api/support/dump` returns the support file as a zip.
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
