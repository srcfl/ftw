package loadpoint

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestRefreshVehicleIndependentOfFreshSoCAndChargeStart(t *testing.T) {
	commands := 0
	c := NewController(NewManager(), nil, nil, SenderFunc(func(_ context.Context, driver string, payload []byte) error {
		var action struct {
			Action string `json:"action"`
		}
		if err := json.Unmarshal(payload, &action); err != nil {
			t.Fatal(err)
		}
		if driver != "tesla" || action.Action != "wake_up" {
			t.Fatalf("driver=%q payload=%s", driver, payload)
		}
		commands++
		return nil
	}))
	c.SetVehicleStatus(func(string) (string, string, bool) { return "", "", false })
	c.SetVehicleRefreshTarget(func(string) (string, error) { return "tesla", nil })
	previous := time.Now().Add(-time.Hour)
	c.wakeLast = map[string]time.Time{"garage": previous}
	c.wakeAttempts = map[string]int{"garage": 5}
	if err := c.RefreshVehicle(context.Background(), "garage"); err != nil {
		t.Fatal(err)
	}
	if commands != 1 || c.wakeAttempts["garage"] != 5 || !c.wakeLast["garage"].Equal(previous) {
		t.Fatal("refresh changed charge-start state")
	}
	c.SetVehicleRefreshTarget(func(string) (string, error) { return "", errors.New("ambiguous vehicle") })
	if err := c.RefreshVehicle(context.Background(), "garage"); err == nil || commands != 1 {
		t.Fatal("ambiguous vehicle refreshed")
	}
}
