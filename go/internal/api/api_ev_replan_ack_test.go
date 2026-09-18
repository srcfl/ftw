package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/state"
)

func TestEVSettingsAcknowledgeWhilePlanIsBlocked(t *testing.T) {
	for _, tc := range []struct{ name, method, path, body string }{
		{"goal", "PUT", "/schedule", `{"soc":0.8,"time_of_day_min_utc":420}`},
		{"remove", "DELETE", "/schedule", ""},
		{"target_goal", "POST", "/target", `{"schedule":{"soc":0.9,"time_of_day_min_utc":480}}`},
		{"solar", "POST", "/target", `{"surplus_only":false}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, mgr, svc := newScheduleServer(t)
			now := time.Now().UTC().Truncate(15 * time.Minute)
			cloud := 0.0
			if err := svc.Store.SaveForecasts([]state.ForecastPoint{{SlotTsMs: now.UnixMilli(), SlotLenMin: 60,
				CloudCoverPct: &cloud, Source: "test", FetchedAtMs: now.UnixMilli()}}); err != nil {
				t.Fatal(err)
			}
			mgr.SetSchedule("garage", loadpoint.Schedule{SoC: .7, TimeOfDayMinUTC: 360})
			mgr.SetSurplusOnly("garage", true)
			entered, release := make(chan struct{}), make(chan struct{})
			var enterOnce, releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			svc.PV = func(time.Time, float64) float64 { enterOnce.Do(func() { close(entered) }); <-release; return 0 }
			req := httptest.NewRequest(tc.method, "/api/loadpoints/garage"+tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { srv.Handler().ServeHTTP(rr, req); close(done) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("planner did not enter blocked predictor")
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				unblock()
				<-done
				t.Fatal("saved settings waited for planner before acknowledgement")
			}
			if rr.Code != http.StatusOK {
				t.Fatalf("write=%d %s", rr.Code, rr.Body.String())
			}
			if !svc.IsReplanning() {
				t.Fatal("pending plan hidden")
			}
			read := func(path string) map[string]json.RawMessage {
				t.Helper()
				rr := httptest.NewRecorder()
				srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
				if rr.Code != 200 {
					t.Fatalf("read %s=%d", path, rr.Code)
				}
				var body map[string]json.RawMessage
				if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				return body
			}
			body := read("/api/loadpoints")
			var points []loadpoint.State
			if err := json.Unmarshal(body["loadpoints"], &points); err != nil {
				t.Fatal(err)
			}
			if len(points) != 1 || !points[0].PlanPending || len(points[0].PlanWindows) != 0 {
				t.Fatalf("pending point=%s", body["loadpoints"])
			}
			if tc.name == "goal" && points[0].Schedule.SoC != .8 {
				t.Fatalf("ack without saved goal: %+v", points[0].Schedule)
			}
			if tc.name == "remove" && !points[0].Schedule.Empty() {
				t.Fatal("ack without clearing goal")
			}
			if tc.name == "solar" && points[0].SurplusOnly {
				t.Fatal("ack without changed solar rule")
			}
			var meta struct {
				Replanning bool `json:"replanning"`
			}
			if err := json.Unmarshal(read("/api/mpc/plan")["meta"], &meta); err != nil {
				t.Fatal(err)
			}
			if !meta.Replanning {
				t.Fatal("plan API hides pending work")
			}
			unblock()
			waitForSchedulePlan(t, svc)
			if svc.IsReplanning() {
				t.Fatal("pending did not clear")
			}
			if plan := svc.Latest(); plan == nil {
				t.Fatal("acknowledged change never produced a plan")
			}
		})
	}
}
