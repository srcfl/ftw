package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
)

func TestHandleSavingsDailyCanceledRequestDoesNotCacheSuccess(t *testing.T) {
	for _, days := range []int{1, 2} {
		t.Run(strconv.Itoa(days), func(t *testing.T) {
			testSavingsDailyCanceledRequest(t, days)
		})
	}
}

func testSavingsDailyCanceledRequest(t *testing.T, days int) {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(&Deps{
		State: st,
		Cfg:   &config.Config{Price: &config.Price{Zone: "SE3"}},
		CfgMu: &sync.RWMutex{},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	path := "/api/savings/daily?days=" + strconv.Itoa(days)
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("canceled savings status = %d, want 500", rr.Code)
	}
	if len(srv.savingsCache) != 0 {
		t.Fatal("canceled request cached an incomplete day")
	}

	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("next savings status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Days []any `json:"days"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Days) != days || len(srv.savingsCache) != days-1 {
		t.Fatalf("retry returned %d days and cached %d, want %d and %d", len(body.Days), len(srv.savingsCache), days, days-1)
	}
}

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
	assertSavingsAttribution(t, body)
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
	assertSavingsAttribution(t, body)
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
		Days       []map[string]any `json:"days"`
		Totals     map[string]any   `json:"totals"`
		ValueScope string           `json:"value_scope"`
		Baseline   string           `json:"baseline"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v (body: %s)", err, rr.Body.String())
	}
	if len(body.Days) != 2 {
		t.Fatalf("want 2 days, got %d", len(body.Days))
	}
	if body.ValueScope != savingsValueScope || body.Baseline != savingsBaseline {
		t.Fatalf("attribution = %q/%q, want %q/%q", body.ValueScope, body.Baseline, savingsValueScope, savingsBaseline)
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
	todayExpected := numberFromMap(today, "expected_ms")
	todayHistory := numberFromMap(today, "history_covered_ms")
	todayPriced := numberFromMap(today, "priced_covered_ms")
	if todayExpected <= 0 || todayHistory <= 0 || todayPriced <= 0 || todayPriced > todayHistory || todayHistory > todayExpected {
		t.Errorf("today coverage durations invalid: expected=%v history=%v priced=%v", todayExpected, todayHistory, todayPriced)
	}
	if got := numberFromMap(today, "history_coverage_pct"); got <= 0 || got > 1 {
		t.Errorf("today.history_coverage_pct = %v, want (0,1]", got)
	}
	if got := numberFromMap(today, "priced_coverage_pct"); got <= 0 || got > 1 {
		t.Errorf("today.priced_coverage_pct = %v, want (0,1]", got)
	}
	// Totals must aggregate the per-day values (yesterday is 0).
	totalSaved, _ := body.Totals["saved_ore"].(float64)
	todaySaved, _ := today["saved_ore"].(float64)
	if !approxEqAPI(totalSaved, todaySaved, 0.01) {
		t.Errorf("totals.saved_ore = %v, want ~%v (only today should have data)", totalSaved, todaySaved)
	}
	var expectedSum, historySum, pricedSum float64
	for _, day := range body.Days {
		expectedSum += numberFromMap(day, "expected_ms")
		historySum += numberFromMap(day, "history_covered_ms")
		pricedSum += numberFromMap(day, "priced_covered_ms")
	}
	if got := numberFromMap(body.Totals, "expected_ms"); got != expectedSum {
		t.Errorf("totals.expected_ms = %v, want %v", got, expectedSum)
	}
	if got := numberFromMap(body.Totals, "history_covered_ms"); got != historySum {
		t.Errorf("totals.history_covered_ms = %v, want %v", got, historySum)
	}
	if got := numberFromMap(body.Totals, "priced_covered_ms"); got != pricedSum {
		t.Errorf("totals.priced_covered_ms = %v, want %v", got, pricedSum)
	}
	if got, want := numberFromMap(body.Totals, "history_coverage_pct"), historySum/expectedSum; !approxEqAPI(got, want, 1e-9) {
		t.Errorf("weighted history coverage = %v, want %v", got, want)
	}
	if got, want := numberFromMap(body.Totals, "priced_coverage_pct"), pricedSum/expectedSum; !approxEqAPI(got, want, 1e-9) {
		t.Errorf("weighted cost coverage = %v, want %v", got, want)
	}
}

func TestHandleSavingsDailyPricedDayWithoutHistoryReportsZeroCoverage(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	start := todayStart.AddDate(0, 0, -1)
	if err := st.SavePrices([]state.PricePoint{{
		Zone: "SE3", SlotTsMs: start.UnixMilli(), SlotLenMin: int(todayStart.Sub(start) / time.Minute),
		SpotOreKwh: 50, TotalOreKwh: 100, Source: "test",
	}}); err != nil {
		t.Fatalf("SavePrices: %v", err)
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
		Days       []map[string]any `json:"days"`
		ValueScope string           `json:"value_scope"`
		Baseline   string           `json:"baseline"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if len(body.Days) != 2 {
		t.Fatalf("days = %d, want 2", len(body.Days))
	}
	day := body.Days[0]
	if got, _ := day["resolution"].(string); got != "slot" {
		t.Fatalf("resolution = %q, want slot", got)
	}
	if numberFromMap(day, "expected_ms") <= 0 || numberFromMap(day, "history_covered_ms") != 0 ||
		numberFromMap(day, "priced_covered_ms") != 0 || numberFromMap(day, "history_coverage_pct") != 0 ||
		numberFromMap(day, "priced_coverage_pct") != 0 {
		t.Fatalf("coverage without history = %+v, want positive expected and zero covered", day)
	}
	for _, key := range []string{"actual_cost_ore", "baseline_cost_ore", "saved_ore"} {
		if got := numberFromMap(day, key); got != 0 {
			t.Errorf("%s = %v, want 0", key, got)
		}
	}
	if body.ValueScope != savingsValueScope || body.Baseline != savingsBaseline {
		t.Fatalf("attribution = %q/%q, want %q/%q", body.ValueScope, body.Baseline, savingsValueScope, savingsBaseline)
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

func TestSavingsCoveragePctIsBounded(t *testing.T) {
	for _, tc := range []struct {
		covered, expected int64
		want              float64
	}{
		{0, 100, 0},
		{-1, 100, 0},
		{10, 0, 0},
		{50, 100, 0.5},
		{120, 100, 1},
	} {
		if got := boundedCoveragePct(tc.covered, tc.expected); got != tc.want {
			t.Errorf("boundedCoveragePct(%d, %d) = %v, want %v", tc.covered, tc.expected, got, tc.want)
		}
	}
}

func numberFromMap(values map[string]any, key string) float64 {
	value, _ := values[key].(float64)
	return value
}

func assertSavingsAttribution(t *testing.T, body map[string]any) {
	t.Helper()
	if body["value_scope"] != savingsValueScope || body["baseline"] != savingsBaseline {
		t.Fatalf("attribution = %v/%v, want %q/%q", body["value_scope"], body["baseline"], savingsValueScope, savingsBaseline)
	}
}

func approxEqAPI(a, b, tol float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= tol
}
