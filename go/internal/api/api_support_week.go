package api

// The support file's week.
//
// A tester asked why the battery emptied overnight, and the support file
// could not say: its log covered 75 minutes and its telemetry one hour. The
// box keeps far more than that. This reads seven days back from its own
// storage (series, control journal, plan snapshots, notifications, start
// and stop events, warning counts), sums them per day and night for the
// help report, and ships them in lower resolution in the archive's week/.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

const (
	supportWeekSpan = 7 * 24 * time.Hour
	// supportWeekStep is the series resolution. Five-minute buckets keep
	// their min/max envelope, so a short spike survives, and a site with a
	// hundred series still makes a file that can be mailed.
	supportWeekStep = 5 * time.Minute
	// supportWeekBudget bounds the reads. The archive must finish well inside
	// the HTTP write timeout on a Raspberry Pi; what is left out is named.
	supportWeekBudget = 60 * time.Second
	// Day and night split at these local hours, so "last night" is one row.
	supportDayStartHour   = 6
	supportNightStartHour = 22

	supportWeekSpanLimit   = 20000
	supportWeekReplanLimit = 10000
	supportWeekNoticeLimit = 2000
	supportWeekDigestLimit = 5000
	supportWeekEventLimit  = 1000

	// supportPlanGapWh is the battery energy over a period, beyond what the
	// plan's own slots asked for, worth a warning. One kilowatt-hour is a
	// tenth of a typical home battery, past rounding and slot-edge effects.
	supportPlanGapWh = 1000
	// supportNightNoteWh is the night discharge worth listing even when it
	// followed the plan, because it is what people ask about.
	supportNightNoteWh = 500
	// supportSourceDownMin is the offline time a device needs before the
	// week calls it out. Shorter stretches are polls that came back.
	supportSourceDownMin = 5 * time.Minute
	// supportRepeatWarnings is how often one warning has to recur in the
	// week before it is a finding of its own.
	supportRepeatWarnings = 100
	// supportBusyReplans: a period with this many replans and three times
	// the median is unusual enough to name.
	supportBusyReplans = 30
)

// supportPeriod is one day (06–22) or night (22–06) in household time.
type supportPeriod struct {
	Start, End time.Time
	Night      bool
	// Partial marks the period that is still running.
	Partial bool
}

// Label names the period the way a person asks about it.
func (p supportPeriod) Label() string {
	if !p.Night {
		return p.Start.Format("Mon 2 Jan") + ", day"
	}
	next := p.Start.AddDate(0, 0, 1)
	from := p.Start.Format("Mon 2")
	if next.Month() != p.Start.Month() {
		from = p.Start.Format("Mon 2 Jan")
	}
	return from + " → " + next.Format("Mon 2 Jan") + ", night"
}

// supportWeekPeriods splits the last seven days at 06:00 and 22:00 in now's
// zone, from the first boundary inside the window to now. Clock changes
// make a night eight or ten hours long rather than moving the boundary.
func supportWeekPeriods(now time.Time) []supportPeriod {
	loc := now.Location()
	since := now.Add(-supportWeekSpan)
	var bounds []time.Time
	y, m, d := since.Date()
	for day := time.Date(y, m, d, 0, 0, 0, 0, loc); !day.After(now); day = day.AddDate(0, 0, 1) {
		for _, h := range []int{supportDayStartHour, supportNightStartHour} {
			b := time.Date(day.Year(), day.Month(), day.Day(), h, 0, 0, 0, loc)
			if !b.Before(since) && b.Before(now) {
				bounds = append(bounds, b)
			}
		}
	}
	out := make([]supportPeriod, 0, len(bounds))
	for i, b := range bounds {
		p := supportPeriod{Start: b, End: now, Night: b.Hour() == supportNightStartHour, Partial: true}
		if i+1 < len(bounds) {
			p.End, p.Partial = bounds[i+1], false
		}
		out = append(out, p)
	}
	return out
}

type supportSeries struct {
	Driver, Metric, Unit string
	Points               []state.SeriesPoint
}

type supportCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// supportPeriodSummary is one row of the week. Nil means not measured.
type supportPeriodSummary struct {
	period          supportPeriod
	Label           string   `json:"label"`
	StartMs         int64    `json:"start_ms"`
	EndMs           int64    `json:"end_ms"`
	Night           bool     `json:"night"`
	Partial         bool     `json:"partial,omitempty"`
	SoCMinPct       *float64 `json:"soc_min_pct"`
	SoCMaxPct       *float64 `json:"soc_max_pct"`
	BatChargedWh    *float64 `json:"battery_charged_wh"`
	BatDischargedWh *float64 `json:"battery_discharged_wh"`
	ImportWh        *float64 `json:"import_wh"`
	ExportWh        *float64 `json:"export_wh"`
	// EVWh integrates the chargers' five-minute series, so it is close,
	// not metered.
	EVWh *float64 `json:"ev_wh_approx"`
	// The plan's battery energy over the slots Core scored in the period,
	// against what the batteries did in those slots. Out is discharge.
	SlotsScored      int            `json:"slots_scored"`
	SlotsOffPlan     int            `json:"slots_off_plan"`
	PlannedInWh      float64        `json:"planned_in_wh"`
	PlannedOutWh     float64        `json:"planned_out_wh"`
	ActualInWh       float64        `json:"actual_in_wh"`
	ActualOutWh      float64        `json:"actual_out_wh"`
	SlotGapWh        float64        `json:"slot_gap_wh"`
	Replans          int            `json:"replans"`
	ReplanReasons    []supportCount `json:"replan_reasons,omitempty"`
	Fallbacks        int            `json:"fallbacks"`
	FuseLimitedS     float64        `json:"fuse_limited_s"`
	SourceDown       []supportCount `json:"sources_down_s,omitempty"` // seconds per driver
	Warnings         int64          `json:"warnings"`
	Errors           int64          `json:"errors"`
	Starts           int            `json:"core_starts"`
	UncleanStarts    int            `json:"core_starts_without_clean_stop"`
	EnergyIncomplete bool           `json:"energy_incomplete,omitempty"`
}

// supportWeek is everything the week reads, loaded once per request.
type supportWeek struct {
	Since, Until time.Time
	Series       []supportSeries
	Spans        []state.ControlSpan
	SpansMore    bool
	Replans      []state.PlannerReplan
	Notices      []state.NotificationEntry
	NoticesMore  bool
	Events       []state.Event
	// EventBefore is the last start or stop event before the window, so
	// the first start inside it can be judged. Nil on a new box.
	EventBefore *state.Event
	// FirstData is the earliest stored series point in the window. Zero
	// when there is none.
	FirstData  time.Time
	Digest     []state.LogDigest
	DigestMore bool
	Periods    []supportPeriodSummary
	// Gaps names what could not be read, so a missing row is not mistaken
	// for a quiet week.
	Gaps []string
}

func supportWeekPoints() int { return int(supportWeekSpan / supportWeekStep) }

// supportSummaryMetric is a series the period table reads. The help report
// alone loads only these; the archive loads every series.
func supportSummaryMetric(metric string) bool {
	return metric == telemetry.DerBattery.String()+"_soc" || metric == telemetry.DerEV.String()+"_w"
}

// loadSupportWeek reads the last seven days from storage. allSeries loads
// every (driver, metric) series for the archive. It returns nil without a
// store.
func (s *Server) loadSupportWeek(ctx context.Context, now time.Time, allSeries bool) *supportWeek {
	if s.deps.State == nil {
		return nil
	}
	now = now.In(time.Local)
	ctx, cancel := context.WithTimeout(ctx, supportWeekBudget)
	defer cancel()
	st := s.deps.State
	w := &supportWeek{Since: now.Add(-supportWeekSpan), Until: now}
	sinceMs, untilMs := w.Since.UnixMilli(), w.Until.UnixMilli()
	gap := func(what string, err error) {
		if err != nil {
			w.Gaps = append(w.Gaps, what+": "+err.Error())
		}
	}

	var err error
	w.Spans, w.SpansMore, err = st.ControlSpans(ctx, state.ControlSpanQuery{SinceMs: sinceMs, UntilMs: untilMs, Limit: supportWeekSpanLimit})
	gap("control journal", err)
	w.Replans, err = st.PlannerReplans(ctx, sinceMs, untilMs, supportWeekReplanLimit)
	gap("plan snapshots", err)
	w.Notices, w.NoticesMore, err = st.NotificationsBetween(ctx, sinceMs, untilMs, "", supportWeekNoticeLimit)
	gap("notifications", err)
	w.Digest, w.DigestMore, err = st.LogDigestBetween(ctx, sinceMs, untilMs, supportWeekDigestLimit)
	gap("warning counts", err)
	events, err := st.RecentEvents(supportWeekEventLimit)
	gap("start and stop events", err)
	for i := len(events) - 1; i >= 0; i-- { // newest first from the store
		e := events[i]
		switch {
		case e.TsMs >= sinceMs && e.TsMs <= untilMs:
			w.Events = append(w.Events, e)
		case e.TsMs < sinceMs && isStartStop(e.Event):
			w.EventBefore = &e
		}
	}

	units := s.metricUnits()
	drivers, err := st.DriverNames()
	gap("series drivers", err)
	metrics, err := st.MetricNames()
	gap("series metrics", err)
	sort.Strings(drivers)
	sort.Strings(metrics)
	skipped := 0
	for _, d := range drivers {
		for _, m := range metrics {
			if !allSeries && !supportSummaryMetric(m) {
				continue
			}
			if ctx.Err() != nil {
				skipped++
				continue
			}
			pts, err := st.LoadSeriesBucketsOrRawContext(ctx, d, m, sinceMs, untilMs, supportWeekPoints())
			if err != nil {
				if ctx.Err() == nil {
					gap("series "+d+"/"+m, err)
				} else {
					skipped++
				}
				continue
			}
			if len(pts) > 0 {
				w.Series = append(w.Series, supportSeries{Driver: d, Metric: m, Unit: units[m], Points: pts})
				firstMs := pts[0].TsMs // a bucket's newest sample
				if pts[0].FirstMS > 0 {
					firstMs = pts[0].FirstMS
				}
				if first := time.UnixMilli(firstMs); w.FirstData.IsZero() || first.Before(w.FirstData) {
					w.FirstData = first
				}
			}
		}
	}
	if skipped > 0 {
		w.Gaps = append(w.Gaps, fmt.Sprintf("series: %d left out when the %s read budget ran out", skipped, supportWeekBudget))
	}

	for _, p := range supportWeekPeriods(now) {
		w.Periods = append(w.Periods, s.summarisePeriod(ctx, w, p))
	}
	if ctx.Err() != nil {
		w.Gaps = append(w.Gaps, "energy per period: the read budget ran out")
	}
	return w
}

func (s *Server) summarisePeriod(ctx context.Context, w *supportWeek, p supportPeriod) supportPeriodSummary {
	startMs, endMs := p.Start.UnixMilli(), p.End.UnixMilli()
	out := supportPeriodSummary{period: p, Label: p.Label(), StartMs: startMs, EndMs: endMs, Night: p.Night, Partial: p.Partial}
	in := func(ts int64) bool { return ts >= startMs && ts < endMs }

	if ctx.Err() == nil {
		if de, err := s.deps.State.DailyEnergy(startMs, endMs); err == nil {
			if de.Intervals == 0 {
				de.Missing |= state.MissingPower
			}
			out.BatChargedWh = measured(de.Missing, state.MissingBattery, de.BatChargedWh)
			out.BatDischargedWh = measured(de.Missing, state.MissingBattery, de.BatDischargedWh)
			out.ImportWh = measured(de.Missing, state.MissingGrid, de.ImportWh)
			out.ExportWh = measured(de.Missing, state.MissingGrid, de.ExportWh)
			out.EnergyIncomplete = de.Missing&(state.MissingBattery|state.MissingGrid) != 0
		}
	}

	widthH := float64(state.BucketWidthMs(w.Since.UnixMilli(), w.Until.UnixMilli(), supportWeekPoints())) / 3_600_000
	for _, ser := range w.Series {
		switch {
		case ser.Metric == telemetry.DerBattery.String()+"_soc":
			for _, pt := range ser.Points {
				if !in(pt.TsMs) {
					continue
				}
				out.SoCMinPct = minPtr(out.SoCMinPct, pt.Min*100)
				out.SoCMaxPct = maxPtr(out.SoCMaxPct, pt.Max*100)
			}
		case ser.Metric == telemetry.DerEV.String()+"_w":
			for _, pt := range ser.Points {
				if in(pt.TsMs) {
					v := math.Max(pt.V, 0) * widthH
					if out.EVWh == nil {
						out.EVWh = new(float64)
					}
					*out.EVWh += v
				}
			}
		}
	}

	down := map[string]float64{}
	for _, sp := range w.Spans {
		end := sp.EndMs
		if end == 0 {
			end = w.Until.UnixMilli()
		}
		if sp.Kind == "slot" {
			if end <= startMs || end > endMs {
				continue
			}
			var e struct {
				PlannedWh float64 `json:"planned_wh"`
				ActualWh  float64 `json:"actual_wh"`
			}
			if json.Unmarshal(sp.First, &e) != nil {
				continue
			}
			out.SlotsScored++
			if sp.Status != "on_target" {
				out.SlotsOffPlan++
			}
			out.PlannedInWh += math.Max(e.PlannedWh, 0)
			out.PlannedOutWh += math.Max(-e.PlannedWh, 0)
			out.ActualInWh += math.Max(e.ActualWh, 0)
			out.ActualOutWh += math.Max(-e.ActualWh, 0)
			out.SlotGapWh += math.Abs(e.PlannedWh - e.ActualWh)
			continue
		}
		overlap := float64(min(end, endMs)-max(sp.StartMs, startMs)) / 1000
		if overlap <= 0 {
			continue
		}
		if sp.Reason == "fuse_limit" || sp.Reason == "fuse_cooldown" {
			out.FuseLimitedS += overlap
		}
		if sp.Kind == "source" && (sp.Status == "offline" || sp.Status == "fault") {
			down[sp.Driver+" "+sp.Status] += overlap
		}
	}
	out.SourceDown = sortedCounts(down)

	reasons := map[string]float64{}
	for _, r := range w.Replans {
		if !in(r.TsMs) {
			continue
		}
		out.Replans++
		reasons[r.Reason]++
		if r.Fallback {
			out.Fallbacks++
		}
	}
	out.ReplanReasons = sortedCounts(reasons)

	for _, g := range w.Digest {
		if !in(g.FirstMs) {
			continue
		}
		if g.Level == "ERROR" {
			out.Errors += g.Count
		} else {
			out.Warnings += g.Count
		}
	}

	prev := w.EventBefore
	for i := range w.Events {
		e := w.Events[i]
		if !isStartStop(e.Event) {
			continue
		}
		if e.Event == "startup" && in(e.TsMs) {
			out.Starts++
			// A start right after another start had no stop between them.
			// The very first start of a new box has nothing before it.
			if prev != nil && prev.Event == "startup" {
				out.UncleanStarts++
			}
		}
		prev = &w.Events[i]
	}
	return out
}

func isStartStop(event string) bool {
	return event == "startup" || event == "shutdown" || event == "restart"
}

func measured(m state.HistoryMissing, flag state.HistoryMissing, v float64) *float64 {
	if m&flag != 0 {
		return nil
	}
	return &v
}

func minPtr(p *float64, v float64) *float64 {
	if p == nil || v < *p {
		return &v
	}
	return p
}

func maxPtr(p *float64, v float64) *float64 {
	if p == nil || v > *p {
		return &v
	}
	return p
}

func sortedCounts(m map[string]float64) []supportCount {
	out := make([]supportCount, 0, len(m))
	for k, v := range m {
		out = append(out, supportCount{Name: k, Count: int(math.Round(v))})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// ---- the report section ----

func writeWeekSection(b *strings.Builder, w *supportWeek) {
	b.WriteString("## What happened over the last week\n\n")
	if w == nil {
		b.WriteString("The box has no store to read the week from.\n\n")
		return
	}
	fmt.Fprintf(b, "From %s to %s, household time, split into days (06–22) and nights (22–06). "+
		"Built from the box's stored history, control journal, plan snapshots, notifications and "+
		"warning counts, not from the log buffer, which holds about the last hour.\n\n",
		w.Since.Format("Mon 2 Jan 15:04"), w.Until.Format("Mon 2 Jan 15:04"))

	findings := weekFindings(w)
	if len(findings) == 0 {
		b.WriteString("Nothing in the stored week stands out.\n\n")
	} else {
		writeFindingList(b, findings)
	}

	b.WriteString("| Period | Charge | Battery in / out | EV | Grid in / out | Plan vs battery | Replans | Limits and faults |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|\n")
	periods := w.Periods
	if empty := periodsBeforeData(w); empty > 0 {
		fmt.Fprintf(b, "| %s to %s | no stored data | | | | | | |\n",
			periods[0].period.Start.Format("Mon 2 Jan 15:04"), periods[empty-1].period.End.Format("Mon 2 Jan 15:04"))
		periods = periods[empty:]
	}
	for _, p := range periods {
		label := p.Label
		if p.Partial {
			label += " (so far)"
		}
		fmt.Fprintf(b, "| %s | %s | %s / %s | %s | %s / %s | %s | %s | %s |\n",
			label, fmtSoCRange(p.SoCMinPct, p.SoCMaxPct),
			fmtWhPtr(p.BatChargedWh), fmtWhPtr(p.BatDischargedWh), fmtEVWh(p.EVWh),
			fmtWhPtr(p.ImportWh), fmtWhPtr(p.ExportWh),
			fmtPlanVsBattery(p), fmtReplans(p), fmtLimits(p))
	}
	b.WriteString("\n`Plan vs battery` compares the battery energy each scored plan slot asked for " +
		"with what the batteries did in it; `off` counts slots outside half to one and a half " +
		"times the plan, or in the wrong direction. EV energy is integrated from five-minute " +
		"averages, so it is close rather than metered. `—` means not measured.\n\n")

	if top := topDigest(w.Digest, 5); len(top) > 0 {
		b.WriteString("Most repeated warnings and errors this week (numbers folded to `#`):\n\n```\n")
		for _, g := range top {
			fmt.Fprintf(b, "%s ×%d", g.Level, g.Count)
			if g.Driver != "" {
				fmt.Fprintf(b, " [%s]", g.Driver)
			}
			fmt.Fprintf(b, " %s (last %s)\n", redactDumpLog(truncateReport(g.Msg, 160)), time.UnixMilli(g.LastMs).In(time.Local).Format("Mon 15:04"))
		}
		b.WriteString("```\n\n")
	}
	var notes []string
	if w.SpansMore {
		notes = append(notes, fmt.Sprintf("the control journal had more than %d spans; the oldest are left out", supportWeekSpanLimit))
	}
	if w.DigestMore {
		notes = append(notes, fmt.Sprintf("more than %d warning rows; the oldest are left out", supportWeekDigestLimit))
	}
	notes = append(notes, w.Gaps...)
	if len(notes) > 0 {
		b.WriteString("Not complete: " + strings.Join(notes, "; ") + ".\n\n")
	}
	b.WriteString("The data behind this table is in `week/` in `/api/support/dump`.\n\n")
}

func writeFindingList(b *strings.Builder, findings []finding) {
	order := map[string]int{sevProblem: 0, sevWarning: 1, sevNote: 2}
	sort.SliceStable(findings, func(i, j int) bool {
		return order[findings[i].Severity] < order[findings[j].Severity]
	})
	for _, f := range findings {
		fmt.Fprintf(b, "- **%s — %s.** %s\n", f.Severity, f.Title, f.Detail)
	}
	b.WriteString("\n")
}

// weekFindings states what stands out in the week, so the section reads
// without the table.
func weekFindings(w *supportWeek) []finding {
	var out []finding
	var followed []string
	for _, p := range w.Periods {
		label := p.Label
		if p.Partial {
			label += " (so far)"
		}
		charge := ""
		if p.SoCMinPct != nil {
			charge = fmt.Sprintf(" (charge %s)", fmtSoCRange(p.SoCMinPct, p.SoCMaxPct))
		}
		discharged := p.ActualOutWh
		if p.BatDischargedWh != nil {
			discharged = *p.BatDischargedWh
		}
		charged := p.ActualInWh
		if p.BatChargedWh != nil {
			charged = *p.BatChargedWh
		}
		if p.SlotsScored > 0 {
			var parts []string
			if d := p.ActualOutWh - p.PlannedOutWh; math.Abs(d) >= supportPlanGapWh {
				parts = append(parts, fmt.Sprintf("discharged %s%s, %s than the plan asked for: its %d scored slots planned %s out and measured %s out",
					fmtReportWh(discharged), charge, moreOrLess(d), p.SlotsScored, fmtReportWh(p.PlannedOutWh), fmtReportWh(p.ActualOutWh)))
			}
			if d := p.ActualInWh - p.PlannedInWh; math.Abs(d) >= supportPlanGapWh {
				parts = append(parts, fmt.Sprintf("charged %s, %s than the plan asked for: planned %s in, measured %s in",
					fmtReportWh(charged), moreOrLess(d), fmtReportWh(p.PlannedInWh), fmtReportWh(p.ActualInWh)))
			}
			if len(parts) > 0 {
				detail := "The battery " + strings.Join(parts, "; and ") + "."
				if p.Replans > 0 {
					detail += " " + fmtReplans(p) + " that period."
				}
				detail += " Safety limits, a device that does not follow its command, or a plan replaced mid-slot " +
					"look like this; the control journal in `week/` shows which."
				out = append(out, finding{sevWarning, "The battery did not follow the plan on " + label, detail})
				continue
			}
		}
		if p.Night && discharged >= supportNightNoteWh {
			s := fmt.Sprintf("%s %s%s", label, fmtReportWh(discharged), charge)
			if p.SlotsScored > 0 {
				s += fmt.Sprintf(", plan %s", fmtReportWh(p.PlannedOutWh))
			}
			followed = append(followed, s)
		}
	}
	if len(followed) > 0 {
		out = append(out, finding{sevNote, "Night discharge",
			"The battery discharged at night: " + strings.Join(followed, "; ") +
				". Where a plan figure is given, the battery stayed within " + fmtReportWh(supportPlanGapWh) + " of it."})
	}

	// Devices that lost contact or reported a fault.
	type down struct {
		total, longest float64
		from           int64
	}
	downs := map[string]*down{}
	for _, sp := range w.Spans {
		if sp.Kind != "source" || (sp.Status != "offline" && sp.Status != "fault") {
			continue
		}
		end := sp.EndMs
		if end == 0 {
			end = w.Until.UnixMilli()
		}
		start := max(sp.StartMs, w.Since.UnixMilli())
		secs := float64(end-start) / 1000
		if secs <= 0 {
			continue
		}
		key := sp.Driver + "\x00" + sp.Status
		d := downs[key]
		if d == nil {
			d = &down{}
			downs[key] = d
		}
		d.total += secs
		if secs > d.longest {
			d.longest, d.from = secs, start
		}
	}
	keys := make([]string, 0, len(downs))
	for k := range downs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		d := downs[k]
		if time.Duration(d.total*float64(time.Second)) < supportSourceDownMin {
			continue
		}
		driver, status, _ := strings.Cut(k, "\x00")
		verb, sev, title := "was offline", sevWarning, driver+" lost contact this week"
		effect := "A device out of contact gets its safe default mode and is left out of the plan, so its share becomes grid flow."
		if status == "fault" {
			verb, sev, title = "reported a fault", sevProblem, driver+" reported a fault this week"
			effect = "A device in fault can be reached but cannot act, so the plan leaves it out and its share becomes grid flow."
		}
		out = append(out, finding{sev, title,
			fmt.Sprintf("%s %s for %s in total; the longest stretch, %s, began %s. %s",
				driver, verb, fmtHours(d.total), fmtHours(d.longest), time.UnixMilli(d.from).In(time.Local).Format("Mon 2 Jan 15:04"), effect)})
	}
	var fuseS, worstFuse float64
	worstLabel := ""
	for _, p := range w.Periods {
		fuseS += p.FuseLimitedS
		if p.FuseLimitedS > worstFuse {
			worstFuse, worstLabel = p.FuseLimitedS, p.Label
		}
	}
	if fuseS >= 60 {
		out = append(out, finding{sevNote, "The fuse limit held power back",
			fmt.Sprintf("Charging or discharging was held under the main fuse for %s this week, most on %s. "+
				"That is protection working; it explains slower charging, not a fault.", fmtHours(fuseS), worstLabel)})
	}

	fallbacks, last := 0, state.PlannerReplan{}
	for _, r := range w.Replans {
		if r.Fallback {
			fallbacks++
			last = r
		}
	}
	if fallbacks > 0 {
		out = append(out, finding{sevWarning, "Plans fell back to Core",
			fmt.Sprintf("%d of %d plans this week came from Core's own planner instead of the optimizer. "+
				"The last, %s: %s.", fallbacks, len(w.Replans),
				time.UnixMilli(last.TsMs).In(time.Local).Format("Mon 2 Jan 15:04"),
				strings.TrimSuffix(redactDumpLog(truncateReport(last.FallbackReason, 200)), "."))})
	}

	if busy, ok := busyPeriod(w.Periods); ok {
		out = append(out, finding{sevNote, "Many replans on " + busy.Label,
			fmt.Sprintf("%s. Frequent replans usually follow a forecast that keeps missing, "+
				"a battery that drifts from its planned charge, or a device that comes and goes.", fmtReplans(busy))})
	}

	var starts, unclean int
	var startTimes []string
	for _, p := range w.Periods {
		starts += p.Starts
		unclean += p.UncleanStarts
	}
	for _, e := range w.Events {
		if e.Event == "startup" {
			startTimes = append(startTimes, time.UnixMilli(e.TsMs).In(time.Local).Format("Mon 15:04"))
		}
	}
	stops := 0
	for _, e := range w.Events {
		if e.Event == "restart" || e.Event == "shutdown" {
			stops++
		}
	}
	switch {
	case unclean > 0:
		out = append(out, finding{sevWarning, "Core stopped without a clean shutdown",
			fmt.Sprintf("Core started %s this week (%s); %d of those came after no recorded stop, "+
				"which is a crash, a power cut or a killed process.", plural(starts, "time"), strings.Join(startTimes, ", "), unclean)})
	case starts > 0 || stops > 0:
		out = append(out, finding{sevNote, "Core restarted",
			fmt.Sprintf("Core recorded %s (restart or shutdown) and %s this week%s. Each restart replans "+
				"and briefly hands devices their default mode.", plural(stops, "stop"), plural(starts, "start"), listSuffix(startTimes))})
	}

	if top := topDigest(w.Digest, 1); len(top) > 0 && top[0].Count >= supportRepeatWarnings {
		g := top[0]
		sev := sevNote
		if g.Level == "ERROR" {
			sev = sevWarning
		}
		who := ""
		if g.Driver != "" {
			who = " from " + g.Driver
		}
		out = append(out, finding{sev, "One message dominates the week's log",
			fmt.Sprintf("%q%s was logged %d times. The table of repeated messages below has the rest.",
				redactDumpLog(truncateReport(g.Msg, 160)), who, g.Count)})
	}

	empty := periodsBeforeData(w)
	if empty > 0 {
		out = append(out, finding{sevNote, "The stored week starts late",
			"The first stored reading is from " + w.FirstData.In(time.Local).Format("Mon 2 Jan 15:04") +
				", so the box is newer than the week or was off before then. The periods before it have no data."})
	}
	// The period holding the first reading is incomplete by definition.
	holding := -1
	if empty < len(w.Periods) && !w.FirstData.IsZero() && w.FirstData.After(w.Periods[empty].period.Start) {
		holding = empty
	}
	var incomplete []string
	for i, p := range w.Periods {
		if i < empty || i == holding || p.Partial || !p.EnergyIncomplete {
			continue
		}
		incomplete = append(incomplete, p.Label)
	}
	if len(incomplete) > 0 {
		which := strings.Join(incomplete, "; ")
		if len(incomplete) > 3 {
			which = fmt.Sprintf("%d periods, from %s to %s", len(incomplete), incomplete[0], incomplete[len(incomplete)-1])
		}
		out = append(out, finding{sevNote, "Stored history is incomplete",
			"Battery or grid energy is missing for " + which +
				". The box was off, a meter was not read, or history was not written."})
	}
	return out
}

// periodsBeforeData counts the leading periods that end before the first
// stored series point. They have nothing to show.
func periodsBeforeData(w *supportWeek) int {
	if w.FirstData.IsZero() {
		return 0
	}
	n := 0
	for _, p := range w.Periods {
		if !p.period.End.After(w.FirstData) {
			n++
			continue
		}
		break
	}
	return n
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func listSuffix(items []string) string {
	if len(items) == 0 {
		return ""
	}
	return " (starts " + strings.Join(items, ", ") + ")"
}

func moreOrLess(delta float64) string {
	if delta > 0 {
		return "more"
	}
	return "less"
}

// busyPeriod is the period with the most replans when it has at least
// supportBusyReplans and three times the median.
func busyPeriod(ps []supportPeriodSummary) (supportPeriodSummary, bool) {
	if len(ps) < 3 {
		return supportPeriodSummary{}, false
	}
	counts := make([]int, len(ps))
	best := 0
	for i, p := range ps {
		counts[i] = p.Replans
		if p.Replans > ps[best].Replans {
			best = i
		}
	}
	sort.Ints(counts)
	median := counts[len(counts)/2]
	if ps[best].Replans >= supportBusyReplans && ps[best].Replans >= 3*max(median, 1) {
		return ps[best], true
	}
	return supportPeriodSummary{}, false
}

// topDigest sums the week's rows per pattern and returns the n most frequent.
func topDigest(rows []state.LogDigest, n int) []state.LogDigest {
	type key struct{ level, driver, pattern string }
	sum := map[key]*state.LogDigest{}
	for _, r := range rows {
		k := key{r.Level, r.Driver, r.Pattern}
		g := sum[k]
		if g == nil {
			c := r
			sum[k] = &c
			continue
		}
		g.Count += r.Count
		if r.LastMs >= g.LastMs {
			g.LastMs, g.Msg, g.Attrs = r.LastMs, r.Msg, r.Attrs
		}
	}
	out := make([]state.LogDigest, 0, len(sum))
	for _, g := range sum {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].LastMs > out[j].LastMs
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// csvNumber keeps three decimals: a bucket average otherwise prints sixteen
// digits, while an energy counter in Wh still needs all its whole ones.
func csvNumber(v float64) string {
	return strconv.FormatFloat(math.Round(v*1000)/1000, 'f', -1, 64)
}

func fmtSoCRange(lo, hi *float64) string {
	if lo == nil || hi == nil {
		return "—"
	}
	return fmt.Sprintf("%.0f–%.0f%%", *lo, *hi)
}

func fmtWhPtr(v *float64) string {
	if v == nil {
		return "—"
	}
	return fmtReportWh(*v)
}

func fmtEVWh(v *float64) string {
	if v == nil {
		return "—"
	}
	return "≈ " + fmtReportWh(*v)
}

func fmtHours(secs float64) string {
	if secs < 3600 {
		return fmt.Sprintf("%.0f min", secs/60)
	}
	return fmt.Sprintf("%.1f h", secs/3600)
}

func fmtPlanVsBattery(p supportPeriodSummary) string {
	if p.SlotsScored == 0 {
		return "—"
	}
	return fmt.Sprintf("%d of %d off · gap %s", p.SlotsOffPlan, p.SlotsScored, fmtReportWh(p.SlotGapWh))
}

func fmtReplans(p supportPeriodSummary) string {
	if p.Replans == 0 {
		return "0 replans"
	}
	s := fmt.Sprintf("%d replans", p.Replans)
	if p.Replans == 1 {
		s = "1 replan"
	}
	var rs []string
	for i, r := range p.ReplanReasons {
		if i == 3 {
			break
		}
		rs = append(rs, fmt.Sprintf("%s %d", r.Name, r.Count))
	}
	if len(rs) > 0 {
		s += " (" + strings.Join(rs, ", ") + ")"
	}
	if p.Fallbacks > 0 {
		s += fmt.Sprintf(" · %d fallback", p.Fallbacks)
	}
	return s
}

func fmtLimits(p supportPeriodSummary) string {
	var parts []string
	if p.FuseLimitedS >= 60 {
		parts = append(parts, "fuse "+fmtHours(p.FuseLimitedS))
	}
	for _, d := range p.SourceDown {
		parts = append(parts, d.Name+" "+fmtHours(float64(d.Count)))
	}
	if p.Warnings > 0 || p.Errors > 0 {
		parts = append(parts, fmt.Sprintf("%d warnings, %d errors", p.Warnings, p.Errors))
	}
	if p.Starts > 0 {
		parts = append(parts, plural(p.Starts, "start"))
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, " · ")
}

// ---- the archive's week/ ----

// addWeekFiles writes the week's data into the archive. JSON passes the
// same redaction as the logs: device errors in the journal carry addresses.
func addWeekFiles(add func(string, []byte), w *supportWeek) {
	if w == nil {
		return
	}
	redacted := func(v any) []byte {
		raw, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return []byte(`{"error":` + fmt.Sprintf("%q", err.Error()) + `}`)
		}
		return []byte(redactDumpLog(string(raw)))
	}
	window := func(extra map[string]any) map[string]any {
		extra["since_ms"], extra["until_ms"] = w.Since.UnixMilli(), w.Until.UnixMilli()
		return extra
	}
	index := make([]map[string]any, 0, len(w.Series))
	for _, ser := range w.Series {
		index = append(index, map[string]any{"driver": ser.Driver, "metric": ser.Metric, "unit": ser.Unit, "buckets": len(ser.Points)})
	}
	add("week/summary.json", redacted(window(map[string]any{
		"tz": w.Until.Location().String(), "bucket_ms": state.BucketWidthMs(w.Since.UnixMilli(), w.Until.UnixMilli(), supportWeekPoints()),
		"periods": w.Periods, "series": index, "gaps": append([]string{}, w.Gaps...),
	})))
	spans := make([]journalSpan, 0, len(w.Spans))
	for _, sp := range w.Spans {
		spans = append(spans, toJournalSpan(sp, w.Until.UnixMilli()))
	}
	add("week/control-journal.json", redacted(window(map[string]any{"spans": spans, "more": w.SpansMore})))
	add("week/plans.json", redacted(window(map[string]any{"replans": w.Replans})))
	add("week/notifications.json", redacted(window(map[string]any{"notifications": w.Notices, "more": w.NoticesMore})))
	events := make([]map[string]any, 0, len(w.Events))
	for _, e := range w.Events {
		events = append(events, map[string]any{"ts_ms": e.TsMs, "event": e.Event})
	}
	add("week/events.json", redacted(window(map[string]any{"events": events})))

	for _, ser := range w.Series {
		var b strings.Builder
		b.WriteString("ts_ms,v,min,max,n\n")
		for _, p := range ser.Points {
			fmt.Fprintf(&b, "%d,%s,%s,%s,%d\n", p.TsMs, csvNumber(p.V), csvNumber(p.Min), csvNumber(p.Max), p.N)
		}
		add("week/series/"+sanitizeName(ser.Driver)+"__"+sanitizeName(ser.Metric)+".csv", []byte(b.String()))
	}

	var b strings.Builder
	if len(w.Digest) == 0 {
		b.WriteString("No warnings or errors stored for the week.\n")
	}
	for _, g := range w.Digest {
		fmt.Fprintf(&b, "%s %s ", time.UnixMilli(g.HourMs).UTC().Format("2006-01-02T15Z"), g.Level)
		if g.Driver != "" {
			fmt.Fprintf(&b, "[%s] ", g.Driver)
		}
		b.WriteString(redactDumpLog(g.Msg))
		fmt.Fprintf(&b, " ×%d first %s last %s", g.Count,
			time.UnixMilli(g.FirstMs).UTC().Format("15:04:05"), time.UnixMilli(g.LastMs).UTC().Format("15:04:05"))
		if g.Attrs != "" {
			b.WriteString(" " + redactDumpLog(g.Attrs))
		}
		b.WriteByte('\n')
	}
	add("logs/warnings-week.log", []byte(b.String()))
}
