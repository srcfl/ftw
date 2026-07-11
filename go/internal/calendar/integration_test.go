//go:build caldav_it

// Integration test against an external CalDAV server. Excluded from the
// normal build; run with a server available (point it at any CalDAV URL):
//
//	FTW_CALDAV_IT_URL=http://localhost:5232 \
//	FTW_CALDAV_IT_USER=ituser FTW_CALDAV_IT_PASS=itpass \
//	go test -tags caldav_it ./internal/calendar/ -run TestCalDAVIntegration -v
package calendar

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	webdav "github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"

	"github.com/frahlg/forty-two-watts/go/internal/config"
)

func itEnv(t *testing.T) (url, user, pass string) {
	url = os.Getenv("FTW_CALDAV_IT_URL")
	user = os.Getenv("FTW_CALDAV_IT_USER")
	pass = os.Getenv("FTW_CALDAV_IT_PASS")
	if url == "" {
		t.Skip("set FTW_CALDAV_IT_URL to run the CalDAV integration test")
	}
	return
}

func itClient(url, user, pass string) *caldav.Client {
	hc := webdav.HTTPClientWithBasicAuth(&http.Client{Timeout: 15 * time.Second}, user, pass)
	c, _ := caldav.NewClient(hc, url)
	return c
}

// mkcalendar ensures a calendar collection exists (idempotent).
func mkcalendar(t *testing.T, url, user, pass, path string) {
	req, _ := http.NewRequest("MKCALENDAR", strings.TrimRight(url, "/")+path, nil)
	req.SetBasicAuth(user, pass)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("MKCALENDAR %s: %v", path, err)
	}
	resp.Body.Close()
	// 201 created, or 405/409 already exists — all fine.
}

func putEvent(t *testing.T, c *caldav.Client, path, uid, summary string, start, end time.Time) {
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropProductID, "-//ftw-it//EN")
	cal.Props.SetText(ical.PropVersion, "2.0")
	ev := ical.NewEvent()
	ev.Props.SetText(ical.PropUID, uid)
	ev.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())
	ev.Props.SetDateTime(ical.PropDateTimeStart, start)
	ev.Props.SetDateTime(ical.PropDateTimeEnd, end)
	ev.Props.SetText(ical.PropSummary, summary)
	cal.Children = append(cal.Children, ev.Component)
	if _, err := c.PutCalendarObject(context.Background(), strings.TrimRight(path, "/")+"/"+uid+".ics", cal); err != nil {
		t.Fatalf("PUT %s: %v", uid, err)
	}
}

func TestCalDAVIntegration(t *testing.T) {
	url, user, pass := itEnv(t)
	energyPath := "/" + user + "/energy/"
	historyPath := "/" + user + "/history/"
	c := itClient(url, user, pass)

	mkcalendar(t, url, user, pass, energyPath)
	mkcalendar(t, url, user, pass, historyPath)

	now := time.Now()
	putEvent(t, c, energyPath, "it-away", "Away — IT", now.Add(1*time.Hour), now.Add(25*time.Hour))
	putEvent(t, c, energyPath, "it-ev", "Charge car 80%", now.Add(2*time.Hour), now.Add(3*time.Hour))

	// ---- Inbound: fetch + classify ----
	s := New(config.CalDAV{
		Enabled: true, URL: url, Username: user, Password: pass,
		CalendarPath: energyPath, HistoryPath: historyPath,
	}, &fakeLP{}, &fakeLM{}, "garage")

	intents, err := s.fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(intents.Away) != 1 {
		t.Fatalf("expected 1 away interval, got %d (%+v)", len(intents.Away), intents.Away)
	}
	if len(intents.EV) != 1 {
		t.Fatalf("expected 1 EV deadline, got %d (%+v)", len(intents.EV), intents.EV)
	}
	if intents.EV[0].TargetSoCPct != 80 {
		t.Fatalf("EV target: want 80, got %v", intents.EV[0].TargetSoCPct)
	}
	t.Logf("inbound OK: away=%+v ev=%+v", intents.Away[0], intents.EV[0])

	// ---- Outbound: write a session, then read it back raw ----
	sess := CompletedSession{ID: "easee", Start: now.Add(-2 * time.Hour), End: now.Add(-30 * time.Minute), EnergyWh: 12300}
	if err := s.writeSession(context.Background(), sess); err != nil {
		t.Fatalf("writeSession: %v", err)
	}

	objs, err := c.QueryCalendar(context.Background(), historyPath, &caldav.CalendarQuery{
		CompRequest: caldav.CalendarCompRequest{Name: "VCALENDAR", Comps: []caldav.CalendarCompRequest{{Name: "VEVENT", AllProps: true}}},
		CompFilter:  caldav.CompFilter{Name: "VCALENDAR", Comps: []caldav.CompFilter{{Name: "VEVENT"}}},
	})
	if err != nil {
		t.Fatalf("read back history: %v", err)
	}
	found := false
	for _, o := range objs {
		if o.Data == nil {
			continue
		}
		for _, ev := range o.Data.Events() {
			sum, _ := ev.Props.Text(ical.PropSummary)
			if strings.Contains(sum, "EV charged") {
				found = true
				t.Logf("outbound OK: history event %q", sum)
			}
		}
	}
	if !found {
		t.Fatalf("history event not found in %s", historyPath)
	}
}

func countEvents(t *testing.T, c *caldav.Client, path, substr string) int {
	objs, err := c.QueryCalendar(context.Background(), path, &caldav.CalendarQuery{
		CompRequest: caldav.CalendarCompRequest{Name: "VCALENDAR", Comps: []caldav.CalendarCompRequest{{Name: "VEVENT", AllProps: true}}},
		CompFilter:  caldav.CompFilter{Name: "VCALENDAR", Comps: []caldav.CompFilter{{Name: "VEVENT"}}},
	})
	if err != nil {
		t.Fatalf("query %s: %v", path, err)
	}
	n := 0
	for _, o := range objs {
		if o.Data == nil {
			continue
		}
		for _, ev := range o.Data.Events() {
			if sum, _ := ev.Props.Text(ical.PropSummary); strings.Contains(sum, substr) {
				n++
			}
		}
	}
	return n
}

func TestCalDAVPlanPublish(t *testing.T) {
	url, user, pass := itEnv(t)
	planPath := "/" + user + "/plan/"
	mkcalendar(t, url, user, pass, planPath)
	c := itClient(url, user, pass)
	now := time.Now()

	s := New(config.CalDAV{
		Enabled: true, URL: url, Username: user, Password: pass,
		CalendarPath: "/" + user + "/energy/", PlanPath: planPath,
	}, &fakeLP{}, &fakeLM{}, "garage")

	// Plan v1: a charge window in the near future (two consecutive slots).
	s.SetPlanSource(func() []PlanSlot {
		return []PlanSlot{
			{Start: now.Add(1 * time.Hour), End: now.Add(2 * time.Hour), BatteryW: 4000, SoCPct: 60},
			{Start: now.Add(2 * time.Hour), End: now.Add(3 * time.Hour), BatteryW: 4000, SoCPct: 80},
		}
	})
	s.publishPlan(context.Background())
	if n := countEvents(t, c, planPath, "Charge battery"); n != 1 {
		t.Fatalf("after v1: want 1 charge event, got %d", n)
	}
	t.Logf("plan publish OK: 1 charge window written")

	// Plan v2: the charge window is gone, replaced by a discharge window. The
	// reconcile must DELETE the stale charge event and PUT the discharge one.
	s.SetPlanSource(func() []PlanSlot {
		return []PlanSlot{
			{Start: now.Add(1 * time.Hour), End: now.Add(2 * time.Hour), BatteryW: -3000, SoCPct: 40},
		}
	})
	s.publishPlan(context.Background())
	if n := countEvents(t, c, planPath, "Charge battery"); n != 0 {
		t.Fatalf("after v2: stale charge event not deleted, %d remain", n)
	}
	if n := countEvents(t, c, planPath, "Discharge battery"); n != 1 {
		t.Fatalf("after v2: want 1 discharge event, got %d", n)
	}
	t.Logf("plan reconcile OK: stale charge deleted, discharge written")
}
