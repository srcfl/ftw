package mpc

import "math"

// PVCurtailment is Core's proof for one control domain covering all site PV.
// The worker sees the executable bounds; proof stays inside Core.
type PVCurtailment struct {
	Driver string
	// A restored diagnostic cannot grant control permission to a new process.
	Proof      string `json:"-"`
	MinW, MaxW float64
}

func (p PVCurtailment) Valid() bool {
	return p.Driver != "" && p.Proof != "" && finite(p.MinW) && finite(p.MaxW) && p.MinW >= 2 && p.MinW == math.Ceil(p.MinW) && p.MaxW >= p.MinW
}

func (p PVCurtailment) Covers(slots []Slot) bool {
	if !p.Valid() {
		return false
	}
	for _, s := range slots {
		if -s.PVW > p.MaxW {
			return false
		}
	}
	return true
}

func (s *Service) pvExecutionAllowed(p PVCurtailment) bool {
	if p.MinW == 0 && p.Proof == "" {
		return true
	}
	return p.Valid() && s.PVExecutionAllowed != nil && s.PVExecutionAllowed(p)
}
