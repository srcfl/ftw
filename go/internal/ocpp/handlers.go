package ocpp

import (
	"encoding/json"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"

	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// Handler implements ocpp1.6/core.CentralSystemHandler. One Handler is shared
// across every connected charger; per-charger state lives in the chargers
// map. All access is mutex-guarded — handler callbacks fire from the OCPP
// library's goroutines.
type Handler struct {
	tel                *telemetry.Store
	heartbeatIntervalS int

	mu       sync.Mutex
	chargers map[string]*chargerState
	nextTxID int
}

// chargerState is what we accumulate from successive OCPP messages for one
// charge point. Survives the OCPP library's stateless handler invocations.
type chargerState struct {
	connected           bool
	charging            bool
	transactionID       int
	sessionStartMeterWh float64
	sessionMeterWh      float64
	lastPowerW          float64
}

// NewHandler returns a Handler ready to register with a CentralSystem.
// heartbeatIntervalS is what we tell each charger to use in the
// BootNotification confirmation.
func NewHandler(tel *telemetry.Store, heartbeatIntervalS int) *Handler {
	return &Handler{
		tel:                tel,
		heartbeatIntervalS: heartbeatIntervalS,
		chargers:           map[string]*chargerState{},
		nextTxID:           1,
	}
}

// state returns the per-charger state, creating it lazily on first sight.
func (h *Handler) state(id string) *chargerState {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.chargers[id]
	if !ok {
		s = &chargerState{transactionID: -1}
		h.chargers[id] = s
	}
	return s
}

// Snapshot returns a copy of all charger states for /api/status etc.
func (h *Handler) Snapshot() map[string]ChargerView {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]ChargerView, len(h.chargers))
	for id, s := range h.chargers {
		out[id] = ChargerView{
			Connected: s.connected,
			Charging:  s.charging,
			PowerW:    s.lastPowerW,
			SessionWh: s.sessionMeterWh,
			TxID:      s.transactionID,
		}
	}
	return out
}

// ChargerView is the public snapshot of a charger's state.
type ChargerView struct {
	Connected bool    `json:"connected"`
	Charging  bool    `json:"charging"`
	PowerW    float64 `json:"power_w"`
	SessionWh float64 `json:"session_wh"`
	TxID      int     `json:"tx_id"`
}

// OnConnect / OnDisconnect are wired by the Server to the OCPP library's
// connection callbacks, not part of CoreHandler.
func (h *Handler) OnConnect(id string) {
	slog.Info("OCPP charger connected", "charger", id)
	h.tel.RecordDriverSuccess(id)
}

func (h *Handler) OnDisconnect(id string) {
	slog.Info("OCPP charger disconnected", "charger", id)
	s := h.state(id)
	h.mu.Lock()
	s.connected = false
	s.charging = false
	s.lastPowerW = 0
	h.mu.Unlock()
	// Push a zero so the dispatch clamp releases — otherwise the last known
	// non-zero w would survive until staleness kicks in.
	h.pushReading(id, s)
}

// ---- core.CentralSystemHandler ----

func (h *Handler) OnBootNotification(id string, req *core.BootNotificationRequest) (*core.BootNotificationConfirmation, error) {
	slog.Info("OCPP boot",
		"charger", id,
		"vendor", req.ChargePointVendor,
		"model", req.ChargePointModel,
		"fw", req.FirmwareVersion)
	h.tel.RecordDriverSuccess(id)
	return core.NewBootNotificationConfirmation(
		types.NewDateTime(time.Now()),
		h.heartbeatIntervalS,
		core.RegistrationStatusAccepted,
	), nil
}

func (h *Handler) OnHeartbeat(id string, _ *core.HeartbeatRequest) (*core.HeartbeatConfirmation, error) {
	h.tel.RecordDriverSuccess(id)
	return core.NewHeartbeatConfirmation(types.NewDateTime(time.Now())), nil
}

func (h *Handler) OnAuthorize(id string, req *core.AuthorizeRequest) (*core.AuthorizeConfirmation, error) {
	// Phase 1: auto-authorize every tag. RFID gating is a Phase 2 feature
	// alongside RemoteStartTransaction.
	slog.Debug("OCPP authorize", "charger", id, "tag", req.IdTag)
	return core.NewAuthorizationConfirmation(
		types.NewIdTagInfo(types.AuthorizationStatusAccepted),
	), nil
}

func (h *Handler) OnDataTransfer(id string, req *core.DataTransferRequest) (*core.DataTransferConfirmation, error) {
	// Vendor-specific extensions — accept gracefully but do nothing.
	slog.Debug("OCPP DataTransfer ignored", "charger", id, "vendor", req.VendorId)
	return core.NewDataTransferConfirmation(core.DataTransferStatusUnknownVendorId), nil
}

func (h *Handler) OnStatusNotification(id string, req *core.StatusNotificationRequest) (*core.StatusNotificationConfirmation, error) {
	s := h.state(id)
	h.mu.Lock()
	switch req.Status {
	case core.ChargePointStatusAvailable, core.ChargePointStatusUnavailable:
		s.connected = false
		s.charging = false
	case core.ChargePointStatusPreparing,
		core.ChargePointStatusFinishing,
		core.ChargePointStatusSuspendedEV,
		core.ChargePointStatusSuspendedEVSE,
		core.ChargePointStatusReserved:
		s.connected = true
		s.charging = false
	case core.ChargePointStatusCharging:
		s.connected = true
		s.charging = true
	case core.ChargePointStatusFaulted:
		s.connected = true
		s.charging = false
	}
	h.mu.Unlock()

	if req.Status == core.ChargePointStatusFaulted {
		h.tel.EmitMetric(id, "ev_fault", 1, "", "", "")
		slog.Warn("OCPP charger faulted", "charger", id, "errorCode", req.ErrorCode, "info", req.Info)
	}
	slog.Info("OCPP status",
		"charger", id, "connector", req.ConnectorId, "status", req.Status)

	h.pushReading(id, s)
	h.tel.RecordDriverSuccess(id)
	return core.NewStatusNotificationConfirmation(), nil
}

func (h *Handler) OnMeterValues(id string, req *core.MeterValuesRequest) (*core.MeterValuesConfirmation, error) {
	s := h.state(id)
	h.mu.Lock()
	for _, mv := range req.MeterValue {
		for _, sv := range mv.SampledValue {
			measurand := sv.Measurand
			// OCPP 1.6 default measurand if unspecified.
			if measurand == "" {
				measurand = types.MeasurandEnergyActiveImportRegister
			}
			val, err := strconv.ParseFloat(sv.Value, 64)
			if err != nil {
				continue
			}
			switch measurand {
			case types.MeasurandPowerActiveImport:
				if sv.Unit == types.UnitOfMeasureKW {
					val *= 1000
				}
				s.lastPowerW = val
			case types.MeasurandEnergyActiveImportRegister:
				if sv.Unit == types.UnitOfMeasureKWh {
					val *= 1000
				}
				if s.transactionID >= 0 {
					s.sessionMeterWh = val - s.sessionStartMeterWh
				}
			}
		}
	}
	h.mu.Unlock()

	h.pushReading(id, s)
	h.tel.RecordDriverSuccess(id)
	return core.NewMeterValuesConfirmation(), nil
}

func (h *Handler) OnStartTransaction(id string, req *core.StartTransactionRequest) (*core.StartTransactionConfirmation, error) {
	h.mu.Lock()
	txID := h.nextTxID
	h.nextTxID++
	s := h.chargersLocked(id)
	s.transactionID = txID
	s.sessionStartMeterWh = float64(req.MeterStart)
	s.sessionMeterWh = 0
	s.connected = true
	s.charging = true
	h.mu.Unlock()

	slog.Info("OCPP transaction started",
		"charger", id, "txid", txID, "tag", req.IdTag, "meter_start_wh", req.MeterStart)
	h.pushReading(id, s)
	h.tel.RecordDriverSuccess(id)
	return core.NewStartTransactionConfirmation(
		types.NewIdTagInfo(types.AuthorizationStatusAccepted),
		txID,
	), nil
}

func (h *Handler) OnStopTransaction(id string, req *core.StopTransactionRequest) (*core.StopTransactionConfirmation, error) {
	s := h.state(id)
	h.mu.Lock()
	sessionWh := float64(req.MeterStop) - s.sessionStartMeterWh
	s.transactionID = -1
	s.charging = false
	s.lastPowerW = 0
	s.sessionMeterWh = sessionWh
	h.mu.Unlock()

	slog.Info("OCPP transaction stopped",
		"charger", id, "txid", req.TransactionId,
		"session_wh", sessionWh, "reason", req.Reason)
	h.pushReading(id, s)
	h.tel.EmitMetric(id, "ev_session_wh", sessionWh, "Wh", "", "")
	h.tel.RecordDriverSuccess(id)
	return core.NewStopTransactionConfirmation(), nil
}

// chargersLocked is the same as state(id) but assumes h.mu is already held.
func (h *Handler) chargersLocked(id string) *chargerState {
	s, ok := h.chargers[id]
	if !ok {
		s = &chargerState{transactionID: -1}
		h.chargers[id] = s
	}
	return s
}

// pushReading pushes the current state as a DerEV reading. The dispatch
// clamp (control/dispatch.go) sums all DerEV readings into state.EVChargingW
// every tick — so the charger's lastPowerW immediately suppresses home
// battery discharge.
func (h *Handler) pushReading(id string, s *chargerState) {
	h.mu.Lock()
	w := s.lastPowerW
	data := map[string]any{
		"type":       "ev",
		"w":          w,
		"connected":  s.connected,
		"charging":   s.charging,
		"session_wh": s.sessionMeterWh,
	}
	h.mu.Unlock()
	blob, _ := json.Marshal(data)
	h.tel.Update(id, telemetry.DerEV, w, nil, blob)
}
