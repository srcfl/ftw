package appproto

import (
	"context"
	"encoding/json"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/srcfl/ftw/go/protocol"
)

// JournalSpan is a control journal span as events.get reads it, before
// its driver name becomes an agent id.
type JournalSpan struct {
	Driver    string
	Kind      string
	Status    string
	Reason    string
	Severity  string
	Mode      string
	StartMs   int64
	EndMs     int64
	Samples   int64
	Requested *protocol.PowerRange
	Actual    *protocol.PowerRange
	// Last is the verdict as Core served it at the latest sample.
	Last json.RawMessage
}

// JournalNotice is one notification attempt as events.get reads it.
type JournalNotice struct {
	TsMs     int64
	Type     string
	Driver   string
	Status   string
	Priority int
}

// JournalQuery picks what events.get reads.
type JournalQuery struct {
	SinceMs, UntilMs int64
	// Driver keeps one source; empty keeps all.
	Driver string
	// Severities keeps these; empty keeps all.
	Severities []string
	Limit      int
}

// JournalSource is the box's control journal and notification history.
type JournalSource interface {
	Journal(ctx context.Context, q JournalQuery) (spans []JournalSpan, notices []JournalNotice, more bool, err error)
}

// eventsWindow is how far back events.get looks when asked for no window.
const eventsWindow = 24 * time.Hour

// onEventsGet answers what the box recorded about control over a window.
// Names stay home: a source goes by its agent id, and only the numbers of a
// verdict's text leave the box.
func (h *Handler) onEventsGet(env Envelope) error {
	if h.cfg.Journal == nil {
		return h.refuse(env.ID, ErrUnavailable, map[string]any{"subsystem": "events"})
	}
	var m protocol.EventsGet
	if len(env.B) > 0 {
		if err := Unmarshal(env.B, &m); err != nil {
			return h.refuse(env.ID, ErrUnknownOp, map[string]any{"t": env.T})
		}
	}
	if m.BeforeMs < 0 || m.AfterMs < 0 || (m.BeforeMs > 0 && m.AfterMs >= m.BeforeMs) {
		return h.refuse(env.ID, ErrUnknownOp, map[string]any{"t": env.T, "field": "beforeMs"})
	}
	q := JournalQuery{SinceMs: m.AfterMs, UntilMs: m.BeforeMs, Limit: int(m.N)}
	nowMs, _ := h.wall()
	if q.UntilMs == 0 {
		q.UntilMs = nowMs
	}
	if q.SinceMs == 0 {
		q.SinceMs = q.UntilMs - eventsWindow.Milliseconds()
	}
	if q.Limit <= 0 || q.Limit > protocol.EventsMax {
		q.Limit = protocol.EventsMax
	}
	switch m.MinSeverity {
	case "", "info":
	case "warning":
		q.Severities = []string{"warning", "alarm"}
	case "alarm":
		q.Severities = []string{"alarm"}
	default:
		return h.refuse(env.ID, ErrUnknownOp, map[string]any{"t": env.T, "field": "minSeverity"})
	}
	if m.Src != "" {
		driver, ok := h.sourceForID(m.Src)
		if !ok {
			return h.refuse(env.ID, ErrUnknownOp, map[string]any{"t": env.T, "field": "src"})
		}
		q.Driver = driver
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	spans, notices, more, err := h.cfg.Journal.Journal(ctx, q)
	if err != nil {
		return h.refuse(env.ID, ErrUnavailable, map[string]any{"subsystem": "events"})
	}
	out := protocol.Events{Events: []cbor.RawMessage{}, Spans: []protocol.EventSpan{}, Notices: []protocol.EventNotice{}, More: more}
	for _, sp := range spans {
		es := protocol.EventSpan{
			Kind: sp.Kind, Status: sp.Status, Reason: sp.Reason, Severity: sp.Severity, Mode: sp.Mode,
			StartMs: sp.StartMs, EndMs: sp.EndMs, Samples: sp.Samples,
			RequestedW: sp.Requested, ActualW: sp.Actual, Last: verdictNumbers(sp.Last),
		}
		if sp.Driver != "" {
			es.Src = h.srcID(sp.Driver)
		}
		out.Spans = append(out.Spans, es)
	}
	for _, n := range notices {
		en := protocol.EventNotice{Ts: n.TsMs, Type: n.Type, Status: n.Status, Priority: n.Priority}
		if n.Driver != "" {
			en.Src = h.srcID(n.Driver)
		}
		out.Notices = append(out.Notices, en)
	}
	// One bulk frame holds the answer; the oldest give way and More says so.
	for !h.fits(protocol.MsgEvents, env.ID, out) && (len(out.Spans) > 0 || len(out.Notices) > 0) {
		out.More = true
		if len(out.Notices) > len(out.Spans) {
			out.Notices = out.Notices[:len(out.Notices)*3/4]
		} else {
			out.Spans = out.Spans[:len(out.Spans)*3/4]
		}
	}
	return h.sendBulk(protocol.MsgEvents, env.ID, out)
}

// sourceForID finds the source an agent id names.
func (h *Handler) sourceForID(id string) (string, bool) {
	if h.cfg.Site == nil {
		return "", false
	}
	for _, s := range h.cfg.Site.Snapshot().Sources {
		if h.srcID(s.ID) == id {
			return s.ID, true
		}
	}
	return "", false
}

// verdictNumbers keeps the numeric fields of a verdict's text. Names are
// text and stay home.
func verdictNumbers(raw json.RawMessage) map[string]float64 {
	if len(raw) == 0 {
		return nil
	}
	var fields map[string]any
	if json.Unmarshal(raw, &fields) != nil {
		return nil
	}
	out := map[string]float64{}
	for k, v := range fields {
		if f, ok := v.(float64); ok {
			out[k] = f
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
