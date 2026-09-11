package mpc

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

type blockingFirstOptimizer struct {
	calls        atomic.Int32
	firstMode    chan Mode
	secondMode   chan Mode
	releaseFirst chan struct{}
}

func (o *blockingFirstOptimizer) Optimize(ctx context.Context, slots []Slot, p Params) (Plan, error) {
	call := o.calls.Add(1)
	if call == 1 {
		o.firstMode <- p.Mode
		// Deliberately ignore cancellation. The generation check must still
		// reject a backend that cannot stop promptly.
		<-o.releaseFirst
	} else if call == 2 {
		o.secondMode <- p.Mode
	}

	plan := Optimize(slots, p)
	status := "old-self-consumption"
	if call == 2 {
		status = "new-arbitrage"
	}
	plan.Solver = &SolverInfo{
		Engine: "test", Backend: "blocking", Status: status,
		Formulation: "deterministic",
	}
	return plan, nil
}

func (*blockingFirstOptimizer) Close() error { return nil }

type cancelThenSolveOptimizer struct {
	calls         atomic.Int32
	firstStarted  chan struct{}
	firstCanceled chan struct{}
	secondStarted chan struct{}
	releaseSecond chan struct{}
}

func (o *cancelThenSolveOptimizer) Optimize(ctx context.Context, slots []Slot, p Params) (Plan, error) {
	switch o.calls.Add(1) {
	case 1:
		close(o.firstStarted)
		<-ctx.Done()
		close(o.firstCanceled)
		return Plan{}, ctx.Err()
	case 2:
		close(o.secondStarted)
		select {
		case <-o.releaseSecond:
		case <-ctx.Done():
			return Plan{}, ctx.Err()
		}
		plan := Optimize(slots, p)
		plan.Solver = &SolverInfo{
			Engine: "test", Backend: "cancel-aware", Status: "new-arbitrage",
			Formulation: "deterministic",
		}
		return plan, nil
	default:
		return Plan{}, context.Canceled
	}
}

func (*cancelThenSolveOptimizer) Close() error { return nil }

type callerCancelOptimizer struct {
	solveCtx chan context.Context
	canceled chan struct{}
}

func (o *callerCancelOptimizer) Optimize(ctx context.Context, _ []Slot, _ Params) (Plan, error) {
	o.solveCtx <- ctx
	<-ctx.Done()
	close(o.canceled)
	return Plan{}, ctx.Err()
}

func (*callerCancelOptimizer) Close() error { return nil }

type waitForCancelOptimizer struct {
	calls    atomic.Int32
	started  chan struct{}
	canceled chan struct{}
	closed   chan struct{}
}

func (o *waitForCancelOptimizer) Optimize(ctx context.Context, _ []Slot, _ Params) (Plan, error) {
	o.calls.Add(1)
	close(o.started)
	<-ctx.Done()
	close(o.canceled)
	return Plan{}, ctx.Err()
}

func (o *waitForCancelOptimizer) Close() error {
	if o.closed != nil {
		close(o.closed)
	}
	return nil
}

type stopWaitOptimizer struct {
	calls             atomic.Int32
	active            atomic.Int32
	secondStarted     chan struct{}
	secondCanceled    chan struct{}
	releaseSecond     chan struct{}
	closed            chan struct{}
	closedWhileActive atomic.Bool
}

func (o *stopWaitOptimizer) Optimize(ctx context.Context, slots []Slot, p Params) (Plan, error) {
	o.active.Add(1)
	defer o.active.Add(-1)

	call := o.calls.Add(1)
	if call == 2 {
		close(o.secondStarted)
		<-ctx.Done()
		close(o.secondCanceled)
		// Deliberately ignore cancellation until the test releases this call.
		// Stop must not close the optimizer while this backend is still active.
		<-o.releaseSecond
	}

	plan := Optimize(slots, p)
	status := "initial"
	if call == 2 {
		status = "external"
	}
	plan.Solver = &SolverInfo{
		Engine: "test", Backend: "stop-wait", Status: status,
		Formulation: "deterministic",
	}
	return plan, nil
}

func (o *stopWaitOptimizer) Close() error {
	if o.active.Load() != 0 {
		o.closedWhileActive.Store(true)
	}
	close(o.closed)
	return nil
}

func newCancellationTestService(t *testing.T, optimizer PlanOptimizer) *Service {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	now := time.Now().UTC().Truncate(time.Hour)
	prices := make([]state.PricePoint, 4)
	for i := range prices {
		prices[i] = state.PricePoint{
			Zone: "SE3", SlotTsMs: now.Add(time.Duration(i) * time.Hour).UnixMilli(),
			SlotLenMin: 60, SpotOreKwh: float64(40 + i*20), TotalOreKwh: float64(90 + i*20),
			Source: "test", FetchedAtMs: now.UnixMilli(),
		}
	}
	if err := st.SavePrices(prices); err != nil {
		t.Fatalf("save prices: %v", err)
	}

	svc := New(st, nil, "SE3", Params{
		Mode: ModeSelfConsumption, SoCLevels: 11, ActionLevels: 5,
		CapacityWh: 10000, InitialSoC: 0.5, SoCMin: 0.1, SoCMax: 0.95,
		MaxChargeW: 3000, MaxDischargeW: 3000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
	})
	svc.Horizon = 4 * time.Hour
	svc.BaseLoad = 500
	svc.Optimizer = optimizer
	return svc
}

func TestReplanNewestRequestWinsWhenOlderSolveFinishesLast(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer st.Close()

	now := time.Now().UTC().Truncate(time.Hour)
	prices := make([]state.PricePoint, 4)
	for i := range prices {
		prices[i] = state.PricePoint{
			Zone: "SE3", SlotTsMs: now.Add(time.Duration(i) * time.Hour).UnixMilli(),
			SlotLenMin: 60, SpotOreKwh: float64(40 + i*20), TotalOreKwh: float64(90 + i*20),
			Source: "test", FetchedAtMs: now.UnixMilli(),
		}
	}
	if err := st.SavePrices(prices); err != nil {
		t.Fatalf("save prices: %v", err)
	}

	optimizer := &blockingFirstOptimizer{
		firstMode:    make(chan Mode, 1),
		secondMode:   make(chan Mode, 1),
		releaseFirst: make(chan struct{}),
	}
	svc := New(st, nil, "SE3", Params{
		Mode: ModeSelfConsumption, SoCLevels: 11, ActionLevels: 5,
		CapacityWh: 10000, InitialSoC: 0.5, SoCMin: 0.1, SoCMax: 0.95,
		MaxChargeW: 3000, MaxDischargeW: 3000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
	})
	svc.Horizon = 4 * time.Hour
	svc.BaseLoad = 500
	svc.Optimizer = optimizer
	var decisionIDCalls atomic.Int32
	svc.decisionIDFactory = func() string {
		decisionIDCalls.Add(1)
		return testDecisionID1
	}

	type savedDiagnostic struct {
		mode       Mode
		reason     string
		decisionID string
	}
	saved := make(chan savedDiagnostic, 2)
	svc.SaveDiag = func(d *Diagnostic, reason string) error {
		saved <- savedDiagnostic{mode: d.Params.Mode, reason: reason, decisionID: d.DecisionID}
		return nil
	}

	oldDone := make(chan *Plan, 1)
	go func() {
		oldDone <- svc.ReplanWithReason(context.Background(), "old-self-consumption")
	}()

	if mode := <-optimizer.firstMode; mode != ModeSelfConsumption {
		t.Fatalf("first solve mode = %q, want %q", mode, ModeSelfConsumption)
	}

	// The second solve starts after the mode change and finishes while the
	// first solve remains blocked.
	svc.SetMode(context.Background(), ModeArbitrage)
	if mode := <-optimizer.secondMode; mode != ModeArbitrage {
		t.Fatalf("second solve mode = %q, want %q", mode, ModeArbitrage)
	}

	published := svc.Latest()
	if published == nil || published.Solver == nil || published.Solver.Status != "new-arbitrage" {
		t.Fatalf("newer plan was not published: %+v", published)
	}
	if published.DecisionID != testDecisionID1 {
		t.Fatalf("published decision ID = %q, want %q", published.DecisionID, testDecisionID1)
	}
	if got := <-saved; got.mode != ModeArbitrage || got.reason != "mode_changed" || got.decisionID != published.DecisionID {
		t.Fatalf("saved diagnostic = %+v, want arbitrage/mode_changed", got)
	}

	close(optimizer.releaseFirst)
	if got := <-oldDone; got != published {
		t.Fatalf("superseded caller returned an unpublished plan: got=%p published=%p", got, published)
	}

	svc.mu.RLock()
	lastMode := svc.lastParams.Mode
	lastReason := svc.lastReason
	lastGeneration := svc.latestReplanGeneration
	svc.mu.RUnlock()
	if lastMode != ModeArbitrage || lastReason != "mode_changed" || lastGeneration != 2 {
		t.Fatalf("published state = mode %q reason %q generation %d", lastMode, lastReason, lastGeneration)
	}
	if latest := svc.Latest(); latest != published {
		t.Fatal("older solve replaced the newer published plan")
	}
	if got := decisionIDCalls.Load(); got != 1 {
		t.Fatalf("decision ID factory calls = %d, want 1 accepted plan", got)
	}
	if d := svc.Diagnose(); d == nil || d.Params.Mode != ModeArbitrage || d.LastReason != "mode_changed" {
		t.Fatalf("diagnostic was replaced by the older solve: %+v", d)
	}
	select {
	case extra := <-saved:
		t.Fatalf("superseded solve persisted a diagnostic: %+v", extra)
	default:
	}
}

func TestNewerReplanCancelsOlderSolveBeforePublishing(t *testing.T) {
	optimizer := &cancelThenSolveOptimizer{
		firstStarted:  make(chan struct{}),
		firstCanceled: make(chan struct{}),
		secondStarted: make(chan struct{}),
		releaseSecond: make(chan struct{}),
	}
	svc := newCancellationTestService(t, optimizer)

	type savedDiagnostic struct {
		mode   Mode
		reason string
	}
	saved := make(chan savedDiagnostic, 2)
	svc.SaveDiag = func(d *Diagnostic, reason string) error {
		saved <- savedDiagnostic{mode: d.Params.Mode, reason: reason}
		return nil
	}

	oldDone := make(chan *Plan, 1)
	go func() {
		oldDone <- svc.ReplanWithReason(context.Background(), "old-self-consumption")
	}()
	select {
	case <-optimizer.firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first solve did not start")
	}

	newDone := make(chan struct{})
	go func() {
		svc.SetMode(context.Background(), ModeArbitrage)
		close(newDone)
	}()
	select {
	case <-optimizer.firstCanceled:
	case <-time.After(time.Second):
		t.Fatal("newer replan did not cancel the older solve")
	}
	select {
	case <-optimizer.secondStarted:
	case <-time.After(time.Second):
		t.Fatal("second solve did not start")
	}
	select {
	case got := <-oldDone:
		if got != nil {
			t.Fatalf("canceled solve returned unpublished plan: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled solve did not return")
	}
	if got := svc.Latest(); got != nil {
		t.Fatalf("canceled solve published before replacement: %+v", got)
	}
	select {
	case got := <-saved:
		t.Fatalf("canceled solve persisted diagnostic: %+v", got)
	default:
	}

	close(optimizer.releaseSecond)
	select {
	case <-newDone:
	case <-time.After(time.Second):
		t.Fatal("newer solve did not finish")
	}
	published := svc.Latest()
	if published == nil || published.Solver == nil || published.Solver.Status != "new-arbitrage" {
		t.Fatalf("newer plan was not published: %+v", published)
	}
	if got := <-saved; got.mode != ModeArbitrage || got.reason != "mode_changed" {
		t.Fatalf("saved diagnostic = %+v, want arbitrage/mode_changed", got)
	}
	if got := optimizer.calls.Load(); got != 2 {
		t.Fatalf("optimizer calls = %d, want 2", got)
	}
}

func TestCallerCancellationStopsExternalSolveAndKeepsFallback(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cancelBefore bool
	}{
		{name: "after optimizer starts"},
		{name: "before replan starts", cancelBefore: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			optimizer := &callerCancelOptimizer{
				solveCtx: make(chan context.Context, 1),
				canceled: make(chan struct{}),
			}
			svc := newCancellationTestService(t, optimizer)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelBefore {
				cancel()
			}
			done := make(chan *Plan, 1)
			go func() { done <- svc.Replan(ctx) }()
			var solveCtx context.Context
			select {
			case solveCtx = <-optimizer.solveCtx:
			case <-time.After(time.Second):
				t.Fatal("external solve did not start")
			}
			if !tc.cancelBefore {
				cancel()
			}
			select {
			case <-optimizer.canceled:
			case <-time.After(time.Second):
				t.Fatal("caller cancellation did not reach external solve")
			}
			if !errors.Is(solveCtx.Err(), context.Canceled) {
				t.Fatalf("external solve context error = %v, want context.Canceled", solveCtx.Err())
			}
			select {
			case got := <-done:
				if got == nil || got.Solver == nil || !got.Solver.Fallback || got.Solver.Status != "fallback" {
					t.Fatalf("caller-canceled solve did not keep Go fallback: %+v", got)
				}
			case <-time.After(time.Second):
				t.Fatal("caller-canceled replan did not return")
			}
		})
	}
}

func TestStopCancelsActiveReplanAndRejectsNewWork(t *testing.T) {
	optimizer := &waitForCancelOptimizer{
		started:  make(chan struct{}),
		canceled: make(chan struct{}),
		closed:   make(chan struct{}),
	}
	svc := newCancellationTestService(t, optimizer)
	svc.Start(context.Background())
	select {
	case <-optimizer.started:
	case <-time.After(time.Second):
		t.Fatal("scheduled solve did not start")
	}

	stopped := make(chan struct{})
	go func() {
		svc.Stop()
		close(stopped)
	}()
	select {
	case <-optimizer.canceled:
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel active solve")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return after cancellation")
	}
	select {
	case <-optimizer.closed:
	default:
		t.Fatal("Stop did not close optimizer")
	}

	if got := svc.Replan(context.Background()); got != nil {
		t.Fatalf("replan after Stop returned plan: %+v", got)
	}
	if got := optimizer.calls.Load(); got != 1 {
		t.Fatalf("optimizer calls after Stop = %d, want 1", got)
	}
}

func TestStopWaitsForExternalReplanBeforeClosingOptimizer(t *testing.T) {
	optimizer := &stopWaitOptimizer{
		secondStarted:  make(chan struct{}),
		secondCanceled: make(chan struct{}),
		releaseSecond:  make(chan struct{}),
		closed:         make(chan struct{}),
	}
	svc := newCancellationTestService(t, optimizer)
	saved := make(chan string, 2)
	svc.SaveDiag = func(d *Diagnostic, _ string) error {
		saved <- d.Solver.Status
		return nil
	}

	svc.Start(context.Background())
	select {
	case got := <-saved:
		if got != "initial" {
			t.Fatalf("initial diagnostic status = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("initial scheduled replan did not publish")
	}

	externalDone := make(chan *Plan, 1)
	go func() {
		externalDone <- svc.ReplanWithReason(context.Background(), "external")
	}()
	select {
	case <-optimizer.secondStarted:
	case <-time.After(time.Second):
		t.Fatal("external replan did not start")
	}

	var releaseStarted atomic.Bool
	returnedBeforeRelease := make(chan string, 2)
	firstStopped := make(chan struct{})
	go func() {
		svc.Stop()
		if !releaseStarted.Load() {
			returnedBeforeRelease <- "first"
		}
		close(firstStopped)
	}()
	select {
	case <-optimizer.secondCanceled:
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel external replan")
	}
	select {
	case <-svc.done:
	case <-time.After(time.Second):
		t.Fatal("planner loop did not stop")
	}
	secondStopped := make(chan struct{})
	go func() {
		svc.Stop()
		if !releaseStarted.Load() {
			returnedBeforeRelease <- "second"
		}
		close(secondStopped)
	}()

	if got := svc.Replan(context.Background()); got == nil || got.Solver == nil || got.Solver.Status != "initial" {
		t.Fatalf("replan during Stop returned %+v, want existing plan", got)
	}
	if got := optimizer.calls.Load(); got != 2 {
		t.Fatalf("optimizer calls during Stop = %d, want 2", got)
	}
	select {
	case <-optimizer.closed:
		t.Fatal("optimizer closed while external replan was active")
	case which := <-returnedBeforeRelease:
		t.Fatalf("%s Stop returned while external replan was active", which)
	case <-time.After(50 * time.Millisecond):
	}

	releaseStarted.Store(true)
	close(optimizer.releaseSecond)
	select {
	case got := <-externalDone:
		if got == nil || got.Solver == nil || got.Solver.Status != "initial" {
			t.Fatalf("canceled external replan returned %+v, want existing plan", got)
		}
	case <-time.After(time.Second):
		t.Fatal("external replan did not return")
	}
	select {
	case <-firstStopped:
	case <-time.After(time.Second):
		t.Fatal("first Stop did not return after external replan exited")
	}
	select {
	case <-secondStopped:
	case <-time.After(time.Second):
		t.Fatal("second Stop did not return after external replan exited")
	}
	select {
	case <-optimizer.closed:
	default:
		t.Fatal("Stop did not close optimizer")
	}
	if optimizer.closedWhileActive.Load() {
		t.Fatal("optimizer Close overlapped Optimize")
	}
	if got := svc.Latest(); got == nil || got.Solver == nil || got.Solver.Status != "initial" {
		t.Fatalf("canceled external plan was published: %+v", got)
	}
	select {
	case got := <-saved:
		t.Fatalf("canceled external replan persisted diagnostic %q", got)
	default:
	}
}
