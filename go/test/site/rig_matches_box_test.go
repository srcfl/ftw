//go:build unix

package site

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/prices"
)

// A beta or stable Linux box plans with Energyplan in passive or active
// arbitrage. The rig must exercise that path by default.
func TestRigRunsWhatABetaBoxRuns(t *testing.T) {
	t.Setenv("FTW_SITE_ENGINE", "")
	st := &stack{}
	want := (&config.Planner{Enabled: true}).EngineForBuild("v0.157.2-beta.1", "linux", "arm64")
	if got := st.engine(); got != want {
		t.Errorf("rig default engine = %q, a v0.157.2-beta.1 linux/arm64 box runs %q", got, want)
	}
	files, _ := filepath.Glob("*_test.go")
	arbitrage := false
	for _, f := range files {
		if f == "rig_matches_box_test.go" {
			continue
		}
		b, _ := os.ReadFile(f)
		if strings.Contains(string(b), "planner_passive_arbitrage") || strings.Contains(string(b), "planner_arbitrage") {
			arbitrage = true
		}
	}
	if !arbitrage {
		t.Errorf("no site-rig scenario selects planner_passive_arbitrage or planner_arbitrage")
	}
}

// Two runs that start on different days, at different times, read the same
// 24 h of prices from their start, and nothing from the live market.
func TestRigPricesArePinnedToTheRunStart(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Skip("no time zone data:", err)
	}
	read := func(start time.Time) []float64 {
		ps := newPriceServer(start)
		defer ps.close()
		svc := prices.FromConfig(&config.Price{Provider: "elprisetjustnu", Zone: "SE3", BaseURL: ps.url()}, nil, nil)
		var out []float64
		next := ps.start
		for _, day := range []time.Time{start, start.AddDate(0, 0, 1)} {
			rows, err := svc.Provider.Fetch(context.Background(), "SE3", day.In(time.Local))
			if err != nil {
				t.Fatalf("start %s, day %s: %v", start, day.Format("2006-01-02"), err)
			}
			for _, r := range rows {
				if !r.SlotStart.Equal(next) || r.SlotLenMin != 15 {
					t.Fatalf("start %s: row %s/%d min, want %s/15 min", start, r.SlotStart, r.SlotLenMin, next)
				}
				next = next.Add(15 * time.Minute)
				out = append(out, r.SEKPerKWh)
			}
		}
		return out
	}
	a := read(time.Date(2026, 10, 27, 14, 7, 0, 0, loc))
	b := read(time.Date(2026, 7, 3, 23, 52, 0, 0, loc))
	if len(a) != 96 || !reflect.DeepEqual(a, b) {
		t.Fatalf("prices differ between runs:\n%v\n%v", a, b)
	}
}
