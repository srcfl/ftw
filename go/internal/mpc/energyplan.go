package mpc

import (
	"context"
	"path/filepath"
	"time"
)

// EnergyplanOptimizer runs the compiled worker on Core's downside forecast.
// Source and builds belong to the private Energyplan repository.
type EnergyplanOptimizer struct {
	*ExternalOptimizer
}

func NewEnergyplanOptimizer(binary string) (*EnergyplanOptimizer, error) {
	external, err := NewExternalOptimizer(ExternalOptimizerConfig{
		Command:   []string{binary, "--time-limit=2s"},
		ModuleDir: filepath.Dir(binary), Timeout: 3 * time.Second,
		IdleTimeout: 2 * time.Minute,
	})
	if err != nil {
		return nil, err
	}
	external.timeBudget = energyplanTimeBudget
	return &EnergyplanOptimizer{ExternalOptimizer: external}, nil
}

func energyplanTimeBudget(slots []Slot, p Params) time.Duration {
	assets := max(1, len(p.Storages)) + 2*len(p.activeLoadpoints())
	if len(slots)*assets > 193*6 || p.PVCurtailment.MinW > 0 || p.PVUncertaintyW > 0 || p.PVRelativeUncertainty > 0 {
		return 2 * time.Second
	}
	return 500 * time.Millisecond
}

func (o *EnergyplanOptimizer) Optimize(ctx context.Context, slots []Slot, p Params) (Plan, error) {
	// Service has already applied the risk margin to these slots. Do not
	// construct a second scenario model for this deterministic solver.
	p.PVUncertaintyW, p.PVRelativeUncertainty, p.PVForecastSafetyK = 0, 0, 0
	return o.ExternalOptimizer.Optimize(ctx, slots, p)
}

func (o *EnergyplanOptimizer) UsesDownsidePV() bool { return true }

func (o *EnergyplanOptimizer) Health(ctx context.Context) (OptimizerRuntimeInfo, error) {
	line, err := o.transport.RoundTrip(ctx, []byte(`{"type":"handshake","protocol_version":1}`))
	if err != nil {
		return OptimizerRuntimeInfo{}, err
	}
	return decodeOptimizerHandshakeFor(line, "process", "ftw-solver")
}

// BundledWithCore prevents sidecar update controls from reporting or replacing
// the independently versioned worker bundled in the Core image.
func (o *EnergyplanOptimizer) BundledWithCore() bool { return true }

func usesDownsidePV(optimizer PlanOptimizer) bool {
	o, ok := optimizer.(interface{ UsesDownsidePV() bool })
	return ok && o.UsesDownsidePV()
}

func (s *Service) OptimizerBundledWithCore() bool {
	o, ok := s.ConfiguredOptimizer().(interface{ BundledWithCore() bool })
	return ok && o.BundledWithCore()
}
