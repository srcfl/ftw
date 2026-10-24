// Package reportwire is how the box's anonymous daily reports reach FTW
// Cloud: each report alone, as one padded inner request sealed with Oblivious
// HTTP (package ohttp) and posted to an independent relay. The gateway reads
// the report's schema and hands it to that report's decoder: the fleet report
// (package fleetwire) or the energy report (package energywire).
//
// Two reports, never one message. With a fleet of a few boxes a driver set
// and a price zone can point at one home, so energy sent beside them would
// belong to that home again. Each report leaves at its own random time.
package reportwire

import (
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/srcfl/ftw/go/protocol/ohttp"
)

// The one inner request. The gateway answers no other.
const (
	Method    = "POST"
	Scheme    = "https"
	Authority = "cloud.ftw.energy"
	Path      = "/v1/report"
)

const (
	// RequestSize is every padded inner request and ResponseSize every padded
	// inner response, so the relay sees one length whatever the report, its
	// kind or the answer. The largest fleet report fits.
	RequestSize  = 1536
	ResponseSize = 16
)

// IDBytes is a report id's length. The box draws one at random for each
// report and keeps it if it ever sends that report again, so the gateway
// counts a report once however often it arrives (RFC 9458, section 6.5).
const IDBytes = 16

// Report is what one inner request carries besides its fixed parts: the
// report's random id, the UTC date the box sent it, and the report itself.
// The id names one report, never a box.
type Report struct {
	ID   []byte
	Day  string
	Body []byte
}

const (
	idField  = "report-id"
	dayField = "report-day"
)

// Request is the padded inner request carrying one report. ok is false for
// an id or a day of the wrong form, and when the report does not fit, so a
// report never goes out at a length of its own.
func Request(r Report) (msg []byte, ok bool) {
	if len(r.ID) != IDBytes || !isDate(r.Day) {
		return nil, false
	}
	msg = ohttp.Request{Method: Method, Scheme: Scheme, Authority: Authority, Path: Path,
		Fields:  []ohttp.Field{{Name: idField, Value: base64.RawURLEncoding.EncodeToString(r.ID)}, {Name: dayField, Value: r.Day}},
		Content: r.Body}.Marshal(RequestSize)
	return msg, len(msg) == RequestSize
}

// Read opens an inner request, or returns false for anything but the one
// padded request with an id and a day.
func Read(msg []byte) (Report, bool) {
	req, err := ohttp.ParseRequest(msg)
	if err != nil || len(msg) != RequestSize || req.Method != Method || req.Scheme != Scheme ||
		req.Authority != Authority || req.Path != Path || len(req.Fields) != 2 ||
		req.Fields[0].Name != idField || req.Fields[1].Name != dayField {
		return Report{}, false
	}
	id, err := base64.RawURLEncoding.DecodeString(req.Fields[0].Value)
	if err != nil || len(id) != IDBytes || !isDate(req.Fields[1].Value) {
		return Report{}, false
	}
	return Report{ID: id, Day: req.Fields[1].Value, Body: req.Content}, true
}

// Fresh reports whether a report sent on day still counts at now: sent today
// or yesterday, UTC. An older one is refused, so a captured request cannot be
// posted again on a later day, when its id is no longer remembered.
func Fresh(day string, now time.Time) bool {
	today := now.UTC().Format(time.DateOnly)
	return day == today || day == now.UTC().AddDate(0, 0, -1).Format(time.DateOnly)
}

func isDate(s string) bool {
	t, err := time.Parse(time.DateOnly, s)
	return err == nil && t.Format(time.DateOnly) == s
}

// Schema reads only a report's schema, to choose its decoder. The decoder
// still checks every byte.
func Schema(body []byte) string {
	var head struct {
		Schema string `json:"schema"`
	}
	if json.Unmarshal(body, &head) != nil {
		return ""
	}
	return head.Schema
}

// Response is the padded inner answer.
func Response(status int) []byte {
	return ohttp.Response{Status: status}.Marshal(ResponseSize)
}
