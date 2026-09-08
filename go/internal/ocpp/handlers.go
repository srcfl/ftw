package ocpp

import (
	"encoding/json"
	"log/slog"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"

	"github.com/srcfl/ftw/go/internal/telemetry"
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
	// approved is the set of charger ids named by a loadpoint in config. A
	// charger outside it is "pending": it may connect and is visible in
	// Snapshot, but nothing it reports enters telemetry — no DerEV reading,
	// no driver health, no metrics — so it cannot influence dispatch. See
	// SetApprovedIDs.
	approved map[string]bool
	nextTxID int

	// vehicleIdentified, when set, fires once per new vehicle identity seen
	// on an APPROVED charger's transaction — main.go uses it to apply the
	// matching vehicle profile (capacity, charging policy) to the loadpoint.
	// Pending chargers never fire it: quarantine means no influence.
	vehicleIdentified func(chargerID, vehicleID, source string)

	// chargingNeeds, when set, fires on every NotifyEVChargingNeeds an
	// APPROVED charger reports — main.go uses it to size the session on
	// what the car asked for (see charging_needs.go). Pending chargers
	// never fire it, for the same reason they never fire the one above.
	chargingNeeds func(chargerID string, needs ChargingNeeds)

	// identityReported, when set, fires when an APPROVED charger says what
	// it is (BootNotification) — main.go writes its row in the device
	// registry from it. See identity.go.
	identityReported func(ChargerIdentity)

	// capabilityProbe, set by the Server, asks a charger which feature
	// profiles it supports (see capabilities.go). Fired from OnConnect and
	// OnBootNotification until the charger answers, then never again.
	capabilityProbe func(id string)
	// probing holds the chargers with a capability request in flight, so
	// the connect and boot triggers — which arrive milliseconds apart, long
	// before any answer — ask once between them rather than twice.
	probing map[string]bool
	// identityProbe asks for a fresh BootNotification; only that message can
	// establish hardware identity on a new socket. See identity_probe.go.
	identityProbe        func(string, Version, func(error)) error
	identityProbeTiming  identityProbeTiming
	identityProbeStopped bool
}

// chargerState is what we accumulate from successive OCPP messages for one
// charge point. Survives the OCPP library's stateless handler invocations.
type chargerState struct {
	// online is whether the charger currently holds a WebSocket session.
	// Distinct from connected below, which tracks whether a connector has a
	// vehicle on it. A charger can be online with nothing plugged in, and
	// still needs to accept a default charging profile in that state.
	online              bool
	connected           bool
	charging            bool
	transactionID       int
	sessionStartMeterWh float64
	sessionMeterWh      float64
	lastPowerW          float64
	forecastPower       telemetry.ForecastPowerSample
	powerConnectedAt    time.Time
	// lastAmps is the most recent per-phase limit this charger accepted.
	// A resume with no rate of its own restores it.
	lastAmps float64
	// version is the OCPP dialect this charger connected with, which decides
	// how commands are encoded on the way back.
	version Version
	// transactionRef is the 2.0.1 transaction id, which is a string rather
	// than the int transactionID above. Empty on the 1.6 path.
	transactionRef string
	// vendor and model come from BootNotification and exist so the UI can
	// label a charger with what it actually is rather than its URL segment.
	// serial is the hardware-stable half of its identity — what the device
	// row is keyed on, when the charger reports one at all. firmware is
	// shown next to them and is otherwise unused.
	vendor   string
	model    string
	serial   string
	firmware string
	// identityCurrent is set only by BootNotification on this connection.
	connectionGeneration  uint64 // process-local socket epoch; never a physical session ID
	connectedKnown        bool   // physical status received on the current socket
	identityCurrent       bool
	identityProbeTimer    *time.Timer
	identityProbeEpoch    uint64
	identityProbeAttempts int
	identityProbeAfter    time.Time
	// vehicleID is the identity presented when the current/last transaction
	// started: the RFID idTag on 1.6, or a 2.0.1 idToken — where the token
	// type MacAddress (autocharge) or eMAID (ISO 15118) names the actual
	// vehicle rather than a card. vehicleIDSource records which kind it was.
	// Kept after the session ends so the UI can show what was last seen.
	vehicleID       string
	vehicleIDSource string
	// featureProfiles is the raw capability answer from the probe in
	// capabilities.go (the SupportedFeatureProfiles CSV on 1.6, a
	// synthesized SmartChargingCtrlr line on 2.0.1); empty until one
	// arrived. steerable is the verdict derived from it: nil = never
	// answered, false = telemetry only, true = accepts charging profiles.
	featureProfiles string
	steerable       *bool
	// needs is the last NotifyEVChargingNeeds this charger's car reported
	// (2.0.1 only), nil until one arrives. Kept after the session ends so
	// the UI can show what the last car asked for.
	needs *ChargingNeeds
}

// NewHandler returns a Handler ready to register with a CentralSystem.
// heartbeatIntervalS is what we tell each charger to use in the
// BootNotification confirmation.
func NewHandler(tel *telemetry.Store, heartbeatIntervalS int) *Handler {
	return &Handler{
		tel:                 tel,
		heartbeatIntervalS:  heartbeatIntervalS,
		chargers:            map[string]*chargerState{},
		approved:            map[string]bool{},
		probing:             map[string]bool{},
		identityProbeTiming: defaultIdentityProbeTiming,
		nextTxID:            1,
	}
}

// SetApprovedIDs declares which charger ids are part of the site — the ids
// loadpoints name in config. Everything else that connects is quarantined as
// pending: accepted at the protocol level so the operator can see it in the
// UI and adopt it, but kept out of telemetry so an unknown-but-authenticated
// device cannot fabricate EV load and steer dispatch (the DerEV sum
// suppresses home-battery discharge). Replaces the previous set.
//
// Loadpoints hot-reload, so this is called again on every config apply:
// adoption takes effect on the next message from the charger, and a charger
// whose loadpoint was removed goes back to pending — with a zero reading
// pushed first, for the same reason OnDisconnect pushes one: its last
// non-zero power must not linger in the DerEV sum it is no longer part of.
func (h *Handler) SetApprovedIDs(ids []string) {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id != "" {
			m[id] = true
		}
	}
	h.mu.Lock()
	var revoked []string
	for id, charger := range h.chargers {
		if h.approved[id] && !m[id] {
			revoked = append(revoked, id)
			h.cancelIdentityProbeLocked(charger)
		}
	}
	h.approved = m
	for id, charger := range h.chargers {
		h.scheduleIdentityProbeLocked(id, charger)
	}
	h.mu.Unlock()
	for _, id := range revoked {
		blob, _ := json.Marshal(map[string]any{"type": "ev", "w": 0.0, "forecast_power": telemetry.ForecastPowerSample{Version: 1}})
		h.tel.Update(id, telemetry.DerEV, 0, nil, blob)
	}
}

// IsApproved reports whether a charger id is on the site allowlist and may
// therefore feed telemetry and accept commands. Pending connectors stay
// visible in Snapshot but are not adopted.
func (h *Handler) IsApproved(id string) bool {
	if h == nil {
		return false
	}
	return h.isApproved(id)
}

// isApproved reports whether a charger id is named by a charger entry
// (loadpoint) and therefore allowed to feed the site model.
func (h *Handler) isApproved(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.approved[id]
}

// The tel* wrappers are the quarantine choke points: every telemetry write
// for a charger goes through one of them, so a pending charger is dropped
// here once rather than gated at each call site.
func (h *Handler) telSuccess(id string) {
	if h.isApproved(id) {
		h.tel.RecordDriverSuccess(id)
	}
}

func (h *Handler) telError(id, msg string) {
	if h.isApproved(id) {
		h.tel.RecordDriverError(id, msg)
	}
}

func (h *Handler) telMetric(id, name string, v float64, unit string) {
	if h.isApproved(id) {
		h.tel.EmitMetric(id, name, v, unit, "", "")
	}
}

// SetVehicleIdentified registers the callback fired when a transaction on an
// approved charger presents a new vehicle identity (RFID idTag on 1.6, any
// idToken on 2.0.1). Fired outside the handler lock.
func (h *Handler) SetVehicleIdentified(fn func(chargerID, vehicleID, source string)) {
	h.mu.Lock()
	h.vehicleIdentified = fn
	h.mu.Unlock()
}

// setControlCapability records a capability-probe answer and the verdict
// derived from it. A telemetry-only verdict is worth a warning: the operator
// may be about to bind a charger the planner can never steer.
func (h *Handler) setControlCapability(id, raw string, steerable bool) {
	h.mu.Lock()
	s := h.chargersLocked(id)
	s.featureProfiles = raw
	b := steerable
	s.steerable = &b
	h.mu.Unlock()
	if steerable {
		slog.Info("ocpp: charger supports smart charging", "charger", id, "profiles", raw)
	} else {
		slog.Warn("ocpp: charger declares no smart-charging support — telemetry only, FTW cannot steer it",
			"charger", id, "profiles", raw)
	}
}

// maybeProbeCapability fires the capability probe unless the charger has
// already answered one, or one is in flight. Called from OnConnect and boot
// notifications so a charger that ignores the probe pre-boot gets asked again
// post-boot, and again on every reconnect, until an answer sticks.
func (h *Handler) maybeProbeCapability(id string) {
	h.mu.Lock()
	fn := h.capabilityProbe
	skip := h.chargersLocked(id).steerable != nil || h.probing[id]
	if fn != nil && !skip {
		h.probing[id] = true
	}
	h.mu.Unlock()
	if fn != nil && !skip {
		go fn(id)
	}
}

// probeFinished releases the in-flight marker, whatever the outcome. A probe
// that failed leaves steerable nil, so the next connect asks again.
func (h *Handler) probeFinished(id string) {
	h.mu.Lock()
	delete(h.probing, id)
	h.mu.Unlock()
}

// noteVehicleID records the identity a transaction presented and fires the
// vehicleIdentified callback when it is new for this charger. Quarantine
// applies: a pending charger's identity is stored (the UI shows it so the
// operator can build a profile from it) but never fires the callback.
func (h *Handler) noteVehicleID(id, vehicleID, source string) {
	if vehicleID == "" {
		return
	}
	h.mu.Lock()
	s := h.chargersLocked(id)
	changed := s.vehicleID != vehicleID
	s.vehicleID = vehicleID
	s.vehicleIDSource = source
	fn := h.vehicleIdentified
	approved := h.approved[id]
	h.mu.Unlock()
	if changed && approved && fn != nil {
		fn(id, vehicleID, source)
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
		v := ChargerView{
			Online:          s.online,
			Connected:       s.connected,
			Charging:        s.charging,
			PowerW:          s.lastPowerW,
			SessionWh:       s.sessionMeterWh,
			TxID:            s.transactionID,
			Version:         string(s.version),
			LastAmps:        s.lastAmps,
			Vendor:          s.vendor,
			Model:           s.model,
			Serial:          s.serial,
			Firmware:        s.firmware,
			Pending:         !h.approved[id],
			VehicleID:       s.vehicleID,
			VehicleIDSource: s.vehicleIDSource,
			FeatureProfiles: s.featureProfiles,
		}
		if s.steerable != nil {
			b := *s.steerable
			v.Steerable = &b
		}
		if s.needs != nil {
			n := *s.needs
			v.ChargingNeeds = &n
		}
		out[id] = v
	}
	return out
}

// ChargerView is the public snapshot of a charger's state.
type ChargerView struct {
	// Online is the WebSocket session; Connected is a vehicle on the
	// connector. A charger is usually online long before it is connected.
	Online    bool    `json:"online"`
	Connected bool    `json:"connected"`
	Charging  bool    `json:"charging"`
	PowerW    float64 `json:"power_w"`
	SessionWh float64 `json:"session_wh"`
	TxID      int     `json:"tx_id"`
	// Version is the OCPP dialect ("1.6" or "2.0.1"); empty until known.
	Version string `json:"version,omitempty"`
	// LastAmps is the last non-zero per-phase limit the charger accepted —
	// what a resume would restore, not necessarily what is flowing now.
	LastAmps float64 `json:"last_amps,omitempty"`
	Vendor   string  `json:"vendor,omitempty"`
	Model    string  `json:"model,omitempty"`
	// Serial is the hardware-stable half of the charger's identity, and
	// what its /api/devices row is keyed on. Empty when the charger
	// reports none — plenty do not.
	Serial   string `json:"serial,omitempty"`
	Firmware string `json:"firmware,omitempty"`
	// Pending is set when no charger entry (loadpoint) names this id. A
	// pending charger is visible here but quarantined from the site: its
	// telemetry is withheld from dispatch until an operator adopts it.
	Pending bool `json:"pending,omitempty"`
	// VehicleID is the identity the current/last transaction presented —
	// an RFID idTag on 1.6 (names the card), a MacAddress/eMAID idToken on
	// 2.0.1 (names the car). VehicleIDSource says which kind.
	VehicleID       string `json:"vehicle_id,omitempty"`
	VehicleIDSource string `json:"vehicle_id_source,omitempty"`
	// FeatureProfiles is the charger's raw capability answer (see
	// capabilities.go); Steerable the verdict: absent = charger never
	// answered the probe, false = telemetry only, true = accepts charging
	// profiles.
	FeatureProfiles string `json:"feature_profiles,omitempty"`
	Steerable       *bool  `json:"steerable,omitempty"`
	// ChargingNeeds is the last thing the car itself asked for, when it
	// said (2.0.1 NotifyEVChargingNeeds). Absent on 1.6, which has no such
	// message, and on any session the EV did not negotiate.
	ChargingNeeds *ChargingNeeds `json:"charging_needs,omitempty"`
}

// OnConnect / OnDisconnect are wired by the Server to the OCPP library's
// connection callbacks, not part of CoreHandler.
func (h *Handler) OnConnect(id string) {
	if h.isApproved(id) {
		slog.Info("OCPP charger connected", "charger", id)
	} else {
		slog.Info("OCPP charger connected as pending — no charger entry names it, telemetry withheld",
			"charger", id)
	}
	s := h.state(id)
	h.mu.Lock()
	s.online = true
	s.powerConnectedAt = time.Now().Truncate(time.Second)
	s.connectionGeneration++
	s.connectedKnown = false
	s.charging = false
	s.lastPowerW = 0
	s.forecastPower.Known = false
	s.identityCurrent = false
	s.featureProfiles = ""
	s.steerable = nil
	h.cancelIdentityProbeLocked(s)
	s.identityProbeAttempts = 0
	h.scheduleIdentityProbeLocked(id, s)
	h.mu.Unlock()
	h.pushReading(id, s)
	h.telSuccess(id)
	h.maybeProbeCapability(id)
}

func (h *Handler) OnDisconnect(id string) {
	slog.Info("OCPP charger disconnected", "charger", id)
	s := h.state(id)
	h.mu.Lock()
	// A probe in flight when the socket dropped may never get its callback,
	// which would leave the marker set and block every future probe. The
	// reconnect is exactly when we want to ask again, so clear it here.
	delete(h.probing, id)
	s.online = false
	s.identityCurrent = false
	h.cancelIdentityProbeLocked(s)
	s.connectedKnown = false
	s.charging = false
	s.lastPowerW = 0
	s.forecastPower.Known = false
	h.mu.Unlock()
	// Push a zero so the dispatch clamp releases — otherwise the last known
	// non-zero w would survive until staleness kicks in.
	h.pushReading(id, s)
}

// ---- core.CentralSystemHandler ----

func (h *Handler) OnBootNotification(id string, req *core.BootNotificationRequest) (*core.BootNotificationConfirmation, error) {
	// 1.6 has two serial fields. chargePointSerialNumber is the current
	// one; chargeBoxSerialNumber is deprecated in the spec and still what
	// a good deal of shipped firmware fills in, so fall back to it rather
	// than lose the only hardware-stable identity a charger will give us.
	serial := req.ChargePointSerialNumber
	if serial == "" {
		serial = req.ChargeBoxSerialNumber
	}
	slog.Info("OCPP boot",
		"charger", id,
		"vendor", req.ChargePointVendor,
		"model", req.ChargePointModel,
		"serial", serial,
		"fw", req.FirmwareVersion)
	s := h.state(id)
	h.mu.Lock()
	s.vendor = req.ChargePointVendor
	s.model = req.ChargePointModel
	s.serial = serial
	s.identityCurrent = true
	h.cancelIdentityProbeLocked(s)
	s.firmware = req.FirmwareVersion
	h.mu.Unlock()
	h.noteIdentity(id)
	h.telSuccess(id)
	h.maybeProbeCapability(id)
	return core.NewBootNotificationConfirmation(
		types.NewDateTime(time.Now()),
		h.heartbeatIntervalS,
		core.RegistrationStatusAccepted,
	), nil
}

func (h *Handler) OnHeartbeat(id string, _ *core.HeartbeatRequest) (*core.HeartbeatConfirmation, error) {
	h.telSuccess(id)
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
	s.connectedKnown = true
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
		h.telMetric(id, "ev_fault", 1, "")
		slog.Warn("OCPP charger faulted", "charger", id, "errorCode", req.ErrorCode, "info", req.Info)
	}
	slog.Info("OCPP status",
		"charger", id, "connector", req.ConnectorId, "status", req.Status)

	h.pushReading(id, s)
	h.telSuccess(id)
	return core.NewStatusNotificationConfirmation(), nil
}

func (h *Handler) OnMeterValues(id string, req *core.MeterValuesRequest) (*core.MeterValuesConfirmation, error) {
	s := h.state(id)
	h.mu.Lock()
	received := time.Now()
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
				if sv.Phase == "" && (sv.Unit == "" || sv.Unit == types.UnitOfMeasureW || sv.Unit == types.UnitOfMeasureKW) {
					measured := received
					if mv.Timestamp != nil {
						measured = mv.Timestamp.Time
					}
					s.recordForecastPower(val, measured, received)
				}
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
	h.telSuccess(id)
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
	s.connectedKnown = true
	s.charging = true
	h.mu.Unlock()

	slog.Info("OCPP transaction started",
		"charger", id, "txid", txID, "tag", req.IdTag, "meter_start_wh", req.MeterStart)
	// The 1.6 idTag names the RFID card that started the session — the
	// closest thing to a vehicle identity this dialect has.
	h.noteVehicleID(id, req.IdTag, "rfid")
	h.pushReading(id, s)
	h.telSuccess(id)
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
	s.forecastPower.Known = false
	s.sessionMeterWh = sessionWh
	h.mu.Unlock()

	slog.Info("OCPP transaction stopped",
		"charger", id, "txid", req.TransactionId,
		"session_wh", sessionWh, "reason", req.Reason)
	h.pushReading(id, s)
	h.telMetric(id, "ev_session_wh", sessionWh, "Wh")
	h.telSuccess(id)
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
	approved := h.approved[id]
	w := s.lastPowerW
	s.forecastPower.Version = 1
	data := map[string]any{
		"type":           "ev",
		"w":              w,
		"charging":       s.charging,
		"session_wh":     s.sessionMeterWh,
		"forecast_power": s.forecastPower,
	}
	data["connection_generation"] = s.connectionGeneration
	if s.online && s.connectedKnown {
		data["connected"] = s.connected
	} else {
		// A socket transition says nothing about the physical cable. Keep
		// the zero out of manual-command acknowledgements in both clients.
		data["connection_unknown"] = true
		data["is_online"] = false
		data["reason_no_current_label"] = "Waiting for charger connection status"
	}
	h.mu.Unlock()
	if !approved {
		// Pending charger: visible in Snapshot, absent from the site model.
		return
	}
	blob, _ := json.Marshal(data)
	h.tel.Update(id, telemetry.DerEV, w, nil, blob)
}

// recordForecastPower is separate from the existing status/dispatch power.
// Only a real aggregate power measurand may refresh it. Samples older than a
// connection or the accepted sample, and future/nonfinite values, cannot revive
// a stale or synthesized reading. The caller holds h.mu.
func (s *chargerState) recordForecastPower(w float64, measured, received time.Time) {
	if math.IsNaN(w) || math.IsInf(w, 0) || w < 0 || measured.IsZero() || measured.After(received) || measured.Before(s.powerConnectedAt) || measured.UnixMilli() <= s.forecastPower.MeasuredAtMS {
		return
	}
	s.forecastPower = telemetry.ForecastPowerSample{Version: 1, Known: true, Watts: w, MeasuredAtMS: measured.UnixMilli(), ReceivedAtMS: received.UnixMilli()}
}
