package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
)

const forecastLearningKey = "forecast/learning_periods_v1"

func (f *forecastTracker) setReplan(fn func(string)) {
	f.learningMu.Lock()
	defer f.learningMu.Unlock()
	f.requestReplan = fn
}

type forecastLearningPeriods struct {
	ConfigRevision string `json:"config_revision"`
	PVMS           int64  `json:"pv_ms"`
	LoadMS         int64  `json:"load_ms"`
}

func (f *forecastTracker) learningSiteLocked(site forecastSite) forecastSite {
	if f.learningPeriods.ConfigRevision == rustConfigRevision(site) {
		site.PVLearningStartedMS, site.LoadLearningStartedMS = f.learningPeriods.PVMS, f.learningPeriods.LoadMS
	}
	return site
}

func (f *forecastTracker) restoreLearning(ctx context.Context) error {
	if data, ok := f.store.LoadConfig(forecastLearningKey); ok {
		if err := json.Unmarshal([]byte(data), &f.learningPeriods); err != nil {
			return fmt.Errorf("read forecast learning periods: %w", err)
		}
		if f.learningPeriods.ConfigRevision == "" || f.learningPeriods.PVMS < 0 || f.learningPeriods.LoadMS < 0 {
			return errors.New("invalid forecast learning periods")
		}
	}
	if f.configMu != nil {
		f.configMu.RLock()
		defer f.configMu.RUnlock()
	}
	f.learningMu.Lock()
	defer f.learningMu.Unlock()
	for _, signal := range []string{"pv", "load"} {
		if err := f.applyLearningLocked(ctx, signal); err != nil {
			slog.Warn("forecast learning restart pending", "signal", signal, "err", err)
		}
	}
	return nil
}

func (f *forecastTracker) applyLearningLocked(ctx context.Context, signal string) error {
	site := f.learningSiteLocked(f.site())
	if site.IdentityPending {
		return nil
	}
	var errs []error
	if signal == "pv" && site.PVLearningStartedMS > 0 && f.pv != nil {
		errs = append(errs, f.pv.RestartLearning(time.UnixMilli(site.PVLearningStartedMS)))
	}
	if signal == "load" && site.LoadLearningStartedMS > 0 && f.load != nil {
		errs = append(errs, f.load.RestartLearning(time.UnixMilli(site.LoadLearningStartedMS)))
	}
	if r, ok := f.candidate.(*rustForecast); ok {
		errs = append(errs, r.RestartLearning(ctx, site, signal))
	}
	err := errors.Join(errs...)
	if f.learningErrors == nil {
		f.learningErrors = make(map[string]error)
	}
	f.learningErrors[signal] = err
	return err
}

func (f *forecastTracker) learningPendingLocked(site forecastSite, signal string) bool {
	if site.IdentityPending || f.learningPeriods.ConfigRevision != rustConfigRevision(site) {
		return false
	}
	cutoff := site.PVLearningStartedMS
	if signal == "load" {
		cutoff = site.LoadLearningStartedMS
	}
	if cutoff == 0 {
		return false
	}
	if f.learningErrors[signal] != nil {
		return true
	}
	if signal == "pv" && f.pv != nil && f.pv.LearningStartedMS() < cutoff {
		return true
	}
	if signal == "load" && f.load != nil && f.load.LearningStartedMS() < cutoff {
		return true
	}
	if r, ok := f.candidate.(*rustForecast); ok {
		return r.LearningStatus(site, signal).Status == "unavailable"
	}
	return false
}

// Both samplers and forecast captures reconcile before using any legacy state.
// In particular, startup may first need to wait for the hardware identity.
func (f *forecastTracker) reconcileLearning(ctx context.Context) {
	if f.configMu != nil {
		f.configMu.RLock()
		defer f.configMu.RUnlock()
	}
	f.learningMu.Lock()
	defer f.learningMu.Unlock()
	site := f.learningSiteLocked(f.site())
	if site.IdentityPending {
		return
	}
	for _, signal := range []string{"pv", "load"} {
		if !f.learningPendingLocked(site, signal) {
			continue
		}
		workCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := f.applyLearningLocked(workCtx, signal)
		cancel()
		if err == nil && f.requestReplan != nil {
			f.requestReplan(signal + "_learning_recovered")
		}
	}
}

// RestartLearning saves the intent before changing either pipeline. Startup
// replays it idempotently, including after a crash between the model saves.
func (f *forecastTracker) RestartLearning(ctx context.Context, signal string) error {
	if f == nil || (signal != "pv" && signal != "load") {
		return errors.New("forecast learning unavailable")
	}
	if f.refreshIdentity != nil {
		f.refreshIdentity()
	}
	if f.configMu != nil {
		f.configMu.RLock()
		defer f.configMu.RUnlock()
	}
	f.learningMu.Lock()
	defer f.learningMu.Unlock()
	site := f.site()
	if site.IdentityPending {
		return errors.New("waiting for site identity before restarting learning")
	}
	if (signal == "pv" && !site.HasLocation && f.pv == nil) || (signal == "load" && f.load == nil) {
		return errors.New("forecast model disabled")
	}
	if r, ok := f.candidate.(*rustForecast); ok {
		if !r.resetSupported {
			return errors.New("forecast worker does not support restarting learning")
		}
		if signal == "pv" && !site.HasLocation {
			return errors.New("solar forecast needs a site location")
		}
	}
	next := f.learningPeriods
	if next.ConfigRevision != rustConfigRevision(site) {
		next = forecastLearningPeriods{ConfigRevision: rustConfigRevision(site)}
	}
	cutoff := f.now().UnixMilli()
	previous := next.PVMS
	if signal == "load" {
		previous = next.LoadMS
	}
	if cutoff <= 0 || cutoff < previous {
		return errors.New("clock precedes the current learning period")
	}
	// A retry of an incomplete operation retains its original cutoff.
	if f.learningErrors[signal] != nil && previous > 0 {
		cutoff = previous
	}
	if signal == "pv" {
		next.PVMS = cutoff
	} else {
		next.LoadMS = cutoff
	}
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	// Cancel a solve that captured the old models. Its replacement waits for
	// learningMu, so it cannot capture a partially reset pair of pipelines.
	if f.requestReplan != nil {
		f.requestReplan(signal + "_learning_restarted")
	}
	if err = f.store.SaveConfig(forecastLearningKey, string(data)); err != nil {
		return fmt.Errorf("save forecast learning period: %w", err)
	}
	f.learningPeriods = next
	if err = f.applyLearningLocked(ctx, signal); err != nil {
		return &forecasting.LearningRestartPendingError{Err: err}
	}
	return nil
}

func (f *forecastTracker) LearningStatus(signal string) forecasting.LearningStatus {
	status := forecasting.LearningStatus{Engine: "legacy", Status: "unavailable"}
	if f == nil {
		return status
	}
	if f.configMu != nil {
		f.configMu.RLock()
		defer f.configMu.RUnlock()
	}
	f.learningMu.RLock()
	defer f.learningMu.RUnlock()
	site := f.learningSiteLocked(f.site())
	if r, ok := f.candidate.(*rustForecast); ok {
		status = r.LearningStatus(site, signal)
	} else {
		status.Status, status.ResetAvailable = "cold_start", !site.IdentityPending
		if signal == "pv" {
			status.StartedMS = site.PVLearningStartedMS
			if f.pv == nil {
				status.Status, status.ResetAvailable = "unavailable", false
			} else {
				m := f.pv.Model()
				status.LatestTrainingMS = m.LastMs
				if m.Samples > 0 {
					status.Status = "learning"
				}
				if m.Quality() >= 1 {
					status.Status = "ready"
				}
			}
		} else {
			status.StartedMS = site.LoadLearningStartedMS
			if f.load == nil {
				status.Status, status.ResetAvailable = "unavailable", false
			} else {
				m := f.load.Model()
				status.LatestTrainingMS = m.LastMs
				if m.Samples > 0 {
					status.Status = "learning"
				}
				if m.Quality() >= 1 {
					status.Status = "ready"
				}
			}
		}
	}
	if site.IdentityPending || (f.learningPeriods.ConfigRevision == rustConfigRevision(site) && f.learningErrors[signal] != nil) {
		status.Status = "unavailable"
	}
	return status
}

// Calibration is per signal: a PV reset also drops joint net errors, while
// retaining load evidence. Neither the archive nor its measured truth is erased.
func learningEvidence(history []forecasting.ErrorSample, observations []forecasting.Observation, pvMS, loadMS int64) ([]forecasting.ErrorSample, []forecasting.Observation) {
	if pvMS == 0 && loadMS == 0 {
		return history, observations
	}
	errors := append([]forecasting.ErrorSample(nil), history...)
	for i := range errors {
		e := &errors[i]
		if e.OriginMS < pvMS || e.StartMS < pvMS {
			e.PVKnown = false
		}
		if e.OriginMS < loadMS || e.StartMS < loadMS {
			e.LoadKnown = false
		}
	}
	truth := append([]forecasting.Observation(nil), observations...)
	for i := range truth {
		if truth[i].StartMS < pvMS {
			truth[i].PVKnown = false
		}
		if truth[i].StartMS < loadMS {
			truth[i].LoadKnown = false
		}
	}
	return errors, truth
}
