package ocpp

// Capability discovery: can this charger be steered, or does it only meter?
//
// OCPP 1.6 chargers advertise feature profiles through the
// SupportedFeatureProfiles configuration key; SmartCharging in that list is
// what makes SetChargingProfile work. 2.0.1 models the same fact as the
// SmartChargingCtrlr component's Available variable. FTW probes once per
// charger (re-trying on every connect/boot until an answer arrives), records
// the raw answer, and derives a tri-state verdict: steerable, telemetry-only,
// or unknown.
//
// The verdict is advisory, never a gate. Commands are still attempted and
// refusals handled empirically by the actuation tracker — a charger that
// misreports its own capabilities cannot lock itself out of control. What the
// verdict buys is honesty up front: the Chargers tab can say "telemetry only"
// before the operator binds a charger the planner will never steer.

import (
	"log/slog"
	"strings"

	ocpp16 "github.com/lorenzodonini/ocpp-go/ocpp1.6"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	ocpp201 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/provisioning"
	types201 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/types"
)

// profilesIncludeSmartCharging reports whether a SupportedFeatureProfiles
// CSV names SmartCharging. Vendors vary in spacing and case.
func profilesIncludeSmartCharging(csv string) bool {
	for _, p := range strings.Split(csv, ",") {
		if strings.EqualFold(strings.TrimSpace(p), "SmartCharging") {
			return true
		}
	}
	return false
}

// probeFeatureProfiles16 asks a 1.6 charge point for its feature profiles.
// Async: the library delivers the confirmation on its own goroutine.
func probeFeatureProfiles16(cs ocpp16.CentralSystem, h *Handler, id string) {
	err := cs.GetConfiguration(id, func(conf *core.GetConfigurationConfirmation, err error) {
		defer h.probeFinished(id)
		if err != nil || conf == nil {
			slog.Info("ocpp: charger did not answer GetConfiguration — control capability stays unknown",
				"charger", id, "err", err)
			return
		}
		for _, kv := range conf.ConfigurationKey {
			if kv.Key == "SupportedFeatureProfiles" && kv.Value != nil {
				h.setControlCapability(id, *kv.Value, profilesIncludeSmartCharging(*kv.Value))
				return
			}
		}
		slog.Info("ocpp: charger answered GetConfiguration without SupportedFeatureProfiles — control capability stays unknown",
			"charger", id)
	}, []string{"SupportedFeatureProfiles"})
	if err != nil {
		// The request never left, so no callback will clear the marker.
		h.probeFinished(id)
		slog.Warn("ocpp: GetConfiguration send failed", "charger", id, "err", err)
	}
}

// probeSmartChargingV201 asks a 2.0.1 station whether SmartChargingCtrlr is
// available — the 2.0.1 shape of the SmartCharging feature profile.
func probeSmartChargingV201(csms ocpp201.CSMS, h *Handler, id string) {
	err := csms.GetVariables(id, func(resp *provisioning.GetVariablesResponse, err error) {
		defer h.probeFinished(id)
		if err != nil || resp == nil || len(resp.GetVariableResult) == 0 {
			slog.Info("ocpp: station did not answer GetVariables — control capability stays unknown",
				"charger", id, "err", err)
			return
		}
		r := resp.GetVariableResult[0]
		if r.AttributeStatus != provisioning.GetVariableStatusAccepted {
			slog.Info("ocpp: SmartChargingCtrlr not reported — control capability stays unknown",
				"charger", id, "status", r.AttributeStatus)
			return
		}
		available := strings.EqualFold(strings.TrimSpace(r.AttributeValue), "true")
		h.setControlCapability(id, "SmartChargingCtrlr.Available="+strings.TrimSpace(r.AttributeValue), available)
	}, []provisioning.GetVariableData{{
		Component: types201.Component{Name: "SmartChargingCtrlr"},
		Variable:  types201.Variable{Name: "Available"},
	}})
	if err != nil {
		h.probeFinished(id)
		slog.Warn("ocpp: GetVariables send failed", "charger", id, "err", err)
	}
}
