package ocpp

// NotifyEVChargingNeeds — what the car asked for.
//
// During an ISO 15118 charge-parameter discovery the EV states its own needs,
// and the charging station forwards them to us as NotifyEVChargingNeeds. This
// is the vehicle speaking rather than the charger or the operator: the energy
// it wants, when it expects to leave, and on DC also its battery capacity and
// present state of charge.
//
// That outranks configuration. Both `vehicle_capacity_wh` and a vehicle
// profile are an operator's estimate of the car that usually parks here; this
// is the car actually plugged in, for this session. Where the message carries
// a figure it replaces the configured one for the session and reverts on
// plug-out, exactly like an identified vehicle profile.
//
// Quarantine applies as everywhere else: a pending charge point's needs are
// recorded and visible in the API so an operator can see what asked, but the
// callback that reaches a loadpoint never fires for it.
//
// Units follow the core convention — energy in Wh, SoC as a 0-1 fraction. The
// wire carries Wh and whole percent, converted here at the boundary.

import (
	"log/slog"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/smartcharging"
	types201 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/types"

	"github.com/srcfl/ftw/go/internal/units"
)

// ChargingNeeds is one NotifyEVChargingNeeds report in core units.
//
// Only TransferMode is mandatory on the wire, so a zero field means "the car
// did not say", never "zero". The two SoC fields are pointers because there a
// genuine zero is meaningful — an empty battery is a real state of charge.
type ChargingNeeds struct {
	// TransferMode is the energy transfer the car asked for:
	// AC_single_phase, AC_two_phase, AC_three_phase or DC.
	TransferMode string `json:"transfer_mode,omitempty"`

	// EnergyWh is the energy requested, including preconditioning.
	EnergyWh float64 `json:"energy_wh,omitempty"`

	// DepartureTime is when the EV estimates it will leave. Zero when the
	// car did not say, which is the common case on AC.
	DepartureTime time.Time `json:"departure_time,omitempty"`

	// PresentSoC and FullSoC are 0-1 fractions: where the battery is now,
	// and where the car considers it full. DC sessions only — the AC
	// parameter set has no SoC at all.
	PresentSoC *float64 `json:"present_soc,omitempty"`
	FullSoC    *float64 `json:"full_soc,omitempty"`

	// CapacityWh is the car's own battery capacity. DC sessions only.
	CapacityWh float64 `json:"capacity_wh,omitempty"`

	// MaxCurrentA, MaxVoltageV and MaxPowerW are the car's own ceilings.
	// They are recorded but never used to raise a limit — control clamps
	// down from the loadpoint's rating, never up from the car's claim.
	MaxCurrentA float64 `json:"max_current_a,omitempty"`
	MaxVoltageV float64 `json:"max_voltage_v,omitempty"`
	MaxPowerW   float64 `json:"max_power_w,omitempty"`

	// EVSEID is the EVSE the needs apply to. A home charger has one.
	EVSEID int `json:"evse_id,omitempty"`

	// ReceivedAt is when we took the report, so a stale departure time is
	// recognisable as stale rather than read as current intent.
	ReceivedAt time.Time `json:"received_at,omitempty"`
}

// TargetSoC derives the state of charge this session should aim for: where the
// battery is now, plus the energy the car asked for, over its own capacity.
//
// Reports false unless the car gave all three. An AC session states energy
// alone, and energy alone says nothing about a fraction of a battery whose
// size is unknown — guessing one there would feed the planner a number the car
// never claimed. Capped at FullSoC when the car named one, otherwise at 1.
func (n ChargingNeeds) TargetSoC() (float64, bool) {
	if n.PresentSoC == nil || n.CapacityWh <= 0 || n.EnergyWh <= 0 {
		return 0, false
	}
	ceiling := 1.0
	if n.FullSoC != nil && *n.FullSoC > 0 {
		ceiling = *n.FullSoC
	}
	target := *n.PresentSoC + n.EnergyWh/n.CapacityWh
	if target > ceiling {
		target = ceiling
	}
	return units.ClampFraction(target), true
}

// chargingNeedsFrom converts a wire request into core units.
func chargingNeedsFrom(req *smartcharging.NotifyEVChargingNeedsRequest, now time.Time) ChargingNeeds {
	n := ChargingNeeds{
		TransferMode: string(req.ChargingNeeds.RequestedEnergyTransfer),
		EVSEID:       req.EvseID,
		ReceivedAt:   now,
	}
	if dt := req.ChargingNeeds.DepartureTime; dt != nil {
		n.DepartureTime = dt.Time
	}
	if ac := req.ChargingNeeds.ACChargingParameters; ac != nil {
		n.EnergyWh = float64(ac.EnergyAmount)
		n.MaxCurrentA = float64(ac.EVMaxCurrent)
		n.MaxVoltageV = float64(ac.EVMaxVoltage)
	}
	if dc := req.ChargingNeeds.DCChargingParameters; dc != nil {
		n.MaxCurrentA = float64(dc.EVMaxCurrent)
		n.MaxVoltageV = float64(dc.EVMaxVoltage)
		if dc.EnergyAmount != nil {
			n.EnergyWh = float64(*dc.EnergyAmount)
		}
		if dc.EVMaxPower != nil {
			n.MaxPowerW = float64(*dc.EVMaxPower)
		}
		if dc.EVEnergyCapacity != nil {
			n.CapacityWh = float64(*dc.EVEnergyCapacity)
		}
		if dc.StateOfCharge != nil {
			f := units.ClampFraction(float64(*dc.StateOfCharge) / 100.0)
			n.PresentSoC = &f
		}
		if dc.FullSoC != nil {
			f := units.ClampFraction(float64(*dc.FullSoC) / 100.0)
			n.FullSoC = &f
		}
	}
	return n
}

// noteChargingNeeds records a report and, for an adopted charger, hands it to
// the loadpoint layer.
//
// Unlike a vehicle identity this fires on every report rather than only on
// change: the car is allowed to revise what it wants mid-session — a departure
// time moves, a preconditioning estimate is refined — and the latest statement
// is the one the planner should size on.
func (h *Handler) noteChargingNeeds(id string, n ChargingNeeds) {
	h.mu.Lock()
	s := h.chargersLocked(id)
	s.needs = &n
	fn := h.chargingNeeds
	approved := h.approved[id]
	h.mu.Unlock()
	if approved && fn != nil {
		fn(id, n)
	}
}

// SetChargingNeeds registers the callback fired when an adopted charger
// reports what its car asked for. Wired by main.go to the loadpoint bound to
// that charger. Pending chargers never reach it.
func (h *Handler) SetChargingNeeds(fn func(chargerID string, needs ChargingNeeds)) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.chargingNeeds = fn
	h.mu.Unlock()
}

// ChargingNeeds returns the last report from a charger, if any.
func (h *Handler) ChargingNeeds(id string) (ChargingNeeds, bool) {
	if h == nil {
		return ChargingNeeds{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.chargers[id]
	if !ok || s.needs == nil {
		return ChargingNeeds{}, false
	}
	return *s.needs, true
}

// ---- smartcharging.CSMSHandler ----

// OnNotifyEVChargingNeeds takes the car's stated needs.
//
// Accepted means we processed the message, not that we can meet the request —
// the spec is explicit about that, and the schedule we send back is whatever
// the planner works out on its own tick.
func (h *handlerV201) OnNotifyEVChargingNeeds(id string, req *smartcharging.NotifyEVChargingNeedsRequest) (*smartcharging.NotifyEVChargingNeedsResponse, error) {
	if req == nil {
		return smartcharging.NewNotifyEVChargingNeedsResponse(smartcharging.EVChargingNeedsStatusRejected), nil
	}
	n := chargingNeedsFrom(req, time.Now())

	slog.Info("OCPP charging needs",
		"charger", id, "version", Version201,
		"evse", n.EVSEID, "mode", n.TransferMode,
		"energy_wh", n.EnergyWh, "capacity_wh", n.CapacityWh,
		"departure", n.DepartureTime)

	h.noteChargingNeeds(id, n)
	h.telSuccess(id)
	return smartcharging.NewNotifyEVChargingNeedsResponse(smartcharging.EVChargingNeedsStatusAccepted), nil
}

// OnNotifyEVChargingSchedule carries the schedule the EV worked out for
// itself. FTW plans centrally against site power, price and PV, so this is
// acknowledged and dropped — accepting it costs nothing and refusing would
// make the charger retry forever.
func (h *handlerV201) OnNotifyEVChargingSchedule(id string, _ *smartcharging.NotifyEVChargingScheduleRequest) (*smartcharging.NotifyEVChargingScheduleResponse, error) {
	h.telSuccess(id)
	return smartcharging.NewNotifyEVChargingScheduleResponse(types201.GenericStatusAccepted), nil
}

// OnNotifyChargingLimit reports a limit imposed by something other than us —
// a local load-management box. Acknowledged and dropped: the charger enforces
// it whatever we send, and our own commands are already clamped below the
// loadpoint's rating.
func (h *handlerV201) OnNotifyChargingLimit(id string, _ *smartcharging.NotifyChargingLimitRequest) (*smartcharging.NotifyChargingLimitResponse, error) {
	h.telSuccess(id)
	return smartcharging.NewNotifyChargingLimitResponse(), nil
}

// OnClearedChargingLimit is the other half of that: the external limit is
// gone. Acknowledged and dropped for the same reason.
func (h *handlerV201) OnClearedChargingLimit(id string, _ *smartcharging.ClearedChargingLimitRequest) (*smartcharging.ClearedChargingLimitResponse, error) {
	h.telSuccess(id)
	return smartcharging.NewClearedChargingLimitResponse(), nil
}

// OnReportChargingProfiles answers a GetChargingProfiles we never send.
// Acknowledged and dropped.
func (h *handlerV201) OnReportChargingProfiles(id string, _ *smartcharging.ReportChargingProfilesRequest) (*smartcharging.ReportChargingProfilesResponse, error) {
	h.telSuccess(id)
	return smartcharging.NewReportChargingProfilesResponse(), nil
}
