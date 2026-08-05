package loadpoint

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
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
