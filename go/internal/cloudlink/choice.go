package cloudlink

import (
	"errors"
	"slices"

	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

var ErrCloudNeedsStatistics = errors.New("cloudlink: statistics are part of FTW Cloud; turn FTW Cloud off, then statistics, to stop contributing")

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
	if err := l.opts.KV.SaveConfig(statsDecisionKey, ""); err != nil {
		return err
	}
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

// SharesStatistics is the one statistics answer, for both anonymous daily
// reports: the fleet report and the energy report (Fredrik, 24 October 2026).
// With FTW Cloud they are part of Cloud, with no separate off switch. A Cloud
// grant under older terms sends nothing until the owner accepts the current
// ones. Without Cloud they are on unless the owner turned them off.
//
// This form is for a box whose Cloud link is not running, so no stored grant
// is honoured: the local answer alone. Link.SharesStatistics counts the grant
// the running link honours, which it does only with the key it was made with.
func SharesStatistics(kv KV) bool {
	if kv == nil {
		return false
	}
	share, _ := kv.LoadConfig(statsShareKey)
	return sharesStatistics(nil, share)
}

// SaidYesToStatistics reports an active yes, for a box whose Cloud link is
// not running: a local yes. A box that shares only by default has not given
// one, so its reports reach FTW Cloud only through the relay, which hides its
// address; nothing goes straight to FTW Cloud from it.
func SaidYesToStatistics(kv KV) bool {
	if kv == nil {
		return false
	}
	share, _ := kv.LoadConfig(statsShareKey)
	return saidYesToStatistics(nil, share)
}

// SharesStatistics is the one answer with the grant this link honours.
func (l *Link) SharesStatistics() bool {
	g, share := l.statisticsAnswer()
	return sharesStatistics(g, share)
}

// SaidYesToStatistics is an active yes with the grant this link honours:
// Cloud under the current terms, or a local yes.
func (l *Link) SaidYesToStatistics() bool {
	g, share := l.statisticsAnswer()
	return saidYesToStatistics(g, share)
}

// statisticsAnswer is the grant this link honours, nil once Cloud is turned
// off, and the stored local answer.
func (l *Link) statisticsAnswer() (*grantRecord, string) {
	l.mu.Lock()
	g := l.grant
	if l.disabled {
		g = nil
	}
	l.mu.Unlock()
	share, _ := l.opts.KV.LoadConfig(statsShareKey)
	return g, share
}

func sharesStatistics(g *grantRecord, share string) bool {
	if g != nil && !g.Revoked && !g.statisticsOnly() {
		return g.Policy == cloudwire.CloudTermsPolicy
	}
	return share == "on" || share == "default"
}

func saidYesToStatistics(g *grantRecord, share string) bool {
	if g != nil && !g.Revoked && !g.statisticsOnly() {
		return g.Policy == cloudwire.CloudTermsPolicy
	}
	return share == "on"
}

// cloudUnderOlderTerms is Cloud on under terms older than the current ones,
// which did not include statistics: nothing goes until the owner accepts.
func cloudUnderOlderTerms(g *grantRecord) bool {
	return g != nil && !g.Revoked && !g.statisticsOnly() && g.Policy != cloudwire.CloudTermsPolicy
}

// ReconcileStatistics makes the grant follow the one answer once it is off:
// the stats purpose and statsId go, and a statistics-only link ends, as
// turning statistics off does. Core calls it after MigrateStatistics, so an
// earlier fleet switch set to off also stops the hourly upload it never
// covered.
func (l *Link) ReconcileStatistics(by string) error {
	g, share := l.statisticsAnswer()
	if share != "off" || g == nil || g.Revoked || cloudRequiresStatistics(g) ||
		(len(g.StatsID) == 0 && !slices.Contains(g.Purposes, protocol.CloudPurposeStats)) {
		return nil
	}
	return l.StopStats(by)
}

// cloudRequiresStatistics reports whether the grant is Cloud under the
// current terms, where statistics have no separate off switch.
func cloudRequiresStatistics(g *grantRecord) bool {
	return g != nil && !g.Revoked && g.Policy == cloudwire.CloudTermsPolicy
}

// MigrateStatistics writes the one answer once, from what an earlier release
// stored. A local No, a fleet report switched off, or FTW Cloud turned off
// (whose screen said that stops statistics) stays off. A local yes stays yes.
// Every other box is on by default: a box without Cloud shares unless its
// owner turns it off. Until this has run, SharesStatistics reads a box
// without Cloud as off.
func MigrateStatistics(kv KV, fleetReportOff bool) error {
	if v, ok := kv.LoadConfig(statsShareKey); ok && (v == "on" || v == "off" || v == "default") {
		return nil
	}
	g, err := loadJSON[grantRecord](kv, grantKey)
	if err != nil {
		return err
	}
	decision, _ := kv.LoadConfig(statsDecisionKey)
	disabled, _ := kv.LoadConfig(disabledKey)
	answeredYes := g != nil && !g.Revoked && g.StatsPolicy == cloudwire.StatsPolicy && len(g.StatsID) == 16
	share := "default"
	switch {
	case fleetReportOff || disabled == "true" || (decision != "" && !answeredYes):
		share = "off"
	case answeredYes && g.statisticsOnly():
		share = "on"
	}
	return kv.SaveConfig(statsShareKey, share)
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
