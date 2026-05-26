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

// pushVehicle is a tiny helper that publishes a DerVehicle reading
// with the given fields. Mirrors what tesla_vehicle.lua does on poll
// — DerVehicle SoC is stored in percent (0-100), not 0-1, because
// vehicles report battery_level as percent and the picker bounds
// against [0,100].
func pushVehicle(t *testing.T, s *Store, driver string, soc, limit float64,
	state string, stale bool, age time.Duration) {
	t.Helper()
	socPct := soc
	data, _ := json.Marshal(map[string]any{
		"charging_state":   state,
		"charge_limit_pct": limit,
		"stale":            stale,
	})
	s.Update(driver, DerVehicle, 0, &socPct, data)
	// Mark health online + age the reading by reaching into the
	// store's UpdatedAt directly via a follow-up Update with a
	// known timestamp would be invasive; instead the test passes
	// `age` by comparing relative to (now - age) inside the helper.
	if age > 0 {
		s.mu.Lock()
		if r := s.readings[key(driver, DerVehicle)]; r != nil {
			r.UpdatedAt = time.Now().Add(-age)
		}
		s.mu.Unlock()
	}
	s.DriverHealthMut(driver).RecordSuccess()
}

func TestPickBestVehicleHonoursRank(t *testing.T) {
	s := NewStore()
	pushVehicle(t, s, "garage", 50, 80, "Stopped", false, 0)
	pushVehicle(t, s, "driveway", 30, 80, "Charging", false, 0)
	pick := PickBestVehicle(s, time.Now())
	if pick.Driver != "driveway" {
		t.Errorf("expected Charging vehicle to win, got %q", pick.Driver)
	}
}

func TestPickBestVehicleSkipsDisconnected(t *testing.T) {
	s := NewStore()
	pushVehicle(t, s, "garage", 50, 80, "Disconnected", false, 0)
	pick := PickBestVehicle(s, time.Now())
	if pick.Driver != "" {
		t.Errorf("Disconnected reading must not be picked, got %q", pick.Driver)
	}
}

func TestPickBestVehicleBoundsSoC(t *testing.T) {
	s := NewStore()
	// Lying proxy reports out-of-range SoC + limit.
	pushVehicle(t, s, "rogue", 250, 200, "Charging", false, 0)
	pick := PickBestVehicle(s, time.Now())
	if pick.SoCPct != 100 {
		t.Errorf("SoC = %v, want clamped to 100", pick.SoCPct)
	}
	if pick.ChargeLimitPct != 100 {
		t.Errorf("ChargeLimit = %v, want clamped to 100", pick.ChargeLimitPct)
	}

	s2 := NewStore()
	pushVehicle(t, s2, "rogue2", -50, -10, "Charging", false, 0)
	pick2 := PickBestVehicle(s2, time.Now())
	if pick2.SoCPct != 0 {
		t.Errorf("negative SoC = %v, want clamped to 0", pick2.SoCPct)
	}
	if pick2.ChargeLimitPct != 0 {
		t.Errorf("negative limit = %v, want clamped to 0", pick2.ChargeLimitPct)
	}
}

func TestPickBestVehicleSkipsStaleByWallclock(t *testing.T) {
	s := NewStore()
	// Reading is 10 min old, well past VehicleMaxAge. Even though
	// the proxy didn't set the `stale` flag, freshness is decided
	// by wallclock — a proxy that stops talking can't keep the
	// last-known SoC live forever.
	pushVehicle(t, s, "asleep", 50, 80, "Charging", false, 10*time.Minute)
	pick := PickBestVehicle(s, time.Now())
	if pick.Driver != "" {
		t.Errorf("stale-by-wallclock reading must not be picked, got %q", pick.Driver)
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
	pushVehicle(t, s, "a", 40, 80, "Charging", false, 60*time.Second)
	pushVehicle(t, s, "b", 60, 80, "Charging", false, 5*time.Second)
	pick := PickBestVehicle(s, time.Now())
	if pick.Driver != "b" {
		t.Errorf("fresher reading should win tiebreak, got %q (soc=%v)", pick.Driver, pick.SoCPct)
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
	pushVehicle(t, s, "tesla-charging", 50, 80, "Charging", false, 0)
	// Tesla #2: parked elsewhere, fresher reading but not charging.
	pushVehicle(t, s, "tesla-parked", 60, 80, "Stopped", false, 0)

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
	pushVehicle(t, s, "tesla-parked", 60, 80, "Stopped", false, 0)
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
	pushVehicle(t, s, "a", 40, 80, "Charging", false, 60*time.Second)
	pushVehicle(t, s, "b", 60, 80, "Charging", false, 5*time.Second)
	pick := PickBestVehicleForLoadpoint(s, true, time.Now())
	if pick.Driver != "b" {
		t.Errorf("strict gate: fresher reading should win tiebreak, got %q", pick.Driver)
	}
}
