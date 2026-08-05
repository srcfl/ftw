package ocpp

// Config controls the OCPP 1.6J Central System.
//
// Bind to LAN-only addresses by default — there is no TLS in Phase 1.
// Charge points connect via ws://<bind>:<port>/<chargerId>. When a loadpoint
// names that chargerId (see ApprovedIDs) it becomes the driver name in
// telemetry.Store and shows up in /api/devices and /api/status.drivers;
// otherwise the charger stays pending and appears only in /api/ocpp/chargers.
//
// NOTE: Bind is advisory-only — the ocpp-go library does not expose a
// bind-address parameter, so the listener currently binds to 0.0.0.0
// regardless of this field. See the TODO in server.go.
type Config struct {
	Enabled            bool   `yaml:"enabled"`
	Bind               string `yaml:"bind"`
	Port               int    `yaml:"port"`
	PortV201           int    `yaml:"port_v201"`
	Path               string `yaml:"path"`
	Username           string `yaml:"username"`
	Password           string `yaml:"password"`
	HeartbeatIntervalS int    `yaml:"heartbeat_interval_s"`

	// ApprovedIDs is the set of charger identities that are part of the site
	// — the ids loadpoints name in config. Derived by the caller, never read
	// from YAML. A charger connecting under any other identity is accepted at
	// the protocol level but quarantined as "pending": visible in the API and
	// UI so an operator can adopt it, withheld from telemetry so it cannot
	// influence dispatch. Empty means every charger is pending.
	ApprovedIDs []string `yaml:"-"`
}

// Defaults fills in any unset fields with safe values.
func (c *Config) Defaults() {
	if c.Bind == "" {
		c.Bind = "0.0.0.0"
	}
	if c.Port == 0 {
		c.Port = 8887
	}
	if c.Path == "" {
		c.Path = "/"
	}
	if c.HeartbeatIntervalS == 0 {
		c.HeartbeatIntervalS = 60
	}
}
