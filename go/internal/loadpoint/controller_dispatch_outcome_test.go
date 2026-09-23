package loadpoint

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// outcomeSender answers per driver so a test can refuse the charger and the
// vehicle independently. Guarded, because the contactor cycle sends from its
// own goroutine.
type outcomeSender struct {
	mu     sync.Mutex
	calls  []outcomeCall
	errFor map[string]error
}

type outcomeCall struct {
	driver string
	action string
	powerW float64
}

func (s *outcomeSender) Send(_ context.Context, driver string, payload []byte) error {
	var d struct {
		Action string  `json:"action"`
		PowerW float64 `json:"power_w"`
	}
	if err := json.Unmarshal(payload, &d); err != nil {
		return err
	}
	s.mu.Lock()
	s.calls = append(s.calls, outcomeCall{driver: driver, action: d.Action, powerW: d.PowerW})
	err := s.errFor[driver]
	s.mu.Unlock()
	return err
}

func (s *outcomeSender) sent() []outcomeCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]outcomeCall(nil), s.calls...)
}

// outcomeLog collects what the controller reported about its dispatch sends.
type outcomeLog struct {
	mu      sync.Mutex
	drivers []string
	errs    []error
}

func (o *outcomeLog) record(driver string, err error, _ time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.drivers = append(o.drivers, driver)
	o.errs = append(o.errs, err)
}

func (o *outcomeLog) reported() ([]string, []error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.drivers...), append([]error(nil), o.errs...)
}

// outcomeFixture is one plugged-in charger with a slot allocation big enough
// to produce a non-zero setpoint.
func outcomeFixture(t *testing.T, now time.Time, sender *outcomeSender) (*Controller, Config, *outcomeLog) {
	t.Helper()
	cfg := Config{
		ID:            "garage",
		DriverName:    "easee",
		MinChargeW:    1400,
		MaxChargeW:    11000,
		AllowedStepsW: []float64{0, 1400, 4100, 7400, 11000},
	}
	directive := Directive{
		SlotStart:         now.Add(-time.Second),
		SlotEnd:           now.Add(15 * time.Minute),
		LoadpointEnergyWh: map[string]float64{cfg.ID: 2750},
	}
	m := NewManager()
	m.Load([]Config{cfg})
	plan := PlanFunc(func(time.Time) (Directive, bool) { return directive, true })
	tel := TelemetryFunc(func(driver string) (EVSample, bool) {
		if driver != cfg.DriverName {
			return EVSample{}, false
		}
		return EVSample{Connected: true, RequestActive: true}, true
	})
	c := NewController(m, plan, tel, sender.Send)
	log := &outcomeLog{}
	c.SetDispatchOutcome(log.record)
	return c, cfg, log
}

// The charger answers every poll and refuses every setpoint. Before this was
// wired the controller logged the refusal and moved on, so nothing upstream
// ever learned that the EV load the plan booked was not being drawn.
func TestTickUsesOutcomeSenderNotPlainSend(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	plain := &outcomeSender{}
	c, _, _ := outcomeFixture(t, now, plain)
	var outcomeN int
	c.SetOutcomeSender(func(_ context.Context, _ string, _ []byte, outcome func(error)) error {
		outcomeN++
		if outcome != nil {
			outcome(nil)
		}
		return nil
	})
	c.Tick(context.Background(), now)
	if n := len(plain.sent()); n != 0 {
		t.Fatalf("plain send used %d times; periodic dispatch must use the outcome sender", n)
	}
	if outcomeN != 1 {
		t.Fatalf("outcome sender used %d times, want 1", outcomeN)
	}
}

func TestRefusedEVSetCurrentIsReported(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	refusal := errors.New("driver_command returned false")
	sender := &outcomeSender{errFor: map[string]error{"easee": refusal}}
	c, cfg, log := outcomeFixture(t, now, sender)

	c.Tick(context.Background(), now)

	drivers, errs := log.reported()
	if len(drivers) != 1 {
		t.Fatalf("refused ev_set_current reported %d times, want 1", len(drivers))
	}
	if drivers[0] != cfg.DriverName {
		t.Errorf("reported driver %q, want %q", drivers[0], cfg.DriverName)
	}
	if !errors.Is(errs[0], refusal) {
		t.Errorf("reported error %v, want the driver's refusal", errs[0])
	}
}

// An accepted setpoint is reported too, or a charger that recovers stays
// excluded until the retry window happens to let it back in.
func TestAcceptedEVSetCurrentIsReported(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	sender := &outcomeSender{}
	c, _, log := outcomeFixture(t, now, sender)

	c.Tick(context.Background(), now)

	_, errs := log.reported()
	if len(errs) != 1 || errs[0] != nil {
		t.Fatalf("accepted ev_set_current reported as %v", errs)
	}
}

// Core's command-fault state is a dispatch gate, not just a planning hint.
// Observation stays live while an excluded charger waits for its timed retry,
// but no setpoint or success outcome may leak through and clear the fault.
func TestOfflineDriverSkipsDispatchButKeepsObservation(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	sender := &outcomeSender{}
	c, cfg, log := outcomeFixture(t, now, sender)
	c.SetDriverOnline(func(string) bool { return false })

	c.Tick(context.Background(), now)

	if calls := sender.sent(); len(calls) != 0 {
		t.Fatalf("offline charger received dispatch: %+v", calls)
	}
	if drivers, _ := log.reported(); len(drivers) != 0 {
		t.Fatalf("offline charger produced a synthetic success outcome: %v", drivers)
	}
	state, ok := c.manager.State(cfg.ID)
	if !ok || !state.PluggedIn {
		t.Fatalf("offline dispatch gate discarded the live plug observation: %+v, ok=%v", state, ok)
	}
}

// ev_set_current can be the call that crosses Core's refusal threshold. The
// outcome callback closes DriverHealth synchronously, so this same tick must
// not continue into a vehicle wake or a charger contactor cycle.
func TestDispatchFaultStopsWakeWorkInTheSameTick(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	sender := &outcomeSender{}
	c, cfg, _ := outcomeFixture(t, now, sender)
	var online atomic.Bool
	online.Store(true)
	c.SetDriverOnline(func(string) bool { return online.Load() })
	c.SetDispatchOutcome(func(string, error, time.Time) { online.Store(false) })
	c.SetVehicleStatus(func(string) (string, string, bool) {
		return "tesla", "Stopped", true
	})
	oldGap := wallboxCycleGap
	wallboxCycleGap = 0
	t.Cleanup(func() { wallboxCycleGap = oldGap })

	c.Tick(context.Background(), now)
	// Old behavior queued ev_pause/ev_resume after sending charge_start.
	// Give that goroutine time to run so absence covers async work too.
	time.Sleep(20 * time.Millisecond)

	sent := sender.sent()
	if len(sent) != 1 || sent[0].driver != cfg.DriverName || sent[0].action != "ev_set_current" {
		t.Fatalf("wake work escaped after dispatch closed health: %+v", sent)
	}
	if sent[0].powerW <= 0 {
		t.Fatalf("test did not create a real wake condition: %+v", sent[0])
	}
	state, ok := c.manager.State(cfg.ID)
	if !ok || !state.PluggedIn {
		t.Fatalf("post-dispatch health gate discarded observation: %+v, ok=%v", state, ok)
	}
}

// A cycle runs off-thread. Health can close after ev_pause but before
// ev_resume, so each physical send needs its own current check.
func TestWallboxCycleChecksHealthBeforeEachSend(t *testing.T) {
	var online atomic.Bool
	online.Store(true)
	var checks atomic.Int32
	pauseSeen := make(chan struct{})
	releasePause := make(chan struct{})
	secondCheck := make(chan struct{})
	var pauseOnce, checkOnce sync.Once
	var callsMu sync.Mutex
	var calls []string
	send := SenderFunc(func(_ context.Context, _ string, payload []byte) error {
		var command struct {
			Action string `json:"action"`
		}
		if err := json.Unmarshal(payload, &command); err != nil {
			return err
		}
		callsMu.Lock()
		calls = append(calls, command.Action)
		callsMu.Unlock()
		if command.Action == "ev_pause" {
			pauseOnce.Do(func() { close(pauseSeen) })
			<-releasePause
		}
		return nil
	})
	c := NewController(NewManager(), nil, nil, send)
	c.SetDriverOnline(func(string) bool {
		if checks.Add(1) == 2 {
			checkOnce.Do(func() { close(secondCheck) })
		}
		return online.Load()
	})
	oldGap := wallboxCycleGap
	wallboxCycleGap = 0
	t.Cleanup(func() { wallboxCycleGap = oldGap })

	c.cycleWallbox("garage", "easee")
	select {
	case <-pauseSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("wallbox cycle never reached ev_pause")
	}
	online.Store(false)
	close(releasePause)
	select {
	case <-secondCheck:
	case <-time.After(2 * time.Second):
		t.Fatal("wallbox cycle never rechecked health before ev_resume")
	}

	callsMu.Lock()
	defer callsMu.Unlock()
	if len(calls) != 1 || calls[0] != "ev_pause" {
		t.Fatalf("offline cycle sent after pause: %v", calls)
	}
}

// A slow charger must consume only its own deadline. Tick walks loadpoints
// in order, so an unbounded first send otherwise prevents every charger after
// it from receiving this tick's setpoint.
func TestBlockedChargerDoesNotStallAnotherLoadpoint(t *testing.T) {
	now := time.Date(2026, 8, 16, 13, 0, 0, 0, time.UTC)
	configs := []Config{
		{ID: "slow", DriverName: "slow", MinChargeW: 1400, MaxChargeW: 7400},
		{ID: "fast", DriverName: "fast", MinChargeW: 1400, MaxChargeW: 7400},
	}
	m := NewManager()
	m.Load(configs)
	directive := Directive{
		SlotStart: now.Add(-time.Second),
		SlotEnd:   now.Add(15 * time.Minute),
		LoadpointEnergyWh: map[string]float64{
			"slow": 1850,
			"fast": 1850,
		},
	}
	plan := PlanFunc(func(time.Time) (Directive, bool) { return directive, true })
	tel := TelemetryFunc(func(string) (EVSample, bool) {
		return EVSample{Connected: true, RequestActive: true}, true
	})
	slowEntered := make(chan struct{})
	fastSent := make(chan struct{})
	releaseSlow := make(chan struct{})
	var slowOnce, fastOnce sync.Once
	send := SenderFunc(func(ctx context.Context, driver string, _ []byte) error {
		if driver == "fast" {
			fastOnce.Do(func() { close(fastSent) })
			return nil
		}
		slowOnce.Do(func() { close(slowEntered) })
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-releaseSlow:
			return nil
		}
	})
	c := NewController(m, plan, tel, send)
	c.SetCommandTimeout(20 * time.Millisecond)
	done := make(chan struct{})
	go func() {
		c.Tick(context.Background(), now)
		close(done)
	}()
	select {
	case <-slowEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("slow charger never received its command")
	}
	select {
	case <-fastSent:
	case <-time.After(50 * time.Millisecond):
		close(releaseSlow)
		<-done
		t.Fatal("slow charger blocked the next loadpoint past its command deadline")
	}
	close(releaseSlow)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("tick did not finish")
	}
}

// The operator API gives ForceStartVehicle 15 seconds because a BLE-backed
// vehicle command can take far longer than the short periodic dispatch
// deadline. Keep that caller deadline when the controller has a fast tick.
func TestForceStartKeepsVehicleCallerDeadline(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "garage", DriverName: "easee"}})
	entered := make(chan struct{})
	release := make(chan struct{})
	send := SenderFunc(func(ctx context.Context, driver string, _ []byte) error {
		if driver != "tesla" {
			t.Fatalf("ForceStart sent to %q, want tesla", driver)
		}
		close(entered)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return nil
		}
	})
	c := NewController(m, nil, nil, send)
	c.SetVehicleStatus(func(string) (string, string, bool) { return "tesla", "Stopped", true })
	c.SetCommandTimeout(10 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := c.ForceStartVehicle(ctx, "garage")
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("ForceStart did not reach the vehicle sender")
	}
	select {
	case err := <-result:
		t.Fatalf("ForceStart returned after the dispatch timeout: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("ForceStart returned %v, want nil after the slow vehicle send", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ForceStart did not return after the vehicle sender released")
	}
}

// A schedule edit asks RefreshVehicle for a fresh vehicle reading. It shares
// the vehicle-send path with ForceStart and must not inherit the tick timeout.
func TestScheduleRefreshKeepsVehicleCallerDeadline(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	send := SenderFunc(func(ctx context.Context, driver string, _ []byte) error {
		if driver != "tesla" {
			t.Fatalf("RefreshVehicle sent to %q, want tesla", driver)
		}
		close(entered)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return nil
		}
	})
	c := NewController(NewManager(), nil, nil, send)
	c.SetVehicleStatus(func(string) (string, string, bool) { return "tesla", "Stopped", true })
	c.SetCommandTimeout(10 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- c.RefreshVehicle(ctx, "garage") }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("schedule refresh did not reach the vehicle sender")
	}
	select {
	case err := <-result:
		t.Fatalf("schedule refresh returned after the dispatch timeout: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("schedule refresh returned %v, want nil after the slow vehicle send", err)
		}
	case <-time.After(time.Second):
		t.Fatal("schedule refresh did not return after the vehicle sender released")
	}
}

// The automatic charge_start runs from the control tick without an API
// deadline. It must use vehicleWakeTimeout, not the periodic charger limit.
func TestAutoChargeStartKeepsVehicleWakeTimeout(t *testing.T) {
	now := time.Date(2026, 8, 16, 13, 0, 0, 0, time.UTC)
	vehicleEntered := make(chan struct{})
	releaseVehicle := make(chan struct{})
	send := SenderFunc(func(ctx context.Context, driver string, payload []byte) error {
		var command struct {
			Action string `json:"action"`
		}
		if err := json.Unmarshal(payload, &command); err != nil {
			return err
		}
		if driver != "tesla" || command.Action != "charge_start" {
			return nil
		}
		close(vehicleEntered)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-releaseVehicle:
			return nil
		}
	})
	c, _, _ := outcomeFixture(t, now, &outcomeSender{})
	c.send = send
	c.SetVehicleStatus(func(string) (string, string, bool) { return "tesla", "Stopped", true })
	c.SetCommandTimeout(10 * time.Millisecond)
	done := make(chan struct{})
	go func() {
		c.Tick(context.Background(), now)
		close(done)
	}()
	select {
	case <-vehicleEntered:
	case <-time.After(time.Second):
		t.Fatal("automatic charge_start did not reach the vehicle sender")
	}
	select {
	case <-done:
		t.Fatal("automatic charge_start returned after the dispatch timeout")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseVehicle)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("tick did not return after the vehicle sender released")
	}
}

// The 0 W standdown is core withdrawing under a stale site meter, not core
// actuating. The staleness tracker owns that transition, and a charger that
// refuses the standdown must not be excluded for a fault belonging to the
// meter.
func TestSafetyStanddownRefusalIsNotReported(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	sender := &outcomeSender{errFor: map[string]error{
		"easee": errors.New("driver_command returned false"),
	}}
	c, _, log := outcomeFixture(t, now, sender)

	c.TickWithDispatch(context.Background(), now, false)

	if sent := sender.sent(); len(sent) != 1 || sent[0].powerW != 0 {
		t.Fatalf("standdown did not reach the charger: %+v", sent)
	}
	if drivers, _ := log.reported(); len(drivers) != 0 {
		t.Fatalf("standdown refusal was counted against the charger: %v", drivers)
	}
}

// A parked car refuses charge_start whenever it is asleep, and the vehicle
// driver behind it is perfectly healthy. Counting that would take the car's
// SoC out of the plan for napping, and wakeVehicleAuto already backs off on
// its own.
func TestVehicleWakeRefusalIsNotReported(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	sender := &outcomeSender{errFor: map[string]error{
		"tesla": errors.New("HTTP 503 vehicle asleep"),
	}}
	c, _, log := outcomeFixture(t, now, sender)
	c.SetVehicleStatus(func(string) (string, string, bool) { return "tesla", "Stopped", true })

	c.Tick(context.Background(), now)

	var wakes int
	for _, call := range sender.sent() {
		if call.driver == "tesla" && call.action == "charge_start" {
			wakes++
		}
	}
	if wakes == 0 {
		t.Fatal("a stopped vehicle received no wake — the test proves nothing")
	}
	drivers, _ := log.reported()
	for _, driver := range drivers {
		if driver == "tesla" {
			t.Fatal("a sleeping vehicle's refused wake was counted as an actuation failure")
		}
	}
}

// The contactor cycle is documented as free for any charger implementing
// ev_pause / ev_resume. A charger that implements neither returns an error
// and is behaving correctly; counting it would exclude a whole class of
// working hardware. It also runs on its own goroutine, which the tracker is
// not built to take.
func TestWallboxCycleRefusalIsNotReported(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	sender := &outcomeSender{errFor: map[string]error{
		"easee": errors.New("CTEK: unknown action ev_pause"),
	}}
	c, cfg, log := outcomeFixture(t, now, sender)

	c.cycleWallbox(cfg.ID, cfg.DriverName)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(sender.sent()) == 0 {
		time.Sleep(time.Millisecond)
	}

	sent := sender.sent()
	if len(sent) == 0 || sent[0].action != "ev_pause" {
		t.Fatalf("contactor cycle sent nothing to refuse: %+v", sent)
	}
	if drivers, _ := log.reported(); len(drivers) != 0 {
		t.Fatalf("contactor-cycle refusal was counted against the charger: %v", drivers)
	}
}
