package state

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Device is one hardware instance bound to a driver in config. The
// device_id is hardware-stable so persistent state (battery_models, RLS
// twin, calibration history) survives renames and re-adds.
type Device struct {
	DeviceID    string
	DriverName  string
	Make        string
	Serial      string
	MAC         string
	Endpoint    string
	FirstSeenMs int64
	LastSeenMs  int64
}

// ResolveDeviceID computes the canonical device_id from the bits of
// identity we know. Priority:
//  1. make + ":" + serial   (hardware-issued, never collides)
//  2. "mac:" + mac          (L2-stable for TCP devices)
//  3. "ep:" + endpoint      (only stable as long as the host/IP doesn't change)
//
// All inputs are lowercased + colons-stripped from the MAC for stability.
func ResolveDeviceID(make, serial, mac, endpoint string) string {
	make = strings.ToLower(strings.TrimSpace(make))
	serial = strings.TrimSpace(serial)
	mac = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(mac)), ":", "")
	if make != "" && serial != "" {
		return make + ":" + serial
	}
	if mac != "" {
		return "mac:" + mac
	}
	if endpoint != "" {
		return "ep:" + endpoint
	}
	return ""
}

// RegisterDevice records or updates a device. If the device_id already
// exists, the row's last_seen_ms is bumped + driver_name/make/serial/mac
// are refreshed (so renames and protocol-detected SN updates are reflected).
// Returns the canonical device_id (non-empty on success).
func (s *Store) RegisterDevice(d Device) (string, error) {
	if d.DeviceID == "" {
		d.DeviceID = ResolveDeviceID(d.Make, d.Serial, d.MAC, d.Endpoint)
	}
	if d.DeviceID == "" {
		return "", errors.New("RegisterDevice: cannot resolve device_id (no make+sn, no mac, no endpoint)")
	}
	now := time.Now().UnixMilli()
	if d.FirstSeenMs == 0 {
		d.FirstSeenMs = now
	}
	d.LastSeenMs = now
	_, err := s.db.Exec(`
		INSERT INTO devices (device_id, driver_name, make, serial, mac, endpoint, first_seen_ms, last_seen_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (device_id) DO UPDATE SET
			driver_name  = excluded.driver_name,
			make         = COALESCE(NULLIF(excluded.make, ''), devices.make),
			serial       = COALESCE(NULLIF(excluded.serial, ''), devices.serial),
			mac          = COALESCE(NULLIF(excluded.mac, ''), devices.mac),
			endpoint     = COALESCE(NULLIF(excluded.endpoint, ''), devices.endpoint),
			last_seen_ms = excluded.last_seen_ms`,
		d.DeviceID, d.DriverName, d.Make, d.Serial, d.MAC, d.Endpoint, d.FirstSeenMs, d.LastSeenMs)
	if err != nil {
		return "", fmt.Errorf("upsert device: %w", err)
	}
	return d.DeviceID, nil
}

// LookupDeviceByDriverName finds the most recently-seen device bound to a
// given driver name. Returns nil if no device has been registered yet
// (cold start before the driver has reported its identity).
func (s *Store) LookupDeviceByDriverName(name string) *Device {
	row := s.db.QueryRow(`SELECT device_id, driver_name,
		COALESCE(make,''), COALESCE(serial,''), COALESCE(mac,''), COALESCE(endpoint,''),
		first_seen_ms, last_seen_ms
		FROM devices WHERE driver_name = ? ORDER BY last_seen_ms DESC LIMIT 1`, name)
	var d Device
	if err := row.Scan(&d.DeviceID, &d.DriverName, &d.Make, &d.Serial, &d.MAC, &d.Endpoint,
		&d.FirstSeenMs, &d.LastSeenMs); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return nil
	}
	return &d
}

// AllDevices returns every registered device, most recently seen first.
func (s *Store) AllDevices() ([]Device, error) {
	rows, err := s.db.Query(`SELECT device_id, driver_name,
		COALESCE(make,''), COALESCE(serial,''), COALESCE(mac,''), COALESCE(endpoint,''),
		first_seen_ms, last_seen_ms
		FROM devices ORDER BY last_seen_ms DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Device, 0)
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.DeviceID, &d.DriverName, &d.Make, &d.Serial, &d.MAC, &d.Endpoint,
			&d.FirstSeenMs, &d.LastSeenMs); err != nil {
			return out, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// MigrateBatteryModelKeys renames any battery_models rows whose key is a
// driver_name (legacy) but where we now know a device_id for that name.
// Idempotent: rows already keyed on device_id are left alone.
//
// Returns the count of migrated rows.
func (s *Store) MigrateBatteryModelKeys() (int, error) {
	rows, err := s.db.Query(`SELECT name, json FROM battery_models`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	type entry struct{ name, js string }
	var all []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.name, &e.js); err != nil {
			return 0, err
		}
		all = append(all, e)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	migrated := 0
	for _, e := range all {
		// Skip if the key already looks like a device_id (contains ":")
		if strings.Contains(e.name, ":") {
			continue
		}
		dev := s.LookupDeviceByDriverName(e.name)
		if dev == nil {
			continue
		}
		// Insert under new key, delete old. INSERT OR IGNORE so we don't
		// clobber existing well-trained data on the device_id side.
		if _, err := s.db.Exec(
			`INSERT OR IGNORE INTO battery_models (name, json) VALUES (?, ?)`,
			dev.DeviceID, e.js); err != nil {
			return migrated, err
		}
		if _, err := s.db.Exec(`DELETE FROM battery_models WHERE name = ?`, e.name); err != nil {
			return migrated, err
		}
		migrated++
	}
	return migrated, nil
}
