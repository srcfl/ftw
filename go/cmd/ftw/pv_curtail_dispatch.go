package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/srcfl/ftw/go/internal/control"
)

// dispatchPVCurtail sends this tick's curtail decision and files what each
// inverter made of it.
//
// The two branches are not the same command wearing different numbers.
//
// A `curtail` is core capping an inverter's output because exporting past the
// cap loses money. It is the PV equivalent of a battery setpoint, and an
// inverter that answers every poll and refuses it keeps producing into a
// negative price while the plan books the saving. Counted.
//
// A `curtail_disable` is core letting go: the inverter goes back to its own
// output. Nothing is being actuated, so a refusal proves nothing about the
// device — the same reason #800 never counts the default release. It also
// must not be counted for a mechanical reason: ComputePVCurtail emits a
// release the moment a driver drops offline, so a counted refusal here would
// let an excluded inverter pin itself out on its own exclusion.
//
// Note what the drivers actually return, because the difference matters more
// than the rule does. ferroamp.lua refuses a curtail that resolves to <= 0 W
// — `pplim arg=0` means "produce nothing" on that firmware and sticks until
// somebody clears it from the portal — and it refuses by returning nil, which
// reaches core as success. That refusal is the driver protecting the site, and
// it is invisible here, which is right. What is visible is a write the driver
// tried and could not land: sungrow.lua returning false when the active-power
// registers will not take the ratio. That one means core cannot cap this
// inverter, and it is the one worth acting on.
func dispatchPVCurtail(
	ctx context.Context,
	reg driverCommandSender,
	tracker *driverActuationTracker,
	targets []control.CurtailTarget,
	timeout time.Duration,
	now time.Time,
) {
	for _, c := range targets {
		if c.LimitW > 0 {
			payload, _ := json.Marshal(map[string]any{
				"action":  "curtail",
				"power_w": c.LimitW,
			})
			tracker.dispatchCommand(ctx, reg, "pv curtail send", c.Driver, payload, timeout, now)
			continue
		}
		payload, _ := json.Marshal(map[string]any{"action": "curtail_disable"})
		tracker.releaseCommand(ctx, reg, "pv curtail release", c.Driver, payload, timeout)
	}
}
