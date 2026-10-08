---
"ftw": patch
---

Keep one Modbus writer per device. The proxy defaults to read only. When external writes are enabled, FTW keeps reading but stops all device writes until the owner changes the setting and restarts Core. Restrict proxy requests to configured unit IDs, give driver traffic priority, and close idle clients on shutdown.

Rename the Home Assistant settings tab to Integrations. Home Assistant MQTT and the Modbus proxy each keep their own section.
