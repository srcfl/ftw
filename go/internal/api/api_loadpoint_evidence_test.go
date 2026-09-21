package api

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/assistant"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestChargingObservationKeepsMissingAndStaleDistinctFromZero(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	oldPower := now.Add(-time.Hour).Format(time.RFC3339Nano)
	cases := []struct {
		name, data, carrier, source, state string
		age                                time.Duration
		watts                              float64
		knownPower, knownConnection        bool
		offline                            bool
	}{
		{name: "drawing", data: `{"connected":true}`, watts: 4100, carrier: "live", source: "live", state: "charging", knownPower: true, knownConnection: true},
		{name: "real zero", data: `{"connected":true}`, carrier: "live", source: "live", state: "not_drawing", knownPower: true, knownConnection: true},
		{name: "unplugged", data: `{"connected":false}`, carrier: "live", source: "live", state: "disconnected", knownPower: true, knownConnection: true},
		{name: "unknown connection", data: `{}`, carrier: "live", source: "live", state: "unknown", knownPower: true},
		{name: "connection reset", data: `{"connected":false,"connection_unknown":true}`, carrier: "live", source: "live", state: "unknown", knownPower: true},
		{name: "stale carrier", data: `{"connected":true}`, age: 2 * time.Minute, watts: 4100, carrier: "stale", source: "unknown", state: "unknown"},
		{name: "offline health", data: `{"connected":true}`, watts: 4100, offline: true, carrier: "stale", source: "unknown", state: "unknown"},
		{name: "cloud unavailable", data: `{"connected":true,"is_online":false}`, watts: 4100, carrier: "live", source: "unknown", state: "unknown"},
		{name: "cached zero", data: `{"connected":true,"power_observed_at":"` + oldPower + `"}`, carrier: "live", source: "stale", state: "unknown", knownConnection: true},
		{name: "cached power", data: `{"connected":true,"power_observed_at":"` + oldPower + `"}`, watts: 4100, carrier: "live", source: "stale", state: "unknown", knownConnection: true},
		{name: "invalid source time", data: `{"connected":true,"power_observed_at":"yesterday"}`, carrier: "live", source: "unknown", state: "unknown", knownConnection: true},
		{name: "future source time", data: `{"connected":true,"power_observed_at":"2026-09-22T12:00:00Z"}`, carrier: "live", source: "stale", state: "unknown", knownConnection: true},
		{name: "future receipt", data: `{"connected":true}`, age: -time.Minute, carrier: "stale", source: "unknown", state: "unknown"},
		{name: "bad payload", data: `{`, carrier: "live", source: "unknown", state: "unknown"},
		{name: "nonfinite power", data: `{"connected":true}`, watts: math.NaN(), carrier: "live", source: "unknown", state: "unknown", knownConnection: true},
		{name: "negative EV power", data: `{"connected":true}`, watts: -100, carrier: "live", source: "unknown", state: "unknown", knownConnection: true},
		{name: "contradicting connection", data: `{"connected":false}`, watts: 4100, carrier: "live", source: "live", state: "unknown", knownPower: true, knownConnection: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rd := &telemetry.DerReading{UpdatedAt: now.Add(-tc.age), RawW: tc.watts, Data: json.RawMessage(tc.data)}
			health := &telemetry.DriverHealth{}
			if tc.offline {
				health.SetOffline()
			}
			got := chargingObservationFrom(rd, health, time.Minute, now)
			if got.Carrier != tc.carrier || got.SrcState != tc.source || got.State != tc.state ||
				(got.PowerW != nil) != tc.knownPower || (got.Connected != nil) != tc.knownConnection {
				t.Fatalf("observation = %+v", got)
			}
			if got.PowerW != nil && *got.PowerW != tc.watts {
				t.Fatalf("power = %v, want %v", *got.PowerW, tc.watts)
			}
			b, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.knownPower && !strings.Contains(string(b), `"power_w":null`) {
				t.Fatalf("missing power must be explicit null: %s", b)
			}
		})
	}
	if got := chargingObservationFrom(nil, nil, time.Minute, now); got.PowerW != nil || got.Connected != nil || got.State != "unknown" {
		t.Fatalf("missing reading = %+v", got)
	}
}

func TestChargingObservationBoundsSourceCadenceSeparatelyFromCarrier(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		maxAgeS int
		age     time.Duration
		live    bool
	}{
		{0, 31 * time.Second, false},
		{180, 2 * time.Minute, true},
		{3600, 4 * time.Minute, false},
	} {
		data, _ := json.Marshal(map[string]any{"connected": true, "power_observed_at": now.Add(-tc.age).Format(time.RFC3339Nano), "power_max_age_s": tc.maxAgeS, "max_a": 16})
		rd := &telemetry.DerReading{UpdatedAt: now, RawW: 11000, Data: data}
		got := chargingObservationFrom(rd, nil, time.Minute, now)
		if got.Carrier != "live" || (got.PowerW != nil) != tc.live || got.ChargerLimitA == nil || *got.ChargerLimitA != 16 {
			t.Fatalf("cadence %+v: %+v", tc, got)
		}
	}
}

func TestChargingEvidenceHTTPAndAssistantShareGoalsAndObservations(t *testing.T) {
	srv, _ := newManualHoldServer(t)
	mgr := srv.deps.Loadpoints
	mgr.Observe("garage", true, 4100, 200, true)
	deadline := time.Now().UTC().Add(8 * time.Hour).Truncate(time.Second)
	mgr.SetTarget("garage", .8, deadline)
	mgr.SetCommanded("garage", 0, "site_meter_stale")
	before, _ := mgr.State("garage")
	tel := telemetry.NewStore()
	tel.Update("easee", telemetry.DerEV, 0, nil, json.RawMessage(`{"connected":true,"max_a":0,"password":"not-for-the-evidence"}`))
	srv.deps.Tel = tel

	req := httptest.NewRequest(http.MethodGet, "/api/loadpoints/garage/evidence", nil)
	if srv.Route(req).Tier != apiauth.TierRead {
		t.Fatal("evidence must use the existing read authority")
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("GET = %d %s", rr.Code, rr.Body.String())
	}
	var httpResult chargingEvidence
	if err := json.Unmarshal(rr.Body.Bytes(), &httpResult); err != nil {
		t.Fatal(err)
	}
	tool, err := srv.runAssistantTool(assistant.ToolChargingEvidence, json.RawMessage(`{"id":"garage"}`))
	if err != nil {
		t.Fatal(err)
	}
	var toolResult chargingEvidence
	if err := json.Unmarshal([]byte(tool), &toolResult); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(httpResult.Loadpoint, toolResult.Loadpoint) || !reflect.DeepEqual(httpResult.Observation, toolResult.Observation) {
		t.Fatalf("HTTP and tool disagree: %+v / %+v", httpResult, toolResult)
	}
	ui := httptest.NewRecorder()
	srv.Handler().ServeHTTP(ui, httptest.NewRequest(http.MethodGet, "/api/loadpoints", nil))
	var uiResult struct {
		Loadpoints []json.RawMessage `json:"loadpoints"`
	}
	if err := json.Unmarshal(ui.Body.Bytes(), &uiResult); err != nil || len(uiResult.Loadpoints) != 1 {
		t.Fatalf("UI read = %s, %v", ui.Body.String(), err)
	}
	uiState, _ := json.Marshal(httpResult.Loadpoint)
	if string(uiResult.Loadpoints[0]) != string(uiState) {
		t.Fatalf("UI and evidence disagree: %s / %s", uiResult.Loadpoints[0], uiState)
	}
	got := httpResult
	if got.SchemaVersion != 1 || got.CollectedAtMs <= 0 || got.ReadCompletedAtMs < got.CollectedAtMs ||
		got.Loadpoint.TargetSoC != .8 || !got.Loadpoint.TargetTime.Equal(deadline) || got.Loadpoint.CommandedReason != "site_meter_stale" ||
		got.Observation.State != "not_drawing" || got.Observation.PowerW == nil || *got.Observation.PowerW != 0 {
		t.Fatalf("lost goal, decision, or actual observation: %+v", got)
	}
	// The manager's earlier 4100 W must not override the driver's fresh zero.
	if got.Loadpoint.CurrentPowerW != 4100 {
		t.Fatal("the controller view should retain its own observation")
	}
	if strings.Contains(tool, "not-for-the-evidence") || strings.Contains(rr.Body.String(), "not-for-the-evidence") {
		t.Fatal("raw driver data leaked into evidence")
	}
	after, _ := mgr.State("garage")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("reading evidence changed controller state")
	}
}

func TestAskWhyCanReadChargingEvidence(t *testing.T) {
	var sawEvidence bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Tools []assistant.ToolDef `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, msg := range request.Messages {
			if msg.Role != "tool" {
				continue
			}
			var evidence chargingEvidence
			if err := json.Unmarshal([]byte(msg.Content), &evidence); err != nil || evidence.Loadpoint.ID != "garage" || evidence.Observation.State != "unknown" {
				t.Errorf("model received invalid or invented evidence: %s", msg.Content)
			}
			sawEvidence = true
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"## Answer\nCharger power is unknown.\n\n## Issue title\n\n## Issue body\n"}}]}`))
			return
		}
		advertised := false
		for _, tool := range request.Tools {
			advertised = advertised || tool.Function.Name == assistant.ToolChargingEvidence
		}
		if !advertised {
			t.Error("charging tool was not advertised to the model")
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"charging-1","type":"function","function":{"name":"get_charging_evidence","arguments":"{\"id\":\"garage\"}"}}]}}]}`))
	}))
	defer upstream.Close()
	srv := assistantTestServer(t, &config.Assistant{Enabled: true, APIKey: "test-key", Model: "test", BaseURL: upstream.URL}, upstream.Client())
	charging, _ := newManualHoldServer(t)
	srv.deps.Loadpoints = charging.deps.Loadpoints
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, postAssistantAsk("Why is the car not charging?"))
	if rr.Code != http.StatusOK || !sawEvidence || !strings.Contains(rr.Body.String(), "Charger power is unknown") {
		t.Fatalf("Ask why = %d %s; saw evidence: %v", rr.Code, rr.Body.String(), sawEvidence)
	}
}

func TestChargingEvidenceDoesNotTreatACommandAsCharging(t *testing.T) {
	srv, _ := newManualHoldServer(t)
	srv.deps.Loadpoints.Observe("garage", true, 0, 0, true)
	srv.deps.Loadpoints.SetCommanded("garage", 11000, "manual_hold")
	got, ok := srv.loadpointEvidence("garage")
	if !ok || got.Loadpoint.CommandedW != 11000 || got.Observation.State != "unknown" || got.Observation.PowerW != nil {
		t.Fatalf("a command invented charging: %+v", got)
	}
}

func TestChargingEvidenceDistinguishesMissingWaitingAndOutdatedPlans(t *testing.T) {
	srv, mgr, svc := newScheduleServer(t)
	now := time.Now()
	mgr.Observe("garage", true, 0, 0, true)
	mgr.SetTarget("garage", .8, now.Add(8*time.Hour))
	mgr.SetCommanded("garage", 0, "no_plan_budget")
	srv.deps.Tel = telemetry.NewStore()
	srv.deps.Tel.Update("easee", telemetry.DerEV, 0, nil, json.RawMessage(`{"connected":true}`))
	missing, _ := srv.loadpointEvidence("garage")
	if missing.PlanStatus != "missing" {
		t.Fatalf("no plan = %+v", missing)
	}
	start := now.Add(time.Hour).UnixMilli()
	svc.InstallPlan(mpc.Plan{GeneratedAtMs: now.UnixMilli(), Actions: []mpc.Action{{
		SlotStartMs: start, SlotLenMin: 15, LoadpointPowerW: map[string]float64{"garage": 6000},
	}}}, svc.Defaults, "garage")
	waiting, _ := srv.loadpointEvidence("garage")
	if waiting.PlanStatus != "current" || waiting.PlanGeneratedAtMs != now.UnixMilli() ||
		waiting.Loadpoint.PlanNextStartMs != start || waiting.Loadpoint.PlanNextWh != 1500 ||
		waiting.Loadpoint.CommandedReason != "no_plan_budget" || waiting.Observation.State != "not_drawing" {
		t.Fatalf("waiting for a future window = %+v", waiting)
	}
	// A failed replan retains old diagnostics but cannot promise the old
	// window for the saved goal. The goal itself must remain present.
	svc.Zone = "no-prices"
	svc.RequestReplan("saved-goal")
	waitForSchedulePlan(t, svc)
	outdated, _ := srv.loadpointEvidence("garage")
	if outdated.PlanStatus != "outdated" || !outdated.Loadpoint.PlanOutdated ||
		len(outdated.Loadpoint.PlanWindows) != 0 || outdated.Loadpoint.PlanNextStartMs != 0 || outdated.Loadpoint.TargetSoC != .8 {
		t.Fatalf("failed replan invented a current plan or lost the goal: %+v", outdated)
	}
}

func TestChargingEvidenceDiscoveryAndMissingLoadpoint(t *testing.T) {
	for _, srv := range []*Server{New(&Deps{}), func() *Server { s, _ := newManualHoldServer(t); return s }()} {
		_, err := srv.toolChargingEvidence(json.RawMessage(`{`))
		if err == nil {
			t.Fatal("malformed arguments accepted")
		}
		if _, err := srv.toolChargingEvidence(json.RawMessage(`{"id":"missing"}`)); err == nil {
			t.Fatal("unknown id accepted")
		}
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/loadpoints/missing/evidence", nil))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("unknown id = %d", rr.Code)
		}
		out, err := srv.toolChargingEvidence(json.RawMessage(`{}`))
		if err != nil || !json.Valid([]byte(out)) {
			t.Fatalf("discovery = %s, %v", out, err)
		}
		if srv.deps.Loadpoints == nil && out != `{"loadpoint_ids":[],"truncated":false}` {
			t.Fatalf("disabled loadpoints = %s", out)
		}
		if srv.deps.Loadpoints != nil && out != `{"loadpoint_ids":["garage"],"truncated":false}` {
			t.Fatalf("configured loadpoints = %s", out)
		}
	}
	if !assistant.AllowedTool(assistant.ToolChargingEvidence) {
		t.Fatal("tool is not callable")
	}
}
