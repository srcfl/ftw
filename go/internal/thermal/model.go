// Package thermal is the vocabulary heat controls are declared in: which heat
// store an asset is, and which lever a driver offers on it. It holds no
// policy. What a planner proposes and what Core accepts are in
// docs/adr/0012-heat-control.md.
package thermal

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
	InfluenceCurveOffset  InfluenceStrategy = "curve_offset"
	InfluenceSetpoint     InfluenceStrategy = "setpoint"
	InfluenceDiscreteMode InfluenceStrategy = "discrete_mode"
	// InfluenceOnOff is a device whose only lever is start and stop.
	InfluenceOnOff InfluenceStrategy = "on_off"
)

// ValidAssetKind reports whether k is one of the declared asset kinds.
func ValidAssetKind(k AssetKind) bool {
	switch k {
	case AssetSpaceHeat, AssetCooling, AssetDHW, AssetBuffer:
		return true
	}
	return false
}
