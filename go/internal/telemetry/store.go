package telemetry

import (
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"time"
)

// DerType classifies what kind of reading a DER produces.
type DerType int

const (
	DerMeter DerType = iota
	DerPV
	DerBattery
	DerEV
	// DerVehicle is a read-only reading from the connected vehicle
	// itself (e.g. via TeslaBLEProxy), distinct from DerEV which is
	// the charger. Carries SoC + `charge_limit_pct`/`charging_state`/
	// `time_to_full_min`/`stale` in Data. RawW is always 0 — vehicle
	// readings don't conflict with dispatch math, they only inform
	// the loadpoint manager's SoC-source selection and the UI.
	DerVehicle
)

// String returns the canonical string form ("meter", "pv", "battery",
// "ev", "vehicle").
func (d DerType) String() string {
	switch d {
	case DerMeter:
		return "meter"
	case DerPV:
		return "pv"
	case DerBattery:
		return "battery"
	case DerEV:
		return "ev"
	case DerVehicle:
		return "vehicle"
	}
	return "unknown"
}

// ParseDerType parses the string form back into a DerType.
func ParseDerType(s string) (DerType, error) {
	switch s {
	case "meter":
		return DerMeter, nil
	case "pv":
		return DerPV, nil
	case "battery":
		return DerBattery, nil
	case "ev":
		return DerEV, nil
	case "vehicle":
		return DerVehicle, nil
	}
	return 0, fmt.Errorf("unknown der type %q", s)
}

// DerReading is one DER telemetry snapshot (raw + smoothed + optional SoC).
type DerReading struct {
	Driver    string
	DerType   DerType
	RawW      float64
	SmoothedW float64
	SoC       *float64 // optional (only for batteries)
	Data      json.RawMessage
	UpdatedAt time.Time
}

// DriverStatus describes the health of one driver.
type DriverStatus int

const (
	StatusOk DriverStatus = iota
	StatusDegraded
	StatusOffline
)

func (s DriverStatus) String() string {
	switch s {
	case StatusOk:
		return "ok"
	case StatusDegraded:
		return "degraded"
	case StatusOffline:
		return "offline"
	}
	return "unknown"
}

// DriverHealth tracks per-driver health metrics.
type DriverHealth struct {
	Name              string
	Status            DriverStatus
	LastSuccess       *time.Time
	ConsecutiveErrors int
	LastError         string
	TickCount         uint64
}

// RecordSuccess resets error state and marks the driver healthy. Call
// this when the driver actually delivered fresh telemetry (i.e. on
// host.emit), not just when its poll loop returned without error.
// LastSuccess is the timestamp the watchdog uses to decide stale-
// ness, so it must only advance when real data flowed.
func (h *DriverHealth) RecordSuccess() {
	now := time.Now()
	h.LastSuccess = &now
	h.ConsecutiveErrors = 0
	h.LastError = ""
	h.Status = StatusOk
	h.TickCount++
}

// RecordTick marks one poll cycle as completed without error, but
// without claiming fresh data flowed. Bumps TickCount so the loop is
// visibly alive in /api/status, but leaves LastSuccess untouched so
// the watchdog correctly flips the driver offline when emits stop.
//
// Why split this from RecordSuccess: an MQTT-fed driver (ferroamp)
// caches the last payload per topic and emits from cache on every
// poll. If the upstream stops publishing — e.g. the EnergyHub loses
// power on a fuse blow — the cache stays populated, the lua poll
// returns nil, and without this split the registry's per-poll
// RecordSuccess would re-stamp LastSuccess forever. Issue: real-world
// outage on 2026-05-02 where ferroamp showed pv_w=-3996.7040 to four
// decimals identical for 30+ minutes after the inverter died.
func (h *DriverHealth) RecordTick() {
	h.TickCount++
}

// RecordError bumps the error counter and degrades the status after 3 in a row.
func (h *DriverHealth) RecordError(err string) {
	h.ConsecutiveErrors++
	h.LastError = err
	h.TickCount++
	if h.ConsecutiveErrors >= 3 {
		h.Status = StatusDegraded
	}
}

// SetOffline marks the driver offline (e.g. by watchdog).
func (h *DriverHealth) SetOffline() {
	h.Status = StatusOffline
}

// IsOnline reports whether the driver is usable for control.
func (h *DriverHealth) IsOnline() bool {
	return h.Status != StatusOffline
}

// MetricSample is one (driver, metric, ts, value) tuple buffered for the
// long-format TS database. State.Store consumes these via FlushSamples.
type MetricSample struct {
	Driver string
	Metric string
	TsMs   int64
	Value  float64
}

// Store is the central telemetry sink that drivers emit into and that the
// control loop reads from. Thread-safe.
type Store struct {
	mu       sync.RWMutex
	readings map[string]*DerReading // key = driver + ":" + der_type
	filters  map[string]*KalmanFilter1D
	health   map[string]*DriverHealth

	processNoise     float64
	measurementNoise float64

	// Separate slow filter for computed load (see UpdateLoad below).
	loadFilter *KalmanFilter1D

	// Per-cycle metric buffer. Drivers push via EmitMetric, the control loop
	// drains via FlushSamples once per tick. Decouples the hot path from
	// the (potentially blocking) DB writer.
	pendingMu sync.Mutex
	pending   []MetricSample

	// Live cache of latest value per (driver, metric). Lets consumers
	// (e.g. the fuse-over-limit notification rule) read a metric without
	// waiting for the control loop to flush it to the TS DB.
	latestMu     sync.RWMutex
	latestMetric map[string]metricSnap
}

type metricSnap struct {
	Value     float64
	UpdatedAt time.Time
}

// NewStore creates an empty telemetry store with default Kalman params.
func NewStore() *Store {
	return &Store{
		readings:         make(map[string]*DerReading),
		filters:          make(map[string]*KalmanFilter1D),
		health:           make(map[string]*DriverHealth),
		latestMetric:     make(map[string]metricSnap),
		processNoise:     100, // W of expected change between samples
		measurementNoise: 50,  // W of sensor noise
		// Load: slow filter (process 20 — load changes slowly, measurement 500 — noisy)
		loadFilter: NewKalman(20, 500),
	}
}

func key(driver string, t DerType) string {
	return driver + ":" + t.String()
}

// Update feeds a new reading. Applies Kalman smoothing and stores both raw
// and smoothed values.
func (s *Store) Update(driver string, t DerType, rawW float64, soc *float64, data json.RawMessage) {
	k := key(driver, t)
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.filters[k]
	if !ok {
		f = NewKalman(s.processNoise, s.measurementNoise)
		s.filters[k] = f
	}
	smoothed := f.Update(rawW)
	// Preserve last-known SoC when the new emit doesn't include one.
	// Some devices (e.g. Ferroamp ESO) publish SoC less frequently than
	// the power-flow telemetry; a missing field this tick doesn't mean
	// the battery has no SoC, just that we haven't heard a fresh number.
	if soc == nil {
		if prev, ok := s.readings[k]; ok && prev.SoC != nil {
			soc = prev.SoC
		}
	}
	now := time.Now()
	s.readings[k] = &DerReading{
		Driver:    driver,
		DerType:   t,
		RawW:      rawW,
		SmoothedW: smoothed,
		SoC:       soc,
		Data:      data,
		UpdatedAt: now,
	}

	// Auto-buffer the standard fields (raw, not smoothed — we store ground
	// truth and let consumers smooth as they like).
	tsMs := now.UnixMilli()
	s.pendingMu.Lock()
	s.pending = append(s.pending,
		MetricSample{Driver: driver, Metric: t.String() + "_w", TsMs: tsMs, Value: rawW},
	)
	if soc != nil {
		s.pending = append(s.pending,
			MetricSample{Driver: driver, Metric: t.String() + "_soc", TsMs: tsMs, Value: *soc},
		)
	}
	s.pendingMu.Unlock()
}

// EmitMetric buffers an arbitrary scalar metric for the long-format TS DB.
// Use for diagnostic data drivers want to record beyond the standard
// pv/battery/meter shape (temperatures, voltages, frequencies, etc.).
// Drained by the control loop via FlushSamples.
func (s *Store) EmitMetric(driver, name string, value float64) {
	now := time.Now()
	s.pendingMu.Lock()
	s.pending = append(s.pending, MetricSample{
		Driver: driver, Metric: name, TsMs: now.UnixMilli(), Value: value,
	})
	s.pendingMu.Unlock()
	s.latestMu.Lock()
	s.latestMetric[driver+":"+name] = metricSnap{Value: value, UpdatedAt: now}
	s.latestMu.Unlock()
}

// LatestMetric returns the most recent value for a given (driver, metric).
// ok=false when nothing has been emitted yet. Used by consumers that need
// live values (e.g. fuse-over-limit rule reading meter_l1_a) without
// waiting for the control loop's flush to the TS DB.
func (s *Store) LatestMetric(driver, name string) (float64, time.Time, bool) {
	s.latestMu.RLock()
	defer s.latestMu.RUnlock()
	if snap, ok := s.latestMetric[driver+":"+name]; ok {
		return snap.Value, snap.UpdatedAt, true
	}
	return 0, time.Time{}, false
}

// FlushSamples returns + clears all buffered metric samples. The control
// loop calls this once per cycle and forwards to the persistent store.
func (s *Store) FlushSamples() []MetricSample {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if len(s.pending) == 0 { return nil }
	out := s.pending
	s.pending = nil
	return out
}

// Get returns the latest reading for a driver+type, or nil if absent.
func (s *Store) Get(driver string, t DerType) *DerReading {
	s.mu.RLock(); defer s.mu.RUnlock()
	if r, ok := s.readings[key(driver, t)]; ok {
		return r
	}
	return nil
}

// ReadingsByType returns all readings of a given type (e.g. all batteries).
func (s *Store) ReadingsByType(t DerType) []*DerReading {
	s.mu.RLock(); defer s.mu.RUnlock()
	out := make([]*DerReading, 0)
	for _, r := range s.readings {
		if r.DerType == t {
			out = append(out, r)
		}
	}
	return out
}

// SumOnlineEVW returns the summed SmoothedW across every online EV
// driver. Used by the status endpoint, the loadmodel sampler, the MPC
// divergence check, and the control loop's grid bias — all four need
// the same "what is the EV charger drawing right now (and it's
// trustworthy)" signal, derived the same way.
//
// Offline drivers (stale telemetry, watchdog tripped) are skipped so a
// dangling 3.6 kW last-known reading can't sneak into load or grid
// accounting after the driver has actually stopped reporting.
//
// Sub-watt floor: when the Kalman residual decays toward zero (driver
// reports a real 0 W), the smoothed value asymptotes to denormals like
// 1e-77. Those leak through any `> 0` guard and corrupt downstream
// arithmetic — most acutely the BatteryCoversEV cap in control/dispatch.go,
// which on a non-zero EVChargingW flips a planned discharge target into
// a charge command and trips applyPlanSignFloor for the whole tick.
// Floor at 1 W: real EV chargers draw kW or zero; nothing in between
// matters here, and forcing exact 0 keeps every consumer's `> 0` guard
// honest.
func (s *Store) SumOnlineEVW() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var sum float64
	for _, r := range s.readings {
		if r.DerType != DerEV {
			continue
		}
		h, ok := s.health[r.Driver]
		if !ok || !h.IsOnline() {
			continue
		}
		sum += r.SmoothedW
	}
	if math.Abs(sum) < 1.0 {
		return 0
	}
	return sum
}

// ReadingsByDriver returns all readings from one driver.
func (s *Store) ReadingsByDriver(driver string) []*DerReading {
	s.mu.RLock(); defer s.mu.RUnlock()
	out := make([]*DerReading, 0)
	for _, r := range s.readings {
		if r.Driver == driver {
			out = append(out, r)
		}
	}
	return out
}

// IsStale reports whether the reading is older than timeout.
func (s *Store) IsStale(driver string, t DerType, timeout time.Duration) bool {
	r := s.Get(driver, t)
	if r == nil {
		return true
	}
	return time.Since(r.UpdatedAt) > timeout
}

// DriverHealth returns the health record for a driver (or nil if unknown).
func (s *Store) DriverHealth(name string) *DriverHealth {
	s.mu.RLock(); defer s.mu.RUnlock()
	return s.health[name]
}

// DriverHealthMut returns the (mutable) health record, creating if missing.
// Holds no lock after return — callers shouldn't share the pointer across goroutines.
func (s *Store) DriverHealthMut(name string) *DriverHealth {
	s.mu.Lock(); defer s.mu.Unlock()
	h, ok := s.health[name]
	if !ok {
		h = &DriverHealth{Name: name}
		s.health[name] = h
	}
	return h
}

// Remove drops all in-memory state for a driver: readings, Kalman
// filters, and the health entry. Called from the driver Registry when
// a driver is removed from config (or restarted — the next Update will
// repopulate) so the API status + UI stop rendering the stale card.
// Historical TS-DB samples are NOT touched; they stay queryable.
func (s *Store) Remove(driver string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range []DerType{DerMeter, DerPV, DerBattery, DerEV, DerVehicle} {
		k := key(driver, t)
		delete(s.readings, k)
		delete(s.filters, k)
	}
	delete(s.health, driver)
}

// AllHealth returns a snapshot of all driver health entries.
func (s *Store) AllHealth() map[string]DriverHealth {
	s.mu.RLock(); defer s.mu.RUnlock()
	out := make(map[string]DriverHealth, len(s.health))
	for name, h := range s.health {
		out[name] = *h
	}
	return out
}

// WatchdogScan checks each known driver's LastSuccess timestamp against
// timeout and toggles Status accordingly. Returns the list of drivers whose
// status just changed (name → new online state). Call this once per control
// cycle so the control loop can react (e.g. exclude offline drivers from
// dispatch and ask them to revert to autonomous mode).
func (s *Store) WatchdogScan(timeout time.Duration) []WatchdogTransition {
	s.mu.Lock(); defer s.mu.Unlock()
	now := time.Now()
	var out []WatchdogTransition
	for name, h := range s.health {
		stale := h.LastSuccess == nil || now.Sub(*h.LastSuccess) > timeout
		wasOnline := h.Status != StatusOffline
		if stale && wasOnline {
			h.Status = StatusOffline
			out = append(out, WatchdogTransition{Name: name, Online: false})
		} else if !stale && !wasOnline {
			h.Status = StatusOk
			h.ConsecutiveErrors = 0
			out = append(out, WatchdogTransition{Name: name, Online: true})
		}
	}
	return out
}

// WatchdogTransition describes a driver whose online state just flipped.
type WatchdogTransition struct {
	Name   string
	Online bool
}

// UpdateLoad applies the slow load filter. load = grid - pv - bat is noisy
// because battery responds faster than the grid meter sees the change. This
// filter gives a stable house-load estimate.
//
// Negative rawLoad is physically impossible for household consumption and
// indicates a transient driver-startup or PV/meter sync-lag artifact.
// Clamping to zero prevents the Kalman filter from tracking garbage.
func (s *Store) UpdateLoad(rawLoad float64) float64 {
	if rawLoad < 0 {
		rawLoad = 0
	}
	s.mu.Lock(); defer s.mu.Unlock()
	return s.loadFilter.Update(rawLoad)
}
