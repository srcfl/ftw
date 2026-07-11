package state

import (
	"database/sql"
	"fmt"
)

// CalDAVObject is one stored calendar object (.ics) for the native in-process
// CalDAV server (#498). Data is the raw iCalendar text.
type CalDAVObject struct {
	Path       string
	Collection string
	ETag       string
	Data       string
	ModifiedMs int64
}

// CalDAVCalendar is a stored calendar collection.
type CalDAVCalendar struct {
	Path        string
	Name        string
	Description string
}

// SaveCalDAVObject upserts one calendar object.
func (s *Store) SaveCalDAVObject(o CalDAVObject) error {
	if o.Path == "" {
		return fmt.Errorf("SaveCalDAVObject: empty path")
	}
	_, err := s.db.Exec(
		`INSERT INTO caldav_objects (path, collection, etag, data, modified_ms)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (path) DO UPDATE SET
		   collection=excluded.collection, etag=excluded.etag,
		   data=excluded.data, modified_ms=excluded.modified_ms`,
		o.Path, o.Collection, o.ETag, o.Data, o.ModifiedMs)
	return err
}

// GetCalDAVObject returns one object by path; ok=false when absent.
func (s *Store) GetCalDAVObject(path string) (CalDAVObject, bool, error) {
	row := s.db.QueryRow(
		`SELECT path, collection, etag, data, modified_ms FROM caldav_objects WHERE path = ?`, path)
	var o CalDAVObject
	err := row.Scan(&o.Path, &o.Collection, &o.ETag, &o.Data, &o.ModifiedMs)
	if err == sql.ErrNoRows {
		return CalDAVObject{}, false, nil
	}
	if err != nil {
		return CalDAVObject{}, false, err
	}
	return o, true, nil
}

// ListCalDAVObjects returns every object in a collection (indexed scan).
func (s *Store) ListCalDAVObjects(collection string) ([]CalDAVObject, error) {
	rows, err := s.db.Query(
		`SELECT path, collection, etag, data, modified_ms FROM caldav_objects WHERE collection = ?`, collection)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CalDAVObject
	for rows.Next() {
		var o CalDAVObject
		if err := rows.Scan(&o.Path, &o.Collection, &o.ETag, &o.Data, &o.ModifiedMs); err != nil {
			return out, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// DeleteCalDAVObject removes one object; nil even if it was already absent.
func (s *Store) DeleteCalDAVObject(path string) error {
	_, err := s.db.Exec(`DELETE FROM caldav_objects WHERE path = ?`, path)
	return err
}

// SaveCalDAVCalendar upserts a calendar collection.
func (s *Store) SaveCalDAVCalendar(c CalDAVCalendar) error {
	if c.Path == "" {
		return fmt.Errorf("SaveCalDAVCalendar: empty path")
	}
	_, err := s.db.Exec(
		`INSERT INTO caldav_calendars (path, name, description) VALUES (?, ?, ?)
		 ON CONFLICT (path) DO UPDATE SET name=excluded.name, description=excluded.description`,
		c.Path, c.Name, c.Description)
	return err
}

// ListCalDAVCalendars returns all calendar collections.
func (s *Store) ListCalDAVCalendars() ([]CalDAVCalendar, error) {
	rows, err := s.db.Query(`SELECT path, name, description FROM caldav_calendars`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CalDAVCalendar
	for rows.Next() {
		var c CalDAVCalendar
		if err := rows.Scan(&c.Path, &c.Name, &c.Description); err != nil {
			return out, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
