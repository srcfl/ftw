package mpc

import (
	"github.com/srcfl/ftw/go/internal/state"
	"path/filepath"
	"testing"
	"time"
)

func shadowTestService(t *testing.T) *Service {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now().UTC().Truncate(time.Hour)
	for i := 0; i < 4; i++ {
		if err := st.SavePrices([]state.PricePoint{{
			Zone: "SE3", SlotTsMs: now.Add(time.Duration(i) * time.Hour).UnixMilli(),
			SlotLenMin: 60, SpotOreKwh: 50 + float64(i)*40, TotalOreKwh: 100 + float64(i)*80,
			Source: "test", FetchedAtMs: now.UnixMilli(),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	svc := New(st, nil, "SE3", Params{
		Mode: ModePassiveArbitrage, SoCLevels: 11, CapacityWh: 10000,
		SoCMin: 0.1, SoCMax: 0.95, InitialSoC: 0.5,
		ActionLevels: 5, MaxChargeW: 2000, MaxDischargeW: 2000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
		// Pinned so the terminal credit is an exact number the test can
		// assert by hand instead of a price-derived default.
		TerminalSoCPrice: 200,
	})
	svc.BaseLoad = 500
	return svc
}

// waitFor polls until cond holds. The shadow lands asynchronously by design,
// so tests wait for it instead of assuming an ordering.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
