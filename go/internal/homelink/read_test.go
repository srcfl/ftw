package homelink

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

func TestReadTargetsAreFixedCoreGETs(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	manager := newGrantTestManager(t, true, &now, 18)
	want := map[Scope]string{
		ScopeOverviewRead:      "/api/home-link/overview",
		ScopeHealthRead:        "/api/health",
		ScopePlanRead:          "/api/mpc/plan",
		ScopeEnergyAssetsRead:  "/api/energy/assets",
		ScopeEnergyHistoryRead: "/api/energy/history",
	}
	for scope, path := range want {
		grant := issueTestAccess(t, manager, scope, time.Minute)
		called := false
		dispatcher := readDispatcherFunc(func(_ context.Context, target ReadTarget, _ ReadRequest, _ Principal) error {
			called = true
			if target.Method != http.MethodGet || target.Path != path {
				t.Errorf("target %q = %+v, want GET %s", scope, target, path)
			}
			return nil
		})
		manager.readDispatcher = dispatcher
		if err := manager.VerifyAndDispatchRead(
			context.Background(), grant.Token, testReadRequest(scope),
		); err != nil {
			t.Fatalf("target %q: %v", scope, err)
		}
		if !called {
			t.Fatalf("target %q was not dispatched", scope)
		}
	}

	request := ReadRequest{Version: ReadContractVersion, GatewayID: testGatewayID, Scope: Scope("ftw.control.write")}
	if err := manager.VerifyAndDispatchRead(context.Background(), "unused", request); err == nil {
		t.Fatal("write scope resolved to a Core target")
	}
	request.Scope = Scope("ftw.status.read")
	if err := manager.VerifyAndDispatchRead(context.Background(), "unused", request); err == nil {
		t.Fatal("side-effecting status handler resolved to a remote target")
	}
}

func TestOneUseGrantExtendsThroughReadDispatch(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	manager := newGrantTestManager(t, true, &now, 19)
	grant := issueTestAccess(t, manager, ScopePlanRead, time.Minute)
	dispatches := 0
	dispatcher := readDispatcherFunc(func(context.Context, ReadTarget, ReadRequest, Principal) error {
		dispatches++
		return nil
	})
	manager.readDispatcher = dispatcher
	request := testReadRequest(ScopePlanRead)
	if err := manager.VerifyAndDispatchRead(context.Background(), grant.Token, request); err != nil {
		t.Fatal(err)
	}
	if err := manager.VerifyAndDispatchRead(context.Background(), grant.Token, request); !errors.Is(err, ErrGrantConsumed) {
		t.Fatalf("second read = %v", err)
	}
	if dispatches != 1 {
		t.Fatalf("dispatch count = %d, want 1", dispatches)
	}
}

func TestConcurrentReadReplayDispatchesOnce(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	manager := newGrantTestManager(t, true, &now, 24)
	grant := issueTestAccess(t, manager, ScopePlanRead, time.Minute)
	var dispatches atomic.Int32
	manager.readDispatcher = readDispatcherFunc(func(context.Context, ReadTarget, ReadRequest, Principal) error {
		dispatches.Add(1)
		return nil
	})

	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			results <- manager.VerifyAndDispatchRead(
				context.Background(), grant.Token, testReadRequest(ScopePlanRead),
			)
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	var accepted, consumed int
	for err := range results {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, ErrGrantConsumed):
			consumed++
		default:
			t.Fatalf("concurrent read = %v", err)
		}
	}
	if accepted != 1 || consumed != 1 || dispatches.Load() != 1 {
		t.Fatalf("accepted=%d consumed=%d dispatches=%d", accepted, consumed, dispatches.Load())
	}
}

func TestGrantExpirySamplesMonotonicClockAfterManagerLock(t *testing.T) {
	wallNow := time.Unix(1_800_000_000, 0)
	monotonicNow := time.Duration(0)
	manager := newGrantTestManagerWithAuthority(
		t, true, func() time.Time { return wallNow }, func() time.Duration { return monotonicNow }, 51,
		newMemoryCredentialAuthority(newMemoryCredentialState()),
	)
	grant := issueTestAccess(t, manager, ScopePlanRead, time.Second)
	var dispatches atomic.Int32
	manager.readDispatcher = readDispatcherFunc(func(context.Context, ReadTarget, ReadRequest, Principal) error {
		dispatches.Add(1)
		return nil
	})

	sampled := make(chan struct{}, 1)
	manager.mu.Lock()
	manager.monotonicNow = func() time.Duration {
		sampled <- struct{}{}
		return monotonicNow
	}
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		close(started)
		result <- manager.VerifyAndDispatchRead(
			context.Background(), grant.Token, testReadRequest(ScopePlanRead),
		)
	}()
	<-started

	sampledBeforeLock := false
	select {
	case <-sampled:
		sampledBeforeLock = true
	case <-time.After(50 * time.Millisecond):
	}
	monotonicNow = time.Second
	manager.mu.Unlock()
	if sampledBeforeLock {
		t.Fatal("grant consume sampled monotonic time before acquiring the manager lock")
	}
	if err := <-result; !errors.Is(err, ErrGrantExpired) {
		t.Fatalf("read after lock crossed deadline = %v", err)
	}
	if dispatches.Load() != 0 {
		t.Fatalf("expired grant dispatched %d reads", dispatches.Load())
	}
}

func TestDispatchFailureStillConsumesGrant(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	manager := newGrantTestManager(t, true, &now, 20)
	grant := issueTestAccess(t, manager, ScopePlanRead, time.Minute)
	wantErr := errors.New("Core read failed")
	dispatcher := readDispatcherFunc(func(context.Context, ReadTarget, ReadRequest, Principal) error {
		return wantErr
	})
	manager.readDispatcher = dispatcher
	request := testReadRequest(ScopePlanRead)
	if err := manager.VerifyAndDispatchRead(context.Background(), grant.Token, request); !errors.Is(err, wantErr) {
		t.Fatalf("dispatch failure = %v", err)
	}
	if err := manager.VerifyAndDispatchRead(context.Background(), grant.Token, request); !errors.Is(err, ErrGrantConsumed) {
		t.Fatalf("retry after dispatch failure = %v", err)
	}
}

func TestMissingDispatcherDoesNotConsumeGrant(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	manager := newGrantTestManager(t, true, &now, 21)
	grant := issueTestAccess(t, manager, ScopePlanRead, time.Minute)
	request := testReadRequest(ScopePlanRead)
	manager.readDispatcher = nil
	if err := manager.VerifyAndDispatchRead(context.Background(), grant.Token, request); err == nil {
		t.Fatal("missing dispatcher was accepted")
	}
	manager.readDispatcher = successfulReadDispatcher()
	if err := manager.VerifyAndDispatchRead(context.Background(), grant.Token, request); err != nil {
		t.Fatalf("missing dispatcher consumed grant: %v", err)
	}

	method, ok := reflect.TypeOf(manager).MethodByName("VerifyAndDispatchRead")
	if !ok {
		t.Fatal("VerifyAndDispatchRead is missing")
	}
	if method.Type.NumIn() != 4 {
		t.Fatalf("remote read can choose its dispatcher: %s", method.Type)
	}
}

func TestCredentialRevokeWaitsForStartedDispatchAndBlocksLaterReads(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	authority := newMemoryCredentialAuthority(newMemoryCredentialState())
	first := newGrantTestManagerWithAuthority(
		t, true, func() time.Time { return now }, func() time.Duration { return 0 }, 25,
		authority,
	)
	second := newGrantTestManagerWithAuthority(
		t, true, func() time.Time { return now }, func() time.Duration { return 0 }, 26,
		authority,
	)
	startedGrant := issueTestAccess(t, first, ScopePlanRead, time.Minute)
	pendingGrant := issueTestAccess(t, first, ScopePlanRead, time.Minute)
	dispatchStarted := make(chan struct{})
	dispatchMayFinish := make(chan struct{})
	first.readDispatcher = readDispatcherFunc(func(context.Context, ReadTarget, ReadRequest, Principal) error {
		close(dispatchStarted)
		<-dispatchMayFinish
		return nil
	})

	readResult := make(chan error, 1)
	go func() {
		readResult <- first.VerifyAndDispatchRead(
			context.Background(), startedGrant.Token, testReadRequest(ScopePlanRead),
		)
	}()
	<-dispatchStarted
	revokeResult := make(chan error, 1)
	go func() {
		revokeResult <- second.RevokeCredential(
			context.Background(), testPrincipal().CredentialID,
		)
	}()
	select {
	case err := <-revokeResult:
		close(dispatchMayFinish)
		<-readResult
		t.Fatalf("revoke returned before started dispatch: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(dispatchMayFinish)
	if err := <-readResult; err != nil {
		t.Fatalf("revoke canceled an already started dispatch: %v", err)
	}
	if err := <-revokeResult; err != nil {
		t.Fatal(err)
	}
	if err := first.VerifyAndDispatchRead(
		context.Background(), pendingGrant.Token, testReadRequest(ScopePlanRead),
	); !errors.Is(err, ErrCredentialRevoked) {
		t.Fatalf("pending grant after revoke = %v", err)
	}
}

func TestReentrantCredentialRevokeDoesNotDeadlockDispatch(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	authority := newMemoryCredentialAuthority(newMemoryCredentialState())
	manager := newGrantTestManagerWithAuthority(
		t, true, func() time.Time { return now }, func() time.Duration { return 0 }, 68,
		authority,
	)
	grant := issueTestAccess(t, manager, ScopePlanRead, time.Minute)
	revokeResult := make(chan error, 1)
	manager.readDispatcher = readDispatcherFunc(func(
		ctx context.Context,
		_ ReadTarget,
		_ ReadRequest,
		principal Principal,
	) error {
		revokeResult <- manager.RevokeCredential(ctx, principal.CredentialID)
		return nil
	})

	dispatchResult := make(chan error, 1)
	go func() {
		dispatchResult <- manager.VerifyAndDispatchRead(
			context.Background(), grant.Token, testReadRequest(ScopePlanRead),
		)
	}()
	select {
	case err := <-revokeResult:
		if !errors.Is(err, ErrCredentialDispatchBusy) {
			t.Fatalf("reentrant revoke = %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("reentrant revoke deadlocked")
	}
	select {
	case err := <-dispatchResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("dispatch after reentrant revoke = %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("dispatch did not stop after reentrant revoke")
	}
}

func TestCredentialRevokeBoundsHungDispatch(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	authority := newMemoryCredentialAuthority(newMemoryCredentialState())
	first := newGrantTestManagerWithAuthority(
		t, true, func() time.Time { return now }, func() time.Duration { return 0 }, 69,
		authority,
	)
	second := newGrantTestManagerWithAuthority(
		t, true, func() time.Time { return now }, func() time.Duration { return 0 }, 70,
		authority,
	)
	startedGrant := issueTestAccess(t, first, ScopePlanRead, time.Minute)
	pendingGrant := issueTestAccess(t, first, ScopePlanRead, time.Minute)
	dispatchStarted := make(chan struct{})
	releaseDispatch := make(chan struct{})
	first.readDispatcher = readDispatcherFunc(func(
		context.Context,
		ReadTarget,
		ReadRequest,
		Principal,
	) error {
		close(dispatchStarted)
		<-releaseDispatch
		return nil
	})
	readResult := make(chan error, 1)
	go func() {
		readResult <- first.VerifyAndDispatchRead(
			context.Background(), startedGrant.Token, testReadRequest(ScopePlanRead),
		)
	}()
	<-dispatchStarted

	revokeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	startedAt := time.Now()
	err := second.RevokeCredential(revokeCtx, testPrincipal().CredentialID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hung-dispatch revoke = %v", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Fatalf("hung-dispatch revoke took %v", elapsed)
	}
	if err := first.VerifyAndDispatchRead(
		context.Background(), pendingGrant.Token, testReadRequest(ScopePlanRead),
	); !errors.Is(err, ErrCredentialRevoked) {
		t.Fatalf("pending grant after bounded revoke = %v", err)
	}
	close(releaseDispatch)
	if err := <-readResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("hung dispatch after revoke = %v", err)
	}
}

func TestHistoryQueryIsBoundedByLedgerV1(t *testing.T) {
	valid := *validHistoryQuery()
	if err := ValidateHistoryQuery(valid); err != nil {
		t.Fatal(err)
	}

	cases := []state.EnergyHistoryQuery{
		{SinceMS: 2, UntilMS: 1, BucketMS: state.EnergyLedgerBucketMS, Limit: 1},
		{SinceMS: 1, UntilMS: 1 + state.EnergyLedgerRetention.Milliseconds() + 1, BucketMS: state.EnergyLedgerRollupBucketMS, Limit: 1},
		{SinceMS: 1, UntilMS: 2, BucketMS: state.EnergyLedgerBucketMS - 1, Limit: 1},
		{SinceMS: 1, UntilMS: 2, BucketMS: MaxHistoryWindowMS + 1, Limit: 1},
		{SinceMS: 1, UntilMS: 2, BucketMS: state.EnergyLedgerBucketMS, Limit: 0},
		{SinceMS: 1, UntilMS: 2, BucketMS: state.EnergyLedgerBucketMS, Limit: MaxHistoryLimit + 1},
	}
	for _, query := range cases {
		if err := ValidateHistoryQuery(query); err == nil {
			t.Fatalf("accepted unbounded history query: %+v", query)
		}
	}
}

func TestEnergyTypesAreLedgerV1Types(t *testing.T) {
	if EnergyLedgerSchemaVersion != 1 {
		t.Fatalf("ledger schema = %d", EnergyLedgerSchemaVersion)
	}
	point := EnergyLedgerPoint{
		SchemaVersion: 1, AssetID: "site/grid_meter", Flow: state.FlowGridImport,
		BucketStartMS: 1, BucketLenMS: state.EnergyLedgerBucketMS, EnergyWh: 2,
		Source: "hardware_counter", Quality: "measured", Provenance: "counter", SampleCount: 1,
	}
	raw, err := json.Marshal(point)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	want := []string{"schema_version", "asset_id", "flow", "bucket_start_ms", "bucket_len_ms", "energy_wh", "source", "quality", "provenance", "sample_count"}
	for _, field := range want {
		if _, ok := fields[field]; !ok {
			t.Fatalf("ledger point lacks %q: %s", field, raw)
		}
	}
	if reflect.TypeOf(point) != reflect.TypeOf(state.EnergyLedgerPoint{}) {
		t.Fatal("remote history created a second point type")
	}
}

func validHistoryQuery() *state.EnergyHistoryQuery {
	until := time.Unix(1_800_000_000, 0).UnixMilli()
	return &state.EnergyHistoryQuery{
		SinceMS: until - int64(24*time.Hour/time.Millisecond), UntilMS: until,
		BucketMS: int64(15 * time.Minute / time.Millisecond), Limit: 100,
	}
}
