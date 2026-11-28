package cloudlink

import (
	"errors"
	"slices"

	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

var ErrCloudNeedsStatistics = errors.New("cloudlink: statistics are part of FTW Cloud; turn FTW Cloud off to stop them")

// ErrCloudTermsFirst is statistics asked for while Cloud runs under older
// terms, which did not include them.
var ErrCloudTermsFirst = errors.New("cloudlink: accept the current FTW Cloud terms first; they include statistics")

// includeStatistics records the exact scope the owner read. Service approval
// still gates every report. The caller holds mu.
func (l *Link) includeStatistics(g *grantRecord, fresh bool) error {
	if fresh || g.StatsPolicy != cloudwire.StatsPolicy || len(g.StatsID) != 16 {
		id, err := randomBytes(statsIDBytes)
		if err != nil {
			return err
		}
		g.StatsID, g.StatsAtMs = id, 0
		if l.clockSynced() {
			g.StatsAtMs = l.opts.Now().UnixMilli()
		}
	}
	g.StatsPolicy = cloudwire.StatsPolicy
	g.Purposes = withPurpose(g.Purposes, protocol.CloudPurposeStats, true)
	return nil
}

func (l *Link) enableLinkChoice() error {
	if err := l.opts.KV.SaveConfig(disabledKey, "false"); err != nil {
		return err
	}
	l.disabled = false
	return nil
}

// End stops all Cloud functions, purges the home and remembers the choice so
// a paired phone or a restart cannot silently turn the link back on.
func (l *Link) End(by string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.opts.KV.SaveConfig(disabledKey, "true"); err != nil {
		return err
	}
	l.disabled = true
	// The durable off flag is now saved. Stop the live session before any
	// follow-up write can fail; load() honours this flag after a restart.
	if l.link != nil {
		rec := *l.link
		rec.Ending = true
		l.link = &rec
	}
	if l.grant != nil {
		g := *l.grant
		g.Revoked, g.By = true, by
		l.grant = &g
	}
	l.rosterDue = true
	l.signal()
	if l.link != nil {
		if err := saveJSON(l.opts.KV, linkKey, *l.link); err != nil {
			return err
		}
	}
	if l.grant != nil {
		if err := saveJSON(l.opts.KV, grantKey, *l.grant); err != nil {
			return err
		}
	}
	return nil
}

// SharesStatistics is the one statistics answer, for the hourly upload and
// both anonymous daily reports, the fleet report and the energy report.
// Statistics are part of FTW Cloud and nothing else (Fredrik, 28 November
// 2026): Cloud on under the current terms shares them, with no separate
// switch, and a box without Cloud sends none. A grant counts only while the
// link honours it, which it does only with the key it was made with, and not
// once Cloud is turned off.
func (l *Link) SharesStatistics() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.disabled && cloudCoversStatistics(l.grant)
}

// cloudCoversStatistics reports whether g is FTW Cloud under the current
// terms, which include statistics.
func cloudCoversStatistics(g *grantRecord) bool {
	return g != nil && !g.Revoked && !g.statisticsOnly() && g.Policy == cloudwire.CloudTermsPolicy
}

// cloudUnderOlderTerms is Cloud on under terms older than the current ones,
// which did not include statistics: nothing goes until the owner accepts.
func cloudUnderOlderTerms(g *grantRecord) bool {
	return g != nil && !g.Revoked && !g.statisticsOnly() && g.Policy != cloudwire.CloudTermsPolicy
}

// RetireLocalStatistics handles the answer an earlier release kept for a box
// without FTW Cloud: "on", "default" or "off". Nothing reads it any more, so
// it can no longer send anything. It is written as "off", so a rollback to a
// release that still reads it does not start sending from a box without
// Cloud; under the current Cloud terms such a release sends as this one does.
func RetireLocalStatistics(kv KV) error {
	if v, _ := kv.LoadConfig(statsShareKey); v == "off" {
		return nil
	}
	return kv.SaveConfig(statsShareKey, "off")
}

// ReconcileStatistics makes a grant from an earlier release follow the rule
// that statistics come only with the current Cloud terms. A statistics-only
// link, which an earlier release started for a box without Cloud, ends: the
// service deletes its rows, as turning statistics off did. A grant under
// older terms that still carries the stats purpose loses it. Core calls this
// once at start, after RetireLocalStatistics.
func (l *Link) ReconcileStatistics(by string) error {
	l.mu.Lock()
	g, disabled := l.grant, l.disabled
	l.mu.Unlock()
	if disabled || g == nil || g.Revoked || cloudCoversStatistics(g) ||
		(len(g.StatsID) == 0 && !slices.Contains(g.Purposes, protocol.CloudPurposeStats)) {
		return nil
	}
	return l.StopStats(by)
}

// An unset clock cannot place a choice in UTC. Start its upload window only
// once the clock is known, never by filling earlier hours after NTP arrives.
func (l *Link) startStatisticsClock() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.disabled || l.grant == nil || l.grant.Revoked || l.grant.StatsPolicy != cloudwire.StatsPolicy || l.grant.StatsAtMs != 0 || !l.clockSynced() {
		return nil
	}
	g := *l.grant
	g.StatsAtMs = l.opts.Now().UnixMilli()
	return l.storeLocked(g, "statistics start clock set")
}
