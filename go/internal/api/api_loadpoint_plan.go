package api

import (
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/mpc"
)

// Planner-visibility decoration for GET /api/loadpoints. Fills the
// fields that answer the operator's first question at plug-in — "when
// will it charge, and if not, why not?":
//
//   - the next window in which the active MPC plan allocates charge
//     energy to the loadpoint (so the UI can say "charging planned
//     02:15–06:30" instead of sitting silent until the cheap slots
//     arrive, which teaches operators to press Start and lose the
//     plan);
//   - whether grid-funded planning is deferred because the deadline
//     lies past the published price horizon (which otherwise behaves
//     exactly like a PV-only mode nobody chose).
//
// Per the api/CLAUDE.md split convention, this lives in its own file
// and is called from handleLoadpoints in api.go.

// decorateLoadpointsWithPlan mutates states in place.
func (s *Server) decorateLoadpointsWithPlan(states []loadpoint.State) {
	now := time.Now()
	var snapshot mpc.PlanSnapshot
	if s.deps.MPC != nil {
		snapshot = s.deps.MPC.PlanSnapshot()
	}
	for i := range states {
		states[i].PlanPending = snapshot.Pending
		states[i].PlanOutdated = snapshot.Outdated
		if snapshot.Outdated {
			continue
		}
		if s.deps.LoadpointCtrl != nil {
			states[i].GridDeferred = s.deps.LoadpointCtrl.GridDeferred(states[i].ID)
		}
		if s.deps.MPC == nil {
			continue
		}
		windows, totalWh := snapshot.LoadpointPlanWindows(states[i].ID, now, maxPlanWindows)
		if len(windows) == 0 {
			continue
		}
		states[i].PlanNextStartMs = windows[0].Start.UnixMilli()
		states[i].PlanNextEndMs = windows[0].End.UnixMilli()
		states[i].PlanNextWh = windows[0].EnergyWh
		states[i].PlanTotalWh = totalWh
		states[i].PlanWindows = make([]loadpoint.PlanWindow, 0, len(windows))
		for _, w := range windows {
			states[i].PlanWindows = append(states[i].PlanWindows, loadpoint.PlanWindow{
				StartMs: w.Start.UnixMilli(),
				EndMs:   w.End.UnixMilli(),
				Wh:      w.EnergyWh,
			})
		}
	}
}

// maxPlanWindows bounds the list a client gets. A 48 h horizon in 15 min
// slots cannot produce more than a few dozen contiguous windows; the cap
// keeps a pathological plan from bloating every /api/loadpoints poll.
const maxPlanWindows = 32
