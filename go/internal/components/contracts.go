// Package components defines compatibility contracts for independently
// released FTW modules. Core remains the safety and dispatch authority.
package components

const (
	ComponentManifestSchemaVersion = 1
	OptimizerProtocolVersion       = 1
	DriverHostAPIVersion           = 1
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
