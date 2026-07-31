package ocpp

// OCPP 2.0.1 CSMS handlers.
//
// These translate 2.0.1 messages into the same charger state and DerEV
// telemetry the 1.6 handlers produce, so everything downstream — dispatch, the
// loadpoint controller, control commands — is unaware of which dialect a
// charger speaks.
//
// The shapes differ more than the names suggest:
//
//   - Start/StopTransaction collapse into a single TransactionEvent with a
//     Started / Updated / Ended trigger, and the transaction id is a string
//     rather than an int.
//   - StatusNotification reports per-EVSE connector status with a different
//     enum, and no longer carries the "charging" meaning — that now comes from
//     the transaction event.
//   - Meter samples arrive inside TransactionEvent as well as in MeterValues.
//
// Everything not needed to meter and steer a charger is acknowledged and
// dropped. Accepting a message we ignore is correct here: refusing it would
// make the charger retry forever.

import (
	"log/slog"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/authorization"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/availability"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/meter"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/provisioning"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/transactions"
	types201 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/types"
)

// handlerV201 adapts a Handler to the 2.0.1 CSMS interfaces. It owns no state
// of its own — everything lands in the shared Handler.
type handlerV201 struct {
	*Handler
}

// ---- provisioning.CSMSHandler ----

func (h *handlerV201) OnBootNotification(id string, req *provisioning.BootNotificationRequest) (*provisioning.BootNotificationResponse, error) {
	vendor, model, serial := "", "", ""
	if req != nil {
		vendor = req.ChargingStation.VendorName
		model = req.ChargingStation.Model
		serial = req.ChargingStation.SerialNumber
	}
	slog.Info("OCPP boot",
		"charger", id, "version", Version201,
		"vendor", vendor, "model", model, "serial", serial)

	h.setVersion(id, Version201)
	h.tel.RecordDriverSuccess(id)

	return provisioning.NewBootNotificationResponse(
		types201.NewDateTime(time.Now()),
		h.heartbeatIntervalS,
		provisioning.RegistrationStatusAccepted,
	), nil
}

// OnNotifyReport carries variable inventory. FTW does not model charger
// configuration, so this is acknowledged and dropped.
func (h *handlerV201) OnNotifyReport(id string, _ *provisioning.NotifyReportRequest) (*provisioning.NotifyReportResponse, error) {
	h.tel.RecordDriverSuccess(id)
	return provisioning.NewNotifyReportResponse(), nil
}

// ---- availability.CSMSHandler ----

func (h *handlerV201) OnHeartbeat(id string, _ *availability.HeartbeatRequest) (*availability.HeartbeatResponse, error) {
	h.tel.RecordDriverSuccess(id)
	return availability.NewHeartbeatResponse(*types201.NewDateTime(time.Now())), nil
}

// OnStatusNotification maps 2.0.1 connector status onto the same
// connected/charging pair the 1.6 path produces.
//
// Unlike 1.6 there is no Charging status here — occupancy is all this tells us,
// and whether energy is actually flowing comes from the transaction event.
func (h *handlerV201) OnStatusNotification(id string, req *availability.StatusNotificationRequest) (*availability.StatusNotificationResponse, error) {
	s := h.state(id)
	h.mu.Lock()
	switch req.ConnectorStatus {
	case availability.ConnectorStatusAvailable, availability.ConnectorStatusUnavailable:
		s.connected = false
		s.charging = false
		s.lastPowerW = 0
	case availability.ConnectorStatusOccupied, availability.ConnectorStatusReserved:
		s.connected = true
	case availability.ConnectorStatusFaulted:
		// Matches the 1.6 path: a faulted connector still has a cable in it,
		// so it stays connected while charging stops.
		s.connected = true
		s.charging = false
		s.lastPowerW = 0
	}
	faulted := req.ConnectorStatus == availability.ConnectorStatusFaulted
	h.mu.Unlock()

	slog.Info("OCPP status",
		"charger", id, "version", Version201,
		"evse", req.EvseID, "connector", req.ConnectorID, "status", req.ConnectorStatus)

	if faulted {
		h.tel.RecordDriverError(id, "ocpp: connector faulted")
	} else {
		h.tel.RecordDriverSuccess(id)
	}
	h.pushReading(id, s)
	return availability.NewStatusNotificationResponse(), nil
}

// ---- transactions.CSMSHandler ----

// OnTransactionEvent replaces 1.6's StartTransaction, StopTransaction and much
// of MeterValues. The trigger says which of those it stands in for.
func (h *handlerV201) OnTransactionEvent(id string, req *transactions.TransactionEventRequest) (*transactions.TransactionEventResponse, error) {
	s := h.state(id)

	// Meter samples ride along with every event type.
	powerW, energyWh, hasEnergy := sampledValuesV201(req.MeterValue)

	h.mu.Lock()
	switch req.EventType {
	case transactions.TransactionEventStarted:
		// 2.0.1 transaction ids are strings; the shared state keeps an int for
		// the 1.6 path, so record presence rather than the id itself and keep
		// the real one alongside.
		h.nextTxID++
		s.transactionID = h.nextTxID
		s.transactionRef = req.TransactionInfo.TransactionID
		s.sessionStartMeterWh = energyWh
		s.sessionMeterWh = 0
		s.connected = true
		s.charging = true

	case transactions.TransactionEventUpdated:
		s.connected = true
		if hasEnergy && s.transactionID >= 0 {
			s.sessionMeterWh = energyWh - s.sessionStartMeterWh
		}
		// A zero power sample during a live transaction is a genuine pause,
		// not a missing reading, so it is taken at face value.
		s.charging = powerW > 0

	case transactions.TransactionEventEnded:
		if hasEnergy {
			s.sessionMeterWh = energyWh - s.sessionStartMeterWh
		}
		s.transactionID = -1
		s.transactionRef = ""
		s.charging = false
		s.lastPowerW = 0
		powerW = 0
	}

	if req.EventType != transactions.TransactionEventEnded {
		s.lastPowerW = powerW
	}
	sessionWh := s.sessionMeterWh
	ended := req.EventType == transactions.TransactionEventEnded
	h.mu.Unlock()

	slog.Info("OCPP transaction event",
		"charger", id, "version", Version201,
		"event", req.EventType, "seq", req.SequenceNo, "w", powerW)

	h.pushReading(id, s)
	if ended {
		h.tel.EmitMetric(id, "ev_session_wh", sessionWh, "Wh", "", "")
	}
	h.tel.RecordDriverSuccess(id)

	return transactions.NewTransactionEventResponse(), nil
}

// ---- meter.CSMSHandler ----

func (h *handlerV201) OnMeterValues(id string, req *meter.MeterValuesRequest) (*meter.MeterValuesResponse, error) {
	s := h.state(id)
	powerW, energyWh, hasEnergy := sampledValuesV201(req.MeterValue)

	h.mu.Lock()
	s.lastPowerW = powerW
	if hasEnergy && s.transactionID >= 0 {
		s.sessionMeterWh = energyWh - s.sessionStartMeterWh
	}
	h.mu.Unlock()

	h.pushReading(id, s)
	h.tel.RecordDriverSuccess(id)
	return meter.NewMeterValuesResponse(), nil
}

// ---- authorization.CSMSHandler ----

// OnAuthorize accepts every token. FTW is a home energy manager, not an access
// control system: the charger is behind the operator's own front door, and
// refusing here would only stop them charging. Matches the 1.6 path.
func (h *handlerV201) OnAuthorize(id string, _ *authorization.AuthorizeRequest) (*authorization.AuthorizeResponse, error) {
	h.tel.RecordDriverSuccess(id)
	return authorization.NewAuthorizationResponse(types201.IdTokenInfo{
		Status: types201.AuthorizationStatusAccepted,
	}), nil
}

// sampledValuesV201 pulls active-import power and energy out of a 2.0.1 meter
// value set, normalising kW/kWh to W/Wh.
//
// 2.0.1 always states the measurand, so unlike 1.6 there is no default to
// assume. hasEnergy distinguishes "no energy sample in this batch" from a
// genuine zero reading, which matters because session energy is a difference
// against the transaction's starting register.
func sampledValuesV201(values []types201.MeterValue) (powerW, energyWh float64, hasEnergy bool) {
	for _, mv := range values {
		for _, sv := range mv.SampledValue {
			val := sv.Value
			switch sv.Measurand {
			case types201.MeasurandPowerActiveImport:
				if unitIsKilo(sv.UnitOfMeasure) {
					val *= 1000
				}
				powerW = val
			case types201.MeasurandEnergyActiveImportRegister:
				if unitIsKilo(sv.UnitOfMeasure) {
					val *= 1000
				}
				energyWh = val
				hasEnergy = true
			}
		}
	}
	return powerW, energyWh, hasEnergy
}

// unitIsKilo reports whether a sample is expressed in kW or kWh. An absent unit
// means the 2.0.1 default, which is already W/Wh.
func unitIsKilo(u *types201.UnitOfMeasure) bool {
	if u == nil {
		return false
	}
	switch u.Unit {
	case "kW", "kWh":
		return true
	default:
		return false
	}
}

