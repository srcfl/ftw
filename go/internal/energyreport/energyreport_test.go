package energyreport

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/statsrelay/statsrelaytest"
)

func TestTheSendTimeIsDrawnInTheWindowAfterTheDayCloses(t *testing.T) {
	closed := day.AddDate(0, 0, 1)
	for name, c := range map[string]struct {
		now      time.Time
		random   float64
		sent     string
		wantDay  time.Time
		from, to time.Time
	}{
		"boot before the window":     {closed.Add(20 * time.Minute), 0, "", day, closed.Add(time.Hour), closed.Add(time.Hour)},
		"latest in the window":       {closed.Add(20 * time.Minute), 1, "", day, closed.Add(12 * time.Hour), closed.Add(12 * time.Hour)},
		"boot inside the window":     {closed.Add(6 * time.Hour), 0.5, "", day, closed.Add(9 * time.Hour), closed.Add(9 * time.Hour)},
		"boot after the window":      {closed.Add(13 * time.Hour), 0, "", closed, closed.Add(25 * time.Hour), closed.Add(25 * time.Hour)},
		"that day was sent already":  {closed.Add(6 * time.Hour), 0, "2026-10-23", closed, closed.Add(25 * time.Hour), closed.Add(25 * time.Hour)},
		"a later day was sent first": {closed.Add(6 * time.Hour), 0, "2026-10-24", closed, closed.Add(25 * time.Hour), closed.Add(25 * time.Hour)},
	} {
		kv := memKV{}
		if c.sent != "" {
			kv[lastDayKey] = c.sent
		}
		s := newSender(t, Options{KV: kv, Random: func() float64 { return c.random }})
		p := s.draw(c.now)
		if !p.day.Equal(c.wantDay) || p.at.Before(c.from) || p.at.After(c.to) || !p.live(c.now) {
			t.Fatalf("%s: plan %+v", name, p)
		}
	}
	// A clock that jumps a year makes the plan stale.
	p := plan{day: day, at: day.Add(30 * time.Hour)}
	if p.live(day.AddDate(1, 0, 0)) || p.live(day.AddDate(-1, 0, 0)) || !p.live(day.Add(30*time.Hour)) {
		t.Fatal("a stale plan stayed live")
	}
}

// Box to relay to gateway: the gateway reads the day's report, and a restart
// does not send the day again.
func TestABoxReportReachesTheGatewayThroughTheRelay(t *testing.T) {
	client, gw, relay := statsrelaytest.New(t, day.Add(30*time.Hour))
	kv := memKV{}
	s := newSender(t, Options{Ledger: whole(t), KV: kv, Relay: client, Enabled: func() bool { return true },
		Now: func() time.Time { return day.Add(30 * time.Hour) }})

	s.send(context.Background(), day)
	got := gw.Energy()
	if len(got) != 1 || got[0].Date != "2026-10-23" || got[0].GridImport != 24 || got[0].VehicleCharge != 24 {
		t.Fatalf("gateway got %+v", got)
	}
	if kv[lastDayKey] != "2026-10-23" || gw.Answered() != http.StatusNoContent || len(relay.Bodies()) != 1 {
		t.Fatalf("kv %v answered %d", kv, gw.Answered())
	}
	s.send(context.Background(), day)
	if len(gw.Energy()) != 1 {
		t.Fatal("the same day went twice")
	}
}

func TestNothingIsSentWithoutTheOwnersAnswer(t *testing.T) {
	client, _, relay := statsrelaytest.New(t, day.Add(30*time.Hour))
	ledger := whole(t)
	var mu sync.Mutex
	reads := 0
	for name, o := range map[string]Options{
		"no answer":        {Enabled: nil},
		"the answer is no": {Enabled: func() bool { return false }},
		"clock not set":    {Enabled: func() bool { return true }, ClockSynced: func() bool { return false }},
		// Withdrawn while the ledger was being read.
		"withdrawn mid-read": {Enabled: func() bool {
			mu.Lock()
			defer mu.Unlock()
			reads++
			return reads == 1
		}},
		"incomplete day": {Enabled: func() bool { return true }, Ledger: wholeDay(t, func(id string, at time.Time) bool { return id == "inverter:1/pv" && at.After(day.Add(time.Hour)) })},
	} {
		if o.Ledger == nil {
			o.Ledger = ledger
		}
		o.Relay, o.KV = client, memKV{}
		newSender(t, o).send(context.Background(), day)
		if len(relay.Bodies()) != 0 {
			t.Fatalf("%s: sent", name)
		}
	}
}

// Without a relay and a gateway key, Run returns and never reads the ledger.
func TestWithoutARelayRunSendsNothing(t *testing.T) {
	l := &countingLedger{}
	s := newSender(t, Options{Ledger: l, Enabled: func() bool { return true }})
	done := make(chan struct{})
	go func() { s.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run waited with nowhere to send")
	}
	if l.n != 0 {
		t.Fatal("read the ledger with nowhere to send")
	}
	if p := s.Preview(context.Background()); p.Relay != "" || p.Day == "" {
		t.Fatalf("preview %+v", p)
	}
}

func newSender(t *testing.T, o Options) *Sender {
	t.Helper()
	if o.Ledger == nil {
		o.Ledger = &countingLedger{}
	}
	s, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type memKV map[string]string

func (m memKV) LoadConfig(k string) (string, bool) { v, ok := m[k]; return v, ok }
func (m memKV) SaveConfig(k, v string) error       { m[k] = v; return nil }

type countingLedger struct{ n int }

func (c *countingLedger) EnergyAssets() ([]state.EnergyAsset, error) { c.n++; return nil, nil }
func (c *countingLedger) LoadEnergyHistoryContext(context.Context, state.EnergyHistoryQuery) ([]state.EnergyLedgerPoint, bool, error) {
	c.n++
	return nil, false, nil
}
