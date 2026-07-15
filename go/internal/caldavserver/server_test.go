package caldavserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	webdav "github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"

	"github.com/srcfl/ftw/go/internal/state"
)

func testHandler(user, pass, principal string, cals []string) http.Handler {
	return NewHandler(user, pass, principal, cals, NewMemStore())
}

func query() *caldav.CalendarQuery {
	return &caldav.CalendarQuery{
		CompRequest: caldav.CalendarCompRequest{Name: "VCALENDAR", Comps: []caldav.CalendarCompRequest{{Name: "VEVENT", AllProps: true}}},
		CompFilter:  caldav.CompFilter{Name: "VCALENDAR", Comps: []caldav.CompFilter{{Name: "VEVENT"}}},
	}
}

func putEvent(t *testing.T, c *caldav.Client, path, uid, summary string, start, end time.Time) {
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
	if _, err := c.PutCalendarObject(context.Background(), path, cal); err != nil {
		t.Fatalf("PUT %s: %v", path, err)
	}
}

// TestNativeServerRoundTrip drives the in-process CalDAV server with FTW's own
// go-webdav client: PUT an event, read it back via a calendar-query REPORT,
// then DELETE it. This is exactly the inbound/outbound path the calendar
// service uses.
func TestNativeServerRoundTrip(t *testing.T) {
	srv := httptest.NewServer(testHandler("u", "p", "/u/", []string{"/u/energy/"}))
	defer srv.Close()
	hc := webdav.HTTPClientWithBasicAuth(http.DefaultClient, "u", "p")
	c, err := caldav.NewClient(hc, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()

	putEvent(t, c, "/u/energy/away.ics", "away", "Away — test", now.Add(time.Hour), now.Add(25*time.Hour))

	objs, err := c.QueryCalendar(context.Background(), "/u/energy/", query())
	if err != nil {
		t.Fatalf("REPORT: %v", err)
	}
	if len(objs) != 1 || objs[0].Data == nil || len(objs[0].Data.Events()) != 1 {
		t.Fatalf("expected 1 event, got %d objects", len(objs))
	}
	if sum, _ := objs[0].Data.Events()[0].Props.Text(ical.PropSummary); sum != "Away — test" {
		t.Fatalf("summary round-trip wrong: %q", sum)
	}

	// DELETE via plain WebDAV (the caldav client has no delete) — same path the
	// plan reconciler uses.
	wc, _ := webdav.NewClient(hc, srv.URL)
	if err := wc.RemoveAll(context.Background(), "/u/energy/away.ics"); err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	objs2, err := c.QueryCalendar(context.Background(), "/u/energy/", query())
	if err != nil {
		t.Fatalf("REPORT after delete: %v", err)
	}
	if len(objs2) != 0 {
		t.Fatalf("expected 0 events after delete, got %d", len(objs2))
	}
}

// TestNativeServerPersistsAcrossRestart proves durability with the state.db
// backend: write an event, close the DB, reopen it (a "restart"), and confirm
// the event is still served.
func TestNativeServerPersistsAcrossRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	st, err := state.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewHandler("u", "p", "/u/", []string{"/u/energy/"}, st))
	hc := webdav.HTTPClientWithBasicAuth(http.DefaultClient, "u", "p")
	c, _ := caldav.NewClient(hc, srv.URL)
	now := time.Now()
	putEvent(t, c, "/u/energy/away.ics", "away", "Away — persisted", now.Add(time.Hour), now.Add(2*time.Hour))
	srv.Close()
	st.Close()

	// "Restart": reopen the same DB and stand the server back up.
	st2, err := state.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	srv2 := httptest.NewServer(NewHandler("u", "p", "/u/", []string{"/u/energy/"}, st2))
	defer srv2.Close()
	c2, _ := caldav.NewClient(hc, srv2.URL)

	objs, err := c2.QueryCalendar(context.Background(), "/u/energy/", query())
	if err != nil {
		t.Fatalf("REPORT after restart: %v", err)
	}
	if len(objs) != 1 {
		t.Fatalf("event did not survive restart: got %d objects", len(objs))
	}
	if sum, _ := objs[0].Data.Events()[0].Props.Text(ical.PropSummary); sum != "Away — persisted" {
		t.Fatalf("persisted summary wrong: %q", sum)
	}
}

func TestNativeServerAuthRejectsBadCreds(t *testing.T) {
	srv := httptest.NewServer(testHandler("u", "p", "/u/", []string{"/u/energy/"}))
	defer srv.Close()
	req, _ := http.NewRequest("PROPFIND", srv.URL+"/u/energy/", nil)
	req.SetBasicAuth("u", "wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad creds: want 401, got %d", resp.StatusCode)
	}
}

func TestNativeServerEmptyPasswordFailsClosed(t *testing.T) {
	srv := httptest.NewServer(testHandler("u", "", "/u/", []string{"/u/energy/"}))
	defer srv.Close()
	req, _ := http.NewRequest("PROPFIND", srv.URL+"/u/energy/", nil)
	req.SetBasicAuth("u", "")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("empty configured password must reject all (fail-closed), got %d", resp.StatusCode)
	}
}
