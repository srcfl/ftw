---
"ftw": patch
---

Recover faster from mute Modbus TCP sessions after a hard reboot (GoodWe single-session dongles): keep TCP keepalive and add exponential reconnect backoff so ghost sessions can age out without power-cycling the inverter.
