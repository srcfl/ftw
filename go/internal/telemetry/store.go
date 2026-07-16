package telemetry

import (
	"encoding/json"
	"fmt"
	"log/slog"
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
	// DerV2X is a bidirectional EV charger. Its power uses the same
	// site convention as batteries: positive means charging the vehicle,
	// negative means discharging the vehicle into the site/grid.
	DerV2X
	// DerVehicle is a read-only reading from the connected vehicle
	// itself (e.g. via TeslaBLEProxy), distinct from DerEV which is
	// the charger. Carries SoC + `charge_limit_pct`/`charging_state`/
	// `time_to_full_min`/`stale` in Data. RawW is always 0 — vehicle
	// readings don't conflict with dispatch math, they only inform
	// the loadpoint manager's SoC-source selection and the UI.
	DerVehicle
)

var allDerTypes = []DerType{DerMeter, DerPV, DerBattery, DerEV, DerV2X, DerVehicle}

// AllDerTypes returns the DER types the telemetry store knows about.
func AllDerTypes() []DerType {
	out := make([]DerType, len(allDerTypes))
	copy(out, allDerTypes)
	return out
}

// String returns the canonical string form ("meter", "pv", "battery",
// "ev", "v2x_charger", "vehicle").
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
	case DerV2X:
		return "v2x_charger"
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
	case "v2x_charger":
		return DerV2X, nil
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
	SoC       *float64 // optional; 0..1 for batteries/V2X, 0..100 for DerVehicle
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

	// WatchdogTimeoutOverride, when > 0, replaces the site-wide
	// timeout in WatchdogScan for this driver only. Drivers with
	// intrinsically slow polling cadences (Tesla BLE proxy, cloud
	// EV APIs) set this from lua via host.set_watchdog_timeout_s so
	// the loadpoint controller doesn't see them as flapping just
	// because their emit interval brushes the 60 s default. The
	// dispatcher still uses LastSuccess + this override for stale
	// detection — there is no separate "degraded" state.
	WatchdogTimeoutOverride time.Duration

	// DeviceFault is set by a driver (via host.set_device_fault) when it can
	// reach the device but the device is in a fault state where it cannot
	// actuate — e.g. a Ferroamp EnergyHub in Fault Mode with its relays open.
	// It is orthogonal to Status: the driver keeps emitting fresh telemetry
	// (so the watchdog sees it as alive and RecordSuccess keeps Status=ok),
	// but IsOnline() returns false so the dispatcher and the MPC plan exclude
	// it — otherwise we keep commanding a dead battery and the un-delivered
	// power silently becomes grid import. DeviceFaultReason is operator-facing.
	DeviceFault       bool
	DeviceFaultReason string
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

// SetDeviceFault flags (or clears) a device-level fault — the driver reaches
// the device but it can't actuate. Independent of Status so a driver that
// keeps emitting from cache doesn't flap it back on every RecordSuccess.
func (h *DriverHealth) SetDeviceFault(faulted bool, reason string) {
	h.DeviceFault = faulted
	if faulted {
		h.DeviceFaultReason = reason
	} else {
		h.DeviceFaultReason = ""
	}
}

// IsOnline reports whether the driver is usable for control. A stale-flagged
// driver (Status offline) OR one its driver flagged as device-faulted is not.
func (h *DriverHealth) IsOnline() bool {
	return h.Status != StatusOffline && !h.DeviceFault
}

// MetricSample is one (driver, metric, ts, value) tuple buffered for the
// long-format TS database. State.Store consumes these via FlushSamples.
type MetricSample struct {
	Driver string
	Metric string
	TsMs   int64
	Value  float64
	// Unit is the optional display unit from host.emit_metric ("°C", "Hz").
	// Carried to the TS DB so the metric catalog stays labelled across
	// restarts, not only while the driver is live.
	Unit string
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
	Unit      string
	Register  string
	Title     string
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

// ValidateReading enforces the site-convention invariants before telemetry
// reaches smoothing, history buffering, control, or models.
func ValidateReading(t DerType, rawW float64, soc *float64) error {
	if !finite(rawW) {
		return fmt.Errorf("%s power is non-finite: %v", t, rawW)
	}
	if soc != nil && !finite(*soc) {
		return fmt.Errorf("%s soc is non-finite: %v", t, *soc)
	}
	switch t {
	case DerPV:
		if rawW > 0 {
			return fmt.Errorf("pv power must be <= 0 in site convention, got %.3f W", rawW)
		}
	case DerEV:
		if rawW < 0 {
			return fmt.Errorf("ev power must be >= 0 in site convention, got %.3f W", rawW)
		}
	case DerBattery:
		if soc != nil && (*soc < 0 || *soc > 1) {
			return fmt.Errorf("battery soc must be a 0..1 fraction, got %.3f", *soc)
		}
	case DerV2X:
		if soc != nil && (*soc < 0 || *soc > 1) {
			return fmt.Errorf("v2x_charger vehicle soc must be a 0..1 fraction, got %.3f", *soc)
		}
	}
	return nil
}

func finite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

// Update feeds a new reading. Applies Kalman smoothing and stores both raw
// and smoothed values.
func (s *Store) Update(driver string, t DerType, rawW float64, soc *float64, data json.RawMessage) {
	if err := ValidateReading(t, rawW, soc); err != nil {
		return
	}
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
// EmitMetric records a scalar metric. unit is an optional display unit
// (e.g. "°C", "Hz", "kW") carried into the live snapshot so the UI can group
// and label metrics; pass "" when unknown. register is an optional source
// address (e.g. a Modbus register id) carried into the live snapshot for the
// per-driver detail view; pass "" when not applicable. title is an optional
// human-readable label (e.g. the device's own point title) surfaced as the
// per-signal explanation in the detail view; pass "" when not available.
func (s *Store) EmitMetric(driver, name string, value float64, unit, register, title string) {
	if !finite(value) {
		return
	}
	now := time.Now()
	s.pendingMu.Lock()
	s.pending = append(s.pending, MetricSample{
		Driver: driver, Metric: name, TsMs: now.UnixMilli(), Value: value, Unit: unit,
	})
	s.pendingMu.Unlock()
	s.latestMu.Lock()
	s.latestMetric[driver+":"+name] = metricSnap{Value: value, UpdatedAt: now, Unit: unit, Register: register, Title: title}
	s.latestMu.Unlock()
}

// MetricSnapshot is a snapshot of one (driver, metric) latest value.
type MetricSnapshot struct {
	Name      string    `json:"name"`
	Value     float64   `json:"value"`
	Unit      string    `json:"unit,omitempty"`
	Register  string    `json:"register,omitempty"`
	Title     string    `json:"title,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// LatestMetricsByDriver returns the latest live cache entries for one
// driver, sorted by metric name. Used by /api/drivers/{name} to render
// the "what's it actually emitting right now" panel without spinning
// up a TS-DB query per metric.
func (s *Store) LatestMetricsByDriver(driver string) []MetricSnapshot {
	prefix := driver + ":"
	s.latestMu.RLock()
	defer s.latestMu.RUnlock()
	out := make([]MetricSnapshot, 0)
	for k, v := range s.latestMetric {
		if len(k) <= len(prefix) || k[:len(prefix)] != prefix {
			continue
		}
		out = append(out, MetricSnapshot{
			Name:      k[len(prefix):],
			Value:     v.Value,
			Unit:      v.Unit,
			Register:  v.Register,
			Title:     v.Title,
			UpdatedAt: v.UpdatedAt,
		})
	}
	return out
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
	if len(s.pending) == 0 {
		return nil
	}
	out := s.pending
	s.pending = nil
	return out
}

// Get returns the latest reading for a driver+type, or nil if absent.
func (s *Store) Get(driver string, t DerType) *DerReading {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if r, ok := s.readings[key(driver, t)]; ok {
		return r
	}
	return nil
}

// ReadingsByType returns all readings of a given type (e.g. all batteries).
func (s *Store) ReadingsByType(t DerType) []*DerReading {
	s.mu.RLock()
	defer s.mu.RUnlock()
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

// SumOnlineV2XW returns the summed SmoothedW across online bidirectional
// V2X chargers. Positive values mean vehicle charging; negative values
// mean the vehicle is discharging into the site/grid.
func (s *Store) SumOnlineV2XW() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var sum float64
	for _, r := range s.readings {
		if r.DerType != DerV2X {
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
	s.mu.RLock()
	defer s.mu.RUnlock()
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

// DriverHealth returns a snapshot of the health record for a driver
// (or nil if unknown). Mutations must go through Store methods or
// DriverHealthMut in single-threaded test setup.
func (s *Store) DriverHealth(name string) *DriverHealth {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h := s.health[name]
	if h == nil {
		return nil
	}
	cp := *h
	return &cp
}

// DriverHealthMut returns the (mutable) health record, creating if missing.
// Holds no lock after return — callers must not share the pointer across
// goroutines. Runtime code should use the Store RecordDriver* helpers.
func (s *Store) DriverHealthMut(name string) *DriverHealth {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.driverHealthLocked(name)
}

func (s *Store) driverHealthLocked(name string) *DriverHealth {
	h, ok := s.health[name]
	if !ok {
		h = &DriverHealth{Name: name}
		s.health[name] = h
	}
	return h
}

func (s *Store) EnsureDriverHealth(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.driverHealthLocked(name)
}

func (s *Store) RecordDriverSuccess(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.driverHealthLocked(name).RecordSuccess()
}

func (s *Store) RecordDriverTick(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.driverHealthLocked(name).RecordTick()
}

func (s *Store) RecordDriverError(name, err string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.driverHealthLocked(name).RecordError(err)
}

// Remove drops all in-memory state for a driver: readings, Kalman
// filters, and the health entry. Called from the driver Registry when
// a driver is removed from config (or restarted — the next Update will
// repopulate) so the API status + UI stop rendering the stale card.
// Historical TS-DB samples are NOT touched; they stay queryable.
func (s *Store) Remove(driver string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range allDerTypes {
		k := key(driver, t)
		delete(s.readings, k)
		delete(s.filters, k)
	}
	delete(s.health, driver)
}

// AllHealth returns a snapshot of all driver health entries.
func (s *Store) AllHealth() map[string]DriverHealth {
	s.mu.RLock()
	defer s.mu.RUnlock()
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
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var out []WatchdogTransition
	for name, h := range s.health {
		eff := timeout
		if h.WatchdogTimeoutOverride > 0 {
			eff = h.WatchdogTimeoutOverride
		}
		stale := h.LastSuccess == nil || now.Sub(*h.LastSuccess) > eff
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

// SetDriverWatchdogTimeout installs a per-driver watchdog override.
// Zero clears it (revert to the site-wide default). Lazily creates a
// DriverHealth entry — drivers can call this from driver_init before
// any telemetry has flowed.
func (s *Store) SetDriverWatchdogTimeout(name string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.health[name]
	if !ok {
		h = &DriverHealth{Name: name}
		s.health[name] = h
	}
	h.WatchdogTimeoutOverride = d
}

// SetDriverDeviceFault flags (or clears) a device-level fault for the driver,
// creating the health record if needed. Wired to host.set_device_fault.
func (s *Store) SetDriverDeviceFault(name string, faulted bool, reason string) {
	s.mu.Lock()
	h, ok := s.health[name]
	if !ok {
		h = &DriverHealth{Name: name}
		s.health[name] = h
	}
	changed := h.DeviceFault != faulted
	h.SetDeviceFault(faulted, reason)
	s.mu.Unlock()
	// Log only the transition (the driver re-asserts the fault every poll) so
	// the entry/exit surfaces in /api/logs as an operator alert without spam.
	if changed {
		if faulted {
			slog.Warn("driver device fault — excluding from dispatch + plan until it recovers", "driver", name, "reason", reason)
		} else {
			slog.Info("driver device fault cleared — back in control", "driver", name)
		}
	}
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
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadFilter.Update(rawLoad)
}
