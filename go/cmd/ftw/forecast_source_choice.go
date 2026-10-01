package main

import (
	"log/slog"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
)

// forecastSourcePick is one signal's source, chosen from paired errors of the
// Energyplan and legacy forecasts in the same issue. An empty Source leaves
// the choice to the worker's quality label.
type forecastSourcePick struct {
	Source                     string
	Samples, Hours, Days       int
	EnergyplanMAEW, LegacyMAEW float64
}

type forecastSourceChoice struct{ PV, Load forecastSourcePick }

// One sunny or unusual day must not pick the source: require scored hours
// spread over several days and a clear gap. Evidence older than a week is
// dropped because both models keep learning.
const (
	forecastChoiceWindow   = 7 * 24 * time.Hour
	forecastChoiceMinHours = 48
	forecastChoiceMinDays  = 3
	forecastChoiceMargin   = 0.1
)

func chooseForecastSources(history []forecasting.ErrorSample, cohort string, origin int64) forecastSourceChoice {
	since := origin - forecastChoiceWindow.Milliseconds()
	recent := make([]forecasting.ErrorSample, 0, len(history))
	for _, e := range history {
		if e.ConfigVersion == cohort && e.StartMS >= since && e.AvailableAtMS <= origin {
			recent = append(recent, e)
		}
	}
	return forecastSourceChoice{
		PV:   pickForecastSource(forecasting.PoolFrozenSeries(recent, "energyplan", "legacy_shadow", "pv_daylight")),
		Load: pickForecastSource(forecasting.PoolFrozenSeries(recent, "energyplan", "legacy_shadow", "load")),
	}
}

func pickForecastSource(m forecasting.PooledPairMetric) forecastSourcePick {
	pick := forecastSourcePick{Samples: m.Samples, Hours: m.Hours, Days: m.Days,
		EnergyplanMAEW: m.ChampionMAEW, LegacyMAEW: m.CandidateMAEW}
	if pick.Hours < forecastChoiceMinHours || pick.Days < forecastChoiceMinDays {
		return pick
	}
	switch {
	case pick.EnergyplanMAEW < pick.LegacyMAEW*(1-forecastChoiceMargin):
		pick.Source = "energyplan"
	case pick.LegacyMAEW < pick.EnergyplanMAEW*(1-forecastChoiceMargin):
		pick.Source = "legacy"
	}
	return pick
}

// forecastSeriesOf names the archived series that holds a source's own
// forecast.
func forecastSeriesOf(source string) string {
	if source == "energyplan" {
		return "energyplan"
	}
	return "legacy_shadow"
}

// forecastMixSeries names the forecast that takes PV from one series and load
// from another.
func forecastMixSeries(pv, load string) string {
	if pv == load {
		return pv
	}
	return "mix:" + pv + "+" + load
}

// riskEvidence holds each source's own errors and both mixes of them, scored
// on the same issues. A slot's margin then describes the sources it plans
// with, whichever sources earlier plans used.
func riskEvidence(history []forecasting.ErrorSample) []forecasting.ErrorSample {
	own := make([]forecasting.ErrorSample, 0, len(history))
	for _, e := range history {
		if e.Series == "energyplan" || e.Series == "legacy_shadow" {
			own = append(own, e)
		}
	}
	out := append([]forecasting.ErrorSample(nil), own...)
	for _, mix := range [][2]string{{"energyplan", "legacy_shadow"}, {"legacy_shadow", "energyplan"}} {
		out = append(out, forecasting.ComposeFrozenSeries(own, mix[0], mix[1], forecastMixSeries(mix[0], mix[1]))...)
	}
	return out
}

// noteSourceChoice logs when measured errors move a signal to another source.
func (f *forecastTracker) noteSourceChoice(c forecastSourceChoice) {
	f.mu.Lock()
	old := f.sourceChoice
	f.sourceChoice = c
	f.mu.Unlock()
	if old.PV.Source == c.PV.Source && old.Load.Source == c.Load.Source {
		return
	}
	for _, s := range []struct {
		signal string
		pick   forecastSourcePick
	}{{"pv", c.PV}, {"load", c.Load}} {
		source := s.pick.Source
		if source == "" {
			source = "quality_rule"
		}
		slog.Info("forecast source chosen", "signal", s.signal, "source", source, "hours", s.pick.Hours,
			"days", s.pick.Days, "energyplan_mae_w", int(s.pick.EnergyplanMAEW), "legacy_mae_w", int(s.pick.LegacyMAEW))
	}
}
