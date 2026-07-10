package caldavserver

import (
	"log/slog"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/caldav"
)

const (
	maxExpansionWindow     = 366 * 24 * time.Hour
	maxExpandedOccurrences = 10000
)

// findExpand walks a CalendarCompRequest tree for a CALDAV:expand directive.
// Clients nest it under the VEVENT comp request, so a plain top-level check
// isn't enough. Returns nil when the client did not request expansion.
//
// NB: go-webdav v0.7's REPORT handler does not decode the <C:expand> element
// into the backend query (it only surfaces the comp-filter), so in practice the
// expansion window comes from filterTimeRange below. This is kept for forward
// compatibility should a future go-webdav start passing it through.
func findExpand(req caldav.CalendarCompRequest) *caldav.CalendarExpandRequest {
	if req.Expand != nil {
		return req.Expand
	}
	for i := range req.Comps {
		if e := findExpand(req.Comps[i]); e != nil {
			return e
		}
	}
	return nil
}

// filterTimeRange returns the [start, end] window carried by a calendar-query's
// (VEVENT) comp-filter time-range, if any. This is how the requested window
// actually reaches the backend in go-webdav v0.7 — the client sends the same
// window on both the comp-filter and the (dropped) expand element. Only returns
// ok when both bounds are present, so an open-ended query keeps its masters.
func filterTimeRange(cf caldav.CompFilter) (time.Time, time.Time, bool) {
	for i := range cf.Comps {
		if s, e, ok := filterTimeRange(cf.Comps[i]); ok {
			return s, e, ok
		}
	}
	if !cf.Start.IsZero() && !cf.End.IsZero() {
		return cf.Start, cf.End, true
	}
	return time.Time{}, time.Time{}, false
}

// expandObjects implements RFC 4791 CALDAV:expand. Every recurring VEVENT in a
// resource is replaced by the concrete instances whose start falls inside
// [start, end], each carrying its own RECURRENCE-ID and stripped of
// RRULE/RDATE/EXDATE. Non-recurring components pass through unchanged. A
// resource left with no in-range component after expansion is dropped.
//
// caldav.Filter has already kept only resources with at least one instance in
// range (it evaluates the recurrence set for the time-range match), so this
// only ever expands events that genuinely have occurrences in the window.
func expandObjects(objs []caldav.CalendarObject, start, end time.Time) []caldav.CalendarObject {
	out := make([]caldav.CalendarObject, 0, len(objs))
	for _, co := range objs {
		if co.Data == nil {
			out = append(out, co)
			continue
		}
		expanded := expandCalendar(co.Data, start, end)
		if expanded == nil {
			continue
		}
		co.Data = expanded
		out = append(out, co)
	}
	return out
}

// eventGroup is the set of VEVENT components sharing one UID: the recurrence
// master (no RECURRENCE-ID) plus zero or more per-instance override components
// (each with a RECURRENCE-ID). Per RFC 5545 a recurrence set lives in a single
// calendar object resource, so grouping within one calendar is sufficient.
type eventGroup struct {
	master    *ical.Component
	overrides []*ical.Component
}

// expandCalendar returns a copy of cal with every recurring VEVENT expanded
// into its per-occurrence instances within [start, end]. RRULE, RDATE and
// EXDATE are resolved via go-ical's RecurrenceSet; per-instance RECURRENCE-ID
// override components replace (or, when STATUS:CANCELLED, delete) the matching
// generated instance. Non-event components (e.g. VTIMEZONE) and non-recurring
// events are preserved verbatim. Returns nil when no component remains.
func expandCalendar(cal *ical.Calendar, start, end time.Time) *ical.Calendar {
	// CalDAV clients control the REPORT window. Bound it before recurrence
	// math so an authenticated but buggy/hostile client cannot request decades
	// of expansion and exhaust a Pi. The planner's configured horizon is at
	// most one year, so this does not truncate an in-scope query.
	if end.After(start.Add(maxExpansionWindow)) {
		end = start.Add(maxExpansionWindow)
	}
	loc := start.Location()
	if loc == nil {
		loc = time.UTC
	}
	out := ical.NewCalendar()
	for name, props := range cal.Props {
		out.Props[name] = append([]ical.Prop(nil), props...)
	}

	// Partition VEVENTs into UID groups; everything else passes through. Events
	// with no UID can't be grouped, so each becomes its own singleton group.
	var groups []*eventGroup
	byUID := map[string]*eventGroup{}
	for _, child := range cal.Children {
		if child.Name != ical.CompEvent {
			out.Children = append(out.Children, child)
			continue
		}
		uid, _ := child.Props.Text(ical.PropUID)
		var g *eventGroup
		if uid != "" {
			g = byUID[uid]
		}
		if g == nil {
			g = &eventGroup{}
			groups = append(groups, g)
			if uid != "" {
				byUID[uid] = g
			}
		}
		if child.Props.Get(ical.PropRecurrenceID) != nil {
			g.overrides = append(g.overrides, child)
		} else {
			g.master = child // a later non-RECURRENCE-ID component wins
		}
	}

	for _, g := range groups {
		expandGroup(out, g, start, end, loc)
	}

	if len(out.Children) == 0 {
		return nil
	}
	return out
}

// expandGroup emits the in-window instances for one UID group into out.
func expandGroup(out *ical.Calendar, g *eventGroup, start, end time.Time, loc *time.Location) {
	// Index overrides by the instant their RECURRENCE-ID identifies.
	overrides := make(map[int64]*ical.Component, len(g.overrides))
	for _, ov := range g.overrides {
		if k, ok := recurrenceKey(ov, loc); ok {
			overrides[k] = ov
		}
	}
	consumed := make(map[int64]bool, len(overrides))

	if g.master != nil {
		if rset, err := g.master.RecurrenceSet(loc); err == nil && rset != nil {
			if rule := rset.GetRRule(); rule != nil {
				freq := rule.OrigOptions.Freq.String()
				if freq == "SECONDLY" || freq == "MINUTELY" {
					// Sub-hourly recurrence has no useful planner meaning and can
					// generate hundreds of thousands of instances in the normal
					// horizon. Preserve the master unexpanded for calendar clients,
					// while keeping the planner from materialising the flood.
					slog.Warn("caldav: refusing sub-hourly recurrence expansion", "frequency", freq)
					out.Children = append(out.Children, g.master)
					return
				}
			}
			ev := ical.Event{Component: g.master}
			st0, errS := ev.DateTimeStart(loc)
			en0, errE := ev.DateTimeEnd(loc)
			var dur time.Duration
			if errS == nil && errE == nil && en0.After(st0) {
				dur = en0.Sub(st0)
			}
			allDay := isAllDay(g.master)
			occurrences := rset.Between(start, end, true)
			if len(occurrences) > maxExpandedOccurrences {
				slog.Warn("caldav: recurrence expansion capped", "count", len(occurrences), "cap", maxExpandedOccurrences)
				occurrences = occurrences[:maxExpandedOccurrences]
			}
			for _, occ := range occurrences {
				occ = occ.In(loc)
				k := occ.UTC().Unix()
				if ov, ok := overrides[k]; ok {
					consumed[k] = true
					// A cancelled occurrence is dropped; a moved one is emitted
					// only if its new time still falls in the window.
					if !isCancelled(ov) && overrideInWindow(ov, start, end, loc) {
						out.Children = append(out.Children, ov)
					}
					continue
				}
				out.Children = append(out.Children, makeInstance(g.master, occ, dur, allDay))
			}
			// Overrides whose original occurrence is outside the window but whose
			// new time was moved into it (or RDATE-style additions) — emit once.
			for _, ov := range g.overrides {
				k, ok := recurrenceKey(ov, loc)
				if !ok || consumed[k] || isCancelled(ov) {
					continue
				}
				if overrideInWindow(ov, start, end, loc) {
					out.Children = append(out.Children, ov)
					consumed[k] = true
				}
			}
			return
		}
		// Master without a recurrence set: a plain non-recurring event. Pass it
		// through unchanged.
		out.Children = append(out.Children, g.master)
	}

	// Orphan overrides (no master in this resource): emit those intersecting the
	// window so a stray override is never silently dropped.
	for _, ov := range g.overrides {
		if isCancelled(ov) {
			continue
		}
		if overrideInWindow(ov, start, end, loc) {
			out.Children = append(out.Children, ov)
		}
	}
}

// makeInstance clones the master into a single concrete occurrence at occ: the
// RRULE/RDATE/EXDATE are stripped and DTSTART/DTEND/RECURRENCE-ID set.
func makeInstance(master *ical.Component, occ time.Time, dur time.Duration, allDay bool) *ical.Component {
	inst := cloneComponent(master)
	inst.Props.Del(ical.PropRecurrenceRule)
	inst.Props.Del(ical.PropRecurrenceDates)
	inst.Props.Del(ical.PropExceptionDates)
	if allDay {
		inst.Props.SetDate(ical.PropDateTimeStart, occ)
		inst.Props.SetDate(ical.PropRecurrenceID, occ)
		if dur > 0 {
			inst.Props.SetDate(ical.PropDateTimeEnd, occ.Add(dur))
		}
	} else {
		inst.Props.SetDateTime(ical.PropDateTimeStart, occ)
		inst.Props.SetDateTime(ical.PropRecurrenceID, occ)
		if dur > 0 {
			inst.Props.SetDateTime(ical.PropDateTimeEnd, occ.Add(dur))
		}
	}
	return inst
}

// recurrenceKey is the instant (unix seconds, UTC) a component's RECURRENCE-ID
// identifies, used to match an override to a generated occurrence.
func recurrenceKey(c *ical.Component, loc *time.Location) (int64, bool) {
	rid := c.Props.Get(ical.PropRecurrenceID)
	if rid == nil {
		return 0, false
	}
	t, err := rid.DateTime(loc)
	if err != nil {
		return 0, false
	}
	return t.UTC().Unix(), true
}

// isAllDay reports whether the component's DTSTART is a DATE (no time-of-day).
func isAllDay(c *ical.Component) bool {
	p := c.Props.Get(ical.PropDateTimeStart)
	return p != nil && p.ValueType() == ical.ValueDate
}

// isCancelled reports whether the component is STATUS:CANCELLED — i.e. this
// occurrence has been removed from the recurrence set.
func isCancelled(c *ical.Component) bool {
	s, err := (&ical.Event{Component: c}).Status()
	return err == nil && s == ical.EventCancelled
}

// overrideInWindow reports whether an override component's own [DTSTART, DTEND]
// intersects [start, end]. Fails open (includes) when the times can't be read,
// mirroring how non-recurring events pass through.
func overrideInWindow(c *ical.Component, start, end time.Time, loc *time.Location) bool {
	ev := ical.Event{Component: c}
	s, err := ev.DateTimeStart(loc)
	if err != nil {
		return true
	}
	e, err := ev.DateTimeEnd(loc)
	if err != nil || !e.After(s) {
		e = s
	}
	return !s.After(end) && !e.Before(start)
}

// cloneComponent deep-copies a component's property slices (so per-instance
// edits never touch the stored master) and shallow-copies its children, which
// the expander only reads.
func cloneComponent(c *ical.Component) *ical.Component {
	nc := ical.NewComponent(c.Name)
	for name, props := range c.Props {
		nc.Props[name] = append([]ical.Prop(nil), props...)
	}
	nc.Children = append(nc.Children, c.Children...)
	return nc
}
