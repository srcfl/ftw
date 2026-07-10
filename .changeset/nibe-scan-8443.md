---
"forty-two-watts": minor
---

Network scan now probes port 8443 (HTTPS) in addition to Modbus 502 / MQTT 1883 /
HTTP 80. On-prem device APIs that only listen on HTTPS — notably the NIBE S-series
heat pump's Local REST API — now show up in Settings → Scan and the setup wizard.
Previously a NIBE pump was pingable but invisible to discovery because its API
port wasn't probed.
