package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// evPollingStore stands up a charger that is answering polls normally, with a
// car plugged in.
func evPollingStore(t *testing.T, name string) *telemetry.Store {
	t.Helper()
	tel := telemetry.NewStore()
	tel.Update(name, telemetry.DerEV, 0, nil, json.RawMessage(`{"connected":true}`))
	tel.DriverHealthMut(name).RecordSuccess()
	return tel
}

// evController mirrors main.go's wiring: the loadpoint controller, the same
// tracker the storage loop uses, and the hop between them. What is under test
// is that hop.
func evController(t *testing.T, tel *telemetry.Store, driver string, send loadpoint.SenderFunc,
	now time.Time) (*loadpoint.Controller, *driverActuationTracker) {
	t.Helper()
	cfg := loadpoint.Config{
		ID:            "garage",
		DriverName:    driver,
		MinChargeW:    1400,
		MaxChargeW:    11000,
		AllowedStepsW: []float64{0, 1400, 4100, 7400, 11000},
	}
	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{cfg})
	plan := loadpoint.PlanFunc(func(time.Time) (loadpoint.Directive, bool) {
		return loadpoint.Directive{
			SlotStart:         now.Add(-time.Second),
			SlotEnd:           now.Add(15 * time.Minute),
			LoadpointEnergyWh: map[string]float64{cfg.ID: 2750},
		}, true
	})
	telFn := loadpoint.TelemetryFunc(func(name string) (loadpoint.EVSample, bool) {
		r := tel.Get(name, telemetry.DerEV)
		if r == nil {
			return loadpoint.EVSample{}, false
		}
		var d struct {
			Connected bool `json:"connected"`
		}
		_ = json.Unmarshal(r.Data, &d)
		return loadpoint.EVSample{Connected: d.Connected, RequestActive: true}, true
	})
	tracker := newDriverActuationTracker(tel)
	c := loadpoint.NewController(mgr, plan, telFn, send)
	c.SetDispatchOutcome(tracker.recordCommandOutcome)
	return c, tracker
}

// A wallbox that answers every poll and refuses every setpoint holds the
// current it last accepted. Everything that plans around it — the EV load in
// the plan, the surplus reserve held back from the battery — goes on counting
// a charge that is not happening. Same failure as the storage one #800 fixed,
// on the other wire.
func TestRefusingLoadpointDriverIsExcludedAndDefaulted(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	tel := evPollingStore(t, "easee")
	send := loadpoint.SenderFunc(func(context.Context, string, []byte) error {
		return errors.New("driver_command returned false")
	})
	c, tracker := evController(t, tel, "easee", send, now)

	c.Tick(context.Background(), now)
	if !tel.DriverHealth("easee").IsOnline() {
		t.Fatal("one refused setpoint dropped the charger out of control")
	}

	for i := 1; i < driverRefusalLimit; i++ {
		now = now.Add(5 * time.Second)
		c.Tick(context.Background(), now)
	}

	h := tel.DriverHealth("easee")
	if h.IsOnline() {
		t.Fatalf("charger refused %d setpoints in a row and is still counted", driverRefusalLimit)
	}
	if h.Status != telemetry.StatusOk {
		t.Errorf("Status = %v, want ok — the charger is answering polls", h.Status)
	}
	assertStringsEqual(t, tracker.update(now, nil), []string{"easee"})
}

// The charger takes setpoints again: the next accepted one puts it back in
// without an operator.
func TestRecoveringLoadpointDriverIsCountedAgain(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	tel := evPollingStore(t, "easee")
	refusing := true
	send := loadpoint.SenderFunc(func(context.Context, string, []byte) error {
		if refusing {
			return errors.New("driver_command returned false")
		}
		return nil
	})
	c, _ := evController(t, tel, "easee", send, now)

	for i := 0; i < driverRefusalLimit; i++ {
		c.Tick(context.Background(), now.Add(time.Duration(i)*5*time.Second))
	}
	if tel.DriverHealth("easee").IsOnline() {
		t.Fatal("charger was not excluded")
	}

	refusing = false
	c.Tick(context.Background(), now.Add(time.Minute))
	if !tel.DriverHealth("easee").IsOnline() {
		t.Fatal("charger accepted a setpoint and is still excluded")
	}
}

// An unplugged charger receives no setpoint at all, so it can never accrue
// refusals — and must not be excluded for the silence.
func TestUnpluggedLoadpointIsNeverExcluded(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	tel := telemetry.NewStore()
	tel.Update("easee", telemetry.DerEV, 0, nil, json.RawMessage(`{"connected":false}`))
	tel.DriverHealthMut("easee").RecordSuccess()
	send := loadpoint.SenderFunc(func(context.Context, string, []byte) error {
		return errors.New("driver_command returned false")
	})
	c, tracker := evController(t, tel, "easee", send, now)

	for i := 0; i < driverRefusalLimit*2; i++ {
		c.Tick(context.Background(), now.Add(time.Duration(i)*5*time.Second))
	}

	if !tel.DriverHealth("easee").IsOnline() {
		t.Fatal("an unplugged charger was excluded without receiving a command")
	}
	if pending := tracker.update(now, nil); len(pending) != 0 {
		t.Fatalf("unplugged charger was sent a default: %v", pending)
	}
}
