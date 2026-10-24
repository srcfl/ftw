package cloudlink

import (
	"context"
	"slices"
	"time"

	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// StatisticsSite reads the last closed hour, without names or hardware ids.
type StatisticsSite interface {
	Statistics(context.Context, time.Time) (cloudwire.Stats, bool)
}

func (w *writer) statistics(snap grantSnapshot, st *cadence, now time.Time) error {
	w.link.mu.Lock()
	ready := w.link.connected && w.link.serviceStatsPolicy == cloudwire.StatsPolicy
	w.link.mu.Unlock()
	if !ready {
		return nil
	}
	source, ok := w.link.opts.Site.(StatisticsSite)
	g := snap.g
	if !ok || g == nil || g.Revoked || g.Controller != cloudwire.ControllerSourceful ||
		g.StatsPolicy != cloudwire.StatsPolicy || len(g.StatsID) != 16 || g.StatsAtMs <= 0 ||
		!slices.Contains(g.Purposes, protocol.CloudPurposeStats) || !w.link.clockSynced() {
		return nil
	}
	// The one answer covers this upload too, and only a given yes sends an
	// hour: Cloud under the current terms or a local yes. Off, a box that
	// shares only by default, or Cloud under older terms sends none,
	// whatever the grant still carries.
	if share, _ := w.link.opts.KV.LoadConfig(statsShareKey); !saidYesToStatistics(g, share) {
		return nil
	}
	hour := now.UTC().Truncate(time.Hour).Add(-time.Hour).UnixMilli()
	if hour < g.StatsAtMs || st.statsHour == hour || now.Sub(st.statsCheckedAt) < time.Minute {
		return nil
	}
	st.statsCheckedAt = now
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	report, ok := source.Statistics(ctx, now)
	if !ok || report.HourMS != hour {
		return nil
	}
	report.StatsID, report.Policy = g.StatsID, cloudwire.StatsPolicy
	if !report.Valid(now.UnixMilli()) {
		return nil
	}
	// Recheck consent after the read: withdrawing it must not emit one more
	// hour while a database query is finishing.
	cur := w.link.current()
	if cur.g == nil || cur.g.Revoked || cur.g.Controller != cloudwire.ControllerSourceful || cur.g.StatsPolicy != g.StatsPolicy || !slices.Equal(cur.g.StatsID, g.StatsID) ||
		!slices.Contains(cur.g.Purposes, protocol.CloudPurposeStats) {
		return nil
	}
	sent, err := w.sendBody(cur, cloudwire.MsgStats, &report, false)
	if sent {
		st.statsHour = hour
	}
	return err
}
