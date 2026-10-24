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

// Without Cloud, statistics are an answer on the box. Turning them on or off
// links nothing to Cloud and publishes no remote access.
func TestLocalStatisticsLinkNothingToCloud(t *testing.T) {
	svc := newFakeService(t)
	h := newHarness(t, svc)
	if s := h.link.Status(); s.StatisticsDecisionRequired || s.Statistics || s.StatisticsRequired {
		t.Fatal("a box asked, or shared before its answer was recorded", s)
	}
	if err := h.link.Stats(StatsRequest{Policy: cloudwire.StatsPolicy, By: "local owner"}); err != nil {
		t.Fatal(err)
	}
	s := h.link.Status()
	if !s.Statistics || s.StatisticsRequired || s.Link != "off" || s.StatisticsOnly || !SharesStatistics(h.kv) {
		t.Fatal("local statistics are not a local answer", s)
	}
	if h.link.Home() != "" || h.link.SessionInfo() != nil || h.link.current().copyOn() || h.link.current().g != nil {
		t.Fatal("local statistics enabled Cloud functions")
	}
	if err := h.link.StopStats("local owner"); err != nil {
		t.Fatal(err)
	}
	if s := h.link.Status(); s.Statistics || s.Link != "off" || s.StatisticsActive || SharesStatistics(h.kv) {
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

// Every earlier No stays no; every other box shares, since a box without
// Cloud shares until its owner turns it off. The answer is written once.
func TestMigrationKeepsEveryEarlierNo(t *testing.T) {
	yes := grantRecord{StatsPolicy: cloudwire.StatsPolicy, StatsID: make([]byte, 16), Purposes: []string{protocol.CloudPurposeStats}}
	for name, c := range map[string]struct {
		kv       map[string]string
		grant    *grantRecord
		fleetOff bool
		want     bool
	}{
		"never asked":           {want: true},
		"local yes":             {grant: &yes, want: true},
		"local no":              {kv: map[string]string{statsDecisionKey: cloudwire.StatsPolicy}, want: false},
		"no to an older policy": {kv: map[string]string{statsDecisionKey: "ftw-project-stats-2026-10-v1"}, want: false},
		"Cloud turned off":      {kv: map[string]string{disabledKey: "true"}, want: false},
		"fleet report off":      {fleetOff: true, want: false},
		"yes, fleet report off": {grant: &yes, fleetOff: true, want: false},
		"already answered: on":  {kv: map[string]string{statsShareKey: "on", disabledKey: "true"}, want: true},
		"already on by default": {kv: map[string]string{statsShareKey: "default"}, want: true},
		"already answered: off": {kv: map[string]string{statsShareKey: "off"}, want: false},
	} {
		kv := &memKV{m: map[string]string{}}
		for k, v := range c.kv {
			kv.m[k] = v
		}
		if c.grant != nil {
			if err := saveJSON(kv, grantKey, *c.grant); err != nil {
				t.Fatal(err)
			}
		}
		if SharesStatistics(kv) && c.kv[statsShareKey] == "" {
			t.Fatalf("%s: shared before the answer was written", name)
		}
		if err := MigrateStatistics(kv, c.fleetOff); err != nil {
			t.Fatal(err)
		}
		if got := SharesStatistics(kv); got != c.want {
			t.Fatalf("%s: shares %v, want %v", name, got, c.want)
		}
		// Only a yes that was given counts as one.
		if said := SaidYesToStatistics(kv); said != (c.want && (c.grant != nil || c.kv[statsShareKey] == "on")) {
			t.Fatalf("%s: said yes %v", name, said)
		}
		// Written once: a later start reads the answer, not the old flags.
		if err := MigrateStatistics(kv, !c.fleetOff); err != nil || SharesStatistics(kv) != c.want {
			t.Fatalf("%s: a second start changed the answer", name)
		}
	}
}

// Without a running link no stored grant counts, so the box's own answer
// alone decides. The link's answer, with the grant it honours, is tested in
// link_test.go.
func TestWithoutALinkOnlyTheLocalAnswerCounts(t *testing.T) {
	kv := &memKV{m: map[string]string{statsShareKey: "off"}}
	cloud := grantRecord{Policy: cloudwire.CloudTermsPolicy, Purposes: []string{protocol.CloudPurposeServe, protocol.CloudPurposeStats}}
	if err := saveJSON(kv, grantKey, cloud); err != nil {
		t.Fatal(err)
	}
	for share, want := range map[string][2]bool{"off": {false, false}, "default": {true, false}, "on": {true, true}} {
		kv.m[statsShareKey] = share
		if got := [2]bool{SharesStatistics(kv), SaidYesToStatistics(kv)}; got != want {
			t.Fatalf("%s: shares, said yes = %v, want %v", share, got, want)
		}
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
	for _, failKey := range []string{statsDecisionKey, linkKey, grantKey} {
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
			if h.link.TakesTun() || h.link.Home() != "" || h.link.SessionInfo() != nil || h.link.current().copyOn() || SharesStatistics(h.kv) {
				t.Fatal("a failed follow-up write left Cloud sharing active")
			}
			if disabled, _ := h.kv.LoadConfig(disabledKey); disabled != "true" {
				t.Fatal("off choice was not saved")
			}
		})
	}
}
