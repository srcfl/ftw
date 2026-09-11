package main

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/srcfl/ftw/go/internal/energyforecast"
	"github.com/srcfl/ftw/go/internal/forecasting"
)

func (r *rustForecast) RestartLearning(ctx context.Context, site forecastSite, signal string) error {
	if signal == "pv" {
		site.LoadLearningStartedMS = 0
	} else {
		site.PVLearningStartedMS = 0
	}
	r.updateMu.Lock()
	defer r.updateMu.Unlock()
	return r.applyLearningPeriodsLocked(ctx, site, time.Now().UnixMilli())
}

// Caller holds updateMu across the request, durable save and memory update.
func (r *rustForecast) applyLearningPeriodsLocked(ctx context.Context, site forecastSite, origin int64) error {
	for _, signal := range []string{"pv", "load"} {
		if signal == "pv" && !site.HasLocation {
			continue
		}
		cutoff := site.PVLearningStartedMS
		if signal == "load" {
			cutoff = site.LoadLearningStartedMS
		}
		if cutoff == 0 {
			continue
		}
		r.mu.RLock()
		saved := r.saved
		r.mu.RUnlock()
		if saved.SiteID != site.SiteID || saved.ConfigRevision != rustConfigRevision(site) {
			saved = savedForecastState{SiteID: site.SiteID, ConfigRevision: rustConfigRevision(site)}
		}
		applied := saved.PVLearningStartedMS
		if signal == "load" {
			applied = saved.LoadLearningStartedMS
		}
		if applied >= cutoff {
			continue
		}
		if !r.resetSupported {
			return errors.New("forecast worker does not support restarting learning")
		}
		origin = max(origin, saved.LatestAvailableMS, cutoff)
		reply, err := r.client.Reset(ctx, energyforecast.ResetRequest{RequestContext: energyforecast.RequestContext{
			RequestID: uuid.NewString(), SiteID: site.SiteID, ConfigRevision: rustConfigRevision(site), OriginMs: origin,
			Config: rustForecastConfig(site), State: saved.State}, Signal: signal, LearningStartedMs: cutoff})
		if err != nil {
			return err
		}
		next := saved
		next.ModelRevision, next.LatestAvailableMS, next.State = reply.ModelRevision, origin, reply.State
		next.LatestTraining = reply.LatestTrainingMs
		if signal == "pv" {
			next.PVLearningStartedMS = reply.LearningStartedMs
		} else {
			next.LoadLearningStartedMS = reply.LearningStartedMs
		}
		data, err := json.Marshal(next)
		if err != nil {
			return err
		}
		if err = r.store.SaveConfig(forecastRustStateKey, string(data)); err != nil {
			return err
		}
		r.mu.Lock()
		r.saved = next
		if signal == "pv" {
			r.pvQuality = "cold_start"
			r.predictedTraining.PV = nil
		} else {
			r.loadQuality = "cold_start"
			r.predictedTraining.Load = nil
		}
		r.mu.Unlock()
	}
	return nil
}

func (r *rustForecast) LearningStatus(site forecastSite, signal string) forecasting.LearningStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	status := forecasting.LearningStatus{Engine: "energyplan", Status: "cold_start", ResetAvailable: r.resetSupported && !site.IdentityPending}
	if signal == "pv" && !site.HasLocation {
		status.Status, status.ResetAvailable = "unavailable", false
		return status
	}
	cutoff, applied, latest, quality := site.PVLearningStartedMS, r.saved.PVLearningStartedMS, r.saved.LatestTraining.PV, r.pvQuality
	if signal == "load" {
		cutoff, applied, latest, quality = site.LoadLearningStartedMS, r.saved.LoadLearningStartedMS, r.saved.LatestTraining.Load, r.loadQuality
	}
	predictedLatest := r.predictedTraining.PV
	if signal == "load" {
		predictedLatest = r.predictedTraining.Load
	}
	if predictedLatest != nil && (latest == nil || *predictedLatest > *latest) {
		latest = predictedLatest
	}
	status.StartedMS = cutoff
	if site.IdentityPending {
		status.Status = "unavailable"
		return status
	}
	if r.saved.SiteID != site.SiteID || r.saved.ConfigRevision != rustConfigRevision(site) {
		applied, latest, quality = 0, nil, "cold_start"
	}
	if applied < cutoff {
		status.Status = "unavailable"
		return status
	}
	if latest != nil && *latest > cutoff {
		status.LatestTrainingMS, status.Status = *latest, "learning"
		if quality == "ready" {
			status.Status = "ready"
		}
	}
	return status
}

func (r *rustForecast) recordLearningQuality(saved savedForecastState, reply energyforecast.PredictReply) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.saved.SiteID != saved.SiteID || r.saved.ConfigRevision != saved.ConfigRevision || r.saved.ModelRevision != saved.ModelRevision {
		return
	}
	pv, load := "ready", "ready"
	for _, p := range reply.Predictions {
		if p.PV == nil || !p.PV.Known {
			pv = leastForecastQuality(pv, "cold_start")
		} else {
			pv = leastForecastQuality(pv, p.PV.Quality)
		}
		if p.Load == nil || !p.Load.Known {
			load = leastForecastQuality(load, "cold_start")
		} else {
			load = leastForecastQuality(load, p.Load.Quality)
		}
	}
	r.pvQuality, r.loadQuality = pv, load
	r.predictedTraining = reply.LatestTrainingMs
}
