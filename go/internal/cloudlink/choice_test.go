package cloudlink

import (
	"crypto/ecdsa"
	"errors"
	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func TestCloudTermsIncludeStatisticsAndCannotOptOutSeparately(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	if err := h.link.Grant(GrantRequest{Scopes: []string{protocol.ScopeLiveRead}, Policy: cloudwire.CloudTermsPolicy, By: "owner"}); err != nil {
		t.Fatal(err)
	}
	r := h.nextRoster(t)
	if r.Grant.TermsPolicy != cloudwire.CloudTermsPolicy || r.Grant.StatsPolicy != cloudwire.StatsPolicy || len(r.Grant.StatsID) != 16 || r.Grant.StatsSinceMs <= 0 {
		t.Fatal("Cloud did not include the accepted statistics scope")
	}
	if err := h.link.StopStats("owner"); !errors.Is(err, ErrCloudNeedsStatistics) {
		t.Fatal("Cloud could opt out independently", err)
	}
	if s := h.link.Status(); !s.CloudTermsAccepted || !s.StatisticsActive || !s.Statistics || !s.StatisticsRequired {
		t.Fatal("new terms not active")
	}
}

// Without FTW Cloud nothing is shared (Fredrik, 28 November 2026): asking
// for statistics is refused and links nothing to Cloud, and turning them off
// has nothing to stop.
func TestWithoutCloudStatisticsAreRefusedAndNothingIsShared(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	if s := h.link.Status(); s.StatisticsDecisionRequired || s.Statistics || s.StatisticsRequired || h.link.SharesStatistics() {
		t.Fatal("a box without Cloud asked, or shared", s)
	}
	if err := h.link.Stats(StatsRequest{Policy: cloudwire.StatsPolicy, By: "local owner"}); !errors.Is(err, ErrNotGranted) {
		t.Fatalf("statistics without Cloud: %v", err)
	}
	if s := h.link.Status(); s.Statistics || s.Link != "off" || s.StatisticsOnly || h.link.SharesStatistics() {
		t.Fatal("a refused request shared statistics", s)
	}
	if h.link.Home() != "" || h.link.SessionInfo() != nil || h.link.current().copyOn() || h.link.current().g != nil {
		t.Fatal("asking for statistics enabled Cloud functions")
	}
	if err := h.link.StopStats("local owner"); err != nil {
		t.Fatal(err)
	}
	if s := h.link.Status(); s.Statistics || s.Link != "off" || s.StatisticsActive {
		t.Fatal("off did not stay off and local", s)
	}
}

func TestEndingCloudPreventsAutomaticRelinkFromPairedPhones(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	h.grant(t)
	h.nextRoster(t)
	if err := h.link.End("owner"); err != nil {
		t.Fatal(err)
	}
	if h.link.TakesTun() || h.link.wantsLink() {
		t.Fatal("disabled Cloud still wanted remote access")
	}
	h.link.RosterChanged()
	disabled, ok := h.link.opts.KV.LoadConfig(disabledKey)
	if !ok || disabled != "true" {
		t.Fatal("off choice was not durable")
	}
}

// Whatever an earlier release stored as the local answer, it is written
// "off" once. Nothing reads it now; off keeps a rollback from sending from a
// box without Cloud.
func TestEveryEarlierLocalAnswerIsRetiredAsOff(t *testing.T) {
	for _, before := range []string{"", "on", "default", "off", "unreadable"} {
		kv := &memKV{m: map[string]string{}}
		if before != "" {
			kv.m[statsShareKey] = before
		}
		for range 2 {
			if err := RetireLocalStatistics(kv); err != nil {
				t.Fatal(err)
			}
			if v, _ := kv.LoadConfig(statsShareKey); v != "off" {
				t.Fatalf("%q: stored %q, want off", before, v)
			}
		}
	}
}

// The old local answer counts for nothing. Without Cloud no answer sends;
// under the current terms an old "off" does not stop what the terms include;
// Cloud turned off sends none.
func TestTheOldLocalAnswerCountsForNothing(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	answers := []string{"on", "default", "off"}
	for _, share := range answers {
		if err := h.kv.SaveConfig(statsShareKey, share); err != nil {
			t.Fatal(err)
		}
		if h.link.SharesStatistics() || h.link.Status().Statistics {
			t.Fatalf("a box without Cloud shares with the old answer %q", share)
		}
	}
	if err := h.link.Grant(GrantRequest{Scopes: []string{protocol.ScopeLiveRead}, Policy: cloudwire.CloudTermsPolicy, By: "owner"}); err != nil {
		t.Fatal(err)
	}
	h.nextRoster(t)
	for _, share := range answers {
		if err := h.kv.SaveConfig(statsShareKey, share); err != nil {
			t.Fatal(err)
		}
		if !h.link.SharesStatistics() || !h.link.Status().Statistics {
			t.Fatalf("the current Cloud terms do not share with the old answer %q", share)
		}
	}
	if err := h.link.End("owner"); err != nil {
		t.Fatal(err)
	}
	if h.link.SharesStatistics() || h.link.Status().Statistics {
		t.Fatal("Cloud turned off still shares")
	}
}

func TestStatisticsClockStartsAfterNTPWithoutBackfill(t *testing.T) {
	now := time.Date(2026, 10, 17, 13, 45, 0, 0, time.UTC)
	synced := false
	kv := &memKV{}
	g := grantRecord{StatsPolicy: cloudwire.StatsPolicy, StatsID: make([]byte, 16)}
	l := &Link{log: slog.Default(), opts: Options{KV: kv, Now: func() time.Time { return now }, ClockSynced: func() bool { return synced }}, grant: &g}
	if err := l.startStatisticsClock(); err != nil {
		t.Fatal(err)
	}
	if l.grant.StatsAtMs != 0 {
		t.Fatal("an unknown clock started collection")
	}
	synced = true
	if err := l.startStatisticsClock(); err != nil {
		t.Fatal(err)
	}
	if l.grant.StatsAtMs != now.UnixMilli() {
		t.Fatal("clock backfilled the earlier choice")
	}
	now = now.Add(time.Hour)
	if err := l.startStatisticsClock(); err != nil {
		t.Fatal(err)
	}
	if l.grant.StatsAtMs != now.Add(-time.Hour).UnixMilli() {
		t.Fatal("clock moved on reconnect")
	}
}

type failingChoiceKV struct {
	KV
	armed *atomic.Bool
	key   string
}

func (f failingChoiceKV) SaveConfig(key, value string) error {
	if f.armed.Load() && key == f.key {
		return errors.New("choice storage unavailable")
	}
	return f.KV.SaveConfig(key, value)
}

func TestWholeCloudOffStopsImmediatelyWhenLaterStorageFails(t *testing.T) {
	for _, failKey := range []string{linkKey, grantKey} {
		t.Run(failKey, func(t *testing.T) {
			var armed atomic.Bool
			svc := newFakeService(t)
			h := newHarnessWith(t, svc, []*ecdsa.PublicKey{&svc.key.PublicKey}, nil, func(o *Options) { o.KV = failingChoiceKV{KV: o.KV, armed: &armed, key: failKey} })
			if err := h.link.Grant(GrantRequest{Scopes: []string{protocol.ScopeLiveRead}, Policy: cloudwire.CloudTermsPolicy, By: "owner"}); err != nil {
				t.Fatal(err)
			}
			h.nextRoster(t)
			armed.Store(true)
			if err := h.link.End("owner"); err == nil {
				t.Fatal("storage failure was not returned")
			}
			if h.link.TakesTun() || h.link.Home() != "" || h.link.SessionInfo() != nil || h.link.current().copyOn() || h.link.SharesStatistics() {
				t.Fatal("a failed follow-up write left Cloud sharing active")
			}
			if disabled, _ := h.kv.LoadConfig(disabledKey); disabled != "true" {
				t.Fatal("off choice was not saved")
			}
		})
	}
}

// FTW Cloud turned off takes no notice, even with a link up: a grant that
// could not clear the off choice leaves the link it made. The box's
// notification history reads a taken notice as queued for the owners'
// phones (#1785).
func TestNoNoticeIsTakenWhileCloudIsOff(t *testing.T) {
	var armed atomic.Bool
	svc := newFakeService(t)
	h := newHarnessWith(t, svc, []*ecdsa.PublicKey{&svc.key.PublicKey}, nil, func(o *Options) { o.KV = failingChoiceKV{KV: o.KV, armed: &armed, key: disabledKey} })
	if err := h.link.End("owner"); err != nil {
		t.Fatal(err)
	}
	armed.Store(true)
	if err := h.link.Grant(GrantRequest{Scopes: []string{protocol.ScopeLiveRead}, Policy: cloudwire.CloudTermsPolicy, By: "owner"}); err == nil {
		t.Fatal("storage failure was not returned")
	}
	if snap := h.link.current(); snap.link == nil || snap.link.Ending {
		t.Fatal("the failed grant left no live link to test against")
	}
	if h.link.Notice(protocol.PushOwnerAdded, "") {
		t.Fatal("FTW Cloud turned off took a notice")
	}
}
