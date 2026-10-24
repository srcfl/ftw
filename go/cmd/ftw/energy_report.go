package main

import (
	"context"
	"log/slog"
	"sync"

	"github.com/srcfl/ftw/go/internal/cloudlink"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/energyreport"
	"github.com/srcfl/ftw/go/internal/state"
)

// The anonymous daily energy report, wired to the box. What it sends and how
// is decided in go/internal/energyreport; this file only gives it the ledger
// and the owner's answer.

// startEnergyReport builds the sender and starts its schedule. It is built
// even when nothing may be sent, so Settings can show the report this box
// would send. A build with no relay or gateway key sends nothing at all.
func startEnergyReport(ctx context.Context, cfg *config.Config, cfgMu *sync.RWMutex, st *state.Store, answer statsAnswer) *energyreport.Sender {
	if st == nil {
		return nil
	}
	sender, err := energyreport.New(energyreport.Options{
		Enabled:     energyReportAllowed(cfg, cfgMu, answer),
		Ledger:      st,
		KV:          st,
		ClockSynced: cloudlink.ClockSynced,
	})
	if err != nil {
		slog.Warn("energy report unavailable", "err", err)
		return nil
	}
	go sender.Run(ctx)
	return sender
}

// energyReportAllowed is the fleet report's answer, and no site a partner
// controls: as for the hourly project statistics, a partner's contract has to
// let such a home in first.
func energyReportAllowed(cfg *config.Config, cfgMu *sync.RWMutex, answer statsAnswer) func() bool {
	return func() bool {
		cfgMu.RLock()
		partner := cfg.Nova != nil && cfg.Nova.Enabled
		cfgMu.RUnlock()
		return !partner && answer.shares()
	}
}
