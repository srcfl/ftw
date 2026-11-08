package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/journal"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// JournalVerdicts is what the control journal samples: each device
// function's control feedback as GET /api/status serves it, each source's
// freshness, whether the site may dispatch, and the operating mode.
func (s *Server) JournalVerdicts(now time.Time) []journal.Verdict {
	var out []journal.Verdict
	for _, f := range s.controlFeedback(now) {
		detail, _ := json.Marshal(f)
		out = append(out, journal.Verdict{
			Subject: f.Kind + ":" + f.Driver, Driver: f.Driver, Kind: f.Kind,
			Status: f.Status, Reason: f.Reason, Severity: f.Severity, Mode: f.Mode,
			RequestedW: f.RequestedW, ActualW: f.ActualW, Detail: detail,
		})
	}
	if s.deps.SiteDispatchBlocked != nil {
		v := journal.Verdict{Subject: "site:dispatch", Kind: "dispatch", Status: "free", Severity: "info"}
		if reason := s.deps.SiteDispatchBlocked(); reason != "" {
			v.Status, v.Reason, v.Severity = "blocked", reason, "warning"
		}
		out = append(out, v)
	}
	if s.deps.Ctrl != nil && s.deps.CtrlMu != nil {
		s.deps.CtrlMu.Lock()
		mode := string(s.deps.Ctrl.Mode)
		s.deps.CtrlMu.Unlock()
		out = append(out, journal.Verdict{Subject: "site:mode", Kind: "mode", Status: mode, Severity: "info", Mode: mode})
	}
	if s.deps.Tel != nil {
		disabled := map[string]bool{}
		if s.deps.Cfg != nil && s.deps.CfgMu != nil {
			s.deps.CfgMu.RLock()
			for _, d := range s.deps.Cfg.Drivers {
				disabled[d.Name] = d.Disabled
			}
			s.deps.CfgMu.RUnlock()
		}
		for name, h := range s.deps.Tel.AllHealth() {
			if disabled[name] {
				continue
			}
			out = append(out, sourceVerdict(name, h))
		}
	}
	return out
}

// sourceVerdict is a driver's health as Core's watchdog sees it: live,
// lagging after a failed read, offline once the watchdog gave up, or fault.
// It is the driver's state, not a reading's age, which control feedback
// judges per device. The error text changes from poll to poll, so it is
// detail, not the reason.
func sourceVerdict(name string, h telemetry.DriverHealth) journal.Verdict {
	v := journal.Verdict{Subject: "source:" + name, Driver: name, Kind: "source", Status: "live", Severity: "info"}
	switch {
	case h.DeviceFault:
		v.Status, v.Reason, v.Severity = "fault", "device_fault", "warning"
	case h.Status == telemetry.StatusOffline:
		v.Status, v.Reason, v.Severity = "offline", "telemetry_stale", "warning"
	case h.Status == telemetry.StatusDegraded:
		v.Status, v.Reason = "lagging", "read_failed"
	}
	detail := map[string]any{"status": h.Status.String(), "consecutive_errors": h.ConsecutiveErrors}
	if h.LastSuccess != nil {
		detail["last_success_ms"] = h.LastSuccess.UnixMilli()
	}
	if h.LastError != "" {
		detail["last_error"] = h.LastError
	}
	if h.DeviceFaultReason != "" {
		detail["device_fault_reason"] = h.DeviceFaultReason
	}
	v.Detail, _ = json.Marshal(detail)
	return v
}

// journalSpan is a span as GET /api/control/journal serves it.
type journalSpan struct {
	ID         int64           `json:"id"`
	Subject    string          `json:"subject"`
	Driver     string          `json:"driver,omitempty"`
	Kind       string          `json:"kind"`
	Status     string          `json:"status"`
	Reason     string          `json:"reason,omitempty"`
	Severity   string          `json:"severity,omitempty"`
	Mode       string          `json:"mode,omitempty"`
	StartMs    int64           `json:"start_ms"`
	EndMs      *int64          `json:"end_ms"`
	UpdatedMs  int64           `json:"updated_ms"`
	DurationS  float64         `json:"duration_s"`
	Samples    int64           `json:"samples"`
	RequestedW *powerSummary   `json:"requested_w"`
	ActualW    *powerSummary   `json:"actual_w"`
	First      json.RawMessage `json:"first,omitempty"`
	Last       json.RawMessage `json:"last,omitempty"`
}

type powerSummary struct {
	Min  float64 `json:"min"`
	Max  float64 `json:"max"`
	Mean float64 `json:"mean"`
	N    int64   `json:"n"`
}

func summarise(p state.PowerStats) *powerSummary {
	mean, ok := p.Mean()
	if !ok {
		return nil
	}
	return &powerSummary{Min: p.Min, Max: p.Max, Mean: mean, N: p.N}
}

func toJournalSpan(sp state.ControlSpan, nowMs int64) journalSpan {
	out := journalSpan{
		ID: sp.ID, Subject: sp.Subject, Driver: sp.Driver, Kind: sp.Kind, Status: sp.Status,
		Reason: sp.Reason, Severity: sp.Severity, Mode: sp.Mode, StartMs: sp.StartMs, UpdatedMs: sp.UpdatedMs,
		Samples: sp.Samples, RequestedW: summarise(sp.Requested), ActualW: summarise(sp.Actual),
		First: sp.First, Last: sp.Last,
	}
	end := nowMs
	if sp.EndMs != 0 {
		e := sp.EndMs
		out.EndMs, end = &e, e
	}
	out.DurationS = float64(end-sp.StartMs) / 1000
	return out
}

// journalSeverities turns ?severity= into the severities to keep: a single
// level keeps it and those above it; a comma list keeps exactly those. ok is
// false for a word that is not a severity.
func journalSeverities(q string) (keep []string, ok bool) {
	known := map[string]bool{"info": true, "warning": true, "alarm": true}
	if q == "" {
		return nil, true
	}
	if strings.Contains(q, ",") {
		for _, sev := range strings.Split(q, ",") {
			sev = strings.TrimSpace(sev)
			if !known[sev] {
				return nil, false
			}
			keep = append(keep, sev)
		}
		return keep, true
	}
	switch q {
	case "info":
		return nil, true
	case "warning":
		return []string{"warning", "alarm"}, true
	case "alarm":
		return []string{"alarm"}, true
	}
	return nil, false
}

// GET /api/control/journal answers "what happened" over a window: each
// stretch a device function, a source, the site's dispatch or the mode held
// one verdict, with the power asked for and delivered, each scored plan
// slot, and the notifications the box sent. since and until are Unix ms
// (the last 24 hours by default); subject keeps subjects with that prefix,
// e.g. battery: or source:easee; driver keeps one driver's spans and
// notifications; severity=warning keeps warning and alarm spans. Notices are
// what the owner was told and are not filtered by severity.
func (s *Server) handleControlJournal(w http.ResponseWriter, r *http.Request) {
	if s.deps.State == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the control journal needs the state store"})
		return
	}
	now := time.Now()
	q := r.URL.Query()
	untilMs, sinceMs := now.UnixMilli(), now.Add(-24*time.Hour).UnixMilli()
	for key, dst := range map[string]*int64{"since": &sinceMs, "until": &untilMs} {
		if v := q.Get(key); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": key + " must be Unix ms"})
				return
			}
			*dst = n
		}
	}
	if untilMs < sinceMs {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "until must be at or after since"})
		return
	}
	limit := 200
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "limit must be a positive number"})
			return
		}
		limit = min(n, 1000)
	}
	severities, ok := journalSeverities(q.Get("severity"))
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "severity must be info, warning or alarm, or a comma list of them"})
		return
	}
	spans, more, err := s.deps.State.ControlSpans(r.Context(), state.ControlSpanQuery{
		SinceMs: sinceMs, UntilMs: untilMs, SubjectPrefix: q.Get("subject"), Driver: q.Get("driver"),
		Severities: severities, Limit: limit,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	notices, noticesMore, err := s.deps.State.NotificationsBetween(r.Context(), sinceMs, untilMs, q.Get("driver"), limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := struct {
		SinceMs       int64                     `json:"since_ms"`
		UntilMs       int64                     `json:"until_ms"`
		Spans         []journalSpan             `json:"spans"`
		More          bool                      `json:"more"`
		Notifications []state.NotificationEntry `json:"notifications"`
		NoticesMore   bool                      `json:"notifications_more"`
	}{SinceMs: sinceMs, UntilMs: untilMs, Spans: []journalSpan{}, More: more, Notifications: notices, NoticesMore: noticesMore}
	for _, sp := range spans {
		out.Spans = append(out.Spans, toJournalSpan(sp, now.UnixMilli()))
	}
	writeJSON(w, http.StatusOK, out)
}
