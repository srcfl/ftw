//go:build unix

package site

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"
)

// pinnedSEKPerKWh is the rig's spot price for each hour after the stack
// starts: an evening peak, a cheap night, a morning peak, a flat midday and
// a second evening. A run sees the same prices from the same start on any
// day, so the plan does not change with the day's market.
var pinnedSEKPerKWh = [24]float64{
	1.80, 1.60, 1.40, 1.10,
	0.40, 0.30, 0.25, 0.25, 0.30, 0.50,
	1.20, 1.50, 1.30, 0.90,
	0.60, 0.50, 0.45, 0.50, 0.70, 1.00,
	1.40, 1.70, 1.50, 1.10,
}

// priceServer answers as elprisetjustnu does, in 15 min rows, with only the
// 24 h of pinned prices from the quarter it started in. Core reads it
// through price.base_url, which also turns off the live Nord Pool fallback.
type priceServer struct {
	start time.Time
	srv   *httptest.Server
}

func newPriceServer(now time.Time) *priceServer {
	p := &priceServer{start: now.Truncate(15 * time.Minute)}
	p.srv = httptest.NewServer(http.HandlerFunc(p.serve))
	return p
}

func (p *priceServer) url() string { return p.srv.URL }

func (p *priceServer) close() { p.srv.Close() }

type priceRow struct {
	SEKPerKWh float64 `json:"SEK_per_kWh"`
	TimeStart string  `json:"time_start"`
	TimeEnd   string  `json:"time_end"`
}

// rows are the pinned quarters that start on the given local date.
func (p *priceServer) rows(year, month, day int) []priceRow {
	from := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.Local)
	to := from.AddDate(0, 0, 1)
	var out []priceRow
	for i := 0; i < 4*len(pinnedSEKPerKWh); i++ {
		t := p.start.Add(time.Duration(i) * 15 * time.Minute)
		if t.Before(from) || !t.Before(to) {
			continue
		}
		out = append(out, priceRow{SEKPerKWh: pinnedSEKPerKWh[i/4],
			TimeStart: t.Format(time.RFC3339), TimeEnd: t.Add(15 * time.Minute).Format(time.RFC3339)})
	}
	return out
}

func (p *priceServer) serve(w http.ResponseWriter, r *http.Request) {
	var year, month, day int
	var zone string
	if n, _ := fmt.Sscanf(r.URL.Path, "/%d/%d-%d_%3s.json", &year, &month, &day, &zone); n != 4 {
		http.NotFound(w, r)
		return
	}
	rows := p.rows(year, month, day)
	if len(rows) == 0 {
		http.NotFound(w, r) // elprisetjustnu's answer for a day it has not published
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rows)
}
