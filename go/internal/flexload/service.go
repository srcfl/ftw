package flexload

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/state"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
	"github.com/frahlg/forty-two-watts/go/internal/thermalmodel"
)

// Device is one configured flexible load (normalized from config.FlexLoad).
type Device struct {
	Type       string // "thermostat" | "deferrable"
	DriverName string

	// thermostat
	HeatingKind     string  // "electric" (default) | "hydronic"
	COP             float64 // electrical→thermal multiplier (1 electric, ~3 heat pump)
	MinC            float64
	MaxC            float64
	MaxHeatW        float64 // zone thermal output cap (W)
	IndoorMetric    string  // driver metric carrying measured indoor temp (°C)
	HeatMetric      string  // optional: metered heating power (W) for RC training
	SetpointAction  string  // driver command action that writes the setpoint
	PreHeatFraction float64

	// deferrable
	EnergyWh     float64
	PowerW       float64
	OnAction     string
	OffAction    string
	PreferPV     bool
	EarliestHour int
	DeadlineHour int
}

// DispatchFunc sends a JSON command to a driver (typically registry.Send).
type DispatchFunc func(ctx context.Context, driver string, payload []byte) error

// Service runs the flex-load scheduler against live forecasts and dispatches
// per-slot directives to Matter drivers. It also trains a per-zone thermal
// RC model from telemetry when a metered heating signal is available.
//
// It is deliberately decoupled from the MPC: the caller supplies a Slots
// function (built from the MPC plan's price/PV curve) and an Outdoor
// temperature function, so flexload neither imports mpc nor duplicates the
// forecast plumbing.
type Service struct {
	Store    *state.Store
	Tele     *telemetry.Store
	Devices  []Device
	Slots    func() []PriceSlot              // current horizon price/PV slots
	Outdoor  func(slotStartMs int64) float64 // outdoor temp forecast (°C)
	Dispatch DispatchFunc

	SampleInterval time.Duration // thermal training cadence (default 60s)
	ReplanInterval time.Duration // schedule + dispatch cadence (default 5m)

	mu         sync.RWMutex
	thermal    map[string]*thermalmodel.Model // by driver name
	lastIndoor map[string]tempSample          // last indoor reading for delta training

	stop chan struct{}
	done chan struct{}
}

type tempSample struct {
	c    float64
	tsMs int64
}

const thermalStateKeyPrefix = "flexload/thermal/"

// NewService builds a flex-load service. Returns nil if there are no
// devices configured, so the caller can skip starting it entirely.
func NewService(st *state.Store, tel *telemetry.Store, devices []Device) *Service {
	if len(devices) == 0 {
		return nil
	}
	s := &Service{
		Store:          st,
		Tele:           tel,
		Devices:        devices,
		SampleInterval: 60 * time.Second,
		ReplanInterval: 5 * time.Minute,
		thermal:        map[string]*thermalmodel.Model{},
		lastIndoor:     map[string]tempSample{},
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
	}
	// Restore or initialize a thermal model per thermostat.
	for _, d := range devices {
		if d.Type != "thermostat" {
			continue
		}
		m := thermalmodel.NewModel()
		if st != nil {
			if js, ok := st.LoadConfig(thermalStateKeyPrefix + d.DriverName); ok && js != "" {
				var loaded thermalmodel.Model
				if err := json.Unmarshal([]byte(js), &loaded); err == nil && loaded.Forgetting > 0 {
					m = &loaded
					slog.Info("flexload thermal model restored",
						"driver", d.DriverName, "samples", m.Samples,
						"tau_h", m.TauSeconds()/3600, "quality", m.Quality())
				}
			}
		}
		s.thermal[d.DriverName] = m
	}
	return s
}

// ThermalModel returns a copy of the learned model for a driver (for the
// API / diagnostics), and whether one exists.
func (s *Service) ThermalModel(driver string) (thermalmodel.Model, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.thermal[driver]
	if !ok {
		return thermalmodel.Model{}, false
	}
	return *m, true
}

// Start spawns the service loop. Safe to call once.
func (s *Service) Start(ctx context.Context) {
	go s.loop(ctx)
}

// Stop signals the loop and persists final model state.
func (s *Service) Stop() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	<-s.done
}

func (s *Service) loop(ctx context.Context) {
	defer close(s.done)
	sampleT := time.NewTicker(s.SampleInterval)
	replanT := time.NewTicker(s.ReplanInterval)
	defer sampleT.Stop()
	defer replanT.Stop()

	// Prime an initial dispatch so devices get a directive at startup
	// rather than waiting a full ReplanInterval.
	s.replan(ctx)
	for {
		select {
		case <-s.stop:
			s.persistAll()
			return
		case <-ctx.Done():
			s.persistAll()
			return
		case <-sampleT.C:
			s.sample()
		case <-replanT.C:
			s.replan(ctx)
		}
	}
}

// sample trains each thermostat's RC model from telemetry. Training only
// runs when a metered heating signal (HeatMetric) is configured — without a
// real power measurement we can't attribute warming to heat vs. heat-loss
// without corrupting the model, so we leave it on its physics prior (the
// scheduler still pre-heats in cheap hours and honors the comfort floor).
func (s *Service) sample() {
	now := time.Now()
	for _, d := range s.Devices {
		if d.Type != "thermostat" || d.IndoorMetric == "" || d.HeatMetric == "" {
			continue
		}
		indoorC, _, ok := s.Tele.LatestMetric(d.DriverName, d.IndoorMetric)
		if !ok {
			continue
		}
		heatW, _, ok := s.Tele.LatestMetric(d.DriverName, d.HeatMetric)
		if !ok {
			heatW = 0
		}
		// HeatMetric is metered ELECTRICAL power; the RC model needs the
		// THERMAL power delivered to the zone. For direct electric COP=1;
		// for a hydronic zone metered at the heat pump, thermal = elec×COP.
		if d.COP > 0 {
			heatW *= d.COP
		}
		outdoor := 0.0
		if s.Outdoor != nil {
			outdoor = s.Outdoor(now.UnixMilli())
		}

		s.mu.Lock()
		prev, hasPrev := s.lastIndoor[d.DriverName]
		s.lastIndoor[d.DriverName] = tempSample{c: indoorC, tsMs: now.UnixMilli()}
		m := s.thermal[d.DriverName]
		if hasPrev && m != nil {
			dt := float64(now.UnixMilli()-prev.tsMs) / 1000.0
			// Guard against long gaps (driver outage) producing a bogus delta.
			if dt > 0 && dt <= 4*float64(s.SampleInterval)/float64(time.Second) {
				if m.Update(prev.c, indoorC, outdoor, heatW, dt, now.UnixMilli()) {
					if m.Samples%10 == 0 {
						s.persist(d.DriverName, m)
					}
				}
			}
		}
		s.mu.Unlock()
	}
}

// replan rebuilds schedules from the current forecast and dispatches the
// directive for the slot covering "now" to each device.
func (s *Service) replan(ctx context.Context) {
	if s.Slots == nil || s.Dispatch == nil {
		return
	}
	slots := s.Slots()
	if len(slots) == 0 {
		return
	}
	now := time.Now()
	nowMs := now.UnixMilli()

	for _, d := range s.Devices {
		switch d.Type {
		case "thermostat":
			s.replanThermostat(ctx, d, slots, nowMs)
		case "deferrable":
			s.replanDeferrable(ctx, d, slots, now, nowMs)
		}
	}
}

func (s *Service) replanThermostat(ctx context.Context, d Device, slots []PriceSlot, nowMs int64) {
	indoorC, _, ok := s.Tele.LatestMetric(d.DriverName, d.IndoorMetric)
	if !ok {
		// No live temperature → assume mid-band so the scheduler still
		// produces a sane setpoint rather than skipping the zone.
		indoorC = (d.MinC + d.MaxC) / 2
	}
	s.mu.RLock()
	model := *s.thermal[d.DriverName]
	s.mu.RUnlock()

	spec := ThermalSpec{
		DriverName:      d.DriverName,
		Model:           model,
		CurrentC:        indoorC,
		MinC:            d.MinC,
		MaxC:            d.MaxC,
		MaxHeatW:        d.MaxHeatW,
		COP:             d.COP,
		Outdoor:         s.Outdoor,
		PreHeatFraction: d.PreHeatFraction,
	}
	if spec.Outdoor == nil {
		spec.Outdoor = func(int64) float64 { return 0 }
	}
	sched := PlanThermal(slots, spec)
	sp, ok := currentThermalSetpoint(sched, nowMs)
	if !ok {
		return
	}
	action := d.SetpointAction
	if action == "" {
		action = "setpoint"
	}
	payload, _ := json.Marshal(map[string]any{"action": action, "value": sp.TargetC})
	if err := s.Dispatch(ctx, d.DriverName, payload); err != nil {
		slog.Warn("flexload thermostat dispatch failed", "driver", d.DriverName, "err", err)
	}
}

func (s *Service) replanDeferrable(ctx context.Context, d Device, slots []PriceSlot, now time.Time, nowMs int64) {
	earliest, deadline := dailyWindow(now, d.EarliestHour, d.DeadlineHour)
	spec := DeferrableSpec{
		DriverName: d.DriverName,
		EnergyWh:   d.EnergyWh,
		PowerW:     d.PowerW,
		EarliestMs: earliest,
		DeadlineMs: deadline,
		PreferPV:   d.PreferPV,
	}
	sched := PlanDeferrable(slots, spec)
	on, ok := currentDeferrableState(sched, nowMs)
	if !ok {
		return
	}
	action := d.OffAction
	if on {
		action = d.OnAction
	}
	if action == "" {
		return // nothing to send (e.g. no off_action configured)
	}
	payload, _ := json.Marshal(map[string]any{"action": action})
	if err := s.Dispatch(ctx, d.DriverName, payload); err != nil {
		slog.Warn("flexload deferrable dispatch failed", "driver", d.DriverName, "err", err)
	}
}

// currentThermalSetpoint returns the setpoint for the slot covering nowMs.
func currentThermalSetpoint(sched ThermalSchedule, nowMs int64) (ThermalSetpoint, bool) {
	for i, sp := range sched.Setpoints {
		var end int64
		if i+1 < len(sched.Setpoints) {
			end = sched.Setpoints[i+1].StartMs
		} else {
			end = sp.StartMs + 3600_000 // assume ≤1h tail
		}
		if nowMs >= sp.StartMs && nowMs < end {
			return sp, true
		}
	}
	// Before the first slot starts, use the first.
	if len(sched.Setpoints) > 0 && nowMs < sched.Setpoints[0].StartMs {
		return sched.Setpoints[0], true
	}
	return ThermalSetpoint{}, false
}

func currentDeferrableState(sched DeferrableSchedule, nowMs int64) (on bool, ok bool) {
	for i, sl := range sched.Slots {
		var end int64
		if i+1 < len(sched.Slots) {
			end = sched.Slots[i+1].StartMs
		} else {
			end = sl.StartMs + 3600_000
		}
		if nowMs >= sl.StartMs && nowMs < end {
			return sl.On, true
		}
	}
	if len(sched.Slots) > 0 && nowMs < sched.Slots[0].StartMs {
		return sched.Slots[0].On, true
	}
	return false, false
}

// dailyWindow resolves earliest/deadline hour-of-day into absolute ms
// around now. Returns (0,0) when both hours are 0 (no window). If the
// deadline hour has already passed today, it rolls to tomorrow.
func dailyWindow(now time.Time, earliestHour, deadlineHour int) (earliestMs, deadlineMs int64) {
	if earliestHour == 0 && deadlineHour == 0 {
		return 0, 0
	}
	loc := now.Location()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	if earliestHour > 0 {
		earliestMs = startOfDay.Add(time.Duration(earliestHour) * time.Hour).UnixMilli()
	}
	if deadlineHour > 0 {
		dl := startOfDay.Add(time.Duration(deadlineHour) * time.Hour)
		if !dl.After(now) {
			dl = dl.Add(24 * time.Hour) // already past today → tomorrow
		}
		deadlineMs = dl.UnixMilli()
	}
	return earliestMs, deadlineMs
}

func (s *Service) persist(driver string, m *thermalmodel.Model) {
	if s.Store == nil {
		return
	}
	js, err := json.Marshal(m)
	if err != nil {
		return
	}
	if err := s.Store.SaveConfig(thermalStateKeyPrefix+driver, string(js)); err != nil {
		slog.Warn("flexload thermal persist failed", "driver", driver, "err", err)
	}
}

func (s *Service) persistAll() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for driver, m := range s.thermal {
		s.persist(driver, m)
	}
}

// DevicesFromConfig is a small adapter so main.go doesn't import this
// package's internals just to translate config. It is implemented in
// main.go's wiring; this comment documents the expected mapping:
//
//	config.FlexLoad{Type, DriverName, MinC, MaxC, ...} → flexload.Device{...}
var _ = fmt.Sprintf
