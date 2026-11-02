package batteryreturn

import (
	"context"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
)

// taxOreKwh is the energy tax on electricity in öre/kWh excluding VAT, by
// Swedish calendar year (Skatteverket, "Skatt på el"). The refund returns
// this tax, not the VAT on it.
var taxOreKwh = map[int]float64{2024: 42.8, 2025: 43.9, 2026: 36.0}

// lastTaxYear is the newest year in taxOreKwh. A later year keeps its rate
// until a release brings the new one.
const lastTaxYear = 2026

// northernDeductionOreKwh is what households in parts of northern Sweden
// deduct from the tax. The municipalities cannot be told from the price
// zone: Torsby, Mora and the rest of northern Dalarna lie in SE3.
const northernDeductionOreKwh = 9.6

// PlanningOreKwh is the refund per kWh a plan over [from, to) can count on:
// the least energy tax any Swedish household pays that year. A plan that
// crosses New Year counts none: the count starts each year with the battery
// full of energy from elsewhere, so grid energy stored before midnight is
// not returned after it.
func PlanningOreKwh(from, to time.Time) float64 {
	loc, err := time.LoadLocation(Zone)
	if err != nil || to.Before(from) {
		return 0
	}
	year := from.In(loc).Year()
	if to.In(loc).Year() != year {
		return 0
	}
	rate, ok := taxOreKwh[min(year, lastTaxYear)]
	if !ok {
		return 0
	}
	return max(rate-northernDeductionOreKwh, 0)
}

// SiteOf reads what the count needs from the configuration.
func SiteOf(cfg *config.Config) Site {
	var site Site
	if cfg == nil {
		return site
	}
	for _, d := range cfg.Drivers {
		if d.BatteryCapacityWh > 0 {
			site.CapacityWh += d.BatteryCapacityWh
			site.Batteries++
		}
	}
	if w := cfg.Weather; w != nil && placed(w.Latitude, w.Longitude) {
		site.Lat, site.Lon, site.Placed = w.Latitude, w.Longitude, true
	}
	return site
}

// placed reports whether the owner set a position in Sweden, where the
// refund applies. A wrong position could put the sun down while panels
// still produce: setup's map starts on central Stockholm, so a site left
// there has none, and swapped coordinates land outside the country.
func placed(lat, lon float64) bool {
	return lat >= 55 && lat <= 69.5 && lon >= 10.5 && lon <= 24.5 && !(lat == 59.3293 && lon == 18.0686)
}

// Held is the battery's grid and other energy after the last hour this
// year's count has, in Wh the battery can give back, and when that hour
// ended. ok is false when the year has no figure. Before the year's first
// hour is counted the battery holds no grid energy.
func Held(ctx context.Context, l Ledger, now time.Time, site Site) (grid, other float64, at time.Time, ok bool, err error) {
	loc, err := time.LoadLocation(Zone)
	if err != nil {
		return 0, 0, time.Time{}, false, err
	}
	r, err := Build(ctx, l, now.In(loc).Year(), now, site)
	if err != nil || r.Problem != "" {
		return 0, 0, time.Time{}, false, err
	}
	if len(r.Hours) == 0 {
		return 0, 0, time.Date(r.Year, 1, 1, 0, 0, 0, 0, loc), true, nil
	}
	last := r.Hours[len(r.Hours)-1]
	return last.GridHeld, last.OtherHeld, last.Start.Add(time.Hour), true, nil
}
