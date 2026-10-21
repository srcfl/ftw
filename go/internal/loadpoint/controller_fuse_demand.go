package loadpoint

// FuseDemand is an eligible charger's requested power before live fuse limits.
// It is intent, never measured power or permission to command the charger.
type FuseDemand struct {
	DriverName string
	PowerW     float64
	PhaseMode  string
}

// FuseDemands returns a copy of the latest tick's positive requests. A stale
// dispatch tick clears them before sending any safety standdown.
func (c *Controller) FuseDemands() []FuseDemand {
	if c == nil {
		return nil
	}
	c.fuseDemandMu.RLock()
	defer c.fuseDemandMu.RUnlock()
	return append([]FuseDemand(nil), c.fuseDemands...)
}

func (c *Controller) clearFuseDemands() {
	c.fuseDemandMu.Lock()
	c.fuseDemands = nil
	c.fuseDemandMu.Unlock()
}

func (c *Controller) recordFuseDemand(driver string, powerW float64, phaseMode string) {
	if driver == "" || !finite(powerW) || powerW <= 0 {
		return
	}
	if phaseMode == "" {
		phaseMode = "3p" // The completed command leaves the driver's default in force.
	} else if phaseMode != "1p" && phaseMode != "2p" && phaseMode != "3p" {
		phaseMode = "auto"
	}
	c.fuseDemandMu.Lock()
	defer c.fuseDemandMu.Unlock()
	for i := range c.fuseDemands {
		d := &c.fuseDemands[i]
		if d.DriverName == driver {
			d.PowerW = max(d.PowerW, powerW)
			if d.PhaseMode != phaseMode {
				d.PhaseMode = "auto"
			}
			return
		}
	}
	c.fuseDemands = append(c.fuseDemands, FuseDemand{DriverName: driver, PowerW: powerW, PhaseMode: phaseMode})
}
