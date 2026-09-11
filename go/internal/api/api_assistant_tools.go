package api

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/assistant"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func (s *Server) runAssistantTool(name string, args json.RawMessage) (string, error) {
	switch name {
	case assistant.ToolSupportReport:
		if s.deps.Ctrl == nil || s.deps.Tel == nil {
			return "", fmt.Errorf("site state is not available")
		}
		return s.buildSupportReport(context.Background(), time.Now()), nil
	case assistant.ToolDriverHealth:
		return s.toolDriverHealth(args), nil
	case assistant.ToolRecentLogs:
		return s.toolRecentLogs(args), nil
	case assistant.ToolPlanNow:
		return s.toolPlanNow(), nil
	case assistant.ToolVersion:
		v := strings.TrimSpace(s.deps.Version)
		if v == "" {
			v = "dev"
		}
		return "FTW version " + v, nil
	default:
		return "unknown tool; Ask why is read-only", nil
	}
}

func (s *Server) toolDriverHealth(args json.RawMessage) string {
	if s.deps.Tel == nil {
		return "no telemetry"
	}
	var in struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(args, &in)
	want := strings.TrimSpace(in.Name)
	health := s.deps.Tel.AllHealth()
	names := make([]string, 0, len(health))
	for name := range health {
		if want != "" && name != want {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	if want != "" && len(names) == 0 {
		return "no driver named " + want
	}
	if len(names) == 0 {
		return "no drivers"
	}
	now := time.Now()
	var b strings.Builder
	for _, name := range names {
		h := health[name]
		status := h.Status.String()
		if h.DeviceFault {
			status = "fault"
		}
		fmt.Fprintf(&b, "%s status=%s errors=%d ticks=%d", name, status, h.ConsecutiveErrors, h.TickCount)
		if h.LastSuccess != nil {
			fmt.Fprintf(&b, " last_success=%s ago", fmtReportAge(now.Sub(*h.LastSuccess)))
		} else {
			b.WriteString(" last_success=never")
		}
		if h.DeviceFault && h.DeviceFaultReason != "" {
			fmt.Fprintf(&b, " fault=%q", assistant.Redact(h.DeviceFaultReason))
		}
		if h.LastError != "" {
			fmt.Fprintf(&b, " last_error=%q", assistant.Redact(h.LastError))
		}
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

func (s *Server) toolRecentLogs(args json.RawMessage) string {
	if s.deps.LogRing == nil {
		return "log ring not configured"
	}
	var in struct {
		Driver string `json:"driver"`
		Limit  int    `json:"limit"`
	}
	_ = json.Unmarshal(args, &in)
	limit := in.Limit
	if limit <= 0 {
		limit = 30
	}
	if limit > 80 {
		limit = 80
	}
	var entries []telemetry.LogEntry
	if strings.TrimSpace(in.Driver) != "" {
		entries = s.deps.LogRing.RecentByDriver(in.Driver, 200)
	} else {
		entries = s.deps.LogRing.RecentGlobal(400)
	}
	kept := make([]telemetry.LogEntry, 0, limit)
	for i := len(entries) - 1; i >= 0 && len(kept) < limit; i-- {
		lvl := strings.ToUpper(entries[i].Level)
		if lvl != "WARN" && lvl != "WARNING" && lvl != "ERROR" {
			continue
		}
		kept = append(kept, entries[i])
	}
	if len(kept) == 0 {
		return "no recent warnings or errors"
	}
	var b strings.Builder
	for i := len(kept) - 1; i >= 0; i-- {
		e := kept[i]
		fmt.Fprintf(&b, "%s %s", e.TS.Format("15:04:05"), e.Level)
		if e.Driver != "" {
			fmt.Fprintf(&b, " [%s]", e.Driver)
		}
		b.WriteByte(' ')
		b.WriteString(assistant.Redact(e.Msg))
		if e.Attrs != "" {
			b.WriteByte(' ')
			b.WriteString(assistant.Redact(e.Attrs))
		}
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

func (s *Server) toolPlanNow() string {
	if s.deps.Ctrl == nil || s.deps.Tel == nil {
		return "site state is not available"
	}
	now := time.Now()
	s.deps.CtrlMu.Lock()
	ctrl := *s.deps.Ctrl
	targets := append([]control.DispatchTarget{}, s.deps.Ctrl.LastTargets...)
	slotEnergy := s.deps.Ctrl.SlotEnergy()
	s.deps.CtrlMu.Unlock()
	snap := s.liveNow(ctrl, now)
	var plan *mpc.Plan
	var replanAt time.Time
	var replanReason string
	var activeSlot *mpc.Action
	if s.deps.MPC != nil {
		plan = s.deps.MPC.Latest()
		replanAt, replanReason = s.deps.MPC.LastReplanInfo()
		activeSlot = activeAction(plan, now)
	}
	var b strings.Builder
	writeRightNow(&b, ctrl, snap, activeSlot, targets, slotEnergy, now)
	b.WriteString("\n")
	writePlanSection(&b, plan, replanAt, replanReason, now)
	writePlanAhead(&b, plan, now)
	return strings.TrimSpace(b.String())
}

func batteryIntent(w float64) string {
	if w > 50 {
		return "charge"
	}
	if w < -50 {
		return "discharge"
	}
	return "idle"
}

// writePlanAhead groups the next 24 hours so Ask why can explain the
// schedule without dumping every 15-minute slot.
func writePlanAhead(b *strings.Builder, plan *mpc.Plan, now time.Time) {
	if plan == nil || len(plan.Actions) == 0 {
		return
	}
	b.WriteString("## Hours ahead\n\n")
	b.WriteString("Grouped battery intent for the next 24 hours. Positive W is charge. Negative W is discharge.\n\n")

	limit := now.Add(24 * time.Hour)
	var (
		started            bool
		dir, reason        string
		start, end         time.Time
		sumW               float64
		n, blocks          int
		minPrice, maxPrice float64
		endSoC             float64
	)
	flush := func() {
		if !started || n == 0 {
			return
		}
		fmt.Fprintf(b, "%s–%s %s %s · %s",
			start.Format("15:04"), end.Format("15:04"),
			dir, fmtReportW(sumW/float64(n)), reason)
		if minPrice == maxPrice {
			fmt.Fprintf(b, " · price %.0f öre", minPrice)
		} else {
			fmt.Fprintf(b, " · price %.0f–%.0f öre", minPrice, maxPrice)
		}
		fmt.Fprintf(b, " · charge ends %.0f%%\n", endSoC)
		blocks++
		started = false
	}
	for i := range plan.Actions {
		a := &plan.Actions[i]
		slotStart := time.UnixMilli(a.SlotStartMs)
		slotLen := time.Duration(a.SlotLenMin) * time.Minute
		if slotLen <= 0 {
			slotLen = 15 * time.Minute
		}
		slotEnd := slotStart.Add(slotLen)
		if slotEnd.Before(now) {
			continue
		}
		if !slotStart.Before(limit) {
			break
		}
		d := batteryIntent(a.BatteryW)
		r := strings.TrimSpace(a.Reason)
		if started && (d != dir || r != reason) {
			flush()
			if blocks >= 16 {
				b.WriteString("… further slots omitted\n")
				break
			}
		}
		if !started {
			dir, reason = d, r
			start = slotStart
			if start.Before(now) {
				start = now
			}
			minPrice, maxPrice = a.PriceOre, a.PriceOre
			started = true
			n = 0
			sumW = 0
		}
		if a.PriceOre < minPrice {
			minPrice = a.PriceOre
		}
		if a.PriceOre > maxPrice {
			maxPrice = a.PriceOre
		}
		sumW += a.BatteryW
		n++
		end = slotEnd
		endSoC = a.SoC * 100
	}
	flush()
	if blocks == 0 {
		b.WriteString("No upcoming slots in the next 24 hours.\n")
	}
	b.WriteByte('\n')
}
