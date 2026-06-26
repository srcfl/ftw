package calendar

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	webdav "github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"

	"github.com/frahlg/forty-two-watts/go/internal/caldavserver"
	"github.com/frahlg/forty-two-watts/go/internal/config"
)

// TestCalendarServiceAgainstNativeServer is the end-to-end proof that the
// native in-process CalDAV server (#498 prototype) is a drop-in for Radicale:
// a real calendar.Service fetches and parses intents straight from it, with no
// Radicale anywhere. CI-safe (everything in-process).
func TestCalendarServiceAgainstNativeServer(t *testing.T) {
	srv := httptest.NewServer(caldavserver.NewHandler("u", "p", "/u/", []string{"/u/energy/"}, caldavserver.NewMemStore()))
	defer srv.Close()

	// A calendar app would PUT this; we do it with the same client 42W uses.
	hc := webdav.HTTPClientWithBasicAuth(http.DefaultClient, "u", "p")
	c, err := caldav.NewClient(hc, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropProductID, "-//ftw-test//EN")
	cal.Props.SetText(ical.PropVersion, "2.0")
	ev := ical.NewEvent()
	ev.Props.SetText(ical.PropUID, "away1")
	ev.Props.SetDateTime(ical.PropDateTimeStamp, now.UTC())
	ev.Props.SetDateTime(ical.PropDateTimeStart, now.Add(time.Hour))
	ev.Props.SetDateTime(ical.PropDateTimeEnd, now.Add(25*time.Hour))
	ev.Props.SetText(ical.PropSummary, "Vacation")
	cal.Children = append(cal.Children, ev.Component)
	if _, err := c.PutCalendarObject(context.Background(), "/u/energy/away1.ics", cal); err != nil {
		t.Fatalf("seed event: %v", err)
	}

	// The real calendar service, pointed at the native server, parses it.
	s := New(config.CalDAV{
		Enabled: true, URL: srv.URL, Username: "u", Password: "p",
		CalendarPath: "/u/energy/",
	}, &fakeLP{}, &fakeLM{}, "garage")

	intents, err := s.fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch from native server: %v", err)
	}
	if len(intents.Away) != 1 {
		t.Fatalf("expected 1 away interval from native server, got %d", len(intents.Away))
	}
	if intents.Away[0].Title != "Vacation" {
		t.Fatalf("title round-trip wrong: %q", intents.Away[0].Title)
	}
}
