# Deploying on a Mac mini or a generic Linux server

The fastest supported path to a running forty-two-watts is the Raspberry
Pi SD-card image ([rpi-image.md](rpi-image.md)). But the same multi-arch
Docker images run anywhere Docker does — this guide covers the two most
common "I already have a box" cases:

- a **Mac mini** (or any Mac) running as an always-on home server, and
- a **generic Linux server** — Ubuntu, Debian, an Intel NUC, a VM, etc.

Both pull the exact same pre-built images from GHCR
(`ghcr.io/frahlg/forty-two-watts` + `…-updater`), published for
`linux/amd64` and `linux/arm64`. No local build step.

> **Which file do I use?**
>
> | Platform | Compose file | Networking |
> |---|---|---|
> | Linux (Pi, Ubuntu, NUC, VM) | `docker-compose.yml` | `network_mode: host` |
> | macOS (Mac mini, etc.) | `docker-compose.macos.yml` | bridge + published ports |
>
> They are **alternatives**, not layers — pick one, don't combine them
> with `-f`. The split exists because `network_mode: host` is a
> Linux-kernel feature; on macOS it binds to the Docker Desktop VM, not
> to the Mac, so the dashboard would be unreachable. See
> [§ macOS networking](#macos-networking-what-changes-and-why) below.

---

## Generic Linux server (Ubuntu / Debian / NUC / VM)

This is the same flow as the Pi, minus the SD-card image. If you're on a
Debian-family distro the one-shot installer does everything:

```bash
curl -fsSL https://raw.githubusercontent.com/frahlg/forty-two-watts/master/scripts/install.sh | bash
```

It installs Docker Engine + the compose plugin, adds you to the `docker`
group, creates `~/forty-two-watts/data` with the right ownership, fetches
`docker-compose.yml`, and brings the stack up. Override the install
location with `FTW_DIR=/srv/ftw`, the image with `FTW_IMAGE=…`. See
[`scripts/install.sh`](../scripts/install.sh) for every env knob.

### Manual install (any Linux)

If you'd rather not pipe a script to bash, or you're on a non-Debian
distro (Fedora, Arch, openSUSE), do it by hand:

```bash
# 1. Docker Engine + compose plugin — use your distro's instructions:
#    https://docs.docker.com/engine/install/

# 2. Lay out the deploy directory.
mkdir -p ~/forty-two-watts/data
cd ~/forty-two-watts

# 3. The container runs as uid 100 / gid 101 (the alpine `ftw` user).
#    A host bind-mount must be owned by those ids or SQLite can't create
#    state.db inside it.
sudo chown -R 100:101 ./data

# 4. Grab the compose file + broker config.
curl -fsSLO https://raw.githubusercontent.com/frahlg/forty-two-watts/master/docker-compose.yml
mkdir -p mosquitto/config
curl -fsSL https://raw.githubusercontent.com/frahlg/forty-two-watts/master/mosquitto/config/mosquitto.conf \
  -o mosquitto/config/mosquitto.conf

# 5. Pull + start.
docker compose pull
docker compose up -d
```

Open `http://<server-ip>:8080/` from another device on the LAN, or
`http://localhost:8080/` on the box itself. First run drops you at the
`/setup` wizard.

### Linux-specific notes

- **Firewall.** Ubuntu Server commonly ships `ufw`. The dashboard
  (`8080`) and the broker (`1883`) need to be reachable from the LAN:

  ```bash
  sudo ufw allow from 192.168.0.0/16 to any port 8080 proto tcp
  sudo ufw allow from 192.168.0.0/16 to any port 1883 proto tcp
  ```

  (Adjust the subnet to your LAN. With `network_mode: host` there is no
  Docker-published port for `ufw` to special-case — the process listens
  directly on the host, so a normal `ufw` rule is all you need.)

- **Same L2 as your hardware = stable device identity.** With host
  networking the container shares the host's ARP table, so TCP devices
  on the same subnet resolve to a `mac:<addr>` `device_id` and battery
  models survive IP changes. A server on a *different* VLAN from your
  inverters falls back to `make:serial` (preferred, set by the driver)
  or `ep:<endpoint>`. See [device-identity.md](device-identity.md).

- **Custom (unmerged) drivers go in `./data/drivers/`.** A driver you
  wrote yourself — or a patched copy of a bundled one — belongs in the
  persistent `./data/drivers/` directory, not `/app/drivers/` inside the
  image. Files there survive image upgrades and reboots and shadow a
  bundled driver of the same name; a `docker cp` into the running
  container does **not** (it lands in the ephemeral container layer). See
  [writing-a-driver.md §9](writing-a-driver.md#9-installing-a-custom-driver-on-a-docker-deploy).

- **In-app updates work out of the box.** The `ftw-updater` sidecar in
  `docker-compose.yml` drives the web UI's Update / Restart buttons. See
  [self-update.md](self-update.md).

- **Run it as a boot service?** Docker's `restart: unless-stopped`
  already survives reboots once `docker.service` is enabled
  (`sudo systemctl enable docker`). If you'd rather run the **native
  binary** under systemd instead of Docker, that's the
  [operations.md](operations.md) path (`make build-amd64` + the
  [`deploy/forty-two-watts.service`](../deploy/forty-two-watts.service)
  unit).

---

## Mac mini (Docker Desktop)

A Mac mini makes a tidy always-on home server. The catch is networking:
Docker Desktop runs every container inside a hidden Linux VM, so the
Linux `network_mode: host` trick doesn't reach macOS. Use the dedicated
[`docker-compose.macos.yml`](../docker-compose.macos.yml) — it swaps host
networking for bridge networking with published ports, which is the only
thing that works on Docker Desktop.

### Quick install

With Docker Desktop installed and running, the one-shot installer does
the rest (lay out the directory, fetch the macOS compose file, start the
stack):

```bash
curl -fsSL https://raw.githubusercontent.com/frahlg/forty-two-watts/master/scripts/install-macos.sh | bash
```

Prefer to do it by hand? The manual steps are below.

### Setup

```bash
# 1. Install Docker Desktop for Mac (Apple Silicon or Intel — the images
#    are multi-arch, so either works):
#    https://docs.docker.com/desktop/install/mac-install/
#    Make sure Docker Desktop is running before continuing.

# 2. Lay out the deploy directory.
mkdir -p ~/forty-two-watts/data
cd ~/forty-two-watts

# NOTE: no `chown 100:101` here. Docker Desktop's file sharing maps host
# ownership transparently, so the container's ftw user can always write
# to ./data. (Running that chown on macOS would do nothing useful.)

# 3. Grab the macOS compose file + broker config.
curl -fsSL https://raw.githubusercontent.com/frahlg/forty-two-watts/master/docker-compose.macos.yml \
  -o docker-compose.macos.yml
mkdir -p mosquitto/config
curl -fsSL https://raw.githubusercontent.com/frahlg/forty-two-watts/master/mosquitto/config/mosquitto.conf \
  -o mosquitto/config/mosquitto.conf

# 4. Pull + start (note the -f on every command).
docker compose -f docker-compose.macos.yml pull
docker compose -f docker-compose.macos.yml up -d
```

Open `http://localhost:8080/` on the Mac, or `http://<mac-ip>:8080/`
from another device. First run drops you at the `/setup` wizard.

> Tip: typing `-f docker-compose.macos.yml` on every command gets old.
> Either `export COMPOSE_FILE=docker-compose.macos.yml` in your shell, or
> rename the file to `compose.yaml` (Docker's default-discovered name) so
> plain `docker compose up -d` finds it.

### macOS networking: what changes, and why

Everything below follows from one fact: **containers run in the Docker
Desktop Linux VM, not directly on macOS.** That VM sits behind a NAT.

| Concern | On Linux (host net) | On macOS (this file) |
|---|---|---|
| Dashboard reachable | host binds `:8080` directly | published `8080:8080` |
| Reach embedded broker | `localhost:1883` | **`mosquitto:1883`** (service name) |
| LAN device → broker | `<host-ip>:1883` | `<mac-ip>:1883` (published) |
| Modbus TCP to a known IP | ✅ direct | ✅ via Docker Desktop NAT |
| mDNS (`zap.local`) | ✅ | ❌ doesn't cross the VM |
| UDP broadcast discovery | ✅ | ❌ doesn't cross the VM |
| ARP `mac:<addr>` identity | ✅ shared ARP table | ❌ → `make:serial` / `ep:` |

Two things you **must** account for in `config.yaml`:

1. **Point MQTT at `mosquitto`, not `localhost`.** Without host
   networking, `localhost` inside the container is the container itself.
   The embedded broker is reachable by its compose service name:

   ```yaml
   # config.yaml — MQTT-based drivers on macOS
   mqtt:
     host: mosquitto      # NOT localhost / 127.0.0.1
     port: 1883
   ```

   A broker elsewhere on the LAN is reachable by its IP as normal
   (outbound unicast works through the NAT).

2. **Give every driver an explicit IP — no auto-discovery.** mDNS names
   like `zap.local` and broadcast-based discovery don't cross the VM
   boundary. A Sourceful Zap, for example, must be configured with its
   numeric address (`192.168.1.x`) rather than `zap.local`. Modbus TCP
   to a known inverter/meter IP works fine — that's plain outbound
   unicast.

If a device shows up with an `ep:<endpoint>` `device_id` instead of
`make:serial`, that's expected on macOS (the ARP path is unavailable) and
harmless as long as the driver sets make + serial. See
[device-identity.md](device-identity.md).

### In-app updates on macOS

The `ftw-updater` sidecar is wired into `docker-compose.macos.yml` and
works on Docker Desktop — its `FTW_UPDATER_COMPOSE` points at the macOS
file so the Update / Restart buttons recreate the right containers. If
you'd rather upgrade by hand, the manual path is:

```bash
cd ~/forty-two-watts
docker compose -f docker-compose.macos.yml pull
docker compose -f docker-compose.macos.yml up -d
```

### Keeping it running

Docker Desktop does not auto-start at login by default. To make the Mac
mini a true appliance:

- Docker Desktop → **Settings → General → "Start Docker Desktop when you
  sign in"**, and disable **"Open Docker Dashboard at startup"**.
- Enable auto-login for the server account (System Settings → Users &
  Groups → Automatically log in as …) so a reboot brings Docker — and
  therefore the `restart: unless-stopped` stack — back up unattended.
- Prevent sleep: System Settings → **Energy** → "Prevent automatic
  sleeping when the display is off", or `sudo pmset -a sleep 0`.

---

## Backup, restore, troubleshooting

These are platform-independent — the state lives in `./data` regardless
of OS. [operations.md § 7](operations.md#7-backup--restore) covers backup
and restore; [§ 8](operations.md#8-troubleshooting-runbook) is the
troubleshooting runbook. The only Docker-specific wrinkle: stop the stack
with `docker compose … down` (add `-f docker-compose.macos.yml` on a Mac)
before snapshotting `./data` for a consistent SQLite copy.

## See also

- [rpi-image.md](rpi-image.md) — the turnkey Raspberry Pi SD-card image
- [operations.md](operations.md) — native-binary + systemd deploy, backup, runbook
- [self-update.md](self-update.md) — the in-app Update/Restart mechanism
- [device-identity.md](device-identity.md) — how `device_id` is resolved
- [configuration.md](configuration.md) — full `config.yaml` schema
