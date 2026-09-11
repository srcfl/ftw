package config

import (
	"math"
	"strconv"
	"strings"
	"sync"
)

const (
	StateKeyForecastTrust = "forecast_trust"
	StateKeyBatteryExport = "battery_export"
	StateKeySafetyK       = "planner_safety_k"
)

// SafetyK bounds. 0 plans against the raw forecast; 2 holds back twice each
// slot's own forecast error. Above 2 the haircut erases the sunny shoulders
// outright, which is a worse plan, not a safer one.
const (
	SafetyKMin     = 0.0
	SafetyKMax     = 2.0
	SafetyKDefault = 1.0
)

// ForecastTrust is how hard the planner bets the PV/price forecast is right.
// cautious holds reserve (high k). bold follows the raw forecast (k=0).
// The stored primitive is the numeric safety_k; this enum survives as the
// compatibility surface older clients read and write.
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

// ClampSafetyK holds a proposed haircut scale inside the slider's range.
// NaN is a failed parse rather than a choice, so it falls back to the
// balanced default instead of poisoning the DP's arithmetic.
func ClampSafetyK(v float64) float64 {
	if math.IsNaN(v) {
		return SafetyKDefault
	}
	if v < SafetyKMin {
		return SafetyKMin
	}
	if v > SafetyKMax {
		return SafetyKMax
	}
	return v
}

// ParseSafetyK reads a stored safety_k. ok=false when the row is absent or
// not a finite number, which sends the caller down the legacy enum path.
func ParseSafetyK(s string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsInf(v, 0) || math.IsNaN(v) {
		return 0, false
	}
	return ClampSafetyK(v), true
}

// FormatSafetyK renders k for SQLite without exponent notation or trailing
// zeros, so 0.85 round-trips as "0.85".
func FormatSafetyK(k float64) string {
	return strconv.FormatFloat(ClampSafetyK(k), 'f', -1, 64)
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

// TrustFromSafetyK maps a numeric haircut scale onto the nearest trust step.
// k is the stored primitive; this is the derivation every enum answer comes
// from, so an old client that only speaks forecast_trust reads a sane step
// whatever the slider was set to.
func TrustFromSafetyK(k float64) ForecastTrust {
	switch {
	case k <= 0.25:
		return ForecastTrustBold
	case k < 1.5:
		return ForecastTrustBalanced
	default:
		return ForecastTrustCautious
	}
}

// ResolvePlannerPrefs builds the live household object from SQLite, then YAML
// (forecast_trust, or a legacy pv_forecast_safety_k), then the persisted
// control mode. missingStored is true when any SQLite key was absent so the
// caller should persist the result.
//
// safetyK is the stored primitive and wins outright when present. A site that
// only ever stored the enum resolves to that step's k, so its plan does not
// move on the upgrade; the caller then writes the float and the slider owns it
// from there. A legacy pv_forecast_safety_k seeds the float exactly — 0.7 stays
// 0.7 rather than snapping to a step — and the nearest step alongside it.
func ResolvePlannerPrefs(storedTrust, storedExport, storedK, persistedMode, yamlTrust, yamlExport string, yamlK *float64) (trust ForecastTrust, export BatteryExport, safetyK float64, missingStored bool) {
	seededK := false
	if t, ok := ParseForecastTrust(storedTrust); ok && storedTrust != "" {
		trust = t
	} else if t, ok := ParseForecastTrust(yamlTrust); ok && yamlTrust != "" {
		trust = t
		missingStored = true
	} else if yamlK != nil {
		safetyK = ClampSafetyK(*yamlK)
		trust = TrustFromSafetyK(safetyK)
		seededK = true
		missingStored = true
	} else {
		trust = ForecastTrustBalanced
		missingStored = true
	}
	if k, ok := ParseSafetyK(storedK); ok {
		safetyK = k
		trust = TrustFromSafetyK(k)
	} else {
		if !seededK {
			safetyK = trust.SafetyK()
		}
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
	return trust, export, safetyK, missingStored
}

// EffectiveSafetyK is the haircut scale the planner runs with, clamped to the
// slider's range. An explicit pv_forecast_safety_k in YAML does not win here:
// it seeds the first boot (ResolvePlannerPrefs) and nothing else — the Plan
// card's slider owns the live value, the same stored-wins contract
// forecast_trust itself has. The old precedence rendered the slider
// permanently disabled with a "config.yaml wins" note, which is a config file
// acting as a user interface. The receiver stays so call sites still ask the
// config and get the same answer.
func (p *Planner) EffectiveSafetyK(k float64) float64 {
	return ClampSafetyK(k)
}

// PlannerPrefs is the in-memory household planner object. SQLite is the
// durable copy; this is what /api/status reads on every poll. SafetyK is the
// primitive; Trust is kept in step with it via TrustFromSafetyK.
type PlannerPrefs struct {
	mu      sync.Mutex
	Trust   ForecastTrust
	Export  BatteryExport
	SafetyK float64
}

func NewPlannerPrefs(trust ForecastTrust, export BatteryExport, safetyK float64) *PlannerPrefs {
	return &PlannerPrefs{Trust: trust, Export: export, SafetyK: ClampSafetyK(safetyK)}
}

func (p *PlannerPrefs) Get() (ForecastTrust, BatteryExport, float64) {
	if p == nil {
		return ForecastTrustBalanced, BatteryExportUnknown, SafetyKDefault
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.Trust, p.Export, p.SafetyK
}

func (p *PlannerPrefs) Set(trust ForecastTrust, export BatteryExport, safetyK float64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.Trust = trust
	p.Export = export
	p.SafetyK = ClampSafetyK(safetyK)
	p.mu.Unlock()
}

func (p *PlannerPrefs) ApplyExportFromMode(mode string, save func(key, value string) error) {
	export, ok := ExportFromPlannerMode(mode)
	if !ok || p == nil {
		return
	}
	trust, _, k := p.Get()
	p.Set(trust, export, k)
	if save != nil {
		_ = save(StateKeyBatteryExport, string(export))
	}
}
