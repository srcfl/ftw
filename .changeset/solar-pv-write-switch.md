---
"ftw": minor
---

A driver's opt-in write path can be turned on from Settings, instead of by
hand-editing two keys in config.yaml.

Everything in the catalog reads. One driver can also write — the NIBE S-series
solar surplus feed — and arming it meant setting `config.write.solar_pv` on the
driver *and* `capabilities.http.allow_write` on the host, neither of which the
settings screen offered. An owner could install the driver from a card in the
UI and then had no way to use the one thing it was built for.

A driver now names its write paths in its `DRIVER` block
(`write_capabilities = { "solar_pv" }`), and Settings → Devices grows a *Solar
PV surplus feed* panel on the drivers that declare one: a switch and the
maximum surplus to report. A driver that declares nothing gets no panel and no
markup, so read-only drivers are untouched and nothing about writing is written
into their config.

The panel keeps the safety properties the YAML had, where an operator can see
them. One switch moves both gates, because holding one without the other never
wrote anything anyway — the host refuses the verb without the grant, and the
driver disables the feed without the verb — so a half-armed config reads as
off. The feed will not arm without a maximum above 0: that ceiling is what
stops a sign error or a telemetry spike from telling a pump there are 100 kW
going spare, and clearing it disarms a running feed. What the pump needs at its
own end — installer menu 7.5.15 set to read/write, its Solar PV input on —
cannot be checked from FTW, so the panel says so rather than letting the writes
fail silently as `read only value`.

The local-API help text also stopped telling NIBE owners to enable the API in
the myUplink app. It is generated on the pump's own screen; there is no app and
no cloud account in that path.
