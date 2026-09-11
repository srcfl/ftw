// Package ferroamp contains the physical-ish simulation of a Ferroamp
// EnergyHub + ESO battery + SSO solar strings. Inputs: commanded battery power
// + simulated time of day. Outputs: realistic per-phase grid power, PV
// generation, battery SoC — the telemetry a real EnergyHub would publish.
//
// The model is intentionally simple but captures the qualities a controller
// cares about: first-order response to battery commands, SoC limits that
// derate output near 0/100%, a time-of-day PV curve, a varying house load.
package ferroamp

import (
	"math"
	"math/rand/v2"
	"sync"
	"time"
)

// Mode of the Ferroamp inverter
type Mode int

const (
	ModeAuto Mode = iota
	ModeCharge
	ModeDischarge
)

// Config is the initial state / tunables for the sim.
type Config struct {
	// Battery pack parameters
	CapacityWh float64 // usable battery capacity in Wh (default 15200)
	SoC        float64 // starting state of charge, 0..1 (default 0.5)

	// Response dynamics
	ResponseTauS float64 // first-order lag seconds (default 2.5)
	GainPct      float64 // efficiency of commanded → actual (default 0.90)

	// Fuse limits
	MaxChargeW    float64 // max charge power (default 5000)
	MaxDischargeW float64 // max discharge power (default 5000)

	// House
	HouseBaseW    float64 // steady-state house load W (default 800)
	HouseJitterW  float64 // amplitude of random load noise (default 150)

	// PV
	PVPeakW float64 // midday peak PV power (default 6000)

	// Sim-time driver (nil → wall clock; set to speed up or freeze)
	Clock func() time.Time
}

// Default returns sensible defaults matching a typical mid-size Ferroamp install.
func Default() Config {
	return Config{
		CapacityWh:   15200,
		SoC: 0.5,
		ResponseTauS: 2.5,
		GainPct:      0.90,
		MaxChargeW:   5000,
		MaxDischargeW: 5000,
		HouseBaseW:   800,
		HouseJitterW: 150,
		PVPeakW:      0, // 0 → use time-of-day curve
	}
}

// Simulator is the stateful physics model. All methods are safe for concurrent use.
type Simulator struct {
	cfg Config

	mu     sync.Mutex
	soc    float64 // 0..1
	mode   Mode
	target float64 // commanded battery power (positive = charge, per EMS convention)
	actual float64 // first-order lagged actual battery power (same sign convention)
	// Energy counters (accumulated since sim start)
	importWh     float64
	exportWh     float64
	pvWh         float64
	batChargeWh  float64
	batDischargeWh float64
	lastTick time.Time
	startTime time.Time
}

// New returns a simulator with the given config.
func New(cfg Config) *Simulator {
	if cfg.CapacityWh <= 0 { cfg.CapacityWh = 15200 }
	if cfg.ResponseTauS <= 0 { cfg.ResponseTauS = 2.5 }
	if cfg.GainPct <= 0 { cfg.GainPct = 0.90 }
	if cfg.MaxChargeW <= 0 { cfg.MaxChargeW = 5000 }
	if cfg.MaxDischargeW <= 0 { cfg.MaxDischargeW = 5000 }
	if cfg.HouseBaseW <= 0 { cfg.HouseBaseW = 800 }
	if cfg.HouseJitterW < 0 { cfg.HouseJitterW = 150 }
	if cfg.Clock == nil { cfg.Clock = time.Now }
	now := cfg.Clock()
	return &Simulator{
		cfg: cfg,
		soc: cfg.SoC,
		lastTick: now,
		startTime: now,
	}
}

// SetMode switches the inverter mode. In Charge/Discharge the target is set
// to the given watts (always positive arg, direction comes from Mode).
func (s *Simulator) SetMode(mode Mode, argW float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = mode
	switch mode {
	case ModeCharge:
		s.target = math.Min(math.Abs(argW), s.cfg.MaxChargeW)
	case ModeDischarge:
		s.target = -math.Min(math.Abs(argW), s.cfg.MaxDischargeW)
	default: // auto → target relaxes back toward zero; physics continues
		s.target = 0
	}
}

// Mode returns the current mode.
func (s *Simulator) Mode() Mode {
	s.mu.Lock(); defer s.mu.Unlock()
	return s.mode
}

// Tick advances the simulation by dt. Idempotent if called with dt=0.
// Returns the computed snapshot for this tick.
func (s *Simulator) Tick(dt time.Duration) Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	if dt <= 0 { dt = time.Since(s.lastTick) }
	s.lastTick = s.cfg.Clock()
	dts := dt.Seconds()
	if dts <= 0 { dts = 0.001 }

	// ---- Battery: first-order lag of actual toward target, SoC-constrained ----
	// Available charge/discharge power based on SoC (simple linear derating)
	maxCharge := s.socMaxCharge()
	maxDischarge := s.socMaxDischarge()
	effectiveTarget := s.target
	if effectiveTarget > maxCharge { effectiveTarget = maxCharge }
	if effectiveTarget < -maxDischarge { effectiveTarget = -maxDischarge }

	alpha := 1 - math.Exp(-dts/s.cfg.ResponseTauS)
	// Effective-actual converges toward target with gain
	desiredActual := effectiveTarget * s.cfg.GainPct
	s.actual += alpha * (desiredActual - s.actual)

	// SoC integrate: charging raises SoC, discharging lowers it
	// Wh added to battery = actual * dt_s / 3600 (converting W to Wh over dt)
	deltaBatWh := s.actual * dts / 3600.0
	// Battery efficiency loss on the Wh that actually lands in cells
	s.soc += deltaBatWh / s.cfg.CapacityWh
	if s.soc > 1.0 { s.soc = 1.0 }
	if s.soc < 0.0 { s.soc = 0.0 }

	// Energy counters for reported telemetry
	if deltaBatWh > 0 {
		s.batChargeWh += deltaBatWh
	} else {
		s.batDischargeWh += -deltaBatWh
	}

	// ---- PV generation: time-of-day bell curve + noise ----
	pvW := s.pvNowW()
	s.pvWh += pvW * dts / 3600.0

	// ---- House load: baseline + noise ----
	loadW := s.cfg.HouseBaseW + (rand.Float64()-0.5)*2*s.cfg.HouseJitterW
	// Realistic slow drift
	timeFrac := math.Mod(float64(time.Since(s.startTime).Seconds())/60, 1.0)
	loadW += 80 * math.Sin(2*math.Pi*timeFrac)
	if loadW < 50 { loadW = 50 }

	// ---- Grid = load - pv - battery_out ----
	// battery_out: positive actual = charging (draws from grid/pv)
	// positive grid = import, negative = export
	gridW := loadW - pvW + s.actual // if actual>0 we charge, extra grid import
	if gridW > 0 {
		s.importWh += gridW * dts / 3600.0
	} else {
		s.exportWh += -gridW * dts / 3600.0
	}

	return Snapshot{
		Mode:          s.mode,
		TargetW:       s.target,
		ActualBatW:    s.actual,
		SoC:           s.soc,
		PVW:           pvW,
		LoadW:         loadW,
		GridW:         gridW,
		ImportWh:      s.importWh,
		ExportWh:      s.exportWh,
		PVWh:          s.pvWh,
		BatChargeWh:   s.batChargeWh,
		BatDischargeWh: s.batDischargeWh,
	}
}

// Snapshot is one tick's output — used by the MQTT publisher.
type Snapshot struct {
	Mode          Mode
	TargetW       float64 // commanded battery power
	ActualBatW    float64 // actual battery power (positive=charging, negative=discharging)
	SoC           float64
	PVW           float64 // positive magnitude
	LoadW         float64 // positive
	GridW         float64 // positive=import, negative=export
	ImportWh      float64
	ExportWh      float64
	PVWh          float64
	BatChargeWh   float64
	BatDischargeWh float64
}

// socMaxCharge returns max charging power currently allowed.
// Soft derating above 95% SoC, hard cut at 100%.
func (s *Simulator) socMaxCharge() float64 {
	if s.soc >= 1.0 { return 0 }
	if s.soc >= 0.95 {
		ratio := (1.0 - s.soc) / 0.05 // 1.0 at 0.95, 0 at 1.0
		return s.cfg.MaxChargeW * ratio
	}
	return s.cfg.MaxChargeW
}

// socMaxDischarge returns max discharging power currently allowed.
// Soft derating below 10% SoC, hard cut at 0%.
func (s *Simulator) socMaxDischarge() float64 {
	if s.soc <= 0 { return 0 }
	if s.soc <= 0.10 {
		ratio := s.soc / 0.10
		return s.cfg.MaxDischargeW * ratio
	}
	return s.cfg.MaxDischargeW
}

// pvNowW returns instantaneous PV power based on the simulated clock's time-of-day.
// If PVPeakW is set explicitly, use that constant. Otherwise produce a bell curve
// centered at noon with ~12h daylight.
func (s *Simulator) pvNowW() float64 {
	if s.cfg.PVPeakW > 0 {
		return s.cfg.PVPeakW
	}
	now := s.cfg.Clock()
	hour := float64(now.Hour()) + float64(now.Minute())/60.0
	// Sunrise 5:00, sunset 21:00 → 16h daylight in summer, scale for simplicity
	if hour < 5 || hour > 21 { return 0 }
	// Peak at ~13:00, bell curve
	x := (hour - 13) / 4.5
	peak := 6000.0
	return peak * math.Exp(-x*x) * (0.9 + 0.1*rand.Float64())
}
