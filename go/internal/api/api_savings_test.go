package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
)

// No state and no config → empty days, 200. Matches /api/energy/daily's
// "history is optional" contract so dev / test harnesses without a DB
// don't 500.
func TestHandleSavingsDailyNoState(t *testing.T) {
	srv := New(&Deps{})
	req := httptest.NewRequest(http.MethodGet, "/api/savings/daily?days=7", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if days, _ := body["days"].([]any); len(days) != 0 {
		t.Fatalf("expected empty days, got %d", len(days))
	}
}

// State present but cfg.Price.Zone empty → endpoint short-circuits with
// empty days. There's nothing to price the load baseline against without prices.
func TestHandleSavingsDailyNoZone(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	srv := New(&Deps{
		State: st,
		Cfg:   &config.Config{Price: &config.Price{Zone: ""}},
		CfgMu: &sync.RWMutex{},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/savings/daily?days=3", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if days, _ := body["days"].([]any); len(days) != 0 {
		t.Fatalf("expected empty days for unconfigured zone, got %d", len(days))
	}
}

// End-to-end with real history + prices: seed a known cheap/expensive
// slot pair within today, confirm the handler returns the load-baseline
// savings the underlying state.DailyCostBreakdown produced.
// This is the cross-layer integration check — if either the SQL changes
// or the handler stops applying export pricing, the math comes out
// wrong and this fails.
func TestHandleSavingsDailyEndToEnd(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now()
	loc := now.Location()
	todayMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	elapsed := now.Sub(todayMidnight)
	if elapsed < 30*time.Minute {
		t.Skip("too close to local midnight; need a wider in-day window")
	}

	// Two 5-min hot-tier slots inside today: import in a cheap slot, then
	// the closing sample (the export half) is integrated against the
	// next-slot price. Same shape as the state-level test but anchored
	// to wall-clock so we land inside the handler's "today" range.
	t0 := todayMidnight.Add(elapsed / 3)
	t1 := t0.Add(5 * time.Minute)
	t2 := t1.Add(5 * time.Minute)
	t3 := t2.Add(5 * time.Minute)

	// Slot 0 cheap (100 öre total / 80 öre spot), slot 1 expensive
	// (200 öre total / 150 öre spot). 1h slots so all four sample mid-
	// points land in the same slot in pairs.
	slot0 := todayMidnight.Add(elapsed/3 - time.Hour).UnixMilli()
	slot1 := slot0 + 3_600_000
	if err := st.SavePrices([]state.PricePoint{
		{Zone: "SE3", SlotTsMs: slot0, SlotLenMin: 60, SpotOreKwh: 80, TotalOreKwh: 100, Source: "test"},
		{Zone: "SE3", SlotTsMs: slot1, SlotLenMin: 60, SpotOreKwh: 150, TotalOreKwh: 200, Source: "test"},
	}); err != nil {
		t.Fatalf("SavePrices: %v", err)
	}

	for _, p := range []state.HistoryPoint{
		{TsMs: t0.UnixMilli(), GridW: 1000, LoadW: 1000},
		{TsMs: t1.UnixMilli(), GridW: 1000, LoadW: 1000},
		{TsMs: t2.UnixMilli(), GridW: -2000, LoadW: 1000},
		{TsMs: t3.UnixMilli(), GridW: -2000, LoadW: 1000},
	} {
		if err := st.RecordHistory(p); err != nil {
			t.Fatalf("RecordHistory: %v", err)
		}
	}

	srv := New(&Deps{
		State: st,
		Cfg:   &config.Config{Price: &config.Price{Zone: "SE3"}},
		CfgMu: &sync.RWMutex{},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/savings/daily?days=2", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}

	var body struct {
		Days   []map[string]any `json:"days"`
		Totals map[string]any   `json:"totals"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v (body: %s)", err, rr.Body.String())
	}
	if len(body.Days) != 2 {
		t.Fatalf("want 2 days, got %d", len(body.Days))
	}
	// Today is the last entry; the prior day should be all-zero.
	yesterday := body.Days[0]
	for _, k := range []string{"import_wh", "export_wh", "load_wh", "actual_cost_ore", "baseline_cost_ore", "flat_cost_ore", "saved_ore"} {
		if v, _ := yesterday[k].(float64); v != 0 {
			t.Errorf("yesterday.%s = %v, want 0", k, v)
		}
	}
	today := body.Days[1]
	if v, _ := today["import_wh"].(float64); v <= 0 {
		t.Errorf("today.import_wh = %v, want > 0", v)
	}
	if v, _ := today["export_wh"].(float64); v <= 0 {
		t.Errorf("today.export_wh = %v, want > 0", v)
	}
	if v, _ := today["load_wh"].(float64); v <= 0 {
		t.Errorf("today.load_wh = %v, want > 0", v)
	}
	if v, _ := today["baseline_cost_ore"].(float64); v <= 0 {
		t.Errorf("today.baseline_cost_ore = %v, want > 0", v)
	}
	if v, _ := today["saved_ore"].(float64); v <= 0 {
		t.Errorf("today.saved_ore = %v — expected positive savings vs load baseline, body: %s", v, rr.Body.String())
	}
	if r, _ := today["resolution"].(string); r != "slot" {
		t.Errorf("today.resolution = %q, want \"slot\"", r)
	}
	// Totals must aggregate the per-day values (yesterday is 0).
	totalSaved, _ := body.Totals["saved_ore"].(float64)
	todaySaved, _ := today["saved_ore"].(float64)
	if !approxEqAPI(totalSaved, todaySaved, 0.01) {
		t.Errorf("totals.saved_ore = %v, want ~%v (only today should have data)", totalSaved, todaySaved)
	}
}

// Same days-param clamping convention as /api/energy/daily: garbage/0/
// negative → default 7, >90 → 90. Keep behavior identical so callers
// have one mental model across daily endpoints.
func TestHandleSavingsDailyDaysClamping(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	srv := New(&Deps{
		State: st,
		Cfg:   &config.Config{Price: &config.Price{Zone: "SE3"}},
		CfgMu: &sync.RWMutex{},
	})

	cases := []struct {
		q    string
		want int
	}{
		{"", 7},
		{"abc", 7},
		{"-5", 7},
		{"0", 7},
		{"14", 14},
		{"150", 90},
	}
	for _, tc := range cases {
		t.Run(tc.q, func(t *testing.T) {
			url := "/api/savings/daily"
			if tc.q != "" {
				url += "?days=" + tc.q
			}
			req := httptest.NewRequest(http.MethodGet, url, nil)
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
			}
			var body struct {
				Days []map[string]any `json:"days"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatalf("json: %v", err)
			}
			if len(body.Days) != tc.want {
				t.Errorf("days=%q → %d days, want %d", tc.q, len(body.Days), tc.want)
			}
		})
	}
}

func TestSavingsResolutionUsesPriceSlotPresence(t *testing.T) {
	if got := resolutionFor(state.DayCostBreakdown{PriceSlotCount: 1}); got != "slot" {
		t.Fatalf("zero-priced slot should still count as priced, got %q", got)
	}
	if got := resolutionFor(state.DayCostBreakdown{}); got != "no_prices" {
		t.Fatalf("missing prices should be no_prices, got %q", got)
	}
}

func approxEqAPI(a, b, tol float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= tol
}
