package loadpoint

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

type manualIntentCrash struct{}

// Each SaveConfig is one durable write. A crash happens after that write,
// before its caller can write the next key or acknowledge the action.
type manualIntentCrashStore struct {
	data       map[string]string
	fail       bool
	writes     int
	crashAfter int
}

func (s *manualIntentCrashStore) LoadConfig(key string) (string, bool) {
	v, ok := s.data[key]
	return v, ok
}

func (s *manualIntentCrashStore) SaveConfig(key, value string) error {
	if s.fail {
		return errors.New("disk unavailable")
	}
	s.data[key] = value
	s.writes++
	if s.crashAfter > 0 && s.writes == s.crashAfter {
		panic(manualIntentCrash{})
	}
	return nil
}

func runManualIntentUntilCrash(t *testing.T, action func() error) {
	t.Helper()
	defer func() {
		if _, ok := recover().(manualIntentCrash); !ok {
			t.Fatal("action did not stop at the selected durable write")
		}
	}()
	if err := action(); err != nil {
		t.Fatalf("unexpected storage error: %v", err)
	}
}

func TestManualIntentSurvivesEveryWriteBoundary(t *testing.T) {
	for _, device := range []string{"", "easee:A"} {
		for _, clear := range []bool{false, true} {
			name := "clear_to_pause"
			if clear {
				name = "pause_to_clear"
			}
			t.Run(fmt.Sprintf("%s/device=%s", name, device), func(t *testing.T) {
				prepare := func() (*Manager, *manualIntentCrashStore) {
					store := &manualIntentCrashStore{data: map[string]string{
						"loadpoint_manual_hold:garage": `{"PowerW":11000,"Persistent":true}`,
						"ev_manual_clear:garage":       "pending",
					}}
					m := sessionManager(store, "garage", "charger")
					m.ObserveSession("garage", true, 0, 1000, true, device, "")
					if err := m.PersistManualHold("garage", ManualHold{Persistent: true}, !clear); err != nil {
						t.Fatal(err)
					}
					store.writes = 0
					return m, store
				}
				m, store := prepare()
				if err := m.PersistManualHold("garage", ManualHold{Persistent: true}, clear); err != nil {
					t.Fatal(err)
				}
				boundaries := store.writes
				if boundaries == 0 {
					t.Fatal("explicit action made no durable write")
				}
				for cut := 1; cut <= boundaries; cut++ {
					t.Run(fmt.Sprintf("after_write_%d", cut), func(t *testing.T) {
						m, store := prepare()
						store.crashAfter = cut
						runManualIntentUntilCrash(t, func() error {
							return m.PersistManualHold("garage", ManualHold{Persistent: true}, clear)
						})
						store.crashAfter = 0
						// Reboot twice: restore may finish cleanup, but neither the
						// old clear nor the legacy positive hold may return later.
						for boot := 1; boot <= 2; boot++ {
							restarted := sessionManager(store, "garage", "charger")
							restarted.ObserveSession("garage", true, 0, 1000, true, device, "")
							h, status := restarted.RestoreManualHold("garage")
							if clear {
								if status != "none" || h.Persistent {
									t.Fatalf("boot %d: old hold returned after clear: %+v %s", boot, h, status)
								}
							} else if status != "restored" || !h.Persistent || h.PowerW != 0 {
								t.Fatalf("boot %d: latest pause lost: %+v %s", boot, h, status)
							}
						}
					})
				}
			})
		}
	}
}

func TestManualIntentRetriesWithoutHardwareIdentity(t *testing.T) {
	for _, action := range []string{"pause", "start", "clear"} {
		t.Run(action, func(t *testing.T) {
			store := &manualIntentCrashStore{data: map[string]string{}}
			m := sessionManager(store, "garage", "charger")
			m.ObserveSession("garage", true, 0, 1000, true, "", "")
			c := NewController(m, nil, nil, nil)
			var saveErr error
			c.SetManualHoldSaver(func(id string, h ManualHold, cleared bool) {
				saveErr = m.PersistManualHold(id, h, cleared)
			})
			if action == "clear" {
				c.SetManualHold("garage", ManualHold{Persistent: true})
				if saveErr != nil {
					t.Fatal(saveErr)
				}
			}
			store.fail = true
			if action == "clear" {
				c.ClearManualHold("garage")
			} else {
				watts := 0.0
				if action == "start" {
					watts = 4140
				}
				c.SetManualHold("garage", ManualHold{Persistent: true, PowerW: watts})
			}
			if saveErr == nil {
				t.Fatal("failed intent write was not reported")
			}
			if st, _ := m.State("garage"); !st.ManualSaveError {
				t.Fatal("failed intent write was not visible")
			}
			// Serial-less OCPP samples must be enough to retry the restriction.
			// The operator does not need to repeat the action after disk recovery.
			store.fail = false
			m.ObserveSession("garage", true, 0, 1000, true, "", "")
			if st, _ := m.State("garage"); st.ManualSaveError {
				t.Fatal("successful retry left the save warning set")
			}
			restarted := sessionManager(store, "garage", "charger")
			restarted.ObserveSession("garage", true, 0, 1000, true, "", "")
			restored := NewController(restarted, nil, nil, nil)
			restored.restoreManualHoldForSession("garage")
			h, held := restored.GetManualHold("garage", time.Now())
			if action == "clear" {
				if held {
					t.Fatalf("cleared hold returned after retry and reboot: %+v", h)
				}
			} else if !held || !h.Persistent || h.PowerW != 0 {
				t.Fatalf("retry did not preserve the zero-power restriction: %+v %v", h, held)
			}
			st, _ := restarted.State("garage")
			if st.ManualRestoreUnconfirmed != (action == "start") || st.ManualSaveError {
				t.Fatalf("wrong restore feedback after retry: %+v", st)
			}
		})
	}
}
