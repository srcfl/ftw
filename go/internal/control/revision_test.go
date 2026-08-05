package control

import "testing"

func TestRevisionStartsAtOneAndHoldsStill(t *testing.T) {
	var rev Revision
	s := &State{Mode: ModeSelfConsumption, GridTargetW: 0}

	first := rev.Observe(s)
	if first != 1 {
		t.Fatalf("first observation = %d, want 1", first)
	}
	if again := rev.Observe(s); again != first {
		t.Fatalf("an unchanged state moved the revision to %d", again)
	}
	if rev.Current() != first {
		t.Fatalf("Current = %d, want %d", rev.Current(), first)
	}
}

// The whole point. Without this the app's expect.rev check passes everything,
// which reads like a conflict check and is not one.
func TestEveryControllableFieldMovesTheRevision(t *testing.T) {
	base := func() *State {
		return &State{
			Mode:               ModeSelfConsumption,
			GridTargetW:        0,
			GridToleranceW:     50,
			PeakLimitW:         5000,
			PeakImportCeilingW: 0,
			MaxExportW:         0,
			ManualEVChargingW:  0,
			BatteryCoversEV:    false,
			PriorityOrder:      []string{"a", "b"},
			Weights:            map[string]float64{"a": 1},
		}
	}

	for name, mutate := range map[string]func(*State){
		"mode":         func(s *State) { s.Mode = ModePeakShaving },
		"grid target":  func(s *State) { s.GridTargetW = 100 },
		"tolerance":    func(s *State) { s.GridToleranceW = 75 },
		"peak limit":   func(s *State) { s.PeakLimitW = 4000 },
		"peak ceiling": func(s *State) { s.PeakImportCeilingW = 7000 },
		"max export":   func(s *State) { s.MaxExportW = 8000 },
		"manual ev":    func(s *State) { s.ManualEVChargingW = 3000 },
		"covers ev":    func(s *State) { s.BatteryCoversEV = true },
		"priority":     func(s *State) { s.PriorityOrder = []string{"b", "a"} },
		"weights":      func(s *State) { s.Weights = map[string]float64{"a": 2} },
	} {
		t.Run(name, func(t *testing.T) {
			var rev Revision
			before := rev.Observe(base())

			changed := base()
			mutate(changed)
			after := rev.Observe(changed)

			if after != before+1 {
				t.Fatalf("revision went %d → %d; changing %s did not bump it", before, after, name)
			}
		})
	}
}

// Per-tick working state must not move it. A revision that changed every
// second would make every command a conflict.
func TestWorkingStateLeavesTheRevisionAlone(t *testing.T) {
	var rev Revision
	s := &State{Mode: ModeSelfConsumption, Weights: map[string]float64{"a": 1}}
	before := rev.Observe(s)

	s.EVChargingW = 4200
	s.PlanStale = true
	s.LastTargets = []DispatchTarget{{Driver: "battery", TargetW: -1000}}
	s.PrevTargets = map[string]float64{"battery": -1000}

	if after := rev.Observe(s); after != before {
		t.Fatalf("revision went %d → %d on per-tick state alone", before, after)
	}
}

// Map iteration order is not a change. Without the sort this test fails
// intermittently, which is the worst way to find out.
func TestWeightOrderIsNotAChange(t *testing.T) {
	var rev Revision
	weights := map[string]float64{"a": 1, "b": 2, "c": 3, "d": 4, "e": 5}

	before := rev.Observe(&State{Mode: ModeSelfConsumption, Weights: weights})
	for i := 0; i < 50; i++ {
		copied := make(map[string]float64, len(weights))
		for k, v := range weights {
			copied[k] = v
		}
		if after := rev.Observe(&State{Mode: ModeSelfConsumption, Weights: copied}); after != before {
			t.Fatalf("revision moved to %d on iteration order alone", after)
		}
	}
}

// Two adjacent strings must not be able to run together into a third pair's
// bytes, or a rename plus an edit could cancel out.
func TestFieldBoundariesAreHashed(t *testing.T) {
	var rev Revision
	before := rev.Observe(&State{Mode: ModeSelfConsumption, PriorityOrder: []string{"ab", "c"}})
	after := rev.Observe(&State{Mode: ModeSelfConsumption, PriorityOrder: []string{"a", "bc"}})

	if after == before {
		t.Fatal("two different priority orders fingerprinted the same")
	}
}
