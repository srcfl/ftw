package ocppcp

import (
	"strings"

	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/provisioning"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/remotecontrol"
	smartcharging201 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/smartcharging"
	types201 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/types"
)

// handlers201 is the 2.0.1 station-side of Sim. Separate type because the
// method signatures do not match 1.6.
type handlers201 struct{ sim *Sim }

func (h *handlers201) OnGetBaseReport(*provisioning.GetBaseReportRequest) (*provisioning.GetBaseReportResponse, error) {
	return provisioning.NewGetBaseReportResponse(types201.GenericDeviceModelStatusRejected), nil
}

func (h *handlers201) OnGetReport(*provisioning.GetReportRequest) (*provisioning.GetReportResponse, error) {
	return provisioning.NewGetReportResponse(types201.GenericDeviceModelStatusRejected), nil
}

func (h *handlers201) OnGetVariables(req *provisioning.GetVariablesRequest) (*provisioning.GetVariablesResponse, error) {
	if req == nil {
		return provisioning.NewGetVariablesResponse(nil), nil
	}
	results := make([]provisioning.GetVariableResult, 0, len(req.GetVariableData))
	for _, d := range req.GetVariableData {
		r := provisioning.GetVariableResult{Component: d.Component, Variable: d.Variable}
		if strings.EqualFold(d.Component.Name, "SmartChargingCtrlr") && strings.EqualFold(d.Variable.Name, "Available") {
			r.AttributeStatus = provisioning.GetVariableStatusAccepted
			r.AttributeValue = "true"
		} else {
			r.AttributeStatus = provisioning.GetVariableStatusUnknownVariable
		}
		results = append(results, r)
	}
	return provisioning.NewGetVariablesResponse(results), nil
}

func (h *handlers201) OnReset(*provisioning.ResetRequest) (*provisioning.ResetResponse, error) {
	return provisioning.NewResetResponse(provisioning.ResetStatusAccepted), nil
}

func (h *handlers201) OnSetNetworkProfile(*provisioning.SetNetworkProfileRequest) (*provisioning.SetNetworkProfileResponse, error) {
	return provisioning.NewSetNetworkProfileResponse(provisioning.SetNetworkProfileStatusRejected), nil
}

func (h *handlers201) OnSetVariables(req *provisioning.SetVariablesRequest) (*provisioning.SetVariablesResponse, error) {
	results := make([]provisioning.SetVariableResult, 0, len(req.SetVariableData))
	for _, d := range req.SetVariableData {
		results = append(results, provisioning.SetVariableResult{
			AttributeStatus: provisioning.SetVariableStatusRejected,
			Component:       d.Component,
			Variable:        d.Variable,
		})
	}
	return provisioning.NewSetVariablesResponse(results), nil
}

func (h *handlers201) OnClearChargingProfile(*smartcharging201.ClearChargingProfileRequest) (*smartcharging201.ClearChargingProfileResponse, error) {
	return smartcharging201.NewClearChargingProfileResponse(smartcharging201.ClearChargingProfileStatusAccepted), nil
}

func (h *handlers201) OnGetChargingProfiles(*smartcharging201.GetChargingProfilesRequest) (*smartcharging201.GetChargingProfilesResponse, error) {
	return smartcharging201.NewGetChargingProfilesResponse(smartcharging201.GetChargingProfileStatusNoProfiles), nil
}

func (h *handlers201) OnGetCompositeSchedule(req *smartcharging201.GetCompositeScheduleRequest) (*smartcharging201.GetCompositeScheduleResponse, error) {
	evse := 0
	if req != nil {
		evse = req.EvseID
	}
	return smartcharging201.NewGetCompositeScheduleResponse(smartcharging201.GetCompositeScheduleStatusRejected, evse), nil
}

func (h *handlers201) OnSetChargingProfile(req *smartcharging201.SetChargingProfileRequest) (*smartcharging201.SetChargingProfileResponse, error) {
	d := decide201(h.sim.Model.Quirks, req)
	evse := 0
	kind := ""
	if req != nil {
		evse = req.EvseID
		if req.ChargingProfile != nil {
			kind = string(req.ChargingProfile.ChargingProfileKind)
		}
	}
	h.sim.recordAttempt(ProfileAttempt{
		EVSEID:  evse,
		Kind:    kind,
		LimitA:  d.limitA,
		Applied: d.applied,
		Status:  d.status,
	})
	return smartcharging201.NewSetChargingProfileResponse(smartcharging201.ChargingProfileStatus(d.status)), nil
}

func (h *handlers201) OnRequestStartTransaction(*remotecontrol.RequestStartTransactionRequest) (*remotecontrol.RequestStartTransactionResponse, error) {
	return remotecontrol.NewRequestStartTransactionResponse(remotecontrol.RequestStartStopStatusAccepted), nil
}

func (h *handlers201) OnRequestStopTransaction(*remotecontrol.RequestStopTransactionRequest) (*remotecontrol.RequestStopTransactionResponse, error) {
	if h.sim.Model.Quirks.IgnoreRemoteStop {
		return remotecontrol.NewRequestStopTransactionResponse(remotecontrol.RequestStartStopStatusAccepted), nil
	}
	go func() { _ = h.sim.stopTx() }()
	return remotecontrol.NewRequestStopTransactionResponse(remotecontrol.RequestStartStopStatusAccepted), nil
}

func (h *handlers201) OnTriggerMessage(req *remotecontrol.TriggerMessageRequest) (*remotecontrol.TriggerMessageResponse, error) {
	if req != nil && req.RequestedMessage == remotecontrol.MessageTriggerBootNotification {
		go func() { _ = h.sim.Boot() }()
		return remotecontrol.NewTriggerMessageResponse(remotecontrol.TriggerMessageStatusAccepted), nil
	}
	return remotecontrol.NewTriggerMessageResponse(remotecontrol.TriggerMessageStatusNotImplemented), nil
}

func (h *handlers201) OnUnlockConnector(*remotecontrol.UnlockConnectorRequest) (*remotecontrol.UnlockConnectorResponse, error) {
	return remotecontrol.NewUnlockConnectorResponse(remotecontrol.UnlockStatusUnlocked), nil
}
