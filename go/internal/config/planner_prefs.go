package config

import "sync"

const (
	StateKeyForecastTrust = "forecast_trust"
	StateKeyBatteryExport = "battery_export"
)

// ForecastTrust is how hard the planner bets the PV/price forecast is right.
// cautious holds reserve (high k). bold follows the raw forecast (k=0).
type ForecastTrust string

const (
	ForecastTrustCautious ForecastTrust = "cautious"
	ForecastTrustBalanced ForecastTrust = "balanced"
	ForecastTrustBold     ForecastTrust = "bold"
)

// BatteryExport is the household permission for battery-driven grid export.
// unknown means not checked: treat as not allowed.
type BatteryExport string

const (
	BatteryExportUnknown    BatteryExport = "unknown"
	BatteryExportNotAllowed BatteryExport = "not_allowed"
	BatteryExportAllowed    BatteryExport = "allowed"
)

func ParseForecastTrust(s string) (ForecastTrust, bool) {
	switch ForecastTrust(s) {
	case ForecastTrustCautious, ForecastTrustBalanced, ForecastTrustBold:
		return ForecastTrust(s), true
	case "":
		return ForecastTrustBalanced, true
	default:
		return "", false
	}
}

func ParseBatteryExport(s string) (BatteryExport, bool) {
	switch BatteryExport(s) {
	case BatteryExportUnknown, BatteryExportNotAllowed, BatteryExportAllowed:
		return BatteryExport(s), true
	default:
		return "", false
	}
}

// SafetyK is the PV downside haircut scale for this trust level.
func (t ForecastTrust) SafetyK() float64 {
	switch t {
	case ForecastTrustCautious:
		return 2.0
	case ForecastTrustBold:
		return 0.0
	default:
		return 1.0
	}
}

// PlannerModeKey is the control/MPC planner mode that matches this permission.
// unknown and not_allowed both stay on passive (no battery export).
func (e BatteryExport) PlannerModeKey() string {
	if e == BatteryExportAllowed {
		return "planner_arbitrage"
	}
	return "planner_passive_arbitrage"
}

// DeriveBatteryExport maps a persisted control mode onto an export permission
// when SQLite has never stored one. Active arbitrage becomes unknown so the
// household must confirm selling; it does not keep selling in silence.
// ExportFromPlannerMode updates the permission when the operator picks a
// planner mode (HA, app, /api/mode). Manual modes return ok=false.
func ExportFromPlannerMode(mode string) (BatteryExport, bool) {
	switch mode {
	case "planner_arbitrage":
		return BatteryExportAllowed, true
	case "planner_passive_arbitrage":
		return BatteryExportNotAllowed, true
	default:
		return "", false
	}
}

func DeriveBatteryExport(persistedMode string) BatteryExport {
	switch persistedMode {
	case "planner_arbitrage":
		return BatteryExportUnknown
	case "planner_passive_arbitrage", "planner_self", "planner_cheap":
		return BatteryExportNotAllowed
	default:
		return BatteryExportUnknown
	}
}

// ResolvePlannerPrefs builds the live household object from SQLite, then YAML,
// then the persisted control mode. missingStored is true when either SQLite
// key was absent so the caller should persist the result.
func ResolvePlannerPrefs(storedTrust, storedExport, persistedMode, yamlTrust, yamlExport string) (trust ForecastTrust, export BatteryExport, missingStored bool) {
	if t, ok := ParseForecastTrust(storedTrust); ok && storedTrust != "" {
		trust = t
	} else if t, ok := ParseForecastTrust(yamlTrust); ok {
		trust = t
		if storedTrust == "" {
			missingStored = true
		}
	} else {
		trust = ForecastTrustBalanced
		missingStored = true
	}
	if e, ok := ParseBatteryExport(storedExport); ok {
		export = e
	} else if e, ok := ParseBatteryExport(yamlExport); ok && yamlExport != "" {
		export = e
		missingStored = true
	} else {
		export = DeriveBatteryExport(persistedMode)
		missingStored = true
	}
	return trust, export, missingStored
}

// EffectiveSafetyK prefers an explicit YAML k over the trust mapping.
func (p *Planner) EffectiveSafetyK(trust ForecastTrust) float64 {
	if p != nil && p.PVForecastSafetyK != nil {
		return *p.PVForecastSafetyK
	}
	return trust.SafetyK()
}

func (p *Planner) YAMLCustomK() bool {
	return p != nil && p.PVForecastSafetyK != nil
}

// PlannerPrefs is the in-memory household planner object. SQLite is the
// durable copy; this is what /api/status reads on every poll.
type PlannerPrefs struct {
	mu     sync.Mutex
	Trust  ForecastTrust
	Export BatteryExport
}

func NewPlannerPrefs(trust ForecastTrust, export BatteryExport) *PlannerPrefs {
	return &PlannerPrefs{Trust: trust, Export: export}
}

func (p *PlannerPrefs) Get() (ForecastTrust, BatteryExport) {
	if p == nil {
		return ForecastTrustBalanced, BatteryExportUnknown
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.Trust, p.Export
}

func (p *PlannerPrefs) Set(trust ForecastTrust, export BatteryExport) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.Trust = trust
	p.Export = export
	p.mu.Unlock()
}

func (p *PlannerPrefs) ApplyExportFromMode(mode string, save func(key, value string) error) {
	export, ok := ExportFromPlannerMode(mode)
	if !ok || p == nil {
		return
	}
	trust, _ := p.Get()
	p.Set(trust, export)
	if save != nil {
		_ = save(StateKeyBatteryExport, string(export))
	}
}
