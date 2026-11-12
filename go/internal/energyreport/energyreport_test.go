package energyreport

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/statsrelay"
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

// Box to FTW Cloud: it reads the day's report, and a restart does not send
// the day again.
func TestABoxReportReachesFTWCloud(t *testing.T) {
	client, cloud := statsrelaytest.NewDirect(t, day.Add(30*time.Hour))
	kv := memKV{}
	s := newSender(t, Options{Ledger: whole(t), KV: kv, Client: client, Enabled: func() bool { return true },
		Now: func() time.Time { return day.Add(30 * time.Hour) }})

	s.send(context.Background(), day)
	got := cloud.Energy()
	if len(got) != 1 || got[0].Date != "2026-10-23" || got[0].GridImport != 24 || got[0].VehicleCharge != 24 {
		t.Fatalf("FTW Cloud got %+v", got)
	}
	if kv[lastDayKey] != "2026-10-23" || cloud.Answered() != http.StatusNoContent || len(cloud.Sizes()) != 1 {
		t.Fatalf("kv %v answered %d", kv, cloud.Answered())
	}
	s.send(context.Background(), day)
	if len(cloud.Sizes()) != 1 {
		t.Fatal("the same day went twice")
	}
}

func TestNothingIsSentWithoutTheOwnersAnswer(t *testing.T) {
	client, cloud := statsrelaytest.NewDirect(t, day.Add(30*time.Hour))
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
		o.Client, o.KV = client, memKV{}
		newSender(t, o).send(context.Background(), day)
		if len(cloud.Sizes()) != 0 {
			t.Fatalf("%s: sent", name)
		}
	}
}

// A box posts to FTW Cloud's energy route and to nothing else, and Settings
// says where.
func TestTheReportGoesStraightToFTWCloud(t *testing.T) {
	s := newSender(t, Options{Enabled: func() bool { return true }})
	if p := s.Preview(context.Background()); p.Endpoint != "https://cloud.ftw.energy/v1/energy" || p.Endpoint != statsrelay.DirectURL || !p.Sending || p.Day == "" {
		t.Fatalf("preview %+v", p)
	}
	if p := newSender(t, Options{}).Preview(context.Background()); p.Sending {
		t.Fatalf("preview without an answer %+v", p)
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
