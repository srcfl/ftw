package homelink

import (
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

var readTargets = map[Scope]ReadTarget{
	ScopeStatusRead:        {Method: http.MethodGet, Path: "/api/status"},
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

// AuthorizedTarget resolves one fixed Core GET only when the consumed access
// grant matches the request's gateway and exact read scope.
func (r ReadRequest) AuthorizedTarget(authorization Authorization) (ReadTarget, error) {
	if err := r.Validate(); err != nil {
		return ReadTarget{}, err
	}
	if err := authorization.valid(); err != nil {
		return ReadTarget{}, err
	}
	gatewayID, _ := gatewayidentity.NormalizeGatewayID(r.GatewayID)
	if authorization.gatewayID != gatewayID {
		return ReadTarget{}, ErrWrongSite
	}
	if authorization.scope != r.Scope {
		return ReadTarget{}, ErrWrongScope
	}
	return readTargets[r.Scope], nil
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
