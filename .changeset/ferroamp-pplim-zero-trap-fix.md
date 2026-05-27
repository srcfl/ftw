---
"forty-two-watts": patch
---

**Ferroamp safety fix:** the Lua driver now refuses to publish
`pplim arg=0` from any `curtail` / `curtail_disable` path.

Ferroamp's extapi treats `{"cmd":{"name":"pplim","arg":0}}` as
"limit PV output to 0 W" — same wire bytes as a naive release would
have, opposite semantics. The inverter sticks at 0 W PV until the
operator clears pplim from the Ferroamp portal or power-cycles the
EnergyHub. On 2026-05-27 this fired against a live SE4 site after the
dispatcher's proportional curtail allocation gave a 0-share to
Ferroamp; recovery required a 30+ minute outage and a portal-side
reset.

Changes:

- `curtail` with `power_w <= 0` is now a logged no-op (was: published
  `pplim arg=0`).
- `curtail_disable` is a logged no-op by default (was: published
  `pplim arg=0`). To restore automatic release, set
  `config.pplim_release_w` on the driver to the inverter's nominal
  max (e.g. `15000` for a 15 kW SSO). The driver then publishes
  `pplim arg=<release_w>` which Ferroamp accepts as "raise the limit".
- New unit tests guard the wire payload against any regression that
  reintroduces `pplim arg=0`.
- Docs in `docs/configuration.md` describe the trap and the new
  config field.

Operators with `supports_pv_curtail: true` on Ferroamp **should** add
`config.pplim_release_w: <SSO-rated-watts>` to keep curtailment
auto-releasing. Without it, curtail still engages correctly, but
release becomes a portal action.
