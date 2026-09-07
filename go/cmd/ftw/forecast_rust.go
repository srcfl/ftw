package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/srcfl/ftw/go/internal/energyforecast"
	"github.com/srcfl/ftw/go/internal/forecasting"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/state"
)

const forecastRustStateKey = "forecast/energyplan_state_v1"

type savedForecastState struct {
	SiteID                string                     `json:"site_id"`
	ConfigRevision        string                     `json:"config_revision"`
	ModelRevision         uint64                     `json:"model_revision"`
	LatestAvailableMS     int64                      `json:"latest_available_ms"`
	LatestTraining        energyforecast.LatestInput `json:"latest_training_ms"`
	PVLearningStartedMS   int64                      `json:"pv_learning_started_ms,omitempty"`
	LoadLearningStartedMS int64                      `json:"load_learning_started_ms,omitempty"`
	State                 json.RawMessage            `json:"state"`
}

type rustForecast struct {
	version                string
	client                 *energyforecast.Client
	transport              interface{ Close() error }
	store                  *state.Store
	mu                     sync.RWMutex
	updateMu               sync.Mutex // serialize complete update/reset exchanges and persistence
	saved                  savedForecastState
	resetSupported         bool
	pvQuality, loadQuality string
	predictedTraining      energyforecast.LatestInput
}

func newRustForecast(st *state.Store, binary string) (*rustForecast, error) {
	transport, err := mpc.NewProcessTransport(mpc.ProcessTransportConfig{Command: []string{binary}, ModuleDir: filepath.Dir(binary), IdleTimeout: 2 * time.Minute})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	line, err := transport.RoundTrip(ctx, []byte(`{"type":"handshake","protocol_version":1}`))
	var info struct {
		Name            string   `json:"name"`
		Version         string   `json:"version"`
		ForecastVersion int      `json:"forecast_protocol_version"`
		Features        []string `json:"features"`
	}
	if err == nil {
		err = json.Unmarshal(line, &info)
	}
	if err != nil || info.Name != "ftw-solver" || info.ForecastVersion != 1 {
		_ = transport.Close()
		return nil, fmt.Errorf("Energyplan forecast v1 unavailable: name=%q version=%d: %v", info.Name, info.ForecastVersion, err)
	}
	r := &rustForecast{version: forecastBinaryIdentity(binary), client: energyforecast.NewClient(transport), transport: transport, store: st}
	for _, feature := range info.Features {
		if feature == "forecast_reset" {
			r.resetSupported = true
		}
	}
	if data, ok := st.LoadConfig(forecastRustStateKey); ok {
		if len(data) <= energyforecast.MaxStateBytes+4096 {
			var saved savedForecastState
			if json.Unmarshal([]byte(data), &saved) == nil && json.Valid(saved.State) {
				r.saved = saved
			}
		}
	}
	return r, nil
}
func (r *rustForecast) Close() error { return r.transport.Close() }
func (r *rustForecast) Snapshot() json.RawMessage {
	r.mu.RLock()
	defer r.mu.RUnlock()
	data, _ := json.Marshal(r.saved)
	return data
}

func rustForecastConfig(site forecastSite) energyforecast.Config {
	cfg := energyforecast.Config{Load: &energyforecast.LoadConfig{}}
	if site.HasLocation {
		cfg.PV = &energyforecast.PVConfig{LatitudeDeg: site.Latitude, LongitudeDeg: site.Longitude}
	}
	return cfg
}
func rustFeatures(t time.Time, site forecastSite, home bool, row *state.ForecastPoint) energyforecast.Features {
	loc, err := time.LoadLocation(site.Timezone)
	if err != nil {
		loc = time.UTC
	}
	local := t.In(loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC).Unix() / 86400
	f := energyforecast.Features{LocalDay: day, LocalWeekday: (int(local.Weekday()) + 6) % 7,
		LocalMinute: local.Hour()*60 + local.Minute(), Home: &home}
	if row != nil {
		f.GHIWm2 = row.SolarWm2
		f.CloudPct = row.CloudCoverPct
		f.TempC = row.TempC
		if f.GHIWm2 != nil || f.CloudPct != nil || f.TempC != nil {
			available := row.FetchedAtMs
			f.WeatherAvailableAtMs = &available
		}
	}
	return f
}

func (r *rustForecast) Update(ctx context.Context, site forecastSite, o forecasting.Observation, weather *state.ForecastPoint, away bool) error {
	r.updateMu.Lock()
	defer r.updateMu.Unlock()
	if err := r.applyLearningPeriodsLocked(ctx, site, max(time.Now().UnixMilli(), o.AvailableAtMS)); err != nil {
		return err
	}
	r.mu.RLock()
	saved := r.saved
	r.mu.RUnlock()
	if saved.SiteID != site.SiteID || saved.ConfigRevision != rustConfigRevision(site) {
		saved = savedForecastState{SiteID: site.SiteID, ConfigRevision: rustConfigRevision(site)}
	}
	origin := max(o.AvailableAtMS, saved.LatestAvailableMS)
	if weather != nil && weather.FetchedAtMs > o.AvailableAtMS {
		return errors.New("observation weather arrived after update origin")
	}
	input := energyforecast.Observation{Interval: energyforecast.Interval{ValidStartMs: o.StartMS, ValidEndMs: o.EndMS},
		Features: rustFeatures(time.UnixMilli(o.StartMS), site, !away, weather), AvailableAtMs: o.AvailableAtMS,
		LoadQuality: energyforecast.QualityMissing, PVQuality: energyforecast.QualityMissing}
	if o.LoadKnown {
		input.HouseholdLoadW = &o.LoadW
		input.LoadQuality = energyforecast.QualityGood
	}
	if o.PVKnown {
		input.PVAvailableW = &o.PVW
		input.PVQuality = energyforecast.QualityGood
	}
	reply, err := r.client.Update(ctx, energyforecast.UpdateRequest{RequestContext: energyforecast.RequestContext{
		RequestID: uuid.NewString(), SiteID: site.SiteID, ConfigRevision: rustConfigRevision(site), OriginMs: origin,
		Config: rustForecastConfig(site), State: saved.State}, Observations: []energyforecast.Observation{input}})
	if err != nil {
		return err
	}
	next := saved
	next.ModelRevision, next.LatestAvailableMS, next.State = reply.ModelRevision, origin, reply.State
	next.LatestTraining = reply.LatestTrainingMs
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	// Persist the complete response atomically before exposing it to planning.
	if err = r.store.SaveConfig(forecastRustStateKey, string(data)); err != nil {
		return err
	}
	r.mu.Lock()
	if r.saved.SiteID != next.SiteID || r.saved.ConfigRevision != next.ConfigRevision {
		r.pvQuality, r.loadQuality = "", ""
		r.predictedTraining = energyforecast.LatestInput{}
	}
	r.saved = next
	r.mu.Unlock()
	return nil
}

func (r *rustForecast) Predict(ctx context.Context, site forecastSite, issued forecasting.Issue, raw json.RawMessage, away map[int64]bool) (forecasting.Issue, error) {
	if err := forecasting.ValidateOccupancy(issued.Occupancy, issued.OriginMS); err != nil {
		return forecasting.Issue{}, err
	}
	occupancy := append([]forecasting.Occupancy(nil), issued.Occupancy...)
	homeByQuarter := make(map[int64]bool, len(occupancy))
	for _, row := range occupancy {
		homeByQuarter[row.StartMS] = row.Home
	}
	var saved savedForecastState
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &saved); err != nil {
			return forecasting.Issue{}, err
		}
	}
	if saved.SiteID != site.SiteID || saved.ConfigRevision != rustConfigRevision(site) {
		saved = savedForecastState{SiteID: site.SiteID, ConfigRevision: rustConfigRevision(site)}
	}
	if saved.LatestAvailableMS > issued.OriginMS {
		return forecasting.Issue{}, errors.New("candidate state newer than forecast origin")
	}
	if len(issued.Series) == 0 {
		return forecasting.Issue{}, errors.New("no champion intervals")
	}
	base := issued.Series[0].Points
	horizon := make([]energyforecast.HorizonSlot, 0)
	for _, p := range base {
		for at := p.StartMS / 900000 * 900000; at < p.EndMS; at += 900000 {
			partStart, partEnd := max(at, p.StartMS, issued.OriginMS), min(at+900000, p.EndMS)
			if partStart >= partEnd {
				continue
			}
			home, knownHome := homeByQuarter[at]
			if len(issued.Occupancy) > 0 && !knownHome {
				return forecasting.Issue{}, errors.New("archived occupancy does not cover candidate quarter")
			}
			if len(issued.Occupancy) == 0 {
				home = !away[at]
				occupancy = append(occupancy, forecasting.Occupancy{StartMS: at, EndMS: at + 900000, AvailableAtMS: issued.OriginMS, Home: home})
			}
			var weather *state.ForecastPoint
			for _, w := range issued.Weather {
				if at >= w.StartMS && at < w.EndMS {
					weather = &state.ForecastPoint{SlotTsMs: w.StartMS, SlotLenMin: int((w.EndMS - w.StartMS) / 60000),
						FetchedAtMs: w.AvailableAtMS, Source: w.Source, SolarWm2: w.GHIWm2, TempC: w.TempC, CloudCoverPct: w.CloudPct}
					break
				}
			}
			horizon = append(horizon, energyforecast.HorizonSlot{Interval: energyforecast.Interval{ValidStartMs: partStart, ValidEndMs: partEnd},
				Features: rustFeatures(time.UnixMilli(at), site, home, weather)})
		}
	}
	if len(horizon) == 0 {
		return forecasting.Issue{}, errors.New("no remaining forecast interval")
	}
	reply, err := r.client.Predict(ctx, energyforecast.PredictRequest{RequestContext: energyforecast.RequestContext{
		RequestID: uuid.NewString(), SiteID: site.SiteID, ConfigRevision: rustConfigRevision(site), OriginMs: issued.OriginMS,
		Config: rustForecastConfig(site), State: saved.State}, Horizon: horizon})
	if err != nil {
		return forecasting.Issue{}, err
	}
	// A durable reset intent can outlive a failed worker exchange. Until that
	// signal has restarted, only its freshly reset legacy fallback may serve it.
	for i := range reply.Predictions {
		if saved.PVLearningStartedMS < site.PVLearningStartedMS {
			reply.Predictions[i].PV = &energyforecast.Estimate{Quality: "cold_start", Uncertainty: "unknown"}
		}
		if saved.LoadLearningStartedMS < site.LoadLearningStartedMS {
			reply.Predictions[i].Load = &energyforecast.Estimate{Quality: "cold_start", Uncertainty: "unknown"}
		}
	}
	r.recordLearningQuality(saved, reply)
	now := time.Now().UnixMilli()
	out := forecasting.Issue{Schema: forecasting.Schema, ID: uuid.NewString(), DecisionID: issued.DecisionID,
		OriginMS: issued.OriginMS, IssuedAtMS: now, ConfigVersion: issued.ConfigVersion, Site: forecastSiteContext(site), Weather: issued.Weather,
		LatestInputMS: max(saved.LatestAvailableMS, issued.LatestInputMS), Occupancy: occupancy}
	for _, row := range out.Occupancy {
		out.LatestInputMS = max(out.LatestInputMS, row.AvailableAtMS)
	}
	for _, row := range out.Weather {
		out.LatestInputMS = max(out.LatestInputMS, row.AvailableAtMS)
	}
	modelState := saved.State
	if len(modelState) == 0 {
		modelState = json.RawMessage("null")
	}
	quality := forecasting.ModelQualityColdStart
	if saved.LatestAvailableMS > 0 {
		quality = forecasting.ModelQualityWarm
	}
	version := "energyplan/v1/" + r.version
	if r.version == "" {
		version = "energyplan/v1/test"
	}
	out.Models = []forecasting.ModelState{{Name: "energyplan", Version: version, UpdatedAtMS: saved.LatestAvailableMS, Quality: quality, State: modelState}}
	metadata, _ := json.Marshal(struct {
		ModelRevision   uint64                     `json:"model_revision"`
		LatestInput     energyforecast.LatestInput `json:"latest_input_ms"`
		LatestTraining  energyforecast.LatestInput `json:"latest_training_ms"`
		LatestAvailable energyforecast.LatestInput `json:"latest_available_at_ms"`
	}{reply.ModelRevision, reply.LatestInputMs, reply.LatestTrainingMs, reply.LatestAvailableAtMs})
	out.Models = append(out.Models, forecasting.ModelState{Name: "energyplan_metadata", Version: version,
		UpdatedAtMS: saved.LatestAvailableMS, Quality: quality, State: metadata})
	series := forecasting.Series{Name: "energyplan", ModelVersion: version}
	// Preserve planner slot bounds while averaging only the remaining duration.
	// Complete quarters and the current partial quarter contribute by energy.
	for _, p := range base {
		predictionStart := max(p.StartMS, issued.OriginMS)
		if predictionStart >= p.EndMS {
			continue
		}
		got := forecasting.Point{StartMS: p.StartMS, EndMS: p.EndMS, PVKnown: site.HasLocation, LoadKnown: true, PVQuality: "ready", LoadQuality: "ready", PVSource: "energyplan", LoadSource: "energyplan"}
		if predictionStart > p.StartMS {
			got.PredictionStartMS = predictionStart
		}
		cursor := predictionStart
		parts, pvEvidence, loadEvidence := 0, 0, 0
		for _, v := range reply.Predictions {
			if v.ValidStartMs < cursor {
				continue
			}
			if v.ValidStartMs != cursor || v.ValidEndMs > p.EndMS {
				break
			}
			fraction := float64(v.ValidEndMs-v.ValidStartMs) / float64(p.EndMS-predictionStart)
			parts++
			if addModelEvidence(&got.ModelPV, v.PV, fraction) {
				pvEvidence++
			}
			if addModelEvidence(&got.ModelLoad, v.Load, fraction) {
				loadEvidence++
			}
			if v.PV == nil || !v.PV.Known || v.PV.PointW == nil {
				got.PVKnown = false
				got.PVQuality = "unknown"
			} else {
				got.PVW += *v.PV.PointW * fraction
				got.PVQuality = leastForecastQuality(got.PVQuality, v.PV.Quality)
			}
			if v.Load == nil || !v.Load.Known || v.Load.PointW == nil {
				got.LoadKnown = false
				got.LoadQuality = "unknown"
			} else {
				got.LoadW += *v.Load.PointW * fraction
				got.LoadQuality = leastForecastQuality(got.LoadQuality, v.Load.Quality)
			}
			cursor = v.ValidEndMs
			if cursor == p.EndMS {
				break
			}
		}
		if pvEvidence != parts || !got.PVKnown {
			got.ModelPV = nil
		}
		if loadEvidence != parts || !got.LoadKnown {
			got.ModelLoad = nil
		}
		if cursor == p.EndMS {
			series.Points = append(series.Points, got)
		}
	}
	if len(series.Points) == 0 {
		return forecasting.Issue{}, errors.New("candidate did not cover a remaining planner interval")
	}
	out.Series = []forecasting.Series{series}
	return out, nil
}

func leastForecastQuality(a, b string) string {
	order := map[string]int{"unknown": 0, "cold_start": 1, "learning": 2, "ready": 3}
	if order[b] < order[a] {
		return b
	}
	return a
}

// The actual worker bytes, not its revision counter, identify the model code.
func forecastBinaryIdentity(binary string) string {
	f, err := os.Open(binary)
	if err != nil {
		return "unavailable"
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "unavailable"
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func rustConfigRevision(site forecastSite) string {
	if site.LearningRevision != "" {
		return site.LearningRevision
	}
	return site.Revision
}

// Aggregated bounds retain the worker's provisional label. Averaging four
// such bounds does not turn them into a calibrated hourly quantile.
func addModelEvidence(dst **forecasting.ModelEstimateEvidence, v *energyforecast.Estimate, fraction float64) bool {
	if v == nil || !v.Known || v.Uncertainty != "provisional" || v.LowerW == nil || v.UpperW == nil {
		return false
	}
	if *dst == nil {
		*dst = &forecasting.ModelEstimateEvidence{Uncertainty: "provisional", Coverage: v.Coverage}
	}
	(*dst).LowerW += *v.LowerW * fraction
	(*dst).UpperW += *v.UpperW * fraction
	(*dst).Coverage = min((*dst).Coverage, v.Coverage)
	return true
}
