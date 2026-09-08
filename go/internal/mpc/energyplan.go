package mpc

import (
	"context"
	"fmt"
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
		Command:   []string{binary, "--time-limit=5s"},
		ModuleDir: filepath.Dir(binary), Timeout: 7 * time.Second,
		IdleTimeout: 2 * time.Minute,
	})
	if err != nil {
		return nil, err
	}
	external.timeBudget = energyplanTimeBudget
	return &EnergyplanOptimizer{ExternalOptimizer: external}, nil
}

const (
	energyplanSmallBudget = 500 * time.Millisecond
	energyplanFleetBudget = 5 * time.Second
	// Leave time for ValidatePlan and publication before firstSlotExpired.
	energyplanPublishMargin = 50 * time.Millisecond
)

func energyplanTimeBudget(slots []Slot, p Params) time.Duration {
	batteries := len(p.Storages)
	if batteries == 0 && p.CapacityWh > 0 {
		batteries = 1
	}
	assets := 3*batteries + 2*len(p.activeLoadpoints())
	// Core already adjusts PV to one downside horizon. That margin does not
	// add worker scenarios or change this model's size.
	budget := energyplanSmallBudget
	if len(slots)*assets >= 193*6 || p.PVCurtailment.MinW > 0 {
		budget = energyplanFleetBudget
	}
	remaining := remainingFirstSlot(slots)
	if remaining < energyplanSmallBudget {
		return 0
	}
	available := remaining - energyplanPublishMargin
	if available < budget {
		return available
	}
	return budget
}

func (o *EnergyplanOptimizer) Optimize(ctx context.Context, slots []Slot, p Params) (Plan, error) {
	// Service has already applied the risk margin to these slots. Do not
	// construct a second scenario model for this deterministic solver.
	p.PVUncertaintyW, p.PVRelativeUncertainty, p.PVForecastSafetyK = 0, 0, 0
	budget := energyplanTimeBudget(slots, p)
	if budget <= 0 {
		return Plan{}, fmt.Errorf("remaining first-slot time %s is below the Energyplan budget", remainingFirstSlot(slots))
	}
	if wait := remainingFirstSlot(slots) - energyplanPublishMargin; o.cfg.Timeout > 0 && wait > 0 && wait < o.cfg.Timeout {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, wait)
		defer cancel()
	}
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
