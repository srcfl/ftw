package homelink

import (
	"context"
	"errors"

	"github.com/srcfl/ftw/go/internal/state"
)

// CoreReadSources are fixed local functions. They are supplied by Core during
// startup and never selected by a remote request.
type CoreReadSources struct {
	Overview func(context.Context) (OverviewReadResponse, error)
	Health   func(context.Context) (HealthReadResponse, error)
	Plan     func(context.Context) (PlanReadResponse, error)
	Assets   func(context.Context) ([]state.EnergyAsset, error)
	History  func(
		context.Context,
		state.EnergyHistoryQuery,
	) ([]state.EnergyLedgerPoint, bool, error)
}

// CoreReadAdapter implements the closed read targets without invoking
// Core's HTTP server.
type CoreReadAdapter struct {
	sources CoreReadSources
}

func NewCoreReadAdapter(sources CoreReadSources) (*CoreReadAdapter, error) {
	if sources.Overview == nil || sources.Health == nil || sources.Plan == nil ||
		sources.Assets == nil || sources.History == nil {
		return nil, errors.New("Home Link Core read source is missing")
	}
	return &CoreReadAdapter{sources: sources}, nil
}

func (a *CoreReadAdapter) DispatchRead(
	ctx context.Context,
	target ReadTarget,
	request ReadRequest,
	principal Principal,
) error {
	_, err := a.DispatchReadResult(ctx, target, request, principal)
	return err
}

func (a *CoreReadAdapter) DispatchReadResult(
	ctx context.Context,
	target ReadTarget,
	request ReadRequest,
	principal Principal,
) (ReadResponse, error) {
	if err := request.Validate(); err != nil {
		return ReadResponse{}, err
	}
	if err := principal.validate(); err != nil {
		return ReadResponse{}, err
	}
	expected, ok := readTargets[request.Scope]
	if !ok || target != expected {
		return ReadResponse{}, errors.New("Home Link Core read target is invalid")
	}
	if err := ctx.Err(); err != nil {
		return ReadResponse{}, err
	}

	response := ReadResponse{Version: ReadContractVersion, Scope: request.Scope}
	switch request.Scope {
	case ScopeOverviewRead:
		value, err := a.sources.Overview(ctx)
		if err != nil {
			return ReadResponse{}, err
		}
		response.Overview = &value
	case ScopeHealthRead:
		value, err := a.sources.Health(ctx)
		if err != nil {
			return ReadResponse{}, err
		}
		response.Health = &value
	case ScopePlanRead:
		value, err := a.sources.Plan(ctx)
		if err != nil {
			return ReadResponse{}, err
		}
		response.Plan = &value
	case ScopeEnergyAssetsRead:
		assets, err := a.sources.Assets(ctx)
		if err != nil {
			return ReadResponse{}, err
		}
		if len(assets) > MaxRemoteAssets {
			return ReadResponse{}, errors.New("Home Link energy asset response is too large")
		}
		response.EnergyAssets = &EnergyAssetsReadResponse{
			Assets: append([]state.EnergyAsset(nil), assets...),
		}
	case ScopeEnergyHistoryRead:
		if request.History == nil || request.History.Limit > MaxRemotePoints {
			return ReadResponse{}, errors.New("Home Link energy history response is too large")
		}
		points, truncated, err := a.sources.History(ctx, *request.History)
		if err != nil {
			return ReadResponse{}, err
		}
		// History.Limit bounds time buckets. One bucket may contain several
		// flow/source rows, so only the separate response-row cap applies here.
		if len(points) > MaxRemotePoints {
			return ReadResponse{}, errors.New("Home Link energy history source exceeded its bound")
		}
		response.EnergyHistory = &EnergyHistoryReadResponse{
			Points:    append([]state.EnergyLedgerPoint(nil), points...),
			Truncated: truncated,
		}
	default:
		return ReadResponse{}, errors.New("Home Link Core read scope is invalid")
	}
	if err := ctx.Err(); err != nil {
		return ReadResponse{}, err
	}
	if err := response.Validate(); err != nil {
		return ReadResponse{}, err
	}
	return response, nil
}
