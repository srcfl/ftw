package ocppcp

import (
	"math"
	"time"
)

const minChargeAmps = 6.0

// Physics is a first-order per-phase current model. LimitA is what the
// charging profile granted; DrawA lags toward it so a CLI demo does not
// jump. Tests set TauS to 0 so a Tick settles instantly.
type Physics struct {
	Voltage  float64
	Phases   int
	MaxAmps  float64
	TauS     float64
	LimitA   float64
	DrawA    float64
	EnergyWh float64
	Plugged  bool
}

func newPhysics(m Model, tauS float64) Physics {
	maxA := m.MaxAmps()
	return Physics{
		Voltage:  SiteVoltage,
		Phases:   m.Phases,
		MaxAmps:  maxA,
		TauS:     tauS,
		LimitA:   maxA, // unsteered charger runs at its hardware maximum
		EnergyWh: 1000,
	}
}

// PowerW is site-convention EV load: positive watts into the car.
func (p Physics) PowerW() float64 {
	return p.DrawA * p.Voltage * float64(p.Phases)
}

func (p Physics) targetA() float64 {
	if !p.Plugged || p.LimitA < minChargeAmps {
		return 0
	}
	if p.LimitA > p.MaxAmps {
		return p.MaxAmps
	}
	return p.LimitA
}

// Tick advances DrawA toward the granted limit and integrates energy.
func (p *Physics) Tick(dt time.Duration) {
	target := p.targetA()
	if p.TauS <= 0 {
		p.DrawA = target
	} else if dt > 0 {
		alpha := 1 - math.Exp(-dt.Seconds()/p.TauS)
		p.DrawA += (target - p.DrawA) * alpha
	}
	hours := dt.Hours()
	if hours > 0 {
		p.EnergyWh += p.PowerW() * hours
	}
}
