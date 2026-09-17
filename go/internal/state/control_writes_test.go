package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
)

func finishControlWrites(t *testing.T, w *ControlWrites) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Error(err)
	}
}

func TestControlWritesCoalescesWithoutClaimingDurability(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	disk := map[string]string{"session": "old"}
	w := newControlWrites(disk, func(values map[string]string) error {
		once.Do(func() { close(entered); <-release })
		maps.Copy(disk, values)
		return nil
	})
	defer finishControlWrites(t, w)
	if err := w.SaveConfig("session", "first"); !errors.Is(err, ErrWritePending) {
		t.Fatal(err)
	}
	<-entered
	for i := 0; i < 1000; i++ {
		if err := w.SaveConfig("session", fmt.Sprint(i)); !errors.Is(err, ErrWritePending) {
			t.Fatal(err)
		}
	}
	if _, ok := w.LoadConfig("session"); ok {
		t.Fatal("pending replacement exposed obsolete restart state")
	}
	w.mu.Lock()
	jobs := len(w.jobs)
	w.mu.Unlock()
	if jobs != 1 {
		t.Fatal("repeated observations grew the queue", jobs)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := w.Flush(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("acknowledged blocked write", err)
	}
	close(release)
	if err := w.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, ok := w.LoadConfig("session"); !ok || got != "999" || disk["session"] != "999" {
		t.Fatal(got, ok, disk)
	}
	if err := w.SaveConfig("session", "999"); err != nil {
		t.Fatal("committed retry should acknowledge", err)
	}
}

func TestControlWritesAtomicHoldAndSQLiteRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.NewEVControlWrites()
	if err != nil {
		t.Fatal(err)
	}
	m := loadpoint.NewManager()
	m.Load([]loadpoint.Config{{ID: "garage", DriverName: "charger", VehicleCapacityWh: 75000}})
	m.SetSessionStore(w)
	m.ObserveSession("garage", true, 3600, 100, true, "easee:one", "session-one")
	m.SetCurrentSoC("garage", .78)
	_ = m.PersistManualHold("garage", loadpoint.ManualHold{Persistent: true, PowerW: 7000}, false)
	// A newer Stop must win over a queued/in-flight Start.
	_ = m.PersistManualHold("garage", loadpoint.ManualHold{Persistent: true}, false)
	if err := m.WaitForPersistence(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st, _ := m.State("garage"); st.ManualSavePending || st.ManualSaveError || st.SoCRetention != "session" {
		t.Fatal("commit status waited for a later control tick", st)
	}
	finishControlWrites(t, w)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w, err = s.NewEVControlWrites()
	if err != nil {
		t.Fatal(err)
	}
	defer finishControlWrites(t, w)
	m = loadpoint.NewManager()
	m.Load([]loadpoint.Config{{ID: "garage", DriverName: "charger", VehicleCapacityWh: 75000}})
	m.SetSessionStore(w)
	m.ObserveSession("garage", true, 0, 100, true, "easee:one", "session-one")
	if st, _ := m.State("garage"); st.CurrentSoC != .78 || st.SoCRetention != "session" {
		t.Fatal("lost confirmed level", st)
	}
	if hold, status := m.RestoreManualHold("garage"); status != "restored" || hold.PowerW != 0 || !hold.Persistent {
		t.Fatal("older queued start replaced stop", hold, status)
	}
}

func TestControlWritesFailureRemainsVisibleAndRetries(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	errDisk := errors.New("disk unavailable")
	entered := make(chan struct{}, 4)
	w := newControlWrites(nil, func(map[string]string) error {
		entered <- struct{}{}
		if fail.Load() {
			return errDisk
		}
		return nil
	})
	defer finishControlWrites(t, w)
	_ = w.SaveConfig("goal", "one")
	<-entered
	deadline := time.Now().Add(time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := w.Flush(ctx)
		cancel()
		if errors.Is(err, errDisk) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
	}
	if _, ok := w.LoadConfig("goal"); ok {
		t.Fatal("failed write reported stored")
	}
	fail.Store(false)
	<-entered
	deadline = time.Now().Add(time.Second)
	for {
		if _, ok := w.LoadConfig("goal"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("failed write was not retried")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestControlWritesBoundsAndAdmissionFailure(t *testing.T) {
	release := make(chan struct{})
	w := newControlWrites(nil, func(map[string]string) error { <-release; return nil })
	defer func() { close(release); finishControlWrites(t, w) }()
	for i := 0; i < controlWriteLimit; i++ {
		if err := w.SaveConfig(fmt.Sprint(i), "v"); !errors.Is(err, ErrWritePending) {
			t.Fatal(err)
		}
	}
	if err := w.SaveConfig("overflow", "v"); err == nil || errors.Is(err, ErrWritePending) {
		t.Fatal(err)
	}
	if err := w.Flush(context.Background()); err == nil {
		t.Fatal("rejected request falsely acknowledged")
	}
	if err := w.SaveConfig("0", strings.Repeat("x", controlWriteBytes+1)); err == nil || errors.Is(err, ErrWritePending) {
		t.Fatal(err)
	}
	// The owner handles these returned admission errors; clear them only in
	// this fixture so cleanup can drain the accepted work.
	w.mu.Lock()
	clear(w.rejected)
	w.mu.Unlock()
}

func TestControlWritesNewSafetyFaultRunsWhileDiskBlocked(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	w := newControlWrites(nil, func(map[string]string) error {
		once.Do(func() { close(entered); <-release })
		return nil
	})
	defer func() { close(release); finishControlWrites(t, w) }()
	m := loadpoint.NewManager()
	cfgs := []loadpoint.Config{{ID: "one", DriverName: "one", VehicleCapacityWh: 75000, MaxChargeW: 11000}, {ID: "two", DriverName: "two", VehicleCapacityWh: 75000, MaxChargeW: 11000}}
	m.Load(cfgs)
	m.SetSessionStore(w)
	now := time.Now()
	stops := map[string]bool{}
	c := loadpoint.NewController(m, nil, func(id string) (loadpoint.EVSample, bool) {
		return loadpoint.EVSample{Connected: true, RequestActive: true, PowerW: 3600, SessionWh: 100, PowerAt: now, EnergyAt: now, DeviceID: id, SessionID: "session"}, true
	}, func(_ context.Context, driver string, payload []byte) error {
		var command struct {
			Action string  `json:"action"`
			Power  float64 `json:"power_w"`
		}
		if err := json.Unmarshal(payload, &command); err != nil {
			return err
		}
		if command.Action == "ev_set_current" && command.Power == 0 {
			stops[driver] = true
		}
		return nil
	})
	for _, cfg := range cfgs {
		m.ObserveSession(cfg.ID, true, 3600, 100, true, cfg.ID, "session")
		m.SetCurrentSoC(cfg.ID, .78)
	}
	<-entered
	done := make(chan struct{})
	go func() {
		c.TickWithDispatch(context.Background(), now, true)
		clear(stops)
		// The fault arrives after a normal tick has queued storage work.
		c.TickWithDispatch(context.Background(), now.Add(time.Second), false)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disk write blocked the next safety decision")
	}
	if !stops["one"] || !stops["two"] {
		t.Fatal(stops)
	}
}
