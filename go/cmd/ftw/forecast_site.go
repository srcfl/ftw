package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
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
	configuredAt          time.Time
	heatingPrior, ratedPV float64
}

type forecastIdentityReceipt struct {
	BaseRevision string            `json:"base_revision"`
	IDs          map[string]string `json:"ids"`
}

const forecastIdentityReceiptKey = "forecast/live_identity_v1"
const forecastPipelinePolicy = "energyplan-primary-v1"

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
	return v
}

func (s *forecastSiteConfig) Configure(cfg *config.Config, catalog []drivers.CatalogEntry) bool {
	v := forecastSite{Meter: cfg.SiteMeterDriver(), Timezone: forecastTimezone(), Options: forecastMeasurementOptions(cfg, catalog)}
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
	scripts := make(map[string]string)
	for _, d := range driverInputs {
		if digest, err := forecastReleaseScriptDigest(d.Lua); err == nil {
			scripts[d.Name] = digest
		} else {
			scripts[d.Name] = "unavailable"
		}
	}
	// Only the digest leaves this function. Driver measurement settings and
	// stable hardware bindings must invalidate learning when they change.
	data, err := json.Marshal(struct {
		Meter, Timezone string
		Options         telemetry.ForecastOptions
		Weather         *config.Weather
		Drivers         []config.Driver
		Scripts         map[string]string
	}{v.Meter, v.Timezone, v.Options, weather, driverInputs, scripts})
	if err != nil {
		slog.Warn("forecast configuration is not serializable", "err", err)
		v.Options.HouseholdInvalidReason = "invalid_forecast_configuration"
		v.HasLocation = false
		data = []byte("invalid:" + uuid.NewString())
	}
	baseRevision := fmt.Sprintf("site-static-v1:%x", sha256.Sum256(data))
	weatherData, _ := json.Marshal(weather)
	weatherRevision := fmt.Sprintf("%x", sha256.Sum256(weatherData))
	s.mu.Lock()
	if s.weatherRevision != weatherRevision || s.weatherSinceMS <= 0 {
		s.weatherRevision = weatherRevision
		s.weatherSinceMS = time.Now().UnixMilli()
		receipt, _ := json.Marshal(struct {
			Revision string
			SinceMS  int64
		}{weatherRevision, s.weatherSinceMS})
		if err := s.store.SaveConfig("forecast/weather_generation", string(receipt)); err != nil {
			slog.Warn("forecast weather generation not saved", "err", err)
		}
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
	s.mu.Unlock()
	s.RefreshIdentity(time.Now())
	return previous != s.Snapshot().Revision
}

// RefreshIdentity reads the running host, never the historical devices table.
// Serial/MAC identities are usable immediately. Endpoint fallback retains the
// existing device contract after a short init grace. A previously stronger
// identity must return before it can qualify again after restart.
// Caller serializes this with model rebinding under forecastConfigMu.
func (s *forecastSiteConfig) RefreshIdentity(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	learning := fmt.Sprintf("site-v2:%x", sha256.Sum256([]byte(s.baseRevision+"/"+string(data))))
	cohort := learning + "/" + Version + "/" + s.engineVersion + "/" + forecastPipelinePolicy
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
	if !pending && (s.accepted.BaseRevision != s.baseRevision || !forecastIdentitiesEqual(s.accepted.IDs, ids)) {
		s.accepted = forecastIdentityReceipt{s.baseRevision, ids}
		if encoded, err := json.Marshal(s.accepted); err == nil {
			if err = s.store.SaveConfig(forecastIdentityReceiptKey, string(encoded)); err != nil {
				slog.Warn("forecast identity binding not saved", "err", err)
			}
		}
	}
	if changed {
		if err := s.store.SaveConfig("forecast/config_revision", revision); err != nil {
			slog.Warn("forecast revision not saved", "err", err)
		}
	}
	return changed
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

// Prefer the host's IANA name so persisted clock buckets retain DST rules.
// UTC is valid when that is the box's configured timezone; never guess a country.
func forecastTimezone() string {
	candidates := []string{strings.TrimPrefix(os.Getenv("TZ"), ":"), time.Local.String()}
	if target, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
		if _, zone, ok := strings.Cut(target, "/zoneinfo/"); ok {
			candidates = append(candidates, zone)
		}
	}
	if data, err := os.ReadFile("/etc/timezone"); err == nil {
		candidates = append(candidates, strings.TrimSpace(string(data)))
	}
	for _, zone := range candidates {
		if zone == "" || zone == "Local" {
			continue
		}
		if _, err := time.LoadLocation(zone); err == nil {
			return zone
		}
	}
	return "UTC"
}

func usableTrainingWeather(row state.ForecastPoint, now time.Time) bool {
	return row.FetchedAtMs > 0 && row.FetchedAtMs <= now.UnixMilli() && now.UnixMilli()-row.FetchedAtMs <= (12*time.Hour).Milliseconds()
}
