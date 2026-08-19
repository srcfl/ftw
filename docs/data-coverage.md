# Geographic coverage of external data sources

FTW controls hardware anywhere, but it depends on external data for three
things: **spot prices**, **weather/PV forecasts** and **PV performance
scoring**. Those three have very different geographic reach, and the difference
decides how much of FTW is useful at a given site.

Short version:

- **Weather and PV forecasting works worldwide.**
- **Price-driven planning works in Europe only.**
- **PV performance scoring works in the Nordic region only.**
- **Roof geometry (planned) is Sweden only.**

A site outside Europe can still run FTW for monitoring, safety and control — but
the economic optimisation that motivates most of the planner has no price source
to work from.

`GET /api/data-sources` answers this per site: it returns every source with its
coverage area and, when the site location is known, whether that source reaches
it. The Weather settings tab renders the same data under the map. This file is
the prose; `go/internal/coverage` is the machine-readable source of truth, and
the two are meant to stay in step.

> **Coverage bounds are advisory.** Each bounded source declares a lat/lon box,
> but STRÅNG's model grid is rotated relative to lat/lon, so its box is a
> *superset* of the real domain — points near a corner pass the box test and
> still return nothing. Treat `covers: false` as definitive and `covers: true`
> as "worth trying". The upstream API is always the final word.

## Spot prices — Europe only

Configured under `price.provider`.

| Provider | Coverage | API key | Notes |
|---|---|---|---|
| `sourceful` | European day-ahead markets | No | Default. Sourceful's cached ENTSO-E API. |
| `elprisetjustnu` | **Sweden only** — zones SE1–SE4 | No | 15-minute PTU since late 2025. |
| `entsoe` | ENTSO-E member markets (most of Europe) | Yes | Direct from the Transparency Platform. |
| `none` | — | — | Disables price fetching entirely. |

There is **no provider for any market outside Europe**. North America (CAISO,
ERCOT, PJM, ISO-NE, NYISO, MISO, SPP, AESO, IESO), Australia (AEMO/NEM), Japan
(JEPX) and everywhere else are unsupported, and there is no manual or
fixed-tariff provider to stand in for them.

Two further Europe-centric assumptions live in the price layer: prices are
stored internally in **öre** (1 SEK = 100 öre), and ENTSO-E's EUR/MWh figures
are converted using **ECB** daily FX rates.

> The Tibber driver (`drivers/tibber.lua`) is telemetry only — it reports meter
> readings, not prices, so it is not a fourth price source.

## Weather and PV forecasts — worldwide

Configured under `weather.provider`. All four work at any latitude/longitude.

| Provider | Coverage | API key | Signal quality |
|---|---|---|---|
| `met_no` | Global | No | Cloud cover only — weakest PV signal. |
| `openweather` | Global | Yes | Cloud cover only. |
| `open_meteo` | Global | No | Shortwave radiation (GHI) — good. |
| `forecast_solar` | Global | No (free tier) | Site-calibrated watts from panel geometry — best. |

Accuracy varies by region because the underlying numerical weather models do,
but none of these are geographically gated. Outside the Nordics, prefer
`open_meteo` or `forecast_solar`: they carry an irradiance signal, which is what
the orientation-aware plane-of-array model needs.

## PV performance scoring — Nordic region only

The scorer (`GET /api/pv/performance`) compares measured production against a
physics baseline built from **SMHI STRÅNG** irradiance. STRÅNG is a mesoscale
analysis product covering the **Nordic region** hourly at ~2.5 km from 1999 to
roughly one day ago. It is free, keyless and CC BY 4.0.

Outside that domain STRÅNG returns no data, so scoring simply never produces
rows and the dashboard overlay stays hidden. Nothing fails loudly; the feature
is just unavailable.

Because the same scoring feeds the **forecast calibration factor**, sites
outside the STRÅNG domain also do not get measured calibration of their PV
forecast — they fall back to the uncalibrated physics estimate.

STRÅNG has **no forward horizon**. It is never used as a forecast provider; see
[architecture.md](architecture.md) for where it sits.

### What STRÅNG actually publishes

Probing the live API on 2026-07-31 (SMHI's own apidocs pages currently 404)
returned data for exactly seven parameters and 404 for everything else. Names
were confirmed from their magnitudes on a clear day rather than from docs:

| Code | Quantity | Unit | Noon value, Stockholm 2026-06-21 |
|---|---|---|---|
| 116 | CIE-weighted UV irradiance | mW/m² | 146.6 |
| 117 | **Global horizontal (GHI)** | W/m² | 810.5 |
| 118 | Direct normal (DNI) | W/m² | 917.2 |
| 119 | **Sunshine duration** | min/h | 60.0 |
| 120 | Photosynthetically active radiation | W/m² | 357.9 |
| 121 | Direct horizontal | W/m² | 723.0 |
| 122 | **Diffuse horizontal (DHI)** | W/m² | 87.5 |

Two checks confirm the identification: 121 + 122 = 723.0 + 87.5 = 810.5, exactly
parameter 117 (direct + diffuse = global), and 119 caps at exactly 60, i.e.
minutes within the hour.

**STRÅNG publishes no cloud cover.** It is a radiation model; cloudiness is not
among its outputs. It is however *derivable*: parameter 119 counts the minutes
in each hour during which direct beam irradiance exceeded the WMO sunshine
threshold, so `1 − minutes/60` is the fraction of the hour the sun spent
obscured. That is an observed quantity rather than an inferred cloud field, but
it is coarser than a forecast provider's cloud percentage — it cannot see thin
cirrus that dims without blocking. FTW exposes it via
`strang.IrradianceHour.CloudCover(lat, lon)`, which returns an explicit
"unknown" rather than defaulting to "clear".

The location argument is not decoration. Sunshine duration is zero at night for
the trivial reason that there is no sun, and zero again near sunrise and sunset
because the beam crosses ten or more air masses and cannot reach the 120 W/m²
threshold even under a spotless sky. Both would read as "100% overcast" if taken
at face value. `CloudCover` therefore declines to answer unless the sun clears
**5° of elevation** at some point in the hour, sampling the hour's start,
midpoint and end so the hour in which the sun crosses that line is still counted.

Live data from 2026-06-21 at Stockholm shows the distinction:

| Hour (UTC) | GHI W/m² | Sunshine | Cloud cover |
|---|---|---|---|
| 00:00 | 0.0 | 0 min | *unknown* — sun below horizon |
| 04:00 | 163.2 | 60 min | 0% |
| 12:00 | 810.5 | 60 min | 0% |
| 20:00 | 2.5 | 0 min | *unknown* — sun minutes from setting |

## Roof geometry — Sweden only (planned)

The roof-derivation module proposed in
[RFC #717](https://github.com/srcfl/ftw/discussions/717) reads **Lantmäteriet**
building footprints and LiDAR, which exist for **Sweden only** and require a
Geotorget account. Everywhere else, panel tilt/azimuth/kWp stays a manual entry
in the Weather settings tab — which is the fallback by design, not a
degraded mode.

## What a non-European site loses

| Capability | Works outside Europe? |
|---|---|
| Device control, safety, dispatch | Yes |
| Telemetry, history, dashboard | Yes |
| Weather + PV forecasting | Yes |
| Self-learning PV twin | Yes |
| Price-driven planning / optimisation | **No** — no price source |
| PV performance scoring + calibration | **No** — outside STRÅNG's domain |
| Automatic roof geometry | **No** — Sweden only |
