package state

import "testing"

func TestComponentUpdateUpsertSurvivesProcessHandoff(t *testing.T) {
	s := freshStore(t)
	started, err := s.UpsertComponentUpdate(ComponentUpdate{
		OperationKey: "core:1000:update:v1.4.0", Kind: "core", ComponentID: "core",
		Action: "update", FromVersion: "v1.3.1", ToVersion: "v1.4.0",
		Outcome: "in_progress", StartedAtMS: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	finished, err := s.UpsertComponentUpdate(ComponentUpdate{
		OperationKey: started.OperationKey, Kind: "core", ComponentID: "core",
		Action: "update", ToVersion: "v1.4.0", Outcome: "succeeded",
		Message: "healthy", StartedAtMS: 1000, FinishedAtMS: 2000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if finished.ID != started.ID || finished.FromVersion != "v1.3.1" || finished.Outcome != "succeeded" {
		t.Fatalf("finished event = %+v", finished)
	}
	events, err := s.ComponentUpdates("core", "core", 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %+v err=%v", events, err)
	}
}
