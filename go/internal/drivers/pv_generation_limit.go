package drivers

import (
	"fmt"
	"math"
	"strconv"
)

// PVGenerationLimit proves a generation ceiling in integer watts, with a
// configured release ceiling. It does not describe export or inverter AC caps.
type PVGenerationLimit struct {
	Token      string
	MinW, MaxW float64
}

func (d *LuaDriver) clearPVGenerationLimit() {
	d.pvProofMu.Lock()
	defer d.pvProofMu.Unlock()
	d.pvProofEpoch++
	d.pvProof = PVGenerationLimit{}
}

// Caller holds the VM lock after successful init. The digest comes from the
// same bytes passed to DoString, never a second read of the driver path.
func (d *LuaDriver) refreshPVGenerationLimit() {
	d.pvProofMu.Lock()
	defer d.pvProofMu.Unlock()
	const reviewedFerroamp = "c04d137d595ba50b8c6178c82d917b115dbe9a7cbd2cf671ef2660e871f96de3"
	if d.loadedSourceSHA256 != reviewedFerroamp || d.Env.MQTT == nil || d.initConfig["_supports_pv_curtail"] != true {
		return
	}
	w, err := strconv.ParseFloat(fmt.Sprint(d.initConfig["pplim_release_w"]), 64)
	if err != nil || math.IsNaN(w) || math.IsInf(w, 0) || w < 2 || w > math.MaxInt32 {
		return
	}
	w = math.Floor(w)
	d.pvProof = PVGenerationLimit{Token: fmt.Sprintf("%s/%d/%.0f", reviewedFerroamp, d.pvProofEpoch, w), MinW: 2, MaxW: w}
}

func (d *LuaDriver) PVGenerationLimit() PVGenerationLimit {
	d.pvProofMu.RLock()
	defer d.pvProofMu.RUnlock()
	return d.pvProof
}

func (r *Registry) PVGenerationLimit(name string) PVGenerationLimit {
	r.mu.Lock()
	defer r.mu.Unlock()
	rd := r.rec[name]
	if rd == nil || rd.cfg.Disabled || rd.cfg.ObserveOnly || rd.cfg.BatteryTelemetryOnly || !rd.cfg.SupportsPVCurtail {
		return PVGenerationLimit{}
	}
	s := rd.controlStatus()
	if s.Blocked || s.RecoveryPending {
		return PVGenerationLimit{}
	}
	l, ok := rd.driver.(*luaRuntime)
	if !ok {
		return PVGenerationLimit{}
	}
	proof := l.PVGenerationLimit()
	if proof.Token != "" {
		proof.Token = fmt.Sprintf("%d/%s", s.Generation, proof.Token)
	}
	return proof
}
