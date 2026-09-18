package loadpoint

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

type blockedSessionStore struct {
	sessionMemory
	block   bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockedSessionStore) SaveConfig(key, value string) error {
	if s.block {
		s.once.Do(func() { close(s.entered) })
		<-s.release
	}
	return s.sessionMemory.SaveConfig(key, value)
}

func TestSafetyStopsAllChargersBeforeBlockedSessionSave(t *testing.T) {
	for _, stale := range []string{"site_meter", "charger_power"} {
		t.Run(stale, func(t *testing.T) {
			now := time.Now()
			cfgs := []Config{
				{ID: "one", DriverName: "one", VehicleCapacityWh: 60000},
				{ID: "two", DriverName: "two", VehicleCapacityWh: 60000},
			}
			samples := map[string]EVSample{}
			for _, cfg := range cfgs {
				samples[cfg.ID] = EVSample{Connected: true, PowerW: 7000, SessionWh: 100, PowerAt: now, EnergyAt: now, DeviceID: cfg.ID, SessionID: "session"}
			}
			c := newTestController(t, cfgs, &Directive{}, samples, &fakeSender{})
			store := &blockedSessionStore{sessionMemory: sessionMemory{data: map[string]string{}}, entered: make(chan struct{}), release: make(chan struct{})}
			c.manager.SetSessionStore(store)
			for _, cfg := range cfgs {
				c.manager.ObserveSample(cfg.ID, samples[cfg.ID])
				c.manager.SetCurrentSoC(cfg.ID, 0.5)
				// A previous failed checkpoint must retry, but it must not
				// delay the site's safety standdown for either charger.
				c.manager.byID[cfg.ID].socRetention = "error"
				if stale == "charger_power" {
					sample := samples[cfg.ID]
					sample.PowerUnavailable = true
					samples[cfg.ID] = sample
				}
			}
			commands := make(chan sentCommand, 8)
			c.send = func(_ context.Context, driver string, body []byte) error {
				var command struct {
					Action string  `json:"action"`
					Power  float64 `json:"power_w"`
				}
				if err := json.Unmarshal(body, &command); err != nil {
					return err
				}
				commands <- sentCommand{driver: driver, action: command.Action, power: command.Power}
				return nil
			}
			store.block = true
			done := make(chan struct{})
			go func() {
				defer close(done)
				c.TickWithDispatch(context.Background(), now, stale != "site_meter")
			}()
			defer func() {
				close(store.release)
				<-done
			}()
			select {
			case <-store.entered:
			case <-time.After(time.Second):
				t.Fatal("checkpoint was not exercised")
			}
			stopped := map[string]bool{}
			for len(commands) > 0 {
				command := <-commands
				if command.action != "ev_set_current" || command.power != 0 {
					t.Fatalf("unexpected safety command: %+v", command)
				}
				stopped[command.driver] = true
			}
			if !stopped["one"] || !stopped["two"] {
				t.Fatalf("storage blocked safety stop: stopped=%v", stopped)
			}
		})
	}
}
