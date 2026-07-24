package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/homelink"
	"github.com/srcfl/ftw/go/internal/homelinkuplink"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

type homeLinkRemoteAccess struct {
	*homelink.GrantManager
	admin *homelink.LocalAdmin
}

func (a homeLinkRemoteAccess) BeginRegistration(
	ctx context.Context,
	pairingID string,
	pairingSecret string,
	label string,
) (homelink.RegistrationChallenge, error) {
	return a.admin.BeginRegistration(ctx, pairingID, pairingSecret, label)
}

func (a homeLinkRemoteAccess) FinishRegistration(
	ctx context.Context,
	expectationID string,
	response []byte,
) (homelink.CredentialSummary, error) {
	return a.admin.FinishRegistration(ctx, expectationID, response)
}

func startHomeLink(
	ctx context.Context,
	cfg *config.Config,
	identityState siteIdentityLoad,
	st *state.Store,
	tel *telemetry.Store,
	planner *mpc.Service,
) (*homelink.LocalAdmin, bool, error) {
	enabled := cfg != nil && cfg.HomeLink != nil && cfg.HomeLink.Enabled
	if identityState.HomeLink == nil {
		return nil, enabled, nil
	}

	pairing := homelink.NewLocalPairingManager(homelink.LocalPairingManagerOptions{})
	authority, err := homelink.NewPersistentCredentialAuthority(
		homelink.PersistentCredentialAuthorityOptions{
			Store: st, SiteID: identityState.HomeLink.GatewayID(),
			PairingAuthorizer: pairing,
		},
	)
	if err != nil {
		return nil, enabled, err
	}
	reads, err := homelink.NewCoreReadAdapter(homelink.CoreReadSources{
		Health: func(context.Context) (homelink.HealthReadResponse, error) {
			status := "ok"
			for _, health := range tel.AllHealth() {
				if health.Status == telemetry.StatusOffline || health.DeviceFault {
					status = "degraded"
					break
				}
			}
			return homelink.HealthReadResponse{
				Status: status, CheckedAtMS: time.Now().UTC().UnixMilli(),
			}, nil
		},
		Plan: func(context.Context) (homelink.PlanReadResponse, error) {
			if planner == nil {
				return homelink.PlanReadResponse{}, nil
			}
			plan := planner.Latest()
			if plan == nil {
				return homelink.PlanReadResponse{}, nil
			}
			return homelink.PlanReadResponse{
				Available: true, GeneratedAtMS: plan.GeneratedAtMs,
				Mode: string(plan.Mode), HorizonSlots: plan.HorizonSlots,
				TotalCostOre: plan.TotalCostOre,
			}, nil
		},
		Assets: func(context.Context) ([]state.EnergyAsset, error) {
			return st.EnergyAssets()
		},
		History: func(
			_ context.Context,
			query state.EnergyHistoryQuery,
		) ([]state.EnergyLedgerPoint, bool, error) {
			return st.LoadEnergyHistory(query)
		},
	})
	if err != nil {
		return nil, enabled, err
	}
	grants, err := homelink.NewGrantManager(
		identityState.HomeLink.GatewayID(),
		homelink.GrantManagerOptions{
			Enabled: enabled, CredentialAuthority: authority,
			ReadDispatcher: reads, PairingAuthorizer: pairing,
		},
	)
	if err != nil {
		return nil, enabled, err
	}
	runtime := &homelink.RemoteRuntimeStatus{}
	admin, err := homelink.NewLocalAdmin(
		enabled, identityState.HomeLink, st, pairing, authority, runtime,
	)
	if err != nil {
		return nil, enabled, err
	}
	if !enabled {
		return admin, false, nil
	}
	service, err := homelinkuplink.NewServiceWithRemoteAccess(
		identityState.HomeLink, homeLinkRemoteAccess{GrantManager: grants, admin: admin},
	)
	if err != nil {
		return nil, enabled, err
	}
	client, err := homelinkuplink.New(identityState.HomeLink)
	if err != nil {
		return nil, enabled, err
	}
	go func() {
		err := client.RunWithStatus(ctx, service, runtime.SetConnected)
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("Home Link uplink stopped")
		}
	}()
	return admin, true, nil
}
