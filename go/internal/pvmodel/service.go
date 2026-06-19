package pvmodel

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/state"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// stateKey is the config k/v key where we persist the model JSON.
// The `_utc` suffix invalidates pre-UTC-coercion models: learned β
// coefficients were fitted against local-zone hour-of-day harmonic
// features and would silently misalign if restored under the current
// UTC-based Features(). Fresh init + ~50 samples retrains.
const stateKey = "pvmodel/state_utc"

// ClearSkyFunc is injected by main.go to decouple pvmodel from the
// forecast package. Returns clear-sky GHI (W/m²) for the site's lat/lon
// baked into the closure.
type ClearSkyFunc func(t time.Time) float64

// CloudFunc returns the cloud-cover percentage (0..100) for a given
// time, based on the latest forecast. Returns (value, ok) where ok is
// false if no forecast covers `t`.
type CloudFunc func(t time.Time) (float64, bool)

// Service owns the online-learning loop for the PV twin. It samples
// measured PV telemetry once per SampleInterval, pulls the matching
// clear-sky + cloud values for that instant, and runs one RLS update.
// The model is persisted to state every PersistEvery samples.
type Service struct {
	Store          *state.Store
	Tele           *telemetry.Store
	ClearSky       ClearSkyFunc
	Cloud          CloudFunc
	SampleInterval time.Duration
	PersistEvery   int64 // samples between SQLite writes

	mu        sync.RWMutex
	model     *Model
	persistMu sync.Mutex // serialises SQLite writes so a stale persist can't clobber a Reset

	// Residuals captures (predicted_at_t, actual_at_t) pairs to compute a
	// short-horizon additive correction the MPC applies on top of the
	// structural prediction. See residual.go for the math + gates. Lives
	// for the lifetime of the Service; cleared by Reset().
	Residuals *ResidualBuffer

	stop chan struct{}
	done chan struct{}
}

// NewService constructs the service. If model state exists in the DB,
// it's restored; otherwise a fresh prior is initialized using ratedW.
func NewService(st *state.Store, tel *telemetry.Store, cs ClearSkyFunc, cf CloudFunc, ratedW float64) *Service {
	s := &Service{
		Store:          st,
		Tele:           tel,
		ClearSky:       cs,
		Cloud:          cf,
		SampleInterval: 60 * time.Second,
		PersistEvery:   10,
		Residuals:      NewResidualBuffer(),
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
	}
	if st != nil {
		if js, ok := st.LoadConfig(stateKey); ok && js != "" {
			var m Model
			if err := json.Unmarshal([]byte(js), &m); err == nil && m.Forgetting > 0 {
				m.RatedW = ratedW // config may have changed rated value
				// Migrate pre-#134 persisted models: Beta[0] was a free
				// intercept that drifted during training and leaked into
				// night predictions. Features() now holds x[0]=0, making
				// the slot a dead coefficient — zero it on load so any
				// drifted value doesn't linger until the first Update()
				// self-heal kicks in.
				m.Beta[0] = 0
				s.model = &m
				slog.Info("pvmodel restored", "samples", m.Samples, "mae_w", m.MAE, "quality", m.Quality())
			}
		}
	}
	if s.model == nil {
		s.model = NewModel(ratedW)
	}
	return s
}

// Model returns the current model (safe for reads; copies are cheap).
func (s *Service) Model() Model {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return *s.model
}

// SetRated updates the array nameplate (W) used by the model's output
// envelope, input outlier guards, and cold-start prior. Learned RLS
// coefficients are NOT reset — the twin has already adapted to reality
// so the learned fit stays more accurate than a fresh prior. Call
// `POST /api/pvmodel/reset` separately if the array itself changed
// and you want the model to re-seed.
func (s *Service) SetRated(w float64) {
	if s == nil || w <= 0 {
		return
	}
	s.mu.Lock()
	prev := s.model.RatedW
	s.model.RatedW = w
	s.mu.Unlock()
	if prev != w {
		slog.Info("pvmodel rated updated", "old_w", prev, "new_w", w)
	}
}

// PredictStructural returns the RLS-driven prediction WITHOUT the
// now-anchor live-telemetry correction. This is the surface the MPC and
// residual-buffer sampler consume: the residual buffer measures and
// corrects exactly the same structural-vs-live bias the now-anchor
// would otherwise apply, so layering both produces a double-correction
// bug (PR #381 follow-up). Keep `Predict` (anchored) for any path that
// wants the live-blended prediction (UI overlays, dispatch "now" gate).
func (s *Service) PredictStructural(t time.Time, cloudPct float64) float64 {
	if s == nil {
		return 0
	}
	cs := s.ClearSky(t)
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.model.Predict(cs, cloudPct, t)
}

// Predict is the main integration point for the UI + dispatch live-reading
// path: it returns the twin's prediction with the live-telemetry "now
// anchor" folded in. The MPC and the residual-buffer sampler should use
// PredictStructural instead — see that method's doc for why.
//
// Near-future predictions are anchored to live telemetry: if the model's
// "what PV should be doing right now" disagrees with what PV IS doing
// right now, a multiplicative correction is applied to the base
// prediction and decayed linearly over NowAnchorHorizon so the
// correction fades as the prediction reaches further into the future
// (weather shifts blur the correction). See applyNowAnchor for the math.
//
// This guards against systematically-wrong forecasts (met.no predicts
// cloudy, sky is clear) that the RLS would need ~50 samples to learn
// away — the twin should react to reality *now*, not in an hour.
func (s *Service) Predict(t time.Time, cloudPct float64) float64 {
	if s == nil {
		return 0
	}
	cs := s.ClearSky(t)
	s.mu.RLock()
	basePred := s.model.Predict(cs, cloudPct, t)
	rated := s.model.RatedW
	s.mu.RUnlock()

	actualNow, ok := s.liveActualPV()
	if !ok {
		return basePred
	}
	now := time.Now()
	cloudNow := 50.0
	if s.Cloud != nil {
		if v, ok := s.Cloud(now); ok {
			cloudNow = v
		}
	}
	csNow := s.ClearSky(now)
	s.mu.RLock()
	priorNow := s.model.Predict(csNow, cloudNow, now)
	s.mu.RUnlock()

	anchored := applyNowAnchor(basePred, priorNow, actualNow, t.Sub(now))
	if rated > 0 && anchored > rated {
		anchored = rated
	}
	return anchored
}

// NowAnchorHorizon is how far into the future the "reality vs model"
// correction from applyNowAnchor is trusted. At t=now the correction
// is applied in full; at t=now+NowAnchorHorizon it fades to zero so
// the base model (which reflects what we've learned about this site's
// diurnal + cloud pattern) takes over for longer-horizon decisions.
//
// 2 hours is a compromise: long enough to cover the MPC's near-term
// dispatch decisions (the current + next slot), short enough that a
// weather front rolling in at t=now doesn't distort the full-day plan.
const NowAnchorHorizon = 2 * time.Hour

// NowAnchorClamp bounds the multiplicative correction to [1/x, x] so
// a momentary inverter restart (actualNow ~ 0) or sensor spike can't
// slash or explode the prediction. The asymmetric tolerance comes from
// the outlier rejection in model.Update — the upward cap (5×) matches
// the rated-power sanity envelope, the downward floor (1/5 = 0.2) keeps
// predictions sane when a fleeting 0 W reading sneaks through.
const NowAnchorClamp = 5.0

// applyNowAnchor returns basePred multiplied by a decayed (actual/prior)
// correction. Pure function — no I/O. All guards live here so the
// caller stays simple and tests can exercise every edge case without
// wiring a telemetry store.
//
//   basePred : model.Predict(t, cloudPct_t)           — in W
//   priorNow : model.Predict(now, cloudPct_now)       — in W
//   actualNow: summed live PV telemetry right now     — in W (≥ 0)
//   dt       : t − now (signed)
//
// Rules:
//   - dt > NowAnchorHorizon → no correction (return basePred).
//   - dt < 0 → treated as 0 (slot already started; full correction).
//     This also tolerates the microsecond drift between the caller's
//     time.Now() and the one we read inside Predict.
//   - priorNow < 50 W → no correction (night / near-night, ratio meaningless).
//   - actualNow < 50 W → no correction (driver outage or near-night).
//   - correction = actualNow / priorNow, clamped to [1/NowAnchorClamp, NowAnchorClamp].
//   - decay = 1 − dt/NowAnchorHorizon, clamped to [0, 1].
//   - result = basePred × (1 + (correction − 1) × decay), floored at 0.
func applyNowAnchor(basePred, priorNow, actualNow float64, dt time.Duration) float64 {
	if basePred < 0 {
		basePred = 0
	}
	dtS := dt.Seconds()
	horS := NowAnchorHorizon.Seconds()
	if dtS > horS {
		return basePred
	}
	if dtS < 0 {
		dtS = 0
	}
	if priorNow < 50 || actualNow < 50 {
		return basePred
	}
	correction := actualNow / priorNow
	if correction > NowAnchorClamp {
		correction = NowAnchorClamp
	}
	if correction < 1.0/NowAnchorClamp {
		correction = 1.0 / NowAnchorClamp
	}
	decay := 1.0 - dtS/horS
	if decay < 0 {
		decay = 0
	}
	if decay > 1 {
		decay = 1
	}
	anchored := basePred * (1 + (correction-1)*decay)
	if anchored < 0 {
		anchored = 0
	}
	return anchored
}

// liveActualPV sums SmoothedW across every PV reading, flipping site-
// sign to produce a non-negative generation value. Mirrors sample().
// Returns (value, false) when nothing's reporting — so Predict falls
// back to pure-model behavior instead of pretending we saw 0 W.
func (s *Service) liveActualPV() (float64, bool) {
	if s.Tele == nil {
		return 0, false
	}
	var pvW float64
	count := 0
	for _, r := range s.Tele.ReadingsByType(telemetry.DerPV) {
		if r.SmoothedW < 0 {
			pvW += -r.SmoothedW
			count++
		}
	}
	if count == 0 || pvW < 1 {
		return 0, false
	}
	return pvW, true
}

// PredictNow returns the twin's prediction for right now using the
// latest cloud cover from the forecast cache. Used by the UI to
// overlay predicted vs actual PV on the live chart.
func (s *Service) PredictNow() float64 {
	if s == nil {
		return 0
	}
	now := time.Now()
	cloud := 50.0
	if s.Cloud != nil {
		if v, ok := s.Cloud(now); ok {
			cloud = v
		}
	}
	return s.Predict(now, cloud)
}

// Start begins the online-learning loop. Safe to call multiple times.
func (s *Service) Start(ctx context.Context) {
	if s == nil {
		return
	}
	go s.loop(ctx)
}

// Stop terminates the learner + flushes a final persist.
func (s *Service) Stop() {
	if s == nil {
		return
	}
	close(s.stop)
	<-s.done
}

func (s *Service) loop(ctx context.Context) {
	defer close(s.done)
	t := time.NewTicker(s.SampleInterval)
	defer t.Stop()
	// Initial sample so we don't wait a full interval.
	s.sample()
	for {
		select {
		case <-s.stop:
			s.persist()
			return
		case <-ctx.Done():
			s.persist()
			return
		case <-t.C:
			s.sample()
		}
	}
}

// sample reads current PV telemetry, pulls current clear-sky + cloud,
// and runs one RLS update.
func (s *Service) sample() {
	now := time.Now()
	cs := s.ClearSky(now)
	if cs < 50 {
		slog.Debug("pvmodel: skip (night)", "cs", cs)
		return // night / near-night — no signal
	}
	cloud := 50.0 // neutral fallback if no forecast row
	if s.Cloud != nil {
		if v, ok := s.Cloud(now); ok {
			cloud = v
		}
	}
	// Aggregate PV across all drivers. PV telemetry is stored as
	// site-sign (negative = generating), so flip to positive.
	var pvW float64
	readings := s.Tele.ReadingsByType(telemetry.DerPV)
	for _, r := range readings {
		if r.SmoothedW < 0 {
			pvW += -r.SmoothedW
		}
	}
	// Guard: if all drivers report 0 when there's meaningful clear-sky,
	// that's likely a driver outage — skip so we don't learn "0 output".
	if pvW < 1 {
		slog.Debug("pvmodel: skip (no PV reading)", "readings", len(readings), "cs", cs)
		return
	}

	// Capture (predicted_at_now, actual_at_now) for the residual buffer
	// BEFORE running the RLS update — otherwise the model's prediction
	// already incorporates the sample we're about to use as ground
	// truth, and the residual collapses to ~0. Predict against the
	// structural model only (no now-anchor) since the residual layer
	// is correcting that structural output. The Residuals buffer
	// itself applies the gates / fade / variance check.
	s.mu.Lock()
	predicted := s.model.Predict(cs, cloud, now)
	updated := s.model.Update(cs, cloud, now, pvW)
	samples := s.model.Samples
	mae := s.model.MAE
	s.mu.Unlock()

	if s.Residuals != nil {
		s.Residuals.Add(now, predicted, pvW)
	}

	slog.Info("pvmodel: sample", "cs_wm2", cs, "cloud_pct", cloud, "pv_w", pvW, "samples", samples, "mae_w", mae, "updated", updated)

	if updated && samples%s.PersistEvery == 0 {
		s.persist()
	}
}

func (s *Service) persist() {
	if s.Store == nil {
		return
	}
	// Serialise the entire marshal+save so a sample-loop persist that
	// started before a Reset cannot finish after Reset's persist and
	// clobber the clean state with stale coefficients.
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	s.mu.RLock()
	js, err := json.Marshal(s.model)
	s.mu.RUnlock()
	if err != nil {
		return
	}
	if err := s.Store.SaveConfig(stateKey, string(js)); err != nil {
		slog.Warn("pvmodel persist", "err", err)
	}
}

// Reset clears the model to a fresh prior (useful after a system change
// — new panels, cleaning, etc.). Also clears the residual buffer so a
// stale set of "old-model" residuals doesn't bias the fresh prior's
// predictions during the cold-start blend.
func (s *Service) Reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	rated := s.model.RatedW
	s.model = NewModel(rated)
	s.Residuals = NewResidualBuffer()
	s.mu.Unlock()
	s.persist()
}

// ResidualCorrect is the integration point for the MPC. Returns the
// additive correction (W) to apply to a base prediction targeting
// tTarget, computed at wall time `now`. Returns 0 when residuals are
// unavailable, insufficient, or noise-dominated. See ResidualBuffer.Correct.
//
// Sign convention: this returns generation-positive W (same as the
// underlying pvmodel.Predict). Callers consuming site-sign PV (e.g.
// mpc.buildSlots which negates) should match the sign at their boundary.
func (s *Service) ResidualCorrect(now, tTarget time.Time, basePrediction float64) float64 {
	if s == nil || s.Residuals == nil {
		return 0
	}
	return s.Residuals.Correct(now, tTarget, basePrediction)
}

// ResidualDiagSnapshot returns the current residual-buffer state for
// /api/pvmodel diagnostics. Zero-valued when the buffer is empty.
func (s *Service) ResidualDiagSnapshot() ResidualDiag {
	if s == nil || s.Residuals == nil {
		return ResidualDiag{WindowMinutes: int(ResidualBufferWindow.Minutes())}
	}
	return s.Residuals.Diag(time.Now())
}

// ResidualStdW returns the std (W) of recent PV-prediction residuals — the
// live PV forecast-error magnitude. The MPC uses it as σ for downside-PV
// safety planning (forecast − k·σ). 0 when there's no error history yet
// (no hedge until the twin has learned how wrong it tends to be).
func (s *Service) ResidualStdW() float64 {
	return s.ResidualDiagSnapshot().StdW
}
