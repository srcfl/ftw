# forecast — weather fetcher + naive PV production estimate

## What it does

Fetches hourly weather (cloud cover + air temp, optionally shortwave irradiance) from one of two providers, derives a rough PV production curve from a geometric clear-sky model × cloud attenuation, and persists both the raw forecast and the PV estimate into `state.Store.SaveForecasts`. Runs a 3-hourly refresh goroutine for the lifetime of the process. Acts as the MPC fallback when the `pvmodel` digital-twin is disabled or cold.

## Key types

| Type | Purpose |
|---|---|
| `Provider` | Interface every weather source implements (`Name() / Fetch(ctx, lat, lon)`). |
| `RawForecast` | One hour of parsed upstream data (cloud %, temp, optional solar W/m²). |
| `MetNoProvider` | api.met.no locationforecast/2.0/compact, no API key, UA required. |
| `OpenWeatherProvider` | OpenWeatherMap One Call 3.0, API key required. |
| `Service` | Owns a provider + `*state.Store` + lat/lon + rated PV W; runs the refresh loop. |

## Public API surface

- `NewMetNo(userAgent string) *MetNoProvider` — `userAgent` defaults to the app name if empty (`forecast.go:62`). met.no blocks default UAs.
- `NewOpenWeather(apiKey string) *OpenWeatherProvider`.
- `FromConfig(cfg *config.Weather, ratedPVW float64, st *state.Store, userAgent string) *Service` — returns `nil` if provider is `""` / `"none"` / unknown.
- `(*Service).Start(ctx)` / `Stop()` — kicks the refresh loop; `Stop` waits for it to exit.
- `(*Service).Load(sinceMs, untilMs)` — passthrough to `state.Store.LoadForecasts`.
- `ClearSkyW(lat, lon, t)` — geometric solar irradiance on a horizontal surface (W/m²). Simplified Bird model; good enough for scaling.
- `EstimatePVW(lat, lon, t, cloudPct, ratedW)` — `ratedW × (clear_sky_w / 1000) × (1 − cloud/100)^1.5`.

## How it talks to neighbors

Writes via `state.Store.SaveForecasts([]state.ForecastPoint)` and reads via `LoadForecasts` — that's the only persistence dependency. `cmd/ftw/main.go` constructs the Service from the `config.Weather` section and passes the `*Service` into `mpc.Service` (fallback PV curve) and `api.Deps.Forecast` (the `GET /api/forecast` handler). No import of `drivers`, `telemetry`, or `control` — forecasts are a pure input to the planner.

## What to read first

`forecast.go` is the whole package. Read top-to-bottom: the provider implementations (met.no @ `:54`, openweather @ `:124`), then the clear-sky math (`ClearSkyW` @ `:189`), then the two-line PV estimate (`EstimatePVW` @ `:225`), then the refresh loop in `(*Service).loop` at `:286`.

## What NOT to do

- **Do NOT treat `EstimatePVW` as a physics model.** It's a rough directional signal for MPC / pre-charge — full tilt/azimuth/soiling belongs in `pvmodel` (the RLS digital-twin adapts to actual production over time). Don't overfit here.
- **Do NOT hit met.no without a User-Agent.** `NewMetNo("")` fills one in (`forecast.go:62`); a plain paho / default Go UA gets 403s.
- **Do NOT refresh faster than hourly.** The scheduler is hard-coded to 3 h (`forecast.go:289`). Both providers have fair-use policies; faster polling gets you banned.
- **Do NOT assume the forecast is always present.** Downstream consumers (`mpc`) must gracefully handle empty `LoadForecasts` — the service returns `nil` from `FromConfig` when weather is disabled, so the `*Service` pointer itself can be `nil`.
- **Do NOT add a new provider without matching `Name()` to the config enum.** `FromConfig`'s switch (`forecast.go:258-265`) is the contract — a new provider also needs a config key documented in `docs/configuration.md`.
