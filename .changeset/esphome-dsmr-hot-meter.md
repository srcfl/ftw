---
"forty-two-watts": minor
---

Add a read-only ESPHome DSMR/P1 meter driver and hot-reload site-meter changes into control, MPC, and load-model services without restarting.

The driver uses site-sign power, retries serial discovery for stable hardware identity, backs off after failures, and omits optional phase or lifetime-counter fields when their reads fail rather than publishing unsafe zeroes.
