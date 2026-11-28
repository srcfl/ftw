# Have you run FTW before?

Find what runs now and follow its row. Only one FTW may control a home.
[Svenska](setup-guide/update-sv.md)

| What runs now | What to do |
|---|---|
| FTW 0.131 or newer, installed on Linux | Update it; do not install again. [Update a native box](native-beta.md#update-a-native-box) |
| FTW 0.131 or newer in Docker | [Update new Docker](native-beta.md#update-new-docker) |
| The Home Assistant app, version 0.x: **FTW (beta)**, once named **FTW (0.x beta)** | Update it under **Settings → Updates**. FTW then opens inside Home Assistant, and a bookmark to port 8080 stops working. [FTW on Home Assistant](home-assistant-app.md#updates) |
| FTW 2.x or 3.x, 0.130 or older, or the old ready-made Pi image | [Switch with a second SD card](#switch-with-a-second-sd-card) |
| Old FTW in Docker, on a computer you keep | [Switch on the same computer](#switch-on-the-same-computer) |
| The Home Assistant app, version 3.x, also named **FTW (beta)** | [Switch in Home Assistant](#switch-in-home-assistant) |
| Not sure | [Find out what is running](native-beta.md#find-out-what-is-running) |

The web UI shows the version under **More → Version**; on Linux,
`ftw status` shows it too.

## Why switch

FTW 2.x and 3.x get no more updates, not even safety fixes. New releases come
on the 0.x line, which starts at `v0.131.0`. The lower number is on purpose:
releases up to 0.130 belong to the old line. Choosing beta in an old install
does not move it to the new line, and 3.x beta is not a step on the way.

A switch is a new setup with its own data. The guided move of old settings,
history and goals is not ready. If those data must move first, ask in
[Discord](https://discord.gg/UK2ygPBu8N) about your install. Do not copy an
old database into the new one.

## Before you switch

- Write down device addresses, goals and schedules. Calendar support is gone;
  the new line uses charging targets and ready-by times.
- Make a full backup with the tools your version has. Keep it off the computer.
- Keep the old install and its data, so you can go back.
- Stop old FTW before new FTW starts, on every computer that could run it. An
  old FTW can keep controlling without a working web page.

## Switch with a second SD card

1. On the old card, save the notes and backup above. Do not run the installer
   there. Keep this card as it is.
2. On your computer, write Raspberry Pi OS Lite (64-bit) to a **second** card.
   The [Pi guide](setup-guide/en.md) shows how. Choose a login name other than
   `ftw`.
3. Shut the Pi down, unplug it and swap the cards. Then plug it in again.
4. Log in over SSH with the new login. The address may have changed.
5. Stop old FTW on any other computer that could control the same equipment.
   If the old card ran your MQTT broker, set up another one; see
   [An MQTT broker](native-beta.md#an-mqtt-broker).
6. Run the install line and set up your home:
   [A Linux computer](install.md#a-linux-computer).
7. Check the result: [Verify the switch](native-beta.md#verify-the-switch).

To go back, shut the Pi down and put the old card back. Stop any new FTW on
another computer too. Data the new setup collected stays on the new card.

## Switch on the same computer

This is for old FTW in Docker on a computer you want to keep. It is harder:
you must find and stop old Core, its updater and whatever starts them at
boot, and keep any MQTT broker running. Follow
[Same Linux host: use a separate Docker project](native-beta.md#same-linux-host-use-a-separate-docker-project).
If that is more than you want, use a second card or another computer.

The installer refuses a computer that holds an FTW site. Do not remove its
checks or the old files to get past it.

## Switch in Home Assistant

The retired Home Assistant app runs 3.x and gets no more updates. It was
also named **FTW (beta)**; its version tells it apart.

1. Take a Home Assistant backup that includes the old app.
2. Under **Settings → Apps**, open the **FTW (beta)** with version 3.x, turn
   off **Start on boot** and **Watchdog**, then choose **Stop**.
3. Install the app with version 0.x: [FTW on Home Assistant](home-assistant-app.md).
   The old settings and history do not move over.

If the FTW app or webapp says the box is too old, check that you run version
0.x. If you used the webapp with an older FTW, remove it from the home screen
and pair again.

## FTW on another computer

Stop it there, and stop whatever starts it at boot, before the new FTW starts.
