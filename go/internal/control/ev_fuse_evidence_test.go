package control

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestEVFuseZeroEvidenceKeepsSourceAndReceiptTimes(t *testing.T) {
	now := time.Now()
	sourceAt := now.Add(-time.Minute)
	for _, tc := range []struct{ name, data, reason string }{
		{"confirmed cloud zero", `"control_power_confirmed":true`, "observed_nonpositive_power"},
		{"unavailable source", `"control_power_available":false`, "invalid_or_unavailable_source"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := telemetry.NewStore()
			data := []byte(`{"control_power_observed_at":"` + sourceAt.Format(time.RFC3339Nano) + `",` + tc.data + `}`)
			store.Update("car", telemetry.DerEV, 0, nil, data)
			store.RecordDriverSuccess("car")
			store.Update("meter", telemetry.DerMeter, 12323, nil, []byte(`{"l1_a":20.45,"l2_a":17.05,"l3_a":15.87}`))
			st := NewState(0, 0, "meter")
			st.SiteFuseAmps, st.SiteFuseVoltage, st.SiteFusePhases = 25, 230, 3
			balance := coherentEVBalance(store, st, store.Get("meter", telemetry.DerMeter), []loadpoint.State{{ID: "car", DriverName: "car", PluggedIn: true}}, time.Now())
			if len(balance.evEvidence) != 1 {
				t.Fatalf("missing zero-credit evidence: %+v", balance)
			}
			e := balance.evEvidence[0]
			if e.Reason != tc.reason || e.ReportedSourceAt != sourceAt.Format(time.RFC3339Nano) || !e.ReceivedAt.After(e.SourceAt) || e.LatestCreditW != 0 {
				t.Fatalf("evidence=%+v", e)
			}
			if _, err := json.Marshal(e); err != nil {
				t.Fatal(err)
			}
			capW, _ := EVFuseBudget(store, st, []loadpoint.State{{ID: "car", DriverName: "car", PluggedIn: true}}, []loadpoint.Config{{ID: "car", DriverName: "car", PhaseMode: "3p", MinChargeW: 4140}}, 17250)
			if capW >= 4140 {
				t.Fatalf("uncertainty must not authorize charging: cap=%v", capW)
			}
		})
	}
}
