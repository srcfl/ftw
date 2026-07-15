// Package forecast fetches hourly weather + derives a PV production estimate
// for the next ~48 hours. Persists to state DB and exposes via /api/forecast.
//
// Supported providers:
//   - met.no — Norwegian Meteorological Institute, all EU, no API key,
//     hourly forecast 0-60h + 6-hourly beyond. User-Agent required.
//   - openweather — OpenWeatherMap, worldwide, API key required.
//
// PV estimate formula (simple, works ok for a fixed-azimuth array):
//
//   clear_sky_w(lat, lon, t) — geometric solar elevation × array rating
//   pv_w ≈ clear_sky_w × (1 − cloud_cover/100)^1.5
//
// A full model would factor in panel tilt, azimuth, temperature derating,
// soiling, etc. This is intentionally coarse — the battery models + RLS
// adapt to the actual pv curve over time, so the forecast just needs to
// be directionally right for MPC / pre-charge scheduling.
package forecast

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
)

// Provider is implemented by each weather source.
type Provider interface {
	Name() string
	Fetch(ctx context.Context, lat, lon float64) ([]RawForecast, error)
}

// RawForecast is one hour's parsed forecast. Fields may be nil if the
// provider didn't return them. Providers populate whichever fields they
// natively expose — the downstream fetchAndStore selects the most
// direct PV-power signal available (PVWEstimated > SolarWm2 → derived
// > CloudCoverPct → derived).
type RawForecast struct {
	HourStart     time.Time
	CloudCoverPct *float64 // 0..100
	TempC         *float64
	SolarWm2      *float64 // direct shortwave radiation W/m² (if provider gives it)
	PVWEstimated  *float64 // provider-native site PV output (if provider gives it, e.g. Forecast.Solar)
}

// ---- met.no provider ----

// MetNoProvider uses api.met.no's locationforecast/2.0/compact endpoint.
// No API key, but MUST send a User-Agent identifying the app — they block
// defaults. See https://api.met.no/doc/TermsOfService
type MetNoProvider struct {
	Client    *http.Client
	BaseURL   string
	UserAgent string
}

// NewMetNo returns a configured provider.
func NewMetNo(userAgent string) *MetNoProvider {
	if userAgent == "" {
		userAgent = "FTW github.com/srcfl/ftw"
	}
	return &MetNoProvider{
		Client:    &http.Client{Timeout: 15 * time.Second},
		BaseURL:   "https://api.met.no/weatherapi/locationforecast/2.0/compact",
		UserAgent: userAgent,
	}
}

func (m *MetNoProvider) Name() string { return "met_no" }

func (m *MetNoProvider) Fetch(ctx context.Context, lat, lon float64) ([]RawForecast, error) {
	url := fmt.Sprintf("%s?lat=%.4f&lon=%.4f", m.BaseURL, lat, lon)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil { return nil, err }
	req.Header.Set("User-Agent", m.UserAgent)
	resp, err := m.Client.Do(req)
	if err != nil { return nil, err }
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("met.no: status %d: %s", resp.StatusCode, string(body))
	}
	// Minimal decode — we need only the time and instant.details fields
	var doc struct {
		Properties struct {
			Timeseries []struct {
				Time string `json:"time"`
				Data struct {
					Instant struct {
						Details struct {
							CloudAreaFraction *float64 `json:"cloud_area_fraction"`
							AirTemperature    *float64 `json:"air_temperature"`
						} `json:"details"`
					} `json:"instant"`
				} `json:"data"`
			} `json:"timeseries"`
		} `json:"properties"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("met.no: decode: %w", err)
	}
	out := make([]RawForecast, 0, len(doc.Properties.Timeseries))
	for _, ts := range doc.Properties.Timeseries {
		t, err := time.Parse(time.RFC3339, ts.Time)
		if err != nil { continue }
		// Snap to UTC hour boundary (met.no returns exact hours in UTC)
		t = t.UTC().Truncate(time.Hour)
		out = append(out, RawForecast{
			HourStart:     t,
			CloudCoverPct: ts.Data.Instant.Details.CloudAreaFraction,
			TempC:         ts.Data.Instant.Details.AirTemperature,
		})
	}
	return out, nil
}

// ---- OpenWeatherMap provider ----

// OpenWeatherProvider uses the One Call 3.0 API (paid tier for forecasts
// beyond 48h, free tier covers 48h hourly). Key required.
type OpenWeatherProvider struct {
	Client  *http.Client
	APIKey  string
	BaseURL string
}

// NewOpenWeather returns a configured provider.
func NewOpenWeather(apiKey string) *OpenWeatherProvider {
	return &OpenWeatherProvider{
		Client:  &http.Client{Timeout: 15 * time.Second},
		APIKey:  apiKey,
		BaseURL: "https://api.openweathermap.org/data/3.0/onecall",
	}
}

func (o *OpenWeatherProvider) Name() string { return "openweather" }

func (o *OpenWeatherProvider) Fetch(ctx context.Context, lat, lon float64) ([]RawForecast, error) {
	if o.APIKey == "" {
		return nil, fmt.Errorf("openweather: API key required")
	}
	url := fmt.Sprintf("%s?lat=%.4f&lon=%.4f&exclude=current,minutely,daily,alerts&units=metric&appid=%s",
		o.BaseURL, lat, lon, o.APIKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil { return nil, err }
	resp, err := o.Client.Do(req)
	if err != nil { return nil, err }
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("openweather: status %d: %s", resp.StatusCode, string(body))
	}
	var doc struct {
		Hourly []struct {
			Dt    int64   `json:"dt"`
			Temp  float64 `json:"temp"`
			Clouds float64 `json:"clouds"` // %
		} `json:"hourly"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("openweather: decode: %w", err)
	}
	out := make([]RawForecast, 0, len(doc.Hourly))
	for _, h := range doc.Hourly {
		temp := h.Temp
		cloud := h.Clouds
		out = append(out, RawForecast{
			HourStart:     time.Unix(h.Dt, 0).UTC(),
			CloudCoverPct: &cloud,
			TempC:         &temp,
		})
	}
	return out, nil
}

// ---- Clear-sky + PV estimation ----

// ClearSkyW estimates direct beam solar irradiance on a horizontal surface
// at (lat, lon, t) in W/m². Uses a simplified Bird-like model — geometric
// solar elevation angle × atmospheric attenuation.
//
// This is NOT a research-grade irradiance model. It's good enough to scale
// a panel-rated-kW into expected PV output for a given time of day and year,
// which combined with cloud cover gives directional forecasts that MPC /
// pre-charge scheduling can use.
func ClearSkyW(lat, lon float64, t time.Time) float64 {
	// Day of year
	doy := float64(t.YearDay())
	// Solar declination (degrees)
	decl := 23.45 * math.Sin(2*math.Pi*(284+doy)/365)
	// Hour angle at local solar time. We approximate local solar time from UTC + longitude.
	utc := t.UTC()
	hourFloat := float64(utc.Hour()) + float64(utc.Minute())/60.0
	solarHour := hourFloat + lon/15.0
	hourAngle := (solarHour - 12) * 15 // degrees
	// Solar elevation (degrees)
	latR := lat * math.Pi / 180
	declR := decl * math.Pi / 180
	haR := hourAngle * math.Pi / 180
	sinElev := math.Sin(latR)*math.Sin(declR) + math.Cos(latR)*math.Cos(declR)*math.Cos(haR)
	if sinElev <= 0 {
		return 0 // below horizon
	}
	// Airmass (Kasten-Young approximation)
	zenith := math.Acos(sinElev) // radians
	zenithDeg := zenith * 180 / math.Pi
	airmass := 1 / (math.Cos(zenith) + 0.50572*math.Pow(96.07995-zenithDeg, -1.6364))
	// Direct normal irradiance attenuated by airmass (very simplified)
	dni := 1367 * math.Pow(0.7, math.Pow(airmass, 0.678))
	// Global horizontal ≈ DNI × cos(zenith) + some diffuse. We just use cos component.
	ghi := dni * sinElev
	if ghi < 0 { ghi = 0 }
	return ghi
}

// EstimatePVW derives expected PV power (W) from:
//   - clear_sky irradiance at (lat, lon, t)
//   - cloud cover pct (if known; treated as 50 if nil)
//   - rated PV output (peak W at STC = 1000 W/m² irradiance)
//
// Formula: pv_w ≈ rated_w × (clear_sky_w / 1000) × (1 − cloud/100)^1.5
func EstimatePVW(lat, lon float64, t time.Time, cloudPct *float64, ratedW float64) float64 {
	if ratedW <= 0 { return 0 }
	cs := ClearSkyW(lat, lon, t)
	if cs <= 0 { return 0 }
	cloud := 50.0
	if cloudPct != nil { cloud = *cloudPct }
	if cloud < 0 { cloud = 0 }
	if cloud > 100 { cloud = 100 }
	// Non-linear: heavy clouds attenuate more than linear
	cloudFactor := math.Pow(1-cloud/100.0, 1.5)
	return ratedW * (cs / 1000.0) * cloudFactor
}

// ---- Service ----

// Service wraps a provider + store + scheduler for forecasts.
type Service struct {
	Provider   Provider
	Store      *state.Store
	Lat, Lon   float64
	RatedPVW   float64 // total rated PV across all arrays (used for estimate)

	stop chan struct{}
	done chan struct{}
}

// FromConfig builds a Service from the weather config section. ratedPVW is
// computed from the sum of driver PV capacities in the main config.
func FromConfig(cfg *config.Weather, ratedPVW float64, st *state.Store, userAgent string) *Service {
	if cfg == nil || cfg.Provider == "" || cfg.Provider == "none" {
		return nil
	}
	var p Provider
	switch cfg.Provider {
	case "met_no":
		p = NewMetNo(userAgent)
	case "openweather":
		p = NewOpenWeather(cfg.APIKey)
	case "open_meteo":
		p = NewOpenMeteo()
	case "forecast_solar":
		// Prefer the multi-array config when set. Falls back to a
		// single synthesized array from the legacy flat fields so
		// existing deploys on forecast_solar don't need to re-enter
		// their geometry just because the config model grew.
		var arrays []Array
		for _, a := range cfg.PVArrays {
			arrays = append(arrays, Array{
				TiltDeg: a.TiltDeg, AzimuthDeg: a.AzimuthDeg, KWp: a.KWp,
			})
		}
		if len(arrays) == 0 {
			arrays = append(arrays, Array{
				TiltDeg: cfg.PVTiltDeg, AzimuthDeg: cfg.PVAzimuthDeg, KWp: ratedPVW / 1000.0,
			})
		}
		p = NewForecastSolarMulti(arrays)
	default:
		return nil
	}
	return &Service{
		Provider: p, Store: st,
		Lat: cfg.Latitude, Lon: cfg.Longitude,
		RatedPVW: ratedPVW,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start begins periodic fetches (every 3 hours).
func (s *Service) Start(ctx context.Context) {
	go s.loop(ctx)
}

// Stop terminates the fetcher.
func (s *Service) Stop() {
	close(s.stop)
	<-s.done
}

func (s *Service) loop(ctx context.Context) {
	defer close(s.done)
	s.fetchAndStore(ctx)
	t := time.NewTicker(3 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			s.fetchAndStore(ctx)
		}
	}
}

func (s *Service) fetchAndStore(ctx context.Context) {
	rows, err := s.Provider.Fetch(ctx, s.Lat, s.Lon)
	if err != nil {
		slog.Warn("forecast fetch failed", "err", err, "provider", s.Provider.Name())
		return
	}
	if len(rows) == 0 { return }
	nowMs := time.Now().UnixMilli()
	points := make([]state.ForecastPoint, 0, len(rows))
	for _, r := range rows {
		// Pick the most direct PV signal the provider gave us. Forecast.Solar
		// returns site-calibrated watts directly; Open-Meteo returns shortwave
		// radiation we turn into watts via rated × W/m²/1000; met.no only has
		// cloud fraction, so we fall through to the naive cloud-derated prior.
		var pvW float64
		switch {
		case r.PVWEstimated != nil:
			pvW = *r.PVWEstimated
		case r.SolarWm2 != nil && s.RatedPVW > 0:
			pvW = s.RatedPVW * (*r.SolarWm2) / 1000.0
		default:
			pvW = EstimatePVW(s.Lat, s.Lon, r.HourStart, r.CloudCoverPct, s.RatedPVW)
		}
		pvPtr := &pvW
		points = append(points, state.ForecastPoint{
			SlotTsMs:      r.HourStart.UnixMilli(),
			SlotLenMin:    60,
			CloudCoverPct: r.CloudCoverPct,
			TempC:         r.TempC,
			SolarWm2:      r.SolarWm2,
			PVWEstimated:  pvPtr,
			Source:        s.Provider.Name(),
			FetchedAtMs:   nowMs,
		})
	}
	if err := s.Store.SaveForecasts(points); err != nil {
		slog.Warn("forecast save failed", "err", err)
		return
	}
	slog.Info("forecast fetched", "count", len(points), "provider", s.Provider.Name())
}

// Load returns forecasts in [sinceMs, untilMs].
func (s *Service) Load(sinceMs, untilMs int64) ([]state.ForecastPoint, error) {
	return s.Store.LoadForecasts(sinceMs, untilMs)
}
