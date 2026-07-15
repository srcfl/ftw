package caldavserver

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/emersion/go-ical"

	"github.com/srcfl/ftw/go/internal/state"
)

const feedObj = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//ftw-test//EN
BEGIN:VEVENT
UID:%s
DTSTAMP:20260101T000000Z
DTSTART:20260101T120000Z
DTEND:20260101T130000Z
SUMMARY:%s
END:VEVENT
END:VCALENDAR
`

// feedFixture returns a server handler whose /u/plan/ collection holds two
// objects, exposed as the read-only feed /feed/plan.ics.
func feedFixture(t *testing.T) http.Handler {
	t.Helper()
	store := NewMemStore()
	for _, e := range []struct{ uid, sum string }{{"plan-1", "Charge window"}, {"plan-2", "Discharge window"}} {
		if err := store.SaveCalDAVObject(state.CalDAVObject{
			Path:       "/u/plan/" + e.uid + ".ics",
			Collection: "/u/plan/",
			Data:       fmt.Sprintf(feedObj, e.uid, e.sum),
		}); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	return NewHandler("u", "p", "/u/", []string{"/u/plan/"}, store, WithFeeds(map[string]string{"plan": "/u/plan/"}))
}

func TestFeedAggregatesCollection(t *testing.T) {
	srv := httptest.NewServer(feedFixture(t))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/feed/plan.ics", nil)
	req.SetBasicAuth("u", "p")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/calendar") {
		t.Fatalf("content-type = %q, want text/calendar", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	cal, err := ical.NewDecoder(strings.NewReader(string(body))).Decode()
	if err != nil {
		t.Fatalf("decode feed: %v", err)
	}
	var events int
	for _, c := range cal.Children {
		if c.Name == ical.CompEvent {
			events++
		}
	}
	if events != 2 {
		t.Fatalf("feed has %d VEVENTs, want 2", events)
	}
}

func TestFeedRequiresAuth(t *testing.T) {
	srv := httptest.NewServer(feedFixture(t))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/feed/plan.ics")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without auth", resp.StatusCode)
	}
}

func TestFeedUnknownNameIs404(t *testing.T) {
	srv := httptest.NewServer(feedFixture(t))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/feed/energy.ics", nil)
	req.SetBasicAuth("u", "p")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an unmapped feed", resp.StatusCode)
	}
}
