package loadmodel

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// TempFunc returns outdoor temperature (°C) for a given time, (value, ok).
// Same shape as pvmodel.CloudFunc — injected by main.go to decouple this
// package from the forecast module.
type TempFunc func(t time.Time) (float64, bool)

// legacyStateKey was bumped from "loadmodel/state" after HourOfWeek
// switched to UTC coercion. It remains as the one-time migration source
// for the default home profile.
const legacyStateKey = "loadmodel/state_utc"

const (
	stateKeyPrefix  = "loadmodel/state_utc:"
	profileStateKey = "loadmodel/profile"
)

func stateKey(profile Profile) string { return stateKeyPrefix + string(profile) }

// ParseProfile normalizes a user/API supplied load-model profile.
func ParseProfile(v string) (Profile, bool) {
	p := Profile(strings.ToLower(strings.TrimSpace(v)))
	return p, p.valid()
}

// Snapshot is a concurrency-safe copy of the service state.
type Snapshot struct {
	ActiveProfile Profile           `json:"active_profile"`
	Profiles      map[Profile]Model `json:"profiles"`
}

// Service trains the load model online from telemetry. Mirrors
// pvmodel.Service so operators + future code have one pattern.
type Service struct {
	Store          *state.Store
	Tele           *telemetry.Store
	SiteMeter      string   // driver name that carries the site's grid meter
	Temp           TempFunc // optional outdoor-temp source (forecast)
	SampleInterval time.Duration
	PersistEvery   int64

	mu     sync.RWMutex
	active Profile
	models map[Profile]*Model

	stop chan struct{}
	done chan struct{}
}

// NewService constructs + restores from state if present.
func NewService(st *state.Store, tel *telemetry.Store, siteMeter string, peakW float64) *Service {
	s := &Service{
		Store:          st,
		Tele:           tel,
		SiteMeter:      siteMeter,
		SampleInterval: 60 * time.Second,
		PersistEvery:   10,
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
		active:         ProfileHome,
		models:         make(map[Profile]*Model),
	}
	for _, profile := range Profiles() {
		s.models[profile] = newProfileModel(peakW, profile)
	}
	if st != nil {
		loadedProfiles := make(map[Profile]bool)
		if v, ok := st.LoadConfig(profileStateKey); ok {
			if profile, valid := ParseProfile(v); valid {
				s.active = profile
			}
		}
		for _, profile := range Profiles() {
			if js, ok := st.LoadConfig(stateKey(profile)); ok && js != "" {
				if m, ok := restoreModel(js, peakW, profile); ok {
					s.models[profile] = m
					loadedProfiles[profile] = true
					slog.Info("loadmodel restored",
						"profile", profile, "samples", m.Samples,
						"mae_w", m.MAE, "quality", m.Quality())
				}
			}
		}
		if js, ok := st.LoadConfig(legacyStateKey); ok && js != "" {
			var m Model
			if err := json.Unmarshal([]byte(js), &m); err == nil && m.Alpha > 0 {
				if !loadedProfiles[ProfileHome] {
					m.PeakW = peakW // config may have changed
					if m.PriorScale <= 0 {
						m.PriorScale = 1
					}
					s.models[ProfileHome] = &m
					slog.Info("loadmodel migrated legacy state",
						"profile", ProfileHome, "samples", m.Samples,
						"mae_w", m.MAE, "quality", m.Quality())
				}
			}
		}
	}
	return s
}

// SetSiteMeter swaps the grid-boundary driver used for future training
// samples. It is safe to call from config hot reload while the sample loop is
// running.
func (s *Service) SetSiteMeter(name string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.SiteMeter = name
	s.mu.Unlock()
}

func restoreModel(js string, peakW float64, profile Profile) (*Model, bool) {
	var m Model
	if err := json.Unmarshal([]byte(js), &m); err != nil || m.Alpha <= 0 {
		return nil, false
	}
	m.PeakW = peakW // config may have changed
	if m.PriorScale <= 0 {
		m.PriorScale = newProfileModel(peakW, profile).PriorScale
	}
	// Repair any bucket means that were poisoned by the pre-guard bug where
	// heating-subtracted samples were clamped to 0 and stored in the EMA.
	m.repairPoisonedBuckets()
	return &m, true
}

// Model returns a snapshot.
func (s *Service) Model() Model {
	if s == nil {
		return Model{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return *s.activeModelLocked()
}

// Snapshot returns all profile models plus the active profile.
func (s *Service) Snapshot() Snapshot {
	if s == nil {
		return Snapshot{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := Snapshot{
		ActiveProfile: s.active,
		Profiles:      make(map[Profile]Model, len(s.models)),
	}
	for _, profile := range Profiles() {
		if m := s.models[profile]; m != nil {
			out.Profiles[profile] = *m
		}
	}
	return out
}

// Profile returns the currently active load-model profile.
func (s *Service) Profile() Profile {
	if s == nil {
		return ProfileHome
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.active
}

// SetProfile changes which profile trains and predicts from now on.
func (s *Service) SetProfile(profile Profile) error {
	if s == nil {
		return nil
	}
	if !profile.valid() {
		return fmt.Errorf("unknown load profile: %s", profile)
	}
	s.mu.Lock()
	if _, ok := s.models[profile]; !ok {
		peak := s.activeModelLocked().PeakW
		s.models[profile] = newProfileModel(peak, profile)
	}
	s.active = profile
	s.mu.Unlock()
	return s.persistProfile(profile)
}

func (s *Service) activeModelLocked() *Model {
	if m := s.models[s.active]; m != nil {
		return m
	}
	if m := s.models[ProfileHome]; m != nil {
		return m
	}
	return NewModel(5000)
}

// Predict is the MPC's integration point — expected load at time t.
// If a temperature source is wired, the heating-gain correction is
// included; otherwise we predict assuming indoor setpoint (no heating).
func (s *Service) Predict(t time.Time) float64 {
	if s == nil {
		return 0
	}
	temp := HeatingReferenceC
	if s.Temp != nil {
		if v, ok := s.Temp(t); ok {
			temp = v
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.activeModelLocked().Predict(t, temp)
}

// PredictWith is like Predict but forces a specific profile's model,
// regardless of which profile is currently active. The calendar service
// (issue #498) uses it to predict reduced "away"-profile load for the slots
// inside a future away interval while the live active profile still tracks
// "now" — so an MPC horizon that crosses in and out of an away window gets
// the right per-slot load. An unknown or not-yet-trained profile falls back
// to the active model.
func (s *Service) PredictWith(t time.Time, profile Profile) float64 {
	if s == nil {
		return 0
	}
	if !profile.valid() {
		return s.Predict(t)
	}
	temp := HeatingReferenceC
	if s.Temp != nil {
		if v, ok := s.Temp(t); ok {
			temp = v
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := s.models[profile]
	if m == nil {
		m = s.activeModelLocked()
	}
	return m.Predict(t, temp)
}

// Start kicks off the online-learning goroutine.
func (s *Service) Start(ctx context.Context) {
	if s == nil {
		return
	}
	go s.loop(ctx)
}

// Stop terminates the learner + persists once.
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
	s.sample()
	for {
		select {
		case <-s.stop:
			if err := s.persist(); err != nil {
				slog.Warn("loadmodel persist", "err", err)
			}
			return
		case <-ctx.Done():
			if err := s.persist(); err != nil {
				slog.Warn("loadmodel persist", "err", err)
			}
			return
		case <-t.C:
			s.sample()
		}
	}
}

func (s *Service) driverOnline(name string) bool {
	if s == nil || s.Tele == nil {
		return false
	}
	h := s.Tele.DriverHealth(name)
	return h != nil && h.IsOnline()
}

// sample computes measured house load = grid_w - pv_w - bat_w - ev_w - v2x_w
// and feeds it to the model. Skips when drivers haven't settled yet
// (no site meter reading). EV is subtracted so the weekly-pattern
// learner tracks house consumption, not "house + occasional 10 kWh
// car session"; V2X is also subtracted because it is vehicle storage,
// not household demand, whether charging or discharging.
func (s *Service) sample() {
	s.sampleAt(time.Now())
}

func (s *Service) sampleAt(now time.Time) {
	s.mu.RLock()
	siteMeter := s.SiteMeter
	s.mu.RUnlock()
	meter := s.Tele.Get(siteMeter, telemetry.DerMeter)
	if meter == nil {
		slog.Debug("loadmodel: skip (no site meter yet)")
		return
	}
	if !s.driverOnline(siteMeter) {
		slog.Debug("loadmodel: skip (site meter offline)", "driver", siteMeter)
		return
	}
	gridW := meter.SmoothedW
	var pvW, batW float64
	for _, r := range s.Tele.ReadingsByType(telemetry.DerPV) {
		if !s.driverOnline(r.Driver) {
			continue
		}
		pvW += r.SmoothedW // site-sign: negative = generating
	}
	for _, r := range s.Tele.ReadingsByType(telemetry.DerBattery) {
		if !s.driverOnline(r.Driver) {
			continue
		}
		batW += r.SmoothedW // site-sign: positive = charging
	}
	evW := s.Tele.SumOnlineEVW()   // online-only so stale readings don't poison load
	v2xW := s.Tele.SumOnlineV2XW() // signed: +charging, -discharging
	loadW := gridW - pvW - batW - evW - v2xW
	if loadW < 0 {
		// Almost always a transient — during a PI step the measured
		// flow can briefly appear negative. Skip rather than train
		// on a physically impossible value.
		slog.Debug("loadmodel: skip (neg load)", "grid_w", gridW, "pv_w", pvW, "bat_w", batW, "ev_w", evW, "v2x_w", v2xW)
		return
	}

	// Outdoor temp for heating-fit. HeatingReferenceC = "no contribution".
	temp := HeatingReferenceC
	if s.Temp != nil {
		if v, ok := s.Temp(now); ok {
			temp = v
		}
	}

	s.mu.Lock()
	profile := s.active
	model := s.activeModelLocked()
	updated := model.Update(now, loadW, temp)
	samples := model.Samples
	mae := model.MAE
	heating := model.HeatingW_per_degC
	s.mu.Unlock()

	slog.Info("loadmodel: sample",
		"profile", profile, "load_w", loadW, "temp_c", temp,
		"samples", samples, "mae_w", mae,
		"heat_w_per_c", heating, "updated", updated)

	if updated && samples%s.PersistEvery == 0 {
		if err := s.persist(); err != nil {
			slog.Warn("loadmodel persist", "err", err)
		}
	}
}

func (s *Service) persistProfile(profile Profile) error {
	if s.Store == nil {
		return nil
	}
	return s.Store.SaveConfig(profileStateKey, string(profile))
}

func (s *Service) persist() error {
	if s.Store == nil {
		return nil
	}
	s.mu.RLock()
	active := s.active
	models := make(map[Profile]string, len(s.models))
	for _, profile := range Profiles() {
		if s.models[profile] == nil {
			continue
		}
		js, err := json.Marshal(s.models[profile])
		if err != nil {
			s.mu.RUnlock()
			return err
		}
		models[profile] = string(js)
	}
	s.mu.RUnlock()
	if err := s.Store.SaveConfig(profileStateKey, string(active)); err != nil {
		return err
	}
	for _, profile := range Profiles() {
		if js, ok := models[profile]; ok {
			if err := s.Store.SaveConfig(stateKey(profile), js); err != nil {
				return err
			}
		}
	}
	return nil
}

// SetHeatingCoef lets the operator declare heating-load sensitivity
// explicitly. Units: W per °C below 18°C. 0 disables. Always overwrites
// the current value across all profiles. Use SeedHeatingCoef on startup
// so a learned coefficient survives restarts.
func (s *Service) SetHeatingCoef(w float64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	for _, model := range s.models {
		model.HeatingW_per_degC = w
	}
	s.mu.Unlock()
}

// SeedHeatingCoef applies an operator-config heating-load sensitivity
// only as a cold-start prior. Per profile: if the model already has
// telemetry-driven samples (Samples > 0), its learned coefficient is
// preserved; the seed value is ignored for that profile.
//
// This is the startup-path entry point. Operator config is the prior;
// observation drives the value once learning has begun. Without this
// guard, every restart would clobber the learned coefficient with the
// (often-stale) config value — defeating the adaptive fit.
//
// Operators who want to forcibly reset the coefficient should hit the
// reset endpoint (which clears bucket samples) or call SetHeatingCoef.
func (s *Service) SeedHeatingCoef(w float64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	for _, model := range s.models {
		if model.Samples > 0 {
			continue
		}
		model.HeatingW_per_degC = w
	}
	s.mu.Unlock()
}

// Reset clears the active profile model (e.g. after a big appliance
// or occupancy-pattern change).
func (s *Service) Reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	profile := s.active
	old := s.activeModelLocked()
	peak := old.PeakW
	heating := old.HeatingW_per_degC
	s.models[profile] = newProfileModel(peak, profile)
	s.models[profile].HeatingW_per_degC = heating
	s.mu.Unlock()
	if err := s.persist(); err != nil {
		slog.Warn("loadmodel persist", "err", err)
	}
}
