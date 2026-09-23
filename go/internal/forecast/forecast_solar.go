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

	"github.com/srcfl/ftw/go/internal/units"
)

// ForecastSolarProvider uses the free tier of api.forecast.solar, which
// combines the EU PVGIS clear-sky model with a weather forecast and
// returns site-specific PV output (watts) per timestamp for the next
// few days. No authentication required. Free-tier rate limit is
// 12 calls/hour over a rolling window; forecast.Service's 3-hour
// refresh loop uses a tiny fraction of that.
//
// Why it's better than cloud-fraction-only providers: the response is
// already calibrated for our site (lat/lon/tilt/azimuth/kWp are part
// of the URL), so the cloud_area_fraction → PV conversion that we
// approximate with (1 - cloud)^1.5 is done for us using their model.
// The RLS twin becomes a thin correction on top of an already-good
// prediction, not the sole calibration mechanism.
//
// Supports multi-plane installs (e.g. panels on south and east roofs)
// by passing multiple Arrays — forecast.solar returns the SUM across
// all planes, which is exactly what the MPC + UI want.
//
// Limitations:
//   - Doesn't return cloud cover or temperature; we set both nil so
//     downstream code falls back to whatever it uses when these are
//     absent (pvmodel: neutral 50%, fuse/thermal: no-op).
//   - The response is per-timestamp at irregular intervals (typically
//     minutely around dawn/dusk, then hourly). We split each preceding
//     period's energy across UTC hours.
type ForecastSolarProvider struct {
	Client  *http.Client
	BaseURL string

	// Arrays is the list of physically-distinct panel groups. Each has
	// its own tilt (0 = flat, 90 = wall), azimuth (compass heading,
	// 180 = south), and rated watts. One entry means single-array.
	Arrays []Array
}

// Array is one panel plane. RatedW is nameplate watts. kWp is derived
// only when building the forecast.solar URL.
type Array struct {
	TiltDeg    float64
	AzimuthDeg float64
	RatedW     float64
}

// NewForecastSolar returns a configured provider with a single panel
// array. For multi-plane installs, append additional entries to the
// Arrays field after construction. ratedW is watts.
func NewForecastSolar(tiltDeg, azimuthDeg, ratedW float64) *ForecastSolarProvider {
	return &ForecastSolarProvider{
		Client:  &http.Client{Timeout: 15 * time.Second},
		BaseURL: "https://api.forecast.solar",
		Arrays:  []Array{{TiltDeg: tiltDeg, AzimuthDeg: azimuthDeg, RatedW: ratedW}},
	}
}

// NewForecastSolarMulti returns a provider configured for a multi-plane
// install (e.g. south + east roofs). The order of arrays determines the
// order in the URL path — forecast.solar sums their outputs regardless.
func NewForecastSolarMulti(arrays []Array) *ForecastSolarProvider {
	return &ForecastSolarProvider{
		Client:  &http.Client{Timeout: 15 * time.Second},
		BaseURL: "https://api.forecast.solar",
		Arrays:  arrays,
	}
}

// Name implements Provider.
func (f *ForecastSolarProvider) Name() string { return "forecast_solar" }

// Fetch implements Provider. Returns one RawForecast per UTC hour with
// PVWEstimated populated; cloud/temperature stay nil.
func (f *ForecastSolarProvider) Fetch(ctx context.Context, lat, lon float64) ([]RawForecast, error) {
	if len(f.Arrays) == 0 {
		return nil, fmt.Errorf("forecast.solar: at least one array required")
	}
	for i, a := range f.Arrays {
		if math.IsNaN(a.RatedW) || math.IsInf(a.RatedW, 0) || a.RatedW <= 0 {
			return nil, fmt.Errorf("forecast.solar: array %d rated_w must be finite and > 0 (got %f W)", i, a.RatedW)
		}
		if math.IsNaN(a.TiltDeg) || math.IsInf(a.TiltDeg, 0) || a.TiltDeg < 0 || a.TiltDeg > 90 {
			return nil, fmt.Errorf("forecast.solar: array %d tilt_deg must be within 0..90 (got %f)", i, a.TiltDeg)
		}
		if math.IsNaN(a.AzimuthDeg) || math.IsInf(a.AzimuthDeg, 0) || a.AzimuthDeg < 0 || a.AzimuthDeg > 360 {
			return nil, fmt.Errorf("forecast.solar: array %d azimuth_deg must be within 0..360 (got %f)", i, a.AzimuthDeg)
		}
	}
	// Multi-plane URL syntax: /estimate/lat/lon/tilt1/az1/kwp1/tilt2/az2/kwp2/...
	// kWp is this vendor's unit — convert at this door only.
	var planes string
	for _, a := range f.Arrays {
		// FTW uses compass headings (north=0, east=90, south=180,
		// west=270). forecast.solar uses south=0, east=-90, west=90.
		vendorAzimuth := a.AzimuthDeg - 180
		planes += fmt.Sprintf("/%.1f/%.1f/%.2f", a.TiltDeg, vendorAzimuth, units.KWpFromWatts(a.RatedW))
	}
	url := fmt.Sprintf("%s/estimate/%.4f/%.4f%s?time=utc", f.BaseURL, lat, lon, planes)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 429 {
		return nil, fmt.Errorf("forecast.solar: rate limited (429); reset at %s",
			resp.Header.Get("X-Ratelimit-Reset"))
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("forecast.solar: status %d: %s", resp.StatusCode, string(body))
	}
	var doc struct {
		Result struct {
			Watts map[string]json.RawMessage `json:"watts"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("forecast.solar: decode: %w", err)
	}

	// Each watts value is the mean for the period from the previous
	// timestamp to its own timestamp. Split that energy by overlap with UTC
	// hours, then divide by one hour to produce energy-preserving hourly
	// means. This also handles the short periods around sunrise and sunset.
	type sample struct {
		at    time.Time
		watts *float64
	}
	samples := make([]sample, 0, len(doc.Result.Watts))
	for tsStr, raw := range doc.Result.Watts {
		t, err := parseForecastSolarTime(tsStr)
		if err != nil {
			continue
		}
		var value *float64
		if json.Unmarshal(raw, &value) != nil || value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 {
			value = nil
		}
		samples = append(samples, sample{at: t, watts: value})
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i].at.Before(samples[j].at) })

	energyWh := make(map[int64]float64, len(samples))
	for i := 1; i < len(samples); i++ {
		start, end := samples[i-1].at, samples[i].at
		if !end.After(start) || end.Sub(start) > 32*24*time.Hour || samples[i].watts == nil {
			continue
		}
		for cursor := start; cursor.Before(end); {
			hourStart := cursor.Truncate(time.Hour)
			overlapEnd := hourStart.Add(time.Hour)
			if end.Before(overlapEnd) {
				overlapEnd = end
			}
			energyWh[hourStart.UnixMilli()] += *samples[i].watts * overlapEnd.Sub(cursor).Hours()
			cursor = overlapEnd
		}
	}

	hours := make([]int64, 0, len(energyWh))
	for hourMs := range energyWh {
		hours = append(hours, hourMs)
	}
	sort.Slice(hours, func(i, j int) bool { return hours[i] < hours[j] })
	out := make([]RawForecast, 0, len(hours))
	for _, hourMs := range hours {
		w := energyWh[hourMs]
		out = append(out, RawForecast{
			HourStart:    time.UnixMilli(hourMs).UTC(),
			PVWEstimated: &w,
		})
	}
	return out, nil
}

func parseForecastSolarTime(value string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t.UTC(), nil
	}
	// Keep support for time_tz=0 and older fixture responses. The request
	// asks for UTC, so these offset-free timestamps are UTC too.
	t, err := time.Parse("2006-01-02 15:04:05", value)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}
