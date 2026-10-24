package reportwire

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/protocol/energywire"
	"github.com/srcfl/ftw/go/protocol/fleetwire"
	"github.com/srcfl/ftw/go/protocol/ohttp"
)

// The largest report of either kind fills the same padded request, so the
// relay cannot tell a fleet report from an energy report, or one box's from
// another's, by its length.
func TestEveryReportHasOneLength(t *testing.T) {
	drivers := make([]string, 16)
	for i := range drivers {
		drivers[i] = strings.Repeat(string(rune('a'+i)), 64)
	}
	fleet, _ := json.Marshal(fleetwire.Report{Schema: fleetwire.Schema, Version: "v1234.1234.1234-beta.1234", Channel: "unknown",
		Drivers: drivers, Battery: "30+", Zone: strings.Repeat("Z", 16), Age: "unknown",
		Solve: "none", Fallbacks: "none", Errors: "none", Update: "failed"})
	energy, _ := json.Marshal(energywire.Report{Schema: energywire.Schema, Date: "2026-10-23", PVGeneration: 1000, GridImport: 1000,
		GridExport: 1000, BatteryCharge: 1000, BatteryDischarge: 1000, VehicleCharge: 1000})
	small, _ := json.Marshal(energywire.Report{Schema: energywire.Schema, Date: "2026-10-23"})
	id := []byte("0123456789abcdef")
	for _, body := range [][]byte{fleet, energy, small} {
		msg, ok := Request(Report{ID: id, Day: "2026-10-24", Body: body})
		if !ok || len(msg) != RequestSize {
			t.Fatalf("%d-byte report padded to %d", len(body), len(msg))
		}
		got, ok := Read(msg)
		if !ok || string(got.Body) != string(body) || string(got.ID) != string(id) || got.Day != "2026-10-24" || Schema(got.Body) == "" {
			t.Fatalf("read back %+v", got)
		}
	}
	if Schema(fleet) != fleetwire.Schema || Schema(energy) != energywire.Schema || Schema([]byte("x")) != "" {
		t.Fatal("schema")
	}
	for name, r := range map[string]Report{
		"oversized": {ID: id, Day: "2026-10-24", Body: make([]byte, RequestSize)},
		"short id":  {ID: id[:15], Day: "2026-10-24", Body: small},
		"no day":    {ID: id, Body: small},
		"loose day": {ID: id, Day: "2026-10-4", Body: small},
	} {
		if _, ok := Request(r); ok {
			t.Fatalf("%s was padded", name)
		}
	}
	if len(Response(204)) != ResponseSize {
		t.Fatal("response padding")
	}
}

func TestOnlyTheOneInnerRequestIsRead(t *testing.T) {
	fields := []ohttp.Field{{Name: "report-id", Value: "MDEyMzQ1Njc4OWFiY2RlZg"}, {Name: "report-day", Value: "2026-10-24"}}
	good := ohttp.Request{Method: Method, Scheme: Scheme, Authority: Authority, Path: Path, Fields: fields, Content: []byte("{}")}
	if _, ok := Read(good.Marshal(RequestSize)); !ok {
		t.Fatal("the one request was refused")
	}
	other := func(f func(*ohttp.Request)) []byte {
		r := good
		f(&r)
		return r.Marshal(RequestSize)
	}
	for name, msg := range map[string][]byte{
		"another path":   other(func(r *ohttp.Request) { r.Path = "/v1/energy/report" }),
		"another method": other(func(r *ohttp.Request) { r.Method = "PUT" }),
		"another host":   other(func(r *ohttp.Request) { r.Authority = "example.com" }),
		"plain http":     other(func(r *ohttp.Request) { r.Scheme = "http" }),
		"another header": other(func(r *ohttp.Request) { r.Fields = append(r.Fields, ohttp.Field{Name: "x-home", Value: "1"}) }),
		"no id":          other(func(r *ohttp.Request) { r.Fields = r.Fields[1:] }),
		"short id":       other(func(r *ohttp.Request) { r.Fields = []ohttp.Field{{Name: "report-id", Value: "MDEy"}, fields[1]} }),
		"bad day":        other(func(r *ohttp.Request) { r.Fields = []ohttp.Field{fields[0], {Name: "report-day", Value: "yesterday"}} }),
		"swapped":        other(func(r *ohttp.Request) { r.Fields = []ohttp.Field{fields[1], fields[0]} }),
		"not padded":     good.Marshal(0),
		"padded more":    good.Marshal(RequestSize + 1),
		"a response":     Response(200),
	} {
		if _, ok := Read(msg); ok {
			t.Fatalf("%s was read", name)
		}
	}
}

// A report counts on the day it was sent and the next, UTC, and no later.
func TestOnlyAFreshReportCounts(t *testing.T) {
	now := time.Date(2026, 10, 24, 0, 30, 0, 0, time.UTC)
	for day, want := range map[string]bool{"2026-10-24": true, "2026-10-23": true, "2026-10-22": false, "2026-10-25": false, "x": false} {
		if Fresh(day, now) != want {
			t.Fatalf("%s: fresh %v", day, !want)
		}
	}
}
