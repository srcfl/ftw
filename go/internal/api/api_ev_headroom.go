package api

import (
	"encoding/json"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"math"
	"time"
)

// EVSiteHeadroom is a conditional projection, never a charger diagnosis or
// dispatch input. Current site loads are held constant in the calculation.
type EVSiteHeadroom struct {
	Evidence       string  `json:"evidence"`
	FuseA          float64 `json:"fuse_a"`
	MarginA        float64 `json:"margin_a"`
	ProjectedMaxA  float64 `json:"projected_max_a"`
	BatteryChargeW float64 `json:"battery_charge_w"`
}

func projectEVHeadroom(f ControlFeedback, phases int, voltage float64, fuse config.Fuse, currents, powers []float64, meterAt, now time.Time) *EVSiteHeadroom {
	// Only balanced, explicitly three-phase charging is supported. Single-phase
	// and automatic switching need a verified phase mapping, not an assumed L1.
	if f.Kind != "ev" || f.Reason != "power_below_target" || !f.ReadingsFresh ||
		f.SentW == nil || f.ActualW == nil || *f.ActualW < 100 || phases != 3 || fuse.Phases != 3 ||
		len(currents) != 3 || len(powers) != 3 || voltage <= 0 || fuse.MaxAmps <= 0 || math.IsNaN(voltage) || math.IsInf(voltage, 0) {
		return nil
	}
	age := now.Sub(meterAt)
	skew := meterAt.Sub(time.UnixMilli(f.ObservedAtMs))
	if age < 0 || age > 15*time.Second || skew < -10*time.Second || skew > 10*time.Second {
		return nil
	}
	gap := *f.SentW - *f.ActualW
	if math.IsNaN(gap) || math.IsInf(gap, 0) || gap <= telemetry.ControlToleranceW(*f.SentW) {
		return nil
	}
	margin := fuse.EffectiveSafetyMarginA()
	if math.IsNaN(margin) || math.IsInf(margin, 0) || margin < 0 || margin >= fuse.MaxAmps {
		return nil
	}
	worst := 0.0
	for i, a := range currents {
		// Meters may report current magnitudes during export. Signed power must
		// establish import on every phase before adding the missing EV load.
		if math.IsNaN(a) || math.IsInf(a, 0) || a < 0 ||
			math.IsNaN(powers[i]) || math.IsInf(powers[i], 0) || powers[i] <= 0 {
			return nil
		}
		worst = math.Max(worst, a+gap/(3*voltage))
	}
	if worst <= fuse.MaxAmps-margin+0.5 {
		return nil
	}
	return &EVSiteHeadroom{Evidence: "calculated", FuseA: fuse.MaxAmps, MarginA: margin, ProjectedMaxA: worst}
}

func (s *Server) evSiteHeadroom(f ControlFeedback, lp loadpoint.State, meter string, now time.Time) *EVSiteHeadroom {
	if s.deps.Cfg == nil || s.deps.CfgMu == nil || s.deps.Tel == nil || meter == "" {
		return nil
	}
	s.deps.CfgMu.RLock()
	fuse := s.deps.Cfg.Fuse
	explicit3P := false
	for _, c := range s.deps.Cfg.Loadpoints {
		if c.DriverName == f.Driver && c.PhaseMode == "3p" {
			explicit3P = true
		}
	}
	s.deps.CfgMu.RUnlock()
	if !explicit3P {
		return nil
	}
	rd := s.deps.Tel.Get(meter, telemetry.DerMeter)
	h := s.deps.Tel.DriverHealth(meter)
	if rd == nil || h == nil || !h.TelemetryLive() || h.DeviceFault {
		return nil
	}
	var phase struct {
		L1 *float64 `json:"l1_a"`
		L2 *float64 `json:"l2_a"`
		L3 *float64 `json:"l3_a"`
		W1 *float64 `json:"l1_w"`
		W2 *float64 `json:"l2_w"`
		W3 *float64 `json:"l3_w"`
	}
	if json.Unmarshal(rd.Data, &phase) != nil || phase.L1 == nil || phase.L2 == nil || phase.L3 == nil ||
		phase.W1 == nil || phase.W2 == nil || phase.W3 == nil {
		return nil
	}
	result := projectEVHeadroom(f, lp.Phases, lp.VoltageV, fuse,
		[]float64{*phase.L1, *phase.L2, *phase.L3}, []float64{*phase.W1, *phase.W2, *phase.W3}, rd.UpdatedAt, now)
	if result == nil {
		return nil
	}
	for _, obs := range s.deps.Tel.OnlineBatteryControlPowers(now, 15*time.Second) {
		skew := rd.UpdatedAt.Sub(obs.Seen())
		if skew >= -10*time.Second && skew <= 10*time.Second && obs.PowerW > 100 {
			result.BatteryChargeW += obs.PowerW
		}
	}

	return result
}
