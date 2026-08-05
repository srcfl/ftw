// Package sunpos computes solar position (zenith/azimuth) and plane-of-array
// irradiance for arbitrary panel orientations. Physics-only, no fitted
// constants — used as the prior for the data-driven PV twin.
//
// Reference: Reda & Andreas (2003), "Solar Position Algorithm for Solar
// Radiation Applications", NREL/TP-560-34302. We use the simplified
// SPENCER (1971) form which is accurate to ~0.05° and avoids planetary
// ephemeris tables — perfect for an embedded EMS that just needs the
// shape of the day right.
package sunpos

import (
	"math"
	"time"
)

// Position is the apparent location of the sun seen from one point on Earth.
//
//	Zenith  = 0° → sun directly overhead, 90° → at horizon, >90° → below.
//	Azimuth = 0° → north, 90° → east, 180° → south, 270° → west (clockwise).
type Position struct {
	ZenithDeg  float64
	AzimuthDeg float64
}

// At returns the sun position at time t for an observer at (lat, lon).
// Time is interpreted in UTC. lat in degrees north, lon in degrees east.
func At(t time.Time, lat, lon float64) Position {
	// Day of year as fractional value (used by Spencer's series).
	utc := t.UTC()
	doy := float64(utc.YearDay())
	hour := float64(utc.Hour()) + float64(utc.Minute())/60 + float64(utc.Second())/3600

	// Spencer (1971) Fourier expansion of solar declination + eqn-of-time.
	// gamma = fractional year in radians.
	gamma := 2 * math.Pi * (doy - 1 + (hour-12)/24) / 365

	// Equation of time (minutes): correction for orbital eccentricity.
	eqt := 229.18 * (0.000075 +
		0.001868*math.Cos(gamma) - 0.032077*math.Sin(gamma) -
		0.014615*math.Cos(2*gamma) - 0.040849*math.Sin(2*gamma))

	// Solar declination (radians).
	decl := 0.006918 -
		0.399912*math.Cos(gamma) + 0.070257*math.Sin(gamma) -
		0.006758*math.Cos(2*gamma) + 0.000907*math.Sin(2*gamma) -
		0.002697*math.Cos(3*gamma) + 0.00148*math.Sin(3*gamma)

	// Solar time (minutes): UTC clock + longitude offset + EoT.
	timeOffset := eqt + 4*lon
	tst := hour*60 + timeOffset

	// Hour angle: 0 at solar noon, +15° per hour east → west.
	ha := (tst/4 - 180) * math.Pi / 180

	latR := lat * math.Pi / 180

	// Zenith.
	cosZ := math.Sin(latR)*math.Sin(decl) + math.Cos(latR)*math.Cos(decl)*math.Cos(ha)
	if cosZ > 1 { cosZ = 1 }
	if cosZ < -1 { cosZ = -1 }
	zenith := math.Acos(cosZ)

	// Azimuth via atan2.
	// Standard form (NOAA): atan2(-sin(ha), tan(decl)*cos(lat) - sin(lat)*cos(ha))
	// returns azimuth measured from NORTH clockwise. At solar noon (ha=0) in
	// the northern hemisphere with sun south of the observer, the
	// denominator is negative → atan2 returns π → 180°. Morning (ha<0) →
	// num positive → az 90°-180° (sun in east). Afternoon → 180°-270°.
	sinHa := math.Sin(ha)
	num := -sinHa
	den := math.Tan(decl)*math.Cos(latR) - math.Sin(latR)*math.Cos(ha)
	az := math.Atan2(num, den)
	if az < 0 { az += 2 * math.Pi }

	return Position{
		ZenithDeg:  zenith * 180 / math.Pi,
		AzimuthDeg: az * 180 / math.Pi,
	}
}

// AOI returns the angle of incidence between the sun's rays and the normal
// of a panel with the given (tiltDeg, azimuthDeg). Used to project DNI on
// the plane of array.
//
// tilt:  0° = horizontal, 90° = vertical
// az:    0° = north-facing, 90° = east, 180° = south, 270° = west
//
// Returns AOI in degrees in [0, 180]. AOI > 90° means sun is behind panel.
func AOI(sun Position, panelTiltDeg, panelAzDeg float64) float64 {
	zR := sun.ZenithDeg * math.Pi / 180
	sR := sun.AzimuthDeg * math.Pi / 180
	tR := panelTiltDeg * math.Pi / 180
	pR := panelAzDeg * math.Pi / 180
	cosAOI := math.Cos(zR)*math.Cos(tR) +
		math.Sin(zR)*math.Sin(tR)*math.Cos(sR-pR)
	if cosAOI > 1 { cosAOI = 1 }
	if cosAOI < -1 { cosAOI = -1 }
	return math.Acos(cosAOI) * 180 / math.Pi
}

// ClearSkyW returns extraterrestrial irradiance on a horizontal surface
// in W/m² (the "what the sun could deliver if there was no atmosphere").
// Multiplied by an atmospheric transmissivity factor (0.7 default) to
// approximate ground-level clear-sky GHI. Used as the prior signal for
// the PV twin.
//
// Returns 0 when the sun is below the horizon.
func ClearSkyW(t time.Time, lat, lon float64) float64 {
	sun := At(t, lat, lon)
	if sun.ZenithDeg >= 90 {
		return 0
	}
	// Solar constant adjusted for orbital distance (Spencer 1971).
	doy := float64(t.UTC().YearDay())
	gamma := 2 * math.Pi * (doy - 1) / 365
	e0 := 1.000110 +
		0.034221*math.Cos(gamma) + 0.001280*math.Sin(gamma) +
		0.000719*math.Cos(2*gamma) + 0.000077*math.Sin(2*gamma)
	const I0 = 1361.0 // solar constant W/m²
	dni := I0 * e0
	// Atmospheric transmissivity (Bird simple model uses ~0.75 average).
	const tau = 0.7
	return dni * tau * math.Cos(sun.ZenithDeg*math.Pi/180)
}

// POA estimates plane-of-array irradiance for one tilted panel using the
// isotropic-sky model, driven by the package's own clear-sky prior. It splits
// that clear-sky horizontal irradiance into beam (DNI) and diffuse (DHI)
// components with a fixed 20% diffuse fraction, then projects each onto the
// panel. Used as the prior signal for the PV twin when no measured irradiance
// is available.
//
// Returns W/m² on the panel surface; clamped to ≥ 0.
//
// When a data source supplies measured irradiance, prefer the two variants
// below: POAFromComponents (GHI + DHI both known, e.g. SMHI STRÅNG params
// 117 + 122) or POAFromGHI (only GHI known, e.g. Open-Meteo shortwave).
func POA(t time.Time, lat, lon, panelTiltDeg, panelAzDeg float64) float64 {
	sun := At(t, lat, lon)
	ghi := ClearSkyW(t, lat, lon)
	// No measured diffuse component from the clear-sky prior, so keep the
	// historical fixed 20% diffuse fraction for this variant.
	return POAFromComponents(sun, ghi, 0.2*ghi, panelTiltDeg, panelAzDeg)
}

// POAFromComponents projects measured global (GHI) and diffuse (DHI)
// horizontal irradiance onto a tilted panel using the isotropic-sky model,
// reusing AOI for the beam projection. All irradiances in W/m²; returns the
// plane-of-array irradiance in W/m², clamped ≥ 0.
//
// Use this when a source gives both GHI and DHI directly (e.g. SMHI STRÅNG
// parameters 117 + 122). When only GHI is available use POAFromGHI, which
// estimates the diffuse split via the Erbs correlation first.
func POAFromComponents(sun Position, ghi, dhi, panelTiltDeg, panelAzDeg float64) float64 {
	if sun.ZenithDeg >= 90 || ghi <= 0 {
		return 0
	}
	if dhi < 0 {
		dhi = 0
	}
	if dhi > ghi {
		dhi = ghi
	}
	tiltR := panelTiltDeg * math.Pi / 180
	diffusePOA := dhi * (1 + math.Cos(tiltR)) / 2 // isotropic sky dome
	aoi := AOI(sun, panelTiltDeg, panelAzDeg)
	if aoi > 90 {
		// Sun behind the panel — only diffuse reaches the surface.
		return diffusePOA
	}
	cosZ := math.Cos(sun.ZenithDeg * math.Pi / 180)
	if cosZ < 0.01 {
		// Sun on the horizon: beam projection is ill-conditioned
		// (divide-by-~0) and diffuse dominates anyway.
		return diffusePOA
	}
	dni := (ghi - dhi) / cosZ
	beamPOA := dni * math.Cos(aoi*math.Pi/180)
	out := beamPOA + diffusePOA
	if out < 0 {
		out = 0
	}
	return out
}

// POAFromGHI projects a measured/forecast global horizontal irradiance (GHI,
// W/m²) onto a tilted panel when no diffuse component is available. It
// estimates the diffuse fraction from the hourly clearness index via the Erbs
// et al. (1982) correlation, then delegates to POAFromComponents.
//
// Use this for radiation providers that expose shortwave/GHI but not diffuse
// (e.g. Open-Meteo shortwave_radiation, or SMHI STRÅNG global-only windows).
func POAFromGHI(t time.Time, lat, lon, ghi, panelTiltDeg, panelAzDeg float64) float64 {
	if math.IsNaN(ghi) || math.IsInf(ghi, 0) || ghi <= 0 {
		return 0
	}
	sun := At(t, lat, lon)
	if sun.ZenithDeg >= 90 {
		return 0
	}
	cosZ := math.Cos(sun.ZenithDeg * math.Pi / 180)
	i0h := extraterrestrialHorizontalW(t, cosZ)
	kt := 0.0
	if i0h > 0 {
		kt = ghi / i0h
	}
	dhi := ghi * ErbsDiffuseFraction(kt)
	return POAFromComponents(sun, ghi, dhi, panelTiltDeg, panelAzDeg)
}

// ErbsDiffuseFraction returns the diffuse fraction (DHI/GHI) for an hourly
// clearness index kt, per Erbs, Klein & Duffie (1982). Result is in
// [0.165, 1]: overcast skies (low kt) are almost entirely diffuse, clear
// skies (high kt) settle near 16.5% diffuse.
func ErbsDiffuseFraction(kt float64) float64 {
	switch {
	case kt <= 0:
		return 1
	case kt <= 0.22:
		return 1 - 0.09*kt
	case kt <= 0.80:
		return 0.9511 - 0.1604*kt + 4.388*kt*kt - 16.638*kt*kt*kt + 12.336*kt*kt*kt*kt
	default:
		return 0.165
	}
}

// extraterrestrialHorizontalW returns top-of-atmosphere irradiance on a
// horizontal surface (W/m²) at time t for a solar cosine-zenith cosZ. Used as
// the denominator of the clearness index. Returns 0 when the sun is at/below
// the horizon.
func extraterrestrialHorizontalW(t time.Time, cosZ float64) float64 {
	if cosZ <= 0 {
		return 0
	}
	doy := float64(t.UTC().YearDay())
	gamma := 2 * math.Pi * (doy - 1) / 365
	e0 := 1.000110 +
		0.034221*math.Cos(gamma) + 0.001280*math.Sin(gamma) +
		0.000719*math.Cos(2*gamma) + 0.000077*math.Sin(2*gamma)
	const i0 = 1361.0 // solar constant W/m²
	return i0 * e0 * cosZ
}
