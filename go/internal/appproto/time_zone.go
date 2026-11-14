package appproto

import "github.com/srcfl/ftw/go/internal/localzone"

// proposeTimeZone offers the client's time zone as the household clock. The
// box decides: it takes the zone only while it keeps none of its own, and
// otherwise answers E_PRECONDITION with the zone it keeps, so a phone abroad
// never moves the house's clock.
func (h *Handler) proposeTimeZone(cmd Cmd, uptimeMs int64) error {
	zones := h.cfg.TimeZones
	if zones == nil {
		return h.rejectWith(cmd, &ErrorBody{
			Code:      ErrUnavailable,
			Retryable: ErrorRetryable[ErrUnavailable],
			Args:      map[string]any{"op": cmd.Op},
		})
	}
	zone, _ := cmd.Args["tz"].(string)
	if _, err := localzone.Load(zone); err != nil {
		return h.rejectWith(cmd, badArg(cmd.Op, "tz", cmd.Args["tz"]))
	}

	if took, err := h.acceptCmd(cmd, uptimeMs); !took || err != nil {
		return err
	}

	answer, err := zones.ProposeTimeZone(zone)
	if err != nil {
		h.log.Warn("time zone not stored", "tz", zone, "err", err)
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
	if !answer.Taken {
		return h.settleAndReport(cmd.CmdID, CmdResult{
			CmdID: cmd.CmdID,
			State: CmdRejected,
			Error: &ErrorBody{
				Code:      ErrPrecondition,
				Retryable: ErrorRetryable[ErrPrecondition],
				Args:      map[string]any{"op": cmd.Op, "tz": answer.Zone},
			},
		})
	}
	// Applied: the site stores the zone. Core restarts to run on it, and the
	// next session's hello names it.
	return h.settleAndReport(cmd.CmdID, CmdResult{CmdID: cmd.CmdID, State: CmdApplied})
}
