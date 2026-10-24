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

// A statistics-only link from a local yes follows the one answer: off sends
// no hour, and ReconcileStatistics ends the link as turning statistics off
// does.
func TestALocalNoStopsTheHourlyUpload(t *testing.T) {
	svc, h := statsHarness(t, true)
	h.grant(t)
	h.nextRoster(t)
	g := h.record(t)
	g.Policy, g.Scopes, g.Purposes = cloudwire.StatsPolicy, []string{}, []string{protocol.CloudPurposeStats}
	g.StatsID, g.StatsPolicy, g.StatsAtMs = make([]byte, statsIDBytes), cloudwire.StatsPolicy, 1
	if err := h.kv.SaveConfig(statsShareKey, "off"); err != nil {
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
		t.Fatal("an answer of off sent an hour")
	}
	if st := h.link.Status(); st.Statistics || st.StatisticsActive {
		t.Fatalf("status says statistics run: %+v", st)
	}

	// Sharing only by default is no given yes, so no hour either.
	if err := h.kv.SaveConfig(statsShareKey, "default"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	if sentStats(svc) {
		t.Fatal("an answer that was never given sent an hour")
	}

	// The same link with the answer on does send, so the answer is the gate.
	if err := h.kv.SaveConfig(statsShareKey, "on"); err != nil {
		t.Fatal(err)
	}
	svc.waitFor(cloudwire.MsgStats, 3*time.Second)

	if err := h.kv.SaveConfig(statsShareKey, "off"); err != nil {
		t.Fatal(err)
	}
	if err := h.link.ReconcileStatistics("test"); err != nil {
		t.Fatal(err)
	}
	if rec := h.record(t); !rec.Revoked || h.link.Status().Link != "ending" {
		t.Fatalf("the statistics-only link did not end: %+v %+v", rec, h.link.Status())
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
