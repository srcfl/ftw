package caldavserver

import (
	"io"
	"net/http"
	"strings"

	"github.com/emersion/go-ical"

	"github.com/frahlg/forty-two-watts/go/internal/state"
)

// feedHandler serves read-only, aggregated iCalendar feeds for selected
// collections (the plan + EVSE-history calendars), so a phone can subscribe to
// them in one tap via a webcal:// link. The CalDAV protocol (go-webdav) only
// answers GET for an individual object; a calendar *subscription* needs the
// whole collection as a single text/calendar document, which this provides.
//
// feeds maps a short feed name (the URL is /feed/<name>.ics) to the collection
// path whose objects are merged. Only read-only collections are exposed here —
// never the read-write "energy" collection 42W reads inbound intents from.
// The handler is mounted behind the same Basic auth as the rest of the server,
// so the webcal:// link carries the managed credential.
type feedHandler struct {
	feeds map[string]string // name -> collection path
	store Store
}

func newFeedHandler(feeds map[string]string, store Store) http.Handler {
	return &feedHandler{feeds: feeds, store: store}
}

func (h *feedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/feed/"), ".ics")
	collection, ok := h.feeds[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	objs, err := h.store.ListCalDAVObjects(collection)
	if err != nil {
		http.Error(w, "feed unavailable", http.StatusInternalServerError)
		return
	}
	body, err := encodeFeed(objs)
	if err != nil {
		http.Error(w, "feed encode failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.WriteString(w, body)
}

// emptyCalendar is a valid, empty VCALENDAR. go-ical refuses to encode a
// calendar with no components, but an empty subscription feed (no plan/history
// events yet) is a legitimate state a client must still be able to fetch.
const emptyCalendar = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//forty-two-watts//CalDAV feed//EN\r\nEND:VCALENDAR\r\n"

// encodeFeed serializes the collection's events into one text/calendar body.
func encodeFeed(objs []state.CalDAVObject) (string, error) {
	feed := mergeFeed(objs)
	if len(feed.Children) == 0 {
		return emptyCalendar, nil
	}
	var sb strings.Builder
	if err := ical.NewEncoder(&sb).Encode(feed); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// mergeFeed flattens the VEVENTs from every stored object in a collection into
// one VCALENDAR. An object that fails to parse is skipped — one bad object must
// not take down the whole feed.
func mergeFeed(objs []state.CalDAVObject) *ical.Calendar {
	out := ical.NewCalendar()
	out.Props.SetText(ical.PropVersion, "2.0")
	out.Props.SetText(ical.PropProductID, "-//forty-two-watts//CalDAV feed//EN")
	for _, o := range objs {
		cal, err := ical.NewDecoder(strings.NewReader(o.Data)).Decode()
		if err != nil {
			continue
		}
		for _, child := range cal.Children {
			if child.Name == ical.CompEvent {
				out.Children = append(out.Children, child)
			}
		}
	}
	return out
}
