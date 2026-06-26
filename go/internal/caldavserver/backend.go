package caldavserver

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-ical"
	webdav "github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"
)

// memBackend is an in-memory caldav.Backend for the native CalDAV server.
//
// PROTOTYPE: storage is in-memory, so calendar objects are lost on restart.
// TODO(#498): back this with state.db (a caldav_objects table) for durability
// before this is more than a proof-of-concept. The interface boundary
// (caldav.Backend) is the same, so swapping the store is local to this file.
type memBackend struct {
	principal string
	now       func() time.Time

	mu        sync.RWMutex
	calendars map[string]*caldav.Calendar       // collection path -> calendar
	objects   map[string]*caldav.CalendarObject // full object path -> object
}

func newMemBackend(principal string, calendarPaths []string) *memBackend {
	b := &memBackend{
		principal: principal,
		now:       time.Now,
		calendars: map[string]*caldav.Calendar{},
		objects:   map[string]*caldav.CalendarObject{},
	}
	for _, p := range calendarPaths {
		b.calendars[p] = &caldav.Calendar{
			Path:                  p,
			Name:                  strings.Trim(strings.TrimPrefix(p, principal), "/"),
			SupportedComponentSet: []string{ical.CompEvent},
		}
	}
	return b
}

func (b *memBackend) CurrentUserPrincipal(ctx context.Context) (string, error) {
	return b.principal, nil
}

func (b *memBackend) CalendarHomeSetPath(ctx context.Context) (string, error) {
	return b.principal, nil
}

func (b *memBackend) ListCalendars(ctx context.Context) ([]caldav.Calendar, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]caldav.Calendar, 0, len(b.calendars))
	for _, c := range b.calendars {
		out = append(out, *c)
	}
	return out, nil
}

func (b *memBackend) GetCalendar(ctx context.Context, path string) (*caldav.Calendar, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if c, ok := b.calendars[path]; ok {
		cp := *c
		return &cp, nil
	}
	return nil, webdav.NewHTTPError(http.StatusNotFound, fmt.Errorf("calendar not found: %s", path))
}

func (b *memBackend) CreateCalendar(ctx context.Context, cal *caldav.Calendar) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := *cal
	if c.SupportedComponentSet == nil {
		c.SupportedComponentSet = []string{ical.CompEvent}
	}
	b.calendars[cal.Path] = &c
	return nil
}

func (b *memBackend) GetCalendarObject(ctx context.Context, path string, req *caldav.CalendarCompRequest) (*caldav.CalendarObject, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if o, ok := b.objects[path]; ok {
		cp := *o
		return &cp, nil
	}
	return nil, webdav.NewHTTPError(http.StatusNotFound, fmt.Errorf("object not found: %s", path))
}

func (b *memBackend) ListCalendarObjects(ctx context.Context, path string, req *caldav.CalendarCompRequest) ([]caldav.CalendarObject, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.objectsIn(path), nil
}

func (b *memBackend) QueryCalendarObjects(ctx context.Context, path string, query *caldav.CalendarQuery) ([]caldav.CalendarObject, error) {
	b.mu.RLock()
	objs := b.objectsIn(path)
	b.mu.RUnlock()
	// caldav.Filter applies the query's component/property/time-range filters.
	// NB: recurrence Expand is not performed here (prototype) — recurring
	// events return as their master VEVENT.
	return caldav.Filter(query, objs)
}

// objectsIn returns a copy of every object whose path is under calPath. Caller
// holds at least RLock.
func (b *memBackend) objectsIn(calPath string) []caldav.CalendarObject {
	var out []caldav.CalendarObject
	for p, o := range b.objects {
		if p != calPath && strings.HasPrefix(p, calPath) {
			out = append(out, *o)
		}
	}
	return out
}

func (b *memBackend) PutCalendarObject(ctx context.Context, path string, cal *ical.Calendar, opts *caldav.PutCalendarObjectOptions) (*caldav.CalendarObject, error) {
	var sb strings.Builder
	if err := ical.NewEncoder(&sb).Encode(cal); err != nil {
		return nil, err
	}
	data := sb.String()
	sum := sha1.Sum([]byte(data))
	obj := &caldav.CalendarObject{
		Path:          path,
		ModTime:       b.now(),
		ContentLength: int64(len(data)),
		ETag:          hex.EncodeToString(sum[:]),
		Data:          cal,
	}
	b.mu.Lock()
	// Auto-create the parent collection if a client PUTs without MKCALENDAR
	// (mirrors Radicale). The collection path is the object path up to the
	// last '/'.
	if i := strings.LastIndex(path, "/"); i >= 0 {
		calPath := path[:i+1]
		if _, ok := b.calendars[calPath]; !ok {
			b.calendars[calPath] = &caldav.Calendar{Path: calPath, SupportedComponentSet: []string{ical.CompEvent}}
		}
	}
	b.objects[path] = obj
	b.mu.Unlock()
	cp := *obj
	return &cp, nil
}

func (b *memBackend) DeleteCalendarObject(ctx context.Context, path string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.objects[path]; !ok {
		return webdav.NewHTTPError(http.StatusNotFound, fmt.Errorf("object not found: %s", path))
	}
	delete(b.objects, path)
	return nil
}
