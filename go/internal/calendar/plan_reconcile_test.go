package calendar

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	webdav "github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"

	"github.com/srcfl/ftw/go/internal/caldavserver"
	"github.com/srcfl/ftw/go/internal/config"
)

func planServer(t *testing.T) (*httptest.Server, *caldav.Client) {
	t.Helper()
	srv := httptest.NewServer(caldavserver.NewHandler("u", "p", "/u/", []string{"/u/energy/", "/u/plan/"}, caldavserver.NewMemStore()))
	t.Cleanup(srv.Close)
	hc := webdav.HTTPClientWithBasicAuth(http.DefaultClient, "u", "p")
	c, err := caldav.NewClient(hc, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return srv, c
}

// putRawPlan writes a plan object the way a previous FTW process would have, so
// tests can stage pre-existing / orphaned objects in the plan collection.
func putRawPlan(t *testing.T, c *caldav.Client, uid, summary string, start, end time.Time) {
	t.Helper()
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropProductID, "-//ftw-test//EN")
	cal.Props.SetText(ical.PropVersion, "2.0")
	ev := ical.NewEvent()
	ev.Props.SetText(ical.PropUID, uid)
	ev.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())
	ev.Props.SetDateTime(ical.PropDateTimeStart, start)
	ev.Props.SetDateTime(ical.PropDateTimeEnd, end)
	ev.Props.SetText(ical.PropSummary, summary)
	cal.Children = append(cal.Children, ev.Component)
	if _, err := c.PutCalendarObject(context.Background(), "/u/plan/"+uid+".ics", cal); err != nil {
		t.Fatalf("seed plan object %s: %v", uid, err)
	}
}

// planSummaries returns the SUMMARY of every object currently in the plan
// collection (wide time range so past objects are included).
func planSummaries(t *testing.T, c *caldav.Client) []string {
	t.Helper()
	now := time.Now()
	objs, err := c.QueryCalendar(context.Background(), "/u/plan/", &caldav.CalendarQuery{
		CompRequest: caldav.CalendarCompRequest{Name: "VCALENDAR", Comps: []caldav.CalendarCompRequest{{Name: "VEVENT", AllProps: true}}},
		CompFilter: caldav.CompFilter{Name: "VCALENDAR", Comps: []caldav.CompFilter{{
			Name: "VEVENT", Start: now.Add(-365 * 24 * time.Hour), End: now.Add(365 * 24 * time.Hour),
		}}},
	})
	if err != nil {
		t.Fatalf("query plan collection: %v", err)
	}
	var out []string
	for _, o := range objs {
		if o.Data == nil {
			continue
		}
		for _, ev := range o.Data.Events() {
			sum, _ := ev.Props.Text(ical.PropSummary)
			out = append(out, sum)
		}
	}
	return out
}

func planService(t *testing.T, url string) *Service {
	t.Helper()
	return New(config.CalDAV{
		Enabled: true, URL: url, Username: "u", Password: "p",
		CalendarPath: "/u/energy/", PlanPath: "/u/plan/",
	}, &fakeLP{}, &fakeLM{}, "garage")
}

// TestPlanReconcileReclaimsOrphansOnRestart proves the cross-restart orphan fix:
// a plan object left by a previous process (whose window the current plan no
// longer regenerates) is deleted on the first publish after a restart, because
// the reconcile seeds its state from the live collection rather than an empty
// in-memory map.
func TestPlanReconcileReclaimsOrphansOnRestart(t *testing.T) {
	srv, c := planServer(t)
	now := time.Now()

	// Stale object from a "previous process": a past discharge window.
	pastStart := now.Add(-48 * time.Hour)
	orphanUID := "ftw-plan-dis-" + strconv.FormatInt(pastStart.Unix(), 10) + "@fortytwowatts"
	putRawPlan(t, c, orphanUID, "Discharge battery ~2.0 kW", pastStart, pastStart.Add(time.Hour))

	// Fresh service (empty planWritten, planSeeded=false) models the restart.
	s := planService(t, srv.URL)
	s.SetPlanSource(func() []PlanSlot {
		return []PlanSlot{{Start: now.Add(time.Hour), End: now.Add(2 * time.Hour), BatteryW: 4000, SoCPct: 70}}
	})
	s.publishPlan(context.Background())

	sums := planSummaries(t, c)
	charge := 0
	for _, sum := range sums {
		if strings.Contains(sum, "Discharge") {
			t.Fatalf("stale orphan not reclaimed after restart; summaries=%v", sums)
		}
		if strings.Contains(sum, "Charge battery") {
			charge++
		}
	}
	if charge != 1 {
		t.Fatalf("want exactly 1 charge window, got %d; summaries=%v", charge, sums)
	}
}

// TestPlanPublishSkipsEmptyPlan proves the guard: when the planner has produced
// nothing yet (empty slots), publishPlan must leave the existing calendar alone
// rather than deleting every window (which — combined with the restart seed —
// would otherwise wipe the plan calendar on each restart).
func TestPlanPublishSkipsEmptyPlan(t *testing.T) {
	srv, c := planServer(t)
	now := time.Now()

	putRawPlan(t, c, "ftw-plan-chg-existing@fortytwowatts", "Charge battery ~3.0 kW", now.Add(time.Hour), now.Add(2*time.Hour))

	s := planService(t, srv.URL)
	s.SetPlanSource(func() []PlanSlot { return nil }) // planner not ready
	s.publishPlan(context.Background())

	if sums := planSummaries(t, c); len(sums) != 1 {
		t.Fatalf("empty plan must leave existing events untouched, got %d: %v", len(sums), sums)
	}
}
