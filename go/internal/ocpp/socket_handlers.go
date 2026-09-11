package ocpp

import (
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/authorization"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/availability"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/meter"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/provisioning"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/smartcharging"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/transactions"
)

type boundHandler16 struct {
	inner    core.CentralSystemHandler
	sessions *socketSessions
}

func (h *boundHandler16) OnBootNotification(alias string, req *core.BootNotificationRequest) (*core.BootNotificationConfirmation, error) {
	return boundCall(h.sessions, alias, func(id string) (*core.BootNotificationConfirmation, error) {
		return h.inner.OnBootNotification(id, req)
	})
}
func (h *boundHandler16) OnHeartbeat(alias string, req *core.HeartbeatRequest) (*core.HeartbeatConfirmation, error) {
	return boundCall(h.sessions, alias, func(id string) (*core.HeartbeatConfirmation, error) { return h.inner.OnHeartbeat(id, req) })
}
func (h *boundHandler16) OnAuthorize(alias string, req *core.AuthorizeRequest) (*core.AuthorizeConfirmation, error) {
	return boundCall(h.sessions, alias, func(id string) (*core.AuthorizeConfirmation, error) { return h.inner.OnAuthorize(id, req) })
}
func (h *boundHandler16) OnDataTransfer(alias string, req *core.DataTransferRequest) (*core.DataTransferConfirmation, error) {
	return boundCall(h.sessions, alias, func(id string) (*core.DataTransferConfirmation, error) { return h.inner.OnDataTransfer(id, req) })
}
func (h *boundHandler16) OnStatusNotification(alias string, req *core.StatusNotificationRequest) (*core.StatusNotificationConfirmation, error) {
	return boundCall(h.sessions, alias, func(id string) (*core.StatusNotificationConfirmation, error) {
		return h.inner.OnStatusNotification(id, req)
	})
}
func (h *boundHandler16) OnMeterValues(alias string, req *core.MeterValuesRequest) (*core.MeterValuesConfirmation, error) {
	return boundCall(h.sessions, alias, func(id string) (*core.MeterValuesConfirmation, error) { return h.inner.OnMeterValues(id, req) })
}
func (h *boundHandler16) OnStartTransaction(alias string, req *core.StartTransactionRequest) (*core.StartTransactionConfirmation, error) {
	return boundCall(h.sessions, alias, func(id string) (*core.StartTransactionConfirmation, error) {
		return h.inner.OnStartTransaction(id, req)
	})
}
func (h *boundHandler16) OnStopTransaction(alias string, req *core.StopTransactionRequest) (*core.StopTransactionConfirmation, error) {
	return boundCall(h.sessions, alias, func(id string) (*core.StopTransactionConfirmation, error) { return h.inner.OnStopTransaction(id, req) })
}

type boundHandler201 struct {
	inner    *handlerV201
	sessions *socketSessions
}

func (h *boundHandler201) OnBootNotification(alias string, req *provisioning.BootNotificationRequest) (*provisioning.BootNotificationResponse, error) {
	return boundCall(h.sessions, alias, func(id string) (*provisioning.BootNotificationResponse, error) {
		return h.inner.OnBootNotification(id, req)
	})
}
func (h *boundHandler201) OnNotifyReport(alias string, req *provisioning.NotifyReportRequest) (*provisioning.NotifyReportResponse, error) {
	return boundCall(h.sessions, alias, func(id string) (*provisioning.NotifyReportResponse, error) { return h.inner.OnNotifyReport(id, req) })
}
func (h *boundHandler201) OnHeartbeat(alias string, req *availability.HeartbeatRequest) (*availability.HeartbeatResponse, error) {
	return boundCall(h.sessions, alias, func(id string) (*availability.HeartbeatResponse, error) { return h.inner.OnHeartbeat(id, req) })
}
func (h *boundHandler201) OnStatusNotification(alias string, req *availability.StatusNotificationRequest) (*availability.StatusNotificationResponse, error) {
	return boundCall(h.sessions, alias, func(id string) (*availability.StatusNotificationResponse, error) {
		return h.inner.OnStatusNotification(id, req)
	})
}
func (h *boundHandler201) OnTransactionEvent(alias string, req *transactions.TransactionEventRequest) (*transactions.TransactionEventResponse, error) {
	return boundCall(h.sessions, alias, func(id string) (*transactions.TransactionEventResponse, error) {
		return h.inner.OnTransactionEvent(id, req)
	})
}
func (h *boundHandler201) OnMeterValues(alias string, req *meter.MeterValuesRequest) (*meter.MeterValuesResponse, error) {
	return boundCall(h.sessions, alias, func(id string) (*meter.MeterValuesResponse, error) { return h.inner.OnMeterValues(id, req) })
}
func (h *boundHandler201) OnAuthorize(alias string, req *authorization.AuthorizeRequest) (*authorization.AuthorizeResponse, error) {
	return boundCall(h.sessions, alias, func(id string) (*authorization.AuthorizeResponse, error) { return h.inner.OnAuthorize(id, req) })
}
func (h *boundHandler201) OnNotifyEVChargingNeeds(alias string, req *smartcharging.NotifyEVChargingNeedsRequest) (*smartcharging.NotifyEVChargingNeedsResponse, error) {
	return boundCall(h.sessions, alias, func(id string) (*smartcharging.NotifyEVChargingNeedsResponse, error) {
		return h.inner.OnNotifyEVChargingNeeds(id, req)
	})
}
func (h *boundHandler201) OnNotifyEVChargingSchedule(alias string, req *smartcharging.NotifyEVChargingScheduleRequest) (*smartcharging.NotifyEVChargingScheduleResponse, error) {
	return boundCall(h.sessions, alias, func(id string) (*smartcharging.NotifyEVChargingScheduleResponse, error) {
		return h.inner.OnNotifyEVChargingSchedule(id, req)
	})
}
func (h *boundHandler201) OnNotifyChargingLimit(alias string, req *smartcharging.NotifyChargingLimitRequest) (*smartcharging.NotifyChargingLimitResponse, error) {
	return boundCall(h.sessions, alias, func(id string) (*smartcharging.NotifyChargingLimitResponse, error) {
		return h.inner.OnNotifyChargingLimit(id, req)
	})
}
func (h *boundHandler201) OnClearedChargingLimit(alias string, req *smartcharging.ClearedChargingLimitRequest) (*smartcharging.ClearedChargingLimitResponse, error) {
	return boundCall(h.sessions, alias, func(id string) (*smartcharging.ClearedChargingLimitResponse, error) {
		return h.inner.OnClearedChargingLimit(id, req)
	})
}
func (h *boundHandler201) OnReportChargingProfiles(alias string, req *smartcharging.ReportChargingProfilesRequest) (*smartcharging.ReportChargingProfilesResponse, error) {
	return boundCall(h.sessions, alias, func(id string) (*smartcharging.ReportChargingProfilesResponse, error) {
		return h.inner.OnReportChargingProfiles(id, req)
	})
}
