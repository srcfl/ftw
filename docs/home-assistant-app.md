# FTW on Home Assistant

The **FTW (beta)** app runs FTW inside Home Assistant OS and opens it there,
behind Home Assistant's login. Each FTW beta arrives as an app update, and you
choose when to install it. A stable app named **FTW** joins the store with the
first stable release. You can also run FTW on a
[separate Linux computer](install.md#a-linux-computer) and connect it to Home
Assistant over [MQTT](ha-integration.md).
[Svenska](setup-guide/home-assistant-sv.md)

Have you run FTW before, here or on another computer? Read
[Have you run FTW before?](before.md) first. Only one FTW may control a home.

## What you need

- Home Assistant OS (for example Home Assistant Green), amd64
  or aarch64. Home Assistant Container/Core has no apps; use a
  [separate computer](install.md#a-linux-computer) instead.
- Room for history and backups.
- Your inverter, battery, meter or charger on the same network.

## 1. Add the app repository

[![Add the FTW repository to my Home Assistant](https://my.home-assistant.io/badges/supervisor_add_addon_repository.svg)](https://my.home-assistant.io/redirect/supervisor_add_addon_repository/?repository_url=https%3A%2F%2Fgithub.com%2Fsrcfl%2Fhome-assistant-addons)

The button opens your Home Assistant and asks to add the repository. Or do it
by hand:

1. **Settings → Apps → Install app** to open the App store.
2. Menu (⋮) top right → **Repositories**.
3. Paste `https://github.com/srcfl/home-assistant-addons` and choose **Add**.
4. Close the dialog. Reload the page if the store has not changed.

## 2. Install

1. Find **FTW (beta)** in the store and open it. Its version starts with `0.`.
2. Choose **Install**. Home Assistant downloads FTW's prebuilt, signed image.
   It takes about as long as the download.
3. Turn on **Watchdog**, so Home Assistant restarts FTW when its health check
   fails. **Start on boot** is on already. Then choose **Start**.
4. Open the **Log** tab. You should see `FTW starting`, then
   `no config found — starting setup wizard` on a new install.
   After setup, the log shows `HTTP API ready`.

## 3. Open FTW and set it up

Choose **Open Web UI**. FTW opens inside Home Assistant, signed in with your
Home Assistant login; it needs an administrator. To open FTW from the
sidebar, turn on **Show in sidebar** on the app's **Info** tab.

A new install opens the setup guide there. It asks for your home and your
devices; [Set up your home](install.md#set-up-your-home) shows what to expect.
Home Assistant's login guards FTW, so the guide asks for no house password.

FTW has no web port of its own on your network; it opens only through Home
Assistant. Give Home Assistant a fixed address (a DHCP reservation in the
router), so your devices keep finding it.

## 4. Connect the iPhone app

In FTW, open **Settings → FTW app** and follow
[Pair with one iPhone](pairing.md). Use the pairing link or the QR code; the
phone then reaches FTW through FTW Cloud. The short code does not work in
this app, because it needs FTW's own web port on your network.

## 5. Home Assistant entities

FTW's values appear as Home Assistant entities with nothing to type. Add the
**Mosquitto broker** app and the **MQTT** integration that Home Assistant then
offers. If you add Mosquitto while FTW runs, FTW finds it within a minute,
with no restart. The app log shows `Home Assistant's MQTT broker is
core-mosquitto:1883` when FTW starts, or `found Home Assistant's MQTT broker`
when FTW finds it later.

For Home Assistant's **Energy dashboard**, pick FTW's **Total grid import**,
**Total grid export**, **Total PV generation**, **Total battery charged** and
**Total battery discharged** sensors.

To turn this off, or to use another broker, open **Settings → Integrations**
in FTW. See [Home Assistant integration](ha-integration.md).

## Updates

New FTW betas arrive as normal app updates under **Settings → Updates**.
Expect frequent updates during the beta. Take a backup first if you want a way
back. Home Assistant owns the app's updates; the native launcher's trial and
automatic rollback do not run here. `ftw update` does not apply inside the app.

The stable **FTW** app is a separate app with its own data, so moving from
**FTW (beta)** to **FTW** is a new setup. Take a backup, stop **FTW (beta)**
and turn off its **Start on boot** and **Watchdog**. Then install **FTW** and
set it up.

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
| Web UI does not open | Check the app **Log**. Open FTW with **Open Web UI** or from the sidebar; a bookmark to port 8080 does not work. Port 18099 on Home Assistant must be free. |
| No FTW entities | Check that the Mosquitto broker app runs and the MQTT integration is added. FTW looks for the broker again within a minute. The app log says what it found. |
| FTW cannot reach a device | Check the device's IP address and that it is on the same network. |
| Two apps named **FTW (beta)** | The one with version 3.x is retired. See [Switch in Home Assistant](before.md#switch-in-home-assistant). |

Report app problems in the
[app issue tracker](https://github.com/srcfl/home-assistant-addons/issues)
and FTW problems [here](https://github.com/srcfl/ftw/issues), or ask in
[Discord](https://discord.gg/UK2ygPBu8N). Include the app log.
