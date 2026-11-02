package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/batteryreturn"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/state"
)

func TestBatteryReturnCreditIsSwedishAndInTheTariff(t *testing.T) {
	from := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	to := from.Add(48 * time.Hour)
	for _, c := range []struct {
		name  string
		price *config.Price
		want  float64
	}{
		{"SE3 in SEK", &config.Price{Provider: "sourceful", Zone: "SE3", GridTariffOreKwh: 61}, 26.4},
		{"an unset zone is SE3", &config.Price{Provider: "elprisetjustnu", GridTariffOreKwh: 61}, 26.4},
		{"SE1 names SEK", &config.Price{Provider: "sourceful", Zone: "SE1", Currency: "sek", GridTariffOreKwh: 61}, 26.4},
		{"no more than the tariff holds", &config.Price{Provider: "sourceful", Zone: "SE4", GridTariffOreKwh: 10}, 10},
		{"another country", &config.Price{Provider: "sourceful", Zone: "NO1", GridTariffOreKwh: 61}, 0},
		{"Swedish zone priced in euro", &config.Price{Provider: "entsoe", Zone: "SE3", Currency: "EUR", GridTariffOreKwh: 61}, 0},
		{"no prices", &config.Price{Provider: "none", Zone: "SE3", GridTariffOreKwh: 61}, 0},
		{"no price section", nil, 0},
	} {
		if got := batteryReturnCredit(c.price, from, to); got < c.want-1e-9 || got > c.want+1e-9 {
			t.Errorf("%s: %.2f öre/kWh, want %.2f", c.name, got, c.want)
		}
	}
}

// chargeLedger reads 600 Wh of battery charge in every request.
type chargeLedger struct{ batteryreturn.Ledger }

func (chargeLedger) LoadEnergyHistoryContext(context.Context, state.EnergyHistoryQuery) ([]state.EnergyLedgerPoint, bool, error) {
	return []state.EnergyLedgerPoint{
		{Flow: state.FlowBatteryCharge, EnergyWh: 600},
		{Flow: state.FlowBatteryDischarge, EnergyWh: 900},
		{Flow: state.FlowGridImport, EnergyWh: 400},
	}, false, nil
}

func TestBatteryReturnPlanning(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Date(2026, 11, 2, 12, 30, 0, 0, time.UTC)
	hour := now.Add(-5 * time.Minute).Truncate(time.Hour)
	cfg := &config.Config{
		Price:   &config.Price{Provider: "sourceful", Zone: "SE3", GridTariffOreKwh: 61},
		Drivers: []config.Driver{{BatteryCapacityWh: 10_000}, {BatteryCapacityWh: 5_000}},
	}
	// tried is this hour, so no count starts in the background.
	b := &batteryReturnPlanning{ledger: st, cfg: cfg, cfgMu: &sync.RWMutex{}, now: func() time.Time { return now }, tried: hour}
	window := func() mpc.BatteryReturnState { return b.state(now, now.Add(time.Hour)) }

	if got := window(); got != (mpc.BatteryReturnState{}) {
		t.Fatalf("two batteries: %+v", got)
	}
	cfg.Drivers = cfg.Drivers[:1]
	if got := window(); got != (mpc.BatteryReturnState{}) {
		t.Fatalf("before the first count: %+v", got)
	}

	// An empty ledger counts a battery with no grid energy.
	b.count(batteryreturn.SiteOf(cfg))
	if got := window(); got.OreKwh < 26.39 || got.GridShare != 0 || b.problem || !b.countedTo.Equal(hour) {
		t.Fatalf("after the count: %+v problem %v counted to %v", got, b.problem, b.countedTo)
	}

	// Energy that came in since the count is not taken for grid energy.
	b.ledger, b.share = chargeLedger{st}, 0.5
	if got := window(); got.GridShare != 0.5 || got.ChargedSinceWh != 600 {
		t.Fatalf("charged since the count: %+v", got)
	}

	// A count more than two hours old says nothing of the battery now.
	b.countedTo = now.Add(-3 * time.Hour)
	if got := window(); got != (mpc.BatteryReturnState{}) {
		t.Fatalf("stale count: %+v", got)
	}
}
