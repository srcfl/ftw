package mpc

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Faults enter after the bundled worker's process boundary, so the wrapper,
// request binding, plan validation, DP fallback and publication gate all run.
type energyplanFaultTransport struct {
	OptimizerTransport
	fault    string
	requests []externalRequest
}

func (f *energyplanFaultTransport) RoundTrip(ctx context.Context, payload []byte) ([]byte, error) {
	var request externalRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		return nil, err
	}
	f.requests = append(f.requests, request)
	reply, err := f.OptimizerTransport.RoundTrip(ctx, payload)
	if err != nil {
		return nil, err
	}
	switch f.fault {
	case "timeout":
		<-ctx.Done()
		return nil, ctx.Err()
	case "invalid_plan":
		var response externalResponse
		if err := json.Unmarshal(reply, &response); err != nil {
			return nil, err
		}
		response.OK, response.Error = true, nil
		if len(response.Plan.Actions) == 0 {
			response.Plan.Actions = []externalAction{{SlotStartMs: request.Slots[0].StartMs, SlotLenMin: request.Slots[0].LenMin}}
		}
		response.Plan.Actions[0].GridW = 1e12
		return json.Marshal(response)
	default:
		return reply, nil
	}
}

func TestNativeEnergyplanEVRecoveryFallbackFaults(t *testing.T) {
	for _, fault := range []string{"timeout", "invalid_plan"} {
		for _, scenario := range []string{"feasible", "infeasible_without_previous", "infeasible_keep_previous"} {
			t.Run(fault+"/"+scenario, func(t *testing.T) {
				external := nativeWorker(t, 500*time.Millisecond)
				t.Cleanup(func() { _ = external.Close() })
				wrapper := &EnergyplanOptimizer{ExternalOptimizer: external}
				health, err := wrapper.Health(context.Background())
				if err != nil || health.Version != "0.4.2" {
					t.Fatalf("bundle health=%+v err=%v", health, err)
				}
				svc := shadowTestService(t)
				svc.Optimizer = wrapper
				svc.Defaults.Mode = ModeArbitrage
				svc.Defaults.SoCLevels, svc.Defaults.ActionLevels = 21, 21
				svc.Tele = telemetry.NewStore()
				measuredSoC := .025
				svc.Tele.Update("battery", telemetry.DerBattery, 0, &measuredSoC, nil)
				_, fixture := nativeFixture()
				ev := *fixture.Loadpoint
				svc.Loadpoint = func(int) *LoadpointSpec { copy := ev; return &copy }
				var previous *Plan
				var previousBytes []byte
				if scenario == "infeasible_keep_previous" {
					previous = svc.Replan(context.Background())
					if previous == nil || previous.Solver == nil || previous.Solver.Fallback || previous.InitialSoC != measuredSoC {
						t.Fatalf("native baseline did not publish from measured low SoC: %+v", previous)
					}
					if err := ValidatePlan(svc.lastSlots, svc.lastParams, previous); err != nil {
						t.Fatal(err)
					}
					svc.shadowWG.Wait()
					previous = svc.Latest()
					previousBytes, _ = json.Marshal(previous)
				}
				if scenario != "feasible" {
					svc.BaseLoad, svc.FuseMaxW = 20000, 1000
				}
				injected := &energyplanFaultTransport{OptimizerTransport: external.transport, fault: fault}
				external.transport = injected
				// Keep the native solve budget unchanged; only bound the lost reply.
				if fault == "timeout" {
					external.cfg.Timeout = 150 * time.Millisecond
				}
				plan := svc.Replan(context.Background())
				if len(injected.requests) != 1 {
					t.Fatalf("wrapper sent %d requests", len(injected.requests))
				}
				request := injected.requests[0]
				if len(request.Storages) != 1 || len(request.FlexLoads) != 1 {
					t.Fatalf("fault did not exercise battery+EV: storages=%d flex=%d", len(request.Storages), len(request.FlexLoads))
				}
				if math.Abs(request.Storages[0].InitialEnergyWh-measuredSoC*svc.Defaults.CapacityWh) > 1e-9 {
					t.Fatalf("worker request clamped measured initial energy: %+v", request.Storages[0])
				}
				if scenario != "feasible" {
					if previous == nil {
						if plan != nil || svc.Latest() != nil {
							t.Fatalf("infeasible fallback published without previous plan: %+v", plan)
						}
					} else {
						got, _ := json.Marshal(svc.Latest())
						returned, _ := json.Marshal(plan)
						if string(got) != string(previousBytes) || string(returned) != string(previousBytes) {
							t.Fatal("rejected fallback replaced or altered previous plan")
						}
					}
					return
				}
				if plan == nil || plan.Solver == nil || !plan.Solver.Fallback || plan.Solver.Engine != "core" || plan.Solver.Status != "fallback" {
					t.Fatalf("missing Core DP fallback: %+v", plan)
				}
				reason := "optimizer plan rejected:"
				if fault == "timeout" {
					reason = "optimizer timeout after"
				}
				if !strings.Contains(plan.Solver.FallbackReason, reason) {
					t.Fatalf("wrong fallback reason: %+v", plan.Solver)
				}
				if plan.InitialSoC != measuredSoC || svc.lastParams.InitialSoC != measuredSoC || svc.lastParams.InitialSoC >= svc.lastParams.SoCMin {
					t.Fatalf("fallback clamped recovery start: plan=%g params=%g floor=%g", plan.InitialSoC, svc.lastParams.InitialSoC, svc.lastParams.SoCMin)
				}
				if err := ValidatePlan(svc.lastSlots, svc.lastParams, plan); err != nil {
					t.Fatalf("published fallback failed physical replay: %v", err)
				}
				if svc.lastParams.Loadpoint == nil || svc.lastParams.Loadpoint.ID != ev.ID {
					t.Fatal("fallback dropped active EV")
				}
				charged := false
				for _, a := range plan.Actions {
					charged = charged || a.LoadpointW > 0
				}
				if !charged || plan.Actions[ev.TargetSlotIdx].LoadpointSoC+1e-9 < ev.TargetSoC {
					t.Fatalf("fallback did not meet feasible EV target: %+v", plan.Actions)
				}
				if svc.Latest() == nil || svc.Latest().DecisionID != plan.DecisionID {
					t.Fatal("valid fallback was not published")
				}
				svc.shadowWG.Wait()
			})
		}
	}
}
