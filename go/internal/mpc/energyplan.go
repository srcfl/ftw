package mpc

import (
	"context"
	"fmt"
	"time"
)

// EnergyplanOptimizer runs the compiled worker on the forecast Core selects.
// Source and builds belong to the private Energyplan repository.
type EnergyplanOptimizer struct {
	*ExternalOptimizer
}

func NewEnergyplanOptimizer(binary string) (*EnergyplanOptimizer, error) {
	external, err := NewExternalOptimizer(ExternalOptimizerConfig{
		Command: []string{binary, "--time-limit=5s"},
		Timeout: 7 * time.Second, IdleTimeout: 2 * time.Minute,
	})
	if err != nil {
		return nil, err
	}
	external.timeBudget = energyplanTimeBudget
	o := &EnergyplanOptimizer{ExternalOptimizer: external}
	external.prepareRequest = o.prepareEnergyplanRequest
	return o, nil
}

const (
	energyplanSmallBudget = 500 * time.Millisecond
	// With a car plugged in, a Raspberry Pi 4 needs about 0.6-0.7 s before
	// the planner can improve on its first plan; at 0.5 s it stopped just
	// short and published plans about 11% above the proven optimum.
	energyplanEVBudget    = 1500 * time.Millisecond
	energyplanFleetBudget = 5 * time.Second
	// Leave time for ValidatePlan and publication before firstSlotExpired.
	energyplanPublishMargin = 50 * time.Millisecond
	// A worker that spends its whole budget must still answer before the
	// call's deadline: 5 % of the time to it, at least this.
	energyplanReplyMargin = 30 * time.Millisecond
)

func energyplanTimeBudget(slots []Slot, p Params) time.Duration {
	return energyplanBudget(slots, p, false)
}

// energyplanBudget is the solve budget; hours says the request carries
// hourly import caps.
func energyplanBudget(slots []Slot, p Params, hours bool) time.Duration {
	batteries := len(p.Storages)
	if batteries == 0 && p.CapacityWh > 0 {
		batteries = 1
	}
	assets := 3*batteries + 2*len(p.activeLoadpoints())
	// Core sends the point forecast and its caution forecast as separate
	// requests of this size. Caution adds no worker scenarios.
	budget := energyplanSmallBudget
	if len(p.activeLoadpoints()) > 0 {
		budget = energyplanEVBudget
	}
	// 1 battery + 1 EV is 5 "assets". A 193-slot villa with demand charges
	// routes to fleet MILP; 193*6 would leave it on the 500 ms budget. The
	// hourly import caps route there too.
	if len(slots)*assets >= 193*5 || p.PVCurtailment.MinW > 0 || len(p.DemandCharges) > 0 || hours {
		budget = energyplanFleetBudget
	}
	return min(budget, energyplanSolveTime(remainingFirstSlot(slots)-energyplanPublishMargin))
}

// energyplanSolveTime is the longest limit a worker can use and still answer
// before a call deadline toDeadline away. It is zero below Core's no-call
// threshold: less than energyplanSmallBudget left in the first slot.
func energyplanSolveTime(toDeadline time.Duration) time.Duration {
	if toDeadline < energyplanSmallBudget-energyplanPublishMargin {
		return 0
	}
	return toDeadline - max(toDeadline/20, energyplanReplyMargin)
}

// Optimize solves the slots Service selects, including point and caution
// attempts. The worker never adds a second risk margin.
func (o *EnergyplanOptimizer) Optimize(ctx context.Context, slots []Slot, p Params) (Plan, error) {
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

// BundledWithCore tells component diagnostics that this worker ships inside
// the Core release and moves with its updates and rollbacks.
func (o *EnergyplanOptimizer) BundledWithCore() bool { return true }

func usesDownsidePV(optimizer PlanOptimizer) bool {
	o, ok := optimizer.(interface{ UsesDownsidePV() bool })
	return ok && o.UsesDownsidePV()
}

func (s *Service) OptimizerBundledWithCore() bool {
	o, ok := s.ConfiguredOptimizer().(interface{ BundledWithCore() bool })
	return ok && o.BundledWithCore()
}
