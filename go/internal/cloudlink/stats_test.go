package cloudlink

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

type statsSite struct{ *fakeSite }

func (s statsSite) Statistics(_ context.Context, now time.Time) (cloudwire.Stats, bool) {
	return cloudwire.Stats{HourMS: now.UTC().Truncate(time.Hour).Add(-time.Hour).UnixMilli(), Metrics: map[string]int64{"pv_generation_wh": 120}}, true
}

func statsHarness(t *testing.T, approved bool) (*fakeService, *harness) {
	svc := newFakeService(t)
	if approved {
		svc.statsPolicy = cloudwire.StatsPolicy
	}
	h := newHarnessWith(t, svc, []*ecdsa.PublicKey{&svc.key.PublicKey}, nil, func(o *Options) {
		o.Site = statsSite{&fakeSite{}}
		o.Now = func() time.Time { return time.Now().Add(-2 * time.Hour) }
	})
	return svc, h
}

func sentStats(svc *fakeService) bool {
	for _, e := range svc.received() {
		if e.T == cloudwire.MsgStats {
			return true
		}
	}
	return false
}

// The hourly upload goes only under the current Cloud terms. Asking for it
// under older terms is refused, and a grant from before this rule that still
// carries the stats purpose under older terms sends nothing either.
func TestStatisticsSendsSignedClosedHourOnlyWithCurrentConsent(t *testing.T) {
	svc, h := statsHarness(t, true)
	h.grant(t)
	h.nextRoster(t)
	if err := h.link.Stats(StatsRequest{Policy: cloudwire.StatsPolicy}); !errors.Is(err, ErrCloudTermsFirst) {
		t.Fatalf("statistics under older terms: %v", err)
	}
	// As an earlier release could leave it: older terms with the stats purpose.
	g := h.record(t)
	g.Purposes, g.StatsID, g.StatsPolicy, g.StatsAtMs = []string{protocol.CloudPurposeServe, protocol.CloudPurposeStats}, make([]byte, statsIDBytes), cloudwire.StatsPolicy, 1
	h.link.mu.Lock()
	err := h.link.storeLocked(g, "test")
	h.link.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	h.nextRoster(t)
	time.Sleep(1500 * time.Millisecond)
	if sentStats(svc) {
		t.Fatal("older terms sent an hour")
	}

	if err := h.link.Grant(GrantRequest{Scopes: []string{protocol.ScopeLiveRead}, Policy: cloudwire.CloudTermsPolicy, By: "owner"}); err != nil {
		t.Fatal(err)
	}
	r := h.nextRoster(t)
	e := svc.waitFor(cloudwire.MsgStats, 3*time.Second)
	var report cloudwire.Stats
	if err := cloudwire.Unmarshal(e.B, &report); err != nil {
		t.Fatal(err)
	}
	if !report.Valid(time.Now().UnixMilli()) || len(r.Grant.StatsID) != 16 || r.Grant.StatsPolicy != cloudwire.StatsPolicy {
		t.Fatalf("invalid report or roster %+v %+v", report, r.Grant)
	}
	if !h.link.Status().StatisticsActive {
		t.Fatal("current consent inactive")
	}
	if err := h.link.StopStats("box"); !errors.Is(err, ErrCloudNeedsStatistics) {
		t.Fatalf("Cloud stopped statistics alone: %v", err)
	}
	if err := h.link.End("box"); err != nil {
		t.Fatal(err)
	}
	if h.link.Status().StatisticsActive {
		t.Fatal("ended Cloud kept statistics active")
	}
}

// A statistics-only link, which an earlier release started for a box
// without Cloud, sends no hour whatever the old local answer says.
// ReconcileStatistics ends it, so the service deletes its rows.
func TestAStatisticsOnlyLinkSendsNothingAndEnds(t *testing.T) {
	svc, h := statsHarness(t, true)
	h.grant(t)
	h.nextRoster(t)
	g := h.record(t)
	g.Policy, g.Scopes, g.Purposes = cloudwire.StatsPolicy, []string{}, []string{protocol.CloudPurposeStats}
	g.StatsID, g.StatsPolicy, g.StatsAtMs = make([]byte, statsIDBytes), cloudwire.StatsPolicy, 1
	if err := h.kv.SaveConfig(statsShareKey, "on"); err != nil {
		t.Fatal(err)
	}
	h.link.mu.Lock()
	err := h.link.storeLocked(g, "test")
	h.link.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	h.nextRoster(t)
	time.Sleep(1500 * time.Millisecond)
	if sentStats(svc) {
		t.Fatal("a statistics-only link sent an hour")
	}
	if st := h.link.Status(); st.Statistics || st.StatisticsActive || !st.StatisticsOnly || h.link.SharesStatistics() {
		t.Fatalf("status says statistics run: %+v", st)
	}
	if err := h.link.ReconcileStatistics("test"); err != nil {
		t.Fatal(err)
	}
	if rec := h.record(t); !rec.Revoked || h.link.Status().Link != "ending" {
		t.Fatalf("the statistics-only link did not end: %+v %+v", rec, h.link.Status())
	}
}

// Cloud under older terms keeps running, but a stats purpose an earlier
// release left on its grant goes: those terms did not include statistics.
// Under the current terms ReconcileStatistics changes nothing.
func TestOlderTermsLoseALeftoverStatsPurpose(t *testing.T) {
	_, h := statsHarness(t, true)
	h.grant(t)
	h.nextRoster(t)
	g := h.record(t)
	g.Purposes, g.StatsID, g.StatsPolicy, g.StatsAtMs = []string{protocol.CloudPurposeServe, protocol.CloudPurposeStats}, make([]byte, statsIDBytes), cloudwire.StatsPolicy, 1
	h.link.mu.Lock()
	err := h.link.storeLocked(g, "test")
	h.link.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	h.nextRoster(t)
	if err := h.link.ReconcileStatistics("test"); err != nil {
		t.Fatal(err)
	}
	if rec := h.record(t); rec.Revoked || len(rec.StatsID) != 0 || len(rec.Purposes) != 1 || rec.Purposes[0] != protocol.CloudPurposeServe || h.link.Status().Link != "on" {
		t.Fatalf("older terms after reconcile: %+v", rec)
	}

	if err := h.link.Grant(GrantRequest{Scopes: []string{protocol.ScopeLiveRead}, Policy: cloudwire.CloudTermsPolicy, By: "owner"}); err != nil {
		t.Fatal(err)
	}
	h.nextRoster(t)
	before := h.record(t)
	if err := h.link.ReconcileStatistics("test"); err != nil {
		t.Fatal(err)
	}
	if after := h.record(t); len(after.Purposes) != 2 || after.Purposes[1] != protocol.CloudPurposeStats ||
		after.StatsPolicy != cloudwire.StatsPolicy || string(after.StatsID) != string(before.StatsID) {
		t.Fatalf("the current terms changed: %+v", after)
	}
}

func TestStatisticsWaitsForTheServicesApprovedPolicy(t *testing.T) {
	svc, h := statsHarness(t, false)
	if err := h.link.Grant(GrantRequest{Scopes: []string{protocol.ScopeLiveRead}, Policy: cloudwire.CloudTermsPolicy, By: "owner"}); err != nil {
		t.Fatal(err)
	}
	h.nextRoster(t)
	if h.link.Status().StatisticsReady {
		t.Fatal("unapproved service declared ready")
	}
	time.Sleep(time.Second)
	if sentStats(svc) {
		t.Fatal("statistics sent without service approval")
	}
}
