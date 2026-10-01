package api

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/mpc"
)

func TestWriteJSONPlanKeepsNumericSoC(t *testing.T) {
	plan := &mpc.Plan{
		Mode:         mpc.ModePassiveArbitrage,
		HorizonSlots: 1,
		CapacityWh:   20000,
		InitialSoC:   0.535,
		Actions: []mpc.Action{{
			SlotStartMs: 1756323000000,
			SlotLenMin:  15,
			BatteryW:    -577,
			SoC:         0.527,
		}},
	}
	rr := httptest.NewRecorder()
	writeJSON(rr, 200, map[string]any{"enabled": true, "plan": plan})
	if rr.Code != 200 {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Plan struct {
			Actions []struct {
				SoC float64 `json:"soc"`
			} `json:"actions"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Plan.Actions) != 1 || math.Abs(got.Plan.Actions[0].SoC-0.527) > 1e-9 {
		t.Fatalf("encoded plan = %s", rr.Body.String())
	}
}

// The Plan card reads live_pv_surplus_soc_cap from /api/mpc/plan to say when
// live solar beyond the plan may charge the battery. It must be the number
// dispatch reads for the same slot.
func TestMPCPlanCarriesLivePVSurplusSoCCap(t *testing.T) {
	const key = "live_pv_surplus_soc_cap"
	now := time.Now()
	start := now.Add(-time.Minute)
	svc := &mpc.Service{}
	svc.InstallPlan(mpc.Plan{
		GeneratedAtMs: now.UnixMilli(),
		Actions: []mpc.Action{
			// Idle now; the next slot buys 2 kW from the grid at 120 öre.
			{SlotStartMs: start.UnixMilli(), SlotLenMin: 15, SpotOre: 40, SoC: 0.5},
			{SlotStartMs: start.Add(15 * time.Minute).UnixMilli(), SlotLenMin: 15,
				PriceOre: 120, BatteryW: 2000, GridW: 2500, SoC: 0.55},
		},
	}, mpc.Params{Mode: mpc.ModeArbitrage, CapacityWh: 10000, ChargeEfficiency: 1}, "")
	dir, ok := svc.SlotDirectiveAt(now)
	if !ok || math.Abs(dir.LivePVSurplusSoCCap-0.55) > 1e-9 {
		t.Fatalf("directive cap = %v (ok=%t), want 0.55", dir.LivePVSurplusSoCCap, ok)
	}

	rr := httptest.NewRecorder()
	New(&Deps{MPC: svc}).Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/mpc/plan", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Plan *struct {
			Actions []map[string]json.RawMessage `json:"actions"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("%v: %s", err, rr.Body.String())
	}
	if body.Plan == nil || len(body.Plan.Actions) != 2 {
		t.Fatalf("plan = %s", rr.Body.String())
	}
	var got float64
	if err := json.Unmarshal(body.Plan.Actions[0][key], &got); err != nil || got != dir.LivePVSurplusSoCCap {
		t.Fatalf("%s = %s, want %v", key, body.Plan.Actions[0][key], dir.LivePVSurplusSoCCap)
	}
	// No grid charge follows the last slot, so it grants nothing, and says so.
	if raw, ok := body.Plan.Actions[1][key]; !ok || string(raw) != "0" {
		t.Fatalf("last slot %s = %s (present %v), want 0", key, raw, ok)
	}
}
