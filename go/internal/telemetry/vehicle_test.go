package telemetry

import (
	"encoding/json"
	"testing"
	"time"
)

// PickBestVehicle is the trust boundary between BLE-proxy telemetry
// (potentially attacker-controlled) and the loadpoint controller / MPC.
// These tests lock in the bounds, freshness check, and rank ordering.

func TestVehicleConnectedRankOrdering(t *testing.T) {
	if VehicleConnectedRank("Charging") <= VehicleConnectedRank("NoPower") {
		t.Error("Charging must outrank NoPower")
	}
	if VehicleConnectedRank("NoPower") <= VehicleConnectedRank("Stopped") {
		t.Error("NoPower must outrank Stopped")
	}
	if VehicleConnectedRank("Stopped") <= VehicleConnectedRank("unknown") {
		t.Error("Stopped must outrank unknown")
	}
	if VehicleConnectedRank("Disconnected") >= 0 {
		t.Error("Disconnected must score negative — never picked")
	}
	if VehicleConnectedRank("Starting") != VehicleConnectedRank("Charging") {
		t.Error("Starting and Charging share top rank — vehicle is engaged")
	}
}

func TestVehicleCompletionRequiresUnambiguousConnectedSource(t *testing.T) {
	s := NewStore()
	pushVehicle(t, s, "one", .8, .8, "Complete", false, 0)
	if got := PickVehicleForCompletion(s, time.Now()); got.Driver != "one" {
		t.Fatal(got)
	}
	pushVehicle(t, s, "two", .5, .8, "Charging", false, 0)
	if got := PickVehicleForCompletion(s, time.Now()); got.Driver != "" {
		t.Fatal("rank is not a car-to-charger binding", got)
	}
	for _, state := range []string{"", "Disconnected"} {
		s = NewStore()
		pushVehicle(t, s, "one", .8, .8, state, false, 0)
		if got := PickVehicleForCompletion(s, time.Now()); got.Driver != "" {
			t.Fatal("missing connection proof", got)
		}
	}
}

// pushVehicle publishes a DerVehicle reading. soc and limit are 0–1
// fractions (core SI). charge_limit_pct in the driver blob is the
// legacy vendor door and is converted at PickBestVehicle.
func pushVehicle(t *testing.T, s *Store, driver string, soc, limit float64,
	state string, stale bool, age time.Duration) {
	t.Helper()
	stored := soc
	data, _ := json.Marshal(map[string]any{
		"charging_state": state,
		"charge_limit":   limit,
		"stale":          stale,
	})
	s.Update(driver, DerVehicle, 0, &stored, data)
	// Mark health online + age the reading and SoC observation by
	// reaching into the store directly; a follow-up Update with a
	// known timestamp would be invasive; instead the test passes
	// `age` by comparing relative to (now - age) inside the helper.
	if age > 0 {
		s.mu.Lock()
		if r := s.readings[key(driver, DerVehicle)]; r != nil {
			agedAt := time.Now().Add(-age)
			r.UpdatedAt = agedAt
			r.SoCUpdatedAt = agedAt
		}
		s.mu.Unlock()
	}
	s.DriverHealthMut(driver).RecordSuccess()
}

func TestPickBestVehicleHonoursRank(t *testing.T) {
	s := NewStore()
	pushVehicle(t, s, "garage", 0.50, 0.80, "Stopped", false, 0)
	pushVehicle(t, s, "driveway", 0.30, 0.80, "Charging", false, 0)
	pick := PickBestVehicle(s, time.Now())
	if pick.Driver != "driveway" {
		t.Errorf("expected Charging vehicle to win, got %q", pick.Driver)
	}
}

func TestPickBestVehicleSkipsDisconnected(t *testing.T) {
	s := NewStore()
	pushVehicle(t, s, "garage", 0.50, 0.80, "Disconnected", false, 0)
	pick := PickBestVehicle(s, time.Now())
	if pick.Driver != "" {
		t.Errorf("Disconnected reading must not be picked, got %q", pick.Driver)
	}
}

func TestPickBestVehicleDropsInvalidSoC(t *testing.T) {
	s := NewStore()
	// Store.Update validates 0–1 before the row exists, so a lying
	// proxy cannot park 2.5 in core for the picker to paper over.
	pushVehicle(t, s, "rogue", 2.50, 0.80, "Charging", false, 0)
	if pick := PickBestVehicle(s, time.Now()); pick.Driver != "" {
		t.Errorf("invalid SoC must not be stored, got %+v", pick)
	}

	s2 := NewStore()
	pushVehicle(t, s2, "rogue2", -0.50, 0.80, "Charging", false, 0)
	if pick := PickBestVehicle(s2, time.Now()); pick.Driver != "" {
		t.Errorf("negative SoC must not be stored, got %+v", pick)
	}
}

func TestPickBestVehicleClampsLegacyChargeLimit(t *testing.T) {
	s := NewStore()
	soc := 0.50
	data, _ := json.Marshal(map[string]any{
		"charging_state":   "Charging",
		"charge_limit_pct": 200,
		"stale":            false,
	})
	s.Update("rogue", DerVehicle, 0, &soc, data)
	s.DriverHealthMut("rogue").RecordSuccess()
	pick := PickBestVehicle(s, time.Now())
	if pick.Driver != "rogue" {
		t.Fatalf("expected pick, got %+v", pick)
	}
	if pick.ChargeLimit != 1 {
		t.Errorf("legacy 200%% charge limit = %v, want clamped to 1", pick.ChargeLimit)
	}
}

func TestPickBestVehicleSkipsStaleByWallclock(t *testing.T) {
	s := NewStore()
	// Reading is 10 min old, well past VehicleMaxAge. Even though
	// the proxy didn't set the `stale` flag, freshness is decided
	// by wallclock — a proxy that stops talking can't keep the
	// last-known SoC live forever.
	pushVehicle(t, s, "asleep", 0.50, 0.80, "Charging", false, 10*time.Minute)
	pick := PickBestVehicle(s, time.Now())
	if pick.Driver != "" {
		t.Errorf("stale-by-wallclock reading must not be picked, got %q", pick.Driver)
	}
}

func TestPickBestVehicleDoesNotFreshenCarriedSoC(t *testing.T) {
	s := NewStore()
	pushVehicle(t, s, "asleep", 0.50, 0.80, "Charging", false, 0)
	staleAt := time.Now().Add(-VehicleMaxAge - time.Minute)
	s.mu.Lock()
	s.readings[key("asleep", DerVehicle)].SoCUpdatedAt = staleAt
	s.mu.Unlock()

	data, err := json.Marshal(map[string]any{
		"charging_state": "Charging",
		"charge_limit":   0.80,
		"soc_fresh":      false,
	})
	if err != nil {
		t.Fatal(err)
	}
	cachedSoC := 0.50
	s.Update("asleep", DerVehicle, 0, &cachedSoC, data)
	r := s.Get("asleep", DerVehicle)
	if r == nil || !r.SoCUpdatedAt.Equal(staleAt) {
		t.Fatalf("power-only update changed SoC source time: %+v", r)
	}
	if !r.UpdatedAt.After(staleAt) {
		t.Fatalf("reading timestamp did not advance: %+v", r)
	}
	if pick := PickBestVehicle(s, time.Now()); pick.Driver != "" {
		t.Fatalf("carried stale SoC must not stay selectable, got %+v", pick)
	}
}

func TestPickBestVehicleSkipsDriverMarkedStale(t *testing.T) {
	s := NewStore()
	pushVehicle(t, s, "driver-stale", 0.50, 0.80, "Charging", true, 0)
	if pick := PickBestVehicle(s, time.Now()); pick.Driver != "" {
		t.Fatalf("driver-marked stale SoC must not be selectable, got %+v", pick)
	}
}

func TestPickBestVehicleNilStoreSafe(t *testing.T) {
	pick := PickBestVehicle(nil, time.Now())
	if pick.Driver != "" {
		t.Errorf("nil store must return zero-value pick, got %+v", pick)
	}
}

func TestPickBestVehicleTiebreakByFreshness(t *testing.T) {
	s := NewStore()
	pushVehicle(t, s, "a", 0.40, 0.80, "Charging", false, 60*time.Second)
	pushVehicle(t, s, "b", 0.60, 0.80, "Charging", false, 5*time.Second)
	pick := PickBestVehicle(s, time.Now())
	if pick.Driver != "b" {
		t.Errorf("fresher reading should win tiebreak, got %q (soc=%v)", pick.Driver, pick.SoC)
	}
}

// Connection-evidence gate: when the loadpoint is actively delivering
// power, only vehicles in Charging/Starting are accepted as the
// connected one. A second car at home reporting Stopped (parked,
// charge-limit reached on a previous session, etc.) must NOT win the
// pick — that's the two-Tesla flap behaviour the gate exists to
// prevent.
func TestPickBestVehicleForLoadpointGatesByDeliveringPower(t *testing.T) {
	s := NewStore()
	// Tesla #1: actively charging on this loadpoint.
	pushVehicle(t, s, "tesla-charging", 0.50, 0.80, "Charging", false, 0)
	// Tesla #2: parked elsewhere, fresher reading but not charging.
	pushVehicle(t, s, "tesla-parked", 0.60, 0.80, "Stopped", false, 0)

	// Loadpoint NOT delivering power → existing behaviour: rank-based
	// pick still wins, charging > stopped.
	pick := PickBestVehicleForLoadpoint(s, false, time.Now())
	if pick.Driver != "tesla-charging" {
		t.Errorf("idle loadpoint: expected tesla-charging, got %q", pick.Driver)
	}

	// Loadpoint delivering power → strict gate, only Charging/Starting
	// accepted. Same outcome here, but the test below demonstrates
	// the gate excludes a parked-but-fresher vehicle.
	pick = PickBestVehicleForLoadpoint(s, true, time.Now())
	if pick.Driver != "tesla-charging" {
		t.Errorf("delivering loadpoint: expected tesla-charging, got %q", pick.Driver)
	}
}

func TestPickBestVehicleForLoadpointStrictExcludesStopped(t *testing.T) {
	s := NewStore()
	// Only a Stopped vehicle is reporting (e.g. parked Tesla in
	// driveway), with no Charging-state counterpart. When the
	// loadpoint is delivering power, the gate must reject — there's
	// no evidence this Stopped car is the one connected.
	pushVehicle(t, s, "tesla-parked", 0.60, 0.80, "Stopped", false, 0)
	pick := PickBestVehicleForLoadpoint(s, true, time.Now())
	if pick.Driver != "" {
		t.Errorf("delivering loadpoint with only Stopped vehicle: must return zero pick, got %q", pick.Driver)
	}
	// Without the gate (idle loadpoint), the Stopped car is a valid
	// pick — ranks above Disconnected, can be the connected one.
	pick = PickBestVehicleForLoadpoint(s, false, time.Now())
	if pick.Driver != "tesla-parked" {
		t.Errorf("idle loadpoint: expected tesla-parked, got %q", pick.Driver)
	}
}

// Regression: two Charging readings at different freshness must still
// pick the freshest under the strict gate (rank parity falls back to
// freshness, exactly as before).
func TestPickBestVehicleForLoadpointStrictTiebreakByFreshness(t *testing.T) {
	s := NewStore()
	pushVehicle(t, s, "a", 0.40, 0.80, "Charging", false, 60*time.Second)
	pushVehicle(t, s, "b", 0.60, 0.80, "Charging", false, 5*time.Second)
	pick := PickBestVehicleForLoadpoint(s, true, time.Now())
	if pick.Driver != "b" {
		t.Errorf("strict gate: fresher reading should win tiebreak, got %q", pick.Driver)
	}
}

func TestPickBestVehicleForDisplayRetainsOldObservation(t *testing.T) {
	s := NewStore()
	pushVehicle(t, s, "asleep", 0.50, 0.80, "Stopped", false, 12*time.Minute)

	if pick := PickBestVehicleForLoadpoint(s, false, time.Now()); pick.Driver != "" {
		t.Fatalf("old SoC must stay unavailable to control, got %+v", pick)
	}

	pick := PickBestVehicleForDisplay(s, false, time.Now())
	if pick.Driver != "asleep" {
		t.Fatalf("display picker lost last-known car report: %+v", pick)
	}
	if !pick.Stale {
		t.Fatalf("12-minute-old report must be marked stale: %+v", pick)
	}
	if pick.SoC != 0.50 {
		t.Fatalf("display SoC=%v, want 0.50", pick.SoC)
	}
}

func TestPickBestVehicleForDisplayRetainsDriverMarkedStale(t *testing.T) {
	s := NewStore()
	pushVehicle(t, s, "invalid", 0.50, 0.80, "Stopped", true, 12*time.Minute)

	if pick := PickBestVehicleForDisplay(s, false, time.Now()); pick.Driver != "invalid" || !pick.Stale {
		t.Fatalf("stale display: %+v", pick)
	}
	if pick := PickBestVehicleForLoadpoint(s, false, time.Now()); pick.Driver != "" {
		t.Fatalf("stale control: %+v", pick)
	}
}

func TestCachedVehicleDisplayAfterRestartAndOlderPredecessor(t *testing.T) {
	for _, predecessor := range []bool{false, true} {
		s := NewStore()
		if predecessor {
			pushVehicle(t, s, "audi-vag", .48, .8, "Stopped", false, 48*time.Hour)
		}
		soc := .98
		s.Update("audi-vag", DerVehicle, 0, &soc, json.RawMessage(`{"soc":98,"soc_fresh":false,"stale":true,"charging_state":"Stopped"}`))
		s.EmitMetric("audi-vag", "vehicle_soc_age_s", 156022, "s", "", "")
		s.DriverHealthMut("audi-vag").RecordSuccess()
		_, receivedAt, _ := s.LatestMetric("audi-vag", "vehicle_soc_age_s")
		now := receivedAt.Add(2 * time.Minute)
		got := PickBestVehicleForDisplay(s, false, now)
		if got.Driver != "audi-vag" || got.SoC != .98 || !got.Stale || int64(now.Sub(got.UpdatedAt)/time.Second) != 156142 {
			t.Fatalf("predecessor=%v: cached display = %+v", predecessor, got)
		}
		if got := PickBestVehicleForLoadpoint(s, false, now); got.Driver != "" {
			t.Fatalf("cached report reached control: %+v", got)
		}
		if got := PickVehicleForCompletion(s, now); got.Driver != "" {
			t.Fatalf("cached report completed goal: %+v", got)
		}
		if got := PickBestVehicleForDisplay(s, true, now); got.Driver != "" {
			t.Fatalf("Stopped car matched charging loadpoint: %+v", got)
		}
	}
}
func TestCachedVehicleDisplayRequiresValidSourceAgeAndSoC(t *testing.T) {
	for _, report := range []string{`{"soc":98,"soc_fresh":false,"stale":true}`, `{"soc":101,"soc_fresh":false,"stale":true}`, `{"soc":98,"soc_fresh":"false","stale":true}`} {
		s := NewStore()
		soc := .98
		s.Update("audi", DerVehicle, 0, &soc, json.RawMessage(report))
		s.DriverHealthMut("audi").RecordSuccess()
		if got := PickBestVehicleForDisplay(s, false, time.Now()); got.Driver != "" {
			t.Fatalf("invalid report/unknown age: %+v", got)
		}
		if report != `{"soc":98,"soc_fresh":false,"stale":true}` {
			s.EmitMetric("audi", "vehicle_soc_age_s", 156022, "s", "", "")
			if got := PickBestVehicleForDisplay(s, false, time.Now()); got.Driver != "" {
				t.Fatalf("invalid raw report: %+v", got)
			}
		}
	}
}

func TestPickVehicleForAnchorAcceptsCloudCadence(t *testing.T) {
	s := NewStore()
	pushVehicle(t, s, "cloud", 0.50, 0.80, "Stopped", false, 30*time.Minute)

	if pick := PickBestVehicleForLoadpoint(s, false, time.Now()); pick.Driver != "" {
		t.Fatalf("30-minute-old SoC is not live: %+v", pick)
	}
	pick := PickVehicleForAnchor(s, false, time.Now())
	if pick.Driver != "cloud" || pick.SoC != 0.50 || !pick.Stale {
		t.Fatalf("anchor pick: %+v", pick)
	}
	if age := time.Since(pick.UpdatedAt); age < 29*time.Minute {
		t.Fatalf("anchor pick must keep the observation time, age %v", age)
	}
}

func TestPickVehicleForAnchorRejectsTooOldAndDriverMarkedStale(t *testing.T) {
	s := NewStore()
	pushVehicle(t, s, "old", 0.50, 0.80, "Stopped", false, VehicleAnchorMaxAge+time.Minute)
	if pick := PickVehicleForAnchor(s, false, time.Now()); pick.Driver != "" {
		t.Fatalf("reading past VehicleAnchorMaxAge: %+v", pick)
	}
	s = NewStore()
	pushVehicle(t, s, "marked", 0.50, 0.80, "Stopped", true, time.Minute)
	if pick := PickVehicleForAnchor(s, false, time.Now()); pick.Driver != "" {
		t.Fatalf("driver-marked stale reading: %+v", pick)
	}
}

func TestCachedCloudSoCCanAnchorWithoutBecomingLive(t *testing.T) {
	s := NewStore()
	soc := .78
	s.Update("vag", DerVehicle, 0, &soc, json.RawMessage(`{"soc":78,"soc_fresh":false,"stale":true,"charging_state":"Stopped"}`))
	s.EmitMetric("vag", "vehicle_soc_age_s", 34*60, "s", "", "")
	s.DriverHealthMut("vag").RecordSuccess()
	_, receivedAt, _ := s.LatestMetric("vag", "vehicle_soc_age_s")
	now := receivedAt.Add(time.Second)
	pick := PickVehicleForAnchor(s, false, now)
	if pick.Driver != "vag" || pick.SoC != .78 || !pick.Stale ||
		!pick.UpdatedAt.Equal(receivedAt.Add(-34*time.Minute)) {
		t.Fatalf("cached cloud anchor: %+v", pick)
	}
	if got := PickBestVehicleForLoadpoint(s, false, now); got.Driver != "" {
		t.Fatalf("historical SoC became live: %+v", got)
	}
	if got := PickVehicleForCompletion(s, now); got.Driver != "" {
		t.Fatalf("historical SoC confirmed completion: %+v", got)
	}
	if got := PickVehicleForAnchor(s, true, now); got.Driver != "" {
		t.Fatalf("stopped car matched a different actively charging car: %+v", got)
	}
	if got := PickVehicleForAnchor(s, false, receivedAt.Add(27*time.Minute)); got.Driver != "" {
		t.Fatalf("cached anchor was rejuvenated beyond one hour: %+v", got)
	}
}

func TestCachedCloudAnchorRequiresKnownAgeAndOnlineConnectedCar(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		age         float64
		online      bool
	}{
		{"unknown-age", "Stopped", -1, true},
		{"too-old", "Stopped", 61 * 60, true},
		{"disconnected", "Disconnected", 34 * 60, true},
		{"offline", "Stopped", 34 * 60, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStore()
			soc := .78
			data, _ := json.Marshal(map[string]any{"soc": 78, "soc_fresh": false, "stale": true, "charging_state": tc.state})
			s.Update("vag", DerVehicle, 0, &soc, data)
			if tc.age >= 0 {
				s.EmitMetric("vag", "vehicle_soc_age_s", tc.age, "s", "", "")
			}
			if tc.online {
				s.DriverHealthMut("vag").RecordSuccess()
			}
			if got := PickVehicleForAnchor(s, false, time.Now()); got.Driver != "" {
				t.Fatalf("invalid cached anchor: %+v", got)
			}
		})
	}
}

func TestCachedCloudAnchorDoesNotReplaceNewerCarObservation(t *testing.T) {
	s := NewStore()
	pushVehicle(t, s, "vag", .80, .80, "Stopped", false, time.Minute)
	latest := s.Get("vag", DerVehicle).SoCUpdatedAt
	soc := .78
	s.Update("vag", DerVehicle, 0, &soc, json.RawMessage(`{"soc":78,"soc_fresh":false,"stale":false,"charging_state":"Stopped"}`))
	s.EmitMetric("vag", "vehicle_soc_age_s", 34*60, "s", "", "")
	got := PickVehicleForAnchor(s, false, time.Now())
	if got.Driver != "vag" || got.SoC != .80 || !got.UpdatedAt.Equal(latest) {
		t.Fatalf("older cached report replaced newer car measurement: %+v", got)
	}
}
