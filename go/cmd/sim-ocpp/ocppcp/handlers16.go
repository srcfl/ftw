package ocppcp

import (
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/remotetrigger"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/smartcharging"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
)

const featureProfiles = "Core,SmartCharging,RemoteTrigger"

func (s *Sim) OnChangeAvailability(*core.ChangeAvailabilityRequest) (*core.ChangeAvailabilityConfirmation, error) {
	return core.NewChangeAvailabilityConfirmation(core.AvailabilityStatusAccepted), nil
}

func (s *Sim) OnChangeConfiguration(*core.ChangeConfigurationRequest) (*core.ChangeConfigurationConfirmation, error) {
	return core.NewChangeConfigurationConfirmation(core.ConfigurationStatusAccepted), nil
}

func (s *Sim) OnClearCache(*core.ClearCacheRequest) (*core.ClearCacheConfirmation, error) {
	return core.NewClearCacheConfirmation(core.ClearCacheStatusAccepted), nil
}

func (s *Sim) OnDataTransfer(*core.DataTransferRequest) (*core.DataTransferConfirmation, error) {
	return core.NewDataTransferConfirmation(core.DataTransferStatusAccepted), nil
}

func (s *Sim) OnGetConfiguration(req *core.GetConfigurationRequest) (*core.GetConfigurationConfirmation, error) {
	v := featureProfiles
	key := core.ConfigurationKey{Key: "SupportedFeatureProfiles", Readonly: true, Value: &v}
	if req == nil || len(req.Key) == 0 {
		return core.NewGetConfigurationConfirmation([]core.ConfigurationKey{key}), nil
	}
	var found []core.ConfigurationKey
	var unknown []string
	for _, k := range req.Key {
		if k == "SupportedFeatureProfiles" {
			found = append(found, key)
		} else {
			unknown = append(unknown, k)
		}
	}
	conf := core.NewGetConfigurationConfirmation(found)
	conf.UnknownKey = unknown
	return conf, nil
}

func (s *Sim) OnRemoteStartTransaction(*core.RemoteStartTransactionRequest) (*core.RemoteStartTransactionConfirmation, error) {
	return core.NewRemoteStartTransactionConfirmation(types.RemoteStartStopStatusAccepted), nil
}

func (s *Sim) OnRemoteStopTransaction(*core.RemoteStopTransactionRequest) (*core.RemoteStopTransactionConfirmation, error) {
	if s.Model.Quirks.IgnoreRemoteStop {
		// Charge Amps: ACK and keep the transaction open. FTW never sends
		// this (it pauses at 0 A), but the quirk has to be here so a test
		// that does send it sees the same lie the hardware tells.
		return core.NewRemoteStopTransactionConfirmation(types.RemoteStartStopStatusAccepted), nil
	}
	go func() { _ = s.stopTx() }()
	return core.NewRemoteStopTransactionConfirmation(types.RemoteStartStopStatusAccepted), nil
}

func (s *Sim) OnReset(*core.ResetRequest) (*core.ResetConfirmation, error) {
	return core.NewResetConfirmation(core.ResetStatusAccepted), nil
}

func (s *Sim) OnUnlockConnector(*core.UnlockConnectorRequest) (*core.UnlockConnectorConfirmation, error) {
	return core.NewUnlockConnectorConfirmation(core.UnlockStatusUnlocked), nil
}

func (s *Sim) OnSetChargingProfile(req *smartcharging.SetChargingProfileRequest) (*smartcharging.SetChargingProfileConfirmation, error) {
	d := decide16(s.Model.Quirks, req)
	connector := 0
	kind := ""
	if req != nil {
		connector = req.ConnectorId
		if req.ChargingProfile != nil {
			kind = string(req.ChargingProfile.ChargingProfileKind)
		}
	}
	s.recordAttempt(ProfileAttempt{
		ConnectorID: connector,
		Kind:        kind,
		LimitA:      d.limitA,
		Applied:     d.applied,
		Status:      d.status,
	})
	return smartcharging.NewSetChargingProfileConfirmation(smartcharging.ChargingProfileStatus(d.status)), nil
}

func (s *Sim) OnClearChargingProfile(*smartcharging.ClearChargingProfileRequest) (*smartcharging.ClearChargingProfileConfirmation, error) {
	return smartcharging.NewClearChargingProfileConfirmation(smartcharging.ClearChargingProfileStatusAccepted), nil
}

func (s *Sim) OnGetCompositeSchedule(*smartcharging.GetCompositeScheduleRequest) (*smartcharging.GetCompositeScheduleConfirmation, error) {
	return smartcharging.NewGetCompositeScheduleConfirmation(smartcharging.GetCompositeScheduleStatusRejected), nil
}

func (s *Sim) OnTriggerMessage(req *remotetrigger.TriggerMessageRequest) (*remotetrigger.TriggerMessageConfirmation, error) {
	if req != nil && string(req.RequestedMessage) == core.BootNotificationFeatureName {
		go func() { _ = s.Boot() }()
		return remotetrigger.NewTriggerMessageConfirmation(remotetrigger.TriggerMessageStatusAccepted), nil
	}
	return remotetrigger.NewTriggerMessageConfirmation(remotetrigger.TriggerMessageStatusNotImplemented), nil
}
