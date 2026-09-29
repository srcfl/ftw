package telemetry

import (
	"encoding/json"
	"testing"
	"time"
)

func TestCommandEvidenceTracksPhysicalResponse(t *testing.T) {
	s := NewStore()
	start := time.Now()
	c := s.BeginCommand("pixii", []byte(`{"action":"battery","power_w":0}`), start)
	s.CompleteCommand(c, "accepted")
	got, _ := s.CommandEvidence("pixii", "battery")
	if !got.PowerMatchSince.IsZero() {
		t.Fatal("driver acceptance claimed measured success")
	}
	// Writes succeeded, but another writer or internal control changes the setpoint.
	sample := func(at time.Time, data string, power float64) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.observeCommand("pixii", DerBattery, power, json.RawMessage(data), at)
	}
	sample(start.Add(time.Second), `{"setpoint_w":-1500,"control_power_w":-1400}`, -1300)
	sample(start.Add(35*time.Second), `{"setpoint_w":-1600,"control_power_w":-1500}`, -1400)
	got, _ = s.CommandEvidence("pixii", "battery")
	if got.ReadbackMismatchSince != start.Add(time.Second) || got.PowerMismatchSince != start.Add(time.Second) || !got.PowerMatchSince.IsZero() {
		t.Fatalf("mismatch evidence: %+v", got)
	}
	// Fresh repeated observations, not polling status, establish recovery.
	sample(start.Add(40*time.Second), `{"setpoint_w":0,"control_power_w":30}`, 30)
	sample(start.Add(52*time.Second), `{"setpoint_w":0,"control_power_w":20}`, 20)
	got, _ = s.CommandEvidence("pixii", "battery")
	if !got.ReadbackMismatchSince.IsZero() || !got.PowerMismatchSince.IsZero() || got.PowerMatchSince != start.Add(40*time.Second) {
		t.Fatalf("recovery: %+v", got)
	}
	sample(start.Add(3*time.Minute), `{"setpoint_w":0,"control_power_w":0}`, 0)
	got, _ = s.CommandEvidence("pixii", "battery")
	if got.PowerMatchSince != start.Add(3*time.Minute) {
		t.Fatal("gap retained proof of live response")
	}
}

func TestCommandEvidenceDoesNotConfuseACWithDCOrMissingPower(t *testing.T) {
	s := NewStore()
	now := time.Now()
	c := s.BeginCommand("hybrid", []byte(`{"action":"battery","power_w":-1000}`), now)
	s.CompleteCommand(c, "accepted")
	s.mu.Lock()
	s.observeCommand("hybrid", DerBattery, -300, json.RawMessage(`{"control_power_w":-990}`), now.Add(time.Second))
	s.mu.Unlock()
	got, _ := s.CommandEvidence("hybrid", "battery")
	if got.PowerMatchSince.IsZero() {
		t.Fatal("compared AC request with DC measurement")
	}
	s.mu.Lock()
	s.observeCommand("hybrid", DerBattery, -1000, json.RawMessage(`{"control_power_available":false}`), now.Add(2*time.Second))
	s.mu.Unlock()
	got, _ = s.CommandEvidence("hybrid", "battery")
	if !got.PowerMatchSince.IsZero() || !got.PowerMismatchSince.IsZero() {
		t.Fatal("missing measurement is not proof or failure")
	}
}

func TestCommandEvidenceLifecycleAndIsolation(t *testing.T) {
	s := NewStore()
	now := time.Now()
	c := s.BeginCommand("hybrid", []byte(`{"action":"battery","power_w":1000}`), now)
	*c.PowerW = 9999
	got, _ := s.CommandEvidence("hybrid", "battery")
	if *got.PowerW != 1000 {
		t.Fatal("caller mutated evidence")
	}
	s.CompleteCommand(c, "accepted")
	next := s.BeginCommand("hybrid", []byte(`{"action":"battery","power_w":1020}`), now.Add(time.Second))
	s.CompleteCommand(c, "failed") // late result from an old call
	got, _ = s.CommandEvidence("hybrid", "battery")
	if got.Result != "pending" || got.Since != now {
		t.Fatalf("late result or small correction: %+v", got)
	}
	s.CompleteCommand(next, "accepted")
	pv := s.BeginCommand("hybrid", []byte(`{"action":"curtail","power_w":-3000}`), now.Add(2*time.Second))
	s.CompleteCommand(pv, "accepted")
	got, _ = s.CommandEvidence("hybrid", "battery")
	if *got.PowerW != 1020 {
		t.Fatal("PV command replaced battery evidence")
	}
	s.EndCommandControl("hybrid", false)
	got, _ = s.CommandEvidence("hybrid", "battery")
	if got.Result != "released" {
		t.Fatal("default retained old command")
	}
	s.Remove("hybrid")
	if _, ok := s.CommandEvidence("hybrid", "battery"); ok {
		t.Fatal("restart retained evidence")
	}
}

func TestCommandEvidencePVIsCeilingAndStalePowerIsNotProof(t *testing.T) {
	s := NewStore()
	now := time.Now()
	c := s.BeginCommand("pv", []byte(`{"action":"curtail","power_w":-3000}`), now)
	s.CompleteCommand(c, "accepted")
	s.mu.Lock()
	s.observeCommand("pv", DerPV, -500, json.RawMessage(`{}`), now.Add(time.Second))
	s.mu.Unlock()
	got, _ := s.CommandEvidence("pv", "pv")
	if !got.PowerMismatchSince.IsZero() {
		t.Fatal("weak sun is not failed curtailment")
	}
	s.mu.Lock()
	s.observeCommand("pv", DerPV, -500, json.RawMessage(`{"power_observed_at":"2020-01-01T00:00:00Z"}`), now.Add(2*time.Second))
	s.mu.Unlock()
	got, _ = s.CommandEvidence("pv", "pv")
	if !got.PowerMatchSince.IsZero() {
		t.Fatal("stale source claimed proof")
	}
}

func TestCommandEvidenceUsesSourceTimeForResponse(t *testing.T) {
	s := NewStore()
	start := time.Now()
	c := s.BeginCommand("charger", []byte(`{"action":"ev_set_current","power_w":1000}`), start)
	s.CompleteCommand(c, "accepted")
	sample := func(sourceAt, receivedAt time.Time) CommandEvidence {
		data, err := json.Marshal(map[string]any{"power_observed_at": sourceAt.Format(time.RFC3339Nano)})
		if err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		s.observeCommand("charger", DerEV, 1000, data, receivedAt)
		s.mu.Unlock()
		got, _ := s.CommandEvidence("charger", "ev")
		return got
	}
	got := sample(start.Add(-time.Second), start.Add(time.Second))
	if !got.PowerMatchSince.IsZero() {
		t.Fatal("a pre-command sample claimed a response")
	}
	first := start.Add(2 * time.Second)
	got = sample(first, start.Add(3*time.Second))
	got = sample(first, start.Add(15*time.Second))
	if !got.PowerMatchSince.Equal(first) || !got.LastObservation.Equal(first) {
		t.Fatalf("repeated cached sample advanced proof: %+v", got)
	}
	last := start.Add(16 * time.Second)
	got = sample(last, start.Add(17*time.Second))
	if !got.PowerMatchSince.Equal(first) || !got.LastObservation.Equal(last) {
		t.Fatalf("fresh source samples did not extend proof: %+v", got)
	}
}
