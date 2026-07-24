package homelink

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/srcfl/ftw/go/internal/gatewayidentity"
	"github.com/srcfl/ftw/go/internal/homelink/wire"
	"github.com/srcfl/ftw/go/internal/state"
)

const (
	ReadContractVersion = 1
	MaxHistoryLimit     = 5000
	MaxHistoryBuckets   = 2000
	MaxAssetIDBytes     = 512
	MaxHistoryWindowMS  = int64(state.EnergyLedgerRetention / time.Millisecond)
	MaxRemoteAssets     = 256
	MaxRemotePoints     = 512
	MaxReadStringBytes  = 512
)

const readHashDomain = "ftw-home-link-read-request/v1"

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

// ReadResultDispatcher is the typed remote-read boundary. Implementations
// return one small response and must not expose a generic HTTP body.
type ReadResultDispatcher interface {
	ReadDispatcher
	DispatchReadResult(context.Context, ReadTarget, ReadRequest, Principal) (ReadResponse, error)
}

var readTargets = map[Scope]ReadTarget{
	ScopeHealthRead:        {Method: http.MethodGet, Path: "/api/health"},
	ScopePlanRead:          {Method: http.MethodGet, Path: "/api/mpc/plan"},
	ScopeEnergyAssetsRead:  {Method: http.MethodGet, Path: "/api/energy/assets"},
	ScopeEnergyHistoryRead: {Method: http.MethodGet, Path: "/api/energy/history"},
}

// ReadRequest is an internal Core request. The uplink maps its closed
// encrypted envelope into this type; no caller supplies an HTTP target.
type ReadRequest struct {
	Version   int
	GatewayID string
	Scope     Scope
	History   *state.EnergyHistoryQuery
}

// ReadBinding binds one access grant to one encrypted browser session and one
// exact request. None of these values comes from the relay.
type ReadBinding struct {
	GatewayID       string
	RouteHandle     string
	RouteGeneration uint64
	SessionID       string
	StreamID        string
	RequestHash     [sha256.Size]byte
}

func (b ReadBinding) Validate() error {
	normalized, err := gatewayidentity.NormalizeGatewayID(b.GatewayID)
	if err != nil || normalized != b.GatewayID {
		return errors.New("read binding gateway is invalid")
	}
	if err := validateRawURLValue(b.RouteHandle, gatewayidentity.RouteHandleBytes); err != nil {
		return errors.New("read binding route is invalid")
	}
	if b.RouteGeneration == 0 {
		return errors.New("read binding generation is invalid")
	}
	if err := validateRawURLValue(b.SessionID, wire.SessionIDBytes); err != nil {
		return errors.New("read binding session is invalid")
	}
	if err := validateRawURLValue(b.StreamID, wire.StreamIDBytes); err != nil {
		return errors.New("read binding stream is invalid")
	}
	if b.RequestHash == ([sha256.Size]byte{}) {
		return errors.New("read binding request hash is invalid")
	}
	return nil
}

func validateRawURLValue(value string, size int) error {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) != size ||
		base64.RawURLEncoding.EncodeToString(raw) != value {
		return errors.New("invalid raw URL value")
	}
	return nil
}

// ReadRequestHash returns the stable semantic hash used by a bound grant.
func ReadRequestHash(requestID string, request ReadRequest) ([sha256.Size]byte, error) {
	if err := validateRawURLValue(requestID, 16); err != nil {
		return [sha256.Size]byte{}, errors.New("read request id is invalid")
	}
	request = cloneReadRequest(request)
	if err := request.Validate(); err != nil {
		return [sha256.Size]byte{}, err
	}
	var payload bytes.Buffer
	payload.WriteString(readHashDomain)
	writeHashString(&payload, requestID)
	writeHashString(&payload, request.GatewayID)
	writeHashString(&payload, string(request.Scope))
	if request.History == nil {
		payload.WriteByte(0)
	} else {
		payload.WriteByte(1)
		writeHashString(&payload, request.History.AssetID)
		_ = binary.Write(&payload, binary.BigEndian, request.History.SinceMS)
		_ = binary.Write(&payload, binary.BigEndian, request.History.UntilMS)
		_ = binary.Write(&payload, binary.BigEndian, request.History.BucketMS)
		_ = binary.Write(&payload, binary.BigEndian, int64(request.History.Limit))
	}
	return sha256.Sum256(payload.Bytes()), nil
}

func writeHashString(dst *bytes.Buffer, value string) {
	_ = binary.Write(dst, binary.BigEndian, uint32(len(value)))
	dst.WriteString(value)
}

type HealthReadResponse struct {
	Status      string `json:"status"`
	CheckedAtMS int64  `json:"checked_at_ms"`
}

type PlanReadResponse struct {
	Available     bool    `json:"available"`
	GeneratedAtMS int64   `json:"generated_at_ms,omitempty"`
	Mode          string  `json:"mode,omitempty"`
	HorizonSlots  int     `json:"horizon_slots,omitempty"`
	TotalCostOre  float64 `json:"total_cost_ore,omitempty"`
}

type EnergyAssetsReadResponse struct {
	Assets []state.EnergyAsset `json:"assets"`
}

type EnergyHistoryReadResponse struct {
	Points    []state.EnergyLedgerPoint `json:"points"`
	Truncated bool                      `json:"truncated"`
}

// ReadResponse is a closed union. Exactly one payload must match Scope.
type ReadResponse struct {
	Version       int                        `json:"version"`
	Scope         Scope                      `json:"scope"`
	Health        *HealthReadResponse        `json:"health,omitempty"`
	Plan          *PlanReadResponse          `json:"plan,omitempty"`
	EnergyAssets  *EnergyAssetsReadResponse  `json:"energy_assets,omitempty"`
	EnergyHistory *EnergyHistoryReadResponse `json:"energy_history,omitempty"`
}

func (r ReadResponse) Validate() error {
	if r.Version != ReadContractVersion {
		return errors.New("read response version is invalid")
	}
	count := 0
	for _, present := range []bool{r.Health != nil, r.Plan != nil, r.EnergyAssets != nil, r.EnergyHistory != nil} {
		if present {
			count++
		}
	}
	if count != 1 {
		return errors.New("read response payload is invalid")
	}
	switch r.Scope {
	case ScopeHealthRead:
		if r.Health == nil || !validHealthStatus(r.Health.Status) ||
			r.Health.CheckedAtMS < 0 {
			return errors.New("health response is invalid")
		}
	case ScopePlanRead:
		if r.Plan == nil || len(r.Plan.Mode) > 64 || r.Plan.HorizonSlots < 0 ||
			r.Plan.HorizonSlots > 4096 || !finite(r.Plan.TotalCostOre) ||
			!safeReadString(r.Plan.Mode, 64, !r.Plan.Available) {
			return errors.New("plan response is invalid")
		}
		if r.Plan.Available && (r.Plan.GeneratedAtMS <= 0 || r.Plan.HorizonSlots == 0) {
			return errors.New("available plan response is incomplete")
		}
	case ScopeEnergyAssetsRead:
		if r.EnergyAssets == nil || len(r.EnergyAssets.Assets) > MaxRemoteAssets {
			return errors.New("energy assets response is invalid")
		}
		for _, asset := range r.EnergyAssets.Assets {
			if !safeReadString(asset.AssetID, MaxAssetIDBytes, false) ||
				!safeReadString(asset.DeviceID, MaxReadStringBytes, true) ||
				!safeReadString(asset.Label, MaxReadStringBytes, true) {
				return errors.New("energy asset is invalid")
			}
			switch asset.Kind {
			case state.AssetGridMeter, state.AssetBattery, state.AssetPV,
				state.AssetObservedConsumer, state.AssetVehicleCharger:
			default:
				return errors.New("energy asset kind is invalid")
			}
			if asset.FirstSeenMS < 0 || asset.LastSeenMS < asset.FirstSeenMS {
				return errors.New("energy asset time is invalid")
			}
		}
	case ScopeEnergyHistoryRead:
		if r.EnergyHistory == nil || len(r.EnergyHistory.Points) > MaxRemotePoints {
			return errors.New("energy history response is invalid")
		}
		for _, point := range r.EnergyHistory.Points {
			if !safeReadString(point.AssetID, MaxAssetIDBytes, false) ||
				!safeReadString(point.Source, 64, false) ||
				!safeReadString(point.Quality, 64, false) ||
				!safeReadString(point.Provenance, MaxReadStringBytes, false) ||
				!finite(point.EnergyWh) || point.EnergyWh < 0 ||
				point.SchemaVersion != EnergyLedgerSchemaVersion ||
				point.BucketStartMS < 0 || point.BucketLenMS < state.EnergyLedgerBucketMS ||
				point.SampleCount < 0 || !validEnergyFlow(point.Flow) {
				return errors.New("energy history point is invalid")
			}
		}
	default:
		return errors.New("read response scope is invalid")
	}
	return nil
}

func safeReadString(value string, maxBytes int, allowEmpty bool) bool {
	if (!allowEmpty && value == "") || len(value) > maxBytes || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) ||
			unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return false
		}
	}
	return true
}

func validHealthStatus(status string) bool {
	switch status {
	case "ok", "degraded", "unavailable":
		return true
	}
	return false
}

func validEnergyFlow(flow state.EnergyFlow) bool {
	switch flow {
	case state.FlowGridImport, state.FlowGridExport,
		state.FlowBatteryCharge, state.FlowBatteryDischarge,
		state.FlowPVGeneration, state.FlowConsumerUse,
		state.FlowVehicleCharge, state.FlowVehicleDischarge:
		return true
	}
	return false
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
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
// consumption is the dispatch start boundary. The method returns no reusable
// authorization or target to the untrusted caller. Credential revocation
// blocks later dispatches, waits only for a bounded time for an in-flight call,
// and cancels the dispatch context if that wait expires. A revoke called from
// inside the dispatch is recognized and cancels that call without waiting on
// itself.
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
	record, dispatchCtx, dispatch, err := m.consumeForDispatch(
		ctx, token, request.GatewayID, request.Scope,
	)
	if err != nil {
		return err
	}
	defer m.coordinator.finishDispatch(dispatch)
	if err := record.principal.validate(); err != nil {
		return err
	}
	err = m.readDispatcher.DispatchRead(
		dispatchCtx, readTargets[request.Scope], request, clonePrincipal(record.principal),
	)
	if err == nil && dispatchCtx.Err() != nil {
		return dispatchCtx.Err()
	}
	return err
}

// VerifyAndDispatchBoundRead validates one session binding, consumes the grant
// before the local read starts, and returns one typed response. A failed,
// canceled or oversized read never restores the grant.
func (m *GrantManager) VerifyAndDispatchBoundRead(
	ctx context.Context,
	token string,
	requestID string,
	request ReadRequest,
	binding ReadBinding,
) (ReadResponse, error) {
	if !m.enabled {
		return ReadResponse{}, ErrRemoteDisabled
	}
	dispatcher, ok := m.readDispatcher.(ReadResultDispatcher)
	if !ok {
		return ReadResponse{}, errors.New("typed Core read dispatcher is missing")
	}
	request = cloneReadRequest(request)
	if err := request.Validate(); err != nil {
		return ReadResponse{}, err
	}
	hash, err := ReadRequestHash(requestID, request)
	if err != nil {
		return ReadResponse{}, err
	}
	if err := binding.Validate(); err != nil || binding.GatewayID != request.GatewayID ||
		!bytes.Equal(binding.RequestHash[:], hash[:]) {
		return ReadResponse{}, ErrWrongBinding
	}
	record, dispatchCtx, dispatch, err := m.consumeForBoundDispatch(
		ctx, token, request.GatewayID, request.Scope, binding,
	)
	if err != nil {
		return ReadResponse{}, err
	}
	defer m.coordinator.finishDispatch(dispatch)
	if err := record.principal.validate(); err != nil {
		return ReadResponse{}, err
	}
	response, err := dispatcher.DispatchReadResult(
		dispatchCtx, readTargets[request.Scope], request, clonePrincipal(record.principal),
	)
	if err != nil {
		return ReadResponse{}, err
	}
	if dispatchCtx.Err() != nil {
		return ReadResponse{}, dispatchCtx.Err()
	}
	if response.Scope != request.Scope {
		return ReadResponse{}, errors.New("Core read response scope is invalid")
	}
	if err := response.Validate(); err != nil {
		return ReadResponse{}, err
	}
	return response, nil
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
