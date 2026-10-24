# FTW on Home Assistant (0.x beta, pilot)

> **Beta / pilot.** The **FTW (0.x beta)** app is for a first Home Assistant
> Green pilot. Reviewed beta pins
> arrive as app updates. You choose when to install them. You can also run FTW on a
> [separate Linux host](native-beta.md) and connect it to Home Assistant with
> the [MQTT integration](ha-integration.md).

[Svenska](setup-guide/home-assistant-sv.md)

## What you need

- Home Assistant OS (for example Home Assistant Green), amd64
  or aarch64. Home Assistant Container/Core has no apps; use a
  [separate host](native-beta.md) instead.
- Keep at least 1 GB free for the first build, plus room for history and backups.
- Your inverter, battery, meter or charger on the same network.

## 1. Stop any other FTW first

Only one FTW may control a site.

- **Old FTW (beta) app (3.x):** open it under **Settings → Apps**, turn
  off **Start on boot** and **Watchdog**, then **Stop**. Take a backup
  first if you want to keep it. Its settings and history do not move over.
- **FTW on another computer:** stop it there before you continue.

## 2. Add the app repository

1. **Settings → Apps → Install app** to open the App store.
2. Menu (⋮) top right → **Repositories**.
3. Paste `https://github.com/srcfl/home-assistant-addons` and choose **Add**.
4. Close the dialog. Reload the page if the store has not changed.

## 3. Install

1. Find **FTW (0.x beta)** in the store and open it.
2. Choose **Install**. Home Assistant builds the app on your device from
   FTW's release package and checks its checksum. The first build downloads
   packages and can take several minutes; wait for it to finish.
3. Turn on **Start on boot** and **Watchdog**, then **Start**.
4. Open the **Log** tab. You should see `FTW starting`, then
   `no config found — starting setup wizard` on a new install.
   After setup, the log shows `HTTP API ready`.

## 4. Open FTW and set it up

Choose **Open web UI** inside Home Assistant. App versions with ingress use
HA's login; no separate FTW password is needed. You can enable **Show in
sidebar** on the app page. HA requires an administrator to open FTW.
The direct web port on the home network is closed.

A new install opens the setup wizard. Follow it to add your devices. The
[setup guide](setup-guide/en.md) explains each step. An existing app with a
saved FTW password uses HA's login after the update too. Its settings and
history stay in place.

Give Home Assistant a fixed address (DHCP reservation in the router), so your
devices keep finding it.

## 5. Connect the FTW app (iPhone) or webapp

The new FTW webapp and iOS app need new FTW 0.x — this app provides it.
Ingress opens the web UI through HA. Direct LAN access from a separate FTW
client is not part of this mode. For a separate client, enable FTW Cloud and
follow [Pair with one iPhone](pairing.md). If you used the webapp with an older FTW,
remove it from the home screen and pair again.

## 6. Home Assistant entities (optional)

FTW can publish its values to Home Assistant over MQTT. See
[Home Assistant integration](ha-integration.md). Use the Mosquitto broker app
and point FTW at your Home Assistant address.

## Updates

New FTW betas arrive as normal app updates under **Settings → Updates**.
Expect frequent updates during the beta. Take a backup first if you want a way
back. Home Assistant owns the app's updates; the native launcher's trial and
automatic rollback do not run here. `ftw update` does not apply inside the app.

## Backup and restore

FTW's data (settings, history, pairing) lives in the app. Home Assistant
backups include it; the app stops briefly while the backup is taken.
Under **Settings → System → Backups**, restore the FTW app from the backup.
This restores its version and data together. Keep other FTW instances stopped.

## Troubleshooting

| Symptom | What to do |
|---|---|
| The app is not in the store | Check the repository URL, reload the page, or **⋮ → Check for updates**. |
| Install fails | Open **Settings → System → Logs → Supervisor**. A download or checksum error means the package could not be fetched; try again later. |
| Web UI does not open | Check the app **Log**. Open it through HA. Ingress port 18099 must be free. |
| FTW cannot reach a device | Check the device's IP address and that it is on the same network. |
| The FTW app/webapp says the box is too old | Make sure you installed **FTW (0.x beta)**, not the old **FTW (beta)**. |

Report app problems in the
[app issue tracker](https://github.com/srcfl/home-assistant-addons/issues)
and FTW problems [here](https://github.com/srcfl/ftw/issues), or ask in
[Discord](https://discord.gg/UK2ygPBu8N). Include the app log.
