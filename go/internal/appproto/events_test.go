package appproto

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

type fakeJournal struct {
	spans   []JournalSpan
	notices []JournalNotice
	asked   JournalQuery
}

func (f *fakeJournal) Journal(_ context.Context, q JournalQuery) ([]JournalSpan, []JournalNotice, bool, error) {
	f.asked = q
	return f.spans, f.notices, false, nil
}

// An agent asks why the app warned this morning. It gets the box's own
// verdicts and numbers over the window, and no names.
func TestEventsReachAnAgentWithoutNames(t *testing.T) {
	r := newAgentRig(t)
	j := &fakeJournal{
		spans: []JournalSpan{{
			Driver: "battery.sungrow", Kind: "battery", Status: "limited", Reason: "power_below_target", Severity: "warning",
			Mode: "planner_arbitrage", StartMs: 1_000, EndMs: 961_000, Samples: 192,
			Requested: &protocol.PowerRange{Min: 4162, Max: 4361, Mean: 4255},
			Actual:    &protocol.PowerRange{Min: 1162, Max: 1362, Mean: 1255},
			Last:      json.RawMessage(`{"driver":"battery.sungrow","site_meter":"meter.p1","sent_w":4250,"readback_w":4239,"battery_soc":0.2}`),
		}, {Kind: "dispatch", Status: "free", Severity: "info", StartMs: 500}},
		notices: []JournalNotice{{TsMs: 2_000, Type: "control.not_following", Driver: "battery.sungrow", Status: "sent", Priority: 4}},
	}
	r.journal = j
	row, _ := r.grant(apiauth.RoleAgent, []string{protocol.ScopeEventsRead}, nil, protocol.Caps{}, time.Hour, protocol.Principal{})
	h, rec := r.agentSession(row)
	src := cloudwire.SourceID(testAgentKey, "battery.sungrow")
	deliver(t, h, protocol.MsgEventsGet, ptrU32(1), protocol.EventsGet{AfterMs: 100, BeforeMs: 2_000_000, Src: src, MinSeverity: "warning"})
	ev := body[protocol.Events](t, rec.last(t, protocol.MsgEvents))

	if j.asked.Driver != "battery.sungrow" || j.asked.SinceMs != 100 || j.asked.UntilMs != 2_000_000 ||
		len(j.asked.Severities) != 2 || j.asked.Limit != protocol.EventsMax {
		t.Fatalf("query %+v", j.asked)
	}
	if len(ev.Spans) != 2 || len(ev.Notices) != 1 {
		t.Fatalf("answer %+v", ev)
	}
	sp := ev.Spans[0]
	if sp.Src != src || sp.Status != "limited" || sp.ActualW == nil || sp.ActualW.Mean != 1255 || sp.Last["sent_w"] != 4250 || sp.Last["battery_soc"] != 0.2 {
		t.Fatalf("span %+v", sp)
	}
	if ev.Spans[1].Src != "" || ev.Notices[0].Src != src {
		t.Fatalf("ids: %+v / %+v", ev.Spans[1], ev.Notices[0])
	}
	raw, _ := json.Marshal(ev)
	for _, name := range []string{"battery.sungrow", "meter.p1"} {
		if strings.Contains(string(raw), name) {
			t.Errorf("the answer names %q: %s", name, raw)
		}
	}
}

func TestEventsRefusesWhatItCannotAnswer(t *testing.T) {
	r := newAgentRig(t)
	row, _ := r.grant(apiauth.RoleAgent, []string{protocol.ScopeEventsRead}, nil, protocol.Caps{}, time.Hour, protocol.Principal{})
	h, rec := r.agentSession(row)
	deliver(t, h, protocol.MsgEventsGet, ptrU32(1), protocol.EventsGet{})
	if got := errCode(t, rec); got != ErrUnavailable {
		t.Errorf("a box without a journal: %s", got)
	}
	r.journal = &fakeJournal{}
	h, rec = r.agentSession(row)
	for _, m := range []protocol.EventsGet{{AfterMs: 7, BeforeMs: 7}, {Src: "not-a-source"}, {MinSeverity: "loud"}} {
		rec.reset()
		deliver(t, h, protocol.MsgEventsGet, ptrU32(2), m)
		if got := errCode(t, rec); got != ErrUnknownOp {
			t.Errorf("%+v: %s", m, got)
		}
	}
}
