package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// pvPollingStore stands up a PV inverter that answers every poll.
func pvPollingStore(t *testing.T, name string) *telemetry.Store {
	t.Helper()
	tel := telemetry.NewStore()
	tel.Update(name, telemetry.DerPV, -6000, nil, nil)
	tel.DriverHealthMut(name).RecordSuccess()
	return tel
}

func refusingSender(msg string) *stubSender {
	return &stubSender{handler: func(context.Context, string) error {
		return errors.New(msg)
	}}
}

// An inverter that takes every poll and refuses every cap keeps exporting
// into a negative price while the plan books the saving. Before this, the
// curtail loop threw the error away, so the inverter stayed online and stayed
// in the plan for as long as the operator left it there.
func TestRefusedCurtailExcludesInverterAndReachesTheDefault(t *testing.T) {
	tel := pvPollingStore(t, "sungrow")
	tracker := newDriverActuationTracker(tel)
	sender := refusingSender("driver_command returned false")
	now := time.Now()
	targets := []control.CurtailTarget{{Driver: "sungrow", LimitW: 2500}}

	dispatchPVCurtail(context.Background(), sender, tracker, targets, time.Second, now)
	if !tel.DriverHealth("sungrow").IsOnline() {
		t.Fatal("one refused cap dropped the inverter out of control")
	}

	for i := 1; i < driverRefusalLimit; i++ {
		now = now.Add(time.Second)
		dispatchPVCurtail(context.Background(), sender, tracker, targets, time.Second, now)
	}

	h := tel.DriverHealth("sungrow")
	if h.IsOnline() {
		t.Fatalf("inverter refused %d caps in a row and is still counted as controllable",
			driverRefusalLimit)
	}
	if h.Status != telemetry.StatusOk {
		t.Errorf("Status = %v, want ok — the inverter is answering polls", h.Status)
	}
	assertStringsEqual(t, tracker.update(now, nil), []string{"sungrow"})

	// The payload the inverter actually received is the cap, not something
	// the loop invented on its way through the tracker.
	calls := sender.recorded()
	if len(calls) != driverRefusalLimit {
		t.Fatalf("sent %d commands, want %d", len(calls), driverRefusalLimit)
	}
	if !strings.Contains(calls[0].payload, `"action":"curtail"`) ||
		!strings.Contains(calls[0].payload, `"power_w":2500`) {
		t.Errorf("cap payload = %s", calls[0].payload)
	}
}

// The negative case, and the one that matters. `curtail_disable` is core
// letting go of the inverter, so refusing it proves nothing about whether the
// device can be actuated — the same rule that reads a rejected
// set_self_consumption in sungrow.lua as the default held rather than failed.
//
// It is also the case that would seal shut: ComputePVCurtail emits a release
// the moment a driver drops offline, so an inverter excluded for any reason
// would refuse its own release and hold itself out for good.
func TestRefusedCurtailReleaseNeverExcludesInverter(t *testing.T) {
	tel := pvPollingStore(t, "ferroamp")
	tracker := newDriverActuationTracker(tel)
	sender := refusingSender("driver_command returned false")
	now := time.Now()
	targets := []control.CurtailTarget{{Driver: "ferroamp", LimitW: 0}}

	for i := 0; i < driverRefusalLimit*3; i++ {
		dispatchPVCurtail(context.Background(), sender, tracker, targets, time.Second, now)
		now = now.Add(time.Second)
	}

	if !tel.DriverHealth("ferroamp").IsOnline() {
		t.Fatal("an inverter that refused only releases was excluded from dispatch")
	}
	if pending := tracker.update(now, nil); len(pending) != 0 {
		t.Fatalf("refused releases produced a default request: %v", pending)
	}
	calls := sender.recorded()
	if len(calls) == 0 || !strings.Contains(calls[0].payload, `"action":"curtail_disable"`) {
		t.Fatalf("release payload = %+v", calls)
	}
}

// An excluded inverter must be able to come back. Its refusals stop the
// moment it takes a cap again.
func TestAcceptedCurtailClearsTheExclusion(t *testing.T) {
	tel := pvPollingStore(t, "sungrow")
	tracker := newDriverActuationTracker(tel)
	refusing := true
	sender := &stubSender{handler: func(context.Context, string) error {
		if refusing {
			return errors.New("driver_command returned false")
		}
		return nil
	}}
	now := time.Now()
	targets := []control.CurtailTarget{{Driver: "sungrow", LimitW: 2500}}

	for i := 0; i < driverRefusalLimit; i++ {
		dispatchPVCurtail(context.Background(), sender, tracker, targets, time.Second, now)
	}
	if tel.DriverHealth("sungrow").IsOnline() {
		t.Fatal("inverter was not excluded")
	}

	refusing = false
	dispatchPVCurtail(context.Background(), sender, tracker, targets, time.Second, now)
	if !tel.DriverHealth("sungrow").IsOnline() {
		t.Fatal("inverter accepted a cap and is still excluded")
	}
}
