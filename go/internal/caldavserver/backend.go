package caldavserver

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	webdav "github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"

	"github.com/frahlg/forty-two-watts/go/internal/state"
)

// backend is a caldav.Backend persisting calendar objects through a Store
// (state.db when wired, in-memory otherwise). iCalendar (de)serialization +
// ETag computation live here; the Store only moves bytes.
type backend struct {
	principal string
	store     Store
	now       func() time.Time
}

func newBackend(principal string, calendarPaths []string, store Store) *backend {
	if store == nil {
		store = NewMemStore()
	}
	b := &backend{principal: principal, store: store, now: time.Now}
	for _, p := range calendarPaths {
		name := strings.Trim(strings.TrimPrefix(p, principal), "/")
		_ = b.store.SaveCalDAVCalendar(state.CalDAVCalendar{Path: p, Name: name})
	}
	return b
}

func (b *backend) CurrentUserPrincipal(ctx context.Context) (string, error) {
	return b.principal, nil
}

func (b *backend) CalendarHomeSetPath(ctx context.Context) (string, error) {
	return b.principal, nil
}

func toCalendar(c state.CalDAVCalendar) caldav.Calendar {
	return caldav.Calendar{
		Path:                  c.Path,
		Name:                  c.Name,
		Description:           c.Description,
		SupportedComponentSet: []string{ical.CompEvent},
	}
}

func (b *backend) ListCalendars(ctx context.Context) ([]caldav.Calendar, error) {
	cals, err := b.store.ListCalDAVCalendars()
	if err != nil {
		return nil, err
	}
	out := make([]caldav.Calendar, 0, len(cals))
	for _, c := range cals {
		out = append(out, toCalendar(c))
	}
	return out, nil
}

func (b *backend) GetCalendar(ctx context.Context, path string) (*caldav.Calendar, error) {
	cals, err := b.store.ListCalDAVCalendars()
	if err != nil {
		return nil, err
	}
	for _, c := range cals {
		if c.Path == path {
			cal := toCalendar(c)
			return &cal, nil
		}
	}
	return nil, webdav.NewHTTPError(http.StatusNotFound, fmt.Errorf("calendar not found: %s", path))
}

func (b *backend) CreateCalendar(ctx context.Context, cal *caldav.Calendar) error {
	return b.store.SaveCalDAVCalendar(state.CalDAVCalendar{
		Path: cal.Path, Name: cal.Name, Description: cal.Description,
	})
}

// parseObject decodes a stored row into a caldav.CalendarObject (Data parsed).
func parseObject(o state.CalDAVObject) (caldav.CalendarObject, error) {
	cal, err := ical.NewDecoder(strings.NewReader(o.Data)).Decode()
	if err != nil {
		return caldav.CalendarObject{}, err
	}
	return caldav.CalendarObject{
		Path:          o.Path,
		ETag:          o.ETag,
		ModTime:       time.UnixMilli(o.ModifiedMs),
		ContentLength: int64(len(o.Data)),
		Data:          cal,
	}, nil
}

func (b *backend) GetCalendarObject(ctx context.Context, path string, req *caldav.CalendarCompRequest) (*caldav.CalendarObject, error) {
	o, ok, err := b.store.GetCalDAVObject(path)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, webdav.NewHTTPError(http.StatusNotFound, fmt.Errorf("object not found: %s", path))
	}
	co, err := parseObject(o)
	if err != nil {
		return nil, err
	}
	return &co, nil
}

func (b *backend) ListCalendarObjects(ctx context.Context, path string, req *caldav.CalendarCompRequest) ([]caldav.CalendarObject, error) {
	rows, err := b.store.ListCalDAVObjects(path)
	if err != nil {
		return nil, err
	}
	out := make([]caldav.CalendarObject, 0, len(rows))
	for _, o := range rows {
		co, err := parseObject(o)
		if err != nil {
			continue // skip an unparseable row rather than failing the listing
		}
		out = append(out, co)
	}
	return out, nil
}

func (b *backend) QueryCalendarObjects(ctx context.Context, path string, query *caldav.CalendarQuery) ([]caldav.CalendarObject, error) {
	objs, err := b.ListCalendarObjects(ctx, path, &query.CompRequest)
	if err != nil {
		return nil, err
	}
	// caldav.Filter applies the component/property/time-range filters (it also
	// evaluates the recurrence set so a recurring master matches when any of its
	// instances falls in range).
	matched, err := caldav.Filter(query, objs)
	if err != nil {
		return nil, err
	}
	// RFC 4791 CALDAV:expand — turn each recurring master into the concrete
	// instances inside the requested window. Without this a "weekly away" event
	// would return only its first occurrence. go-webdav v0.7 drops the explicit
	// <expand> element, so the window comes from the comp-filter time-range that
	// the client sends alongside it. See expand.go.
	if exp := findExpand(query.CompRequest); exp != nil {
		matched = expandObjects(matched, exp.Start, exp.End)
	} else if start, end, ok := filterTimeRange(query.CompFilter); ok {
		matched = expandObjects(matched, start, end)
	}
	return matched, nil
}

func (b *backend) PutCalendarObject(ctx context.Context, path string, cal *ical.Calendar, opts *caldav.PutCalendarObjectOptions) (*caldav.CalendarObject, error) {
	var sb strings.Builder
	if err := ical.NewEncoder(&sb).Encode(cal); err != nil {
		return nil, err
	}
	data := sb.String()
	sum := sha1.Sum([]byte(data))
	collection := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		collection = path[:i+1]
	}
	// Auto-create the parent collection if a client PUTs without MKCALENDAR.
	_ = b.store.SaveCalDAVCalendar(state.CalDAVCalendar{
		Path: collection,
		Name: strings.Trim(strings.TrimPrefix(collection, b.principal), "/"),
	})
	row := state.CalDAVObject{
		Path:       path,
		Collection: collection,
		ETag:       hex.EncodeToString(sum[:]),
		Data:       data,
		ModifiedMs: b.now().UnixMilli(),
	}
	if err := b.store.SaveCalDAVObject(row); err != nil {
		return nil, err
	}
	return &caldav.CalendarObject{
		Path:          path,
		ETag:          row.ETag,
		ModTime:       time.UnixMilli(row.ModifiedMs),
		ContentLength: int64(len(data)),
		Data:          cal,
	}, nil
}

func (b *backend) DeleteCalendarObject(ctx context.Context, path string) error {
	_, ok, err := b.store.GetCalDAVObject(path)
	if err != nil {
		return err
	}
	if !ok {
		return webdav.NewHTTPError(http.StatusNotFound, fmt.Errorf("object not found: %s", path))
	}
	return b.store.DeleteCalDAVObject(path)
}
