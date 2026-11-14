package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/thermaltwin"
)

type twinTele struct{}

func (twinTele) LatestMetric(string, string) (float64, time.Time, bool) { return 0, time.Time{}, false }

func getThermalTwins(t *testing.T, srv *Server) map[string]json.RawMessage {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/thermal/twins", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/thermal/twins = %d, body %s", rec.Code, rec.Body.String())
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

// Every answer says shadow, whether or not twins exist, so no reader can
// take a trusted twin for one that steers anything.
func TestThermalTwinsAreAlwaysMarkedShadow(t *testing.T) {
	got := getThermalTwins(t, New(&Deps{}))
	if string(got["shadow"]) != "true" || string(got["enabled"]) != "false" || string(got["twins"]) != "[]" {
		t.Errorf("without a service = %v", got)
	}

	svc := thermaltwin.NewService(nil, twinTele{})
	svc.Zones = func() []thermaltwin.Zone {
		return []thermaltwin.Zone{{Key: "wiser/room_temp_c", SensorDriver: "wiser", SensorMetric: "room_temp_c"}}
	}
	svc.Tick(time.Now())
	got = getThermalTwins(t, New(&Deps{ThermalTwin: svc}))
	if string(got["shadow"]) != "true" || string(got["enabled"]) != "true" {
		t.Errorf("with a service = %v", got)
	}
	var twins []thermaltwin.ZoneStatus
	if err := json.Unmarshal(got["twins"], &twins); err != nil {
		t.Fatal(err)
	}
	if len(twins) != 1 || twins[0].Key != "wiser/room_temp_c" || twins[0].Status != thermaltwin.StatusNoHeatInput {
		t.Errorf("twins = %+v", twins)
	}
}
