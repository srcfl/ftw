// Package thermal defines the site-level contract for heat pumps, cooling,
// domestic hot water, and thermal buffers.
package thermal

import "math"

// AssetKind identifies which thermal store an asset represents.
type AssetKind string

const (
	AssetSpaceHeat AssetKind = "space_heat"
	AssetCooling   AssetKind = "cooling"
	AssetDHW       AssetKind = "dhw"
	AssetBuffer    AssetKind = "buffer"
)

// InfluenceStrategy describes the surface a driver can use to bias a device.
type InfluenceStrategy string

const (
	InfluenceSyntheticPrice InfluenceStrategy = "synthetic_price"
	InfluenceCurveOffset    InfluenceStrategy = "curve_offset"
	InfluenceSetpoint       InfluenceStrategy = "setpoint"
	InfluenceDiscreteMode   InfluenceStrategy = "discrete_mode"
)

// IntentKind is planner intent before the driver maps it to vendor commands.
type IntentKind string

const (
	IntentNeutral        IntentKind = "neutral"
	IntentPrecondition   IntentKind = "precondition"
	IntentShed           IntentKind = "shed"
	IntentProtectComfort IntentKind = "protect_comfort"
	IntentBoostDHW       IntentKind = "boost_dhw"
)

// TemperatureBand is a hard min/max envelope with an optional normal target.
type TemperatureBand struct {
	MinC    float64
	NormalC float64
	MaxC    float64
}

// Contains reports whether temp is within the hard envelope.
func (b TemperatureBand) Contains(tempC float64) bool {
	return finite(tempC) && tempC >= b.MinC && tempC <= b.MaxC
}

// WarmHeadroomC returns degrees available before hitting the max bound.
func (b TemperatureBand) WarmHeadroomC(tempC float64) float64 {
	if !finite(tempC) || tempC >= b.MaxC {
		return 0
	}
	return b.MaxC - tempC
}

// ShedHeadroomC returns degrees available before hitting the min bound.
func (b TemperatureBand) ShedHeadroomC(tempC float64) float64 {
	if !finite(tempC) || tempC <= b.MinC {
		return 0
	}
	return tempC - b.MinC
}

// MarginalPrice is the site-level cost signal for the next thermal kWh.
type MarginalPrice struct {
	// ImportOreKWh is consumer cost when the marginal kWh comes from grid import.
	ImportOreKWh float64
	// ExportOreKWh is opportunity cost when PV surplus would otherwise export.
	ExportOreKWh float64
	// PVSurplusW > 0 means the marginal thermal load can be supplied from PV
	// that would otherwise cross the site meter as export.
	PVSurplusW float64
	// ImportPressureOreKWh penalizes thermal load during fuse/import pressure.
	ImportPressureOreKWh float64
	// BatteryOpportunityOreKWh penalizes spending energy the battery needs later.
	BatteryOpportunityOreKWh float64
	// ThermalUrgencyCreditOreKWh rewards filling thermal storage before a
	// forecast comfort or DHW shortfall.
	ThermalUrgencyCreditOreKWh float64
}

// SyntheticOreKWh returns the price to publish to smart native controllers.
func (p MarginalPrice) SyntheticOreKWh() float64 {
	base := p.ImportOreKWh
	if p.PVSurplusW > 0 && finite(p.ExportOreKWh) {
		base = p.ExportOreKWh
	}
	out := base + p.ImportPressureOreKWh + p.BatteryOpportunityOreKWh - p.ThermalUrgencyCreditOreKWh
	if !finite(out) {
		return 0
	}
	return out
}

// DecisionInput is the minimum state needed for the first policy controller.
type DecisionInput struct {
	Kind AssetKind

	SpaceTempC *float64
	SpaceBand  TemperatureBand

	DHWTempC *float64
	DHWBand  TemperatureBand

	Price MarginalPrice

	// CheapBelowOreKWh and ExpensiveAboveOreKWh are optional thresholds.
	// nil means "unset" — a *float64 so that an explicit 0 öre/kWh
	// threshold (precondition/boost only when energy is free or
	// negative-priced) is distinguishable from "not configured". Swedish
	// spot prices go negative regularly, so 0 is a legitimate threshold.
	CheapBelowOreKWh     *float64
	ExpensiveAboveOreKWh *float64
	PVPreconditionW      float64
	AllowDHWBoost        bool
}

// Intent is a strategy-neutral instruction from planner to driver.
type Intent struct {
	Kind                  IntentKind
	SyntheticOreKWh       float64
	PreconditionHeadroomC float64
	ShedHeadroomC         float64
	DHWWarmHeadroomC      float64
	Reason                string
}

// DecideIntent implements the conservative first-pass thermal policy. MPC can
// later replace the decision engine while keeping this same driver contract.
func DecideIntent(in DecisionInput) Intent {
	price := in.Price.SyntheticOreKWh()
	intent := Intent{
		Kind:            IntentNeutral,
		SyntheticOreKWh: price,
		Reason:          "neutral",
	}

	if in.SpaceTempC != nil {
		temp := *in.SpaceTempC
		intent.PreconditionHeadroomC, intent.ShedHeadroomC = spaceHeadroom(in.Kind, in.SpaceBand, temp)
		if finite(temp) && temp < in.SpaceBand.MinC {
			intent.Kind = IntentProtectComfort
			intent.Reason = "space temperature below minimum"
			return intent
		}
		if finite(temp) && temp > in.SpaceBand.MaxC && in.Kind == AssetCooling {
			intent.Kind = IntentProtectComfort
			intent.Reason = "space temperature above maximum"
			return intent
		}
	}

	if in.DHWTempC != nil {
		temp := *in.DHWTempC
		intent.DHWWarmHeadroomC = in.DHWBand.WarmHeadroomC(temp)
		if finite(temp) && temp < in.DHWBand.MinC {
			intent.Kind = IntentProtectComfort
			intent.Reason = "dhw temperature below minimum"
			return intent
		}
	}

	cheap := in.CheapBelowOreKWh != nil && price <= *in.CheapBelowOreKWh
	expensive := in.ExpensiveAboveOreKWh != nil && price >= *in.ExpensiveAboveOreKWh
	pvSurplus := in.PVPreconditionW > 0 && in.Price.PVSurplusW >= in.PVPreconditionW

	if in.AllowDHWBoost && (cheap || pvSurplus) && intent.DHWWarmHeadroomC > 0 {
		intent.Kind = IntentBoostDHW
		intent.Reason = "dhw headroom and cheap energy"
		return intent
	}
	if (cheap || pvSurplus) && intent.PreconditionHeadroomC > 0 {
		intent.Kind = IntentPrecondition
		intent.Reason = "thermal headroom and cheap energy"
		return intent
	}
	if expensive && intent.ShedHeadroomC > 0 {
		intent.Kind = IntentShed
		intent.Reason = "expensive energy and shed headroom"
		return intent
	}

	return intent
}

func spaceHeadroom(kind AssetKind, band TemperatureBand, tempC float64) (preconditionC, shedC float64) {
	if kind == AssetCooling {
		return band.ShedHeadroomC(tempC), band.WarmHeadroomC(tempC)
	}
	return band.WarmHeadroomC(tempC), band.ShedHeadroomC(tempC)
}

func finite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}
