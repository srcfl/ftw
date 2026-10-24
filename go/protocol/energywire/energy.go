// Package energywire names the anonymous daily energy report: one closed UTC
// day of one home as six whole-kWh sums, and nothing that says whose.
//
// It travels alone through a relay (package reportwire), so the gateway never
// sees the box's address. It carries no id, version, driver, zone or size,
// because the daily fleet report (package fleetwire) carries those, and the
// two must never be joined into one home's record.
package energywire

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"
)

const Schema = "ftw.energy/1"

const (
	// MaxBodyBytes bounds the report itself, before any parsing.
	MaxBodyBytes = 256

	// MaxKWh bounds one field of one day. A home on a 3 × 63 A fuse moves
	// at most about 1,000 kWh a day.
	MaxKWh = 1000

	// AcceptDays is how many closed days back a report may name. A box sends
	// 1 to 12 hours after the day closes, so one day would do; three leave
	// room for a slow clock.
	AcceptDays = 3

	// MaxReports caps one date's reports, so junk can add bounded noise and
	// no more.
	MaxReports = 100_000
)

// Fields are the report's sums, in the order the public view lists them.
var Fields = []string{"pv_generation_kwh", "grid_import_kwh", "grid_export_kwh", "battery_charge_kwh", "battery_discharge_kwh", "vehicle_charge_kwh"}

// Report is one home's closed UTC day. Never add an identifier.
type Report struct {
	Schema           string `json:"schema"`
	Date             string `json:"date"`
	PVGeneration     int64  `json:"pv_generation_kwh"`
	GridImport       int64  `json:"grid_import_kwh"`
	GridExport       int64  `json:"grid_export_kwh"`
	BatteryCharge    int64  `json:"battery_charge_kwh"`
	BatteryDischarge int64  `json:"battery_discharge_kwh"`
	VehicleCharge    int64  `json:"vehicle_charge_kwh"`
}

// Sums are the report's fields in Fields order.
func (r Report) Sums() []int64 {
	return []int64{r.PVGeneration, r.GridImport, r.GridExport, r.BatteryCharge, r.BatteryDischarge, r.VehicleCharge}
}

// Valid checks the fields a sender fills. The date is checked against the
// receiver's clock in Decode.
func (r Report) Valid() bool {
	if r.Schema != Schema || !isDate(r.Date) {
		return false
	}
	for _, v := range r.Sums() {
		if v < 0 || v > MaxKWh {
			return false
		}
	}
	return true
}

var errBad = errors.New("energy: invalid report")

// Decode accepts only the bytes a box writes: the eight fields in their
// order, whole numbers in range, and a date in the last AcceptDays closed UTC
// days. Another order, spacing, a repeated or extra field, or a number
// written another way is refused.
func Decode(data []byte, now time.Time) (Report, error) {
	var r Report
	if len(data) > MaxBodyBytes || json.Unmarshal(data, &r) != nil || !r.Valid() || !Accepts(r.Date, now) {
		return Report{}, errBad
	}
	if canonical, err := json.Marshal(r); err != nil || !bytes.Equal(canonical, data) {
		return Report{}, errBad
	}
	return r, nil
}

// Accepts reports whether date is one of the last AcceptDays closed UTC days.
func Accepts(date string, now time.Time) bool {
	day, err := time.Parse(time.DateOnly, date)
	today := now.UTC().Truncate(24 * time.Hour)
	return err == nil && day.Before(today) && !day.Before(today.AddDate(0, 0, -AcceptDays))
}

// Closed reports whether no report may name date any more, so its totals are
// final.
func Closed(date string, now time.Time) bool {
	day, err := time.Parse(time.DateOnly, date)
	return err == nil && day.Before(now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -AcceptDays))
}

func isDate(s string) bool {
	t, err := time.Parse(time.DateOnly, s)
	return err == nil && t.Format(time.DateOnly) == s
}
