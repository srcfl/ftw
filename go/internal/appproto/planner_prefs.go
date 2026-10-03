package appproto

import (
	"github.com/srcfl/ftw/go/internal/config"
)

// setPlannerPrefs stores the household safety factor and battery-export
// permission. Which planner mode that permission selects is the port's
// answer, read back after the write — this handler never maps an export
// permission onto a mode of its own.
func (h *Handler) setPlannerPrefs(cmd Cmd, uptimeMs int64) error {
	prefs := h.cfg.PlannerPrefs
	if prefs == nil {
		return h.sendCmdResult(CmdResult{
			CmdID: cmd.CmdID,
			State: CmdRejected,
			Error: &ErrorBody{
				Code:      ErrUnavailable,
				Retryable: ErrorRetryable[ErrUnavailable],
				Args:      map[string]any{"op": cmd.Op},
			},
		})
	}

	k, ok := argNum(cmd.Args, "safety_k")
	if !ok {
		return h.rejectArg(cmd, "safety_k", cmd.Args["safety_k"])
	}
	export, _ := cmd.Args["battery_export"].(string)
	if _, ok := config.ParseBatteryExport(export); !ok {
		return h.rejectArg(cmd, "battery_export", cmd.Args["battery_export"])
	}

	if _, err := h.acceptCmd(cmd, uptimeMs); err != nil {
		return err
	}

	snap, err := prefs.Apply(k, export)
	if err != nil {
		return h.settleAndReport(cmd.CmdID, CmdResult{
			CmdID: cmd.CmdID,
			State: CmdRejected,
			Error: &ErrorBody{
				Code:      ErrUnavailable,
				Retryable: ErrorRetryable[ErrUnavailable],
				Args:      map[string]any{"op": cmd.Op},
			},
		})
	}

	readAtMs := h.cfg.Clock.UptimeMs()
	return h.settleAndReport(cmd.CmdID, CmdResult{
		CmdID: cmd.CmdID,
		State: CmdApplied,
		Observed: &Observed{
			Value:    snap.SafetyK,
			Src:      ObservedSrcCore,
			UptimeMs: readAtMs,
		},
	})
}
