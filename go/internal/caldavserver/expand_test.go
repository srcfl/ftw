package caldavserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	webdav "github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"
)

// expandQuery is a calendar-query REPORT carrying a VEVENT time-range — the
// shape 42W's calendar client sends — which the backend uses as the recurrence
// expansion window.
func expandQuery(start, end time.Time) *caldav.CalendarQuery {
	return &caldav.CalendarQuery{
		CompRequest: caldav.CalendarCompRequest{
			Name: "VCALENDAR",
			Comps: []caldav.CalendarCompRequest{{
				Name:     "VEVENT",
				AllProps: true,
				Expand:   &caldav.CalendarExpandRequest{Start: start, End: end},
			}},
		},
		CompFilter: caldav.CompFilter{
			Name:  "VCALENDAR",
			Comps: []caldav.CompFilter{{Name: "VEVENT", Start: start, End: end}},
		},
	}
}

// TestNativeServerExpandsRecurrence proves the gap that used to require an
// external CalDAV server is closed: a daily-recurring event is returned as one
// concrete instance per occurrence in the queried window — each with a
// RECURRENCE-ID and no RRULE — rather than just its master VEVENT.
func TestNativeServerExpandsRecurrence(t *testing.T) {
	srv := httptest.NewServer(testHandler("u", "p", "/u/", []string{"/u/energy/"}))
	defer srv.Close()
	hc := webdav.HTTPClientWithBasicAuth(http.DefaultClient, "u", "p")
	c, err := caldav.NewClient(hc, srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	// A daily-recurring 1 h "Away" event anchored at a fixed instant so the test
	// is independent of the wall clock (the window below is explicit).
	anchor := time.Date(2026, time.June, 1, 9, 0, 0, 0, time.UTC)
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropProductID, "-//ftw-test//EN")
	cal.Props.SetText(ical.PropVersion, "2.0")
	ev := ical.NewEvent()
	ev.Props.SetText(ical.PropUID, "away-daily")
	ev.Props.SetDateTime(ical.PropDateTimeStamp, anchor)
	ev.Props.SetDateTime(ical.PropDateTimeStart, anchor)
	ev.Props.SetDateTime(ical.PropDateTimeEnd, anchor.Add(time.Hour))
	ev.Props.SetText(ical.PropSummary, "Away — daily")
	// RRULE must keep its default RECUR value type — SetText would tag it
	// VALUE=TEXT and break parsing, which real calendar apps never do.
	ev.Props.Set(&ical.Prop{Name: ical.PropRecurrenceRule, Value: "FREQ=DAILY;COUNT=10"})
	cal.Children = append(cal.Children, ev.Component)
	if _, err := c.PutCalendarObject(context.Background(), "/u/energy/away.ics", cal); err != nil {
		t.Fatalf("PUT: %v", err)
	}

	// A window covering Jun 1, 2, 3 (ending just before the Jun 4 occurrence).
	start := anchor.Add(-time.Hour)
	end := anchor.Add(3*24*time.Hour - time.Minute)
	objs, err := c.QueryCalendar(context.Background(), "/u/energy/", expandQuery(start, end))
	if err != nil {
		t.Fatalf("REPORT: %v", err)
	}

	instances := 0
	for _, o := range objs {
		if o.Data == nil {
			continue
		}
		for _, e := range o.Data.Events() {
			instances++
			if rr, _ := e.Props.Text(ical.PropRecurrenceRule); rr != "" {
				t.Fatalf("expanded instance must not carry an RRULE, got %q", rr)
			}
			if rid := e.Props.Get(ical.PropRecurrenceID); rid == nil {
				t.Fatalf("expanded instance must carry a RECURRENCE-ID")
			}
		}
	}
	if instances != 3 {
		t.Fatalf("expected 3 expanded instances in the 3-day window, got %d", instances)
	}
}

// TestExpandCalendarUnit exercises the pure expander without the HTTP layer:
// a non-recurring event passes through untouched; a recurring one fans out.
func TestExpandCalendarUnit(t *testing.T) {
	anchor := time.Date(2026, time.June, 1, 9, 0, 0, 0, time.UTC)
	mk := func(rrule string) *ical.Calendar {
		cal := ical.NewCalendar()
		ev := ical.NewEvent()
		ev.Props.SetText(ical.PropUID, "x")
		ev.Props.SetDateTime(ical.PropDateTimeStart, anchor)
		ev.Props.SetDateTime(ical.PropDateTimeEnd, anchor.Add(time.Hour))
		ev.Props.SetText(ical.PropSummary, "x")
		if rrule != "" {
			ev.Props.Set(&ical.Prop{Name: ical.PropRecurrenceRule, Value: rrule})
		}
		cal.Children = append(cal.Children, ev.Component)
		return cal
	}
	start, end := anchor.Add(-time.Hour), anchor.Add(3*24*time.Hour-time.Minute)

	// Non-recurring: returned unchanged (still has exactly one event).
	if got := expandCalendar(mk(""), start, end); got == nil || len(got.Events()) != 1 {
		t.Fatalf("non-recurring event should pass through as 1 event, got %v", got)
	}
	// Recurring daily: 3 instances in the window.
	if got := expandCalendar(mk("FREQ=DAILY;COUNT=10"), start, end); got == nil || len(got.Events()) != 3 {
		n := 0
		if got != nil {
			n = len(got.Events())
		}
		t.Fatalf("daily recurrence should expand to 3 instances, got %d", n)
	}
}

// mkEvent builds a timed VEVENT for the expansion edge-case tests.
func mkEvent(uid, summary string, start time.Time, dur time.Duration) *ical.Event {
	ev := ical.NewEvent()
	ev.Props.SetText(ical.PropUID, uid)
	ev.Props.SetDateTime(ical.PropDateTimeStamp, start)
	ev.Props.SetDateTime(ical.PropDateTimeStart, start)
	ev.Props.SetDateTime(ical.PropDateTimeEnd, start.Add(dur))
	ev.Props.SetText(ical.PropSummary, summary)
	return ev
}

// June 1 2026, 09:00 UTC and a window covering Jun 1, 2, 3 (Jun 4 excluded).
var (
	expAnchor = time.Date(2026, time.June, 1, 9, 0, 0, 0, time.UTC)
	expStart  = expAnchor.Add(-time.Hour)
	expEnd    = expAnchor.Add(3*24*time.Hour - time.Minute)
)

func summaries(cal *ical.Calendar) []string {
	if cal == nil {
		return nil
	}
	out := []string{}
	for _, e := range cal.Events() {
		s, _ := e.Props.Text(ical.PropSummary)
		out = append(out, s)
	}
	return out
}

// TestExpandRecurrenceIDOverride: a per-instance override replaces exactly that
// occurrence (no duplicate), and the other occurrences are still generated.
func TestExpandRecurrenceIDOverride(t *testing.T) {
	cal := ical.NewCalendar()
	master := mkEvent("e", "Away", expAnchor, time.Hour)
	master.Props.Set(&ical.Prop{Name: ical.PropRecurrenceRule, Value: "FREQ=DAILY;COUNT=10"})
	cal.Children = append(cal.Children, master.Component)

	ov := mkEvent("e", "Away (changed)", expAnchor.AddDate(0, 0, 1), time.Hour) // Jun 2
	ov.Props.SetDateTime(ical.PropRecurrenceID, expAnchor.AddDate(0, 0, 1))
	cal.Children = append(cal.Children, ov.Component)

	got := summaries(expandCalendar(cal, expStart, expEnd))
	if len(got) != 3 {
		t.Fatalf("want 3 instances, got %d (%v)", len(got), got)
	}
	changed := 0
	for _, s := range got {
		if s == "Away (changed)" {
			changed++
		}
	}
	if changed != 1 {
		t.Fatalf("want exactly one overridden instance, got %d (%v)", changed, got)
	}
}

// TestExpandRecurrenceIDCancellation: a STATUS:CANCELLED override deletes that
// occurrence from the set.
func TestExpandRecurrenceIDCancellation(t *testing.T) {
	cal := ical.NewCalendar()
	master := mkEvent("e", "Away", expAnchor, time.Hour)
	master.Props.Set(&ical.Prop{Name: ical.PropRecurrenceRule, Value: "FREQ=DAILY;COUNT=10"})
	cal.Children = append(cal.Children, master.Component)

	cancel := mkEvent("e", "Away", expAnchor.AddDate(0, 0, 1), time.Hour) // Jun 2
	cancel.Props.SetDateTime(ical.PropRecurrenceID, expAnchor.AddDate(0, 0, 1))
	cancel.SetStatus(ical.EventCancelled)
	cal.Children = append(cal.Children, cancel.Component)

	got := expandCalendar(cal, expStart, expEnd)
	if n := len(got.Events()); n != 2 {
		t.Fatalf("cancelled occurrence should leave 2 instances, got %d", n)
	}
	for _, e := range got.Events() {
		if st, _ := e.DateTimeStart(time.UTC); st.Equal(expAnchor.AddDate(0, 0, 1)) {
			t.Fatalf("the cancelled Jun 2 occurrence must not appear")
		}
	}
}

// TestExpandEXDATE: an EXDATE removes a generated occurrence.
func TestExpandEXDATE(t *testing.T) {
	cal := ical.NewCalendar()
	master := mkEvent("e", "Away", expAnchor, time.Hour)
	master.Props.Set(&ical.Prop{Name: ical.PropRecurrenceRule, Value: "FREQ=DAILY;COUNT=10"})
	master.Props.SetDateTime(ical.PropExceptionDates, expAnchor.AddDate(0, 0, 1)) // drop Jun 2
	cal.Children = append(cal.Children, master.Component)

	got := expandCalendar(cal, expStart, expEnd)
	if n := len(got.Events()); n != 2 {
		t.Fatalf("EXDATE should leave 2 instances (Jun 1, 3), got %d", n)
	}
}

// TestExpandRDATE: an RDATE adds an occurrence beyond the RRULE.
func TestExpandRDATE(t *testing.T) {
	cal := ical.NewCalendar()
	master := mkEvent("e", "Away", expAnchor, time.Hour)
	master.Props.Set(&ical.Prop{Name: ical.PropRecurrenceRule, Value: "FREQ=DAILY;COUNT=2"}) // Jun 1, 2
	master.Props.SetDateTime(ical.PropRecurrenceDates, expAnchor.AddDate(0, 0, 2))           // add Jun 3
	cal.Children = append(cal.Children, master.Component)

	got := expandCalendar(cal, expStart, expEnd)
	if n := len(got.Events()); n != 3 {
		t.Fatalf("RDATE should add a third instance (Jun 1, 2, 3), got %d", n)
	}
}
