package api

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestHealthShowsRejectedPrimaryHistory(t *testing.T) {
	srv, st, _ := newSeriesTestServer(t)
	srv.deps.Tel = telemetry.NewStore()
	if err := st.EnqueueTelemetryTick(nil, []state.Sample{{Driver: "meter", Metric: "power", Value: math.NaN()}}, nil); err == nil {
		t.Fatal("invalid tick accepted")
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	var body struct {
		Status  string `json:"status"`
		History struct {
			Engine string                    `json:"engine"`
			Role   string                    `json:"role"`
			Writer state.HistoryWriterStatus `json:"writer"`
		} `json:"history_storage"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rr.Code != 200 || body.Status != "degraded" || body.History.Engine != "duckdb" || body.History.Role != "primary" || body.History.Writer.Rejected != 1 || body.History.Writer.Committed != 0 {
		t.Fatalf("health hid a collection error: %s", rr.Body.String())
	}
}
