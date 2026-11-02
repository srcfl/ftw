package batteryreturn

import (
	"context"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
)

func TestPlanningCountsTheLeastTaxAnyHouseholdPays(t *testing.T) {
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 12, 0, 0, 0, stockholm) }
	for _, c := range []struct {
		name     string
		from, to time.Time
		want     float64
	}{
		{"2026, after the northern deduction", day(2026, 11, 2), day(2026, 11, 4), 36.0 - 9.6},
		{"across new year counts nothing", day(2025, 12, 31), day(2026, 1, 2), 0},
		{"2025", day(2025, 6, 1), day(2025, 6, 3), 43.9 - 9.6},
		{"a year past the table keeps the last rate", day(2027, 3, 1), day(2027, 3, 3), 36.0 - 9.6},
		{"a year before the table counts nothing", day(2023, 6, 1), day(2023, 6, 2), 0},
		{"new year is Swedish midnight", time.Date(2025, 12, 31, 22, 30, 0, 0, time.UTC), time.Date(2025, 12, 31, 23, 15, 0, 0, time.UTC), 0},
		{"the last Swedish hour of 2025 is still 2025", time.Date(2025, 12, 31, 22, 15, 0, 0, time.UTC), time.Date(2025, 12, 31, 22, 45, 0, 0, time.UTC), 43.9 - 9.6},
	} {
		if got := PlanningOreKwh(c.from, c.to); got < c.want-1e-9 || got > c.want+1e-9 {
			t.Errorf("%s: %.2f öre/kWh, want %.2f", c.name, got, c.want)
		}
	}
}

func TestOnlyASwedishPositionPlacesTheSite(t *testing.T) {
	for _, c := range []struct {
		lat, lon float64
		want     bool
	}{
		{59.6, 16.5, true}, {67.86, 20.23, true}, {55.6, 13.0, true},
		{16.5, 59.6, false},       // swapped
		{59.6, -16.5, false},      // lost sign
		{59.3293, 18.0686, false}, // setup's starting pin
		{0, 0, false}, {60.17, 24.94 + 1, false},
	} {
		if got := placed(c.lat, c.lon); got != c.want {
			t.Fatalf("placed(%v, %v) = %v", c.lat, c.lon, got)
		}
	}
}

func TestSiteOfSumsSizedBatteriesAndPlacesInSweden(t *testing.T) {
	cfg := &config.Config{
		Drivers: []config.Driver{{BatteryCapacityWh: 10_000}, {}, {BatteryCapacityWh: 5_000}},
		Weather: &config.Weather{Latitude: 59.6, Longitude: 16.5},
	}
	if s := SiteOf(cfg); s.CapacityWh != 15_000 || s.Batteries != 2 || !s.Placed || s.Lat != 59.6 {
		t.Fatalf("site %+v", s)
	}
	if s := SiteOf(nil); s != (Site{}) {
		t.Fatalf("nil config gave %+v", s)
	}
}

func TestHeldIsTheMixAfterTheLastCountedHour(t *testing.T) {
	r := build(t, site(arbitrage(0, 1)), oneBattery)
	last := r.Hours[len(r.Hours)-1]
	grid, other, at, ok, err := Held(context.Background(), site(arbitrage(0, 1)), reportNow, oneBattery)
	if err != nil || !ok {
		t.Fatalf("ok %v err %v", ok, err)
	}
	if grid != last.GridHeld || other != last.OtherHeld || !at.Equal(twoDays) {
		t.Fatalf("held %.1f/%.1f at %v, want %.1f/%.1f at %v", grid, other, at, last.GridHeld, last.OtherHeld, twoDays)
	}
	two := Site{CapacityWh: 10_000, Batteries: 2}
	if _, _, _, ok, _ := Held(context.Background(), site(arbitrage(0)), reportNow, two); ok {
		t.Fatal("two batteries have no figure, so nothing is held")
	}
}
