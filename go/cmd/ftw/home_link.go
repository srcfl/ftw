package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
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
	ctrl *control.State,
	ctrlMu *sync.Mutex,
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
		Overview: func(context.Context) (homelink.OverviewReadResponse, error) {
			return homeLinkOverview(st, tel, ctrl, ctrlMu)
		},
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

func homeLinkOverview(
	st *state.Store,
	tel *telemetry.Store,
	ctrl *control.State,
	ctrlMu *sync.Mutex,
) (homelink.OverviewReadResponse, error) {
	now := time.Now()
	response := homelink.OverviewReadResponse{CheckedAtMS: now.UTC().UnixMilli()}
	if tel == nil || ctrl == nil || ctrlMu == nil {
		return response, errors.New("Home Link overview source is unavailable")
	}

	ctrlMu.Lock()
	siteMeterDriver := ctrl.SiteMeterDriver
	response.Mode = string(ctrl.Mode)
	response.PlanStale = ctrl.PlanStale
	ctrlMu.Unlock()

	if siteMeterDriver == "" {
		response.GridAvailable = true
	} else if health := tel.DriverHealth(siteMeterDriver); health != nil && health.IsOnline() {
		if reading := tel.Get(siteMeterDriver, telemetry.DerMeter); reading != nil {
			response.GridAvailable = true
			response.GridW = reading.SmoothedW
		}
	}
	for _, reading := range tel.ReadingsByType(telemetry.DerPV) {
		if health := tel.DriverHealth(reading.Driver); health != nil && health.IsOnline() {
			response.PVW += reading.SmoothedW
		}
	}
	var socTotal float64
	var socCount int
	for _, reading := range tel.ReadingsByType(telemetry.DerBattery) {
		if health := tel.DriverHealth(reading.Driver); health == nil || !health.IsOnline() {
			continue
		}
		response.BatW += reading.SmoothedW
		if reading.SoC != nil {
			socTotal += *reading.SoC
			socCount++
		}
	}
	if socCount > 0 {
		response.BatSoCAvailable = true
		response.BatSoC = socTotal / float64(socCount)
	}
	response.EVW = tel.SumOnlineEVW()
	response.V2XW = tel.SumOnlineV2XW()
	if response.GridAvailable {
		response.LoadW = response.GridW - response.BatW - response.PVW -
			response.EVW - response.V2XW
		if response.LoadW < 0 {
			response.LoadW = 0
		}
	}

	if st != nil {
		midnight := time.Date(
			now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location(),
		)
		totals, err := st.DailyEnergy(midnight.UnixMilli(), now.UnixMilli())
		if err != nil {
			return homelink.OverviewReadResponse{}, err
		}
		if totals.Intervals > 0 {
			response.EnergyToday = &homelink.OverviewEnergyToday{
				ImportWh: totals.ImportWh, ExportWh: totals.ExportWh,
				PVWh: totals.PVWh, BatChargedWh: totals.BatChargedWh,
				BatDischargedWh: totals.BatDischargedWh, LoadWh: totals.LoadWh,
			}
		}
	}
	return response, nil
}
