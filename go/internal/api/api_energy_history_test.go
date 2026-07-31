package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

func apiEnergyPtr(v float64) *float64 { return &v }

func seedAPIEnergyHistory(t *testing.T) (*state.Store, int64, string) {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Hour).UnixMilli()
	base = base / state.EnergyLedgerBucketMS * state.EnergyLedgerBucketMS
	assetID := state.HardwareEnergyAssetID("maker:api-meter", state.AssetGridMeter)
	makeObservation := func(at int64, counter float64) state.EnergyObservation {
		return state.EnergyObservation{
			AssetID: assetID, DeviceID: "maker:api-meter", AssetKind: state.AssetGridMeter,
			Label: "Site meter", Flow: state.FlowGridImport, AtMs: at,
			CounterWh: apiEnergyPtr(counter), PowerW: apiEnergyPtr(600),
		}
	}
	if err := st.RecordTickWithEnergy(state.HistoryPoint{TsMs: base, JSON: "{}"}, nil,
		[]state.EnergyObservation{makeObservation(base, 100)}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordTickWithEnergy(state.HistoryPoint{TsMs: base + 60_000, JSON: "{}"}, nil,
		[]state.EnergyObservation{makeObservation(base+60_000, 112)}); err != nil {
		t.Fatal(err)
	}
	return st, base, assetID
}

func TestEnergyHistoryAPIProvidesBoundedSystemAndAssetReads(t *testing.T) {
	st, base, assetID := seedAPIEnergyHistory(t)
	t.Cleanup(func() { _ = st.Close() })
	srv := New(&Deps{State: st})

	query := url.Values{
		"scope": {"asset"}, "asset_id": {assetID}, "since": {formatInt64(base)},
		"until": {formatInt64(base + state.EnergyLedgerBucketMS)}, "bucket": {"5m"}, "limit": {"50"},
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/energy/history?"+query.Encode(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var response struct {
		SchemaVersion int                       `json:"schema_version"`
		Scope         string                    `json:"scope"`
		Points        []state.EnergyLedgerPoint `json:"points"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.SchemaVersion != 1 || response.Scope != "asset" || len(response.Points) == 0 {
		t.Fatalf("response = %+v", response)
	}
	var measured float64
	for _, point := range response.Points {
		if point.AssetID != assetID {
			t.Fatalf("asset filter leaked %q", point.AssetID)
		}
		if point.Quality == "measured" {
			measured += point.EnergyWh
		}
	}
	if measured != 12 {
		t.Fatalf("measured energy=%v, want 12", measured)
	}
}

func TestEnergyHistoryCSVIncludesProvenanceColumns(t *testing.T) {
	st, base, _ := seedAPIEnergyHistory(t)
	t.Cleanup(func() { _ = st.Close() })
	srv := New(&Deps{State: st})
	path := "/api/energy/history.csv?scope=system&bucket=5m&since=" + formatInt64(base) +
		"&until=" + formatInt64(base+state.EnergyLedgerBucketMS)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/csv") {
		t.Fatalf("content type=%q", got)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "source,quality,provenance") || !strings.Contains(body, "hardware_counter,measured,counter") {
		t.Fatalf("CSV missing provenance columns/row:\n%s", body)
	}
}

func TestEnergyHistoryQueryRejectsUnboundedBucketCountAndClampsLimit(t *testing.T) {
	now := time.Now().UnixMilli()
	tooWide := "/api/energy/history?since=" + formatInt64(now-30*24*60*60*1000) +
		"&until=" + formatInt64(now) + "&bucket=5m"
	if _, err := parseEnergyHistoryQuery(httptest.NewRequest(http.MethodGet, tooWide, nil)); err == nil {
		t.Fatal("30 days at five-minute resolution should exceed the bucket bound")
	}

	bounded := "/api/energy/history?since=" + formatInt64(now-24*60*60*1000) +
		"&until=" + formatInt64(now) + "&bucket=5m&limit=999999"
	q, err := parseEnergyHistoryQuery(httptest.NewRequest(http.MethodGet, bounded, nil))
	if err != nil {
		t.Fatal(err)
	}
	if q.Limit != energyHistoryMaxLimit {
		t.Fatalf("limit=%d, want clamp=%d", q.Limit, energyHistoryMaxLimit)
	}
}

func TestEnergyHistoryLimitReturnsWholeBuckets(t *testing.T) {
	st, base, _ := seedAPIEnergyHistory(t)
	t.Cleanup(func() { _ = st.Close() })
	assetID := state.HardwareEnergyAssetID("maker:api-meter", state.AssetGridMeter)
	observation := state.EnergyObservation{
		AssetID: assetID, DeviceID: "maker:api-meter", AssetKind: state.AssetGridMeter,
		Label: "Site meter", Flow: state.FlowGridImport, AtMs: base + 6*60_000,
		CounterWh: apiEnergyPtr(124), PowerW: apiEnergyPtr(600),
	}
	if err := st.RecordTickWithEnergy(state.HistoryPoint{TsMs: observation.AtMs, JSON: "{}"}, nil,
		[]state.EnergyObservation{observation}); err != nil {
		t.Fatal(err)
	}
	srv := New(&Deps{State: st})
	path := "/api/energy/history?scope=asset&asset_id=" + url.QueryEscape(assetID) +
		"&bucket=5m&limit=1&since=" + formatInt64(base) + "&until=" + formatInt64(base+10*60_000)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var response struct {
		Truncated bool                      `json:"truncated"`
		LimitUnit string                    `json:"limit_unit"`
		Points    []state.EnergyLedgerPoint `json:"points"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Truncated || response.LimitUnit != "buckets" || len(response.Points) < 2 {
		t.Fatalf("expected complete multi-provenance first bucket plus truncation: %+v", response)
	}
	for _, point := range response.Points {
		if point.BucketStartMS != base {
			t.Fatalf("row limit cut into the next bucket: %+v", response.Points)
		}
	}
}

func formatInt64(v int64) string {
	return strconv.FormatInt(v, 10)
}
