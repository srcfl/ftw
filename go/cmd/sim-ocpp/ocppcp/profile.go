package ocppcp

import (
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/smartcharging"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
	smartcharging201 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/smartcharging"
	types201 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/types"
)

// ProfileAttempt is one SetChargingProfile the simulator received, so tests
// can see the connector-0 retry without reading the OCPP library's guts.
type ProfileAttempt struct {
	ConnectorID int
	EVSEID      int
	Kind        string
	LimitA      float64
	Applied     bool
	Status      string
}

type profileDecision struct {
	status  string
	applied bool
	limitA  float64
}

func decideProfile(q Quirks, connectorOrEVSE int, kind string, hasStart bool, limitA float64) profileDecision {
	if q.IgnoreAbsoluteWithoutStart && kind == string(types.ChargingProfileKindAbsolute) && !hasStart {
		return profileDecision{status: string(smartcharging.ChargingProfileStatusAccepted), applied: false, limitA: limitA}
	}
	if q.RejectConnectorZero && connectorOrEVSE == 0 {
		return profileDecision{status: string(smartcharging.ChargingProfileStatusRejected), applied: false, limitA: limitA}
	}
	return profileDecision{status: string(smartcharging.ChargingProfileStatusAccepted), applied: true, limitA: limitA}
}

func decide16(q Quirks, req *smartcharging.SetChargingProfileRequest) profileDecision {
	if req == nil || req.ChargingProfile == nil || req.ChargingProfile.ChargingSchedule == nil {
		return profileDecision{status: string(smartcharging.ChargingProfileStatusRejected)}
	}
	limit := 0.0
	periods := req.ChargingProfile.ChargingSchedule.ChargingSchedulePeriod
	if len(periods) > 0 {
		limit = periods[0].Limit
	}
	hasStart := req.ChargingProfile.ChargingSchedule.StartSchedule != nil
	return decideProfile(q, req.ConnectorId, string(req.ChargingProfile.ChargingProfileKind), hasStart, limit)
}

func decide201(q Quirks, req *smartcharging201.SetChargingProfileRequest) profileDecision {
	if req == nil || req.ChargingProfile == nil || len(req.ChargingProfile.ChargingSchedule) == 0 {
		return profileDecision{status: string(smartcharging201.ChargingProfileStatusRejected)}
	}
	sched := req.ChargingProfile.ChargingSchedule[0]
	limit := 0.0
	if len(sched.ChargingSchedulePeriod) > 0 {
		limit = sched.ChargingSchedulePeriod[0].Limit
	}
	hasStart := sched.StartSchedule != nil
	kind := string(req.ChargingProfile.ChargingProfileKind)
	if kind == string(types201.ChargingProfileKindAbsolute) {
		kind = string(types.ChargingProfileKindAbsolute)
	}
	return decideProfile(q, req.EvseID, kind, hasStart, limit)
}
