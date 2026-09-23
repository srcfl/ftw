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

func TestControlWritesCommitSequenceTracksOnlyFinishedWrites(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var writes atomic.Int64
	w := newControlWrites(nil, func(map[string]string) error {
		if writes.Add(1) == 2 {
			close(entered)
			<-release
		}
		return nil
	})
	defer finishControlWrites(t, w)
	var once sync.Once
	defer once.Do(func() { close(release) })
	if err := w.SaveConfig("session", "first"); !errors.Is(err, ErrWritePending) {
		t.Fatal(err)
	}
	if err := w.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, first, ok := w.CommittedConfig("session")
	if !ok || first == 0 {
		t.Fatal("finished write has no sequence", first, ok)
	}
	if err := w.SaveConfig("session", "second"); !errors.Is(err, ErrWritePending) {
		t.Fatal(err)
	}
	<-entered
	if value, seq, ok := w.CommittedConfig("session"); !ok || value != "first" || seq != first {
		t.Fatal("unfinished write advanced committed state", value, seq, ok)
	}
	once.Do(func() { close(release) })
	if err := w.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	value, second, ok := w.CommittedConfig("session")
	if !ok || value != "second" || second <= first {
		t.Fatal("finished replacement did not advance sequence", value, second, ok)
	}
	if err := w.SaveConfig("session", "second"); err != nil {
		t.Fatal(err)
	}
	if _, seq, _ := w.CommittedConfig("session"); seq != second || writes.Load() != 2 {
		t.Fatal("unchanged value caused another write", seq, writes.Load())
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

func TestControlWritesVehicleGoalRestoresPastDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	w, err := s.NewEVControlWrites()
	if err != nil {
		t.Fatal(err)
	}
	before := time.Date(2026, 9, 17, 4, 0, 0, 0, time.UTC)
	newManager := func() *loadpoint.Manager {
		m := loadpoint.NewManager()
		m.Load([]loadpoint.Config{{ID: "garage", DriverName: "charger"}})
		m.SetSessionStore(w)
		m.SetNowFn(func() time.Time { return before })
		return m
	}
	m := newManager()
	m.SetScheduleSaver(func(id string, goal loadpoint.Schedule) error {
		data, err := json.Marshal(goal)
		if err != nil {
			return err
		}
		return s.SaveConfig("loadpoint_schedule:"+id, string(data))
	})
	m.SetSchedule("garage", loadpoint.Schedule{FinishAtVehicleLimit: true, Recurring: true, TimeOfDayMinUTC: 300})
	m.RollSchedules(before)
	m.ObserveSession("garage", true, 3600, 100, true, "charger", "same-session")
	if err := m.WaitForPersistence(context.Background()); err != nil {
		t.Fatal(err)
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
	m = newManager()
	m.HydrateSchedules(func(id string) (loadpoint.Schedule, bool) {
		raw, found := s.LoadConfig("loadpoint_schedule:" + id)
		var goal loadpoint.Schedule
		err := json.Unmarshal([]byte(raw), &goal)
		return goal, found && err == nil
	})
	m.RollSchedules(before.Add(2 * time.Hour))
	m.ObserveSession("garage", true, 3600, 200, true, "charger", "same-session")
	if st, _ := m.State("garage"); !st.TargetTime.Equal(before.Add(time.Hour)) || st.GoalRetention != "session" {
		t.Fatal("durable unfinished goal moved to tomorrow", st)
	}
}

func TestControlWritesCommittedProgressKeepsCheckpointBound(t *testing.T) {
	s := openTestStore(t)
	var writes atomic.Int64
	w := newControlWrites(nil, func(values map[string]string) error {
		writes.Add(1)
		return s.SaveConfigValues(values)
	})
	defer finishControlWrites(t, w)
	m := loadpoint.NewManager()
	m.Load([]loadpoint.Config{{ID: "garage", DriverName: "charger", VehicleCapacityWh: 75000}})
	m.SetSessionStore(w)
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	m.SetNowFn(func() time.Time { return at })
	sample := loadpoint.EVSample{Connected: true, RequestActive: true, DeviceID: "charger", SessionID: "session", PowerW: 3600, PowerAt: at, SessionWh: 1000, EnergyAt: at}
	m.ObserveSample("garage", sample)
	m.SetCurrentSoC("garage", .78)
	if err := m.WaitForPersistence(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 17; i++ {
		at = at.Add(5 * time.Second)
		sample.PowerAt = at
		m.ObserveSample("garage", sample)
		if err := m.WaitForPersistence(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := writes.Load(); n < 3 || n > 4 {
		t.Fatal("commits must follow the 30 Wh / 30 s checkpoint bound, not every tick", n)
	}
	before, _ := m.State("garage")
	if before.SoCRetention != "session" {
		t.Fatal(before)
	}
	// Restore through the committed cache and verify the bounded energy gap.
	m = loadpoint.NewManager()
	m.Load([]loadpoint.Config{{ID: "garage", DriverName: "charger", VehicleCapacityWh: 75000}})
	m.SetSessionStore(w)
	m.SetNowFn(func() time.Time { return at })
	m.ObserveSample("garage", sample)
	after, _ := m.State("garage")
	if before.DeliveredWhSession-after.DeliveredWhSession >= 30 {
		t.Fatal("checkpoint reduction lost too much progress", before.DeliveredWhSession, after.DeliveredWhSession)
	}
}

func TestControlDeviceCacheDoesNotWaitForDatabaseConnection(t *testing.T) {
	s := openTestStore(t)
	original := Device{DriverName: "meter", Make: "maker", Serial: "original", MAC: "aa:bb", Endpoint: "tcp://meter"}
	if _, err := s.RegisterDevice(original); err != nil {
		t.Fatal(err)
	}
	s.db.SetMaxOpenConns(1)
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	done := make(chan []Device, 1)
	go func() { done <- s.CachedDevices() }()
	select {
	case devices := <-done:
		if len(devices) != 1 || devices[0].Serial != "original" {
			t.Fatal(devices)
		}
	case <-time.After(time.Second):
		t.Fatal("identity read waited for a database connection")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	// Match SQL's coalesce behavior when a driver reports only its serial.
	if _, err := s.RegisterDevice(Device{DeviceID: "maker:original", DriverName: "renamed"}); err != nil {
		t.Fatal(err)
	}
	devices := s.CachedDevices()
	if len(devices) != 1 || devices[0].DriverName != "renamed" || devices[0].Serial != "original" || devices[0].MAC != original.MAC {
		t.Fatal(devices)
	}
}

func TestControlBatteryModelQueueDoesNotResolveIdentityAfterAdmission(t *testing.T) {
	s := openTestStore(t)
	w := s.NewBatteryModelWrites()
	defer finishControlWrites(t, w)
	s.db.SetMaxOpenConns(1)
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := w.SaveConfig("battery", `{"generation":"before-identity"}`); !errors.Is(err, ErrWritePending) {
		t.Fatal(err)
	}
	// A new identity is registered while the older model waits for disk.
	if _, err := conn.ExecContext(context.Background(), `INSERT INTO devices(device_id,driver_name,make,serial,mac,endpoint,first_seen_ms,last_seen_ms) VALUES ('maker:new','battery','maker','new','','',1,1)`); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	var key string
	if err := s.db.QueryRow(`SELECT name FROM battery_models`).Scan(&key); err != nil || key != "battery" {
		t.Fatal("queued model acquired another device's identity", key, err)
	}
}
