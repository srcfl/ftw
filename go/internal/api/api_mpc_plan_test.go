package api

import (
	"encoding/json"
	"math"
	"net/http/httptest"
	"testing"

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
