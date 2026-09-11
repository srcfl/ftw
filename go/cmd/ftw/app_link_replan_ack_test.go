package main

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/state"
)

func TestAppEVEditsConfirmWhilePlanIsBlocked(t *testing.T) {
	for _, which := range []string{"soc", "solar"} {
		t.Run(which, func(t *testing.T) {
			st, err := state.Open(filepath.Join(t.TempDir(), "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			now := time.Now().UTC().Truncate(15 * time.Minute)
			if err := st.SavePrices([]state.PricePoint{{Zone: "SE4", SlotTsMs: now.UnixMilli(), SlotLenMin: 15, SpotOreKwh: 50, TotalOreKwh: 100}}); err != nil {
				t.Fatal(err)
			}
			cloud := 0.0
			if err := st.SaveForecasts([]state.ForecastPoint{{SlotTsMs: now.UnixMilli(), SlotLenMin: 15,
				CloudCoverPct: &cloud, Source: "test", FetchedAtMs: now.UnixMilli()}}); err != nil {
				t.Fatal(err)
			}
			svc := mpc.New(st, nil, "SE4", mpc.Params{Mode: mpc.ModeSelfConsumption, SoCLevels: 11, ActionLevels: 5, CapacityWh: 10000, InitialSoC: .5, SoCMin: .1, SoCMax: .95, MaxChargeW: 3000, MaxDischargeW: 3000, ChargeEfficiency: .95, DischargeEfficiency: .95})
			svc.Horizon = time.Hour
			svc.BaseLoad = 500
			entered, release := make(chan struct{}), make(chan struct{})
			var enterOnce, releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(func() {
				unblock()
				until := time.Now().Add(5 * time.Second)
				for svc.IsReplanning() {
					if time.Now().After(until) {
						t.Fatal("planner did not finish")
					}
					time.Sleep(time.Millisecond)
				}
			})
			svc.PV = func(time.Time, float64) float64 { enterOnce.Do(func() { close(entered) }); <-release; return 0 }
			mgr := loadpoint.NewManager()
			mgr.Load([]loadpoint.Config{{ID: "garage", DriverName: "easee", VehicleCapacityWh: 60000, SurplusOnly: true}})
			mgr.Observe("garage", true, 0, 0, true)
			port := &appLoadpoints{mgr: mgr, mpc: svc}
			done := make(chan bool, 1)
			go func() {
				if which == "soc" {
					done <- port.SetSoC("garage", .62)
				} else {
					_, ok := port.SetSurplusOnly("garage", false)
					done <- ok
				}
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("planner did not start")
			}
			select {
			case ok := <-done:
				if !ok {
					t.Fatal("edit refused")
				}
			case <-time.After(time.Second):
				unblock()
				<-done
				t.Fatal("saved edit waited for planner")
			}
			got, _ := mgr.State("garage")
			if which == "soc" && got.CurrentSoC != .62 {
				t.Fatalf("soc=%v", got.CurrentSoC)
			}
			if which == "solar" && got.SurplusOnly {
				t.Fatal("solar choice not applied")
			}
			if !svc.PlanSnapshot().Pending {
				t.Fatal("pending plan hidden")
			}
		})
	}
}
