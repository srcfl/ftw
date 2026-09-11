package forecast

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"time"
)

// OpenMeteoProvider uses Open-Meteo's forecast API to fetch hourly
// shortwave solar irradiance alongside cloud cover and air temperature.
// Free, no key required, ECMWF-driven; covers 16 days ahead.
//
// The radiation signal is materially better input for PV prediction
// than cloud_area_fraction alone because "49% cloud in the sky" can
// mean anything from full sun to overcast depending on which part of
// the sky the Sun is behind — radiation measures what actually reaches
// a horizontal surface. The downstream PV derivation projects this
// irradiance onto each complete `weather.pv_arrays` plane when present,
// and uses the safe flat `rated × (W/m² / 1000)` estimate otherwise.
// Forecast.Solar remains site-calibrated server-side; Open-Meteo uses the
// configured arrays directly rather than ignoring them.
type OpenMeteoProvider struct {
	Client  *http.Client
	BaseURL string
}

// NewOpenMeteo returns a configured provider. No API key — Open-Meteo
// is free under a fair-use policy; the existing 3-hour refresh loop in
// forecast.Service.loop is well inside the limits.
func NewOpenMeteo() *OpenMeteoProvider {
	return &OpenMeteoProvider{
		Client:  &http.Client{Timeout: 15 * time.Second},
		BaseURL: "https://api.open-meteo.com/v1/forecast",
	}
}

// Name implements Provider.
func (o *OpenMeteoProvider) Name() string { return "open_meteo" }

// Fetch implements Provider. Returns one RawForecast per UTC hour for
// the next ~16 days with SolarWm2 + CloudCoverPct + TempC populated.
func (o *OpenMeteoProvider) Fetch(ctx context.Context, lat, lon float64) ([]RawForecast, error) {
	url := fmt.Sprintf(
		"%s?latitude=%.4f&longitude=%.4f&hourly=shortwave_radiation,cloud_cover,temperature_2m&timezone=UTC&forecast_days=16",
		o.BaseURL, lat, lon,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := o.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("open-meteo: status %d: %s", resp.StatusCode, string(body))
	}
	var doc struct {
		Hourly struct {
			Time               []string          `json:"time"`
			ShortwaveRadiation []json.RawMessage `json:"shortwave_radiation"` // W/m²
			CloudCover         []json.RawMessage `json:"cloud_cover"`         // %
			Temperature2m      []json.RawMessage `json:"temperature_2m"`      // °C
		} `json:"hourly"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("open-meteo: decode: %w", err)
	}
	type sample struct {
		at         time.Time
		radiation  *float64
		cloudCover *float64
		tempC      *float64
	}
	samples := make([]sample, 0, len(doc.Hourly.Time))
	for i, ts := range doc.Hourly.Time {
		// Open-Meteo returns naive local times per the &timezone= param;
		// with &timezone=UTC they're UTC-zoned ISO8601 without offset.
		t, err := parseOpenMeteoTime(ts)
		if err != nil {
			continue
		}
		s := sample{at: t}
		if i < len(doc.Hourly.ShortwaveRadiation) {
			s.radiation = parseOpenMeteoNumber(doc.Hourly.ShortwaveRadiation[i], func(v float64) bool { return v >= 0 })
		}
		if i < len(doc.Hourly.CloudCover) {
			s.cloudCover = parseOpenMeteoNumber(doc.Hourly.CloudCover[i], func(v float64) bool { return v >= 0 && v <= 100 })
		}
		if i < len(doc.Hourly.Temperature2m) {
			s.tempC = parseOpenMeteoNumber(doc.Hourly.Temperature2m[i], func(float64) bool { return true })
		}
		samples = append(samples, s)
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i].at.Before(samples[j].at) })

	// Open-Meteo timestamps shortwave_radiation at the end of the hour it
	// averages. Normalize it to that interval's start. Cloud cover and air
	// temperature are instantaneous, so align them to the same hour with a
	// trapezoidal mean of the two endpoints. If an endpoint is absent, leave
	// that field absent instead of moving one instant to another hour.
	out := make([]RawForecast, 0, len(samples))
	byTime := make(map[int64]sample, len(samples))
	for _, s := range samples {
		byTime[s.at.UnixMilli()] = s
	}
	for _, end := range samples {
		startTime := end.at.Add(-time.Hour)
		row := RawForecast{HourStart: startTime}
		row.SolarWm2 = end.radiation
		if start, ok := byTime[startTime.UnixMilli()]; ok {
			row.CloudCoverPct = meanOpenMeteoEndpoints(start.cloudCover, end.cloudCover)
			row.TempC = meanOpenMeteoEndpoints(start.tempC, end.tempC)
		}
		if row.SolarWm2 != nil || row.CloudCoverPct != nil || row.TempC != nil {
			out = append(out, row)
		}
	}
	return out, nil
}

func parseOpenMeteoTime(value string) (time.Time, error) {
	if t, err := time.Parse("2006-01-02T15:04", value); err == nil {
		return t.UTC(), nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

func parseOpenMeteoNumber(raw json.RawMessage, valid func(float64) bool) *float64 {
	var value *float64
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil || value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || !valid(*value) {
		return nil
	}
	return value
}

func meanOpenMeteoEndpoints(start, end *float64) *float64 {
	if start == nil || end == nil {
		return nil
	}
	mean := (*start + *end) / 2
	if math.IsNaN(mean) || math.IsInf(mean, 0) {
		return nil
	}
	return &mean
}
