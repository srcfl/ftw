package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/srcfl/ftw/go/internal/forecast"
	"github.com/srcfl/ftw/go/internal/forecasting"
	"github.com/srcfl/ftw/go/internal/loadmodel"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/pvmodel"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

type forecastSite struct {
	IdentityPending     bool
	WeatherSinceMS      int64
	LearningRevision    string
	Revision            string
	SiteID              string
	Meter               string
	Latitude, Longitude float64
	HasLocation         bool
	HasPVScale          bool
	Timezone            string
	Options             telemetry.ForecastOptions
}

// forecastCandidate is a host adapter around the compiled model. State is
// captured at forecast origin; later observations cannot alter that request.
type forecastCandidate interface {
	Snapshot() json.RawMessage
	Update(context.Context, forecastSite, forecasting.Observation, *state.ForecastPoint, bool) error
	Predict(context.Context, forecastSite, forecasting.Issue, json.RawMessage, map[int64]bool) (forecasting.Issue, error)
	Close() error
}

type forecastJob struct {
	issue forecasting.Issue
}

type forecastTracker struct {
	refreshIdentity func()
	configMu        *sync.RWMutex
	store           *state.Store
	tele            *telemetry.Store
	pv              *pvmodel.Service
	load            *loadmodel.Service
	site            func() forecastSite
	away            func(time.Time) bool
	curtailed       func(time.Time) bool
	candidate       forecastCandidate
	queue           chan forecastJob
	cancel          context.CancelFunc
	wg              sync.WaitGroup
	mu              sync.RWMutex
	errors          []forecasting.ErrorSample
	observations    []forecasting.Observation
	accumulator     telemetry.ForecastAccumulator
	lastRevision    string
	pvAccumulator   telemetry.ForecastAccumulator
	stopped         bool
	clock           func() time.Time
}

func (f *forecastTracker) Start(ctx context.Context) error {
	initCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	err := f.store.InitForecastArchive(initCtx)
	cancel()
	if err != nil {
		return err
	}
	f.queue = make(chan forecastJob, 8)
	workerCtx, stop := context.WithCancel(ctx)
	f.cancel = stop
	f.wg.Add(1)
	go func() { defer f.wg.Done(); f.run(workerCtx) }()
	return nil
}
func (f *forecastTracker) Stop() {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.stopped = true
	f.mu.Unlock()
	if f.cancel != nil {
		f.cancel()
	}
	f.wg.Wait()
	if f.candidate != nil {
		_ = f.candidate.Close()
	}
}

func (f *forecastTracker) now() time.Time {
	if f.clock != nil {
		return f.clock().UTC()
	}
	return time.Now().UTC()
}

func (f *forecastTracker) run(ctx context.Context) {
	var pending *forecastJob
	defer func() {
		f.mu.Lock()
		f.stopped = true
		f.mu.Unlock()
		// Finish archiving the already issued primary and shadow together under
		// a fresh bounded context. Their frozen inputs cannot be recreated.
		drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		save := func(job forecastJob) {
			if err := f.store.SaveForecastIssue(drainCtx, job.issue); err != nil {
				slog.Warn("forecast archive: shutdown issue unavailable", "id", job.issue.ID, "err", err)
			}
		}
		if pending != nil {
			save(*pending)
		}
		for {
			select {
			case job := <-f.queue:
				save(job)
			default:
				return
			}
		}
	}()
	f.refreshEvidence(ctx, f.now())
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-f.queue:
			pending = &job
			writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := f.store.SaveForecastIssue(writeCtx, job.issue)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				pending = nil
				slog.Warn("forecast archive: issue not saved", "id", job.issue.ID, "err", err)
				continue
			}
			pending = nil
		case now := <-tick.C:
			f.observe(ctx, now)
		}
	}
}

func (f *forecastTracker) observe(ctx context.Context, now time.Time) {
	if f.refreshIdentity != nil {
		f.refreshIdentity()
	}
	if f.configMu != nil {
		f.configMu.RLock()
	}
	site := f.site()
	if site.IdentityPending {
		if f.configMu != nil {
			f.configMu.RUnlock()
		}
		f.accumulator = telemetry.ForecastAccumulator{}
		f.pvAccumulator = telemetry.ForecastAccumulator{}
		f.lastRevision = ""
		return
	}

	r := f.tele.ForecastMeasurement(now, site.Meter, site.Options)
	if f.configMu != nil {
		f.configMu.RUnlock()
	}
	if f.curtailed != nil && f.curtailed(now) {
		r.PVValid = false
		r.PVReason = "curtailed"
	}
	for _, o := range f.observationIntervals(r, site, now) {
		writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := f.store.SaveForecastObservation(writeCtx, o)
		cancel()
		if err != nil {
			slog.Warn("forecast archive: observation not saved", "err", err)
			continue
		}
		if f.candidate != nil {
			rows, _ := f.store.LoadForecasts(o.StartMS-time.Hour.Milliseconds(), o.EndMS)
			weather := forecastRow(usableTrackingWeather(rows, now.UnixMilli()), o.StartMS, now.UnixMilli())
			if weather != nil && weather.FetchedAtMs < site.WeatherSinceMS {
				weather = nil
			}
			away := f.away != nil && f.away(time.UnixMilli(o.StartMS))
			workCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err = f.candidate.Update(workCtx, site, o, weather, away)
			cancel()
			if err != nil {
				slog.Debug("forecast candidate update unavailable", "err", err)
			}
		}
		f.score(ctx, now)
	}
}

// observationIntervals keeps the two measurement claims independent. Unknown
// battery power invalidates household load, but cannot erase valid PV evidence.
func (f *forecastTracker) observationIntervals(r telemetry.ForecastReading, site forecastSite, now time.Time) []forecasting.Observation {
	if site.Revision != f.lastRevision {
		f.accumulator = telemetry.ForecastAccumulator{}
		f.pvAccumulator = telemetry.ForecastAccumulator{}
		f.lastRevision = site.Revision
	}
	byStart := map[int64]forecasting.Observation{}
	for _, v := range f.accumulator.Observe(r) {
		byStart[v.Start.UnixMilli()] = forecasting.Observation{StartMS: v.Start.UnixMilli(), EndMS: v.End.UnixMilli(), AvailableAtMS: now.UnixMilli(), LoadW: v.HouseholdW, LoadKnown: true, Quality: v.Quality, ConfigVersion: site.Revision}
	}
	pv := r
	pv.Valid = r.PVValid
	pv.HouseholdW = 0
	pv.Latest = r.PVLatest
	for _, v := range f.pvAccumulator.Observe(pv) {
		key := v.Start.UnixMilli()
		o, ok := byStart[key]
		if !ok {
			o = forecasting.Observation{StartMS: key, EndMS: v.End.UnixMilli(), AvailableAtMS: now.UnixMilli(), Quality: "complete_pv_v1", ConfigVersion: site.Revision}
		}
		o.PVW = math.Max(0, -v.PVW)
		o.PVKnown = v.PVValid
		byStart[key] = o
	}
	out := make([]forecasting.Observation, 0, len(byStart))
	for _, o := range byStart {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartMS < out[j].StartMS })
	return out
}

func (f *forecastTracker) score(ctx context.Context, now time.Time) {
	scoreCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	observations, err := f.store.LoadForecastObservations(scoreCtx, now.Add(-2*time.Hour).UnixMilli(), now.UnixMilli())
	if err != nil {
		slog.Warn("forecast evaluation: observations unavailable", "err", err)
		return
	}
	// Decode one archived model snapshot at a time. Only the last complete
	// hour of outcomes can have become scoreable since the previous tick.
	scores := make(map[forecastErrorKey]forecasting.ErrorSample)
	err = f.store.VisitForecastIssues(scoreCtx, now.Add(-50*time.Hour).UnixMilli(), now.UnixMilli(), func(issue forecasting.Issue) error {
		mergeForecastErrors(scores, forecasting.Errors([]forecasting.Issue{issue}, observations, now.UnixMilli()))
		return nil
	})
	if err == nil {
		err = f.store.SaveForecastErrors(scoreCtx, forecastErrorValues(scores), now.UnixMilli())
	}
	if err != nil {
		slog.Warn("forecast evaluation failed", "err", err)
		return
	}
	f.refreshEvidence(ctx, now)
}

type forecastErrorKey struct {
	Series, Config string
	Start, End     int64
	Lead           int
}

func mergeForecastErrors(dst map[forecastErrorKey]forecasting.ErrorSample, src []forecasting.ErrorSample) {
	for _, e := range src {
		key := forecastErrorKey{e.Series, e.ConfigVersion, e.StartMS, e.EndMS, e.Lead}
		old, ok := dst[key]
		if !ok || e.OriginMS > old.OriginMS || (e.OriginMS == old.OriginMS && (e.IssuedAtMS > old.IssuedAtMS || (e.IssuedAtMS == old.IssuedAtMS && e.IssueID > old.IssueID))) {
			dst[key] = e
		}
	}
}
func forecastErrorValues(m map[forecastErrorKey]forecasting.ErrorSample) []forecasting.ErrorSample {
	out := make([]forecasting.ErrorSample, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.StartMS != b.StartMS {
			return a.StartMS < b.StartMS
		}
		if a.Series != b.Series {
			return a.Series < b.Series
		}
		if a.ConfigVersion != b.ConfigVersion {
			return a.ConfigVersion < b.ConfigVersion
		}
		if a.Lead != b.Lead {
			return a.Lead < b.Lead
		}
		return a.EndMS < b.EndMS
	})
	return out
}

func (f *forecastTracker) refreshEvidence(ctx context.Context, now time.Time) {
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	errors, err := f.store.LoadForecastErrors(readCtx, now.Add(-30*24*time.Hour).UnixMilli(), now.UnixMilli(), true)
	if err != nil {
		slog.Warn("forecast calibration unavailable", "err", err)
		return
	}
	observations, err := f.store.LoadForecastObservations(readCtx, now.Add(-8*24*time.Hour).UnixMilli(), now.UnixMilli())
	if err != nil {
		slog.Warn("forecast baselines unavailable", "err", err)
		return
	}
	f.mu.Lock()
	f.errors = errors
	f.observations = observations
	f.mu.Unlock()
}

func forecastHash(value any) string {
	data, _ := json.Marshal(value)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func forecastRow(rows []state.ForecastPoint, target, origin int64) *state.ForecastPoint {
	for i := range rows {
		r := &rows[i]
		length := r.SlotLenMin
		if length <= 0 {
			length = 60
		}
		if target >= r.SlotTsMs && target < r.SlotTsMs+int64(length)*60000 &&
			r.FetchedAtMs > 0 && r.FetchedAtMs <= origin && origin-r.FetchedAtMs <= mpc.ForecastMaxAge.Milliseconds() {
			return r
		}
	}
	return nil
}

func (f *forecastTracker) Snapshot(_ time.Time, weather []state.ForecastPoint) mpc.ForecastInputs {
	if f.refreshIdentity != nil {
		f.refreshIdentity()
	}
	if f.configMu != nil {
		f.configMu.RLock()
		defer f.configMu.RUnlock()
	}
	captureAt := f.now()
	site := f.site()
	if site.IdentityPending {
		// Keep persisted learning untouched until the running hardware proves its
		// binding. An explicit empty weather slice prevents fallback to old rows.
		return mpc.ForecastInputs{Weather: []state.ForecastPoint{}}
	}
	site.Options.ExpectedFlows = append([]telemetry.ForecastFlow(nil), site.Options.ExpectedFlows...)
	pv := f.pv.ForecastSnapshot()
	load := f.load.Snapshot()
	f.mu.RLock()
	history := f.errors
	observations := f.observations
	f.mu.RUnlock()
	candidateState := json.RawMessage(nil)
	if f.candidate != nil {
		candidateState = append(json.RawMessage(nil), f.candidate.Snapshot()...)
	}
	away := make(map[int64]bool)
	start := captureAt.Truncate(15 * time.Minute)
	for i := 0; i < 193; i++ {
		at := start.Add(time.Duration(i) * 15 * time.Minute)
		away[at.UnixMilli()] = f.away != nil && f.away(at)
	}
	frozen := cloneForecastRows(weather)
	if !site.HasPVScale {
		for i := range frozen {
			if frozen[i].Source != "forecast_solar" {
				frozen[i].PVWEstimated = nil
			}
		}
	}
	origin := f.now() // all models, occupancy and weather captured before inference
	frozen = usableTrackingWeather(frozen, origin.UnixMilli())
	if site.WeatherSinceMS > 0 {
		fresh := frozen[:0]
		for _, row := range frozen {
			if row.FetchedAtMs >= site.WeatherSinceMS {
				fresh = append(fresh, row)
			}
		}
		frozen = fresh
	}
	calibrator := forecasting.NewCalibrator(history, site.Revision, origin.UnixMilli())
	pvFn := mpc.PVPredictor(nil)
	if f.pv != nil && site.HasLocation {
		pvFn = func(t time.Time, cloud float64) float64 {
			return pv.Structural(t, forecast.ClearSkyWm2(site.Latitude, site.Longitude, t), cloud)
		}
	}
	loadFn := func(t time.Time) float64 {
		profile := load.ActiveProfile
		if away[t.Truncate(15*time.Minute).UnixMilli()] {
			profile = loadmodel.ProfileAway
		}
		m := load.Profiles[profile]
		temp := math.NaN()
		if row := forecastRow(frozen, t.UnixMilli(), origin.UnixMilli()); row != nil && row.TempC != nil {
			temp = *row.TempC
		}
		return m.Predict(t, temp)
	}
	in := mpc.ForecastInputs{PV: pvFn, PVResidualCorrect: pv.ResidualCorrect, Load: loadFn, Weather: frozen}
	in.PVWeight = func(t time.Time) float64 {
		if !site.HasPVScale {
			return 1
		}
		return mpc.PlannerRadiationWeight * pv.Model.Trust(t)
	}
	models := []forecasting.ModelState{}
	latest := int64(0)
	if f.pv != nil {
		data, _ := json.Marshal(struct {
			Model     pvmodel.Model                 `json:"model"`
			Residuals []pvmodel.ResidualObservation `json:"residuals"`
		}{pv.Model, pv.Residuals})
		updated := int64(0)
		quality := "cold_start"
		if !pv.LatestInput.IsZero() {
			updated = pv.LatestInput.UnixMilli()
			quality = forecasting.ModelQualityWarm
		}
		models = append(models, forecasting.ModelState{Name: "legacy_pv", Version: pv.Revision, UpdatedAtMS: updated, Quality: quality, State: data})
		latest = max(latest, updated)
	}
	for _, profile := range loadmodel.Profiles() {
		m, ok := load.Profiles[profile]
		if !ok {
			continue
		}
		data, _ := json.Marshal(m)
		quality := "cold_start"
		if m.LastMs > 0 {
			quality = forecasting.ModelQualityWarm
		}
		models = append(models, forecasting.ModelState{Name: "legacy_load_" + string(profile), Version: forecastHash(m),
			UpdatedAtMS: m.LastMs, Quality: quality, State: data})
		latest = max(latest, m.LastMs)
	}
	for _, r := range frozen {
		if r.FetchedAtMs <= origin.UnixMilli() {
			latest = max(latest, r.FetchedAtMs)
		}
	}
	for _, o := range observations {
		if o.ConfigVersion == site.Revision && o.AvailableAtMS <= origin.UnixMilli() {
			latest = max(latest, o.AvailableAtMS)
		}
	}
	for _, e := range history {
		if e.ConfigVersion == site.Revision && e.AvailableAtMS <= origin.UnixMilli() {
			latest = max(latest, e.AvailableAtMS)
		}
	}
	makeIssue := func(base []mpc.Slot, issued int64) forecasting.Issue {
		issue := forecasting.Issue{Schema: forecasting.Schema, ID: uuid.NewString(),
			OriginMS: origin.UnixMilli(), IssuedAtMS: max(issued, origin.UnixMilli()), LatestInputMS: latest, ConfigVersion: site.Revision, Site: forecastSiteContext(site), Models: cloneForecastModels(models)}
		for i := 0; i < 193; i++ {
			at := start.Add(time.Duration(i) * 15 * time.Minute).UnixMilli()
			issue.Occupancy = append(issue.Occupancy, forecasting.Occupancy{StartMS: at, EndMS: at + 900000, AvailableAtMS: issue.OriginMS, Home: !away[at]})
			issue.LatestInputMS = max(issue.LatestInputMS, issue.OriginMS)
		}
		for _, r := range frozen {
			if r.FetchedAtMs <= 0 || r.FetchedAtMs > issue.OriginMS {
				continue
			}
			length := r.SlotLenMin
			if length <= 0 {
				length = 60
			}
			var directPV *float64
			if r.Source == "forecast_solar" {
				directPV = r.PVWEstimated
			}
			issue.Weather = append(issue.Weather, forecasting.Weather{StartMS: r.SlotTsMs, EndMS: r.SlotTsMs + int64(length)*60000,
				AvailableAtMS: r.FetchedAtMs, Source: r.Source, GHIWm2: r.SolarWm2, CloudPct: r.CloudCoverPct, TempC: r.TempC, DirectPVW: directPV, EstimatedPVW: r.PVWEstimated})
		}
		issue.Series = append(issue.Series, forecasting.Series{Name: "champion", ModelVersion: forecastModelRevision(models), Points: forecastPoints(base, frozen, site, origin, load)})
		return issue
	}
	candidate := f.candidate
	var legacy []mpc.Slot
	var prediction *forecasting.Issue
	var selected []forecasting.Point
	in.Resolve = func(ctx context.Context, base []mpc.Slot) []mpc.Slot {
		legacy = append([]mpc.Slot(nil), base...)
		issued := makeIssue(base, origin.UnixMilli())
		selected = append([]forecasting.Point(nil), issued.Series[0].Points...)
		for i := range selected {
			selected[i].PVSource, selected[i].LoadSource = "legacy", "legacy"
		}
		prediction = nil
		if candidate == nil {
			return append([]mpc.Slot(nil), base...)
		}
		workCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		out, err := candidate.Predict(workCtx, site, issued, candidateState, away)
		if err == nil {
			err = workCtx.Err()
		}
		if err != nil {
			slog.Debug("primary forecast unavailable; using legacy", "err", err)
			return append([]mpc.Slot(nil), base...)
		}
		// The worker client validates its wire response. The host also binds the
		// completed forecast to this capture before it can alter planner inputs.
		if out.OriginMS != issued.OriginMS || out.ConfigVersion != issued.ConfigVersion || out.LatestInputMS > issued.OriginMS || len(out.Series) != 1 || out.Series[0].Name != "energyplan" {
			slog.Warn("primary forecast binding invalid; using legacy")
			return append([]mpc.Slot(nil), base...)
		}
		applyForecastBandsWith(&out, calibrator)
		if err := out.Validate(); err != nil {
			slog.Warn("primary forecast invalid; using legacy", "err", err)
			return append([]mpc.Slot(nil), base...)
		}
		prediction = &out
		resolved := append([]mpc.Slot(nil), base...)
		byStart := make(map[int64]forecasting.Point, len(out.Series[0].Points))
		for _, point := range out.Series[0].Points {
			byStart[point.StartMS] = point
		}
		for i, slot := range base {
			point, ok := byStart[slot.StartMs]
			if !ok || point.EndMS != slot.StartMs+int64(slot.LenMin)*60000 {
				continue
			}
			if point.StartMS < issued.OriginMS && point.PredictionStartMS != issued.OriginMS {
				continue
			}
			selected[i].PredictionStartMS = point.PredictionStartMS
			if usablePrimaryForecast(point.PVKnown, point.PVQuality, point.PVW) {
				resolved[i].PVW = -point.PVW
				selected[i].PVW, selected[i].PVKnown, selected[i].PVQuality = point.PVW, true, point.PVQuality
				selected[i].PVSource, selected[i].ModelPV = "energyplan", point.ModelPV
			}
			if usablePrimaryForecast(point.LoadKnown, point.LoadQuality, point.LoadW) {
				resolved[i].LoadW = point.LoadW
				selected[i].LoadW, selected[i].LoadKnown, selected[i].LoadQuality = point.LoadW, true, point.LoadQuality
				selected[i].LoadSource, selected[i].ModelLoad = "energyplan", point.ModelLoad
			}
		}
		return resolved
	}
	// Calibrate the actual composed primary. A Rust prediction does not inherit
	// the old model's residual spread or its training trust threshold.
	in.Risk = func(base, planning []mpc.Slot, k float64) {
		if k <= 0 || math.IsNaN(k) || math.IsInf(k, 0) {
			return
		}
		for i, s := range base {
			net := s.LoadW + s.PVW
			end := s.StartMs + int64(s.LenMin)*time.Minute.Milliseconds()
			band := calibrator.BandForInterval("champion", "net", max(s.StartMs, origin.UnixMilli()), end, net)
			if band.Method == forecasting.BandMethodEmpirical {
				extra := math.Max(0, k*(band.HighW-net))
				pvLoss := math.Min(-s.PVW, extra)
				planning[i].PVW = s.PVW + pvLoss
				planning[i].LoadW = s.LoadW + extra - pvLoss
			} else if i < len(selected) && selected[i].PVSource == "energyplan" {
				loss := .5 * (-s.PVW)
				if evidence := selected[i].ModelPV; evidence != nil {
					loss = math.Max(0, -s.PVW-evidence.LowerW)
				}
				planning[i].PVW = s.PVW + math.Min(-s.PVW, k*loss)
			} else {
				loss := .5 * (-s.PVW)
				if relative := pv.RelativeUncertainty(); relative > 0 {
					loss = relative * (-s.PVW)
				} else if absolute := pv.ResidualStdW(origin); absolute > 0 {
					loss = absolute
				}
				planning[i].PVW = s.PVW + math.Min(-s.PVW, k*loss)
			}
		}
	}
	in.Record = func(base, planning []mpc.Slot, decisionID string, issued int64) {
		issue := makeIssue(base, issued)
		issue.DecisionID = decisionID
		if prediction != nil {
			issue.Models = append(issue.Models, cloneForecastModels(prediction.Models)...)
			issue.LatestInputMS = max(issue.LatestInputMS, prediction.LatestInputMS)
		}
		version := forecastModelRevision(issue.Models)
		issue.Series[0].ModelVersion = version
		issue.Series[0].Points = primaryForecastPoints(base, issue.Series[0].Points, selected)
		issue.Series = append(issue.Series, forecasting.Series{Name: "planning", ModelVersion: version,
			Points: primaryForecastPoints(planning, forecastPoints(planning, frozen, site, origin, load), selected)})
		shadow := legacy
		if shadow == nil {
			shadow = base // Compatibility with callers that only archive legacy.
		}
		shadowPoints := forecastPoints(shadow, frozen, site, origin, load)
		for i := range shadowPoints {
			shadowPoints[i].PVSource, shadowPoints[i].LoadSource = "legacy", "legacy"
		}
		issue.Series = append(issue.Series, forecasting.Series{Name: "legacy_shadow", ModelVersion: forecastModelRevision(models), Points: shadowPoints})
		if prediction != nil {
			issue.Series = append(issue.Series, prediction.Series...)
		}
		issue.Series = append(issue.Series, forecastBaselines(base, frozen, observations, site, origin)...)
		applyForecastBandsWith(&issue, calibrator)
		job := forecastJob{issue: issue}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.stopped {
			slog.Warn("forecast archive stopped; issue unavailable for evaluation", "decision_id", decisionID)
			return
		}
		select {
		case f.queue <- job:
		default:
			slog.Warn("forecast archive queue full; issue unavailable for evaluation", "decision_id", decisionID)
		}
	}
	return in
}

func usablePrimaryForecast(known bool, quality string, watts float64) bool {
	return known && (quality == "cold_start" || quality == "learning" || quality == "ready") && watts >= 0 && !math.IsNaN(watts) && !math.IsInf(watts, 0)
}

// Preserve the source and quality used for each signal, while recording the
// actual capped or risk-adjusted watts passed through MPC.
func primaryForecastPoints(slots []mpc.Slot, fallback, selected []forecasting.Point) []forecasting.Point {
	for i, slot := range slots {
		fallback[i].PVSource, fallback[i].LoadSource = "legacy", "legacy"
		if i < len(selected) && selected[i].StartMS == slot.StartMs && selected[i].EndMS == slot.StartMs+int64(slot.LenMin)*60000 {
			fallback[i] = selected[i]
		}
		fallback[i].PVW, fallback[i].LoadW = math.Max(0, -slot.PVW), math.Max(0, slot.LoadW)
	}
	return fallback
}

func usableTrackingWeather(rows []state.ForecastPoint, origin int64) []state.ForecastPoint {
	out := make([]state.ForecastPoint, 0, len(rows))
	for _, r := range rows {
		length := r.SlotLenMin
		if length <= 0 {
			length = 60
		}
		if r.Source == "" || r.FetchedAtMs <= 0 || r.FetchedAtMs > origin || origin-r.FetchedAtMs > mpc.ForecastMaxAge.Milliseconds() || length > 60 {
			continue
		}
		if !forecastFinite(r.PVWEstimated) || (r.PVWEstimated != nil && *r.PVWEstimated < 0) {
			r.PVWEstimated = nil
		}
		if !forecastFinite(r.SolarWm2) || (r.SolarWm2 != nil && *r.SolarWm2 < 0) {
			r.SolarWm2 = nil
		}
		if !forecastFinite(r.CloudCoverPct) || (r.CloudCoverPct != nil && (*r.CloudCoverPct < 0 || *r.CloudCoverPct > 100)) {
			r.CloudCoverPct = nil
		}
		if !forecastFinite(r.TempC) {
			r.TempC = nil
		}
		out = append(out, r)
	}
	return out
}

func cloneForecastRows(rows []state.ForecastPoint) []state.ForecastPoint {
	out := append([]state.ForecastPoint(nil), rows...)
	copyNumber := func(v *float64) *float64 {
		if v == nil {
			return nil
		}
		copy := *v
		return &copy
	}
	for i := range out {
		r := &out[i]
		r.CloudCoverPct = copyNumber(r.CloudCoverPct)
		r.TempC = copyNumber(r.TempC)
		r.SolarWm2 = copyNumber(r.SolarWm2)
		r.PVWEstimated = copyNumber(r.PVWEstimated)
	}
	return out
}
func cloneForecastModels(models []forecasting.ModelState) []forecasting.ModelState {
	out := append([]forecasting.ModelState(nil), models...)
	for i := range out {
		out[i].State = append(json.RawMessage(nil), out[i].State...)
	}
	return out
}
func forecastModelRevision(models []forecasting.ModelState) string {
	revisions := make(map[string]string, len(models))
	for _, m := range models {
		revisions[m.Name] = m.Version
	}
	return forecastHash(revisions)
}

func forecastFinite(v *float64) bool { return v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0) }
func forecastPVKnown(row *state.ForecastPoint, site forecastSite) bool {
	if row == nil {
		return false
	}
	if row.Source == "forecast_solar" {
		return forecastFinite(row.PVWEstimated) && *row.PVWEstimated >= 0
	}
	if !site.HasLocation || !site.HasPVScale {
		return false
	}
	return (forecastFinite(row.CloudCoverPct) && *row.CloudCoverPct >= 0 && *row.CloudCoverPct <= 100) || (forecastFinite(row.SolarWm2) && *row.SolarWm2 >= 0)
}
func forecastPoints(slots []mpc.Slot, weather []state.ForecastPoint, site forecastSite, origin time.Time, loads ...loadmodel.Snapshot) []forecasting.Point {
	out := make([]forecasting.Point, 0, len(slots))
	for _, s := range slots {
		row := forecastRow(weather, s.StartMs, origin.UnixMilli())
		known := forecastPVKnown(row, site)
		quality := "pv_input_unknown"
		if known {
			quality = "weather_model"
			if row.Source == "forecast_solar" {
				quality = "weather_direct"
			}
		}
		loadQuality := "cold_start"
		if len(loads) > 0 {
			snap := loads[0]
			m := snap.Profiles[snap.ActiveProfile]
			if m.LastMs > 0 {
				loadQuality = "learning"
				if m.Coverage(time.UnixMilli(s.StartMs)) == 0 {
					loadQuality = "uncovered"
				}
			}
		}
		predictionStart := int64(0)
		if s.StartMs < origin.UnixMilli() && origin.UnixMilli() < s.StartMs+int64(s.LenMin)*60000 {
			predictionStart = origin.UnixMilli()
		}
		out = append(out, forecasting.Point{PredictionStartMS: predictionStart, StartMS: s.StartMs, EndMS: s.StartMs + int64(s.LenMin)*60000,
			PVW: math.Max(0, -s.PVW), LoadW: math.Max(0, s.LoadW), PVKnown: known, LoadKnown: true,
			PVQuality: quality, LoadQuality: loadQuality})
	}
	return out
}

// baselineObservation averages a fully covered source interval by energy.
// It never stretches one quarter's observation across an hour or fills a gap.
func baselineObservation(obs []forecasting.Observation, site forecastSite, start, end, origin int64) (forecasting.Observation, bool) {
	return coverBaseline(indexBaselineObservations(obs, site, origin), start, end, site.Revision)
}
func indexBaselineObservations(obs []forecasting.Observation, site forecastSite, origin int64) map[int64]forecasting.Observation {
	byStart := make(map[int64]forecasting.Observation)
	for _, o := range obs {
		if o.ConfigVersion != site.Revision || o.AvailableAtMS > origin || o.Validate() != nil {
			continue
		}
		if old, ok := byStart[o.StartMS]; !ok || o.EndMS > old.EndMS {
			byStart[o.StartMS] = o
		}
	}
	return byStart
}
func coverBaseline(byStart map[int64]forecasting.Observation, start, end int64, revision string) (forecasting.Observation, bool) {
	out := forecasting.Observation{StartMS: start, EndMS: end, PVKnown: true, LoadKnown: true, ConfigVersion: revision, Quality: "observed_baseline"}
	var pvEnergy, loadEnergy float64
	for cursor := start; cursor < end; {
		o, ok := byStart[cursor]
		if !ok || o.EndMS <= cursor || o.EndMS > end {
			return forecasting.Observation{}, false
		}
		duration := float64(o.EndMS - o.StartMS)
		pvEnergy += o.PVW * duration
		loadEnergy += o.LoadW * duration
		out.PVKnown = out.PVKnown && o.PVKnown
		out.LoadKnown = out.LoadKnown && o.LoadKnown
		out.AvailableAtMS = max(out.AvailableAtMS, o.AvailableAtMS)
		cursor = o.EndMS
	}
	if end <= start {
		return forecasting.Observation{}, false
	}
	out.PVW = pvEnergy / float64(end-start)
	out.LoadW = loadEnergy / float64(end-start)
	return out, true
}
func baselineDayShift(target int64, days int, zone string) (int64, bool) {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		loc = time.UTC
	}
	return baselineDayShiftAt(target, days, loc)
}
func baselineDayShiftAt(target int64, days int, loc *time.Location) (int64, bool) {
	local := time.UnixMilli(target).In(loc)
	shifted := local.AddDate(0, 0, days)
	// A spring-forward wall hour may not exist on the source day.
	if shifted.Hour() != local.Hour() || shifted.Minute() != local.Minute() {
		return 0, false
	}
	return shifted.UnixMilli(), true
}
func forecastBaselines(slots []mpc.Slot, weather []state.ForecastPoint, obs []forecasting.Observation, site forecastSite, origin time.Time) []forecasting.Series {
	out := []forecasting.Series{}
	index := indexBaselineObservations(obs, site, origin.UnixMilli())
	loc, err := time.LoadLocation(site.Timezone)
	if err != nil {
		loc = time.UTC
	}
	for _, name := range []string{"weather_prior", "previous_day", "previous_week", "persistence"} {
		series := forecasting.Series{Name: name, ModelVersion: "baseline-v2-local-interval"}
		for _, s := range slots {
			p := forecasting.Point{StartMS: s.StartMs, EndMS: s.StartMs + int64(s.LenMin)*60000, PVQuality: "missing", LoadQuality: "missing"}
			if p.StartMS < origin.UnixMilli() && origin.UnixMilli() < p.EndMS {
				p.PredictionStartMS = origin.UnixMilli()
			}
			switch name {
			case "weather_prior":
				if row := forecastRow(weather, p.StartMS, origin.UnixMilli()); forecastPVKnown(row, site) && forecastFinite(row.PVWEstimated) {
					p.PVW = math.Max(0, *row.PVWEstimated)
					p.PVKnown = true
					p.PVQuality = "weather"
				}
				p.LoadW = s.LoadW
				p.LoadKnown = true
				p.LoadQuality = "champion_load"
			default:
				target, valid := p.StartMS, true
				if name == "previous_day" {
					target, valid = baselineDayShiftAt(p.StartMS, -1, loc)
				}
				if name == "previous_week" {
					target, valid = baselineDayShiftAt(p.StartMS, -7, loc)
				}
				length := p.EndMS - p.StartMS
				if name == "persistence" {
					// The most recent complete interval of the same duration is available
					// even if observation delivery lagged the latest quarter boundary.
					target = 0
					for _, o := range obs {
						if o.ConfigVersion == site.Revision && o.AvailableAtMS <= origin.UnixMilli() && o.EndMS <= origin.UnixMilli() && o.EndMS-length > target {
							target = o.EndMS - length
						}
					}
					valid = target > 0
				}
				if valid {
					if o, ok := coverBaseline(index, target, target+length, site.Revision); ok {
						p.PVW, p.LoadW, p.PVKnown, p.LoadKnown = o.PVW, o.LoadW, o.PVKnown, o.LoadKnown
						p.PVQuality, p.LoadQuality = "observed_baseline", "observed_baseline"
					}
				}
			}
			series.Points = append(series.Points, p)
		}
		out = append(out, series)
	}
	return out
}

func applyForecastBandsWith(issue *forecasting.Issue, calibrator *forecasting.Calibrator) {
	for i := range issue.Series {
		s := &issue.Series[i]
		for j := range s.Points {
			p := &s.Points[j]
			start := max(p.StartMS, p.PredictionStartMS)
			p.PVBand = calibrator.BandForInterval(s.Name, "pv", start, p.EndMS, p.PVW)
			p.LoadBand = calibrator.BandForInterval(s.Name, "load", start, p.EndMS, p.LoadW)
			p.NetBand = calibrator.BandForInterval(s.Name, "net", start, p.EndMS, p.LoadW-p.PVW)
		}
	}
}
func applyForecastBands(issue *forecasting.Issue, history []forecasting.ErrorSample) {
	applyForecastBandsWith(issue, forecasting.NewCalibrator(history, issue.ConfigVersion, issue.OriginMS))
}

func forecastSiteContext(site forecastSite) *forecasting.SiteContext {
	if !site.HasLocation {
		site.Latitude, site.Longitude = 0, 0
	}
	if site.SiteID == "" || site.Timezone == "" {
		return nil
	}
	return &forecasting.SiteContext{SiteID: site.SiteID, LearningRevision: rustConfigRevision(site), Timezone: site.Timezone,
		HasLocation: site.HasLocation, Latitude: site.Latitude, Longitude: site.Longitude}
}
