// Package pvperf scores measured PV production against a weather-expected
// baseline derived from SMHI STRÅNG irradiance and the site's per-plane
// geometry.
//
// It is read-only analytics with no control-path coupling: given historical
// irradiance and the panel arrays, it computes the DC energy the arrays should
// have produced under that irradiance, then compares to the measured energy to
// yield a performance ratio (PR). A sustained PR below ~1 hints at soiling,
// snow, shading or degradation; the raw PR series is the signal, the diagnosis
// is left to the caller/UI.
//
// The expected baseline deliberately omits system losses (inverter, wiring,
// temperature). The performance ratio absorbs the site's fixed derate, so a
// healthy site simply sits at a stable PR < 1 and anomalies show as departures
// from that baseline.
package pvperf

import (
	"time"

	"github.com/srcfl/ftw/go/internal/sunpos"
	"github.com/srcfl/ftw/go/internal/units"
)

// Array is one panel plane: nameplate DC watts at a tilt/azimuth (same
// conventions as sunpos — tilt 0=flat..90=wall, azimuth 0=N,90=E,180=S,270=W).
type Array struct {
	RatedW     float64
	TiltDeg    float64
	AzimuthDeg float64
}

// Irradiance is one hour of horizontal irradiance (W/m²). DHIWm2 is nil when
// the diffuse component is unavailable, in which case an Erbs split is used.
type Irradiance struct {
	HourStart time.Time
	GHIWm2    float64
	DHIWm2    *float64
}

// ExpectedWh returns the DC energy (Wh, positive) the given arrays would
// produce under the supplied hourly irradiance at (lat, lon). Each hour's
// plane-of-array irradiance is projected via sunpos and integrated over one
// hour. When measured diffuse is present it is used directly (more accurate);
// otherwise the diffuse fraction is estimated from the clearness index.
func ExpectedWh(lat, lon float64, arrays []Array, hours []Irradiance) float64 {
	var wh float64
	for _, h := range hours {
		if h.GHIWm2 <= 0 {
			continue
		}
		sun := sunpos.At(h.HourStart, lat, lon)
		for _, a := range arrays {
			if a.RatedW <= 0 {
				continue
			}
			var poa float64
			if h.DHIWm2 != nil {
				poa = sunpos.POAFromComponents(sun, h.GHIWm2, *h.DHIWm2, a.TiltDeg, a.AzimuthDeg)
			} else {
				poa = sunpos.POAFromGHI(h.HourStart, lat, lon, h.GHIWm2, a.TiltDeg, a.AzimuthDeg)
			}
			// Watts at STC × (POA / 1000 W/m²) × 1 h = Wh this hour.
			wh += units.PVFromIrradiance(a.RatedW, poa)
		}
	}
	return wh
}

// minExpectedWh is the floor below which a performance ratio is meaningless
// (polar-night / near-dark days) — reported as "not available" instead.
const minExpectedWh = 100.0

// PerformanceRatio returns actualWh / expectedWh, clamped to [0, 2]. The second
// return is false when expected production is too small to form a meaningful
// ratio, so callers can render "n/a" rather than a divide-by-tiny artifact.
func PerformanceRatio(expectedWh, actualWh float64) (float64, bool) {
	if expectedWh < minExpectedWh {
		return 0, false
	}
	pr := actualWh / expectedWh
	if pr < 0 {
		pr = 0
	}
	if pr > 2 {
		pr = 2
	}
	return pr, true
}
