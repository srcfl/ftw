package forecast

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
			Time               []string   `json:"time"`
			ShortwaveRadiation []*float64 `json:"shortwave_radiation"` // W/m²
			CloudCover         []*float64 `json:"cloud_cover"`         // %
			Temperature2m      []*float64 `json:"temperature_2m"`      // °C
		} `json:"hourly"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("open-meteo: decode: %w", err)
	}
	n := len(doc.Hourly.Time)
	out := make([]RawForecast, 0, n)
	for i := 0; i < n; i++ {
		// Open-Meteo returns naive local times per the &timezone= param;
		// with &timezone=UTC they're UTC-zoned ISO8601 without offset.
		t, err := time.Parse("2006-01-02T15:04", doc.Hourly.Time[i])
		if err != nil {
			continue
		}
		t = t.UTC()
		row := RawForecast{HourStart: t}
		if i < len(doc.Hourly.CloudCover) {
			row.CloudCoverPct = doc.Hourly.CloudCover[i]
		}
		if i < len(doc.Hourly.Temperature2m) {
			row.TempC = doc.Hourly.Temperature2m[i]
		}
		if i < len(doc.Hourly.ShortwaveRadiation) {
			row.SolarWm2 = doc.Hourly.ShortwaveRadiation[i]
		}
		out = append(out, row)
	}
	return out, nil
}
