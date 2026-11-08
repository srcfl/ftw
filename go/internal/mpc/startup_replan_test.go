package mpc

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestStartReplansAnArchivedPlanWithFreshSoC(t *testing.T) {
	old := newCancellationTestService(t, nil)
	if _, err := old.Store.ClearPrices(); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Truncate(15 * time.Minute)
	var prices []state.PricePoint
	for i := 0; i < 16; i++ {
		prices = append(prices, state.PricePoint{
			Zone: "SE3", SlotTsMs: start.Add(time.Duration(i) * 15 * time.Minute).UnixMilli(),
			SlotLenMin: 15, SpotOreKwh: 50, TotalOreKwh: 100,
			Source: "test", FetchedAtMs: start.UnixMilli(),
		})
	}
	if err := old.Store.SavePrices(prices); err != nil {
		t.Fatal(err)
	}
	if old.Replan(context.Background()) == nil {
		t.Fatal("could not create the previous process's plan")
	}
	archived := old.Diagnose()

	tel := telemetry.NewStore()
	soc := .4
	tel.Update("battery", telemetry.DerBattery, 0, &soc, nil)
	tel.RecordDriverSuccess("battery")
	svc := New(old.Store, tel, "SE3", old.Defaults)
	svc.Horizon, svc.BaseLoad = old.Horizon, old.BaseLoad
	svc.Interval = time.Hour
	if !svc.RestoreDiagnostic(archived, time.Now(), "restored_diagnostic") {
		t.Fatal("could not restore the archived plan")
	}
	saved := make(chan *Diagnostic, 1)
	svc.SaveDiag = func(d *Diagnostic, _ string) error {
		saved <- d
		return nil
	}
	svc.Start(context.Background())
	t.Cleanup(svc.Stop)
	select {
	case d := <-saved:
		if math.Abs(d.Params.InitialSoC-soc) > 1e-9 {
			t.Fatalf("startup SoC = %v, want the fresh %v", d.Params.InitialSoC, soc)
		}
		if d.DecisionID == archived.DecisionID {
			t.Fatal("startup reused the previous process's decision")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("startup waited for the scheduled interval instead of making a fresh plan")
	}
}
