package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
	"github.com/srcfl/ftw/go/internal/loadmodel"
	"github.com/srcfl/ftw/go/internal/pvmodel"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

type forecastLearningAPIMock struct {
	status map[string]forecasting.LearningStatus
	err    error
	calls  []string
}

func (m *forecastLearningAPIMock) LearningStatus(signal string) forecasting.LearningStatus {
	return m.status[signal]
}

func (m *forecastLearningAPIMock) RestartLearning(_ context.Context, signal string) error {
	m.calls = append(m.calls, signal)
	if m.err != nil {
		return m.err
	}
	status := m.status[signal]
	status.Status = "learning"
	status.StartedMS += 1000
	m.status[signal] = status
	return nil
}

type forecastLearningAPIResponse struct {
	Status   string                     `json:"status"`
	Error    string                     `json:"error"`
	Learning forecasting.LearningStatus `json:"learning"`
}

func decodeForecastLearningResponse(t *testing.T, rr *httptest.ResponseRecorder) forecastLearningAPIResponse {
	t.Helper()
	var got forecastLearningAPIResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v; body: %s", err, rr.Body.String())
	}
	return got
}

func TestForecastLearningEndpointsDelegateExactSignalAndReportCurrentStatus(t *testing.T) {
	learning := &forecastLearningAPIMock{status: map[string]forecasting.LearningStatus{
		"pv":   {Engine: "energyplan", Status: "ready", StartedMS: 100, LatestTrainingMS: 150, ResetAvailable: true},
		"load": {Engine: "energyplan", Status: "ready", StartedMS: 200, LatestTrainingMS: 250, ResetAvailable: true},
	}}
	srv := New(&Deps{
		PVModel:          pvmodel.NewService(nil, nil, nil, nil, 5000),
		LoadModel:        loadmodel.NewService(nil, telemetry.NewStore(), "site", 4000, 0),
		ForecastLearning: learning,
	})

	for _, tc := range []struct {
		signal string
		path   string
	}{
		{signal: "pv", path: "/api/pvmodel"},
		{signal: "load", path: "/api/loadmodel"},
	} {
		t.Run(tc.signal, func(t *testing.T) {
			get := httptest.NewRecorder()
			srv.Handler().ServeHTTP(get, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if get.Code != http.StatusOK {
				t.Fatalf("GET status = %d, body: %s", get.Code, get.Body.String())
			}
			before := decodeForecastLearningResponse(t, get)
			if before.Learning != learning.status[tc.signal] {
				t.Fatalf("GET learning = %+v, want %+v", before.Learning, learning.status[tc.signal])
			}

			post := httptest.NewRecorder()
			srv.Handler().ServeHTTP(post, httptest.NewRequest(http.MethodPost, tc.path+"/reset", nil))
			if post.Code != http.StatusOK {
				t.Fatalf("POST status = %d, body: %s", post.Code, post.Body.String())
			}
			after := decodeForecastLearningResponse(t, post)
			if after.Status != "reset" || after.Learning != learning.status[tc.signal] {
				t.Fatalf("POST response = %+v, current status = %+v", after, learning.status[tc.signal])
			}
		})
	}
	if got := strings.Join(learning.calls, ","); got != "pv,load" {
		t.Fatalf("restart signals = %q, want pv,load", got)
	}
}

func TestForecastLearningResetRefusesWithoutCoordinatorAndKeepsProfileActionSeparate(t *testing.T) {
	pv := pvmodel.NewService(nil, nil, nil, nil, 5000)
	pv.Residuals.Add(timeForForecastLearningTest(), 1000, 1500)
	load := loadmodel.NewService(nil, telemetry.NewStore(), "site", 4000, 0)
	srv := New(&Deps{PVModel: pv, LoadModel: load})
	profile := httptest.NewRecorder()
	profileRequest := httptest.NewRequest(http.MethodPost, "/api/loadmodel/profile", strings.NewReader(`{"profile":"away"}`))
	profileRequest.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(profile, profileRequest)
	if profile.Code != http.StatusOK {
		t.Fatalf("profile action status = %d, body: %s", profile.Code, profile.Body.String())
	}

	for _, path := range []string{"/api/pvmodel/reset", "/api/loadmodel/reset"} {
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, path, nil))
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s status = %d, want 503; body: %s", path, rr.Code, rr.Body.String())
		}
		got := decodeForecastLearningResponse(t, rr)
		if got.Status == "reset" || got.Error == "" {
			t.Fatalf("%s claimed success: %+v", path, got)
		}
	}
	if pv.Residuals.Len() != 1 {
		t.Fatal("PV legacy state was reset without the coordinator")
	}
	if load.Profile() != loadmodel.ProfileAway {
		t.Fatal("load learning reset changed the separate profile action")
	}
}

func TestForecastLearningPersistenceFailureReturns503WithoutLegacyReset(t *testing.T) {
	pv := pvmodel.NewService(nil, nil, nil, nil, 5000)
	pv.Residuals.Add(timeForForecastLearningTest(), 1000, 1500)
	wantErr := errors.New("persist learning boundary: disk full")
	learning := &forecastLearningAPIMock{
		status: map[string]forecasting.LearningStatus{"pv": {Engine: "energyplan", Status: "error", ResetAvailable: true}},
		err:    wantErr,
	}
	srv := New(&Deps{PVModel: pv, ForecastLearning: learning})

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/pvmodel/reset", nil))

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", rr.Code, rr.Body.String())
	}
	got := decodeForecastLearningResponse(t, rr)
	if got.Status != "error" || !strings.Contains(got.Error, wantErr.Error()) {
		t.Fatalf("failure response = %+v", got)
	}
	if pv.Residuals.Len() != 1 {
		t.Fatal("handler ran a legacy-only reset after coordinator failure")
	}
	if len(learning.calls) != 1 || learning.calls[0] != "pv" {
		t.Fatalf("restart calls = %v", learning.calls)
	}
}

func TestForecastLearningDurablePendingErrorReturns503Pending(t *testing.T) {
	cause := errors.New("load model save failed")
	learning := &forecastLearningAPIMock{
		status: map[string]forecasting.LearningStatus{"load": {Engine: "energyplan", Status: "pending", StartedMS: 1234, ResetAvailable: true}},
		err:    &forecasting.LearningRestartPendingError{Err: cause},
	}
	srv := New(&Deps{LoadModel: loadmodel.NewService(nil, telemetry.NewStore(), "site", 4000, 0), ForecastLearning: learning})

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/loadmodel/reset", nil))

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", rr.Code, rr.Body.String())
	}
	got := decodeForecastLearningResponse(t, rr)
	if got.Status != "pending" || got.Learning != learning.status["load"] || !strings.Contains(got.Error, cause.Error()) {
		t.Fatalf("pending response = %+v", got)
	}
}

func timeForForecastLearningTest() time.Time {
	return time.Unix(1, 0)
}
