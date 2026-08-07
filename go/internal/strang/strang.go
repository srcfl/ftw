// Package strang fetches historical solar irradiance from SMHI's STRÅNG
// mesoscale model (https://strang.smhi.se/).
//
// STRÅNG is an analysis/reanalysis product: it covers the Nordic region hourly
// at ~2.5 km resolution from 1999 to ~1 day ago. It has NO forward horizon, so
// it is used here for historical PV-performance scoring and model calibration,
// never as a forward forecast provider (those stay in the forecast package).
//
// Data is free and licensed CC BY 4.0 — attribution to SMHI required. No API
// key. The public point time-series endpoint is:
//
//	{base}/geotype/point/lon/{lon}/lat/{lat}/parameter/{p}/data.json?from=&to=&interval=hourly
package strang

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/srcfl/ftw/go/internal/coverage"
	"github.com/srcfl/ftw/go/internal/sunpos"
)

// ErrOutsideDomain is returned when a request falls outside STRÅNG's Nordic
// model domain. Callers should treat it as "this site can never be scored by
// STRÅNG" and stop asking, rather than as a transient failure to retry.
var ErrOutsideDomain = errors.New("outside STRÅNG domain")

// STRÅNG parameter codes (category "strang1g", version 1).
//
// These are the complete set — probing 100..130 against the live API on
// 2026-07-31 returned 200 for exactly 116..122 and 404 for everything else.
// Names were confirmed by their magnitudes on a clear day at Stockholm rather
// than from documentation, since SMHI's apidocs pages currently 404: parameter
// 119 caps at exactly 60 (minutes in an hour), 118 exceeds 117 the way direct
// *normal* irradiance exceeds global, and at solar noon 121 + 122 = 723.0 +
// 87.5 = 810.5, which is exactly 117 — the direct-plus-diffuse identity that
// tells us 121 is direct *horizontal* and not something else.
//
// Note what is absent: STRÅNG models radiation only and publishes no cloud
// cover. See CloudCover for how cloudiness is recovered from parameter 119.
const (
	ParamCIEUV             = 116 // CIE-weighted UV irradiance, mW/m²
	ParamGlobalIrradiance  = 117 // Global (horizontal) irradiance, W/m² — GHI
	ParamDirectNormal      = 118 // Direct normal irradiance, W/m² — DNI
	ParamSunshineDuration  = 119 // Sunshine duration within the hour, minutes 0..60
	ParamPAR               = 120 // Photosynthetically active radiation, W/m²
	ParamDirectHorizontal  = 121 // Direct horizontal irradiance, W/m²
	ParamDiffuseIrradiance = 122 // Diffuse (horizontal) irradiance, W/m² — DHI
)

// minutesPerHour is the full-sun value of ParamSunshineDuration.
const minutesPerHour = 60.0

// DefaultBaseURL is SMHI's open-data meteorological-analysis host for STRÅNG.
const DefaultBaseURL = "https://opendata-download-metanalys.smhi.se/api/category/strang1g/version/1"

// IrradianceHour is one hour of horizontal irradiance at a point. DHIWm2 is nil
// when the diffuse component is unavailable (e.g. windows before 2017-04-18, or
// a transient diffuse-parameter error) — callers then estimate the split.
// SunshineMin is nil on the same terms and carries parameter 119.
type IrradianceHour struct {
	HourStart   time.Time
	GHIWm2      float64
	DHIWm2      *float64
	SunshineMin *float64
}

// CloudCover derives a cloud-cover fraction (0 = clear, 1 = overcast) for the
// hour at (lat, lon), and reports whether it could be derived at all.
//
// STRÅNG publishes no cloud-cover parameter, but parameter 119 is sunshine
// duration: the number of minutes in the hour during which direct beam
// irradiance exceeded the WMO sunshine threshold (120 W/m²). One minus that
// fraction is the share of the hour the sun spent obscured, which is what
// "cloud cover" means for a solar model's purposes.
//
// This is an *observed* quantity, not an inference from a cloud field, so it is
// better grounded than a forecast provider's cloud percentage. It is coarser in
// one direction: it cannot see thin cirrus that dims without blocking.
//
// The location is required because sunshine duration is zero at night for the
// trivial reason that there is no sun — reading that as "100% overcast" would be
// confidently wrong every single night. When the sun is below the horizon for
// the whole hour this returns not-ok, and callers must treat that as unknown
// rather than as clear or as overcast.
func (h IrradianceHour) CloudCover(lat, lon float64) (float64, bool) {
	if h.SunshineMin == nil {
		return 0, false
	}
	if !h.daylight(lat, lon) {
		return 0, false
	}
	m := *h.SunshineMin
	if m < 0 {
		return 0, false
	}
	if m > minutesPerHour {
		m = minutesPerHour
	}
	return 1 - m/minutesPerHour, true
}

// minSunElevationDeg is how high the sun must get during the hour before a
// sunshine-duration reading says anything about cloud.
//
// The WMO sunshine threshold is 120 W/m² of direct beam. Near the horizon the
// beam crosses roughly ten or more air masses and cannot reach that threshold
// even under a spotless sky, so a zero reading there is a statement about
// geometry, not about cloud. Five degrees is where the beam can plausibly clear
// the threshold; below it we decline to answer rather than report a twilight
// hour as fully overcast.
const minSunElevationDeg = 5.0

// daylight reports whether the sun climbs above minSunElevationDeg at any point
// in the hour. Sampling start, middle and end catches the sunrise and sunset
// hours, where the midpoint alone would misclassify half the hour.
func (h IrradianceHour) daylight(lat, lon float64) bool {
	for _, off := range []time.Duration{0, 30 * time.Minute, 59 * time.Minute} {
		if sunpos.At(h.HourStart.Add(off), lat, lon).ZenithDeg < 90-minSunElevationDeg {
			return true
		}
	}
	return false
}

// Client is a thin STRÅNG point-series HTTP client.
type Client struct {
	HTTP      *http.Client
	BaseURL   string
	UserAgent string
}

// NewClient returns a Client with sane defaults. A descriptive User-Agent is
// required by SMHI's fair-use policy, mirroring the forecast providers.
func NewClient(userAgent string) *Client {
	if userAgent == "" {
		userAgent = "FTW github.com/srcfl/ftw"
	}
	return &Client{
		HTTP:      &http.Client{Timeout: 30 * time.Second},
		BaseURL:   DefaultBaseURL,
		UserAgent: userAgent,
	}
}

// FetchWindow returns hourly irradiance for [start, end] (dates, UTC) at
// (lat, lon). Global irradiance is required; diffuse is best-effort — a diffuse
// error (common for pre-2017 windows) leaves DHIWm2 nil rather than failing the
// whole window. Rows are returned ascending by hour.
func (c *Client) FetchWindow(ctx context.Context, lat, lon float64, start, end time.Time) ([]IrradianceHour, error) {
	if !coverage.Covers("strang", lat, lon) {
		return nil, fmt.Errorf("strang: %w: (%.4f, %.4f) is outside the Nordic model domain", ErrOutsideDomain, lat, lon)
	}
	ghi, err := c.fetchParam(ctx, lat, lon, ParamGlobalIrradiance, start, end)
	if err != nil {
		return nil, fmt.Errorf("strang: global irradiance: %w", err)
	}
	// Best-effort extras: never fail the window because a secondary parameter
	// errored. Both leave their field nil and callers fall back — the diffuse
	// split is estimated, and cloud cover simply reports unknown.
	dhi, _ := c.fetchParam(ctx, lat, lon, ParamDiffuseIrradiance, start, end)
	sun, _ := c.fetchParam(ctx, lat, lon, ParamSunshineDuration, start, end)

	hours := make([]int64, 0, len(ghi))
	for ms := range ghi {
		hours = append(hours, ms)
	}
	sort.Slice(hours, func(i, j int) bool { return hours[i] < hours[j] })

	out := make([]IrradianceHour, 0, len(hours))
	for _, ms := range hours {
		h := IrradianceHour{HourStart: time.UnixMilli(ms).UTC(), GHIWm2: ghi[ms]}
		if d, ok := dhi[ms]; ok {
			dv := d
			h.DHIWm2 = &dv
		}
		if s, ok := sun[ms]; ok {
			sv := s
			h.SunshineMin = &sv
		}
		out = append(out, h)
	}
	return out, nil
}

// fetchParam returns hour-start-ms → value for one STRÅNG parameter.
func (c *Client) fetchParam(ctx context.Context, lat, lon float64, param int, start, end time.Time) (map[int64]float64, error) {
	url := fmt.Sprintf("%s/geotype/point/lon/%.4f/lat/%.4f/parameter/%d/data.json?from=%s&to=%s&interval=hourly",
		c.BaseURL, lon, lat, param,
		start.UTC().Format("2006-01-02"), end.UTC().Format("2006-01-02"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}
	// STRÅNG data.json is an array of {date_time, value}.
	var doc []struct {
		DateTime string   `json:"date_time"`
		Value    *float64 `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	out := make(map[int64]float64, len(doc))
	for _, d := range doc {
		if d.Value == nil {
			continue
		}
		t, err := time.Parse(time.RFC3339, d.DateTime)
		if err != nil {
			continue
		}
		out[t.UTC().Truncate(time.Hour).UnixMilli()] = *d.Value
	}
	return out, nil
}
