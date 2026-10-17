package control

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// This is a synthetic ambiguity reproducer inspired by issue #1674, not a
// replay of missing field-level control observations. A delayed EV start and
// a new household load can present the same latest site/EV readings. Until
// the physical flow is confirmed, neither can earn guessed replacement credit.
func TestImportCeilingStartSkewEvidence(t *testing.T) {

	store := telemetry.NewStore()
	at := time.Now().Add(-10 * time.Second)
	emit := func(driver string, kind telemetry.DerType, w float64, when time.Time, phases bool) {
		data := map[string]any{"control_power_observed_at": when.Format(time.RFC3339Nano), "control_power_available": true}
		if phases {
			data["l1_a"], data["l2_a"], data["l3_a"] = 10.01, 11.19, 10.02
		}
		b, _ := json.Marshal(data)
		store.Update(driver, kind, w, nil, b)
		store.RecordDriverSuccess(driver)
	}
	emit("car", telemetry.DerEV, 0, at, false)
	emit("meter", telemetry.DerMeter, 900, at.Add(time.Second), true)
	emit("meter", telemetry.DerMeter, 7100, at.Add(5*time.Second), true)
	st := NewState(0, 0, "meter")
	st.SiteFuseAmps, st.SiteFuseVoltage, st.SiteFusePhases = 25, 230, 3
	st.PeakImportCeilingW = 7500
	now := time.Now()
	st.clock = func() time.Time { return now }
	var log bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)
	capW, known := EVFuseBudget(store, st, []loadpoint.State{{ID: "a", DriverName: "car", PluggedIn: true}}, []loadpoint.Config{{ID: "a", DriverName: "car", PhaseMode: "3p", MinChargeW: 4140}}, 17250)
	if !known || math.Abs(capW-400) > .001 {
		t.Fatalf("unconfirmed EV cannot earn credit: cap=%v known=%v", capW, known)
	}
	var e map[string]any
	if err := json.Unmarshal(log.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e["level"] != "DEBUG" {
		t.Fatalf("budget evidence level=%v want DEBUG", e["level"])
	}
	for key, want := range map[string]float64{"configured_import_ceiling_w": 7500, "physical_fuse_w": 17250, "latest_grid_w": 7100, "latest_ev_credit_w": 0, "latest_site_cap_w": 400, "final_cap_w": 400} {
		if e[key] != want {
			t.Fatalf("%s=%v want %v", key, e[key], want)
		}
	}
	// The 400 W cap is below the field site's 4140 W minimum and thus
	// explains why that possible observation sequence would stop charging.
	if capW >= 4140 {
		t.Fatal("incident minimum-step condition not reproduced")
	}

	// Repeated blocked ticks must not flood the production log ring.
	before := log.Len()
	_, _ = EVFuseBudget(store, st, []loadpoint.State{{ID: "a", DriverName: "car", PluggedIn: true}}, []loadpoint.Config{{ID: "a", DriverName: "car", PhaseMode: "3p", MinChargeW: 4140}}, 17250)
	if log.Len() != before {
		t.Fatal("unchanged blocked verdict was logged again inside one minute")
	}
	// Once actual EV power brackets a site observation, the same site reading
	// earns replacement credit. Diagnostic logging must not alter that recovery.
	emit("car", telemetry.DerEV, 6200, at.Add(6*time.Second), false)
	emit("meter", telemetry.DerMeter, 7100, at.Add(7*time.Second), true)
	emit("car", telemetry.DerEV, 6200, at.Add(8*time.Second), false)
	now = time.Now()
	capW, known = EVFuseBudget(store, st, []loadpoint.State{{ID: "a", DriverName: "car", PluggedIn: true}}, []loadpoint.Config{{ID: "a", DriverName: "car", PhaseMode: "3p", MinChargeW: 4140}}, 17250)
	if !known || math.Abs(capW-6600) > .001 {
		t.Fatalf("confirmed EV recovery changed: cap=%v known=%v", capW, known)
	}
}
