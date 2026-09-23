package mpc

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
)

func TestPlanSnapshotFailedRequestDoesNotMakeOldPlanCurrent(t *testing.T) {
	svc := newCancellationTestService(t, nil)
	now := time.Now()
	svc.InstallPlan(Plan{GeneratedAtMs: now.UnixMilli(), Actions: []Action{{
		SlotStartMs: now.UnixMilli(), SlotLenMin: 60, LoadpointW: 6000,
	}}}, svc.Defaults, "garage")
	before := svc.PlanSnapshot()
	if windows, wh := before.LoadpointPlanWindows("garage", now, 0); len(windows) != 1 || wh != 6000 {
		t.Fatalf("initial windows=%v wh=%v", windows, wh)
	}
	// Missing prices keeps the last result for diagnostics. It must neither
	// display nor dispatch that result as the newly saved setting.
	svc.Zone = "no-prices"
	svc.RequestReplan("saved-goal")
	waitForRequestedReplans(t, svc)
	failed := svc.PlanSnapshot()
	if failed.Pending || !failed.Outdated || failed.Plan != before.Plan || !failed.ReplanAt.Equal(before.ReplanAt) || failed.Reason != before.Reason {
		t.Fatalf("failed request freshness=%+v", failed)
	}
	if windows, wh := failed.LoadpointPlanWindows("garage", now, 0); len(windows) != 0 || wh != 0 {
		t.Fatalf("failed new request exposes old windows=%v wh=%v", windows, wh)
	}
	assertPlanDispatchAvailable(t, svc, now, false)

	svc.Zone = "SE3"
	svc.RequestReplan("retry-goal")
	waitForRequestedReplans(t, svc)
	after := svc.PlanSnapshot()
	if after.Pending || after.Outdated || after.Plan == nil || after.Plan == before.Plan || after.Reason != "retry-goal" {
		t.Fatalf("successful retry freshness=%+v", after)
	}
	assertPlanDispatchAvailable(t, svc, time.UnixMilli(after.Plan.Actions[0].ExecutionStart()), true)
	// A caller that already captured the old snapshot keeps its own matching
	// plan and legacy loadpoint ID even after the service publishes another.
	if windows, wh := before.LoadpointPlanWindows("garage", now, 0); len(windows) != 1 || wh != 6000 {
		t.Fatalf("captured snapshot changed with the service: windows=%v wh=%v", windows, wh)
	}
}

func assertPlanDispatchAvailable(t *testing.T, svc *Service, now time.Time, want bool) {
	t.Helper()
	if _, ok := svc.SlotDirectiveAt(now); ok != want {
		t.Fatalf("energy dispatch available=%v, want %v", ok, want)
	}
	if _, _, _, ok := svc.SlotAt(now); ok != want {
		t.Fatalf("legacy dispatch available=%v, want %v", ok, want)
	}
}

func TestPendingReplanKeepsPlanUntilFailureButRetryCannotReviveIt(t *testing.T) {
	svc := newCancellationTestService(t, nil)
	now := time.Now()
	svc.InstallPlan(Plan{GeneratedAtMs: now.UnixMilli(), Actions: []Action{{
		SlotStartMs: now.UnixMilli(), SlotLenMin: 60, BatteryW: 2000, LoadpointW: 6000,
	}}}, svc.Defaults, "garage")
	request := svc.beginReplan(context.Background(), "saved-goal")
	assertPlanDispatchAvailable(t, svc, now, true)
	// Finishing without publishing is the same path as missing prices or a
	// rejected solve. A retry alone is not a fresh, validated plan.
	svc.finishReplan(request)
	assertPlanDispatchAvailable(t, svc, now, false)
	retry := svc.beginReplan(context.Background(), "retry-goal")
	assertPlanDispatchAvailable(t, svc, now, false)
	svc.finishReplan(retry)
}

func TestFailedGoalReplanStopsOldEVAllocationButKeepsManualControl(t *testing.T) {
	svc := newCancellationTestService(t, nil)
	now := time.Now()
	svc.InstallPlan(Plan{GeneratedAtMs: now.UnixMilli(), Actions: []Action{{
		SlotStartMs: now.UnixMilli(), SlotLenMin: 60, LoadpointW: 6000,
	}}}, svc.Defaults, "garage")
	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{{ID: "garage", DriverName: "charger", VehicleCapacityWh: 60000,
		MinChargeW: 4140, MaxChargeW: 11000, PhaseMode: "3p"}})
	mgr.SetSchedule("garage", loadpoint.Schedule{SoC: .9, TimeOfDayMinUTC: 7 * 60, Recurring: true})
	var sent []float64
	ctrl := loadpoint.NewController(mgr, func(at time.Time) (loadpoint.Directive, bool) {
		d, ok := svc.SlotDirectiveAt(at)
		return d.LoadpointDirective(), ok
	}, func(string) (loadpoint.EVSample, bool) {
		return loadpoint.EVSample{Connected: true, RequestActive: true, DeviceID: "easee:A", SessionID: "session-1"}, true
	}, func(_ context.Context, _ string, payload []byte) error {
		var command struct {
			Action string  `json:"action"`
			PowerW float64 `json:"power_w"`
		}
		if err := json.Unmarshal(payload, &command); err != nil {
			return err
		}
		// ev_resume is a side effect of a 0 W → offer edge; this test
		// is about the setpoint the charger is ordered to deliver.
		if command.Action != "" && command.Action != "ev_set_current" {
			return nil
		}
		sent = append(sent, command.PowerW)
		return nil
	})
	ctrl.SetSiteFuse(loadpoint.SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3})
	tickPower := func() float64 {
		t.Helper()
		before := len(sent)
		ctrl.Tick(context.Background(), now)
		now = now.Add(time.Second)
		if len(sent) != before+1 {
			t.Fatalf("tick sent %d commands", len(sent)-before)
		}
		return sent[len(sent)-1]
	}
	if got := tickPower(); got <= 0 {
		t.Fatalf("initial plan did not charge: %v", got)
	}
	mgr.ClearSchedule("garage")
	svc.Zone = "no-prices"
	svc.RequestReplan("schedule_cleared")
	waitForRequestedReplans(t, svc)
	if got := tickPower(); got != 0 {
		t.Fatalf("removed goal still dispatched %v W", got)
	}
	ctrl.SetManualHold("garage", loadpoint.ManualHold{PowerW: 4140, Persistent: true})
	if got := tickPower(); got != 4140 {
		t.Fatalf("manual Start lost during failed plan: %v W", got)
	}
	ctrl.SetManualHold("garage", loadpoint.ManualHold{Persistent: true})
	if got := tickPower(); got != 0 {
		t.Fatalf("manual Pause lost during failed plan: %v W", got)
	}
}

func TestPlanSnapshotIsCurrentBeforeDiagnosticWriteFinishes(t *testing.T) {
	svc := newCancellationTestService(t, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(func() { unblock(); waitForRequestedReplans(t, svc) })
	svc.SaveDiag = func(*Diagnostic, string) error {
		close(entered)
		<-release
		return nil
	}
	svc.RequestReplan("saved-soc")
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not publish its plan")
	}
	snapshot := svc.PlanSnapshot()
	if snapshot.Plan == nil || snapshot.ReplanAt.IsZero() || snapshot.Reason != "saved-soc" || snapshot.Pending || snapshot.Outdated {
		t.Fatalf("published snapshot=%+v", snapshot)
	}
	if !svc.IsReplanning() {
		t.Fatal("work waiter lost the outstanding diagnostic write")
	}
}

func TestRequestReplanCoalescesEditsWhileOldSolveIsBlocked(t *testing.T) {
	optimizer := &blockingFirstOptimizer{
		firstMode: make(chan Mode, 1), secondMode: make(chan Mode, 1), releaseFirst: make(chan struct{}),
	}
	svc := newCancellationTestService(t, optimizer)
	var once sync.Once
	unblock := func() { once.Do(func() { close(optimizer.releaseFirst) }) }
	t.Cleanup(func() { unblock(); waitForRequestedReplans(t, svc) })
	svc.RequestReplan("first")
	select {
	case <-optimizer.firstMode:
	case <-time.After(time.Second):
		t.Fatal("first request did not reach optimizer")
	}
	for i := 0; i < 100; i++ {
		svc.RequestReplan(fmt.Sprintf("edit-%d", i))
	}
	if calls := optimizer.calls.Load(); calls != 1 {
		t.Fatalf("edits started %d parallel solves while the old one was blocked", calls)
	}
	if snapshot := svc.PlanSnapshot(); !snapshot.Pending || !snapshot.Outdated || snapshot.Plan != nil {
		t.Fatalf("pending edits freshness=%+v", snapshot)
	}
	unblock()
	waitForRequestedReplans(t, svc)
	if calls := optimizer.calls.Load(); calls != 2 {
		t.Fatalf("optimizer calls=%d, want old request and latest edit only", calls)
	}
	snapshot := svc.PlanSnapshot()
	if snapshot.Plan == nil || snapshot.Pending || snapshot.Outdated || snapshot.Reason != "edit-99" {
		t.Fatalf("latest edit not published: %+v", snapshot)
	}
}

func TestStopWaitsForRequestedReplanAndDiscardsQueuedEdits(t *testing.T) {
	optimizer := &stopWaitOptimizer{
		secondStarted: make(chan struct{}), secondCanceled: make(chan struct{}),
		releaseSecond: make(chan struct{}), closed: make(chan struct{}),
	}
	svc := newCancellationTestService(t, optimizer)
	var once sync.Once
	unblock := func() { once.Do(func() { close(optimizer.releaseSecond) }) }
	svc.Start(context.Background())
	t.Cleanup(func() { unblock(); svc.Stop() })
	deadline := time.Now().Add(time.Second)
	for svc.Latest() == nil || svc.IsReplanning() {
		if time.Now().After(deadline) {
			t.Fatal("initial scheduled plan did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	svc.RequestReplan("blocked-edit")
	select {
	case <-optimizer.secondStarted:
	case <-time.After(time.Second):
		t.Fatal("requested plan did not reach optimizer")
	}
	for i := 0; i < 10; i++ {
		svc.RequestReplan("queued-edit")
	}
	stopped := make(chan struct{})
	go func() { svc.Stop(); close(stopped) }()
	deadline = time.Now().Add(time.Second)
	for {
		svc.mu.RLock()
		stopping := svc.stopping
		svc.mu.RUnlock()
		if stopping {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Stop did not begin")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-stopped:
		t.Fatal("Stop returned while the optimizer still ran")
	default:
	}
	unblock()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not drain the requested replan queue")
	}
	svc.RequestReplan("after-stop")
	if optimizer.closedWhileActive.Load() || optimizer.calls.Load() != 2 {
		t.Fatalf("unsafe stop: closed while active=%v, calls=%d", optimizer.closedWhileActive.Load(), optimizer.calls.Load())
	}
	if snapshot := svc.PlanSnapshot(); snapshot.Pending || !snapshot.Outdated {
		t.Fatalf("stopped request freshness=%+v", snapshot)
	}
}

func waitForRequestedReplans(t *testing.T, svc *Service) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		svc.mu.RLock()
		idle := !svc.requestedReplanRunning && svc.queuedReplan == nil
		svc.mu.RUnlock()
		if idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("requested replans did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}
