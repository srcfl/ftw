package state

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func sqlLookupByDriverName(t *testing.T, s *Store, name string) string {
	t.Helper()
	var id string
	err := s.db.QueryRow(`SELECT device_id FROM devices WHERE driver_name = ?
		ORDER BY last_seen_ms DESC, device_id ASC LIMIT 1`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func cacheLookupID(s *Store, name string) string {
	if d := s.LookupDeviceByDriverName(name); d != nil {
		return d.DeviceID
	}
	return ""
}

// The cache must give the same answer as the old SQL query across
// renames, swaps, re-registration and a reopen (loadDeviceCache).
func TestLookupDeviceByDriverNameCacheMatchesSQL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	reg := func(d Device) {
		t.Helper()
		if _, err := s.RegisterDevice(d); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Millisecond)
	}
	names := []string{"battery", "pv", "meter", "ghost"}
	check := func(stage string) {
		t.Helper()
		for _, n := range names {
			if got, want := cacheLookupID(s, n), sqlLookupByDriverName(t, s, n); got != want {
				t.Fatalf("%s: %s cache=%q sql=%q", stage, n, got, want)
			}
		}
	}
	reg(Device{DriverName: "battery", Make: "Sungrow", Serial: "A1"})
	reg(Device{DriverName: "pv", Make: "Fronius", Serial: "P1"})
	check("initial")
	// Hardware swap: a new battery behind the same driver name.
	reg(Device{DriverName: "battery", Make: "Sungrow", Serial: "B2"})
	check("swap")
	if cacheLookupID(s, "battery") != "sungrow:B2" {
		t.Fatalf("swap: newest device not chosen: %q", cacheLookupID(s, "battery"))
	}
	// Rename: the old device moves to a new driver name.
	reg(Device{DriverName: "meter", Make: "Sungrow", Serial: "A1"})
	check("rename")
	// Old device returns under its old name.
	reg(Device{DriverName: "battery", Make: "Sungrow", Serial: "A1"})
	check("return")
	// Re-register with only a MAC as a different id.
	reg(Device{DriverName: "pv", MAC: "aa:bb:cc:dd:ee:ff"})
	check("mac")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	check("reopen")
	// batteryModelKey still resolves through the cache after reopen.
	if k := s.batteryModelKey("battery"); k != "sungrow:A1" {
		t.Fatalf("batteryModelKey=%q", k)
	}
	if k := s.batteryModelKey("ghost"); k != "ghost" {
		t.Fatalf("batteryModelKey(ghost)=%q", k)
	}
}
