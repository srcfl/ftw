package evcloud

import (
	"fmt"

	"github.com/frahlg/forty-two-watts/go/internal/config"
)

// Transport identifies the wire protocol a provider speaks. Drives the
// wizard's field renderer and the runtime's connection wiring.
type Transport string

const (
	TransportHTTP   Transport = "http"
	TransportModbus Transport = "modbus"
)

// Charger is the common representation returned by all providers.
type Charger struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// Descriptor is the self-description a provider hands to the UI so the
// wizard can render the right form fields. Returned by GET /api/ev/providers.
type Descriptor struct {
	Name          string    `json:"name"`                    // canonical id, e.g. "easee"
	Label         string    `json:"label"`                   // human-facing, e.g. "Easee"
	Transport     Transport `json:"transport"`               // "http" | "modbus"
	NeedsAuth     bool      `json:"needs_auth"`              // true → wizard shows username+password
	UsernameLabel string    `json:"username_label,omitempty"` // e.g. "Email"; empty → "Username"
	DefaultPort   int       `json:"default_port,omitempty"`   // Modbus: typically 502
	DefaultUnitID int       `json:"default_unit_id,omitempty"` // Modbus: typically 1
	LuaDriver     string    `json:"lua_driver,omitempty"`     // hint for wizard: the drivers/*.lua entry to write alongside
}

// Provider can authenticate (if required) with an EV charger service
// and list the chargers reachable via the supplied EVCharger config.
type Provider interface {
	Describe() Descriptor
	ListChargers(*config.EVCharger) ([]Charger, error)
}

var providers = map[string]Provider{}

// Register adds a named provider. Call from init().
//
// Panics on nil or duplicate registration — both indicate a programming
// error (two packages claiming the same provider string would otherwise
// race on init-order and silently pick the last one).
func Register(name string, p Provider) {
	if p == nil {
		panic(fmt.Sprintf("evcloud: Register(%q): nil provider", name))
	}
	if _, exists := providers[name]; exists {
		panic(fmt.Sprintf("evcloud: Register(%q): provider already registered", name))
	}
	providers[name] = p
}

// Get returns the provider for the given name or an error.
func Get(name string) (Provider, error) {
	p, ok := providers[name]
	if !ok {
		return nil, fmt.Errorf("unknown ev provider: %q", name)
	}
	return p, nil
}

// Names returns all registered provider names in arbitrary order.
func Names() []string {
	out := make([]string, 0, len(providers))
	for k := range providers {
		out = append(out, k)
	}
	return out
}

// Describe returns the descriptor for every registered provider. Order
// is unspecified — the UI sorts by Label if it cares.
func Describe() []Descriptor {
	out := make([]Descriptor, 0, len(providers))
	for _, p := range providers {
		out = append(out, p.Describe())
	}
	return out
}
