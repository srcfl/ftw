package api

// The "I need help" report.
//
// GET /api/support/report returns ONE self-explanatory Markdown file that
// answers the questions people actually ask in support channels: why is the
// battery discharging, why did it export, why is nothing happening, why is my
// device offline, why did it not update.
//
// Design constraints, learned from a support thread that took six hours and
// nine screenshots to establish that a load forecast said 383 W while the
// house drew 7.9 kW:
//
//   - ONE file. People paste it into a chat and tag the bot. A tarball of
//     twelve JSON files does not survive that journey.
//   - Small enough for a chat upload and a model's context: the plan is
//     trimmed to a few hours either side of now, logs to warnings and errors.
//   - Self-diagnosing. Findings runs the checks a human would run first, so
//     the file is useful before anyone reads the tables under it.
//   - NOW is answered before NEXT. The dashboard's plan card leads with the
//     next action; that ambiguity is what sent the thread down the wrong path.
//   - No secrets. Config is not dumped — only the planner settings that
//     change decisions.
//
// /api/support/dump remains the deep bundle (logs, telemetry, config) for
// cases this report cannot close.

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// reportPlanWindow is how far either side of now the plan table reaches.
// Three hours covers "what just happened" and "what happens next" without
// pushing a 48 h horizon into the file.
const reportPlanWindow = 3 * time.Hour

// reportLogGroups caps the log tail, counted in DISTINCT messages rather
// than lines. Repeats collapse into one entry with a count, so a loop
// warning every tick can no longer crowd out a one-off error.
const reportLogGroups = 40

// logRepeatThreshold is the repeat count at which a warning stops being
// noise in the log and becomes a finding in its own right.
const logRepeatThreshold = 10

// loadmodelWarmBucketsForTrust is the point past which the weekly load
// pattern is filled in enough that its predictions carry the plan. Below
// it the planner is guessing, which is worth saying out loud before
// anyone debugs a decision built on that guess. Roughly two-thirds of the
// 168 hourly buckets.
const loadmodelWarmBucketsForTrust = 112

// Severity labels. Plain words, not symbols — a model reading this file
// should not have to infer meaning from an emoji.
const (
	sevProblem = "PROBLEM"
	sevWarning = "WARNING"
	sevNote    = "NOTE"
)

type finding struct {
	Severity string
	Title    string
	Detail   string
}

func (s *Server) handleSupportReport(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	body := s.buildSupportReport(r.Context(), now)
	stamp := now.UTC().Format("20060102-150405")
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="ftw-help-`+stamp+`.md"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(body))
}

// liveSnapshot is the aggregated site state the report needs. It mirrors
// handleStatus's aggregation but deliberately skips Tel.UpdateLoad — that
// call mutates the shared smoothing filter, and generating a report must
// not perturb control inputs. The unsmoothed value is what we want anyway:
// a report should show the instant, not a filtered version of it.
type liveSnapshot struct {
	GridW       float64
	HaveGrid    bool
	PVW         float64
	BatW        float64
	EVW         float64
	V2XW        float64
	LoadW       float64
	SoCPct      float64
	HaveSoC     bool
	PredictedPV float64
	PredictedLd float64
}

func (s *Server) liveNow(ctrl control.State, now time.Time) liveSnapshot {
	var snap liveSnapshot
	if statusDriverTelemetryUsable(s.deps.Tel, ctrl.SiteMeterDriver) {
		if r := s.deps.Tel.Get(ctrl.SiteMeterDriver, telemetry.DerMeter); r != nil {
			snap.GridW = r.SmoothedW
			snap.HaveGrid = true
		}
	}
	for _, r := range s.deps.Tel.ReadingsByType(telemetry.DerPV) {
		if h := s.deps.Tel.DriverHealth(r.Driver); h == nil || !h.IsOnline() {
			continue
		}
		snap.PVW += r.SmoothedW
	}
	var socSum, socWeight float64
	for _, r := range s.deps.Tel.ReadingsByType(telemetry.DerBattery) {
		if h := s.deps.Tel.DriverHealth(r.Driver); h == nil || !h.IsOnline() {
			continue
		}
		snap.BatW += r.SmoothedW
		if r.SoC != nil {
			socSum += *r.SoC
			socWeight++
		}
	}
	if socWeight > 0 {
		snap.SoCPct = socSum / socWeight * 100
		snap.HaveSoC = true
	}
	snap.EVW = s.deps.Tel.SumOnlineEVW()
	snap.V2XW = s.deps.Tel.SumOnlineV2XW()
	if snap.HaveGrid {
		snap.LoadW = snap.GridW - snap.BatW - snap.PVW - snap.EVW - snap.V2XW
		if snap.LoadW < 0 {
			snap.LoadW = 0
		}
	}
	if s.deps.PVModel != nil {
		snap.PredictedPV = -s.deps.PVModel.PredictNow()
	}
	if s.deps.LoadModel != nil {
		snap.PredictedLd = s.deps.LoadModel.Predict(now)
	}
	return snap
}

func (s *Server) buildSupportReport(ctx context.Context, now time.Time) string {
	s.deps.CtrlMu.Lock()
	ctrl := *s.deps.Ctrl
	targets := append([]control.DispatchTarget{}, s.deps.Ctrl.LastTargets...)
	s.deps.CtrlMu.Unlock()

	snap := s.liveNow(ctrl, now)

	var plan *mpc.Plan
	var activeSlot *mpc.Action
	var lastReplanAt time.Time
	var lastReplanReason string
	if s.deps.MPC != nil {
		plan = s.deps.MPC.Latest()
		lastReplanAt, lastReplanReason = s.deps.MPC.LastReplanInfo()
		activeSlot = activeAction(plan, now)
	}

	health := s.deps.Tel.AllHealth()
	findings := s.collectFindings(ctrl, snap, plan, activeSlot, targets, health, now)

	var b strings.Builder
	writeReportHeader(&b, s.deps.Version, now)
	writeFindings(&b, findings)
	writeRightNow(&b, ctrl, snap, activeSlot, targets, now)
	writePlanSection(&b, plan, lastReplanAt, lastReplanReason, now)
	writeForecastSection(&b, s, snap, activeSlot, now)
	writeDeviceSection(&b, health, now)
	s.writeComponentSection(&b, ctx, plan)
	s.writeLogSection(&b)
	writeReportFooter(&b)
	return b.String()
}

func writeReportHeader(b *strings.Builder, version string, now time.Time) {
	fmt.Fprintf(b, "# FTW help report\n\n")
	fmt.Fprintf(b, "Generated %s · FTW %s\n\n", now.Format("2006-01-02 15:04:05 MST"), version)
	b.WriteString("Sign convention: **positive W = power into the site** " +
		"(importing, charging the battery); **negative W = power out** " +
		"(exporting, discharging). This holds in every table below.\n\n")
	b.WriteString("Read `Findings` first — it lists what looks wrong without " +
		"needing the tables. `Right now` is what the system is doing at this " +
		"instant; `Plan` is what it intends to do next. Those two answer " +
		"different questions and often disagree, which is usually the point " +
		"of the question.\n\n")
}

func writeFindings(b *strings.Builder, findings []finding) {
	b.WriteString("## Findings\n\n")
	if len(findings) == 0 {
		b.WriteString("No automated check fired. The system looks healthy from " +
			"the inside, so the question is probably about intent rather than " +
			"a fault — see `Plan` for the reasoning behind the current slot.\n\n")
		return
	}
	// Problems first: a reader (human or model) should hit the worst thing
	// before the merely notable.
	order := map[string]int{sevProblem: 0, sevWarning: 1, sevNote: 2}
	sort.SliceStable(findings, func(i, j int) bool {
		return order[findings[i].Severity] < order[findings[j].Severity]
	})
	for _, f := range findings {
		fmt.Fprintf(b, "- **%s — %s.** %s\n", f.Severity, f.Title, f.Detail)
	}
	b.WriteString("\n")
}

func writeRightNow(
	b *strings.Builder,
	ctrl control.State,
	snap liveSnapshot,
	activeSlot *mpc.Action,
	targets []control.DispatchTarget,
	now time.Time,
) {
	b.WriteString("## Right now\n\n")
	fmt.Fprintf(b, "Mode **%s**", ctrl.Mode)
	if ctrl.PlanStale {
		b.WriteString(" · plan is **stale**, running safe live balancing")
	}
	b.WriteString("\n\n")

	b.WriteString("| Measure | Value |\n|---|---|\n")
	if snap.HaveGrid {
		fmt.Fprintf(b, "| Grid | %s |\n", fmtReportW(snap.GridW))
	} else {
		b.WriteString("| Grid | **no site meter reading** |\n")
	}
	fmt.Fprintf(b, "| Solar | %s |\n", fmtReportW(snap.PVW))
	fmt.Fprintf(b, "| House load | %s |\n", fmtReportW(snap.LoadW))
	fmt.Fprintf(b, "| Battery | %s |\n", fmtReportW(snap.BatW))
	if snap.EVW != 0 {
		fmt.Fprintf(b, "| EV | %s |\n", fmtReportW(snap.EVW))
	}
	if snap.V2XW != 0 {
		fmt.Fprintf(b, "| V2X | %s |\n", fmtReportW(snap.V2XW))
	}
	if snap.HaveSoC {
		fmt.Fprintf(b, "| Battery charge | %.1f%% |\n", snap.SoCPct)
	}
	fmt.Fprintf(b, "| Grid setpoint | %s |\n", fmtReportW(ctrl.GridTargetW))
	b.WriteString("\n")

	// The plan's intent for THIS slot, stated before anything about the
	// next one. The dashboard leads with "next"; a report that repeats
	// that mistake cannot settle a "why is it doing this right now"
	// argument.
	if activeSlot != nil {
		end := time.UnixMilli(activeSlot.SlotStartMs).
			Add(time.Duration(activeSlot.SlotLenMin) * time.Minute)
		fmt.Fprintf(b, "The plan slot covering this moment (%s–%s) intends "+
			"**battery %s** — %q.\n\n",
			time.UnixMilli(activeSlot.SlotStartMs).Format("15:04"),
			end.Format("15:04"),
			fmtReportW(activeSlot.BatteryW),
			activeSlot.Reason)
		if delta := math.Abs(activeSlot.BatteryW - snap.BatW); delta > 500 {
			fmt.Fprintf(b, "The battery is **%s away from that intent** "+
				"(planned %s, actual %s). A gap this size is either a "+
				"safety clamp, a device that cannot follow the command, or "+
				"a plan replaced since the slot began — check the replan "+
				"reason in `Plan`.\n\n",
				fmtReportW(delta), fmtReportW(activeSlot.BatteryW), fmtReportW(snap.BatW))
		}
	} else {
		b.WriteString("No plan slot covers this moment.\n\n")
	}

	if len(targets) > 0 {
		b.WriteString("Commands sent to each device on the last control tick:\n\n")
		b.WriteString("| Device | Target | Held below request |\n|---|---|---|\n")
		for _, t := range targets {
			clamped := "no"
			if t.Clamped {
				clamped = "**yes**"
			}
			fmt.Fprintf(b, "| %s | %s | %s |\n", t.Driver, fmtReportW(t.TargetW), clamped)
		}
		// This column covers per-device limits only — state of charge,
		// rated power, the fuse guard. The site-meter clamp that caps the
		// fleet total before this split happens upstream of these targets
		// and cannot show up here, so "no" on every row is not the same as
		// "nothing was limited". Say so rather than let the table imply it.
		b.WriteString("\nThis column covers per-device limits: charge level, " +
			"rated power, fuse guard. A limit applied to the site total " +
			"before it was split across devices does not appear here — check " +
			"the log for clamp messages.\n\n")
	}

	st := ctrl.SlotDeliveryStats
	if st.OverDeliveryCount+st.UnderDeliveryCount+st.SignMismatchCount > 0 {
		fmt.Fprintf(b, "Slot delivery misses since start: %d over, %d under, "+
			"%d in the wrong direction.\n\n",
			st.OverDeliveryCount, st.UnderDeliveryCount, st.SignMismatchCount)
	}
}

func writePlanSection(
	b *strings.Builder,
	plan *mpc.Plan,
	replanAt time.Time,
	replanReason string,
	now time.Time,
) {
	b.WriteString("## Plan\n\n")
	if plan == nil {
		b.WriteString("No plan exists. The planner is off, or it has not " +
			"produced one yet.\n\n")
		return
	}
	fmt.Fprintf(b, "Strategy **%s** · %d slots · built %s",
		plan.Mode, len(plan.Actions),
		time.UnixMilli(plan.GeneratedAtMs).Format("15:04:05"))
	if !replanAt.IsZero() {
		fmt.Fprintf(b, " · last replan %s ago", fmtReportAge(now.Sub(replanAt)))
		if replanReason != "" {
			fmt.Fprintf(b, " (%s)", replanReason)
		}
	}
	b.WriteString("\n\n")

	if sv := plan.Solver; sv != nil {
		engine := sv.Engine
		if sv.Backend != "" {
			engine += " / " + sv.Backend
		}
		fmt.Fprintf(b, "Solver **%s** · %s", engine, sv.Status)
		if sv.SolveMs > 0 {
			fmt.Fprintf(b, " · %.0f ms", sv.SolveMs)
		}
		b.WriteString("\n\n")
		if sv.Fallback {
			fmt.Fprintf(b, "This plan came from the **built-in Go fallback**, "+
				"not the mathematical optimizer: %s\n\n", sv.FallbackReason)
		}
	} else {
		b.WriteString("No solver information on this plan.\n\n")
	}

	fmt.Fprintf(b, "Slots within %s of now. `→` marks the slot covering this "+
		"moment. Solar is negative because generation flows out of the site.\n\n",
		fmtReportAge(reportPlanWindow))
	b.WriteString("| | Time | Price | Spot | Solar | Load | Battery | Grid | Charge end | Reason |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|\n")
	from := now.Add(-reportPlanWindow).UnixMilli()
	to := now.Add(reportPlanWindow).UnixMilli()
	shown := 0
	for i := range plan.Actions {
		a := &plan.Actions[i]
		if a.SlotStartMs < from || a.SlotStartMs > to {
			continue
		}
		marker := ""
		if isActiveAction(a, now) {
			marker = "→"
		}
		fmt.Fprintf(b, "| %s | %s | %.1f | %.1f | %s | %s | %s | %s | %.1f%% | %s |\n",
			marker,
			time.UnixMilli(a.SlotStartMs).Format("15:04"),
			a.PriceOre, a.SpotOre,
			fmtReportW(a.PVW), fmtReportW(a.LoadW),
			fmtReportW(a.BatteryW), fmtReportW(a.GridW),
			a.SoCPct, a.Reason)
		shown++
	}
	if shown == 0 {
		b.WriteString("| | _no slots in this window_ | | | | | | | | |\n")
	}
	b.WriteString("\n")
}

func writeForecastSection(b *strings.Builder, s *Server, snap liveSnapshot, activeSlot *mpc.Action, now time.Time) {
	b.WriteString("## Forecast quality\n\n")
	b.WriteString("The planner decides against forecasts, not live readings. " +
		"When a forecast is wrong the plan is wrong, however well the " +
		"optimizer solves it.\n\n")

	b.WriteString("| Quantity | Forecast | Actual now |\n|---|---|---|\n")
	fmt.Fprintf(b, "| House load | %s | %s |\n",
		fmtReportW(snap.PredictedLd), fmtReportW(snap.LoadW))
	fmt.Fprintf(b, "| Solar | %s | %s |\n",
		fmtReportW(snap.PredictedPV), fmtReportW(snap.PVW))
	if activeSlot != nil {
		fmt.Fprintf(b, "| Load the active slot assumed | %s | %s |\n",
			fmtReportW(activeSlot.LoadW), fmtReportW(snap.LoadW))
		fmt.Fprintf(b, "| Solar the active slot assumed | %s | %s |\n",
			fmtReportW(activeSlot.PVW), fmtReportW(snap.PVW))
	}
	b.WriteString("\n")

	if s.deps.LoadModel != nil {
		m := s.deps.LoadModel.Model()
		stats := loadModelStatsFrom(m)
		fmt.Fprintf(b, "Load model: %d samples · average error %s · quality %.2f · "+
			"%d of %d weekly buckets trained · heating %.0f W/°C · profile %s\n\n",
			stats.Samples, fmtReportW(stats.MAEW), stats.Quality,
			stats.BucketsWarm, stats.BucketsTotal, stats.HeatingWPerDegC,
			s.deps.LoadModel.Profile())
		b.WriteString("The load model learns from `grid − solar − battery − EV`. " +
			"It discards any sample where that arithmetic comes out negative, " +
			"so on a site with large solar the surviving daytime samples skew " +
			"low. A model that reports a small average error while the table " +
			"above shows a large gap has learned the wrong house.\n\n")
	} else {
		b.WriteString("Load model is not running: the planner is using a flat " +
			"base load for every slot.\n\n")
	}
}

func writeDeviceSection(b *strings.Builder, health map[string]telemetry.DriverHealth, now time.Time) {
	b.WriteString("## Devices\n\n")
	if len(health) == 0 {
		b.WriteString("No drivers are registered.\n\n")
		return
	}
	names := make([]string, 0, len(health))
	for n := range health {
		names = append(names, n)
	}
	sort.Strings(names)

	b.WriteString("| Device | State | Last reading | Errors in a row | Last error |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, n := range names {
		h := health[n]
		state := "ok"
		if !h.IsOnline() {
			state = "**offline**"
		}
		if h.DeviceFault {
			state = "**device fault**"
			if h.DeviceFaultReason != "" {
				state += " (" + h.DeviceFaultReason + ")"
			}
		}
		last := "never"
		if h.LastSuccess != nil {
			last = fmtReportAge(now.Sub(*h.LastSuccess)) + " ago"
		}
		lastErr := h.LastError
		if lastErr == "" {
			lastErr = "—"
		}
		fmt.Fprintf(b, "| %s | %s | %s | %d | %s |\n",
			n, state, last, h.ConsecutiveErrors, truncateReport(lastErr, 90))
	}
	b.WriteString("\n")
}

func (s *Server) writeComponentSection(b *strings.Builder, ctx context.Context, plan *mpc.Plan) {
	b.WriteString("## Versions\n\n")
	fmt.Fprintf(b, "- Core **%s**\n", s.deps.Version)
	if s.deps.MPC == nil || s.deps.MPC.Optimizer == nil {
		b.WriteString("- Optimizer **not configured** — every plan comes from " +
			"the built-in Go fallback\n")
	} else if h, ok := s.deps.MPC.Optimizer.(optimizerHealth); ok {
		hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		info, err := h.Health(hctx)
		cancel()
		if err != nil {
			fmt.Fprintf(b, "- Optimizer **unreachable**: %s\n", err.Error())
		} else {
			fmt.Fprintf(b, "- Optimizer **%s** (protocol %d)\n",
				info.Version, info.ProtocolVersion)
		}
	}
	if plan != nil && plan.Solver != nil && plan.Solver.Fallback {
		b.WriteString("- The active plan is running on the fallback, whatever " +
			"the optimizer reports above\n")
	}
	b.WriteString("\n")
}

// logGroup collapses repeats of the same message. A control loop that
// warns every tick produced 49 identical lines in the first real report,
// which would have pushed a single unique ERROR clean out of the window.
// Grouping on (level, driver, message) — not on the attribute tail, which
// carries per-tick numbers — keeps one line per distinct problem.
type logGroup struct {
	Level  string
	Driver string
	Msg    string
	Attrs  string // from the most recent occurrence
	Count  int
	First  time.Time
	Last   time.Time
}

// groupLogs returns distinct warnings and errors, most-recent last.
func groupLogs(entries []telemetry.LogEntry) []logGroup {
	type key struct{ level, driver, msg string }
	index := map[key]int{}
	var groups []logGroup
	for _, e := range entries {
		lvl := strings.ToUpper(e.Level)
		if lvl != "WARN" && lvl != "WARNING" && lvl != "ERROR" {
			continue
		}
		k := key{lvl, e.Driver, e.Msg}
		if i, ok := index[k]; ok {
			groups[i].Count++
			groups[i].Attrs = e.Attrs
			if e.TS.After(groups[i].Last) {
				groups[i].Last = e.TS
			}
			continue
		}
		index[k] = len(groups)
		groups = append(groups, logGroup{
			Level: lvl, Driver: e.Driver, Msg: e.Msg, Attrs: e.Attrs,
			Count: 1, First: e.TS, Last: e.TS,
		})
	}
	sort.SliceStable(groups, func(i, j int) bool {
		return groups[i].Last.Before(groups[j].Last)
	})
	return groups
}

func (s *Server) writeLogSection(b *strings.Builder) {
	b.WriteString("## Recent warnings and errors\n\n")
	if s.deps.LogRing == nil {
		b.WriteString("Log buffer is not configured.\n\n")
		return
	}
	groups := groupLogs(s.deps.LogRing.RecentGlobal(0))
	if len(groups) == 0 {
		b.WriteString("None in the buffer.\n\n")
		return
	}
	b.WriteString("Repeats are collapsed; `×N` is how many times the same " +
		"message appeared. The attribute tail comes from the most recent one.\n\n")
	if len(groups) > reportLogGroups {
		fmt.Fprintf(b, "Showing the %d most recent of %d distinct messages.\n\n",
			reportLogGroups, len(groups))
		groups = groups[len(groups)-reportLogGroups:]
	}
	b.WriteString("```\n")
	for _, g := range groups {
		fmt.Fprintf(b, "%s %s ", g.Last.Format("15:04:05"), g.Level)
		if g.Driver != "" {
			fmt.Fprintf(b, "[%s] ", g.Driver)
		}
		b.WriteString(g.Msg)
		if g.Count > 1 {
			fmt.Fprintf(b, " ×%d", g.Count)
			if span := g.Last.Sub(g.First); span > time.Second {
				fmt.Fprintf(b, " over %s", fmtReportAge(span))
			}
		}
		if g.Attrs != "" {
			b.WriteByte(' ')
			b.WriteString(truncateReport(g.Attrs, 200))
		}
		b.WriteByte('\n')
	}
	b.WriteString("```\n\n")
}

func writeReportFooter(b *strings.Builder) {
	b.WriteString("---\n\n")
	b.WriteString("Deeper data lives in `/api/support/dump` (logs, telemetry, " +
		"redacted config) and `/api/mpc/diagnose` (every slot the optimizer " +
		"saw). This report deliberately omits both to stay readable.\n")
}

// ---- findings ----

func (s *Server) collectFindings(
	ctrl control.State,
	snap liveSnapshot,
	plan *mpc.Plan,
	activeSlot *mpc.Action,
	targets []control.DispatchTarget,
	health map[string]telemetry.DriverHealth,
	now time.Time,
) []finding {
	var out []finding

	if !snap.HaveGrid {
		out = append(out, finding{sevProblem, "No site meter reading",
			"Dispatch stops without a live site meter. Everything below is " +
				"guesswork until the meter reports again."})
	}

	if ctrl.PlanStale {
		out = append(out, finding{sevProblem, "The plan is stale",
			"The planner has not produced a usable plan, so control fell back " +
				"to safe live balancing. The plan shown in the app may no " +
				"longer be the one being followed."})
	}

	if plan == nil && strings.HasPrefix(string(ctrl.Mode), "planner_") {
		out = append(out, finding{sevProblem, "Planner mode with no plan",
			"A planner strategy is selected but no plan exists."})
	}

	if plan != nil && plan.Solver != nil && plan.Solver.Fallback {
		out = append(out, finding{sevProblem, "Running on the built-in fallback",
			"The mathematical optimizer did not produce this plan: " +
				plan.Solver.FallbackReason})
	}

	// The load-forecast check. This is the one that would have closed the
	// 383 W thread in a single message.
	if forecastMiss(snap.PredictedLd, snap.LoadW) {
		out = append(out, finding{sevProblem, "The load forecast is far from reality",
			fmt.Sprintf("The model predicts %s for right now; the house is "+
				"drawing %s. The planner sized this slot against the forecast, "+
				"so its charge and discharge decisions are built on the wrong "+
				"house. Resetting the load model (Settings, or POST "+
				"/api/loadmodel/reset) makes it relearn.",
				fmtReportW(snap.PredictedLd), fmtReportW(snap.LoadW))})
	} else if activeSlot != nil && forecastMiss(activeSlot.LoadW, snap.LoadW) {
		out = append(out, finding{sevProblem, "The active slot assumed a different house",
			fmt.Sprintf("This slot was planned for a load of %s; actual load is "+
				"%s. Expect the live setpoint to diverge from the plan, and "+
				"expect frequent replans.",
				fmtReportW(activeSlot.LoadW), fmtReportW(snap.LoadW))})
	}

	if activeSlot != nil && forecastMiss(math.Abs(activeSlot.PVW), math.Abs(snap.PVW)) {
		out = append(out, finding{sevWarning, "The solar forecast is far from reality",
			fmt.Sprintf("This slot was planned for %s of solar; actual is %s.",
				fmtReportW(activeSlot.PVW), fmtReportW(snap.PVW))})
	}

	if activeSlot != nil {
		if delta := math.Abs(activeSlot.BatteryW - snap.BatW); delta > 1000 {
			out = append(out, finding{sevWarning, "The battery is not following the plan",
				fmt.Sprintf("The slot intends %s, the battery is doing %s. "+
					"Either safety clamped it, the device cannot deliver, or "+
					"a replan changed the plan after this slot started.",
					fmtReportW(activeSlot.BatteryW), fmtReportW(snap.BatW))})
		}
	}

	var clamped []string
	for _, t := range targets {
		if t.Clamped {
			clamped = append(clamped, t.Driver)
		}
	}
	if len(clamped) > 0 {
		out = append(out, finding{sevWarning, "Safety limits are active",
			fmt.Sprintf("These devices are being held below the requested "+
				"power: %s. Safety limits always win over the plan.",
				strings.Join(clamped, ", "))})
	}

	// A message repeating every control tick is the loudest signal in the
	// log and the easiest to scroll past. The first real report carried 49
	// copies of one clamp warning while Findings said nothing at all.
	// Reported generically, by count, so this does not rot the moment
	// someone rewords a log line.
	if s.deps.LogRing != nil {
		var worst logGroup
		for _, g := range groupLogs(s.deps.LogRing.RecentGlobal(0)) {
			if g.Count > worst.Count {
				worst = g
			}
		}
		if worst.Count >= logRepeatThreshold {
			sev := sevWarning
			if worst.Level == "ERROR" {
				sev = sevProblem
			}
			detail := fmt.Sprintf("%q has been logged %d times",
				worst.Msg, worst.Count)
			if span := worst.Last.Sub(worst.First); span > time.Second {
				detail += " in " + fmtReportAge(span)
			}
			detail += ". Something is retrying or being limited on every " +
				"control cycle; the full line is in the log section below."
			out = append(out, finding{sev, "A message is repeating", detail})
		}
	}

	var offline, faulted []string
	for name, h := range health {
		if h.DeviceFault {
			faulted = append(faulted, name)
			continue
		}
		if !h.IsOnline() {
			offline = append(offline, name)
		}
	}
	sort.Strings(offline)
	sort.Strings(faulted)
	if len(faulted) > 0 {
		out = append(out, finding{sevProblem, "A device reports a fault",
			fmt.Sprintf("%s can be reached but cannot act. The planner excludes "+
				"it, and any power it was expected to move becomes grid flow "+
				"instead.", strings.Join(faulted, ", "))})
	}
	if len(offline) > 0 {
		out = append(out, finding{sevProblem, "Devices are offline",
			fmt.Sprintf("%s. An offline device gets its safe default mode and "+
				"is left out of the plan.", strings.Join(offline, ", "))})
	}

	// A fresh install makes odd-looking decisions for a legitimate reason,
	// and "quality 0.00" in the table below does not say so out loud.
	if s.deps.LoadModel != nil {
		stats := loadModelStatsFrom(s.deps.LoadModel.Model())
		if stats.BucketsWarm < loadmodelWarmBucketsForTrust {
			out = append(out, finding{sevNote, "The load model is still learning",
				fmt.Sprintf("Only %d of %d hourly patterns have enough samples "+
					"to trust. Until the week fills in, the planner is working "+
					"from a rough guess at this household's demand.",
					stats.BucketsWarm, stats.BucketsTotal)})
		}
	}

	if snap.HaveSoC {
		if snap.SoCPct <= 12 {
			out = append(out, finding{sevNote, "The battery is near empty",
				fmt.Sprintf("Charge is %.0f%%. Near the floor the battery stops "+
					"discharging whatever the plan says.", snap.SoCPct)})
		}
		if snap.SoCPct >= 97 {
			out = append(out, finding{sevNote, "The battery is full",
				fmt.Sprintf("Charge is %.0f%%. Surplus solar exports because "+
					"there is nowhere to put it.", snap.SoCPct)})
		}
	}

	if !strings.HasPrefix(string(ctrl.Mode), "planner_") {
		out = append(out, finding{sevNote, "No planning strategy is running",
			fmt.Sprintf("Mode is %s. Price-aware scheduling only happens in a "+
				"planner strategy; in this mode the battery follows a simple "+
				"live rule.", ctrl.Mode)})
	}

	return out
}

// forecastMiss reports whether a forecast is wrong enough to change decisions.
// The 500 W floor keeps small absolute errors on a quiet house from firing;
// the 60 % band is wide enough that ordinary forecast noise stays quiet.
func forecastMiss(predicted, actual float64) bool {
	base := math.Max(math.Abs(actual), 500)
	return math.Abs(predicted-actual)/base > 0.6
}

// ---- helpers ----

func activeAction(plan *mpc.Plan, now time.Time) *mpc.Action {
	if plan == nil {
		return nil
	}
	for i := range plan.Actions {
		if isActiveAction(&plan.Actions[i], now) {
			return &plan.Actions[i]
		}
	}
	return nil
}

func isActiveAction(a *mpc.Action, now time.Time) bool {
	start := a.SlotStartMs
	end := start + int64(a.SlotLenMin)*60_000
	ms := now.UnixMilli()
	return ms >= start && ms < end
}

func fmtReportW(v float64) string {
	if math.Abs(v) >= 1000 {
		return fmt.Sprintf("%.2f kW", v/1000)
	}
	return fmt.Sprintf("%.0f W", v)
}

func fmtReportAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%.0f s", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%.0f min", d.Minutes())
	default:
		return fmt.Sprintf("%.1f h", d.Hours())
	}
}

func truncateReport(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
