package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
			Engine  string                    `json:"engine"`
			Archive string                    `json:"archive"`
			Writer  state.HistoryWriterStatus `json:"writer"`
		} `json:"history_storage"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rr.Code != 200 || body.Status != "degraded" || body.History.Engine != "sqlite" || body.History.Archive != "sqlite" || body.History.Writer.Rejected != 1 || body.History.Writer.Committed != 0 {
		t.Fatalf("health hid a collection error: %s", rr.Body.String())
	}
}

func TestHealthShowsFailedArchiveAndRecovery(t *testing.T) {
	srv, st, _ := newSeriesTestServer(t)
	srv.deps.Tel = telemetry.NewStore()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := st.MaintainHistory(ctx, t.TempDir(), 0, time.Now()); err == nil {
		t.Fatal("cancelled maintenance succeeded")
	}
	check := func(want string) {
		t.Helper()
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/health", nil))
		var body struct {
			Status  string `json:"status"`
			History struct {
				Maintenance state.HistoryMaintenanceStatus `json:"maintenance"`
			} `json:"history_storage"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Status != want || body.History.Maintenance.Failures != 1 || body.History.Maintenance.LastFailureMS == 0 {
			t.Fatalf("maintenance status missing: %s", rr.Body.String())
		}
	}
	check("degraded")
	if err := st.MaintainHistory(context.Background(), t.TempDir(), 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	check("ok")
	status := st.HistoryMaintenanceStatus()
	if status.State != "complete" || status.LastError != "" || status.LastFailureError == "" || status.LastSuccessMS == 0 {
		t.Fatalf("recovery status=%+v", status)
	}
}
