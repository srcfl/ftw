package caldavserver

import (
	"sync"

	"github.com/srcfl/ftw/go/internal/state"
)

// Store is the persistence the native CalDAV server needs. *state.Store
// satisfies it (durable, in state.db); NewMemStore is an in-memory impl for
// tests and the no-database fallback.
type Store interface {
	SaveCalDAVObject(o state.CalDAVObject) error
	GetCalDAVObject(path string) (state.CalDAVObject, bool, error)
	ListCalDAVObjects(collection string) ([]state.CalDAVObject, error)
	DeleteCalDAVObject(path string) error
	SaveCalDAVCalendar(c state.CalDAVCalendar) error
	ListCalDAVCalendars() ([]state.CalDAVCalendar, error)
}

// memStore is an in-memory Store (non-durable). Used by tests and when no
// state DB is wired.
type memStore struct {
	mu   sync.RWMutex
	objs map[string]state.CalDAVObject
	cals map[string]state.CalDAVCalendar
}

// NewMemStore returns a non-persistent in-memory Store.
func NewMemStore() Store {
	return &memStore{
		objs: map[string]state.CalDAVObject{},
		cals: map[string]state.CalDAVCalendar{},
	}
}

func (m *memStore) SaveCalDAVObject(o state.CalDAVObject) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[o.Path] = o
	return nil
}

func (m *memStore) GetCalDAVObject(path string) (state.CalDAVObject, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	o, ok := m.objs[path]
	return o, ok, nil
}

func (m *memStore) ListCalDAVObjects(collection string) ([]state.CalDAVObject, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []state.CalDAVObject
	for _, o := range m.objs {
		if o.Collection == collection {
			out = append(out, o)
		}
	}
	return out, nil
}

func (m *memStore) DeleteCalDAVObject(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, path)
	return nil
}

func (m *memStore) SaveCalDAVCalendar(c state.CalDAVCalendar) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cals[c.Path] = c
	return nil
}

func (m *memStore) ListCalDAVCalendars() ([]state.CalDAVCalendar, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]state.CalDAVCalendar, 0, len(m.cals))
	for _, c := range m.cals {
		out = append(out, c)
	}
	return out, nil
}
