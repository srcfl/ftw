// Package components defines compatibility contracts for independently
// released FTW modules. Core remains the safety and dispatch authority.
package components

const (
	ComponentManifestSchemaVersion = 1
	OptimizerProtocolVersion       = 1
	// DriverHostAPIVersion is the newest API implemented by this host. The
	// v1 surface remains available for read-only and legacy drivers; v2 is a
	// separate, restricted control surface selected by signed package metadata.
	DriverHostAPIMinVersion = 1
	DriverHostAPIVersion    = 2
)

type Kind string

const (
	KindCore      Kind = "core"
	KindOptimizer Kind = "optimizer"
	KindDriver    Kind = "driver"
)

// CompatibleRange is inclusive. Zero means the current version when omitted
// by a v1 producer, keeping initial manifests compact.
type CompatibleRange struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

func (r CompatibleRange) Includes(version int) bool {
	min, max := r.Min, r.Max
	if min == 0 {
		min = version
	}
	if max == 0 {
		max = min
	}
	return version >= min && version <= max
}

// OverlapsDriverHost reports whether at least one API version requested by a
// driver is implemented by this host. Runtime selection still uses the exact
// signed profile; overlap alone never upgrades a v1 driver to v2.
func (r CompatibleRange) OverlapsDriverHost() bool {
	min, max := r.Min, r.Max
	if min == 0 {
		min = DriverHostAPIMinVersion
	}
	if max == 0 {
		max = min
	}
	return max >= DriverHostAPIMinVersion && min <= DriverHostAPIVersion
}
