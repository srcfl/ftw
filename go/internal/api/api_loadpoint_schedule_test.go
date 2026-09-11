package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/state"
)

func TestScheduleStorageFailureKeepsGoalAndRetrySaves(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, body string
		clear                    bool
	}{
		{"put", http.MethodPut, "/schedule", `{"soc_pct":90,"time_of_day_min_utc":420}`, false},
		{"delete", http.MethodDelete, "/schedule", "", true},
		{"put_null", http.MethodPut, "/schedule", "null", true},
		{"target_set", http.MethodPost, "/target", `{"schedule":{"soc_pct":90,"time_of_day_min_utc":420}}`, false},
		{"target_clear", http.MethodPost, "/target", `{"schedule":null}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, mgr, svc := newScheduleServer(t)
			path := filepath.Join(t.TempDir(), "goals.db")
			disk, err := state.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { disk.Close() })
			mgr.SetScheduleSaver(func(_ string, s loadpoint.Schedule) error {
				b, err := json.Marshal(s)
				if err != nil {
					return err
				}
				return disk.SaveConfig("goal", string(b))
			})
			old := loadpoint.Schedule{SoC: .8, TimeOfDayMinUTC: 360, Recurring: true}
			if !mgr.SetSchedule("garage", old) {
				t.Fatal("initial save failed")
			}
			mgr.RollSchedules(time.Now())
			before, _ := mgr.State("garage")
			if err := disk.Close(); err != nil {
				t.Fatal(err)
			}
			request := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest(tc.method, "/api/loadpoints/garage"+tc.path, strings.NewReader(tc.body))
				r.Header.Set("Content-Type", "application/json")
				rr := httptest.NewRecorder()
				srv.Handler().ServeHTTP(rr, r)
				return rr
			}
			rr := request()
			if rr.Code != http.StatusInternalServerError || !strings.Contains(rr.Body.String(), "previous goal is unchanged") {
				t.Fatalf("failed storage returned %d: %s", rr.Code, rr.Body.String())
			}
			after, _ := mgr.State("garage")
			if after.Schedule != old || after.TargetSoC != before.TargetSoC || after.TargetTime != before.TargetTime {
				t.Fatalf("failed request changed the running goal: %+v", after)
			}
			if _, reason := svc.LastReplanInfo(); reason != "" {
				t.Fatalf("failed save triggered replan: %s", reason)
			}
			disk, err = state.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if rr = request(); rr.Code != http.StatusOK {
				t.Fatalf("retry returned %d: %s", rr.Code, rr.Body.String())
			}
			raw, found := disk.LoadConfig("goal")
			var saved loadpoint.Schedule
			if !found || json.Unmarshal([]byte(raw), &saved) != nil {
				t.Fatalf("retry did not persist a goal: %q", raw)
			}
			after, _ = mgr.State("garage")
			if tc.clear {
				if !saved.Empty() || !after.Schedule.Empty() || after.TargetSoC != 0 || !after.TargetTime.IsZero() {
					t.Fatalf("retry failed to remove goal: saved=%+v state=%+v", saved, after)
				}
			} else if saved.SoC != .9 || after.Schedule != saved || after.TargetSoC != .9 || after.TargetTime.IsZero() {
				t.Fatalf("retry did not apply the saved goal: saved=%+v state=%+v", saved, after)
			}
		})
	}
}

// The schedule-only route. Its tier is pinned alongside the other
// verb-blind cases in TestRouteTierIgnoresTheMethod; these tests cover
// what the handlers do: PUT stores and rolls, DELETE clears, and both
// force a replan tagged with the schedule-change reason.

// newScheduleServer wires a manager and an MPC service with enough input for
// the route-triggered replan to publish its plan and reason together.
func newScheduleServer(t *testing.T) (*Server, *loadpoint.Manager, *mpc.Service) {
	t.Helper()
	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{{ID: "garage", DriverName: "easee"}})
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("opening state store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	start := time.Now().UTC().Truncate(15 * time.Minute)
	prices := make([]state.PricePoint, 4)
	for i := range prices {
		prices[i] = state.PricePoint{
			Zone: "SE4", SlotTsMs: start.Add(time.Duration(i) * 15 * time.Minute).UnixMilli(),
			SlotLenMin: 15, SpotOreKwh: 50, TotalOreKwh: 100,
			Source: "test", FetchedAtMs: start.UnixMilli(),
		}
	}
	if err := st.SavePrices(prices); err != nil {
		t.Fatalf("saving prices: %v", err)
	}
	svc := mpc.New(st, nil, "SE4", mpc.Params{
		Mode: mpc.ModeSelfConsumption, SoCLevels: 11, ActionLevels: 5,
		CapacityWh: 10000, InitialSoC: 0.5, SoCMin: 0.1, SoCMax: 0.95,
		MaxChargeW: 3000, MaxDischargeW: 3000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
	})
	svc.Horizon = time.Hour
	svc.BaseLoad = 500
	t.Cleanup(func() { waitForSchedulePlan(t, svc) })
	return New(&Deps{Loadpoints: mgr, MPC: svc}), mgr, svc
}

func putSchedule(t *testing.T, srv *Server, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/loadpoints/"+id+"/schedule", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

func TestSchedulePutStoresRollsAndReplans(t *testing.T) {
	srv, mgr, svc := newScheduleServer(t)

	rr := putSchedule(t, srv, "garage",
		`{"soc_pct":80,"time_of_day_min_utc":360,"recurring":true,"days":31}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}

	got, ok := mgr.GetSchedule("garage")
	if !ok {
		t.Fatal("PUT did not store the schedule")
	}
	want := loadpoint.Schedule{SoC: 0.8, TimeOfDayMinUTC: 360, Recurring: true, Days: 31}
	if got != want {
		t.Fatalf("stored schedule = %+v, want %+v", got, want)
	}

	// The handler rolls immediately: the derived one-shot target is
	// seeded before the response goes out.
	lpState, _ := mgr.State("garage")
	if lpState.TargetTime.IsZero() || !lpState.TargetTime.After(time.Now()) {
		t.Fatalf("PUT did not roll: target_time = %v", lpState.TargetTime)
	}
	if lpState.TargetSoC != 0.8 {
		t.Fatalf("PUT did not roll: target_soc_pct = %v, want 80", lpState.TargetSoC)
	}

	waitForSchedulePlan(t, svc)
	if _, reason := svc.LastReplanInfo(); reason != "loadpoint_schedule_changed" {
		t.Fatalf("replan reason = %q, want loadpoint_schedule_changed", reason)
	}
}

func TestScheduleDeleteClearsAndReplans(t *testing.T) {
	srv, mgr, svc := newScheduleServer(t)
	// Seeded on the manager directly, so the replan reason below can
	// only have come from the DELETE.
	mgr.SetSchedule("garage", loadpoint.Schedule{SoC: 0.8, TimeOfDayMinUTC: 360, Recurring: true})
	mgr.RollSchedules(time.Now())

	req := httptest.NewRequest(http.MethodDelete, "/api/loadpoints/garage/schedule", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}

	if _, ok := mgr.GetSchedule("garage"); ok {
		t.Fatal("DELETE did not clear the schedule")
	}
	state, _ := mgr.State("garage")
	if state.TargetSoC != 0 || !state.TargetTime.IsZero() {
		t.Fatalf("DELETE left an active target after removing the goal: %+v", state)
	}
	waitForSchedulePlan(t, svc)
	if _, reason := svc.LastReplanInfo(); reason != "loadpoint_schedule_changed" {
		t.Fatalf("replan reason = %q, want loadpoint_schedule_changed", reason)
	}
}

// PUT with a JSON null body clears too — the same signal the target
// route's embedded schedule field accepts.
func TestSchedulePutNullClears(t *testing.T) {
	srv, mgr, _ := newScheduleServer(t)
	mgr.SetSchedule("garage", loadpoint.Schedule{SoC: 0.8, TimeOfDayMinUTC: 360, Recurring: true})

	rr := putSchedule(t, srv, "garage", `null`)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT null status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if _, ok := mgr.GetSchedule("garage"); ok {
		t.Fatal("PUT null did not clear the schedule")
	}
}

func TestSchedulePutValidates(t *testing.T) {
	srv, _, _ := newScheduleServer(t)
	cases := []struct {
		name string
		id   string
		body string
		want int
	}{
		{"time of day too large", "garage", `{"soc_pct":80,"time_of_day_min_utc":1440}`, http.StatusBadRequest},
		{"negative time of day", "garage", `{"soc_pct":80,"time_of_day_min_utc":-1}`, http.StatusBadRequest},
		{"days beyond seven bits", "garage", `{"soc_pct":80,"time_of_day_min_utc":360,"days":128}`, http.StatusBadRequest},
		{"days not a number", "garage", `{"soc_pct":80,"days":"weekdays"}`, http.StatusBadRequest},
		{"malformed json", "garage", `{`, http.StatusBadRequest},
		{"empty body", "garage", ``, http.StatusBadRequest},
		{"unknown loadpoint", "ghost", `{"soc_pct":80,"time_of_day_min_utc":360}`, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rr := putSchedule(t, srv, tc.id, tc.body); rr.Code != tc.want {
				t.Errorf("status = %d, want %d (body: %s)", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

func TestScheduleRoutesWithoutLoadpoints(t *testing.T) {
	srv := New(&Deps{}) // no Loadpoints wired
	if rr := putSchedule(t, srv, "garage", `{"soc_pct":80}`); rr.Code != http.StatusNotFound {
		t.Errorf("PUT without loadpoints: status = %d, want 404", rr.Code)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/loadpoints/garage/schedule", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("DELETE without loadpoints: status = %d, want 404", rr.Code)
	}
}

// The target route keeps carrying an embedded schedule for the on-box
// UI — the schedule-only route is an addition, not a move. Guards the
// extraction of the shared apply path.
func TestTargetRouteStillCarriesSchedule(t *testing.T) {
	srv, mgr, svc := newScheduleServer(t)

	body := `{"schedule":{"soc_pct":70,"time_of_day_min_utc":420,"recurring":true}}`
	req := httptest.NewRequest(http.MethodPost, "/api/loadpoints/garage/target", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST target status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	got, ok := mgr.GetSchedule("garage")
	if !ok || got.SoC != 0.7 || got.TimeOfDayMinUTC != 420 {
		t.Fatalf("target route stopped storing schedules: got=%+v ok=%v", got, ok)
	}
	waitForSchedulePlan(t, svc)
	if _, reason := svc.LastReplanInfo(); reason != "loadpoint_schedule_changed" {
		t.Fatalf("replan reason = %q, want loadpoint_schedule_changed", reason)
	}

	// And the embedded null still clears.
	req = httptest.NewRequest(http.MethodPost, "/api/loadpoints/garage/target",
		strings.NewReader(`{"schedule":null}`))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST target schedule:null status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if _, ok := mgr.GetSchedule("garage"); ok {
		t.Fatal("target route stopped clearing schedules via null")
	}
}

func waitForSchedulePlan(t *testing.T, svc *mpc.Service) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for svc.IsReplanning() {
		if time.Now().After(deadline) {
			t.Fatal("planner did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}
