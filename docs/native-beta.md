# Install and update FTW

New to FTW? [Install FTW](install.md) is the short path. This page holds
every path and command, for people and agents.

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

**Still using the SD card that runs old FTW? Do not run the native install
commands on that card.** For a Raspberry Pi, use the second-card path below.
A failed native install is a stop point, not a reason to try Docker next.

Choose **one** path. These are alternatives, not steps to run in order:

- **Old FTW on a Pi:** [use a second SD card](#raspberry-pi-use-a-second-sd-card).
  Keep the old card. This is the recommended path if you need step-by-step help.
- **Already on new 0.x:** [update native](#update-a-native-box) or
  [update new Docker](#update-new-docker). Do not run a new install.
- **An empty Linux host with no FTW:** [install native](#install).
  [Docker](#docker) is an alternative for people who manage Docker themselves.

<details>
<summary>Other install types and the full path table</summary>

| What runs now | How to switch or update |
|---|---|
| Old Docker: 0.x up to 0.130.x, 1.x or 2.x, including `ghcr.io/frahlg/forty-two-watts` images | [Switch from an older FTW](#coming-from-an-older-ftw). Use a new SD card/host, or separate Docker project and data. |
| Docker 3.x, including beta | The same switch. No intermediate release and no further 3.x updates. |
| The old ready-made FTW Raspberry Pi image | It runs old Docker. Use a second card with Raspberry Pi OS Lite 64-bit; keep the old card. |
| New 0.x native with launcher/release slots | [Update the native box](#update-a-native-box). Do not reinstall. |
| New 0.x Docker built from the release package | [Update new Docker](#update-new-docker) with **Update FTW** or `ftw update`, as a native box does. A project made before Docker updated itself first takes the new `Dockerfile` and `compose.yaml`. |
| The **FTW (beta)** Home Assistant app, version 0.x (once named **FTW (0.x beta)**) | Update and back up through Home Assistant. See [the app guide](home-assistant-app.md). |
| Older direct native service without release slots | Identify its unit and data paths first. Use a separate host/card, or a checked migration for that site. The [native migration pilot](self-update.md#pilot-an-older-native-systemd-site) is not a general installer. |
| Home Assistant app on the old line (version 3.x) | Install the **FTW (beta)** app, version 0.x ([FTW on Home Assistant](home-assistant-app.md)), or run new FTW on another Linux host. Either way, stop the old app and its automatic start/watchdog before new Core controls the equipment. |
| Custom build, manual binary, macOS or Windows | Identify the process and data first. The release install paths here require 64-bit Linux. |

</details>

### If you already got an error

**Stop at the first error. Do not paste the next command block or change
install methods.** Save the command and its output, and ask in
[Discord](https://discord.gg/UK2ygPBu8N) if the next step is unclear.

| Message or symptom | What to do |
|---|---|
| `Existing FTW installation found` or `An FTW service already exists` | This host/card holds another FTW install, such as Docker, 3.x or an older native one. Keep those files. For a Pi, shut down and use the prepared new card. Some published installers say “try 0.x beside it”; this does not mean Docker is the next step. |
| `FTW was removed from this computer, but its data is still in /var/lib/ftw` or `its ftw account is still here` | A native install was removed and left its data or account. Back up the data with the command the message gives; the copy holds the box's keys, so keep it private. Then run its clean-up commands and the install line again. See [Remove FTW](#remove-ftw). |
| `FTW is already installed on this computer` | Not an error: the line changed nothing. Open the address it prints. Use `ftw update` for a newer release. |
| `curl: (22)` / `404` | The download failed. Check the network and the line, or the tag if you gave one. Do not run an old `install.sh`, reuse old Compose files or continue to build. |
| `ftw-local` already exists | An earlier attempt has left files. Stop and inspect that project before retrying; do not delete its data or overwrite its `.env`. |
| `requires buildx plugin` | Docker is missing a build tool. Follow the Docker prerequisite below before building. |
| `checking context: no permission to read .../data/applink.json` | Private FTW data is entering the build. Keep its permissions. The `.dockerignore` step under [Update new Docker](#update-new-docker) fixes the supplied recipe; do not try `chmod` or a build as root to read those files. |
| New `ftw-local` container keeps `Restarting` while old Core is `Up` | Stop the **new test container** using its confirmed name (`sudo docker stop --time 60 <new-container-name>`). Keep both data sets. Diagnose before starting it again; do not stop a shared MQTT broker. |

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

- A 64-bit Linux host: a Raspberry Pi 3 or newer with Raspberry Pi OS Lite
  64-bit (Bookworm or Trixie), Debian 12 or 13, or an x86_64 machine.
- Running any older FTW already, perhaps from the Raspberry Pi image?
  Read [Coming from an older FTW](#coming-from-an-older-ftw) first.
- Port 8080 free and about 250 MB of free space.
- The programs `curl`, `tar`, `gzip`, `sha256sum`, `grep`, `awk` and
  `mktemp`, and `sudo` unless you install as root. Raspberry Pi OS, Debian
  and Ubuntu come with them. If one is missing, the installer names its
  package and the command that adds it, for example
  `sudo apt update && sudo apt install -y tar`.
- FTW needs no MQTT broker of its own. Ferroamp and CTEK equipment runs
  one itself, and FTW connects to it. Pixii and Heishamon publish to a
  broker you choose: use the one in Home Assistant, or
  [install Mosquitto](#an-mqtt-broker).
- FTW controls the battery and charger you configure. Keep the equipment's
  own app at hand to take control back.

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

1. **On the old card:** save the settings and off-host backup described above.
   Do not run an installer here. Keep this card intact.
2. **On your normal computer:** write Raspberry Pi OS Lite **64-bit** to a
   **second** card. The [Pi setup guide](setup-guide/en.md) shows the Imager
   steps. Enable SSH and choose a login name other than `ftw`, such as `pi`.
3. **At the Pi:** shut it down, unplug power, remove the old card and insert
   the new one. Reconnect power. The physical card swap must happen before
   you run any install command.
4. **Back on your computer:** open a new SSH connection using the login you
   chose for the new card. The IP address may have changed; check the router.
   If you have not swapped cards, stop here.
5. **Before setting up devices:** stop old FTW on any other host that could
   control the same equipment. If the old card supplied MQTT, arrange a broker
   for the new setup. The old card's broker does not move with FTW.
6. **In SSH on the new card:** follow [Install](#install), then open
   `http://<host>:8080/setup` and set up the site again.
7. Follow [Verify the switch](#verify-the-switch).

To return, shut down and put the old card back. Also stop any new FTW you ran
on another machine. Data collected by the new setup stays on the new card.

### Same Linux host: use a separate Docker project

**Advanced alternative to changing cards.** Choose it only if you can identify
and stop the old Core and updater, handle their start rules, and preserve MQTT.
Otherwise use a second card or ask for help. Do not start this path just
because the native installer refused the old card.

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
an old site's files to make `--fresh-host` pass. Only the data a removed
native install leaves behind may go, after a backup; see [Remove FTW](#remove-ftw). Home Assistant users install the
**FTW (beta)** app, version 0.x; the old app is not an intermediate upgrade. Stop its
Core, Start on boot and Watchdog before the new site takes control. Keep a
Home Assistant backup and the old app's data for recovery. The MQTT integration
with a separate FTW host still works; see [Home Assistant](ha-integration.md).

## Install

**Only on the new card or an empty host.** If old FTW ran on this card,
return to [the second-card steps](#raspberry-pi-use-a-second-sd-card) first.
This section does not update or migrate an old installation.

Run this line in SSH on the new host:

```bash
curl -fsSL https://ftw.energy/install.sh | bash -s -- --fresh-host
```

`https://ftw.energy/install.sh` redirects to
[`scripts/install.sh`](../scripts/install.sh) on `master`. Without `--tag` it
installs the newest stable release, or the newest beta while there is no
stable, and says which. Add `--beta` to take the newest beta when it is newer
than the newest stable, or `--tag v0.X.Y` for one exact published release. Do
not pick a release from GitHub `releases/latest`; it is not the newest FTW.
`--fresh-host` confirms that this host has no existing FTW site, including
stopped containers or data in a custom directory. A download that stops
partway runs nothing.

It checks the package's SHA-256, creates the `ftw` user, puts releases in
`/opt/ftw`, data in `/var/lib/ftw`, the `ftw` command in `/usr/local/bin`
and starts the `ftw` service. At the end it prints the addresses to open,
`http://<address>:8080/setup`; set up the site there. If the install stops
partway, run the same line with `--resume` in place of `--fresh-host`. It
finishes the same release and keeps any data already saved. On a box it set
up, the line changes nothing: it says FTW is already installed, prints the
addresses and points to `ftw status` and `ftw update`. It exits 0 while FTW
runs or starts and 1 when it does not. It ignores `--tag` and `--beta` there;
`ftw update` moves the box to a newer release.

If it reports an error, stop and use [the error table](#if-you-already-got-an-error).
Do not continue to the Docker instructions.

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
You can also open **More → Version → Update FTW** on the box's web UI. It
uses the same Core API and launcher, follows progress across restart, and
shows the version that actually runs afterward. Keep the box powered;
closing or reloading the page does not cancel or repeat the update.

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
(
  set -eu
  tag=v0.X.Y-beta.N
  if [[ ! "$tag" =~ ^v0\.([0-9]+)\.[0-9]+(-beta\.[0-9]+)?$ ]] || (( 10#${BASH_REMATCH[1]} < 131 )); then
    echo "STOP: copy an exact published new 0.x tag from Releases." >&2
    exit 1
  fi
  installer=$(mktemp)
  trap 'rm -f "$installer"' EXIT
  curl -fSL "https://raw.githubusercontent.com/srcfl/ftw/${tag}/scripts/install.sh" -o "$installer"
  bash "$installer" --refresh --tag "$tag"
)
```

## Docker

This is an alternative to native installation, not its next step. It creates
a new site. If old FTW is on this host, complete [the same-host switch steps](#same-linux-host-use-a-separate-docker-project)
first, including stopping old Core and its updater. Do not connect both to the
same equipment.

You need Docker Engine, Compose **and Buildx** on 64-bit Linux. The
[Docker Debian instructions](https://docs.docker.com/engine/install/debian/)
include both plugins. Do not reinstall Docker on a running site without
checking its existing services. Docker Desktop has a different network setup;
this Linux host-network recipe does not establish LAN device access there.

Copy an exact published new 0.x tag from [Releases](https://github.com/srcfl/ftw/releases).
Replace only `v0.X.Y-beta.N` and paste the **whole block, including `(` and `)`**.
It downloads both files to a temporary directory before creating the project,
so a failed download leaves no project behind. Correct the tag or connection
and paste the whole block again. It stops at the first error and refuses an
existing `~/ftw-local` directory, including an earlier test. If that happens, inspect it before doing anything
else; do not delete it to make the command pass.

```bash
(
  set -eu
  tag=v0.X.Y-beta.N
  if [[ ! "$tag" =~ ^v0\.([0-9]+)\.[0-9]+(-beta\.[0-9]+)?$ ]] || (( 10#${BASH_REMATCH[1]} < 131 )); then
    echo "STOP: copy an exact published new 0.x tag from Releases." >&2
    exit 1
  fi
  sudo docker compose version
  sudo docker buildx version
  sudo docker info >/dev/null
  install_dir="$HOME/ftw-local"
  if [ -e "$install_dir" ] || [ -L "$install_dir" ]; then
    echo "STOP: $install_dir already exists. Inspect it before retrying." >&2
    exit 1
  fi
  downloads=$(mktemp -d)
  trap 'rm -rf "$downloads"' EXIT
  base="https://raw.githubusercontent.com/srcfl/ftw/${tag}/deploy/docker"
  curl -fSL "${base}/compose.yaml" -o "$downloads/compose.yaml"
  curl -fSL "${base}/Dockerfile" -o "$downloads/Dockerfile"
  mkdir "$install_dir"
  cp "$downloads/compose.yaml" "$downloads/Dockerfile" "$install_dir/"
  cd "$install_dir"
  printf '*\n!Dockerfile\n!.dockerignore\n' > .dockerignore
  printf 'FTW_VERSION=%s\n' "$tag" > .env
  mkdir data
  sudo chown 100:101 data
  sudo docker compose up -d --build
)
```

The block writes `.dockerignore` locally because earlier release tags do not
include it. This keeps private data and `.env` out of every build. Compose
builds a local image from the checksummed release package; it compiles nothing.
Open `http://<host>:8080/setup`. Data lives in `~/ftw-local/data`.

Core runs through the same launcher as a native install. Its releases live in
the `ftw-local_releases` volume, apart from the data, and Docker's restart
policy starts Core again after an update, as systemd does on a native box.

After success, use `cd ~/ftw-local` before the commands below. Use `sudo docker`
if your login cannot access Docker without it.

```bash
docker compose ps                            # running and healthy
docker compose exec ftw ftw status           # the same status as a native install
docker compose run --rm --entrypoint ftw ftw update     # install the next release
docker compose run --rm --entrypoint ftw ftw rollback   # return to the previous release
docker compose exec ftw ftw support --output /app/data/support.zip   # lands in ~/ftw-local/data
docker compose logs --tail 100               # logs
docker compose restart                       # restart
```

`ftw update` and `ftw rollback` run in a container of their own. Core
restarts its container to start a release. That ends anything
`docker compose exec` started there, so `ftw` would stop before it could
report the result.

### Update new Docker

Use this only for an existing **new 0.x** project from the recipe above, not
for old 2.x/3.x Docker. Confirm the project and data path, then `cd ~/ftw-local`.

Update it as you would a native box: **More → Version → Update FTW** on the box's web
UI, the FTW app, or:

```bash
docker compose exec ftw ftw status
docker compose run --rm --entrypoint ftw ftw update
docker compose ps
```

Core downloads and checks the release, keeps it in the releases volume and
restarts. The launcher starts the new release once, as a trial. If it does
not become ready, or keeps stopping in its first hour, the launcher starts
the previous release again and `ftw status` says so. `ftw rollback` returns
to the previous release while both read the same data.

Rebuilding still works. A newer `FTW_VERSION` in `.env` and
`docker compose up -d --build` installs that release once, the same way. An
older `FTW_VERSION` changes nothing, because the box may have updated itself
past it; the log says which release stays. Use `ftw rollback` to go back.
Keep `FTW_VERSION` at or after the tag you took the `Dockerfile` from. A
release from before Docker updated itself has a launcher that cannot start
this image: it stops with `flag provided but not defined: -image`. Set the
version back and rebuild.

A project made before Docker updated itself runs Core without the launcher.
Take the new files once, from a published release that has them. Replace only
`v0.X.Y-beta.N`; the block keeps `data` and every other line of `.env`:

```bash
(
  set -eu
  tag=v0.X.Y-beta.N
  if [[ ! "$tag" =~ ^v0\.([0-9]+)\.[0-9]+(-beta\.[0-9]+)?$ ]] || (( 10#${BASH_REMATCH[1]} < 131 )); then
    echo "STOP: copy an exact published new 0.x tag from Releases." >&2
    exit 1
  fi
  if [ ! -f compose.yaml ] || [ ! -f .env ] || [ ! -d data ]; then
    echo "STOP: run this in the Compose project, normally ~/ftw-local." >&2
    exit 1
  fi
  downloads=$(mktemp -d)
  trap 'rm -rf "$downloads"' EXIT
  base="https://raw.githubusercontent.com/srcfl/ftw/${tag}/deploy/docker"
  curl -fSL "${base}/compose.yaml" -o "$downloads/compose.yaml"
  curl -fSL "${base}/Dockerfile" -o "$downloads/Dockerfile"
  grep -v '^FTW_VERSION=' .env > "$downloads/env" || true
  printf 'FTW_VERSION=%s\n' "$tag" >> "$downloads/env"
  cp "$downloads/compose.yaml" "$downloads/Dockerfile" .
  cp "$downloads/env" .env
)
```

Then rebuild with `docker compose up -d --build`. The first start installs
that release into the new releases volume and runs it; `data` stays as it was.

An install made with the old instructions may lack `.dockerignore`. Create it
beside the `Dockerfile` before any rebuild:

```bash
if [ ! -e .dockerignore ]; then
  printf '*\n!Dockerfile\n!.dockerignore\n' > .dockerignore
fi
```

If a file already exists, check that it excludes `data` and `.env`. Keep private
files private; do not change their permissions to make a build pass.

Each rebuilt version stays as an image: `docker image ls ftw-local` lists
them, and `docker image rm ftw-local:<version>` frees the space of one you no
longer need. The releases volume keeps the current and previous releases.

Back up and restore the whole data directory while FTW is stopped. The
releases volume needs no backup:

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

To remove it, run `docker compose down -v`, which also removes the releases
volume, remove the `ftw-local` images and delete `~/ftw-local`.

## For an agent that runs the host

Give this section to an agent that operates FTW for you. FTW controls real
equipment, so the agent uses the same commands a person would and leaves
decisions about the site to you.

- Choose one install path. Do not treat the native and Docker sections as a
  sequence. Stop on a download or install error; do not use stale files or
  switch methods to get past a refusal. Never make private data readable to
  fix a Docker build; exclude it from the build context.
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
  under `/opt/ftw`, and never remove `/var/lib/ftw`. Running
  `install.sh --fresh-host` again on an installed box only reports its state
  and changes nothing; it is no way to update or repair it.
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
- In Docker, run `ftw` in the container: `docker compose exec ftw ftw status`.
  Run `ftw update` and `ftw rollback` in a container of their own,
  `docker compose run --rm --entrypoint ftw ftw update`, so they outlive
  Core's restart. Read logs with `docker compose logs --tail 200`. Never
  remove the `ftw-local_releases` volume or edit files under `/opt/ftw`.

## Report

Open an issue at <https://github.com/srcfl/ftw/issues> that names the beta
and says what you did, what happened and what you expected. `ftw support`
writes a zip with logs and settings. Core removes secrets from it, but it
still describes your site, so share it privately if you prefer.

## Remove FTW

To keep a copy of the home's settings and history, run
`ftw backup --output-dir <folder>` first. Then:

```bash
sudo systemctl disable --now ftw
sudo rm -rf /opt/ftw /etc/systemd/system/ftw.service /usr/local/bin/ftw
sudo systemctl daemon-reload
```

The data stays in `/var/lib/ftw`, and the `ftw` account stays too. The
installer will not install over them, and says so. To install again on this
computer, back up the data if you may want it, then remove both. The copy
holds the box's keys, so the first line makes it readable only by you; keep
it private.

```bash
(umask 077; sudo tar -czf - -C /var/lib ftw > ~/ftw-data-backup.tar.gz)
sudo rm -rf /var/lib/ftw
sudo userdel ftw
```
