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
	metrics := forecasting.CompareFrozenSeries(recent, "energyplan", "legacy_shadow")
	return forecastSourceChoice{PV: pickForecastSource(metrics, "pv_daylight"), Load: pickForecastSource(metrics, "load")}
}

func pickForecastSource(metrics []forecasting.PairMetric, signal string) forecastSourcePick {
	var pick forecastSourcePick
	for _, m := range metrics {
		if m.Signal != signal {
			continue
		}
		pick.Samples += m.Samples
		pick.Hours = max(pick.Hours, m.Samples) // one sample per scored hour in each lead bucket
		pick.Days = max(pick.Days, m.Days)
		pick.EnergyplanMAEW += m.ChampionMAEW * float64(m.Samples)
		pick.LegacyMAEW += m.CandidateMAEW * float64(m.Samples)
	}
	if pick.Samples == 0 {
		return pick
	}
	pick.EnergyplanMAEW /= float64(pick.Samples)
	pick.LegacyMAEW /= float64(pick.Samples)
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
