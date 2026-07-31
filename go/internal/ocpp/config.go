package ocpp

// Config controls the OCPP 1.6J Central System.
//
// Bind to LAN-only addresses by default — there is no TLS in Phase 1.
// Charge points connect via ws://<bind>:<port>/<chargerId>; the chargerId
// segment becomes the driver name in telemetry.Store and shows up in
// /api/devices and /api/status.drivers.
//
// NOTE: Bind is advisory-only — the ocpp-go library does not expose a
// bind-address parameter, so the listener currently binds to 0.0.0.0
// regardless of this field. See the TODO in server.go.
type Config struct {
	Enabled            bool   `yaml:"enabled"`
	Bind               string `yaml:"bind"`
	Port               int    `yaml:"port"`
	Path               string `yaml:"path"`
	Username           string `yaml:"username"`
	Password           string `yaml:"password"`
	HeartbeatIntervalS int    `yaml:"heartbeat_interval_s"`
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
