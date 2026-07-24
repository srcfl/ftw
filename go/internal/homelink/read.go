package homelink

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/gatewayidentity"
	"github.com/srcfl/ftw/go/internal/state"
)

const (
	ReadContractVersion = 1
	MaxHistoryLimit     = 5000
	MaxHistoryBuckets   = 2000
	MaxAssetIDBytes     = 512
	MaxHistoryWindowMS  = int64(state.EnergyLedgerRetention / time.Millisecond)
)

type ReadTarget struct {
	Method string
	Path   string
}

// ReadDispatcher is Core's trusted one-shot read boundary. It is fixed when
// the grant manager is built, not supplied by a remote request. Implementations
// call the fixed local handler and receive no reusable authorization object.
type ReadDispatcher interface {
	DispatchRead(context.Context, ReadTarget, ReadRequest, Principal) error
}

var readTargets = map[Scope]ReadTarget{
	ScopeHealthRead:        {Method: http.MethodGet, Path: "/api/health"},
	ScopePlanRead:          {Method: http.MethodGet, Path: "/api/mpc/plan"},
	ScopeEnergyAssetsRead:  {Method: http.MethodGet, Path: "/api/energy/assets"},
	ScopeEnergyHistoryRead: {Method: http.MethodGet, Path: "/api/energy/history"},
}

// ReadRequest is an internal Core request. It has no relay JSON form in this
// slice; the existing Core endpoint owns each response body.
type ReadRequest struct {
	Version   int
	GatewayID string
	Scope     Scope
	History   *state.EnergyHistoryQuery
}

func (r ReadRequest) Validate() error {
	if r.Version != ReadContractVersion {
		return fmt.Errorf("unsupported read contract version %d", r.Version)
	}
	if _, err := gatewayidentity.NormalizeGatewayID(r.GatewayID); err != nil {
		return err
	}
	if _, ok := readTargets[r.Scope]; !ok {
		return fmt.Errorf("scope %q is not a remote read", r.Scope)
	}
	if r.Scope == ScopeEnergyHistoryRead {
		if r.History == nil {
			return errors.New("energy history scope needs a bounded query")
		}
		return ValidateHistoryQuery(*r.History)
	}
	if r.History != nil {
		return errors.New("history query is only valid for energy history")
	}
	return nil
}

// VerifyAndDispatchRead validates the request, consumes its exact one-use
// access grant, resolves one fixed Core GET, and calls Core once. Grant
// consumption is the dispatch start boundary. A later credential revoke does
// not cancel that in-flight Core call. The method returns no reusable
// authorization or target to the untrusted caller.
// Credential revocation waits for an already consumed grant's dispatch to
// return, then prevents every later dispatch for that credential.
func (m *GrantManager) VerifyAndDispatchRead(
	ctx context.Context,
	token string,
	request ReadRequest,
) error {
	if !m.enabled {
		return ErrRemoteDisabled
	}
	if m.readDispatcher == nil {
		return errors.New("Core read dispatcher is missing")
	}
	request = cloneReadRequest(request)
	if err := request.Validate(); err != nil {
		return err
	}
	record, release, err := m.consumeForDispatch(token, request.GatewayID, request.Scope)
	if err != nil {
		return err
	}
	defer release()
	if err := record.principal.validate(); err != nil {
		return err
	}
	return m.readDispatcher.DispatchRead(
		ctx, readTargets[request.Scope], request, clonePrincipal(record.principal),
	)
}

func cloneReadRequest(request ReadRequest) ReadRequest {
	if request.History != nil {
		history := *request.History
		request.History = &history
	}
	return request
}

func ValidateHistoryQuery(q state.EnergyHistoryQuery) error {
	if q.SinceMS < 0 || q.UntilMS <= q.SinceMS {
		return errors.New("history bounds are invalid")
	}
	if q.UntilMS-q.SinceMS > MaxHistoryWindowMS {
		return errors.New("history range exceeds ledger retention")
	}
	if q.BucketMS < state.EnergyLedgerBucketMS {
		return errors.New("history bucket is below ledger detail")
	}
	if q.BucketMS > MaxHistoryWindowMS {
		return errors.New("history bucket exceeds ledger retention")
	}
	window := q.UntilMS - q.SinceMS
	if (window-1)/q.BucketMS+1 > MaxHistoryBuckets {
		return errors.New("history query has too many buckets")
	}
	if q.Limit < 1 || q.Limit > MaxHistoryLimit {
		return fmt.Errorf("history limit must be from 1 through %d", MaxHistoryLimit)
	}
	if len(q.AssetID) > MaxAssetIDBytes || strings.TrimSpace(q.AssetID) != q.AssetID {
		return errors.New("asset id is invalid")
	}
	return nil
}

// These aliases keep the existing ledger v1 JSON fields as the remote shape.
type EnergyAsset = state.EnergyAsset
type EnergyLedgerPoint = state.EnergyLedgerPoint

const EnergyLedgerSchemaVersion = state.EnergyLedgerSchemaVersion
