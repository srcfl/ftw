package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/localzone"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// forecastSiteConfig owns the small, immutable subset needed by asynchronous
// forecast work. It never holds cfgMu or ctrlMu while a model runs.
type forecastSiteConfig struct {
	mu                    sync.RWMutex
	value                 forecastSite
	store                 *state.Store
	engineVersion         string
	weatherRevision       string
	weatherSinceMS        int64
	identity              func(string) (string, bool)
	baseRevision          string
	baseOptions           telemetry.ForecastOptions
	required              map[string]bool // value identifies PV sources
	accepted              forecastIdentityReceipt
	bindingDirty          bool
	bindingRetryAt        time.Time // a failed binding save waits until then
	configuredAt          time.Time
	heatingPrior, ratedPV float64
	annualBase            float64
	annualIncludesHeating bool
	priorRevision         string
}

type forecastIdentityReceipt struct {
	BaseRevision         string            `json:"base_revision"`
	IDs                  map[string]string `json:"ids"`
	LearningBaseRevision string            `json:"learning_base_revision,omitempty"`
}

const forecastIdentityReceiptKey = "forecast/live_identity_v1"

// forecastBindingRetry spaces out binding saves after a failure. Each attempt
// can wait seconds on SQLite, and the identity poll runs every second.
const forecastBindingRetry = 30 * time.Second

// forecastPipelinePolicy names how Core composes the forecast it plans with.
// The evaluation cohort keys on it rather than on the Core version, so error
// bands and baselines survive updates that leave forecasting alone. Bump it
// whenever Core changes what reaches the planner.
const forecastPipelinePolicy = "energyplan-primary-v5"

func newForecastSiteConfig(st *state.Store) *forecastSiteConfig {
	id, _ := st.LoadConfig("forecast/site_id")
	if id == "" {
		id = uuid.NewString()
		if err := st.SaveConfig("forecast/site_id", id); err != nil {
			slog.Warn("forecast site identity not saved", "err", err)
		}
	}
	revision, _ := st.LoadConfig("forecast/config_revision")
	var receipt struct {
		Revision string
		SinceMS  int64
	}
	if raw, ok := st.LoadConfig("forecast/weather_generation"); ok {
		_ = json.Unmarshal([]byte(raw), &receipt)
	}
	var accepted forecastIdentityReceipt
	if raw, ok := st.LoadConfig(forecastIdentityReceiptKey); ok {
		_ = json.Unmarshal([]byte(raw), &accepted)
	}
	return &forecastSiteConfig{accepted: accepted, weatherRevision: receipt.Revision, weatherSinceMS: receipt.SinceMS, store: st, engineVersion: forecastBinaryIdentity(resolveEnergyplanBinary()), value: forecastSite{SiteID: id, Revision: revision}}
}

func (s *forecastSiteConfig) Snapshot() forecastSite {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v := s.value
	v.Options.ExpectedFlows = append([]telemetry.ForecastFlow(nil), v.Options.ExpectedFlows...)
	v.HistoryOptions.ExpectedFlows = append([]telemetry.ForecastFlow(nil), v.HistoryOptions.ExpectedFlows...)
	return v
}

func (s *forecastSiteConfig) Configure(cfg *config.Config, catalog []drivers.CatalogEntry) bool {
	v := forecastSite{Meter: cfg.SiteMeterDriver(), Timezone: localzone.Name(), Options: forecastMeasurementOptions(cfg, catalog), HistoryOptions: historyMeasurementOptions(cfg, catalog), HistorySun: historySunOf(siteLocation(cfg))}
	if cfg.Weather != nil && cfg.Weather.Provider != "" && cfg.Weather.Provider != "none" {
		v.Latitude, v.Longitude = cfg.Weather.Latitude, cfg.Weather.Longitude
		v.HasLocation = !math.IsNaN(v.Latitude) && !math.IsNaN(v.Longitude) && math.Abs(v.Latitude) <= 90 && math.Abs(v.Longitude) <= 180
		v.HasPVScale = forecastRatedPVW(cfg.Weather) > 0
	}
	// The revision contains model inputs, never provider credentials or unrelated
	// control settings. Sorting prevents driver order alone from losing learning.
	sort.Slice(v.Options.ExpectedFlows, func(i, j int) bool {
		a, b := v.Options.ExpectedFlows[i], v.Options.ExpectedFlows[j]
		if a.Driver != b.Driver {
			return a.Driver < b.Driver
		}
		return a.DerType < b.DerType
	})
	var weather *config.Weather
	if cfg.Weather != nil {
		cp := *cfg.Weather
		cp.APIKey = ""
		weather = &cp
	}
	driverInputs := append([]config.Driver(nil), cfg.Drivers...)
	sort.Slice(driverInputs, func(i, j int) bool { return driverInputs[i].Name < driverInputs[j].Name })
	learningDrivers := forecastLearningDrivers(v.Meter, v.Options.ExpectedFlows, driverInputs)
	// Nameplate and heating estimates change predictions, not the meaning of
	// a measured sample. Keep their revision out of the learning identity.
	learningWeather := forecastLearningWeather(weather)
	baseRevision, err := forecastStaticRevision(v, learningWeather, learningDrivers)
	// Earlier policies hashed the resolved script path: beta.4 the measurement
	// drivers, beta.3 every driver. Verify that exact old contract before
	// retaining its learning ID. A release that also moved the path cannot
	// match, so such a site starts over once, as every update did before.
	earlier := make([]string, 0, 2)
	if old, oldErr := forecastStaticRevision(v, weather, learningDrivers); oldErr == nil {
		earlier = append(earlier, old)
	}
	for _, inputs := range [][]config.Driver{learningDrivers, driverInputs} {
		if old, oldErr := forecastResolvedPathRevision(v, weather, inputs); oldErr == nil {
			earlier = append(earlier, old)
		}
	}
	if err != nil {
		slog.Warn("forecast configuration is not serializable", "err", err)
		v.Options.HouseholdInvalidReason = "invalid_forecast_configuration"
		v.HasLocation = false
		baseRevision = "invalid:" + uuid.NewString()
	}
	weatherData, _ := json.Marshal(weather)
	weatherRevision := fmt.Sprintf("%x", sha256.Sum256(weatherData))
	var weatherReceipt []byte
	s.mu.Lock()
	for _, old := range earlier {
		if err != nil || old == baseRevision || s.accepted.BaseRevision != old {
			continue
		}
		if s.accepted.LearningBaseRevision == "" {
			s.accepted.LearningBaseRevision = old
		}
		s.accepted.BaseRevision = baseRevision
		s.bindingDirty = true
		break
	}
	if s.weatherRevision != weatherRevision || s.weatherSinceMS <= 0 {
		s.weatherRevision = weatherRevision
		s.weatherSinceMS = time.Now().UnixMilli()
		weatherReceipt, _ = json.Marshal(struct {
			Revision string
			SinceMS  int64
		}{weatherRevision, s.weatherSinceMS})
	}
	v.WeatherSinceMS = s.weatherSinceMS
	v.SiteID = s.value.SiteID
	previous := s.value.Revision
	s.value = v
	if s.baseRevision != baseRevision {
		s.configuredAt = time.Now()
	}
	s.baseRevision = baseRevision
	s.baseOptions = v.Options
	s.required = make(map[string]bool)
	if v.Meter != "" {
		s.required[v.Meter] = false
	}
	for _, flow := range v.Options.ExpectedFlows {
		s.required[flow.Driver] = s.required[flow.Driver] || flow.DerType == telemetry.DerPV
	}
	s.heatingPrior = 0
	if cfg.Weather != nil {
		s.heatingPrior = cfg.Weather.HeatingWPerDegC
	}
	s.ratedPV = forecastRatedPVW(cfg.Weather)
	s.annualBase, _ = cfg.AnnualBaseKWh()
	s.annualIncludesHeating = s.annualBase > 0 && cfg.AnnualPriorIncludesHeating()
	priorData, _ := json.Marshal(struct {
		Weather         *config.Weather
		AnnualBase      float64
		IncludesHeating bool
	}{weather, s.annualBase, s.annualIncludesHeating})
	s.priorRevision = fmt.Sprintf("%x", sha256.Sum256(priorData))
	s.mu.Unlock()
	// Save outside s.mu: the control tick reads Snapshot and must not wait on SQLite.
	if weatherReceipt != nil {
		if err := s.store.SaveConfig("forecast/weather_generation", string(weatherReceipt)); err != nil {
			slog.Warn("forecast weather generation not saved", "err", err)
		}
	}
	s.RefreshIdentity(time.Now())
	return previous != s.Snapshot().Revision
}

// RefreshIdentity reads the running host, never the historical devices table.
// Serial/MAC identities are usable immediately. Endpoint fallback retains the
// existing device contract after a short init grace. A previously stronger
// identity must return before it can qualify again after restart.
// Caller serializes this with model rebinding under forecastConfigMu, so the
// saves below, made outside s.mu, keep their order.
func (s *forecastSiteConfig) RefreshIdentity(now time.Time) bool {
	s.mu.Lock()
	ids := make(map[string]string, len(s.required))
	pending, pvPending := false, false
	for name, isPV := range s.required {
		id, known := "", false
		if s.identity != nil {
			id, known = s.identity(name)
		}
		old := ""
		if s.accepted.BaseRevision == s.baseRevision {
			old = s.accepted.IDs[name]
		}
		ready := known && id != "" && forecastIdentityStrength(id) >= forecastIdentityStrength(old)
		if forecastIdentityStrength(id) == 1 && now.Sub(s.configuredAt) < 3*time.Second {
			ready = false
		}
		if !ready {
			pending = true
			pvPending = pvPending || isPV
		}
		if ready {
			ids[name] = id
		} else {
			ids[name] = "unconfirmed"
		}
	}
	data, _ := json.Marshal(ids)
	learningBase := s.baseRevision
	if s.accepted.BaseRevision == s.baseRevision && s.accepted.LearningBaseRevision != "" {
		learningBase = s.accepted.LearningBaseRevision
	}
	learning := fmt.Sprintf("site-v2:%x", sha256.Sum256([]byte(learningBase+"/"+string(data))))
	cohort := learning + "/" + s.engineVersion + "/" + forecastPipelinePolicy + "/" + s.priorRevision
	revision := fmt.Sprintf("forecast-v1:%x", sha256.Sum256([]byte(cohort)))
	opts := s.baseOptions
	if pending && opts.HouseholdInvalidReason == "" {
		opts.HouseholdInvalidReason = "unconfirmed_device_identity"
	}
	if pvPending {
		opts.PVInvalidReason = "unconfirmed_pv_identity"
	}
	changed := s.value.Revision != revision || s.value.IdentityPending != pending || s.value.Options.PVInvalidReason != opts.PVInvalidReason
	s.value.LearningRevision, s.value.Revision, s.value.IdentityPending, s.value.Options = learning, revision, pending, opts
	if !pending && (s.bindingDirty || s.accepted.BaseRevision != s.baseRevision || !forecastIdentitiesEqual(s.accepted.IDs, ids)) {
		s.accepted = forecastIdentityReceipt{BaseRevision: s.baseRevision, IDs: ids}
		if learningBase != s.baseRevision {
			s.accepted.LearningBaseRevision = learningBase
		}
		s.bindingDirty = true
	}
	var binding []byte
	if !pending && s.bindingDirty && !now.Before(s.bindingRetryAt) {
		binding, _ = json.Marshal(s.accepted)
	}
	s.mu.Unlock()

	// Save outside s.mu: the control tick reads Snapshot and must not wait on SQLite.
	if binding != nil {
		err := s.store.SaveConfig(forecastIdentityReceiptKey, string(binding))
		s.mu.Lock()
		if err != nil {
			s.bindingRetryAt = now.Add(forecastBindingRetry)
		} else {
			s.bindingDirty = false
		}
		s.mu.Unlock()
		if err != nil {
			slog.Warn("forecast identity binding not saved", "err", err, "retry_in", forecastBindingRetry)
		}
	}
	if changed {
		if err := s.store.SaveConfig("forecast/config_revision", revision); err != nil {
			slog.Warn("forecast revision not saved", "err", err)
		}
	}
	return changed
}

// forecastStaticRevision identifies what learned models depend on: meter,
// zone, measurement options, weather settings, the measurement drivers and
// their script content. A driver whose content was read is hashed with its
// configured path (drivers/<name>.lua), not the release directory it resolved
// to, so an update that only moves the scripts keeps the learning. Without a
// content digest the resolved path stays, so a different script still resets.
func forecastStaticRevision(v forecastSite, weather *config.Weather, inputs []config.Driver) (string, error) {
	scripts := forecastScriptDigests(inputs)
	portable := config.Config{Drivers: append([]config.Driver(nil), inputs...)}
	portable.UnresolveDriverPaths("")
	for i, d := range inputs {
		if scripts[d.Name] == forecastScriptUnavailable {
			portable.Drivers[i].Lua = d.Lua
		}
	}
	return forecastRevisionOf(v, weather, portable.Drivers, scripts)
}

// forecastResolvedPathRevision is the policy before #1488, which hashed the
// resolved path. It only recognises learning IDs accepted under that policy.
func forecastResolvedPathRevision(v forecastSite, weather *config.Weather, inputs []config.Driver) (string, error) {
	return forecastRevisionOf(v, weather, inputs, forecastScriptDigests(inputs))
}

const forecastScriptUnavailable = "unavailable"

// forecastScriptDigests reads each driver's script through its resolved path.
func forecastScriptDigests(inputs []config.Driver) map[string]string {
	scripts := make(map[string]string)
	for _, d := range inputs {
		digest, err := forecastReleaseScriptDigest(d.Lua)
		if err != nil {
			digest = forecastScriptUnavailable
		}
		scripts[d.Name] = digest
	}
	return scripts
}

// forecastRevisionOf hashes the given driver entries and script digests.
// Keep this encoding compatible with beta.3 so an upgrade can prove that only
// the hash policy changed. Never infer compatibility from a driver name.
func forecastRevisionOf(v forecastSite, weather *config.Weather, hashed []config.Driver, scripts map[string]string) (string, error) {
	hashed = append([]config.Driver(nil), hashed...)
	for i := range hashed {
		// credential_owner is bookkeeping for OAuth rotation. It must not
		// reset learned PV/load models when Core mints or migrates it.
		hashed[i].CredentialOwner = ""
	}
	data, err := json.Marshal(struct {
		Meter, Timezone string
		Options         telemetry.ForecastOptions
		Weather         *config.Weather
		Drivers         []config.Driver
		Scripts         map[string]string
	}{v.Meter, v.Timezone, v.Options, weather, hashed, scripts})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("site-static-v1:%x", sha256.Sum256(data)), nil
}

// forecastLearningDrivers are the physical measurement sources whose script
// or scaling change must reset learned PV/load models. EV/V2X chargers stay
// in ExpectedFlows for household completeness, but their Lua packaging is
// not a house or PV measurement contract.
func forecastLearningDrivers(meter string, flows []telemetry.ForecastFlow, drivers []config.Driver) []config.Driver {
	keep := map[string]bool{}
	if meter != "" {
		keep[meter] = true
	}
	for _, flow := range flows {
		switch flow.DerType {
		case telemetry.DerMeter, telemetry.DerPV, telemetry.DerBattery:
			if flow.Driver != "" {
				keep[flow.Driver] = true
			}
		}
	}
	out := make([]config.Driver, 0, len(keep))
	for _, d := range drivers {
		if keep[d.Name] {
			out = append(out, d)
		}
	}
	return out
}

func forecastIdentityStrength(id string) int {
	switch {
	case id == "":
		return 0
	case strings.HasPrefix(id, "ep:"):
		return 1
	case strings.HasPrefix(id, "mac:"):
		return 2
	default:
		return 3
	}
}
func forecastIdentitiesEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for name, id := range a {
		if b[name] != id {
			return false
		}
	}
	return true
}
func (s *forecastSiteConfig) ModelPriors() (heating, rated float64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.heatingPrior, s.ratedPV
}

func (s *forecastSiteConfig) AnnualBase() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.annualBase
}

func (s *forecastSiteConfig) AnnualPrior() (float64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.annualBase, s.annualIncludesHeating
}

func forecastLearningWeather(w *config.Weather) *config.Weather {
	if w == nil {
		return nil
	}
	cp := *w
	cp.PVRatedW, cp.HeatingWPerDegC = 0, 0
	cp.PVArrays = nil
	cp.PVTiltDeg, cp.PVAzimuthDeg = 0, 0
	return &cp
}

func forecastRatedPVW(w *config.Weather) float64 {
	if w == nil {
		return 0
	}
	var total float64
	for _, a := range w.PVArrays {
		x := a.RatedWatts()
		if x > 0 && !math.IsInf(x, 0) && !math.IsNaN(x) {
			total += x
		}
	}
	if total > 0 {
		return total
	}
	if w.PVRatedW > 0 && !math.IsInf(w.PVRatedW, 0) && !math.IsNaN(w.PVRatedW) {
		return w.PVRatedW
	}
	return 0
}

func usableTrainingWeather(row state.ForecastPoint, now time.Time) bool {
	return row.FetchedAtMs > 0 && row.FetchedAtMs <= now.UnixMilli() && now.UnixMilli()-row.FetchedAtMs <= (12*time.Hour).Milliseconds()
}
