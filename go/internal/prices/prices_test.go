package prices

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
)

// ---- Applier ----

func TestApplierComputesConsumerPrice(t *testing.T) {
	// 1.50 SEK/kWh spot, 30 öre/kWh grid tariff, 25% VAT
	// spot in öre = 150, total = (150 + 30) * 1.25 = 225
	a := Applier{GridTariffOreKwh: 30, VATPercent: 25}
	spot, total := a.Apply(1.5)
	if spot != 150 {
		t.Errorf("spot: %f, want 150", spot)
	}
	if total != 225 {
		t.Errorf("total: %f, want 225", total)
	}
}

func TestApplierZeroGridTariff(t *testing.T) {
	a := Applier{GridTariffOreKwh: 0, VATPercent: 25}
	_, total := a.Apply(2.0)
	// 200 * 1.25 = 250
	if total != 250 {
		t.Errorf("%f", total)
	}
}

func TestApplierZeroVAT(t *testing.T) {
	a := Applier{GridTariffOreKwh: 10, VATPercent: 0}
	_, total := a.Apply(1.0)
	if total != 110 {
		t.Errorf("%f", total)
	}
}

// ---- Sourceful parse ----

func TestSourcefulFetchesOneDayInSEK(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/SE3" {
			t.Errorf("path: got %q, want /SE3", r.URL.Path)
		}
		if got := r.URL.Query().Get("date"); got != "2026-07-17" {
			t.Errorf("date: got %q", got)
		}
		if got := r.URL.Query().Get("days"); got != "1" {
			t.Errorf("days: got %q, want 1", got)
		}
		if got := r.URL.Query().Get("currency"); got != "SEK" {
			t.Errorf("currency: got %q, want SEK", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"area":       "SE3",
			"currency":   "SEK",
			"unit":       "SEK/MWh",
			"resolution": "PT15M",
			"timezone":   "UTC",
			"prices": []map[string]any{
				{"datetime": "2026-07-16T22:00:00+00:00", "price": 1079.579865},
				{"datetime": "2026-07-16T22:15:00+00:00", "price": -12.5},
			},
		})
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	p := &SourcefulProvider{Client: &http.Client{}, BaseURL: srv.URL}
	day := time.Date(2026, 7, 17, 0, 0, 0, 0, time.Local)
	rows, err := p.Fetch(context.Background(), "se3", day)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].SlotLenMin != 15 {
		t.Errorf("slot len: got %d, want 15", rows[0].SlotLenMin)
	}
	if math.Abs(rows[0].SEKPerKWh-1.079579865) > 1e-9 {
		t.Errorf("price conversion: got %g SEK/kWh", rows[0].SEKPerKWh)
	}
	if math.Abs(rows[1].SEKPerKWh-(-0.0125)) > 1e-9 {
		t.Errorf("negative price conversion: got %g SEK/kWh", rows[1].SEKPerKWh)
	}
}

func TestSourcefulHandlesUnpublishedDay(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	p := &SourcefulProvider{Client: &http.Client{}, BaseURL: srv.URL}
	rows, err := p.Fetch(context.Background(), "SE3", time.Now())
	if err != nil {
		t.Fatalf("404 should not error: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("404 should return empty, got %d", len(rows))
	}
}

func TestSourcefulRejectsUnexpectedCurrency(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"area":"SE3","currency":"EUR","unit":"EUR/MWh","resolution":"PT15M","prices":[]}`)
	}))
	defer srv.Close()
	p := &SourcefulProvider{Client: &http.Client{}, BaseURL: srv.URL}
	_, err := p.Fetch(context.Background(), "SE3", time.Now())
	if err == nil || !strings.Contains(err.Error(), "unexpected currency") {
		t.Fatalf("got error %v, want unexpected currency", err)
	}
}

// ---- Elpriser parse ----

func TestElpriserDetects15MinFromTimeEnd(t *testing.T) {
	// Real elprisetjustnu response shape: time_end + time_start explicitly give the slot
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rows := []map[string]any{
			{"SEK_per_kWh": 1.25, "time_start": "2026-04-14T00:00:00+02:00", "time_end": "2026-04-14T00:15:00+02:00"},
			{"SEK_per_kWh": 1.20, "time_start": "2026-04-14T00:15:00+02:00", "time_end": "2026-04-14T00:30:00+02:00"},
			{"SEK_per_kWh": 1.10, "time_start": "2026-04-14T00:30:00+02:00", "time_end": "2026-04-14T00:45:00+02:00"},
			{"SEK_per_kWh": 1.05, "time_start": "2026-04-14T00:45:00+02:00", "time_end": "2026-04-14T01:00:00+02:00"},
		}
		_ = json.NewEncoder(w).Encode(rows)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	p := &ElpriserProvider{Client: &http.Client{}, BaseURL: srv.URL}
	day, _ := time.Parse("2006-01-02", "2026-04-14")
	rows, err := p.Fetch(context.Background(), "SE3", day)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4 (15-min slots)", len(rows))
	}
	for i, r := range rows {
		if r.SlotLenMin != 15 {
			t.Errorf("row %d: slot_len_min=%d, want 15", i, r.SlotLenMin)
		}
	}
}

func TestElpriserDetects60MinFromSpacing(t *testing.T) {
	// Legacy hourly response (no time_end) — spacing-based detection kicks in
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rows := []map[string]any{
			{"SEK_per_kWh": 1.25, "time_start": "2026-04-14T00:00:00+02:00"},
			{"SEK_per_kWh": 1.10, "time_start": "2026-04-14T01:00:00+02:00"},
			{"SEK_per_kWh": 0.90, "time_start": "2026-04-14T02:00:00+02:00"},
		}
		_ = json.NewEncoder(w).Encode(rows)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	p := &ElpriserProvider{Client: &http.Client{}, BaseURL: srv.URL}
	day, _ := time.Parse("2006-01-02", "2026-04-14")
	rows, err := p.Fetch(context.Background(), "SE3", day)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows", len(rows))
	}
	if rows[0].SlotLenMin != 60 {
		t.Errorf("hourly data should be tagged as 60-min, got %d", rows[0].SlotLenMin)
	}
}

func TestElpriserHandles404(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()
	p := &ElpriserProvider{Client: &http.Client{}, BaseURL: srv.URL}
	rows, err := p.Fetch(context.Background(), "SE3", time.Now())
	if err != nil {
		t.Fatalf("404 should not error: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("404 should return empty, got %d", len(rows))
	}
}

func TestElpriserURL(t *testing.T) {
	captured := make(chan string, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case captured <- r.URL.Path:
		default:
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()
	p := &ElpriserProvider{Client: &http.Client{}, BaseURL: srv.URL}
	day, _ := time.Parse("2006-01-02", "2026-04-14")
	_, _ = p.Fetch(context.Background(), "SE3", day)
	got := <-captured
	want := "/2026/04-14_SE3.json"
	if got != want {
		t.Errorf("URL path: got %q, want %q", got, want)
	}
}

// ---- Service integration: fetch → save → load ----

func TestServiceFetchesAndStores(t *testing.T) {
	// Always use today's local date so the test doesn't break on a
	// calendar boundary. fetchAndStore queries elprisetjustnu's URL
	// shape for the current date — match it dynamically.
	today := time.Now().In(time.Local)
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.Local)
	todayPath := today.Format("01-02_") // MM-DD_ — same fragment fetchAndStore uses
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, todayPath) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"SEK_per_kWh": 1.0, "time_start": today.Format(time.RFC3339), "time_end": today.Add(15 * time.Minute).Format(time.RFC3339)},
				{"SEK_per_kWh": 2.0, "time_start": today.Add(15 * time.Minute).Format(time.RFC3339), "time_end": today.Add(30 * time.Minute).Format(time.RFC3339)},
			})
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	s := &Service{
		Provider: &ElpriserProvider{Client: &http.Client{}, BaseURL: srv.URL},
		Store:    st,
		Zone:     "SE3",
		Applier:  Applier{GridTariffOreKwh: 30, VATPercent: 25},
	}
	s.fetchAndStore(context.Background())

	pts, err := st.LoadPrices("SE3", today.UnixMilli(), today.Add(time.Hour).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 2 {
		t.Fatalf("got %d prices, want 2", len(pts))
	}
	if pts[0].SpotOreKwh != 100 {
		t.Errorf("spot: %f", pts[0].SpotOreKwh)
	}
	if pts[0].TotalOreKwh != 162.5 {
		t.Errorf("total: %f", pts[0].TotalOreKwh)
	}
	if pts[0].SlotLenMin != 15 {
		t.Errorf("slot: %d", pts[0].SlotLenMin)
	}
}

// ---- FromConfig ----

func TestFromConfigNilWhenDisabled(t *testing.T) {
	if FromConfig(nil, nil, nil) != nil {
		t.Error("nil cfg → nil service")
	}
	if FromConfig(&config.Price{Provider: "none"}, nil, nil) != nil {
		t.Error("none → nil service")
	}
	if FromConfig(&config.Price{Provider: ""}, nil, nil) != nil {
		t.Error("empty → nil service")
	}
}

func TestFromConfigDefaultsZoneAndVAT(t *testing.T) {
	st, _ := state.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()
	s := FromConfig(&config.Price{Provider: "sourceful"}, st, nil)
	if s == nil {
		t.Fatal("expected service")
	}
	if s.Provider.Name() != "sourceful" {
		t.Errorf("default provider: %s", s.Provider.Name())
	}
	if s.Zone != "SE3" {
		t.Errorf("default zone: %s", s.Zone)
	}
	if s.Applier.VATPercent != 25 {
		t.Errorf("default VAT: %f", s.Applier.VATPercent)
	}
}

// ---- ENTSOE minimal checks ----

func TestENTSOERequiresAPIKey(t *testing.T) {
	p := NewENTSOE("")
	_, err := p.Fetch(context.Background(), "SE3", time.Now())
	if err == nil {
		t.Error("expected error for missing API key")
	}
}

func TestENTSOERejectsUnknownZone(t *testing.T) {
	p := NewENTSOE("any-key")
	_, err := p.Fetch(context.Background(), "ZZZ", time.Now())
	if err == nil {
		t.Error("expected error for unknown zone")
	}
}

// entsoeServer returns an httptest server that always replies with the
// given A44 XML body, plus a provider wired to it with an identity
// EUR→native converter (so SEKPerKWh == EUR/kWh and assertions are exact).
func entsoeServer(t *testing.T, body string) (*ENTSOEProvider, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, body)
	}))
	p := &ENTSOEProvider{
		Client:      &http.Client{},
		APIKey:      "test-key",
		BaseURL:     srv.URL,
		Currency:    "SEK",
		EURToNative: func(eur float64) float64 { return eur }, // identity
	}
	return p, srv.Close
}

// A real day-ahead A44 document, trimmed to a 3-hour PT60M period. The
// default xmlns + the dotted element names (price.amount) are exactly
// what the live transparency platform emits.
const entsoeHourlyXML = `<?xml version="1.0" encoding="UTF-8"?>
<Publication_MarketDocument xmlns="urn:iec62325.351:tc57wg16:451-3:publicationdocument:7:0">
  <type>A44</type>
  <TimeSeries>
    <currency_Unit.name>EUR</currency_Unit.name>
    <price_Measure_Unit.name>MWH</price_Measure_Unit.name>
    <Period>
      <timeInterval>
        <start>2026-06-02T22:00Z</start>
        <end>2026-06-03T01:00Z</end>
      </timeInterval>
      <resolution>PT60M</resolution>
      <Point><position>1</position><price.amount>50.0</price.amount></Point>
      <Point><position>2</position><price.amount>40.0</price.amount></Point>
      <Point><position>3</position><price.amount>30.0</price.amount></Point>
    </Period>
  </TimeSeries>
</Publication_MarketDocument>`

func TestENTSOEParsesHourlyDayAhead(t *testing.T) {
	p, closeFn := entsoeServer(t, entsoeHourlyXML)
	defer closeFn()

	day, _ := time.Parse("2006-01-02", "2026-06-03")
	rows, err := p.Fetch(context.Background(), "SE3", day)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	wantStart, _ := time.Parse(time.RFC3339, "2026-06-02T22:00:00Z")
	if !rows[0].SlotStart.UTC().Equal(wantStart) {
		t.Errorf("slot[0] start: got %s, want %s", rows[0].SlotStart.UTC(), wantStart)
	}
	if rows[0].SlotLenMin != 60 {
		t.Errorf("slot len: got %d, want 60", rows[0].SlotLenMin)
	}
	// EUR/MWh → EUR/kWh (÷1000), identity converter → SEKPerKWh.
	for i, want := range []float64{0.050, 0.040, 0.030} {
		if math.Abs(rows[i].SEKPerKWh-want) > 1e-9 {
			t.Errorf("slot[%d] price: got %g, want %g", i, rows[i].SEKPerKWh, want)
		}
	}
	// Consecutive slots are exactly one resolution apart.
	if d := rows[1].SlotStart.Sub(rows[0].SlotStart); d != time.Hour {
		t.Errorf("slot spacing: got %s, want 1h", d)
	}
}

// ENTSOE omits a Point when the price is unchanged from the previous
// position (A44 sparse representation). The parser must carry the last
// price forward to fill the gap, otherwise slots silently vanish.
const entsoeQuarterlySparseXML = `<?xml version="1.0" encoding="UTF-8"?>
<Publication_MarketDocument xmlns="urn:iec62325.351:tc57wg16:451-3:publicationdocument:7:0">
  <TimeSeries>
    <currency_Unit.name>EUR</currency_Unit.name>
    <Period>
      <timeInterval>
        <start>2026-06-02T22:00Z</start>
        <end>2026-06-02T23:00Z</end>
      </timeInterval>
      <resolution>PT15M</resolution>
      <Point><position>1</position><price.amount>100.0</price.amount></Point>
      <Point><position>3</position><price.amount>80.0</price.amount></Point>
    </Period>
  </TimeSeries>
</Publication_MarketDocument>`

func TestENTSOEFifteenMinCarriesForwardGaps(t *testing.T) {
	p, closeFn := entsoeServer(t, entsoeQuarterlySparseXML)
	defer closeFn()

	day, _ := time.Parse("2006-01-02", "2026-06-02")
	rows, err := p.Fetch(context.Background(), "SE3", day)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	// 1h / 15min = 4 slots, even though only positions 1 and 3 are given.
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4 (15-min slots over 1h)", len(rows))
	}
	for i, r := range rows {
		if r.SlotLenMin != 15 {
			t.Errorf("slot[%d] len: got %d, want 15", i, r.SlotLenMin)
		}
	}
	// pos1=100 → slots 1,2 carry 100; pos3=80 → slots 3,4 carry 80.
	wantPrices := []float64{0.100, 0.100, 0.080, 0.080}
	for i, want := range wantPrices {
		if math.Abs(rows[i].SEKPerKWh-want) > 1e-9 {
			t.Errorf("slot[%d] price: got %g, want %g", i, rows[i].SEKPerKWh, want)
		}
	}
}

// With no converter wired (NewENTSOE path before FromConfig sets one),
// the parser must still produce a sane SEK figure rather than emitting
// raw EUR. Falls back to the ballpark 11.5 SEK/EUR.
func TestENTSOEFallsBackToBallparkFXWhenConverterNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, entsoeHourlyXML)
	}))
	defer srv.Close()
	p := &ENTSOEProvider{Client: &http.Client{}, APIKey: "k", BaseURL: srv.URL} // EURToNative nil
	day, _ := time.Parse("2006-01-02", "2026-06-03")
	rows, err := p.Fetch(context.Background(), "SE3", day)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	// 50 EUR/MWh → 0.05 EUR/kWh × 11.5 ≈ 0.575 SEK/kWh
	if math.Abs(rows[0].SEKPerKWh-0.575) > 1e-9 {
		t.Errorf("fallback price: got %g, want 0.575", rows[0].SEKPerKWh)
	}
}

// ---- Error paths ----

func TestElpriserHandles500(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, "oops")
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()
	p := &ElpriserProvider{Client: &http.Client{}, BaseURL: srv.URL}
	_, err := p.Fetch(context.Background(), "SE3", time.Now())
	if err == nil {
		t.Error("expected error for 5xx")
	}
}
