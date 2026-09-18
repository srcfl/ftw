package api

import (
	"context"
	"github.com/srcfl/ftw/go/internal/state"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestResearchDoesNotPairMeanPowerWithLatestEVObservation(t *testing.T) {
	rows := []state.HistoryPoint{
		{TsMs: 1000, GridW: 3000, LoadW: 1000, JSON: `{"ev_w":2000}`},
		{TsMs: 2000, GridW: 4000, LoadW: 1000, N: 2, ResolutionMS: 10000, JSON: `{"ev_w":6000}`},
		{TsMs: 3000, GridW: 9000, JSON: `{"forecast_measurement_quality":"aggregate_not_for_training","ev_w":6000}`},
	}
	got := buildLoadResearchBuckets(rows)
	if len(got) != 1 || got[0].count != 1 || got[0].gridW != 3000 || got[0].evW != 2000 || got[0].houseLoadW != 1000 {
		t.Fatal(got)
	}
}

func TestResearchExplainsWhenOnlyDashboardSummariesRemain(t *testing.T) {
	srv, st, _ := newSeriesTestServer(t)
	if err := st.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Minute).UnixMilli()
	p := state.HistoryPoint{TsMs: base, GridW: 3000, JSON: `{"ev_w":2000}`}
	if err := st.EnqueueTelemetryTick(&p, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	srv.handleLoadResearchDump(rr, httptest.NewRequest(http.MethodGet, "/api/research/load/dump?days=1", nil))
	if rr.Code != http.StatusConflict {
		t.Fatal(rr.Code, rr.Body.String())
	}
}
