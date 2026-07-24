package homelink

import (
	"context"
	"encoding/base64"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/gatewayidentity"
	"github.com/srcfl/ftw/go/internal/state"
)

type resultReadDispatcherFunc func(
	context.Context,
	ReadTarget,
	ReadRequest,
	Principal,
) (ReadResponse, error)

func (f resultReadDispatcherFunc) DispatchRead(
	ctx context.Context,
	target ReadTarget,
	request ReadRequest,
	principal Principal,
) error {
	_, err := f(ctx, target, request, principal)
	return err
}

func (f resultReadDispatcherFunc) DispatchReadResult(
	ctx context.Context,
	target ReadTarget,
	request ReadRequest,
	principal Principal,
) (ReadResponse, error) {
	return f(ctx, target, request, principal)
}

func TestBoundReadConsumesOnceAndRejectsAnotherSession(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	manager := newGrantTestManager(t, true, &now, 71)
	request := testReadRequest(ScopePlanRead)
	requestID := testRawURL(16, 1)
	binding := testReadBinding(t, requestID, request)
	var dispatches atomic.Int32
	manager.readDispatcher = resultReadDispatcherFunc(func(
		_ context.Context,
		_ ReadTarget,
		_ ReadRequest,
		_ Principal,
	) (ReadResponse, error) {
		dispatches.Add(1)
		return ReadResponse{
			Version: ReadContractVersion, Scope: ScopePlanRead,
			Plan: &PlanReadResponse{
				Available: true, GeneratedAtMS: 1, Mode: "cost", HorizonSlots: 24,
			},
		}, nil
	})
	grant := issueTestBoundAccess(t, manager, request.Scope, binding)

	wrongBindings := []ReadBinding{}
	wrong := binding
	wrong.RouteGeneration++
	wrongBindings = append(wrongBindings, wrong)
	wrong = binding
	wrong.RouteHandle = testRawURL(gatewayidentity.RouteHandleBytes, 7)
	wrongBindings = append(wrongBindings, wrong)
	wrong = binding
	wrong.SessionID = testRawURL(16, 8)
	wrongBindings = append(wrongBindings, wrong)
	wrong = binding
	wrong.StreamID = testRawURL(16, 9)
	wrongBindings = append(wrongBindings, wrong)
	wrong = binding
	wrong.RequestHash[0] ^= 1
	wrongBindings = append(wrongBindings, wrong)
	wrong = binding
	wrong.GatewayID = "0123aabbcc01ddeeff"
	wrongBindings = append(wrongBindings, wrong)
	for _, wrong := range wrongBindings {
		if _, err := manager.VerifyAndDispatchBoundRead(
			context.Background(), grant.Token, requestID, request, wrong,
		); !errors.Is(err, ErrWrongBinding) {
			t.Fatalf("wrong session binding %+v = %v", wrong, err)
		}
	}
	if dispatches.Load() != 0 {
		t.Fatal("wrong binding dispatched")
	}
	if err := manager.VerifyAndDispatchRead(
		context.Background(), grant.Token, request,
	); !errors.Is(err, ErrWrongBinding) {
		t.Fatalf("bound grant used by legacy path = %v", err)
	}
	response, err := manager.VerifyAndDispatchBoundRead(
		context.Background(), grant.Token, requestID, request, binding,
	)
	if err != nil || response.Plan == nil {
		t.Fatalf("bound read = (%+v, %v)", response, err)
	}
	if _, err := manager.VerifyAndDispatchBoundRead(
		context.Background(), grant.Token, requestID, request, binding,
	); !errors.Is(err, ErrGrantConsumed) {
		t.Fatalf("bound replay = %v", err)
	}
	if dispatches.Load() != 1 {
		t.Fatalf("dispatches = %d", dispatches.Load())
	}
}

func TestBoundReadFailureAndCancelDoNotRestoreGrant(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	manager := newGrantTestManager(t, true, &now, 72)
	request := testReadRequest(ScopeHealthRead)
	requestID := testRawURL(16, 2)
	binding := testReadBinding(t, requestID, request)
	manager.readDispatcher = resultReadDispatcherFunc(func(
		ctx context.Context,
		_ ReadTarget,
		_ ReadRequest,
		_ Principal,
	) (ReadResponse, error) {
		<-ctx.Done()
		return ReadResponse{}, ctx.Err()
	})
	grant := issueTestBoundAccess(t, manager, request.Scope, binding)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.VerifyAndDispatchBoundRead(
		ctx, grant.Token, requestID, request, binding,
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled bound read = %v", err)
	}
	if _, err := manager.VerifyAndDispatchBoundRead(
		context.Background(), grant.Token, requestID, request, binding,
	); !errors.Is(err, ErrGrantConsumed) {
		t.Fatalf("retry after cancel = %v", err)
	}
}

func TestReadRequestHashBindsRequestIDAndEveryHistoryField(t *testing.T) {
	request := testReadRequest(ScopeEnergyHistoryRead)
	requestID := testRawURL(16, 21)
	want, err := ReadRequestHash(requestID, request)
	if err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		requestID string
		request   ReadRequest
	}{}
	mutated := cloneReadRequest(request)
	mutated.History.AssetID = "site/grid_meter"
	mutations = append(mutations, struct {
		requestID string
		request   ReadRequest
	}{requestID, mutated})
	mutated = cloneReadRequest(request)
	mutated.History.SinceMS++
	mutations = append(mutations, struct {
		requestID string
		request   ReadRequest
	}{requestID, mutated})
	mutated = cloneReadRequest(request)
	mutated.History.UntilMS++
	mutations = append(mutations, struct {
		requestID string
		request   ReadRequest
	}{requestID, mutated})
	mutated = cloneReadRequest(request)
	mutated.History.BucketMS *= 2
	mutations = append(mutations, struct {
		requestID string
		request   ReadRequest
	}{requestID, mutated})
	mutated = cloneReadRequest(request)
	mutated.History.Limit++
	mutations = append(mutations, struct {
		requestID string
		request   ReadRequest
	}{requestID, mutated})
	mutations = append(mutations, struct {
		requestID string
		request   ReadRequest
	}{testRawURL(16, 22), request})
	for _, mutation := range mutations {
		got, err := ReadRequestHash(mutation.requestID, mutation.request)
		if err != nil {
			t.Fatal(err)
		}
		if got == want {
			t.Fatalf("request hash ignored mutation: id=%q request=%+v", mutation.requestID, mutation.request)
		}
	}
}

func TestCoreReadAdapterDispatchesOnlyFourTypedSources(t *testing.T) {
	var healthCalls, planCalls, assetCalls, historyCalls atomic.Int32
	adapter, err := NewCoreReadAdapter(CoreReadSources{
		Health: func(context.Context) (HealthReadResponse, error) {
			healthCalls.Add(1)
			return HealthReadResponse{Status: "ok", CheckedAtMS: 1}, nil
		},
		Plan: func(context.Context) (PlanReadResponse, error) {
			planCalls.Add(1)
			return PlanReadResponse{Available: false}, nil
		},
		Assets: func(context.Context) ([]state.EnergyAsset, error) {
			assetCalls.Add(1)
			return []state.EnergyAsset{{AssetID: "site/grid_meter", Kind: state.AssetGridMeter}}, nil
		},
		History: func(
			_ context.Context,
			_ state.EnergyHistoryQuery,
		) ([]state.EnergyLedgerPoint, bool, error) {
			historyCalls.Add(1)
			return []state.EnergyLedgerPoint{{
				SchemaVersion: 1, AssetID: "site/grid_meter",
				Flow: state.FlowGridImport, BucketStartMS: 1,
				BucketLenMS: state.EnergyLedgerBucketMS,
				EnergyWh:    2, Source: "counter", Quality: "measured",
				Provenance: "hardware", SampleCount: 1,
			}}, false, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []Scope{
		ScopeHealthRead, ScopePlanRead, ScopeEnergyAssetsRead, ScopeEnergyHistoryRead,
	} {
		request := testReadRequest(scope)
		response, err := adapter.DispatchReadResult(
			context.Background(), readTargets[scope], request, testPrincipal(),
		)
		if err != nil {
			t.Fatalf("%s: %v", scope, err)
		}
		if response.Scope != scope || response.Validate() != nil {
			t.Fatalf("%s response = %+v", scope, response)
		}
	}
	if healthCalls.Load() != 1 || planCalls.Load() != 1 ||
		assetCalls.Load() != 1 || historyCalls.Load() != 1 {
		t.Fatalf("calls = %d/%d/%d/%d", healthCalls.Load(), planCalls.Load(), assetCalls.Load(), historyCalls.Load())
	}
}

func TestCoreReadAdapterAllowsSeveralFlowsPerHistoryBucket(t *testing.T) {
	points := []state.EnergyLedgerPoint{
		{
			SchemaVersion: 1, AssetID: "system", Flow: state.FlowGridImport,
			BucketStartMS: 1, BucketLenMS: state.EnergyLedgerBucketMS,
			EnergyWh: 2, Source: "counter", Quality: "measured",
			Provenance: "hardware", SampleCount: 1,
		},
		{
			SchemaVersion: 1, AssetID: "system", Flow: state.FlowPVGeneration,
			BucketStartMS: 1, BucketLenMS: state.EnergyLedgerBucketMS,
			EnergyWh: 3, Source: "counter", Quality: "measured",
			Provenance: "hardware", SampleCount: 1,
		},
	}
	adapter, err := NewCoreReadAdapter(CoreReadSources{
		Health: func(context.Context) (HealthReadResponse, error) {
			return HealthReadResponse{}, nil
		},
		Plan: func(context.Context) (PlanReadResponse, error) {
			return PlanReadResponse{}, nil
		},
		Assets: func(context.Context) ([]state.EnergyAsset, error) {
			return nil, nil
		},
		History: func(
			_ context.Context,
			query state.EnergyHistoryQuery,
		) ([]state.EnergyLedgerPoint, bool, error) {
			if query.Limit != 1 {
				t.Fatalf("history bucket limit = %d, want 1", query.Limit)
			}
			return points, false, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := testReadRequest(ScopeEnergyHistoryRead)
	request.History.Limit = 1
	response, err := adapter.DispatchReadResult(
		context.Background(), readTargets[request.Scope], request, testPrincipal(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if response.EnergyHistory == nil || len(response.EnergyHistory.Points) != len(points) {
		t.Fatalf("history response = %+v", response.EnergyHistory)
	}
}

func TestDisabledBoundReadNeverCallsLocalCore(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	manager := newGrantTestManager(t, false, &now, 73)
	var calls atomic.Int32
	manager.readDispatcher = resultReadDispatcherFunc(func(
		context.Context, ReadTarget, ReadRequest, Principal,
	) (ReadResponse, error) {
		calls.Add(1)
		return ReadResponse{}, nil
	})
	request := testReadRequest(ScopeHealthRead)
	requestID := testRawURL(16, 3)
	binding := testReadBinding(t, requestID, request)
	if _, err := manager.VerifyAndDispatchBoundRead(
		context.Background(), "invalid", requestID, request, binding,
	); !errors.Is(err, ErrRemoteDisabled) {
		t.Fatalf("disabled bound read = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("disabled remote read called Core")
	}
}

func issueTestBoundAccess(
	t *testing.T,
	manager *GrantManager,
	scope Scope,
	binding ReadBinding,
) Grant {
	t.Helper()
	challenge, err := manager.BeginLocalAssertion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	grant, err := manager.IssueOneUseBoundAccess(
		context.Background(), challenge.ID, testAssertion(), scope, time.Minute, binding,
	)
	if err != nil {
		t.Fatal(err)
	}
	return grant
}

func testReadBinding(t *testing.T, requestID string, request ReadRequest) ReadBinding {
	t.Helper()
	hash, err := ReadRequestHash(requestID, request)
	if err != nil {
		t.Fatal(err)
	}
	return ReadBinding{
		GatewayID:       testGatewayID,
		RouteHandle:     testRawURL(gatewayidentity.RouteHandleBytes, 4),
		RouteGeneration: 1,
		SessionID:       testRawURL(16, 5),
		StreamID:        testRawURL(16, 6),
		RequestHash:     hash,
	}
}

func testRawURL(size int, fill byte) string {
	return base64.RawURLEncoding.EncodeToString(makeFilled(size, fill))
}

func makeFilled(size int, fill byte) []byte {
	value := make([]byte, size)
	for i := range value {
		value[i] = fill
	}
	return value
}
