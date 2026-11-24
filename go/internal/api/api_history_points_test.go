package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

// storedRows is more than historyMaxPoints, one second apart, so a read
// that is not clamped returns more rows than the limit allows.
const storedRows = 3000

func newHistoryPointsServer(t *testing.T) *Server {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	start := time.Now().Add(-55 * time.Minute).UnixMilli()
	samples := make([]state.Sample, 0, storedRows)
	for i := int64(0); i < storedRows; i++ {
		ts := start + i*1000
		if err := st.RecordHistory(state.HistoryPoint{TsMs: ts, GridW: 1000, PVW: -200, LoadW: 800, JSON: "{}"}); err != nil {
			t.Fatal(err)
		}
		samples = append(samples, state.Sample{Driver: "meter", Metric: "meter_w", TsMs: ts, Value: 1000, Unit: "W"})
	}
	if err := st.RecordSamples(samples); err != nil {
		t.Fatal(err)
	}
	return New(&Deps{State: st})
}

func historyPointsGet(t *testing.T, srv *Server, url string) (int, int) {
	t.Helper()
	rr := httptest.NewRecorder()
	srv.mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, url, nil))
	if rr.Code != http.StatusOK {
		return rr.Code, 0
	}
	var body struct {
		Items  []json.RawMessage `json:"items"`
		Points []json.RawMessage `json:"points"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: %v: %s", url, err, rr.Body.String())
	}
	return rr.Code, len(body.Items) + len(body.Points)
}

func TestHistoryAndSeriesClampLargePoints(t *testing.T) {
	srv := newHistoryPointsServer(t)
	for _, url := range []string{
		"/api/history?range=1h&points=5000",
		"/api/history?range=1h&points=1000000000",
		"/api/series?driver=meter&metric=meter_w&range=1h&points=5000",
		"/api/series?driver=meter&metric=meter_w&range=1h&points=1000000000",
	} {
		code, n := historyPointsGet(t, srv, url)
		if code != http.StatusOK {
			t.Errorf("%s: status %d, want 200 with points clamped", url, code)
			continue
		}
		if n == 0 || n > historyMaxPoints {
			t.Errorf("%s: %d rows, want 1..%d", url, n, historyMaxPoints)
		}
	}
}

func TestHistoryAndSeriesRefuseBadPoints(t *testing.T) {
	srv := newHistoryPointsServer(t)
	for _, base := range []string{
		"/api/history?range=1h&points=",
		"/api/series?driver=meter&metric=meter_w&range=1h&points=",
	} {
		for _, bad := range []string{"-1", "-5000", "abc", "1.5", "99999999999999999999"} {
			if code, _ := historyPointsGet(t, srv, base+bad); code != http.StatusBadRequest {
				t.Errorf("%s%s: status %d, want 400", base, bad, code)
			}
		}
	}
}

func TestHistoryPointsZeroAndOmittedKeepDashboardDefault(t *testing.T) {
	srv := newHistoryPointsServer(t)
	for _, url := range []string{
		"/api/history?range=1h",
		"/api/history?range=1h&points=0",
	} {
		code, n := historyPointsGet(t, srv, url)
		if code != http.StatusOK || n == 0 || n > 200 {
			t.Errorf("%s: status %d, %d rows; want 200 and 1..200 rows", url, code, n)
		}
	}
	if code, n := historyPointsGet(t, srv, "/api/history?range=1h&points=360"); code != http.StatusOK || n <= 200 || n > 360 {
		t.Errorf("points=360: status %d, %d rows; want the dashboard's 360-point budget", code, n)
	}
}

func TestSeriesPointsZeroAndOmittedReturnStoredRows(t *testing.T) {
	srv := newHistoryPointsServer(t)
	for _, url := range []string{
		"/api/series?driver=meter&metric=meter_w&range=1h",
		"/api/series?driver=meter&metric=meter_w&range=1h&points=0",
	} {
		code, n := historyPointsGet(t, srv, url)
		if code != http.StatusOK || n != storedRows {
			t.Errorf("%s: status %d, %d rows; want 200 and all %d stored rows", url, code, n, storedRows)
		}
	}
}
