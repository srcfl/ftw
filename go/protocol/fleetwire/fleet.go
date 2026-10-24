// Package fleetwire names the anonymous daily report, without a home id.
package fleetwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
)

// Schema is the report a box sends: the shape of the box and how it ran
// lately. SchemaV1, the shape alone, is still accepted from older boxes.
const Schema = "ftw.fleet/2"
const SchemaV1 = "ftw.fleet/1"
const StatsSchema = "ftw.fleet-stats/1"
const Meaning = "reports, not unique boxes"
const MaxBodyBytes = 4096
const MaxDays = 90
const MaxLabels = 128
const MaxReports = 100_000

// Report carries only shared labels and buckets. Never add an identifier.
type Report struct {
	Schema  string   `json:"schema"`
	Version string   `json:"ftw_version"`
	Channel string   `json:"channel"`
	Drivers []string `json:"drivers"`
	Battery string   `json:"battery_kwh"`
	Zone    string   `json:"price_zone"`
	Age     string   `json:"install_age"`
	// How the box ran lately, in Schema only: the slowest plan of the last
	// 24 hours, how many plans fell back, the share of driver polls that
	// failed, and the newest update of the last 7 days.
	Solve     string `json:"plan_solve,omitempty"`
	Fallbacks string `json:"plan_fallbacks,omitempty"`
	Errors    string `json:"driver_errors,omitempty"`
	Update    string `json:"update,omitempty"`
}

// The health buckets, each a few values wide.
var (
	SolveBuckets    = []string{"none", "0-1s", "1-5s", "5s+"}
	FallbackBuckets = []string{"none", "0", "1-3", "4+"}
	ErrorBuckets    = []string{"none", "0", "0-1%", "1%+"}
	UpdateBuckets   = []string{"none", "ok", "failed"}
)

var release = regexp.MustCompile(`^v[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,4}(-beta\.[0-9]{1,4})?$`)
var driver = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
var zone = regexp.MustCompile(`^[A-Z0-9-]{2,16}$`)

// Decode refuses extra fields, so a sender cannot add a household record.
// A SchemaV1 report has the seven shape fields; a Schema report has the
// four health fields too.
func Decode(data []byte) (Report, error) {
	bad := errors.New("fleet: invalid report")
	var r Report
	if len(data) > MaxBodyBytes {
		return r, bad
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || bytes.Equal(bytes.TrimSpace(fields["drivers"]), []byte("null")) {
		return r, bad
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&r) != nil || (r.Version != "unknown" && !release.MatchString(r.Version)) {
		return r, bad
	}
	switch {
	case r.Schema == SchemaV1 && len(fields) == 7:
	case r.Schema == Schema && len(fields) == 11 && slices.Contains(SolveBuckets, r.Solve) && slices.Contains(FallbackBuckets, r.Fallbacks) &&
		slices.Contains(ErrorBuckets, r.Errors) && slices.Contains(UpdateBuckets, r.Update):
	default:
		return r, bad
	}
	channel := "stable"
	if r.Version == "unknown" {
		channel = "unknown"
	} else if strings.Contains(r.Version, "-beta.") {
		channel = "beta"
	}
	if r.Channel != channel || len(r.Drivers) > 16 || !slices.Contains([]string{"none", "0-5", "5-15", "15-30", "30+"}, r.Battery) ||
		(r.Zone != "unknown" && !zone.MatchString(r.Zone)) || !slices.Contains([]string{"unknown", "0-1m", "1-6m", "6-12m", "1y+"}, r.Age) {
		return r, bad
	}
	for i, name := range r.Drivers {
		if !driver.MatchString(name) || (i > 0 && name <= r.Drivers[i-1]) {
			return r, bad
		}
	}
	return r, nil
}

// Day is a day's totals. No individual report survives aggregation. The
// health counts take only Schema reports, and are absent on a day without
// one.
type Day struct {
	Date      string         `json:"date"`
	Reports   int            `json:"reports"`
	Versions  map[string]int `json:"ftw_versions"`
	Channels  map[string]int `json:"channels"`
	Drivers   map[string]int `json:"drivers"`
	Battery   map[string]int `json:"battery_kwh"`
	Zones     map[string]int `json:"price_zones"`
	Age       map[string]int `json:"install_age"`
	Solve     map[string]int `json:"plan_solve,omitempty"`
	Fallbacks map[string]int `json:"plan_fallbacks,omitempty"`
	Errors    map[string]int `json:"driver_errors,omitempty"`
	Update    map[string]int `json:"update,omitempty"`
}

type View struct {
	Schema  string `json:"schema"`
	Meaning string `json:"meaning"`
	Days    []Day  `json:"days"`
}

func NewDay(date string) Day {
	return Day{Date: date, Versions: map[string]int{}, Channels: map[string]int{}, Drivers: map[string]int{},
		Battery: map[string]int{}, Zones: map[string]int{}, Age: map[string]int{}}
}

// Add bounds both the count and the number of labels in a day.
func (d *Day) Add(r Report) bool {
	if d.Reports < 0 || d.Reports >= MaxReports || d.Versions == nil || d.Channels == nil || d.Drivers == nil || d.Battery == nil || d.Zones == nil || d.Age == nil {
		return false
	}
	d.Reports++
	inc(d.Versions, r.Version)
	inc(d.Channels, r.Channel)
	if len(r.Drivers) == 0 {
		inc(d.Drivers, "none")
	} else {
		for _, name := range r.Drivers {
			inc(d.Drivers, name)
		}
	}
	inc(d.Battery, r.Battery)
	inc(d.Zones, r.Zone)
	inc(d.Age, r.Age)
	if r.Schema == Schema {
		for _, c := range []struct {
			counts *map[string]int
			label  string
		}{{&d.Solve, r.Solve}, {&d.Fallbacks, r.Fallbacks}, {&d.Errors, r.Errors}, {&d.Update, r.Update}} {
			if *c.counts == nil {
				*c.counts = map[string]int{}
			}
			inc(*c.counts, c.label)
		}
	}
	return true
}

func inc(counts map[string]int, label string) {
	if _, ok := counts[label]; !ok && len(counts) >= MaxLabels-1 {
		label = "other"
	}
	counts[label]++
}
