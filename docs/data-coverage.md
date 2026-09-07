# Geographic coverage of external data sources

FTW controls hardware anywhere. The data it plans with does not.

- **Weather and PV forecasting works worldwide.**
- **Day-ahead spot prices work in Europe only.**
- **A static / time-of-use tariff works anywhere.** That is how a site
  outside ENTSO-E (or on a flat retail contract inside it) gets
  price-driven planning.

A site outside Europe still gets monitoring, safety, control, the
dashboard, weather forecasts and the self-learning PV twin. What it did
not have, until `price.provider: static`, was a price source.

## Spot prices — Europe, or a tariff you type

Configured under `price.provider`.

| Provider | Coverage | API key | Notes |
|---|---|---|---|
| `sourceful` | European day-ahead markets | No | Default. Sourceful's cached ENTSO-E API. |
| `elprisetjustnu` | **Sweden only** — zones SE1–SE4 | No | 15-minute PTU since late 2025. |
| `entsoe` | ENTSO-E member markets | Yes | Direct from the Transparency Platform. |
| `static` | **Worldwide** | No | Flat rate or time-of-use schedule the operator types. |
| `none` | — | — | Disables price fetching entirely. |

There is still no live adapter for CAISO, ERCOT, PJM, AEMO/NEM, JEPX or
the other non-European markets. `static` is the stand-in: a US TOU
tariff, a Japanese flat contract, or any other schedule that can be
written as hours and a rate.

Prices are stored in **minor units of the configured currency** per kWh
(öre, cent, ¢, pence, …). ENTSO-E figures that arrive in another
currency are converted with **ECB** daily FX rates. Static prices are
already in the install currency; nothing is converted.

> The Tibber driver (`drivers/tibber.lua`) is telemetry only — it reports
> meter readings, not prices, so it is not a fifth price source.

```yaml
price:
  provider: static
  currency: USD
  static_ore_kwh: 8          # off-peak, ¢/kWh (minor units)
  static_tou:
    - { start: "07:00", end: "23:00", ore_kwh: 22 }   # peak
  grid_tariff_ore_kwh: 4
  vat_percent: 0
```

Windows are local time on the box. First match wins; hours they miss
keep `static_ore_kwh`. Overnight windows wrap (`22:00`–`06:00`). An
optional `days` list (`mon`…`sun`) limits a window to those weekdays.

The zone picker is served from `GET /api/prices/zones` and is hidden
when the provider is `static`. See
[`go/internal/prices/zones.go`](../go/internal/prices/zones.go) and
[`go/internal/prices/static.go`](../go/internal/prices/static.go).

## Weather and PV forecasts — worldwide

Configured under `weather.provider`. All four work at any latitude/longitude.

| Provider | Coverage | API key | Signal quality |
|---|---|---|---|
| `met_no` | Global | No | Cloud cover only — weakest PV signal. |
| `openweather` | Global | Yes | Cloud cover only. |
| `open_meteo` | Global | No | Shortwave radiation (GHI) — good. |
| `forecast_solar` | Global | No (free tier) | Site-calibrated watts from panel geometry — best. |

Prefer `open_meteo` or `forecast_solar` when the site has array
geometry: they carry an irradiance signal, which is what the
orientation-aware plane-of-array model needs.

## What is not in this tree yet

| Capability | Planned source | Coverage when it lands |
|---|---|---|
| PV performance scoring / forecast calibration | SMHI STRÅNG ([#734](https://github.com/srcfl/ftw/pull/734)) | Nordic region only |
| Automatic roof geometry | Lantmäteriet via STAC ([#735](https://github.com/srcfl/ftw/pull/735)) | Sweden by default; any conformant STAC catalog |

Until those land, array geometry stays a manual entry on the Weather
tab, and the PV twin still calibrates from measured production.

## What a non-European site has today

| Capability | Works outside Europe? |
|---|---|
| Device control, safety, dispatch | Yes |
| Telemetry, history, dashboard | Yes |
| Weather + PV forecasting | Yes |
| Self-learning PV twin | Yes |
| Price-driven planning | **Yes, via `static`** |
| Live day-ahead spot | **No** — Europe only |
| Manual array geometry | Yes |
