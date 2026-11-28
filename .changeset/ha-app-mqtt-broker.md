---
"ftw": minor
---

In the Home Assistant app, FTW's entities now appear in Home Assistant without typing a broker. The app passes Home Assistant's own MQTT broker (the Mosquitto broker app) to Core. A site that never set up the Home Assistant bridge starts it with that broker, and an empty broker field means that broker. A broker you typed yourself still wins, and **Settings → Integrations → Enabled** still turns the bridge off. Outside the Home Assistant app nothing changes.
