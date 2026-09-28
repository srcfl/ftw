package main

import (
	"log/slog"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
)

// batteryPowerLimits resolves limits for both planning and dispatch, at boot
// and on config reload. Missing ratings use dispatch's existing 5 kW default:
// battery energy capacity alone does not establish a safe power rating.
func batteryPowerLimits(d config.Driver, b config.Battery) control.PowerLimits {
	limits := control.PowerLimits{
		MaxChargeW: control.MaxCommandW, MaxDischargeW: control.MaxCommandW,
		MaxChargeWSet: true, MaxDischargeWSet: true,
	}
	if d.MaxChargeW > 0 {
		limits.MaxChargeW = d.MaxChargeW
	}
	if d.MaxDischargeW > 0 {
		limits.MaxDischargeW = d.MaxDischargeW
	}
	// Preserve the existing both-zero error policy and one-sided zero limits.
	if b.MaxChargeW != nil && *b.MaxChargeW == 0 && b.MaxDischargeW != nil && *b.MaxDischargeW == 0 {
		slog.Warn("battery: ignoring both-zero overrides; retaining driver limits or the dispatch default",
			"driver", d.Name, "max_charge_w", limits.MaxChargeW, "max_discharge_w", limits.MaxDischargeW)
		return limits
	}
	apply := func(override *float64, limit *float64, field string) {
		if override == nil {
			return
		}
		if *override >= 0 {
			*limit = *override
		} else {
			slog.Warn("battery: ignoring negative power limit; retaining driver limit or the dispatch default",
				"driver", d.Name, "field", field, "value", *override, "retained_w", *limit)
		}
	}
	apply(b.MaxChargeW, &limits.MaxChargeW, "max_charge_w")
	apply(b.MaxDischargeW, &limits.MaxDischargeW, "max_discharge_w")
	return limits
}
