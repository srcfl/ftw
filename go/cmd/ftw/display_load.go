package main

import (
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Display load is stored as a Core metric series, apart from every driver.
const (
	displayLoadDriver = "ftw:site"
	displayLoadMetric = "load_display_w"
)

// displayLoadSample is the live Home use clients show, for the metric store.
// Nothing while the site meter is unknown: that is what clients show too.
func displayLoadSample(tel *telemetry.Store, siteMeter string, nowMs int64, options telemetry.ForecastOptions, sun historySun) (state.Sample, bool) {
	if tel == nil {
		return state.Sample{}, false
	}
	options.PVDark = sun.darkness
	live := tel.LiveMeasurementNow(siteMeter, options)
	if !live.LoadKnown {
		return state.Sample{}, false
	}
	return state.Sample{Driver: displayLoadDriver, Metric: displayLoadMetric, TsMs: nowMs, Value: live.LoadW, Unit: "W"}, true
}
