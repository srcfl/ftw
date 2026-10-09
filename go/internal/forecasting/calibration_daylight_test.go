package forecasting

import (
	"testing"
	"time"
)

func daylightCalibrationHistory(days, daylightHours int) []ErrorSample {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	var history []ErrorSample
	for day := 0; day < days; day++ {
		for hour := 0; hour < 24; hour++ {
			start := base + int64(day*24+hour)*hourMS
			e := testError("champion", "cfg", "i", float64(start), float64(start-2*hourMS), 0, 20)
			if hour >= 10 && hour < 10+daylightHours {
				e.Daylight, e.PVErrorW, e.LoadErrorW = true, -1000, 100
			}
			history = append(history, e)
		}
	}
	return history
}

func TestCalibratorSeparatesPVAndNetDayFromNight(t *testing.T) {
	history := daylightCalibrationHistory(7, 8)
	origin := time.Date(2026, 8, 9, 8, 0, 0, 0, time.UTC).UnixMilli()
	c := NewCalibrator(history, "cfg", origin)
	site := testSiteContext()
	day := origin + 2*hourMS
	night := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC).UnixMilli()
	// Use a separate origin to keep night in the same tested lead bucket.
	nightC := NewCalibrator(history, "cfg", night-2*hourMS)
	for _, tc := range []struct {
		name, signal string
		calibrator   *Calibrator
		target       int64
		prediction   float64
		wantLow      float64
		wantSamples  int
	}{
		{"day PV", "pv", c, day, 2000, 1000, 56},
		{"day net", "net", c, day, 300, 1400, 56},
		{"night PV", "pv", nightC, night, 0, 0, 112},
		{"night net", "net", nightC, night, 1000, 1020, 112},
	} {
		t.Run(tc.name, func(t *testing.T) {
			band := tc.calibrator.BandForSiteInterval("champion", tc.signal, site, tc.target, tc.target+hourMS, tc.prediction)
			if band.Method != BandMethodEmpirical || band.Samples != tc.wantSamples || band.Days != 7 || band.LowW != tc.wantLow || band.HighW != tc.wantLow {
				t.Fatalf("band mixed day and night: %+v", band)
			}
		})
	}
	// Location-free callers retain the old all-hours behaviour; load is
	// independent of the PV regime even where location is known.
	for _, signal := range []string{"pv", "load", "net"} {
		want := c.Band("champion", signal, day, 1000)
		for _, missing := range []*SiteContext{nil, {HasLocation: false}} {
			if got := c.BandForSiteInterval("champion", signal, missing, day, day+hourMS, 1000); got != want {
				t.Fatalf("%s without location changed: got %+v want %+v", signal, got, want)
			}
		}
		if signal == "load" {
			if got := c.BandForSiteInterval("champion", signal, site, day, day+hourMS, 1000); got != want {
				t.Fatalf("load calibration changed: got %+v want %+v", got, want)
			}
		}
	}
}

func TestCalibratorNightCannotCompleteDaylightCoverage(t *testing.T) {
	origin := time.Date(2026, 8, 9, 8, 0, 0, 0, time.UTC).UnixMilli()
	for _, tc := range []struct {
		name                  string
		days, daylightHours   int
		wantSamples, wantDays int
	}{
		{"not enough daylight hours", 7, 6, 42, 7},
		{"not enough daylight days", 6, 8, 48, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCalibrator(daylightCalibrationHistory(tc.days, tc.daylightHours), "cfg", origin)
			for _, signal := range []string{"pv", "net"} {
				band := c.BandForSiteInterval("champion", signal, testSiteContext(), origin+2*hourMS, origin+3*hourMS, 2000)
				if band.Method != BandMethodColdStart || band.Samples != tc.wantSamples || band.Days != tc.wantDays {
					t.Fatalf("%s used nights to claim daytime calibration: %+v", signal, band)
				}
			}
		})
	}
}
