package state

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
)

const configurationKey = "settings/config_v1"

var ErrConfigurationConflict = errors.New("settings changed; reload them before saving")

type Configuration struct {
	Version  int             `json:"version"`
	Revision int64           `json:"revision"`
	Document json.RawMessage `json:"document"`
}

func decodeConfiguration(raw string) (Configuration, error) {
	var c Configuration
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return c, fmt.Errorf("read settings: %w", err)
	}
	if c.Version != 1 || c.Revision < 1 || !json.Valid(c.Document) {
		return c, errors.New("unsupported or invalid settings document")
	}
	return c, nil
}

// ReadConfiguration opens the existing database without creating, migrating or
// healing it. A missing or unreadable authority must never fall back to YAML.
func ReadConfiguration(path string) (Configuration, error) {
	db, err := sql.Open("sqlite", readOnlyDatabaseURI(path))
	if err != nil {
		return Configuration{}, err
	}
	defer db.Close()
	var raw string
	if err := db.QueryRow(`SELECT value FROM config WHERE key = ?`, configurationKey).Scan(&raw); err != nil {
		return Configuration{}, fmt.Errorf("read stored settings: %w", err)
	}
	return decodeConfiguration(raw)
}

func readOnlyDatabaseURI(path string) string {
	path = filepath.ToSlash(path)
	if strings.HasPrefix(path, "//?/UNC/") {
		path = "//" + strings.TrimPrefix(path, "//?/UNC/")
	} else {
		path = strings.TrimPrefix(path, "//?/")
	}
	// A Windows drive is part of the URI path, never its authority.
	if len(path) > 1 && path[1] == ':' {
		path = "/" + path
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}
	return u.String()
}

func (s *Store) Configuration() (Configuration, bool, error) {
	raw, found, err := s.ConfigValue(configurationKey)
	if err != nil || !found {
		return Configuration{}, found, err
	}
	c, err := decodeConfiguration(raw)
	return c, true, err
}

// ConfigValue distinguishes a missing legacy key from a failed read.
func (s *Store) ConfigValue(key string) (string, bool, error) {
	var raw string
	err := s.db.QueryRow(`SELECT value FROM config WHERE key = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return raw, err == nil, err
}

// durableConfigWrite gives only this writer a FULL synchronous connection.
// History keeps its existing policy. FULL syncs the WAL before acknowledging
// settings, rather than waiting for a later checkpoint.
func (s *Store) durableConfigWrite(write func(*sql.Tx) error) error {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA synchronous=FULL`); err != nil {
		return err
	}
	defer func() {
		if _, err := conn.ExecContext(ctx, `PRAGMA synchronous=NORMAL`); err != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := write(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func saveConfigValues(tx *sql.Tx, values map[string]string) error {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, err := tx.Exec(`INSERT INTO config (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, values[key]); err != nil {
			return err
		}
	}
	return nil
}

// SaveConfigValues commits related settings together before callers publish
// them to the running system. It leaves runtime/model keys alone.
func (s *Store) SaveConfigValues(values map[string]string) error {
	return s.durableConfigWrite(func(tx *sql.Tx) error { return saveConfigValues(tx, values) })
}

// SaveConfiguration compares the revision and commits the complete document
// with its legacy credential rows. Revision zero means first import only.
func (s *Store) SaveConfiguration(document []byte, expected int64, credentials map[string]string) (int64, error) {
	if !json.Valid(document) {
		return 0, errors.New("invalid settings JSON")
	}
	next := expected + 1
	err := s.durableConfigWrite(func(tx *sql.Tx) error {
		var raw string
		err := tx.QueryRow(`SELECT value FROM config WHERE key = ?`, configurationKey).Scan(&raw)
		var actual int64
		if err == nil {
			current, err := decodeConfiguration(raw)
			if err != nil {
				return err
			}
			actual = current.Revision
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if actual != expected {
			return ErrConfigurationConflict
		}
		encoded, err := json.Marshal(Configuration{Version: 1, Revision: next, Document: document})
		if err != nil {
			return err
		}
		values := make(map[string]string, len(credentials)+1)
		for k, v := range credentials {
			values[k] = v
		}
		values[configurationKey] = string(encoded)
		return saveConfigValues(tx, values)
	})
	if err != nil {
		return 0, err
	}
	return next, nil
}

// SavePlannerPreferences changes an existing planner mode in the same
// transaction. A concurrently saved manual mode keeps its place.
func (s *Store) SavePlannerPreferences(values map[string]string, plannerModes []string, mapped string) error {
	return s.durableConfigWrite(func(tx *sql.Tx) error {
		if err := saveConfigValues(tx, values); err != nil {
			return err
		}
		if len(plannerModes) == 0 {
			return nil
		}
		args := []any{mapped}
		for _, mode := range plannerModes {
			args = append(args, mode)
		}
		_, err := tx.Exec(`UPDATE config SET value = ? WHERE key = 'mode' AND value IN (`+strings.TrimSuffix(strings.Repeat("?,", len(plannerModes)), ",")+`)`, args...)
		return err
	})
}
