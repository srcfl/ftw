// Package sungrow contains the Sungrow SH-series physics simulation.
// Shares the first-order-lag + SoC-integration approach with the Ferroamp
// sim, but with Sungrow-specific conventions:
//   - SoC is stored as ×0.1% (0..1000) and reported in register 13022
//   - battery power (reg 13021) is UNSIGNED — direction comes from the
//     running-status register (13000) bits: bit 1=charging, bit 2=discharging
//   - grid meter at reg 5600 is signed I32 LE (+ import, − export)
//   - PV power at reg 5016 is unsigned U32 LE
//   - control: write 13049=2 (forced), 13050=0xAA/0xBB (cmd), 13051=watts
package sungrow

import (
	"math"
	"math/rand/v2"
	"sync"
	"time"
)

// Mode mirrors the writable EMS mode at reg 13049.
type Mode int

const (
	ModeStop   Mode = 0 // 13049 = 0, battery idle
	ModeForced Mode = 2 // 13049 = 2, follow 13050/13051
)

// ForceCmd mirrors the writable force-command at reg 13050.
type ForceCmd uint16

const (
	ForceCharge    ForceCmd = 0xAA
	ForceDischarge ForceCmd = 0xBB
	ForceStop      ForceCmd = 0xCC
)

// Config for the Sungrow sim.
type Config struct {
	// Battery
	CapacityWh  float64 // default 9600 (SBR 096 pack)
	SoC         float64 // 0..1, default 0.5

	// Response
	ResponseTauS  float64 // first-order lag seconds, default 3.0 (Sungrow slower than Ferroamp)
	GainPct       float64 // actual/commanded, default 0.96 (SH is efficient)

	// Hard limits (configurable via regs 33046/33047)
	MaxChargeW    float64 // default 5000
	MaxDischargeW float64 // default 5000

	// House load (for grid calc). In practice the site meter will be
	// dominated by whatever other devices are on the bus; the sim just
	// needs self-consistent numbers.
	HouseBaseW   float64 // default 600 (Sungrow typically smaller fraction of load)
	HouseJitterW float64 // default 100

	// PV
	PVPeakW float64 // override; 0 = time-of-day curve

	// Meta
	SerialNumber string // default "SH10RT-SIM-001"
	DeviceType   uint16 // default 3598 (SH10RT family code)
	RatedW       uint16 // default 10000

	// Sim clock injection (nil → time.Now)
	Clock func() time.Time
}

func Default() Config {
	return Config{
		CapacityWh:    9600,
		SoC: 0.5,
		ResponseTauS:  3.0,
		GainPct:       0.96,
		MaxChargeW:    5000,
		MaxDischargeW: 5000,
		HouseBaseW:    600,
		HouseJitterW:  100,
		PVPeakW:       0,
		SerialNumber:  "SH10RT-SIM-00001",
		DeviceType:    3598,
		RatedW:        10000,
	}
}

// Simulator is thread-safe and holds all simulated state.
type Simulator struct {
	cfg Config

	mu        sync.Mutex
	soc       float64
	mode      Mode
	forceCmd  ForceCmd
	forceW    float64 // commanded power (watts from reg 13051)
	actualBat float64 // lagged actual battery power (EMS convention: +=charge, -=discharge)

	// Configurable fuse limits via 33046/33047
	maxChargeW    float64
	maxDischargeW float64
	// SoC limits (regs 13057-13058, ×0.1%)
	socMinPct float64 // 0..100
	socMaxPct float64

	// Accumulated counters (Wh)
	importWh        float64
	exportWh        float64
	pvWh            float64
	batChargeWh     float64
	batDischargeWh  float64
	lastTick        time.Time
	startTime       time.Time
}

func New(cfg Config) *Simulator {
	if cfg.CapacityWh <= 0 { cfg.CapacityWh = 9600 }
	if cfg.ResponseTauS <= 0 { cfg.ResponseTauS = 3.0 }
	if cfg.GainPct <= 0 { cfg.GainPct = 0.96 }
	if cfg.MaxChargeW <= 0 { cfg.MaxChargeW = 5000 }
	if cfg.MaxDischargeW <= 0 { cfg.MaxDischargeW = 5000 }
	if cfg.HouseBaseW <= 0 { cfg.HouseBaseW = 600 }
	if cfg.SerialNumber == "" { cfg.SerialNumber = "SH10RT-SIM-00001" }
	if cfg.DeviceType == 0 { cfg.DeviceType = 3598 }
	if cfg.RatedW == 0 { cfg.RatedW = 10000 }
	if cfg.Clock == nil { cfg.Clock = time.Now }
	now := cfg.Clock()
	return &Simulator{
		cfg:           cfg,
		soc:           cfg.SoC,
		maxChargeW:    cfg.MaxChargeW,
		maxDischargeW: cfg.MaxDischargeW,
		socMinPct:     10,
		socMaxPct:     90,
		startTime:     now,
		lastTick:      now,
	}
}

// ---- Control interface (called when controller writes registers) ----

// SetMode writes to reg 13049. 0 = stop, 2 = forced.
func (s *Simulator) SetMode(m Mode) {
	s.mu.Lock(); defer s.mu.Unlock()
	s.mode = m
	if m != ModeForced {
		s.forceCmd = ForceStop
		s.forceW = 0
	}
}

// SetForceCmd writes to reg 13050.
func (s *Simulator) SetForceCmd(cmd ForceCmd) {
	s.mu.Lock(); defer s.mu.Unlock()
	s.forceCmd = cmd
}

// SetForceW writes to reg 13051.
func (s *Simulator) SetForceW(w float64) {
	s.mu.Lock(); defer s.mu.Unlock()
	s.forceW = math.Abs(w)
}

// SetMaxChargeW writes to reg 33046 (×0.01 kW = W × 0.01 kW / 10W = raw).
// Accepts watts directly for simplicity.
func (s *Simulator) SetMaxChargeW(w float64) {
	s.mu.Lock(); defer s.mu.Unlock()
	s.maxChargeW = math.Max(0, w)
}

// SetMaxDischargeW writes to reg 33047.
func (s *Simulator) SetMaxDischargeW(w float64) {
	s.mu.Lock(); defer s.mu.Unlock()
	s.maxDischargeW = math.Max(0, w)
}

// MaxChargeW returns the configured max charge (for register readback).
func (s *Simulator) MaxChargeW() float64 {
	s.mu.Lock(); defer s.mu.Unlock()
	return s.maxChargeW
}

// MaxDischargeW returns the configured max discharge.
func (s *Simulator) MaxDischargeW() float64 {
	s.mu.Lock(); defer s.mu.Unlock()
	return s.maxDischargeW
}

// ---- Sim advancement ----

// Tick advances the simulation by dt (or wall-clock elapsed if dt≤0).
func (s *Simulator) Tick(dt time.Duration) Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	if dt <= 0 { dt = time.Since(s.lastTick) }
	s.lastTick = s.cfg.Clock()
	dts := dt.Seconds()
	if dts <= 0 { dts = 0.001 }

	// Compute signed target based on mode + cmd
	target := 0.0
	if s.mode == ModeForced {
		switch s.forceCmd {
		case ForceCharge:
			target = math.Min(s.forceW, s.maxChargeW)
		case ForceDischarge:
			target = -math.Min(s.forceW, s.maxDischargeW)
		}
	}

	// SoC-aware derating
	if target > 0 && s.soc >= 1.0 {
		target = 0
	} else if target > 0 && s.soc >= 0.95 {
		target *= (1.0 - s.soc) / 0.05
	}
	if target < 0 && s.soc <= 0 {
		target = 0
	} else if target < 0 && s.soc <= 0.1 {
		target *= s.soc / 0.1
	}

	// First-order lag toward steady-state (target × gain)
	alpha := 1 - math.Exp(-dts/s.cfg.ResponseTauS)
	desired := target * s.cfg.GainPct
	s.actualBat += alpha * (desired - s.actualBat)

	// SoC integration
	deltaWh := s.actualBat * dts / 3600.0
	s.soc += deltaWh / s.cfg.CapacityWh
	if s.soc > 1.0 { s.soc = 1.0 }
	if s.soc < 0.0 { s.soc = 0.0 }

	if deltaWh > 0 {
		s.batChargeWh += deltaWh
	} else {
		s.batDischargeWh += -deltaWh
	}

	// PV
	pvW := s.pvNowW()
	s.pvWh += pvW * dts / 3600.0

	// House load
	loadW := s.cfg.HouseBaseW + (rand.Float64()-0.5)*2*s.cfg.HouseJitterW
	timeFrac := math.Mod(float64(time.Since(s.startTime).Seconds())/60, 1.0)
	loadW += 60 * math.Sin(2*math.Pi*timeFrac)
	if loadW < 50 { loadW = 50 }

	// Grid = load − pv + battery_charge
	gridW := loadW - pvW + s.actualBat
	if gridW > 0 {
		s.importWh += gridW * dts / 3600.0
	} else {
		s.exportWh += -gridW * dts / 3600.0
	}

	return Snapshot{
		Mode:          s.mode,
		ForceCmd:      s.forceCmd,
		ForceW:        s.forceW,
		TargetW:       target,
		ActualBatW:    s.actualBat,
		SoC:           s.soc,
		PVW:           pvW,
		LoadW:         loadW,
		GridW:         gridW,
		ImportWh:      s.importWh,
		ExportWh:      s.exportWh,
		PVWh:          s.pvWh,
		BatChargeWh:   s.batChargeWh,
		BatDischargeWh: s.batDischargeWh,
		MaxChargeW:    s.maxChargeW,
		MaxDischargeW: s.maxDischargeW,
		SocMinPct:     s.socMinPct,
		SocMaxPct:     s.socMaxPct,
	}
}

// Snapshot is the computed state after a Tick. Used by the Modbus server to
// encode register values.
type Snapshot struct {
	Mode      Mode
	ForceCmd  ForceCmd
	ForceW    float64
	TargetW   float64 // signed: +=charge, −=discharge
	ActualBatW float64
	SoC       float64

	PVW   float64
	LoadW float64
	GridW float64

	ImportWh        float64
	ExportWh        float64
	PVWh            float64
	BatChargeWh     float64
	BatDischargeWh  float64

	MaxChargeW    float64
	MaxDischargeW float64
	SocMinPct     float64
	SocMaxPct     float64
}

// Config returns the immutable config (for serial number, device type, etc.)
func (s *Simulator) Config() Config { return s.cfg }

// pvNowW returns instantaneous PV power based on simulated time of day.
func (s *Simulator) pvNowW() float64 {
	if s.cfg.PVPeakW > 0 {
		return s.cfg.PVPeakW
	}
	now := s.cfg.Clock()
	hour := float64(now.Hour()) + float64(now.Minute())/60.0
	if hour < 5 || hour > 21 { return 0 }
	x := (hour - 13) / 4.5
	peak := 4500.0 // Sungrow typically smaller PV than Ferroamp in our setup
	return peak * math.Exp(-x*x) * (0.9 + 0.1*rand.Float64())
}
