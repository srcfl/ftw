package main

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/batteryreturn"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/prices"
	"github.com/srcfl/ftw/go/internal/state"
)

// batteryReturnPlanning tells the planner what the energy tax refund after
// battery storage is worth and how much grid energy the battery holds, as
// History counts it. The count reads the year's ledger, so it runs in the
// background once an hour; the planner counts no refund until one is done.
type batteryReturnPlanning struct {
	ledger batteryreturn.Ledger
	cfg    *config.Config
	cfgMu  *sync.RWMutex
	now    func() time.Time

	mu       sync.Mutex
	counting bool
	// tried is the hour the last count ran in; a failed one waits for the
	// next hour.
	tried time.Time
	// countedTo is the end of the hour the last good count ran to, and
	// share the grid part of what the battery held then. History's energies
	// use the least efficiency the year's readings allow, low early in a
	// year, so only the share carries over.
	countedTo time.Time
	problem   bool
	share     float64
}

// state is the planner's BatteryReturn hook.
func (b *batteryReturnPlanning) state(from, to time.Time) mpc.BatteryReturnState {
	b.cfgMu.RLock()
	credit := batteryReturnCredit(b.cfg.Price, from, to)
	site := batteryreturn.SiteOf(b.cfg)
	b.cfgMu.RUnlock()
	// History counts one battery of known size, and nothing otherwise.
	if credit <= 0 || site.Batteries != 1 {
		return mpc.BatteryReturnState{}
	}
	now := b.now()
	hour := now.Add(-5 * time.Minute).Truncate(time.Hour)
	b.mu.Lock()
	if !b.counting && b.tried.Before(hour) {
		b.counting, b.tried = true, hour
		go b.count(site)
	}
	countedTo, share, problem := b.countedTo, b.share, b.problem
	b.mu.Unlock()
	// No figure for this site, or no count that says what the battery
	// holds now: nothing to claim.
	if problem || countedTo.Before(now.Add(-2*time.Hour)) {
		return mpc.BatteryReturnState{}
	}
	refund := mpc.BatteryReturnState{OreKwh: credit, GridShare: share}
	if share > 0 {
		charged, err := b.chargedSince(countedTo, now)
		if err != nil {
			slog.Warn("mpc: battery charge since the refund count unknown; planning without stored grid energy", "err", err)
			refund.GridShare = 0
		}
		refund.ChargedSinceWh = charged
	}
	return refund
}

func (b *batteryReturnPlanning) count(site batteryreturn.Site) {
	now := b.now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	grid, other, at, ok, err := batteryreturn.Held(ctx, b.ledger, now, site)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.counting = false
	if err != nil {
		slog.Warn("mpc: battery return count failed; planning without the refund until the next hour", "err", err)
		return
	}
	b.countedTo, b.problem, b.share = at, !ok, 0
	if ok && grid+other > 0 {
		b.share = grid / (grid + other)
	}
	slog.Debug("mpc: battery return count", "grid_wh", grid, "other_wh", other, "hour_end", at, "ok", ok)
}

// chargedSince is the AC energy the battery took in over [from, to), from
// the ledger's five-minute buckets.
func (b *batteryReturnPlanning) chargedSince(from, to time.Time) (float64, error) {
	if !to.After(from) {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	points, _, err := b.ledger.LoadEnergyHistoryContext(ctx, state.EnergyHistoryQuery{
		SinceMS: from.UnixMilli(), UntilMS: to.UnixMilli(),
		BucketMS: max(to.Sub(from).Milliseconds(), state.EnergyLedgerBucketMS), Limit: 2,
	})
	var wh float64
	for _, p := range points {
		if p.Flow == state.FlowBatteryCharge && p.EnergyWh > 0 {
			wh += p.EnergyWh
		}
	}
	return wh, err
}

// batteryReturnCredit is what a kWh of returned grid energy is worth to a
// plan over [from, to): the refund for a Swedish household priced in SEK,
// never more than the grid tariff the import price carries the tax in.
func batteryReturnCredit(price *config.Price, from, to time.Time) float64 {
	if price == nil || price.Provider == "" || price.Provider == "none" {
		return 0
	}
	zone := price.Zone
	if zone == "" {
		zone = "SE3"
	}
	currency := strings.ToUpper(strings.TrimSpace(price.Currency))
	if currency == "" {
		currency = prices.ZoneCurrency(zone)
	}
	switch zone {
	case "SE1", "SE2", "SE3", "SE4":
	default:
		return 0
	}
	if currency != "SEK" {
		return 0
	}
	return min(batteryreturn.PlanningOreKwh(from, to), max(price.GridTariffOreKwh, 0))
}
