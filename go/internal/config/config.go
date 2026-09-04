// Package config parses and validates the top-level YAML config.
//
// This is the single source of truth that the file-watcher re-parses on
// every change and that the settings UI writes back. All fields are
// hot-reloadable unless noted otherwise.
package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/optimizercontract"
	"gopkg.in/yaml.v3"
)

// Config is the full application config.
type Config struct {
	Site             Site               `yaml:"site" json:"site"`
	Fuse             Fuse               `yaml:"fuse" json:"fuse"`
	Drivers          []Driver           `yaml:"drivers" json:"drivers"`
	API              API                `yaml:"api" json:"api"`
	HomeAssistant    *HomeAssistant     `yaml:"homeassistant,omitempty" json:"homeassistant,omitempty"`
	State            *StateConf         `yaml:"state,omitempty" json:"state,omitempty"`
	Price            *Price             `yaml:"price,omitempty" json:"price,omitempty"`
	Weather          *Weather           `yaml:"weather,omitempty" json:"weather,omitempty"`
	Planner          *Planner           `yaml:"planner,omitempty" json:"planner,omitempty"`
	Batteries        map[string]Battery `yaml:"batteries,omitempty" json:"batteries,omitempty"`
	EVCharger        *EVCharger         `yaml:"ev_charger,omitempty" json:"ev_charger,omitempty"`
	CalDAV           *CalDAV            `yaml:"caldav,omitempty" json:"caldav,omitempty"`
	Loadpoints       []Loadpoint        `yaml:"loadpoints,omitempty" json:"loadpoints,omitempty"`
	Vehicles         []Vehicle          `yaml:"vehicles,omitempty" json:"vehicles,omitempty"`
	V2X              *V2XPolicy         `yaml:"v2x,omitempty" json:"v2x,omitempty"`
	Notifications    *Notifications     `yaml:"notifications,omitempty" json:"notifications,omitempty"`
	AppLink          *AppLink           `yaml:"app_link,omitempty" json:"app_link,omitempty"`
	FleetPing        *FleetPing         `yaml:"fleet_ping,omitempty" json:"fleet_ping,omitempty"`
	Nova             *Nova              `yaml:"nova,omitempty" json:"nova,omitempty"`
	DeviceRepository *DeviceRepository  `yaml:"device_repository,omitempty" json:"device_repository,omitempty"`
	OCPP             *OCPP              `yaml:"ocpp,omitempty" json:"ocpp,omitempty"`
	Assistant        *Assistant         `yaml:"assistant,omitempty" json:"assistant,omitempty"`

	// LoadWarnings collects recoverable problems Parse repaired instead of
	// refusing the file: an on-disk config an older version accepted must
	// still boot, or the operator loses the UI they would fix it with. The
	// write path (Settings save, bootstrap) never populates this — it calls
	// Validate directly and stays strict. Never serialized.
	LoadWarnings []string `yaml:"-" json:"-"`
}

// OCPP configures the built-in OCPP 1.6J and 2.0.1 Central System. Chargers connect to
// us, so there is no driver and no per-charger config entry — a charge point
// appears as a device the moment it sends its first BootNotification, keyed by
// the identity segment of the URL it dialled.
//
// Disabled by default, and enabling it requires credentials. The socket cannot
// be restricted to one interface: the OCPP library builds its own listen
// address from the port alone, so it is open on every interface the host has.
// Bind therefore refuses the handshake for a connection that arrived
// elsewhere, which controls access without shrinking the attack surface — an
// empty Username or Password is still rejected rather than silently accepted.
type OCPP struct {
	Enabled bool `yaml:"enabled" json:"enabled"`

	// Bind is the address chargers are served on. Empty or 0.0.0.0 accepts
	// every interface, which is the default and what a flat home LAN wants.
	// Set it to one LAN address to refuse chargers reaching the box any
	// other way — over a VPN interface, say, or a second NIC.
	Bind string `yaml:"bind,omitempty" json:"bind,omitempty"`

	Port               int    `yaml:"port,omitempty" json:"port,omitempty"`
	PortV201           int    `yaml:"port_v201,omitempty" json:"port_v201,omitempty"`
	Path               string `yaml:"path,omitempty" json:"path,omitempty"`
	Username           string `yaml:"username,omitempty" json:"username,omitempty"`
	Password           string `yaml:"password,omitempty" json:"password,omitempty"`
	HeartbeatIntervalS int    `yaml:"heartbeat_interval_s,omitempty" json:"heartbeat_interval_s,omitempty"`

	// TLS serves wss:// instead of ws://. Optional, and worth the
	// certificate management on any site where the charger and the box are
	// not on the same trusted wire.
	TLS *OCPPTLS `yaml:"tls,omitempty" json:"tls,omitempty"`

	// Chargers gives named charge points a credential of their own, so the
	// shared password stops being enough to claim their identity. Optional
	// and per charger: anything not listed keeps using Username/Password.
	Chargers []OCPPCharger `yaml:"chargers,omitempty" json:"chargers,omitempty"`
}

// OCPPTLS points at the certificate the OCPP listener presents, and optionally
// at the CA that signs the charge points allowed to connect.
type OCPPTLS struct {
	CertFile string `yaml:"cert_file,omitempty" json:"cert_file,omitempty"`
	KeyFile  string `yaml:"key_file,omitempty" json:"key_file,omitempty"`

	// ClientCAFile turns on mutual TLS: every charge point must present a
	// certificate signed by this CA. That is OCPP 2.0.1 security profile 3,
	// and the only identity here that cannot be copied out of one charger's
	// configuration and replayed by another device.
	ClientCAFile string `yaml:"client_ca_file,omitempty" json:"client_ca_file,omitempty"`
}

// OCPPCharger is one charge point's own credential.
//
// The ID is the identity the charger dials with — the last segment of its URL,
// and the same string a loadpoint's driver_name uses to adopt it. On OCPP the
// basic-auth username is that identity, so a charger listed here must present
// both, and a device holding only the shared password can no longer connect
// under its name.
type OCPPCharger struct {
	ID       string `yaml:"id" json:"id"`
	Password string `yaml:"password,omitempty" json:"password,omitempty"`
}

// Validate rejects an enabled server that would accept anonymous charge
// points. A nil or disabled section is fine — OCPP is opt-in.
func (o *OCPP) Validate() error {
	if o == nil || !o.Enabled {
		return nil
	}
	if o.Username == "" || o.Password == "" {
		return errors.New("ocpp: username and password are required when enabled, because the listener cannot be bound to a single interface")
	}
	if o.Port < 0 || o.Port > 65535 {
		return fmt.Errorf("ocpp.port must be between 0 and 65535, got %d", o.Port)
	}
	if o.PortV201 < 0 || o.PortV201 > 65535 {
		return fmt.Errorf("ocpp.port_v201 must be between 0 and 65535, got %d", o.PortV201)
	}
	// Each version needs its own listener, so they cannot share a port.
	if o.PortV201 > 0 && o.PortV201 == o.Port {
		return fmt.Errorf("ocpp.port_v201 must differ from ocpp.port, both are %d", o.Port)
	}
	if o.HeartbeatIntervalS < 0 {
		return fmt.Errorf("ocpp.heartbeat_interval_s must be >= 0, got %d", o.HeartbeatIntervalS)
	}
	// A bind address that does not parse would silently fall back to "every
	// interface" — the opposite of what the operator asked for.
	if o.Bind != "" && net.ParseIP(o.Bind) == nil {
		return fmt.Errorf("ocpp.bind must be an IP address, got %q", o.Bind)
	}
	if err := o.TLS.validate(); err != nil {
		return err
	}
	seen := make(map[string]bool, len(o.Chargers))
	for i, c := range o.Chargers {
		if c.ID == "" {
			return fmt.Errorf("ocpp.chargers[%d].id is required", i)
		}
		if seen[c.ID] {
			return fmt.Errorf("ocpp.chargers has two entries for %q", c.ID)
		}
		seen[c.ID] = true
		// An entry with no password would quietly fall back to the shared
		// one, leaving the operator believing this charger was pinned to a
		// credential of its own.
		if c.Password == "" {
			return fmt.Errorf("ocpp.chargers[%d] (%s) needs a password; remove the entry to use the shared one", i, c.ID)
		}
	}
	return nil
}

// validate checks the TLS section without touching the filesystem — the paths
// are read when the listener starts, which is where a missing file is
// reported. A nil section means plaintext ws://, which is the default.
func (t *OCPPTLS) validate() error {
	if t == nil {
		return nil
	}
	if t.CertFile == "" && t.KeyFile == "" {
		if t.ClientCAFile != "" {
			return errors.New("ocpp.tls.client_ca_file needs cert_file and key_file: client certificates are only verified on a TLS listener")
		}
		return nil
	}
	if t.CertFile == "" || t.KeyFile == "" {
		return errors.New("ocpp.tls needs both cert_file and key_file")
	}
	return nil
}

// ChargerSecrets is the per-charger credential map, keyed by charge point
// identity. Empty when none are configured.
func (o *OCPP) ChargerSecrets() map[string]string {
	if o == nil || len(o.Chargers) == 0 {
		return nil
	}
	out := make(map[string]string, len(o.Chargers))
	for _, c := range o.Chargers {
		if c.ID != "" && c.Password != "" {
			out[c.ID] = c.Password
		}
	}
	return out
}

// AppLink controls the outbound connection the FTW app reaches this box
// through. It defaults on when the section is absent; an explicit false is the
// opt-out. One switch is enough because the relay is content-blind and fixed
// by the protocol, so there is no endpoint or transport to choose.
type AppLink struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
}

// On is the single reading of the app-link switch. A nil section means an old
// config that has never made the choice, so it follows the default. An empty
// section has Enabled's false zero value and remains an opt-out.
func (a *AppLink) On() bool {
	if a == nil {
		return true
	}
	return a.Enabled
}

// FleetPing configures the once-a-day count of how many boxes run FTW, on
// which version and with which drivers. On by default, because the numbers
// are what decide where engineering effort goes and a fleet nobody can see is
// a fleet guessed about instead.
//
// It can be on by default because what it carries is a shape rather than an
// identity: no id, no key, no counter and no timestamp. Not because it is
// beyond reproach — the fields still describe a household and the endpoint
// sees the source IP. Settings renders the exact payload and says both, so
// this can be weighed rather than believed. See go/internal/fleetping.
type FleetPing struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Endpoint is where the ping goes. Configurable so a deployment can point
	// it at itself or at nothing, and because a hard-coded address in a
	// package that sends data out is worth being able to see and change.
	Endpoint string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
}

// DefaultFleetPingEndpoint is the fleet-report door on FTW's relay. The relay
// adds the report to daily totals and discards the request; a failed ping is
// forgotten, never retried.
const DefaultFleetPingEndpoint = "https://relay.ftw.energy/fleet"

// On reports whether the ping is switched on, and is the only place that
// answers it.
//
// Nil means never configured, which is the state every existing box is in, and
// applyDefaults reads it as on. Config posted to /api/config does not pass
// through applyDefaults — configreload.Apply swaps it in as it arrives — so a
// second reading of nil here would silently stop the ping the first time a
// household saved settings from a screen that never sent the section.
func (f *FleetPing) On() bool {
	if f == nil {
		return true
	}
	return f.Enabled
}

// Resolved is where the ping goes.
//
// The fallback lives here rather than in applyDefaults, so the address is never
// written into a household's config.yaml. A baked-in copy would keep posting to
// this address after FTW moved the collector, and the aggregate would
// quietly lose every box that had ever saved its settings.
func (f *FleetPing) Resolved() string {
	if f == nil || f.Endpoint == "" {
		return DefaultFleetPingEndpoint
	}
	return f.Endpoint
}

// Validate rejects an endpoint that would undo what the payload is careful
// about. Nil-safe: an absent section is a valid one.
func (f *FleetPing) Validate() error {
	if f == nil || f.Endpoint == "" {
		return nil
	}
	return ValidateFleetPingEndpoint(f.Endpoint)
}

// ValidateFleetPingEndpoint refuses anything that is not a plain HTTPS URL.
//
// Plain HTTP would put the site's shape in the clear for every network between
// the house and the relay — a worse leak than the ones the payload is designed
// around. Credentials, a query and a fragment are refused because none of them
// addresses a collector: a URL carrying one is far more likely a mistake than a
// choice, and credentials in a config field are a mistake whatever they are for.
// The path is not policed, because a path is how a collector names its own
// endpoint. None of this makes the address secret — Settings shows the endpoint
// this box will post to, whatever it is.
func ValidateFleetPingEndpoint(endpoint string) error {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return fmt.Errorf("fleet_ping.endpoint %q is not a URL", endpoint)
	}
	if u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("fleet_ping.endpoint must be an https URL, got %q", endpoint)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("fleet_ping.endpoint must carry no credentials, query or fragment")
	}
	return nil
}

// DeviceRepository configures independently distributed Lua drivers. Remote
// refresh never changes an active driver; activation is always an explicit API
// action. TrustedKeys maps key IDs to base64-encoded Ed25519 public keys.
type DeviceRepository struct {
	Enabled          bool                     `yaml:"enabled" json:"enabled"`
	RefreshIntervalH int                      `yaml:"refresh_interval_h,omitempty" json:"refresh_interval_h,omitempty"`
	RootDir          string                   `yaml:"root_dir,omitempty" json:"root_dir,omitempty"`
	Repositories     []DriverRepositorySource `yaml:"repositories,omitempty" json:"repositories,omitempty"`
}

type DriverRepositorySource struct {
	ID            string            `yaml:"id" json:"id"`
	Name          string            `yaml:"name,omitempty" json:"name,omitempty"`
	Format        string            `yaml:"format,omitempty" json:"format,omitempty"`
	ManifestURL   string            `yaml:"manifest_url" json:"manifest_url"`
	Enabled       bool              `yaml:"enabled" json:"enabled"`
	TrustedKeys   map[string]string `yaml:"trusted_keys,omitempty" json:"trusted_keys,omitempty"`
	AllowUnsigned bool              `yaml:"allow_unsigned,omitempty" json:"allow_unsigned,omitempty"`
	AllowInsecure bool              `yaml:"allow_insecure,omitempty" json:"allow_insecure,omitempty"`
}

const (
	DriverRepositoryFormatFTWManifestV1    = "ftw.manifest/v1"
	DriverRepositoryFormatSourcefulIndexV1 = "sourceful.driver-index/v1"

	DefaultDriverRepositoryID              = "ftw-official"
	DefaultDriverRepositoryName            = "FTW device drivers"
	DefaultDriverRepositoryManifestURL     = "https://github.com/srcfl/device-drivers/releases/download/drivers-stable/manifest.json"
	DefaultDriverRepositoryBetaID          = "ftw-official-beta"
	DefaultDriverRepositoryBetaName        = "FTW device drivers beta"
	DefaultDriverRepositoryBetaManifestURL = "https://github.com/srcfl/device-drivers/releases/download/drivers-beta/manifest.json"
	DefaultDriverRepositorySigningKeyID    = "ftw-drivers-2026-01"
	DefaultDriverRepositoryPublicKey       = "MX+j27UBkyM099hTyJlmMLK9qlTTDUJsaK/vH12fFKc="

	legacyDriverRepositoryName        = "FTW official drivers"
	legacyDriverRepositoryManifestURL = "https://github.com/srcfl/ftw/releases/download/drivers-stable/manifest.json"
)

// Assistant is the optional Ask why helper: a user-supplied OpenRouter
// (or OpenAI-compatible) key used to explain the local help report.
// Nil or disabled is the unavailable state. The helper never issues
// driver commands or writes config.
type Assistant struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	APIKey  string `yaml:"api_key,omitempty" json:"api_key,omitempty"`
	// Model is an OpenRouter model id. Empty means openrouter/free.
	Model string `yaml:"model,omitempty" json:"model,omitempty"`
	// BaseURL is the OpenAI-compatible root (no /chat/completions suffix).
	// Empty means https://openrouter.ai/api/v1. A later Sourceful proxy
	// can set this without a code change.
	BaseURL string `yaml:"base_url,omitempty" json:"base_url,omitempty"`

	// HasAPIKey is JSON-only: true when a key exists on disk. Set by
	// MaskSecrets before APIKey is blanked so Settings can render
	// "configured — hidden". Never written to YAML.
	HasAPIKey bool `yaml:"-" json:"has_api_key,omitempty"`
}

const (
	DefaultAssistantModel   = "openrouter/free"
	DefaultAssistantBaseURL = "https://openrouter.ai/api/v1"
)

// Ready is true when Ask why can make an outbound call.
func (a *Assistant) Ready() bool {
	return a != nil && a.Enabled && strings.TrimSpace(a.APIKey) != ""
}

// ResolvedModel returns the configured model or the free-router default.
func (a *Assistant) ResolvedModel() string {
	if a == nil || strings.TrimSpace(a.Model) == "" {
		return DefaultAssistantModel
	}
	return strings.TrimSpace(a.Model)
}

// ResolvedBaseURL returns the OpenAI-compatible root without a trailing slash.
func (a *Assistant) ResolvedBaseURL() string {
	if a == nil || strings.TrimSpace(a.BaseURL) == "" {
		return DefaultAssistantBaseURL
	}
	return strings.TrimRight(strings.TrimSpace(a.BaseURL), "/")
}

// Validate checks optional assistant settings. A missing key is allowed:
// the Ask why endpoint reports that as unavailable rather than refusing
// the whole config.
func (a *Assistant) Validate() error {
	if a == nil {
		return nil
	}
	if strings.TrimSpace(a.Model) != "" && len(a.Model) > 120 {
		return errors.New("assistant.model is too long")
	}
	if strings.TrimSpace(a.BaseURL) == "" {
		return nil
	}
	u, err := url.Parse(strings.TrimSpace(a.BaseURL))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("assistant.base_url must be an http(s) URL")
	}
	if u.User != nil {
		return errors.New("assistant.base_url must carry no credentials")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return errors.New("assistant.base_url must carry no query or fragment")
	}
	return nil
}

// Notifications configures outbound push notifications. Exactly one
// transport provider is active at a time, selected by Provider. Today
// the only implemented provider is "ntfy" (ntfy.sh or self-hosted);
// future providers add their own nested config block and register in
// go/internal/notifications.
type Notifications struct {
	Enabled         bool               `yaml:"enabled" json:"enabled"`
	Provider        string             `yaml:"provider,omitempty" json:"provider,omitempty"`
	DefaultPriority int                `yaml:"default_priority,omitempty" json:"default_priority,omitempty"`
	Ntfy            *NtfyConfig        `yaml:"ntfy,omitempty" json:"ntfy,omitempty"`
	Events          []NotificationRule `yaml:"events,omitempty" json:"events,omitempty"`
}

// NtfyConfig is the ntfy.sh transport settings.
type NtfyConfig struct {
	Server      string `yaml:"server,omitempty" json:"server,omitempty"`
	Topic       string `yaml:"topic,omitempty" json:"topic,omitempty"`
	AccessToken string `yaml:"access_token,omitempty" json:"access_token,omitempty"`
	Username    string `yaml:"username,omitempty" json:"username,omitempty"`
	Password    string `yaml:"password,omitempty" json:"password,omitempty"`

	// HasAccessToken is a JSON-only signal for the UI: true means a
	// token exists on disk. Set by MaskSecrets before AccessToken is
	// blanked so the Settings form can render "configured — hidden"
	// instead of an empty input. Never written to YAML.
	HasAccessToken bool `yaml:"-" json:"has_access_token,omitempty"`
}

// NotificationRule is one event type the operator can toggle.
type NotificationRule struct {
	Type       string `yaml:"type" json:"type"`
	Enabled    bool   `yaml:"enabled" json:"enabled"`
	ThresholdS int    `yaml:"threshold_s,omitempty" json:"threshold_s,omitempty"`
	// ThresholdN is a count-based threshold used by event types that
	// aggregate across drivers (concurrent_drivers_offline). Ignored
	// by per-driver events. Default behaviour per event documented
	// alongside the const in notifications/service.go.
	ThresholdN    int    `yaml:"threshold_n,omitempty" json:"threshold_n,omitempty"`
	Priority      int    `yaml:"priority,omitempty" json:"priority,omitempty"`
	Tags          string `yaml:"tags,omitempty" json:"tags,omitempty"`
	TitleTemplate string `yaml:"title_template,omitempty" json:"title_template,omitempty"`
	BodyTemplate  string `yaml:"body_template,omitempty" json:"body_template,omitempty"`
	CooldownS     int    `yaml:"cooldown_s,omitempty" json:"cooldown_s,omitempty"`
}

// Nova is the opt-in Sourceful Nova Core federation config. When enabled,
// FTW publishes telemetry to Nova's MQTT broker (NATS MQTT
// adapter) and reconciles device/DER registrations via Nova's core-api.
//
// Identity is an ES256 keypair generated on first run and stored at
// KeyPath (default <state.path sibling>/nova.key). The public key is
// registered in Nova via the claim flow; the private key signs a short-
// lived JWT used as the MQTT password.
//
// SchemaMode controls the wire format sent to Nova:
//   - "legacy"  (default): translate FTW's native clean payload
//     to the current Nova wire shape (battery sign flip,
//     PascalCase fields, pv→solar, ev→ev_port). The translation
//     layer is in internal/nova and is designed to be deleted
//     once Nova adopts the unified schema.
//   - "unified": publish FTW's clean payload directly. Enable
//     once the Nova schema-alignment PR lands.
type Nova struct {
	Enabled            bool   `yaml:"enabled" json:"enabled"`
	URL                string `yaml:"url" json:"url"`
	MQTTHost           string `yaml:"mqtt_host" json:"mqtt_host"`
	MQTTPort           int    `yaml:"mqtt_port,omitempty" json:"mqtt_port,omitempty"`
	MQTTTLS            bool   `yaml:"mqtt_tls,omitempty" json:"mqtt_tls,omitempty"`
	GatewaySerial      string `yaml:"gateway_serial" json:"gateway_serial"`
	OrgID              string `yaml:"org_id" json:"org_id"`
	SiteID             string `yaml:"site_id" json:"site_id"`
	KeyPath            string `yaml:"key_path,omitempty" json:"key_path,omitempty"`
	SchemaMode         string `yaml:"schema_mode,omitempty" json:"schema_mode,omitempty"`
	PublishIntervalS   int    `yaml:"publish_interval_s,omitempty" json:"publish_interval_s,omitempty"`
	ReconcileIntervalH int    `yaml:"reconcile_interval_h,omitempty" json:"reconcile_interval_h,omitempty"`
}

// Loadpoint is one EV charge point the planner can reason about.
// The planner and go/internal/loadpoint optimize battery + EV jointly.
type Loadpoint struct {
	ID            string    `yaml:"id" json:"id"`
	DriverName    string    `yaml:"driver_name" json:"driver_name"`
	MinChargeW    float64   `yaml:"min_charge_w,omitempty" json:"min_charge_w,omitempty"`
	MaxChargeW    float64   `yaml:"max_charge_w,omitempty" json:"max_charge_w,omitempty"`
	AllowedStepsW []float64 `yaml:"allowed_steps_w,omitempty" json:"allowed_steps_w,omitempty"`
	// VehicleCapacityWh is the usable battery capacity of ONE vehicle — the
	// car this charger usually serves. It feeds the SoC estimate and the
	// planner's energy sizing; charging works without it, and a wrong value
	// costs planning accuracy, never safety. When several cars share the
	// charger, add Vehicles profiles: a charging session that identifies
	// the car (RFID idTag on 1.6, MacAddress/eMAID idToken on 2.0.1)
	// switches the loadpoint to that car's capacity and policy for the
	// session. A session matching no profile leaves this value in charge —
	// the visitor default.
	VehicleCapacityWh float64 `yaml:"vehicle_capacity_wh,omitempty" json:"vehicle_capacity_wh,omitempty"`
	PluginSoC         float64 `yaml:"plugin_soc,omitempty" json:"plugin_soc,omitempty"`
	PluginSoCPct      float64 `yaml:"plugin_soc_pct,omitempty" json:"plugin_soc_pct,omitempty"`

	// PhaseMode selects how the controller picks between 1Φ and 3Φ
	// delivery: "3p" (default) | "1p" | "auto". Empty == "3p" for
	// backward compat with pre-switching configs. See loadpoint.Config.
	PhaseMode     string  `yaml:"phase_mode,omitempty" json:"phase_mode,omitempty"`
	PhaseSplitW   float64 `yaml:"phase_split_w,omitempty" json:"phase_split_w,omitempty"`
	MinPhaseHoldS int     `yaml:"min_phase_hold_s,omitempty" json:"min_phase_hold_s,omitempty"`
	SurplusOnly   bool    `yaml:"surplus_only,omitempty" json:"surplus_only,omitempty"`
}

// Vehicle is a car profile the loadpoint can switch to when a charging
// session identifies the vehicle. Identification comes from the OCPP
// transaction: the RFID idTag on 1.6 (names the card — works only if the
// card lives in the car), a MacAddress (autocharge) or eMAID (ISO 15118
// Plug & Charge) idToken on 2.0.1 (names the actual vehicle). A session
// whose identity matches no profile leaves the loadpoint's own settings
// untouched — that is the visitor default. Tracked upstream in issue #835.
type Vehicle struct {
	// ID is the stable slug other config and logs refer to.
	ID string `yaml:"id" json:"id"`
	// Name is the human label the UI shows; falls back to ID.
	Name string `yaml:"name,omitempty" json:"name,omitempty"`
	// CapacityWh is this car's usable battery capacity. Applied to the
	// loadpoint for the session so SoC estimation and planner energy
	// sizing follow the car actually plugged in. 0 = leave unchanged.
	CapacityWh float64 `yaml:"capacity_wh,omitempty" json:"capacity_wh,omitempty"`
	// Identifiers are the identity strings that mean "this car": RFID tag
	// uids, MAC addresses, eMAIDs. Compared case-insensitively, trimmed.
	Identifiers []string `yaml:"identifiers,omitempty" json:"identifiers,omitempty"`
	// SurplusOnly, when the car is identified, sets the loadpoint's
	// PV-surplus-only flag to exactly this value — charge this car from
	// surplus PV alone (true) or allow grid charging (false).
	SurplusOnly bool `yaml:"surplus_only,omitempty" json:"surplus_only,omitempty"`
	// TargetSoC > 0 sets a charge target for the session (0–1), which is
	// what hands the loadpoint to the planner: it fills toward the target
	// in the cheapest tariff slots. 0 = no target, loadpoint keeps its own.
	TargetSoC    float64 `yaml:"target_soc,omitempty" json:"target_soc,omitempty"`
	TargetSoCPct float64 `yaml:"target_soc_pct,omitempty" json:"target_soc_pct,omitempty"`
}

// VehicleByIdentifier finds the vehicle profile claiming an identity string,
// or nil. Matching is case-insensitive on trimmed identifiers, so an eMAID
// or MAC compares the way the wire formats vary.
func (c *Config) VehicleByIdentifier(identifier string) *Vehicle {
	want := strings.ToLower(strings.TrimSpace(identifier))
	if want == "" {
		return nil
	}
	for i := range c.Vehicles {
		for _, id := range c.Vehicles[i].Identifiers {
			if strings.ToLower(strings.TrimSpace(id)) == want {
				return &c.Vehicles[i]
			}
		}
	}
	return nil
}

// validateVehicles keeps vehicle profiles unambiguous: unique ids, sane
// numbers, and no identifier claimed by two cars — the identify-then-apply
// path must never have to guess which profile wins.
func (c *Config) validateVehicles() error {
	ids := make(map[string]bool, len(c.Vehicles))
	claimed := make(map[string]string, len(c.Vehicles))
	for _, v := range c.Vehicles {
		if v.ID == "" {
			return errors.New("vehicle: id is required")
		}
		if ids[v.ID] {
			return fmt.Errorf("vehicle %q: duplicate id", v.ID)
		}
		ids[v.ID] = true
		if v.CapacityWh < 0 {
			return fmt.Errorf("vehicle %q: capacity_wh must be >= 0", v.ID)
		}
		if v.TargetSoC < 0 || v.TargetSoC > 1 {
			return fmt.Errorf("vehicle %q: target_soc must be within 0..1", v.ID)
		}
		for _, ident := range v.Identifiers {
			key := strings.ToLower(strings.TrimSpace(ident))
			if key == "" {
				return fmt.Errorf("vehicle %q: empty identifier", v.ID)
			}
			if owner, dup := claimed[key]; dup {
				return fmt.Errorf("vehicle %q: identifier %q already claimed by vehicle %q", v.ID, ident, owner)
			}
			claimed[key] = v.ID
		}
	}
	return nil
}

// V2XPolicy is the opt-in policy envelope for automatic V2X use. The
// current V2X pilot still dispatches only manual operator commands; this
// config lets the API expose "what would be safe right now?" before the
// planner is allowed to consume V2X as a dispatchable asset.
type V2XPolicy struct {
	Enabled bool `yaml:"enabled" json:"enabled"`

	// DriverName, when set, scopes the policy to one configured V2X driver.
	// Empty means the same policy applies to every V2X driver.
	DriverName string `yaml:"driver_name,omitempty" json:"driver_name,omitempty"`

	// VehicleCapacityWh is optional if the charger reports capacity, but is
	// required for reserve/departure energy math when the driver does not.
	VehicleCapacityWh float64 `yaml:"vehicle_capacity_wh,omitempty" json:"vehicle_capacity_wh,omitempty"`

	MinReserveSoC         float64 `yaml:"min_reserve_soc,omitempty" json:"min_reserve_soc,omitempty"`
	DepartureTargetSoC    float64 `yaml:"departure_target_soc,omitempty" json:"departure_target_soc,omitempty"`
	MinReserveSoCPct      float64 `yaml:"min_reserve_soc_pct,omitempty" json:"min_reserve_soc_pct,omitempty"`
	DepartureTargetSoCPct float64 `yaml:"departure_target_soc_pct,omitempty" json:"departure_target_soc_pct,omitempty"`

	// DepartureTime is either "HH:MM" local time (next occurrence) or RFC3339.
	DepartureTime string `yaml:"departure_time,omitempty" json:"departure_time,omitempty"`

	MaxChargeW    float64 `yaml:"max_charge_w,omitempty" json:"max_charge_w,omitempty"`
	MaxDischargeW float64 `yaml:"max_discharge_w,omitempty" json:"max_discharge_w,omitempty"`

	ExportAllowed       bool    `yaml:"export_allowed" json:"export_allowed"`
	GridChargingAllowed bool    `yaml:"grid_charging_allowed" json:"grid_charging_allowed"`
	CycleCostOreKWh     float64 `yaml:"cycle_cost_ore_kwh,omitempty" json:"cycle_cost_ore_kwh,omitempty"`
}

// EVCharger is the high-level EV charger config written by the Settings UI.
// Exactly one transport block (HTTP or Modbus) is meaningful per provider —
// the runtime picks which to populate based on the provider's declared
// transport in evcloud.Provider.
//
// Password is stored in state.db (key "ev_charger_password"), NOT in config.yaml.
// It is populated at runtime by main.go after loading state and by the API
// handler on POST /api/config. Providers that don't need auth (e.g. local
// Modbus) leave Username + Password empty.
type EVCharger struct {
	Provider string `yaml:"provider" json:"provider"` // "easee" | "zaptec" | "tesla-wc" | "ctek"

	// Connection — populate the block matching the provider's transport.
	HTTP   *EVChargerHTTP   `yaml:"http,omitempty" json:"http,omitempty"`
	Modbus *EVChargerModbus `yaml:"modbus,omitempty" json:"modbus,omitempty"`

	// Optional auth — required by cloud HTTP providers like Easee and
	// Zaptec, unused by local providers (CTEK Modbus, Tesla Wall Connector).
	Username string `yaml:"username,omitempty" json:"username,omitempty"`
	Password string `yaml:"-" json:"password,omitempty"` // persisted in state.db, not YAML

	Serial string `yaml:"serial,omitempty" json:"serial,omitempty"`

	// EmailLegacy preserves backward compatibility with the original
	// `email:` field. Normalize() copies it into Username if Username
	// is empty, so configs written before the generalization still load.
	// New code should always read Username.
	EmailLegacy string `yaml:"email,omitempty" json:"email,omitempty"`
}

// EVChargerHTTP is the HTTP/cloud connection block. BaseURL is optional —
// when empty the provider uses its default (e.g. https://api.easee.com/api).
type EVChargerHTTP struct {
	BaseURL string `yaml:"base_url,omitempty" json:"base_url,omitempty"`
}

// EVChargerModbus is the Modbus/TCP connection block. Port defaults to 502
// and UnitID defaults to 1 if zero — see provider-specific Validate.
type EVChargerModbus struct {
	Host   string `yaml:"host" json:"host"`
	Port   int    `yaml:"port,omitempty" json:"port,omitempty"`
	UnitID int    `yaml:"unit_id,omitempty" json:"unit_id,omitempty"`
}

// CalDAV configures the calendar-constraints feature (issue #498). FTW hosts
// its own in-process, pure-Go CalDAV server (emersion/go-webdav, MIT — see
// internal/caldavserver) and runs a CalDAV *client* against it that polls the
// calendar collection and maps events into planner intents:
//
//   - an "away"/vacation event switches the load model to its away profile
//     for the interval, so the planner conserves battery while the house is
//     empty;
//   - an EV "charged-by-departure" event sets the matching loadpoint's
//     target SoC + deadline, which the MPC already honours.
//
// Events are classified by case-insensitive keyword match on the event
// title (SUMMARY). Keyword lists are configurable so non-English calendars
// work. The whole feature is opt-in (Enabled) and fail-soft: an unreachable
// server never blocks control.
//
// Password is stored in state.db (key "caldav_password"), NOT in config.yaml,
// mirroring EVCharger.Password.
type CalDAV struct {
	Enabled bool `yaml:"enabled" json:"enabled"`

	// URL is the base URL of the CalDAV server. Defaults to the in-process
	// native server at http://localhost:5232.
	URL string `yaml:"url,omitempty" json:"url,omitempty"`

	Username string `yaml:"username,omitempty" json:"username,omitempty"`
	Password string `yaml:"-" json:"password,omitempty"` // persisted in state.db, not YAML

	// CalendarPath is the collection path polled for events, relative to URL
	// (e.g. "/ftw/energy/" for new configs). The runtime fallback below keeps
	// the former path for configs that omitted this field before the rebrand.
	CalendarPath string `yaml:"calendar_path,omitempty" json:"calendar_path,omitempty"`

	// PollIntervalS is how often the collection is re-fetched. Default 300s.
	PollIntervalS int `yaml:"poll_interval_s,omitempty" json:"poll_interval_s,omitempty"`

	// HorizonDays bounds the calendar-query time range (recurrences are
	// expanded server-side within it). Default 7.
	HorizonDays int `yaml:"horizon_days,omitempty" json:"horizon_days,omitempty"`

	// EVLoadpointID is the loadpoint an EV event targets when the title
	// names no specific one. Empty = the first/only configured loadpoint.
	EVLoadpointID string `yaml:"ev_loadpoint_id,omitempty" json:"ev_loadpoint_id,omitempty"`

	EVDefaultTargetSoC    float64 `yaml:"ev_default_target_soc,omitempty" json:"ev_default_target_soc,omitempty"`
	EVDefaultTargetSoCPct float64 `yaml:"ev_default_target_soc_pct,omitempty" json:"ev_default_target_soc_pct,omitempty"`

	// AwayKeywords / EVKeywords classify an event by its title. Matching is
	// case-insensitive substring. Empty lists fall back to the built-in
	// defaults (see DefaultAwayKeywords / DefaultEVKeywords).
	AwayKeywords []string `yaml:"away_keywords,omitempty" json:"away_keywords,omitempty"`
	EVKeywords   []string `yaml:"ev_keywords,omitempty" json:"ev_keywords,omitempty"`

	// EVSEHistory (default ON when enabled) makes FTW *write* a calendar
	// event for each completed EV charging session into HistoryPath. This is
	// an outbound capability — the user subscribes to HistoryPath to see when
	// the charger was used. HistoryPath MUST differ from CalendarPath so FTW
	// never re-reads its own history events as inbound intents.
	EVSEHistory *bool  `yaml:"evse_history,omitempty" json:"evse_history,omitempty"`
	HistoryPath string `yaml:"history_path,omitempty" json:"history_path,omitempty"`

	// PublishPlan (default ON when enabled) makes FTW write its forward-looking
	// plan — upcoming battery charge/discharge windows from the MPC — as
	// read-only events into PlanPath (a SEPARATE collection), so you can see
	// what FTW intends to do. Reconciled each publish so stale events are
	// removed rather than piling up.
	PublishPlan          *bool  `yaml:"publish_plan,omitempty" json:"publish_plan,omitempty"`
	PlanPath             string `yaml:"plan_path,omitempty" json:"plan_path,omitempty"`
	PlanPublishIntervalS int    `yaml:"plan_publish_interval_s,omitempty" json:"plan_publish_interval_s,omitempty"`

	// ManageCredentials (default ON when enabled) makes FTW generate a random
	// password on first enable, which the in-process CalDAV server then
	// authenticates against. The credential is shown in the Settings → Calendar
	// tab (with a QR) to paste into a calendar app, so the operator never has to
	// set one by hand.
	ManageCredentials *bool `yaml:"manage_credentials,omitempty" json:"manage_credentials,omitempty"`

	// Listen is the bind address for the in-process CalDAV server. Default
	// ":5232". FTW binds it on the LAN.
	Listen string `yaml:"listen,omitempty" json:"listen,omitempty"`
}

// ListenAddr returns the native CalDAV server bind address (default ":5232").
func (cv *CalDAV) ListenAddr() string {
	if cv != nil && strings.TrimSpace(cv.Listen) != "" {
		return strings.TrimSpace(cv.Listen)
	}
	return ":5232"
}

// ManageCredentialsEnabled reports whether FTW should auto-generate the managed
// CalDAV credential. Nil-safe; defaults ON when the feature is on.
func (cv *CalDAV) ManageCredentialsEnabled() bool {
	return cv != nil && cv.Enabled && (cv.ManageCredentials == nil || *cv.ManageCredentials)
}

// EVSEHistoryEnabled reports whether FTW should write EV-session history
// events. Nil-safe; defaults ON when the feature is enabled.
func (cv *CalDAV) EVSEHistoryEnabled() bool {
	return cv != nil && cv.Enabled && (cv.EVSEHistory == nil || *cv.EVSEHistory)
}

// PublishPlanEnabled reports whether FTW should publish its forward-looking
// plan calendar. Nil-safe; defaults ON when the feature is enabled.
func (cv *CalDAV) PublishPlanEnabled() bool {
	return cv != nil && cv.Enabled && (cv.PublishPlan == nil || *cv.PublishPlan)
}

// CalDAV defaults. Keyword identifiers are English; operators may override
// with localised terms via config (the values are user-facing).
var (
	DefaultCalDAVURL          = "http://localhost:5232"
	DefaultCalDAVCalendarPath = "/fortytwowatts/energy/"
	DefaultCalDAVHistoryPath  = "/fortytwowatts/history/"
	DefaultCalDAVPlanPath     = "/fortytwowatts/plan/"
	DefaultCalDAVPlanPublishS = 900
	DefaultCalDAVUsername     = "fortytwowatts"
	DefaultCalDAVPollS        = 300
	DefaultCalDAVHorizonDays  = 7
	DefaultCalDAVEVTargetSoC  = 0.8
	DefaultAwayKeywords       = []string{"away", "vacation", "holiday"}
	DefaultEVKeywords         = []string{"ev", "car", "charge"}
)

// Validate enforces range rules. Defaults are applied by the calendar
// service at construction time, so unset fields are legal here.
func (cv *CalDAV) Validate() error {
	if cv == nil || !cv.Enabled {
		return nil
	}
	if cv.PollIntervalS < 0 {
		return errors.New("caldav.poll_interval_s must be >= 0")
	}
	if cv.HorizonDays < 0 {
		return errors.New("caldav.horizon_days must be >= 0")
	}
	if cv.EVDefaultTargetSoC < 0 || cv.EVDefaultTargetSoC > 1 {
		return errors.New("caldav.ev_default_target_soc must be in [0, 1]")
	}
	return nil
}

// Normalize folds the legacy `email:` YAML key into Username and clears
// it so subsequent writes use the canonical key. Idempotent.
func (e *EVCharger) Normalize() {
	if e == nil {
		return
	}
	if e.Username == "" && e.EmailLegacy != "" {
		e.Username = e.EmailLegacy
	}
	e.EmailLegacy = ""
}

// Validate enforces per-provider shape rules. Password is intentionally
// not required here — it's loaded from state.db after YAML parse (see
// main.go's ev_charger_password restore step), so at Validate() time
// the field may be legitimately empty.
func (e *EVCharger) Validate() error {
	if e == nil {
		return nil
	}
	switch e.Provider {
	case "":
		return errors.New("ev_charger.provider: required")
	case "easee", "zaptec":
		// Username/Password are NOT enforced here. The runtime cloud
		// driver logs + idles when creds are missing, and the API picker
		// requires both before calling the vendor cloud. Letting a partial
		// ev_charger block load is the original contract — the wizard
		// writes provider intent first, then captures creds in a second
		// API call.
		if e.Modbus != nil {
			return fmt.Errorf("ev_charger.modbus: not valid for provider %s (HTTP transport)", e.Provider)
		}
	case "tesla-wc":
		// Local HTTP: LAN origin in http.base_url, no cloud account.
		// Empty base_url is allowed so the wizard can write provider
		// intent before the host is filled in.
		if e.Modbus != nil {
			return errors.New("ev_charger.modbus: not valid for provider tesla-wc (HTTP transport)")
		}
		// Switching from Easee/Zaptec leaves username/password on the
		// posted document, and POST /api/config restores ev_charger_password
		// from state.db. Drop them so the provider switch can save.
		e.Username = ""
		e.Password = ""
		e.EmailLegacy = ""
	case "ctek":
		if e.Modbus == nil || e.Modbus.Host == "" {
			return errors.New("ev_charger.modbus.host: required for provider ctek")
		}
		if e.Modbus.Port < 0 {
			return errors.New("ev_charger.modbus.port: must be >= 0")
		}
		if e.Modbus.UnitID < 0 || e.Modbus.UnitID > 247 {
			return errors.New("ev_charger.modbus.unit_id: must be in 0..247")
		}
		if e.HTTP != nil {
			return errors.New("ev_charger.http: not valid for provider ctek (Modbus transport)")
		}
		if e.Username != "" || e.Password != "" {
			return errors.New("ev_charger: username/password not valid for provider ctek")
		}
	default:
		return fmt.Errorf("ev_charger.provider %q: not supported (valid: easee, zaptec, tesla-wc, ctek)", e.Provider)
	}
	return nil
}

type OptimizerMultistage struct {
	ScenarioLimit          int      `yaml:"scenario_limit,omitempty" json:"scenario_limit,omitempty"`
	BranchIntervalSlots    int      `yaml:"branch_interval_slots,omitempty" json:"branch_interval_slots,omitempty"`
	BranchHorizonSlots     int      `yaml:"branch_horizon_slots,omitempty" json:"branch_horizon_slots,omitempty"`
	MaxBranching           int      `yaml:"max_branching,omitempty" json:"max_branching,omitempty"`
	NearHorizonSlots       int      `yaml:"near_horizon_slots,omitempty" json:"near_horizon_slots,omitempty"`
	MidHorizonSlots        int      `yaml:"mid_horizon_slots,omitempty" json:"mid_horizon_slots,omitempty"`
	MidBlockSlots          int      `yaml:"mid_block_slots,omitempty" json:"mid_block_slots,omitempty"`
	FarBlockSlots          int      `yaml:"far_block_slots,omitempty" json:"far_block_slots,omitempty"`
	ServiceCVaRWeight      *float64 `yaml:"service_cvar_weight,omitempty" json:"service_cvar_weight,omitempty"`
	ServiceCVaRAlpha       float64  `yaml:"service_cvar_alpha,omitempty" json:"service_cvar_alpha,omitempty"`
	EconomicCVaRWeight     float64  `yaml:"economic_cvar_weight,omitempty" json:"economic_cvar_weight,omitempty"`
	EconomicCVaRAlpha      float64  `yaml:"economic_cvar_alpha,omitempty" json:"economic_cvar_alpha,omitempty"`
	DecompositionThreshold int      `yaml:"decomposition_threshold,omitempty" json:"decomposition_threshold,omitempty"`
	DecompositionMethod    string   `yaml:"decomposition_method,omitempty" json:"decomposition_method,omitempty"`
	PHMaxIterations        int      `yaml:"ph_max_iterations,omitempty" json:"ph_max_iterations,omitempty"`
	PHRho                  float64  `yaml:"ph_rho,omitempty" json:"ph_rho,omitempty"`
	PHToleranceW           float64  `yaml:"ph_tolerance_w,omitempty" json:"ph_tolerance_w,omitempty"`
}

// Planner configures the MPC scheduler (optional — disabled if omitted).
// Mode: "self_consumption" (default) | "cheap_charge" | "arbitrage".
type Planner struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Mode    string `yaml:"mode,omitempty" json:"mode,omitempty"`
	// ForecastTrust is the first-boot household slider: cautious | balanced | bold.
	// After first boot the live value lives in SQLite (forecast_trust), like mode.
	ForecastTrust string `yaml:"forecast_trust,omitempty" json:"forecast_trust,omitempty"`
	// BatteryExport is the first-boot battery-sale permission:
	// unknown | not_allowed | allowed. Live value is SQLite battery_export.
	BatteryExport string `yaml:"battery_export,omitempty" json:"battery_export,omitempty"`
	// Engine selects the planner that produces the active plan: "core"
	// (default) solves in process with the Go DP; "python" hands the
	// champion role to the CVXPY/HiGHS worker. "go" and "dp" are accepted
	// spellings of "core". Read it through EngineName.
	Engine string `yaml:"engine,omitempty" json:"engine,omitempty"`
	// ShadowPython runs the Python/HiGHS worker after each Core replan, on
	// the inputs the champion solved, and records the terminal-corrected
	// cost difference. Shadow output never reaches dispatch. Pointer so an
	// unset field keeps the default (on) and an explicit false turns the
	// comparison off. Ignored when Engine is python.
	ShadowPython *bool `yaml:"shadow_python,omitempty" json:"shadow_python,omitempty"`
	// OptimizerCommand is the Python executable used for the local worker.
	// It is an executable path, not a shell command. The module invocation is
	// fixed by the host to avoid shell parsing and configuration injection.
	OptimizerCommand                      string               `yaml:"optimizer_command,omitempty" json:"optimizer_command,omitempty"`
	OptimizerDir                          string               `yaml:"optimizer_dir,omitempty" json:"optimizer_dir,omitempty"`
	OptimizerTransport                    string               `yaml:"optimizer_transport,omitempty" json:"optimizer_transport,omitempty"`
	OptimizerSocket                       string               `yaml:"optimizer_socket,omitempty" json:"optimizer_socket,omitempty"`
	OptimizerSolver                       string               `yaml:"optimizer_solver,omitempty" json:"optimizer_solver,omitempty"`
	OptimizerFormulation                  string               `yaml:"optimizer_formulation,omitempty" json:"optimizer_formulation,omitempty"`
	OptimizerTimeoutS                     float64              `yaml:"optimizer_timeout_s,omitempty" json:"optimizer_timeout_s,omitempty"`
	OptimizerIdleTimeoutS                 float64              `yaml:"optimizer_idle_timeout_s,omitempty" json:"optimizer_idle_timeout_s,omitempty"`
	OptimizerMIPRelGap                    float64              `yaml:"optimizer_mip_rel_gap,omitempty" json:"optimizer_mip_rel_gap,omitempty"`
	OptimizerCVaRWeight                   *float64             `yaml:"optimizer_cvar_weight,omitempty" json:"optimizer_cvar_weight,omitempty"`
	OptimizerCVaRAlpha                    float64              `yaml:"optimizer_cvar_alpha,omitempty" json:"optimizer_cvar_alpha,omitempty"`
	OptimizerRecourseShadow               bool                 `yaml:"optimizer_recourse_shadow,omitempty" json:"optimizer_recourse_shadow,omitempty"`
	OptimizerRecourseNonAnticipativeSlots int                  `yaml:"optimizer_recourse_non_anticipative_slots,omitempty" json:"optimizer_recourse_non_anticipative_slots,omitempty"`
	OptimizerChallengerPolicy             string               `yaml:"optimizer_challenger_policy,omitempty" json:"optimizer_challenger_policy,omitempty"`
	OptimizerMultistage                   *OptimizerMultistage `yaml:"optimizer_multistage,omitempty" json:"optimizer_multistage,omitempty"`
	BaseLoadW                             float64              `yaml:"base_load_w,omitempty" json:"base_load_w,omitempty"`
	HorizonHours                          int                  `yaml:"horizon_hours,omitempty" json:"horizon_hours,omitempty"`
	IntervalMin                           int                  `yaml:"interval_min,omitempty" json:"interval_min,omitempty"`
	SoCMin                                float64              `yaml:"soc_min,omitempty" json:"soc_min,omitempty"`
	SoCMax                                float64              `yaml:"soc_max,omitempty" json:"soc_max,omitempty"`
	SoCMinPct                             float64              `yaml:"soc_min_pct,omitempty" json:"soc_min_pct,omitempty"`
	SoCMaxPct                             float64              `yaml:"soc_max_pct,omitempty" json:"soc_max_pct,omitempty"`

	// Deprecated: SoCSafetyFloorPct / SafetyFloorPenaltyOreKwhHour. The
	// SoC-percentage safety floor was replaced by downside-PV planning
	// (PVForecastSafetyK) — a percentage is the wrong unit (relative to
	// battery size) for an absolute forecast risk. Still parsed so old
	// config files load; ignored at runtime with a warning. Remove from
	// your config and set pv_forecast_safety_k instead.
	SoCSafetyFloorPct            float64 `yaml:"soc_safety_floor_pct,omitempty" json:"soc_safety_floor_pct,omitempty"`
	SafetyFloorPenaltyOreKwhHour float64 `yaml:"safety_floor_penalty_ore_kwh_hour,omitempty" json:"safety_floor_penalty_ore_kwh_hour,omitempty"`

	// PVForecastSafetyK scales the downside-PV haircut: the MPC plans
	// against forecast PV minus k·σ, where σ is the recent PV forecast
	// error std (pvmodel residual). The DP then won't run the battery
	// down betting on PV that may not arrive — a reserve emerges from the
	// live forecast uncertainty itself, sized to the real risk (large on
	// variable cloudy days, ~zero on clear days or in winter), not a flat
	// SoC %. Pointer so unset is distinct from an explicit 0.
	//
	// FIRST-BOOT SEED ONLY. The live value follows the Plan card's
	// slider (SQLite key planner_safety_k, same stored-wins contract as
	// forecast_trust); this field seeds that float verbatim once when
	// nothing is stored, and never overrides the slider — the old
	// precedence disabled the slider for as long as the field existed.
	PVForecastSafetyK *float64 `yaml:"pv_forecast_safety_k,omitempty" json:"pv_forecast_safety_k,omitempty"`

	// PVChargeBonusOreKwh credits each kWh of battery charge fed from
	// live PV surplus, in passive_arbitrage mode. Default 0 (disabled)
	// — the import-tariff + VAT asymmetry already makes "store PV now"
	// strictly preferred over "export PV now, reimport later" in the
	// underlying DP economics, so the bonus is redundant under typical
	// retail pricing. Setting it > 0 reinstates the bias and can pull
	// battery charging forward; on days with future negative-price
	// hours this leaves no headroom to absorb negative-priced PV and
	// forces export at a loss. Use only if you have evidence that the
	// DP is undervaluing storage in your specific configuration.
	PVChargeBonusOreKwh float64 `yaml:"pv_charge_bonus_ore_kwh,omitempty" json:"pv_charge_bonus_ore_kwh,omitempty"`

	ChargeEfficiency    float64 `yaml:"charge_efficiency,omitempty" json:"charge_efficiency,omitempty"`
	DischargeEfficiency float64 `yaml:"discharge_efficiency,omitempty" json:"discharge_efficiency,omitempty"`
	ExportOrePerKWh     float64 `yaml:"export_ore_per_kwh,omitempty" json:"export_ore_per_kwh,omitempty"` // 0 = use mean spot

	// MinArbitrageSpreadOreKwh is the operator's "don't cycle the battery
	// for marginal gains" knob, in öre per kWh. The planner won't cycle for
	// grid arbitrage unless the price gain beats this many öre/kWh on top of
	// round-trip losses. Applies only to the arbitrage modes
	// (planner_arbitrage / planner_passive_arbitrage); self-consumption is
	// never affected. It biases the planner's decision only — the savings
	// statistics stay on real spot economics. 0 (default) = disabled.
	MinArbitrageSpreadOreKwh float64 `yaml:"min_arbitrage_spread_ore_kwh,omitempty" json:"min_arbitrage_spread_ore_kwh,omitempty"`

	// LegacyDispatch reverts the control loop from the default
	// energy-allocation path back to the legacy PI-on-grid-target
	// path. Provided for emergency rollback only — the energy path
	// respects the principle "plan allocates energy, EMS reacts to
	// live data".
	LegacyDispatch bool `yaml:"legacy_dispatch,omitempty" json:"legacy_dispatch,omitempty"`

	// UseEnergyDispatch is the deprecated inverse of LegacyDispatch.
	// Pointer so we can distinguish "unset" (nil) from "explicitly
	// false" (*false) — the latter matters because an operator who
	// previously picked legacy dispatch must not be silently flipped
	// to the energy path on upgrade. Honored with a startup WARN
	// and will be removed after one release.
	UseEnergyDispatch *bool `yaml:"use_energy_dispatch,omitempty" json:"use_energy_dispatch,omitempty"`
}

// PlannerEngineCore and PlannerEnginePython are the two planners that can hold
// the champion role.
const (
	PlannerEngineCore   = "core"
	PlannerEnginePython = "python"
)

// EngineName resolves planner.engine to one of the two champions. Unset means
// core: the Go DP measured within öre of the external MILP on replayed site
// snapshots, and it needs no sidecar to be running to produce a plan.
func (p *Planner) EngineName() string {
	if p == nil {
		return PlannerEngineCore
	}
	if strings.EqualFold(strings.TrimSpace(p.Engine), PlannerEnginePython) {
		return PlannerEnginePython
	}
	return PlannerEngineCore
}

// ShadowPythonEnabled reports whether the external optimizer runs behind a Core
// champion as a comparison shadow. Default on: the per-replan cost difference
// it records is the field evidence for retiring the external stack.
func (p *Planner) ShadowPythonEnabled() bool {
	if p == nil || p.ShadowPython == nil {
		return true
	}
	return *p.ShadowPython
}

// OptimizerTimeout returns the runtime contract value for an unset timeout.
// Parsing also fills it so API clients do not invent a shorter default when
// they save an otherwise unchanged planner.
func (p *Planner) OptimizerTimeout() time.Duration {
	if p == nil || p.OptimizerTimeoutS <= 0 {
		return optimizercontract.DefaultTimeout
	}
	return time.Duration(p.OptimizerTimeoutS * float64(time.Second))
}

// Site is the top-level control loop config.
type Site struct {
	TroubleshootingMode  bool    `yaml:"troubleshooting_mode,omitempty" json:"troubleshooting_mode,omitempty"`
	Name                 string  `yaml:"name" json:"name"`
	ControlIntervalS     int     `yaml:"control_interval_s" json:"control_interval_s"`
	GridTargetW          float64 `yaml:"grid_target_w" json:"grid_target_w"`
	GridToleranceW       float64 `yaml:"grid_tolerance_w" json:"grid_tolerance_w"`
	WatchdogTimeoutS     int     `yaml:"watchdog_timeout_s" json:"watchdog_timeout_s"`
	SmoothingAlpha       float64 `yaml:"smoothing_alpha" json:"smoothing_alpha"`
	Gain                 float64 `yaml:"gain" json:"gain"`
	SlewRateW            float64 `yaml:"slew_rate_w" json:"slew_rate_w"`
	MinDispatchIntervalS int     `yaml:"min_dispatch_interval_s" json:"min_dispatch_interval_s"`

	// SlewEnabled gates the external per-cycle ramp limiter. Both
	// supported inverter families (Ferroamp, Sungrow) have their own
	// internal power-ramp control loops; the external slew was
	// originally added to dampen reactive-PI oscillation under noisy
	// meter sampling, but it also slows legitimate step-response and
	// can interact badly with PI integrator state (the 2026-05-25
	// recovery took ~3 min of slew-bounded ramping after the integral
	// finally unwound).
	//
	// Pointer so we can distinguish "unset → default true" from
	// "explicitly false". Defaults to enabled to preserve back-compat
	// on existing installs.
	SlewEnabled *bool `yaml:"slew_enabled,omitempty" json:"slew_enabled,omitempty"`

	// PVSurplusAbsorbSoCCapPct is the operator override for the PV-surplus
	// absorber underlay in the energy-dispatch path (planner_cheap /
	// planner_arbitrage). When the planner's slot allocation would still
	// leave grid exporting beyond pv_surplus_absorb_threshold_w AND
	// average SoC is below this cap, the dispatch redirects the leftover
	// export into the battery instead of crossing the meter. Never
	// reverses a discharge plan. 0 = no operator override; the planner can
	// still enable a slot when capture displaces a more expensive future
	// grid-funded charge.
	//
	// Suggested 0.88 — leaves a little margin below the planner's typical
	// soc_max = 0.90 so the absorber doesn't slam into the wall.
	PVSurplusAbsorbSoCCap    float64 `yaml:"pv_surplus_absorb_soc_cap,omitempty" json:"pv_surplus_absorb_soc_cap,omitempty"`
	PVSurplusAbsorbSoCCapPct float64 `yaml:"pv_surplus_absorb_soc_cap_pct,omitempty" json:"pv_surplus_absorb_soc_cap_pct,omitempty"`

	// PVSurplusAbsorbThresholdW is the trigger threshold for the
	// absorber: only fires when projected grid export exceeds this many
	// watts after the plan's target. Defaults to 100 W whenever the
	// operator or planner enables absorption.
	PVSurplusAbsorbThresholdW float64 `yaml:"pv_surplus_absorb_threshold_w,omitempty" json:"pv_surplus_absorb_threshold_w,omitempty"`

	// DCLinkProtectionEnabled opts into a live-state PV curtail that
	// fires when SoC is near full AND PV significantly exceeds load
	// — the configuration most exposed to a load-step-triggered
	// inverter trip (real 2026-05-25 incident: Ferroamp EnergyHub
	// fault from a 2.7 kW load step under 6 kW PV + 85 % SoC).
	// Engaging pre-curtails PV to live load + margin so a sudden
	// load step inside the margin lands without DC-link stress.
	// Disabled by default — opt-in for sites that see repeated
	// inverter trips.
	DCLinkProtectionEnabled bool `yaml:"dc_link_protection_enabled,omitempty" json:"dc_link_protection_enabled,omitempty"`

	// DCLinkProtectionSoCThreshold (0-1) is the SoC fraction at or
	// above which the protective curtail engages. Default 0.80.
	DCLinkProtectionSoCThreshold float64 `yaml:"dc_link_protection_soc_threshold,omitempty" json:"dc_link_protection_soc_threshold,omitempty"`

	// DCLinkProtectionMarginW is the headroom (W) kept above live
	// load when the protection fires. Larger margin = more PV
	// allowed through, smaller load-step capacity before re-curtail.
	// Default 1000.
	DCLinkProtectionMarginW float64 `yaml:"dc_link_protection_margin_w,omitempty" json:"dc_link_protection_margin_w,omitempty"`

	// MaxExportW caps total site export (W, magnitude) below the physical
	// fuse. 0 = disabled (export bounded only by the fuse). When > 0 it is
	// enforced two ways: the dispatch fuse guard scales battery discharge
	// back so predicted export stays under it, and the MPC caps each slot's
	// export so the planner never schedules a discharge that would
	// over-export. Protects inverters that trip on sustained export well
	// below the breaker rating — the recurring Ferroamp EnergyHub fault
	// state 0x8030 after ~8 kW sustained midday export, which only cleared
	// as PV waned. Set it just under the observed trip point.
	MaxExportW float64 `yaml:"max_export_w,omitempty" json:"max_export_w,omitempty"`
}

// DefaultFuseSafetyMarginA is the fall-back per-phase amp headroom
// applied when fuse.safety_margin_a is unset (nil) in the YAML.
// Single source of truth — main.go routes through Fuse.Effective-
// SafetyMarginA() rather than re-declaring it.
const DefaultFuseSafetyMarginA = 0.5

// Fuse describes the shared breaker limit used by the fuse guard.
type Fuse struct {
	MaxAmps float64 `yaml:"max_amps" json:"max_amps"`
	Phases  int     `yaml:"phases" json:"phases"`
	Voltage float64 `yaml:"voltage" json:"voltage"`

	// SafetyMarginA reserves headroom (per-phase amps) below MaxAmps
	// inside the dispatch fuse guard. Pointer so we can distinguish
	// "unset" (nil → DefaultFuseSafetyMarginA) from "explicitly
	// disabled" (non-nil 0.0). Inverters often have their own per-
	// phase current protection that trips before the breaker; without
	// a margin the dispatch can ride right up to MaxAmps and the
	// inverter cuts to 0 W in one tick, then dispatch ramps back up —
	// visible as a flap. 0.5 A × 230 V × 3 phases ≈ 345 W of aggregate
	// headroom.
	SafetyMarginA *float64 `yaml:"safety_margin_a,omitempty" json:"safety_margin_a,omitempty"`
}

// MaxPowerW returns the total power budget for the fuse guard.
func (f Fuse) MaxPowerW() float64 {
	return f.MaxAmps * f.Voltage * float64(f.Phases)
}

// EffectiveSafetyMarginA returns the per-phase amp headroom to apply,
// resolving nil ("unset → use default") vs an explicit value (including
// 0.0 to disable the margin entirely). Single read site so the default
// can never drift across consumers.
func (f Fuse) EffectiveSafetyMarginA() float64 {
	if f.SafetyMarginA == nil {
		return DefaultFuseSafetyMarginA
	}
	return *f.SafetyMarginA
}

// Driver is one driver entry. Each driver is a Lua script loaded by
// the driver host at startup (or on hot-reload via the file watcher).
type Driver struct {
	Name              string  `yaml:"name" json:"name"`
	Lua               string  `yaml:"lua,omitempty" json:"lua,omitempty"` // path to .lua file
	IsSiteMeter       bool    `yaml:"is_site_meter,omitempty" json:"is_site_meter,omitempty"`
	BatteryCapacityWh float64 `yaml:"battery_capacity_wh,omitempty" json:"battery_capacity_wh,omitempty"`
	// BatteryTelemetryOnly allows a read-only gateway driver to publish a
	// physical battery's telemetry without making that driver eligible for
	// battery dispatch. It is an explicit control-pool opt-out and wins even if
	// a stale or hand-written config also contains BatteryCapacityWh.
	// Sourceful Zap is the canonical user: its local API exposes battery data,
	// but no stable semantic set-power endpoint.
	BatteryTelemetryOnly bool `yaml:"battery_telemetry_only,omitempty" json:"battery_telemetry_only,omitempty"`
	// ObserveOnly keeps structured battery telemetry (host.emit "battery")
	// and UI visibility while excluding this driver from dispatch, MPC,
	// battery-model training, and watchdog DefaultMode commands. Use when
	// another party (e.g. a retailer VPP) owns actuation. Requires
	// battery_capacity_wh > 0 — without capacity, battery emits are
	// dropped at the host boundary anyway.
	ObserveOnly bool `yaml:"observe_only,omitempty" json:"observe_only,omitempty"`
	// MaxChargeW + MaxDischargeW set this driver's per-command power
	// ceiling (site-signed +/-). Both optional; zero = fall through to
	// the global MaxCommandW = 5 kW default the dispatcher has shipped
	// with since v0.x. On a hybrid inverter that can actually deliver
	// more (e.g. Ferroamp 10-15 kW, Sungrow 8-10 kW on 32 A), lifting
	// the per-driver cap is the right move — site-wide fuse protection
	// (applyFuseGuard) still enforces the grid-boundary budget above
	// whatever per-battery cap you set. Issue #145.
	MaxChargeW    float64 `yaml:"max_charge_w,omitempty" json:"max_charge_w,omitempty"`
	MaxDischargeW float64 `yaml:"max_discharge_w,omitempty" json:"max_discharge_w,omitempty"`
	// InverterGroup tags this driver as belonging to a shared
	// inverter+battery unit (e.g. set `inverter_group: ferroamp` on
	// both the Ferroamp battery driver and anything publishing its PV
	// telemetry). The dispatcher prefers routing charge to the battery
	// whose group also has live PV output — staying DC-coupled on the
	// same inverter avoids the DC→AC→AC→DC conversion overhead of
	// cross-charging. Untagged drivers keep today's capacity-proportional
	// behavior. See issue #143.
	InverterGroup string `yaml:"inverter_group,omitempty" json:"inverter_group,omitempty"`
	// SupportsPVCurtail flags this driver as one that handles the
	// `curtail` / `curtail_disable` actions in its lua. Drivers with
	// it set become eligible for ComputePVCurtail dispatch when the
	// MPC's slot directive carries a PVLimitW > 0 (negative-export
	// economic guard). Default false — operators must opt in per
	// driver to avoid surprising older configs. The lua side has
	// always been there for sungrow / ferroamp / deye / huawei /
	// solis; this flag just turns on the Go-side dispatcher.
	SupportsPVCurtail bool `yaml:"supports_pv_curtail,omitempty" json:"supports_pv_curtail,omitempty"`
	// Disabled skips this driver at startup / reload. Set via the UI when
	// you want to temporarily take a driver out without editing yaml.
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`
	// Control opts this one site into one exact signed control artifact.
	// The runtime rejects control unless all three pins match the active
	// Device Support package. Merely selecting the beta channel or installing
	// a control-capable artifact never enables writes.
	Control *DriverControlOptIn `yaml:"control,omitempty" json:"control,omitempty"`
	// HasPassword is a JSON-only signal to the UI that Config["password"]
	// holds a non-empty value on disk. Populated by MaskSecrets after the
	// real password is blanked out so the operator can still tell apart
	// "never entered" from "saved but masked". Never written to yaml.
	HasPassword bool `yaml:"-" json:"has_password,omitempty"`

	// Capabilities: the resources this driver is allowed to use.
	// Unset capabilities are explicitly denied.
	Capabilities Capabilities `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`

	// Driver-specific config: arbitrary key/value map passed to
	// driver_init(config) in Lua. Used for credentials, device addresses,
	// thresholds, etc. that don't fit the generic capabilities model.
	Config map[string]any `yaml:"config,omitempty" json:"config,omitempty"`

	// Legacy protocol fields (equivalent to capabilities, still accepted
	// for backwards compatibility with master-branch configs).
	MQTT   *MQTTConfig   `yaml:"mqtt,omitempty" json:"mqtt,omitempty"`
	Modbus *ModbusConfig `yaml:"modbus,omitempty" json:"modbus,omitempty"`
}

// DriverControlOptIn is a per-site, fail-closed control grant. PackageID,
// Version and ArtifactSHA256 must match signed active package metadata.
type DriverControlOptIn struct {
	Enabled        bool   `yaml:"enabled" json:"enabled"`
	PackageID      string `yaml:"package_id" json:"package_id"`
	Version        string `yaml:"version" json:"version"`
	ArtifactSHA256 string `yaml:"artifact_sha256" json:"artifact_sha256"`
}

// Capabilities explicitly scope what host resources a driver can access.
type Capabilities struct {
	// AllowUnverifiedLocal permits a driver to use an mDNS answer obtained by
	// FTW. When false, the transport leaves .local names to the system resolver.
	// A name allowlist does not prove server identity.
	AllowUnverifiedLocal bool            `yaml:"allow_unverified_local,omitempty" json:"allow_unverified_local,omitempty"`
	MQTT                 *MQTTConfig     `yaml:"mqtt,omitempty" json:"mqtt,omitempty"`
	Modbus               *ModbusConfig   `yaml:"modbus,omitempty" json:"modbus,omitempty"`
	Serial               *SerialConfig   `yaml:"serial,omitempty" json:"serial,omitempty"`
	HTTP                 *HTTPCapability `yaml:"http,omitempty" json:"http,omitempty"`
	WebSocket            *WSCapability   `yaml:"websocket,omitempty" json:"websocket,omitempty"`
	TCP                  *TCPCapability  `yaml:"tcp,omitempty" json:"tcp,omitempty"`
	Standalone           bool            `yaml:"standalone,omitempty" json:"standalone,omitempty"`
}

// MQTTConfig grants access to one MQTT broker.
type MQTTConfig struct {
	Host     string `yaml:"host" json:"host"`
	Port     int    `yaml:"port,omitempty" json:"port,omitempty"` // default 1883
	Username string `yaml:"username,omitempty" json:"username,omitempty"`
	Password string `yaml:"password,omitempty" json:"password,omitempty"`
	// AllowUnverifiedLocal is copied from capabilities.allow_unverified_local
	// by the core before this config reaches the transport factory. It is
	// runtime-only and never comes from this nested YAML block.
	AllowUnverifiedLocal bool `yaml:"-" json:"-"`
}

// ModbusConfig grants access to one Modbus TCP endpoint.
type ModbusConfig struct {
	Host   string `yaml:"host" json:"host"`
	Port   int    `yaml:"port,omitempty" json:"port,omitempty"`       // default 502
	UnitID int    `yaml:"unit_id,omitempty" json:"unit_id,omitempty"` // default 1
	// AllowUnverifiedLocal is copied from capabilities.allow_unverified_local
	// by the core before this config reaches the transport factory. It is
	// runtime-only and never comes from this nested YAML block.
	AllowUnverifiedLocal bool `yaml:"-" json:"-"`
}

// SerialConfig grants read-only access to one local serial device.
type SerialConfig struct {
	Address       string `yaml:"address" json:"address"`
	BaudRate      int    `yaml:"baud_rate,omitempty" json:"baud_rate,omitempty"`
	DataBits      int    `yaml:"data_bits,omitempty" json:"data_bits,omitempty"`
	StopBits      int    `yaml:"stop_bits,omitempty" json:"stop_bits,omitempty"`
	Parity        string `yaml:"parity,omitempty" json:"parity,omitempty"`
	ReadTimeoutMS int    `yaml:"read_timeout_ms,omitempty" json:"read_timeout_ms,omitempty"`
}

// HTTPCapability grants HTTP access to specific hostnames (future).
type HTTPCapability struct {
	AllowedHosts []string `yaml:"allowed_hosts" json:"allowed_hosts"`
	// TLSPinSHA256, when set, pins the HTTPS server's leaf certificate to
	// this SHA-256 fingerprint (hex; colons/whitespace ignored, case-
	// insensitive). It is the SHA-256 over the DER certificate — identical
	// to `openssl x509 -fingerprint -sha256`. Use it for HTTPS endpoints
	// that present a self-signed certificate the system trust store cannot
	// validate (e.g. a NIBE heat pump's local REST API). When set, normal
	// chain/hostname verification is REPLACED by an exact fingerprint match
	// for this driver only; when empty, standard verification against the
	// system roots applies (unchanged for every existing HTTP driver).
	TLSPinSHA256 string `yaml:"tls_pin_sha256,omitempty" json:"tls_pin_sha256,omitempty"`
	// AllowWrite grants host.http_patch — the verb REST device APIs use for
	// state-changing writes — as a separate, explicit operator decision, the
	// HTTP twin of a read-only Modbus driver versus one allowed to write
	// registers. Scope is exactly http_patch: http_get stays a read and
	// http_post stays under the plain HTTP grant (existing drivers POST to
	// query-style APIs), so granting HTTP for telemetry never implicitly
	// grants the ability to mutate a device. Default off.
	AllowWrite bool `yaml:"allow_write,omitempty" json:"allow_write,omitempty"`
}

// WSCapability grants WebSocket (ws://, wss://) access. Same allowlist
// semantics as HTTPCapability — bare host = any port; "host:port" = exact.
type WSCapability struct {
	AllowedHosts []string `yaml:"allowed_hosts" json:"allowed_hosts"`
}

// TCPCapability grants raw TCP socket access (host.tcp_open). Same
// allowlist semantics as the HTTP/WS lists: bare host entry matches any
// port; "host:port" requires an exact match. Empty list = any host:port,
// which is fine for fully-trusted LAN deployments but loose enough to
// warrant an explicit list in shared installs.
type TCPCapability struct {
	AllowedHosts []string `yaml:"allowed_hosts" json:"allowed_hosts"`
}

// EffectiveMQTT returns the driver's MQTT config, preferring capabilities over legacy.
func (d Driver) EffectiveMQTT() *MQTTConfig {
	if d.Capabilities.MQTT != nil {
		return d.Capabilities.MQTT
	}
	return d.MQTT
}

// EffectiveModbus returns the driver's Modbus config, preferring capabilities.
func (d Driver) EffectiveModbus() *ModbusConfig {
	if d.Capabilities.Modbus != nil {
		return d.Capabilities.Modbus
	}
	return d.Modbus
}

// API is the HTTP server config.
type API struct {
	Port    int  `yaml:"port" json:"port"`
	LANAuth bool `yaml:"lan_auth,omitempty" json:"lan_auth,omitempty"`
}

// HomeAssistant is the MQTT bridge config.
type HomeAssistant struct {
	Enabled          bool   `yaml:"enabled" json:"enabled"`
	Broker           string `yaml:"broker" json:"broker"`
	Port             int    `yaml:"port,omitempty" json:"port,omitempty"`
	Username         string `yaml:"username,omitempty" json:"username,omitempty"`
	Password         string `yaml:"password,omitempty" json:"password,omitempty"`
	PublishIntervalS int    `yaml:"publish_interval_s,omitempty" json:"publish_interval_s,omitempty"`
	// AllowUnverifiedLocal permits the bridge to use an mDNS answer obtained by
	// FTW. When false, the transport leaves .local names to the system resolver.
	AllowUnverifiedLocal bool `yaml:"allow_unverified_local,omitempty" json:"allow_unverified_local,omitempty"`
}

// StateConf is the persistent state DB config.
//
// Path is the SQLite file (default "state.db"). ColdDir is the directory
// where >14d-old time-series data is rolled off as Parquet, partitioned
// YYYY/MM/DD.parquet (default "cold/" alongside Path).
//
// ColdRetentionDays bounds the cold Parquet tier: day files older than
// this are deleted by the hourly rolloff. 0 (default) keeps everything —
// a year of ~50 metrics is a few GB, so bounding is opt-in for small
// SD cards.
type StateConf struct {
	Path              string `yaml:"path" json:"path"`
	ColdDir           string `yaml:"cold_dir" json:"cold_dir"`
	ColdRetentionDays int    `yaml:"cold_retention_days,omitempty" json:"cold_retention_days,omitempty"`
	// BackupDir stores verified full-backup archives. Relative paths resolve
	// beside state.db; an absolute path can point at an externally mounted
	// USB disk or network share.
	BackupDir string `yaml:"backup_dir,omitempty" json:"backup_dir,omitempty"`
}

// Price is the spot-price source config.
type Price struct {
	Provider         string  `yaml:"provider" json:"provider"` // sourceful | elprisetjustnu | entsoe | static | none
	Zone             string  `yaml:"zone,omitempty" json:"zone,omitempty"`
	GridTariffOreKwh float64 `yaml:"grid_tariff_ore_kwh,omitempty" json:"grid_tariff_ore_kwh,omitempty"`
	VATPercent       float64 `yaml:"vat_percent,omitempty" json:"vat_percent,omitempty"`
	APIKey           string  `yaml:"api_key,omitempty" json:"api_key,omitempty"`

	// Currency is the ISO code for pricing (default "SEK"). ENTSOE
	// returns EUR/MWh; we convert using ECB daily FX rates.
	Currency string `yaml:"currency,omitempty" json:"currency,omitempty"`

	// ExportBonusOreKwh is a per-kWh bonus on top of spot when exporting.
	// Some retailers pay spot + fixed bonus (e.g. 60 öre in Sweden via
	// "skattereduktion" + electricity-certificate value). Default 0.
	ExportBonusOreKwh float64 `yaml:"export_bonus_ore_kwh,omitempty" json:"export_bonus_ore_kwh,omitempty"`

	// ExportFeeOreKwh is a per-kWh deduction on export (e.g. transmission
	// fees some DSOs charge for feed-in). Reduces effective export price.
	ExportFeeOreKwh float64 `yaml:"export_fee_ore_kwh,omitempty" json:"export_fee_ore_kwh,omitempty"`

	// ExportFloorOreKwh, if set, clamps per-slot export revenue at the
	// given floor (öre/kWh). Use this only when your retailer caps
	// negative-spot export at zero — i.e. they don't bill you when
	// spot goes negative. Default (unset / nil) lets export revenue
	// follow real spot, which can go negative; that's the physics
	// most Swedish customer agreements pass through. Set to a pointer
	// to 0.0 if you have a guaranteed-zero-floor agreement.
	ExportFloorOreKwh *float64 `yaml:"export_floor_ore_kwh,omitempty" json:"export_floor_ore_kwh,omitempty"`

	// StaticOreKwh is the default energy price in minor units per kWh when
	// provider=static. Time-of-use windows below override matching hours;
	// hours they miss keep this rate. Zero is a real price (free energy).
	StaticOreKwh float64 `yaml:"static_ore_kwh,omitempty" json:"static_ore_kwh,omitempty"`
	// StaticTOU is an optional time-of-use schedule in the box's local
	// time. First matching window wins. An empty Days list means every day.
	StaticTOU []TOUWindow `yaml:"static_tou,omitempty" json:"static_tou,omitempty"`
}

// TOUWindow is one time-of-use period for provider=static.
type TOUWindow struct {
	Start  string   `yaml:"start" json:"start"` // "HH:MM" local, inclusive
	End    string   `yaml:"end" json:"end"`     // "HH:MM" local, exclusive; "24:00" allowed
	OreKwh float64  `yaml:"ore_kwh" json:"ore_kwh"`
	Days   []string `yaml:"days,omitempty" json:"days,omitempty"` // mon..sun; empty = every day
}

// Validate checks a static tariff's windows. Other providers are accepted
// as-is: an unknown name currently disables fetching rather than failing
// config load, and that behaviour stays.
func (p *Price) Validate() error {
	if p == nil || !strings.EqualFold(p.Provider, "static") {
		return nil
	}
	for i, w := range p.StaticTOU {
		if strings.TrimSpace(w.Start) == "" || strings.TrimSpace(w.End) == "" {
			return fmt.Errorf("price.static_tou[%d]: start and end are required (HH:MM)", i)
		}
	}
	return nil
}

// Weather is the weather-forecast source config.
type Weather struct {
	Provider  string  `yaml:"provider" json:"provider"` // met_no | openweather | open_meteo | forecast_solar | none
	Latitude  float64 `yaml:"latitude" json:"latitude"`
	Longitude float64 `yaml:"longitude" json:"longitude"`
	APIKey    string  `yaml:"api_key,omitempty" json:"api_key,omitempty"`

	// PVRatedW is the system's nameplate PV output (W) — used as the
	// initial twin prior AND the ceiling for naive PV estimates. If 0,
	// we fall back to a heuristic (sum of battery_capacity_wh / 3),
	// which is only roughly right for homes where PV and storage were
	// sized together. Set explicitly for accurate day-1 forecasts.
	PVRatedW float64 `yaml:"pv_rated_w,omitempty" json:"pv_rated_w,omitempty"`

	// PVTiltDeg / PVAzimuthDeg describe the physical orientation of a
	// single panel group. Legacy single-array config — when PVArrays
	// below is empty, the forecast_solar provider synthesizes one
	// array from these + PVRatedW. Kept for backwards compatibility.
	PVTiltDeg    float64 `yaml:"pv_tilt_deg,omitempty" json:"pv_tilt_deg,omitempty"`
	PVAzimuthDeg float64 `yaml:"pv_azimuth_deg,omitempty" json:"pv_azimuth_deg,omitempty"`

	// PVArrays is the list of physically-distinct panel groups at the
	// site. Homes often have more than one roof plane (e.g. south and
	// east), and the forecast_solar provider gives noticeably better
	// predictions when each plane is described separately than when
	// everything is averaged into a single tilt/azimuth.
	//
	// When set, PVArrays overrides the legacy single-array fields for
	// geometry-aware providers. GHI providers such as open_meteo project
	// radiation onto these planes; incomplete entries are ignored and the
	// provider falls back to its flat estimate.
	PVArrays []PVArray `yaml:"pv_arrays,omitempty" json:"pv_arrays,omitempty"`

	// HeatingWPerDegC adds load proportional to max(18°C − outdoor_temp, 0).
	// A rough-but-useful way to teach the planner that cold nights cost
	// more than mild ones without running a full ML temperature fit.
	// Typical Swedish single-family values: 200–500 W/°C. 0 disables.
	HeatingWPerDegC float64 `yaml:"heating_w_per_degc,omitempty" json:"heating_w_per_degc,omitempty"`
}

// PVArray is one physically-distinct panel group. Multi-plane
// residential installs typically have two or three (e.g. south roof
// + east roof + garage) with different tilt/azimuth. The sum of all
// rated_w values should match the total PV nameplate at the site.
// kwp is a legacy YAML key folded into rated_w on load.
type PVArray struct {
	Name       string   `yaml:"name,omitempty" json:"name,omitempty"`
	RatedW     float64  `yaml:"rated_w,omitempty" json:"rated_w,omitempty"`
	KWp        float64  `yaml:"kwp,omitempty" json:"kwp,omitempty"`
	TiltDeg    *float64 `yaml:"tilt_deg" json:"tilt_deg"`
	AzimuthDeg *float64 `yaml:"azimuth_deg" json:"azimuth_deg"`
}

// CompleteGeometry returns one usable PV plane. Tilt and azimuth are
// pointers so an omitted field cannot be confused with a valid 0° value.
// Invalid or partial entries are intentionally not fatal: callers use the
// flat forecast path when no complete plane remains.
func (a PVArray) CompleteGeometry() (tiltDeg, azimuthDeg, ratedW float64, ok bool) {
	ratedW = a.RatedWatts()
	if ratedW <= 0 || math.IsNaN(ratedW) || math.IsInf(ratedW, 0) ||
		a.TiltDeg == nil || a.AzimuthDeg == nil {
		return 0, 0, 0, false
	}
	tiltDeg, azimuthDeg = *a.TiltDeg, *a.AzimuthDeg
	if math.IsNaN(tiltDeg) || math.IsInf(tiltDeg, 0) || tiltDeg < 0 || tiltDeg > 90 ||
		math.IsNaN(azimuthDeg) || math.IsInf(azimuthDeg, 0) || azimuthDeg < 0 || azimuthDeg > 360 {
		return 0, 0, 0, false
	}
	return tiltDeg, azimuthDeg, ratedW, true
}

// Battery is per-battery overrides (keyed by driver name in the top-level map).
type Battery struct {
	SoCMin        *float64 `yaml:"soc_min,omitempty" json:"soc_min,omitempty"`
	SoCMax        *float64 `yaml:"soc_max,omitempty" json:"soc_max,omitempty"`
	MaxChargeW    *float64 `yaml:"max_charge_w,omitempty" json:"max_charge_w,omitempty"`
	MaxDischargeW *float64 `yaml:"max_discharge_w,omitempty" json:"max_discharge_w,omitempty"`
	Weight        *float64 `yaml:"weight,omitempty" json:"weight,omitempty"`
}

// MaskSecrets returns a copy of the config with sensitive fields (passwords,
// API keys) replaced by empty strings so they are never exposed via the API.
// The original config is not modified.
func (c Config) MaskSecrets() Config {
	out := c

	if out.EVCharger != nil {
		cp := *out.EVCharger
		cp.Password = ""
		out.EVCharger = &cp
	}
	if out.CalDAV != nil {
		cp := *out.CalDAV
		cp.Password = ""
		out.CalDAV = &cp
	}
	if out.HomeAssistant != nil {
		cp := *out.HomeAssistant
		cp.Password = ""
		out.HomeAssistant = &cp
	}
	// The OCPP password is the only thing standing in front of a listener that
	// is reachable on every interface, so it must never leave over the API.
	// The per-charger passwords are the same secret with a narrower blast
	// radius, and the slice has to be copied rather than blanked in place —
	// the struct copy above shares its backing array with the live config.
	if out.OCPP != nil {
		cp := *out.OCPP
		cp.Password = ""
		if len(cp.Chargers) > 0 {
			chargers := make([]OCPPCharger, len(cp.Chargers))
			for i, c := range cp.Chargers {
				c.Password = ""
				chargers[i] = c
			}
			cp.Chargers = chargers
		}
		out.OCPP = &cp
	}
	if out.Price != nil {
		cp := *out.Price
		cp.APIKey = ""
		if len(cp.StaticTOU) > 0 {
			tou := make([]TOUWindow, len(cp.StaticTOU))
			copy(tou, cp.StaticTOU)
			for i := range tou {
				if len(tou[i].Days) > 0 {
					days := make([]string, len(tou[i].Days))
					copy(days, tou[i].Days)
					tou[i].Days = days
				}
			}
			cp.StaticTOU = tou
		}
		out.Price = &cp
	}
	if out.Weather != nil {
		cp := *out.Weather
		cp.APIKey = ""
		out.Weather = &cp
	}
	if out.Assistant != nil {
		cp := *out.Assistant
		cp.HasAPIKey = strings.TrimSpace(cp.APIKey) != ""
		cp.APIKey = ""
		out.Assistant = &cp
	}
	if out.Notifications != nil {
		cp := *out.Notifications
		if cp.Ntfy != nil {
			nc := *cp.Ntfy
			nc.HasAccessToken = strings.TrimSpace(nc.AccessToken) != ""
			nc.AccessToken = ""
			nc.Password = ""
			cp.Ntfy = &nc
		}
		if len(cp.Events) > 0 {
			evs := make([]NotificationRule, len(cp.Events))
			copy(evs, cp.Events)
			cp.Events = evs
		}
		out.Notifications = &cp
	}

	if len(out.Drivers) > 0 {
		drivers := make([]Driver, len(out.Drivers))
		copy(drivers, out.Drivers)
		for i := range drivers {
			if drivers[i].Config != nil {
				cp := make(map[string]any, len(drivers[i].Config))
				for k, v := range drivers[i].Config {
					cp[k] = v
				}
				if pw, has := cp["password"]; has {
					// Signal "stored" to the UI before we blank it out.
					if s, ok := pw.(string); ok && s != "" {
						drivers[i].HasPassword = true
					}
					cp["password"] = ""
				}
				drivers[i].Config = cp
			}
			if drivers[i].Capabilities.MQTT != nil {
				cp := *drivers[i].Capabilities.MQTT
				cp.Password = ""
				drivers[i].Capabilities.MQTT = &cp
			}
			if drivers[i].MQTT != nil {
				cp := *drivers[i].MQTT
				cp.Password = ""
				drivers[i].MQTT = &cp
			}
		}
		out.Drivers = drivers
	}

	return out
}

// PreserveMaskedSecrets copies real secrets from `existing` into `incoming`
// wherever the incoming value is empty (the UI sends "" for masked fields).
// Call this before saving a config received from the API.
func (incoming *Config) PreserveMaskedSecrets(existing *Config) {
	if incoming.EVCharger != nil && existing.EVCharger != nil && incoming.EVCharger.Password == "" {
		incoming.EVCharger.Password = existing.EVCharger.Password
	}
	if incoming.CalDAV != nil && existing.CalDAV != nil && incoming.CalDAV.Password == "" {
		incoming.CalDAV.Password = existing.CalDAV.Password
	}
	// Masked out on the way to the UI, so an unchanged password comes back
	// empty. Without this a save from the settings tab would blank it, and an
	// enabled server would then fail validation on the next reload.
	if incoming.OCPP != nil && existing.OCPP != nil {
		if incoming.OCPP.Password == "" {
			incoming.OCPP.Password = existing.OCPP.Password
		}
		// Per-charger passwords are masked the same way, and are matched by
		// charger id rather than position: the settings UI can reorder the
		// list or drop an entry, and restoring by index would then hand one
		// charger another's credential.
		if len(incoming.OCPP.Chargers) > 0 {
			stored := existing.OCPP.ChargerSecrets()
			for i := range incoming.OCPP.Chargers {
				c := &incoming.OCPP.Chargers[i]
				if c.Password == "" {
					c.Password = stored[c.ID]
				}
			}
		}
	}
	if incoming.HomeAssistant != nil && existing.HomeAssistant != nil && incoming.HomeAssistant.Password == "" {
		incoming.HomeAssistant.Password = existing.HomeAssistant.Password
	}
	if incoming.Price != nil && existing.Price != nil && incoming.Price.APIKey == "" {
		incoming.Price.APIKey = existing.Price.APIKey
	}
	if incoming.Weather != nil && existing.Weather != nil && incoming.Weather.APIKey == "" {
		incoming.Weather.APIKey = existing.Weather.APIKey
	}
	if incoming.Assistant != nil && existing.Assistant != nil && incoming.Assistant.APIKey == "" {
		incoming.Assistant.APIKey = existing.Assistant.APIKey
	}
	if incoming.Notifications != nil && existing.Notifications != nil &&
		incoming.Notifications.Ntfy != nil && existing.Notifications.Ntfy != nil {
		if incoming.Notifications.Ntfy.AccessToken == "" {
			incoming.Notifications.Ntfy.AccessToken = existing.Notifications.Ntfy.AccessToken
		}
		if incoming.Notifications.Ntfy.Password == "" {
			incoming.Notifications.Ntfy.Password = existing.Notifications.Ntfy.Password
		}
	}
	for i := range incoming.Drivers {
		for _, ed := range existing.Drivers {
			if incoming.Drivers[i].Name != ed.Name {
				continue
			}
			if incoming.Drivers[i].Config != nil && ed.Config != nil {
				if pw, ok := incoming.Drivers[i].Config["password"]; ok {
					if pw == "" || pw == nil {
						incoming.Drivers[i].Config["password"] = ed.Config["password"]
					}
				}
			}
			// Restore MQTT password in capabilities block.
			if incoming.Drivers[i].Capabilities.MQTT != nil && ed.Capabilities.MQTT != nil &&
				incoming.Drivers[i].Capabilities.MQTT.Password == "" {
				incoming.Drivers[i].Capabilities.MQTT.Password = ed.Capabilities.MQTT.Password
			}
			// Restore MQTT password in legacy block.
			if incoming.Drivers[i].MQTT != nil && ed.MQTT != nil &&
				incoming.Drivers[i].MQTT.Password == "" {
				incoming.Drivers[i].MQTT.Password = ed.MQTT.Password
			}
			break
		}
	}
}

// Load parses a config file from disk. Returns a fully-validated Config.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return Parse(data, filepath.Dir(path))
}

// Parse parses config bytes and validates. baseDir resolves driver Lua paths.
func Parse(data []byte, baseDir string) (*Config, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}
	var c Config
	if err := doc.Decode(&c); err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}
	// An omitted app_link section follows the new default. An explicit YAML
	// null was a valid opt-out before that default changed, so retain it as an
	// explicit disabled section instead of letting applyDefaults turn it on.
	if topLevelYAMLNull(&doc, "app_link") {
		c.AppLink = &AppLink{Enabled: false}
	}
	applyDefaults(&c)
	c.demoteExtraSiteMeters()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	c.ResolveDriverPaths(baseDir)
	return &c, nil
}

// demoteExtraSiteMeters keeps the first declared is_site_meter driver and
// clears the flag on the rest, recording a LoadWarning per demotion.
//
// Load-time only. Duplicate site meters are an operator mistake Validate
// rejects on the write path, but a file already on disk was often written
// by an older version that silently used the first match
// (SiteMeterDriver's order) — refusing it at boot crash-loops the process
// before the HTTP listener binds, and the operator loses the very UI they
// would fix the config with (field incident 2026-08-29: a driver install
// on v1.15.0 left two site meters; the box updated to v2.3.0 and went
// dark until SSH). Demoting reproduces exactly what the older version
// dispatched against, and the warning makes the ambiguity visible where
// silence caused it to be missed.
func (c *Config) demoteExtraSiteMeters() {
	kept := ""
	for i := range c.Drivers {
		d := &c.Drivers[i]
		if !d.IsSiteMeter {
			continue
		}
		if kept == "" {
			kept = d.Name
			continue
		}
		d.IsSiteMeter = false
		c.LoadWarnings = append(c.LoadWarnings, fmt.Sprintf(
			"config: drivers %q and %q both set is_site_meter: true; keeping %q as the site meter and ignoring the flag on %q — fix config.yaml so exactly one driver has it",
			kept, d.Name, kept, d.Name))
	}
}

func topLevelYAMLNull(doc *yaml.Node, key string) bool {
	if doc == nil || doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return false
	}
	// Node values leave ignored subtrees opaque while yaml.v3 applies merge
	// precedence to the root keys.
	var values map[string]yaml.Node
	if err := doc.Content[0].Decode(&values); err != nil {
		return true
	}
	value, ok := values[key]
	if !ok {
		return false
	}
	node := &value
	seen := make(map[*yaml.Node]bool)
	for node != nil && node.Kind == yaml.AliasNode {
		if seen[node] {
			return true
		}
		seen[node] = true
		node = node.Alias
	}
	return node == nil || node.ShortTag() == "!!null"
}

// DriversDirOverride redirects resolution of relative "drivers/<name>.lua"
// Lua paths to this directory instead of the config sibling. main.go sets
// it once at startup from the -drivers flag so Docker images — where
// drivers live in the immutable image layer (/app/drivers) rather than
// next to the user's config (/app/data) — can still load driver scripts.
// Empty string preserves the historical "sibling-of-config" behaviour.
var DriversDirOverride string

// UserDriversDirOverride is the first lookup path. It is tried before managed
// signed drivers and DriversDirOverride. It holds persistent user drivers in
// the docker deploy where DriversDirOverride lives in the immutable
// image layer. When set, ResolveDriverPaths checks whether a file
// exists in this directory first and uses it when found; otherwise
// falls back to the managed and bundled directories. Empty = single-dir
// behaviour (back-compat).
var UserDriversDirOverride string

// ManagedDriversDirOverride contains stable active symlinks maintained by the
// signed driver repository. It is checked after the local user overlay and
// before the bundled recovery snapshot.
var ManagedDriversDirOverride string

// ResolveDriverPaths joins relative Lua driver paths with baseDir, or
// with DriversDirOverride when the relative path starts with "drivers/".
// When UserDriversDirOverride is also set, paths starting with "drivers/"
// are first probed there. They then fall through to the managed directory
// and DriversDirOverride.
func (c *Config) ResolveDriverPaths(baseDir string) {
	for i := range c.Drivers {
		c.Drivers[i].Lua = stripLeadingDotDot(c.Drivers[i].Lua)
		p := c.Drivers[i].Lua
		if p == "" || filepath.IsAbs(p) {
			continue
		}
		if strings.HasPrefix(p, "drivers/") {
			rel := strings.TrimPrefix(p, "drivers/")
			if UserDriversDirOverride != "" {
				candidate := filepath.Join(UserDriversDirOverride, rel)
				if _, err := os.Stat(candidate); err == nil {
					c.Drivers[i].Lua = candidate
					continue
				}
			}
			if ManagedDriversDirOverride != "" {
				candidate := filepath.Join(ManagedDriversDirOverride, rel)
				if _, err := os.Stat(candidate); err == nil {
					c.Drivers[i].Lua = candidate
					continue
				}
			}
			if DriversDirOverride != "" {
				c.Drivers[i].Lua = filepath.Join(DriversDirOverride, rel)
				continue
			}
		}
		c.Drivers[i].Lua = filepath.Join(baseDir, p)
	}
}

func stripLeadingDotDot(p string) string {
	for strings.HasPrefix(p, "../") {
		p = p[3:]
	}
	return p
}

// UnresolveDriverPaths converts resolved driver paths back to config-relative form.
//
// Paths that are outside baseDir (filepath.Rel would yield a ../-prefixed
// result) are left absolute — otherwise the next ResolveDriverPaths would
// strip the leading ../ via stripLeadingDotDot and silently re-anchor the
// driver under baseDir. When DriversDirOverride is set, paths resolved
// through it are rewritten back to "drivers/<basename>" so the YAML + UI
// round-trip stays portable (no /app/drivers/... baked into config.yaml).
func (c *Config) UnresolveDriverPaths(baseDir string) {
	for i := range c.Drivers {
		p := c.Drivers[i].Lua
		if p != "" {
			// Check UserDriversDirOverride first so that user-dir paths are
			// re-serialised as portable "drivers/<rel>" just like bundled paths.
			if UserDriversDirOverride != "" {
				rel, err := filepath.Rel(UserDriversDirOverride, p)
				if err == nil && !strings.HasPrefix(rel, "..") {
					c.Drivers[i].Lua = filepath.ToSlash(filepath.Join("drivers", rel))
					continue
				}
			}
			if ManagedDriversDirOverride != "" {
				rel, err := filepath.Rel(ManagedDriversDirOverride, p)
				if err == nil && !strings.HasPrefix(rel, "..") {
					c.Drivers[i].Lua = filepath.ToSlash(filepath.Join("drivers", rel))
					continue
				}
			}
			if DriversDirOverride != "" {
				rel, err := filepath.Rel(DriversDirOverride, p)
				if err == nil && !strings.HasPrefix(rel, "..") {
					c.Drivers[i].Lua = filepath.ToSlash(filepath.Join("drivers", rel))
					continue
				}
			}
		}
		c.Drivers[i].Lua = relToBaseDir(baseDir, p)
	}
}

func relToBaseDir(baseDir, p string) string {
	if p == "" {
		return p
	}
	rel, err := filepath.Rel(baseDir, p)
	if err != nil {
		return p
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	return rel
}

// applyDefaults fills in sensible zero-value defaults.
func applyDefaults(c *Config) {
	if c.DeviceRepository == nil {
		// The official signed stable catalog is safe to discover by default:
		// refresh is read-only and never activates or restarts a driver. An
		// explicit enabled:false block remains the operator opt-out.
		c.DeviceRepository = &DeviceRepository{Enabled: true}
	}
	if c.DeviceRepository != nil {
		// Move only the exact built-in FTW source. Sites that changed the URL,
		// trust root, security flags, name, or source count keep their config.
		if len(c.DeviceRepository.Repositories) == 1 && isLegacyDefaultDriverRepository(c.DeviceRepository.Repositories[0]) {
			c.DeviceRepository.Repositories[0].Name = DefaultDriverRepositoryName
			c.DeviceRepository.Repositories[0].ManifestURL = DefaultDriverRepositoryManifestURL
		}
		if c.DeviceRepository.RefreshIntervalH == 0 {
			c.DeviceRepository.RefreshIntervalH = 24
		}
		// The pinned official trust root is a secure default and needs no key
		// copied into every site configuration.
		if len(c.DeviceRepository.Repositories) == 0 {
			c.DeviceRepository.Repositories = []DriverRepositorySource{{
				ID:          DefaultDriverRepositoryID,
				Name:        DefaultDriverRepositoryName,
				ManifestURL: DefaultDriverRepositoryManifestURL,
				Enabled:     true,
				TrustedKeys: map[string]string{
					DefaultDriverRepositorySigningKeyID: DefaultDriverRepositoryPublicKey,
				},
			}}
		}
	}
	if c.AppLink == nil {
		// The app relay carries only end-to-end encrypted frames under a handle
		// that changes every hour. It still sees connection metadata. Existing
		// sites with no app_link section join the supported remote path after
		// upgrading; an explicit enabled:false block remains the operator opt-out.
		c.AppLink = &AppLink{Enabled: true}
	}
	if c.FleetPing == nil {
		// Absent means never configured, which is the state every existing
		// box is in. Enabled is the owner's call; an explicit
		// `fleet_ping: {enabled: false}` block is the opt-out and survives
		// this because only a missing section is filled in.
		c.FleetPing = &FleetPing{Enabled: true}
	}
	// Endpoint is deliberately left blank: Resolved() supplies the default at
	// use, so the collector's address is never written into a household's YAML.
	if c.Site.ControlIntervalS == 0 {
		// 2 s matches Ferroamp's ehub MQTT cadence (~1 Hz) without
		// dispatching twice on the same telemetry sample, and halves
		// the perceived response lag operators saw at the original 5 s.
		c.Site.ControlIntervalS = 2
	}
	if c.Site.GridToleranceW == 0 {
		c.Site.GridToleranceW = 42 // The Answer
	}
	if c.Site.WatchdogTimeoutS == 0 {
		c.Site.WatchdogTimeoutS = 60
	}
	if c.Site.SmoothingAlpha == 0 {
		c.Site.SmoothingAlpha = 0.3
	}
	if c.Site.Gain == 0 {
		c.Site.Gain = 0.5
	}
	if c.Site.SlewRateW == 0 {
		// 3000 W/cycle at the 2 s default control interval = 1500 W/s
		// ramp ceiling. Both Ferroamp and Sungrow internal EMS loops
		// ramp slower than this naturally (Sungrow spec: ~1000 W/s),
		// so the external slew rarely fires under normal conditions
		// but still bounds the post-windup recovery from snapping to
		// full output in a single cycle.
		c.Site.SlewRateW = 3000
	}
	if c.Site.SlewEnabled == nil {
		t := true
		c.Site.SlewEnabled = &t
	}
	if c.Site.MinDispatchIntervalS == 0 {
		// Match control_interval_s. The holdoff exists to suppress
		// command-spam when the tick is faster than the battery's
		// response — at 2 s ticks the natural cadence is already the
		// minimum, so the holdoff is a no-op debouncer in practice.
		c.Site.MinDispatchIntervalS = 2
	}
	if c.Planner != nil && c.Planner.OptimizerTimeoutS == 0 {
		c.Planner.OptimizerTimeoutS = optimizercontract.DefaultTimeout.Seconds()
	}
	if c.Fuse.Phases == 0 {
		c.Fuse.Phases = 3
	}
	if c.Fuse.Voltage == 0 {
		c.Fuse.Voltage = 230
	}
	if c.API.Port == 0 {
		c.API.Port = 8080
	}
	// Driver connection defaults
	for i := range c.Drivers {
		d := &c.Drivers[i]
		if cap := d.Capabilities.MQTT; cap != nil && cap.Port == 0 {
			cap.Port = 1883
		}
		if cap := d.Capabilities.Modbus; cap != nil {
			if cap.Port == 0 {
				cap.Port = 502
			}
			if cap.UnitID == 0 {
				cap.UnitID = 1
			}
		}
		if cap := d.Capabilities.Serial; cap != nil {
			if cap.BaudRate == 0 {
				cap.BaudRate = 115200
			}
			if cap.DataBits == 0 {
				cap.DataBits = 8
			}
			if cap.StopBits == 0 {
				cap.StopBits = 1
			}
			if cap.Parity == "" {
				cap.Parity = "N"
			}
			if cap.ReadTimeoutMS == 0 {
				cap.ReadTimeoutMS = 500
			}
		}
		if cap := d.MQTT; cap != nil && cap.Port == 0 {
			cap.Port = 1883
		}
		if cap := d.Modbus; cap != nil {
			if cap.Port == 0 {
				cap.Port = 502
			}
			if cap.UnitID == 0 {
				cap.UnitID = 1
			}
		}
	}
	if c.HomeAssistant != nil {
		if c.HomeAssistant.Port == 0 {
			c.HomeAssistant.Port = 1883
		}
		if c.HomeAssistant.PublishIntervalS == 0 {
			c.HomeAssistant.PublishIntervalS = 5
		}
	}
	// Backfill for configs that predate notifications: — lands a
	// populated-but-disabled stub so upgrading an existing install
	// lights up the Notifications tab with the defaults instead of an
	// empty form. Nothing is written to disk until the operator Saves.
	if c.Notifications == nil {
		c.Notifications = &Notifications{
			Enabled:         false,
			Provider:        "ntfy",
			DefaultPriority: 3,
			Ntfy:            &NtfyConfig{Server: "https://ntfy.sh"},
			Events: []NotificationRule{
				{Type: "driver_offline", Enabled: false, ThresholdS: 600, Priority: 4, CooldownS: 3600},
				{Type: "driver_recovered", Enabled: false, Priority: 3},
				{Type: "update_available", Enabled: false, Priority: 3, CooldownS: 3600},
				{Type: "fuse_over_limit", Enabled: false, ThresholdS: 30, Priority: 5, CooldownS: 900},
			},
		}
	}
	// Rule-list migration: add new built-in event types to existing
	// configs that predate them so upgrading lights up the toggle in
	// Settings → Notifications instead of needing manual YAML edits.
	if c.Notifications != nil {
		builtins := []NotificationRule{
			{Type: "driver_offline", Enabled: false, ThresholdS: 600, Priority: 4, CooldownS: 3600},
			{Type: "driver_recovered", Enabled: false, Priority: 3},
			{Type: "update_available", Enabled: false, Priority: 3, CooldownS: 3600},
			{Type: "fuse_over_limit", Enabled: false, ThresholdS: 30, Priority: 5, CooldownS: 900},
		}
		have := make(map[string]bool, len(c.Notifications.Events))
		for _, r := range c.Notifications.Events {
			have[r.Type] = true
		}
		for _, b := range builtins {
			if !have[b.Type] {
				c.Notifications.Events = append(c.Notifications.Events, b)
			}
		}
	}
	if c.Notifications != nil {
		if c.Notifications.Provider == "" {
			c.Notifications.Provider = "ntfy"
		}
		if c.Notifications.DefaultPriority == 0 {
			c.Notifications.DefaultPriority = 3
		}
		if c.Notifications.Provider == "ntfy" {
			if c.Notifications.Ntfy == nil {
				c.Notifications.Ntfy = &NtfyConfig{}
			}
			if c.Notifications.Ntfy.Server == "" {
				c.Notifications.Ntfy.Server = "https://ntfy.sh"
			}
		}
	}
	if c.Nova != nil {
		if c.Nova.MQTTPort == 0 {
			c.Nova.MQTTPort = 1883
		}
		if c.Nova.SchemaMode == "" {
			c.Nova.SchemaMode = "legacy"
		}
		if c.Nova.PublishIntervalS == 0 {
			c.Nova.PublishIntervalS = 5
		}
		if c.Nova.ReconcileIntervalH == 0 {
			c.Nova.ReconcileIntervalH = 24
		}
	}
}

func isLegacyDefaultDriverRepository(repo DriverRepositorySource) bool {
	if repo.ID != DefaultDriverRepositoryID || repo.ManifestURL != legacyDriverRepositoryManifestURL ||
		(repo.Name != "" && repo.Name != legacyDriverRepositoryName) ||
		(repo.Format != "" && repo.Format != DriverRepositoryFormatFTWManifestV1) ||
		repo.AllowUnsigned || repo.AllowInsecure || len(repo.TrustedKeys) != 1 {
		return false
	}
	return repo.TrustedKeys[DefaultDriverRepositorySigningKeyID] == DefaultDriverRepositoryPublicKey
}

// Validate ensures the config is internally consistent and safe to run with.
func (c *Config) Validate() error {
	c.NormalizeUnits()
	if c.State != nil && c.State.ColdRetentionDays < 0 {
		return fmt.Errorf("state.cold_retention_days must be >= 0, got %d", c.State.ColdRetentionDays)
	}
	if c.EVCharger != nil {
		c.EVCharger.Normalize()
		if err := c.EVCharger.Validate(); err != nil {
			return err
		}
	}
	if err := c.CalDAV.Validate(); err != nil {
		return err
	}
	if err := c.FleetPing.Validate(); err != nil {
		return err
	}
	if err := c.OCPP.Validate(); err != nil {
		return err
	}
	if err := c.Assistant.Validate(); err != nil {
		return err
	}
	if c.Price != nil {
		if err := c.Price.Validate(); err != nil {
			return err
		}
	}
	if err := c.validateVehicles(); err != nil {
		return err
	}

	// Empty drivers list is a valid shape — e.g. an EV-only site that
	// configured a cloud EV charger in the setup wizard and doesn't
	// own local inverter/meter hardware. Control loop becomes a no-op
	// (SiteMeterDriver() returns "" and telemetry lookups just miss);
	// the site meter check below only fires once at least one driver
	// exists.
	siteMeters := 0
	names := make(map[string]bool, len(c.Drivers))
	for _, d := range c.Drivers {
		if d.Name == "" {
			return errors.New("driver: name is required")
		}
		if names[d.Name] {
			return fmt.Errorf("driver %q: duplicate name", d.Name)
		}
		names[d.Name] = true

		if d.IsSiteMeter {
			siteMeters++
		}
		if d.Lua == "" {
			return fmt.Errorf("driver %q: must specify `lua`", d.Name)
		}
		if d.Control != nil && d.Control.Enabled {
			if !strings.HasPrefix(d.Control.PackageID, "com.sourceful.driver.") || d.Control.Version == "" {
				return fmt.Errorf("driver %q: control requires an exact Sourceful package_id and version", d.Name)
			}
			hash, err := hex.DecodeString(strings.ToLower(strings.TrimSpace(d.Control.ArtifactSHA256)))
			if err != nil || len(hash) != 32 {
				return fmt.Errorf("driver %q: control artifact_sha256 must be 64 hexadecimal characters", d.Name)
			}
		}
		if d.EffectiveMQTT() == nil && d.EffectiveModbus() == nil &&
			d.Capabilities.Serial == nil && !d.Capabilities.Standalone &&
			d.Capabilities.HTTP == nil && d.Capabilities.WebSocket == nil &&
			d.Capabilities.TCP == nil {
			return fmt.Errorf("driver %q: must have mqtt, modbus, serial, http, websocket, tcp, or standalone capability", d.Name)
		}
		if serial := d.Capabilities.Serial; serial != nil {
			if strings.TrimSpace(serial.Address) == "" {
				return fmt.Errorf("driver %q: serial address is required", d.Name)
			}
			if serial.BaudRate <= 0 || serial.ReadTimeoutMS <= 0 || serial.ReadTimeoutMS > 60_000 {
				return fmt.Errorf("driver %q: serial baud_rate and read_timeout_ms must be valid", d.Name)
			}
			if serial.DataBits < 5 || serial.DataBits > 8 || (serial.StopBits != 1 && serial.StopBits != 2) {
				return fmt.Errorf("driver %q: serial data_bits or stop_bits is invalid", d.Name)
			}
			switch strings.ToUpper(serial.Parity) {
			case "N", "E", "O":
			default:
				return fmt.Errorf("driver %q: serial parity must be N, E, or O", d.Name)
			}
		}
		if d.ObserveOnly && d.BatteryCapacityWh <= 0 {
			return fmt.Errorf("driver %q: observe_only requires battery_capacity_wh > 0", d.Name)
		}
	}
	if len(c.Drivers) > 0 && siteMeters == 0 {
		return errors.New("at least one driver must be is_site_meter: true")
	}
	// SiteMeterDriver() returns the first match, so a second is_site_meter
	// entry was silently ignored — the operator thinks meter B is the site
	// boundary while dispatch trusts meter A. Make the ambiguity an error.
	if siteMeters > 1 {
		return fmt.Errorf("exactly one driver may set is_site_meter: true (found %d)", siteMeters)
	}

	if c.Site.ControlIntervalS < 0 {
		return errors.New("site.control_interval_s must be >= 0")
	}
	if c.Site.GridToleranceW < 0 {
		return errors.New("site.grid_tolerance_w must be >= 0")
	}
	if c.Site.WatchdogTimeoutS < 0 {
		return errors.New("site.watchdog_timeout_s must be >= 0")
	}
	if c.Site.SmoothingAlpha <= 0 || c.Site.SmoothingAlpha > 1 {
		return errors.New("site.smoothing_alpha must be in (0, 1]")
	}
	if c.Site.Gain < 0 {
		return errors.New("site.gain must be >= 0")
	}
	if c.Site.SlewRateW < 0 {
		return errors.New("site.slew_rate_w must be >= 0")
	}
	if c.Site.MinDispatchIntervalS < 0 {
		return errors.New("site.min_dispatch_interval_s must be >= 0")
	}
	if c.Fuse.MaxAmps <= 0 {
		return errors.New("fuse.max_amps must be > 0")
	}
	if c.Fuse.Phases <= 0 {
		return errors.New("fuse.phases must be > 0")
	}
	// The phase-current safety model ([3]float64 arrays, meter_l1..l3_a
	// metrics) covers 1..3 conductors. A larger value used to be silently
	// truncated to 3 by the dispatch freshness gate while MaxPowerW still
	// counted every configured phase. That overstated the usable aggregate
	// fuse budget. Reject it instead.
	if c.Fuse.Phases > 3 {
		return errors.New("fuse.phases must be 1, 2 or 3")
	}
	if c.Fuse.Voltage <= 0 {
		return errors.New("fuse.voltage must be > 0")
	}
	// safety_margin_a must be in [0, max_amps) when explicitly set.
	// Negative would *raise* the per-phase threshold above the breaker
	// rating (defeating the guard); >= max_amps zeroes the headroom
	// and silently disables the per-phase clamp — both are real safety
	// holes if reached through a typo'd config. nil (unset) is OK and
	// resolves to DefaultFuseSafetyMarginA at the consumer.
	if c.Fuse.SafetyMarginA != nil {
		v := *c.Fuse.SafetyMarginA
		if v < 0 {
			return errors.New("fuse.safety_margin_a must be >= 0")
		}
		if v >= c.Fuse.MaxAmps {
			return errors.New("fuse.safety_margin_a must be < fuse.max_amps")
		}
	}
	if err := c.validateV2XPolicy(names); err != nil {
		return err
	}
	if n := c.Notifications; n != nil {
		if n.DefaultPriority < 0 || n.DefaultPriority > 5 {
			return errors.New("notifications.default_priority must be in [0,5]")
		}
		if n.Enabled {
			switch n.Provider {
			case "", "ntfy":
				// Web push is engine-owned — keyed by stored push
				// subscriptions, not by this field — so "enabled" no
				// longer implies a working ntfy. The box delivers over web
				// push with no provider configured at all. Ntfy is a
				// secondary, opt-in channel, and its one indispensable
				// setting is the topic: it has no default and nothing can
				// be published without it. So ntfy counts as *active* only
				// once a topic is set. Until then it is inactive, and a box
				// carrying the legacy default (provider "ntfy", server
				// "https://ntfy.sh", blank topic — present on real boxes)
				// can still enable notifications. NewProvider makes the
				// same call at runtime and logs the inactive ntfy, so the
				// drop is warned, not silent.
				if n.Ntfy != nil && strings.TrimSpace(n.Ntfy.Topic) != "" {
					// A topic means the operator does intend ntfy; a topic
					// with no server to publish it to is a real mistake
					// still worth catching.
					if strings.TrimSpace(n.Ntfy.Server) == "" {
						return errors.New("notifications.ntfy.server required when notifications.ntfy.topic is set")
					}
				}
			default:
				return fmt.Errorf("notifications.provider %q not supported", n.Provider)
			}
		}
		for i, ev := range n.Events {
			if strings.TrimSpace(ev.Type) == "" {
				return fmt.Errorf("notifications.events[%d]: type required", i)
			}
			if ev.ThresholdS < 0 {
				return fmt.Errorf("notifications.events[%d]: threshold_s must be >= 0", i)
			}
			if ev.Priority < 0 || ev.Priority > 5 {
				return fmt.Errorf("notifications.events[%d]: priority must be in [0,5]", i)
			}
			if ev.CooldownS < 0 {
				return fmt.Errorf("notifications.events[%d]: cooldown_s must be >= 0", i)
			}
		}
	}
	if c.Nova != nil && c.Nova.Enabled {
		if c.Nova.URL == "" {
			return errors.New("nova.url is required when nova.enabled")
		}
		if c.Nova.MQTTHost == "" {
			return errors.New("nova.mqtt_host is required when nova.enabled")
		}
		if c.Nova.GatewaySerial == "" {
			return errors.New("nova.gateway_serial is required when nova.enabled — run `ftw nova-claim`")
		}
		if c.Nova.OrgID == "" {
			return errors.New("nova.org_id is required when nova.enabled")
		}
		if c.Nova.SiteID == "" {
			return errors.New("nova.site_id is required when nova.enabled")
		}
		switch c.Nova.SchemaMode {
		case "legacy", "unified":
		default:
			return fmt.Errorf("nova.schema_mode must be \"legacy\" or \"unified\", got %q", c.Nova.SchemaMode)
		}
	}
	if c.Planner != nil && c.Planner.MinArbitrageSpreadOreKwh < 0 {
		return fmt.Errorf("planner.min_arbitrage_spread_ore_kwh must be ≥ 0, got %g", c.Planner.MinArbitrageSpreadOreKwh)
	}
	if c.Planner != nil {
		p := c.Planner
		if p.ForecastTrust != "" {
			if _, ok := ParseForecastTrust(p.ForecastTrust); !ok {
				return fmt.Errorf("planner.forecast_trust must be cautious, balanced, or bold, got %q", p.ForecastTrust)
			}
		}
		if p.BatteryExport != "" {
			if _, ok := ParseBatteryExport(p.BatteryExport); !ok {
				return fmt.Errorf("planner.battery_export must be unknown, not_allowed, or allowed, got %q", p.BatteryExport)
			}
		}
		switch strings.ToLower(strings.TrimSpace(p.Engine)) {
		case "", PlannerEngineCore, "go", "dp", PlannerEnginePython:
		default:
			return fmt.Errorf("planner.engine must be %q or %q, got %q",
				PlannerEngineCore, PlannerEnginePython, p.Engine)
		}
		switch strings.ToUpper(p.OptimizerSolver) {
		case "", "HIGHS", "CLARABEL":
		default:
			return fmt.Errorf("planner.optimizer_solver must be \"HIGHS\" or \"CLARABEL\", got %q", p.OptimizerSolver)
		}
		switch p.OptimizerFormulation {
		case "", "auto", "milp", "relaxed":
		default:
			return fmt.Errorf("planner.optimizer_formulation must be auto, milp, or relaxed, got %q", p.OptimizerFormulation)
		}
		switch p.OptimizerTransport {
		case "", "auto", "unix", "process":
		default:
			return fmt.Errorf("planner.optimizer_transport must be auto, unix, or process, got %q", p.OptimizerTransport)
		}
		if p.OptimizerTimeoutS < 0 || p.OptimizerIdleTimeoutS < 0 || p.OptimizerMIPRelGap < 0 || (p.OptimizerCVaRWeight != nil && *p.OptimizerCVaRWeight < 0) {
			return errors.New("planner optimizer timeout, idle timeout, MIP gap, and CVaR weight must be non-negative")
		}
		if p.OptimizerMIPRelGap > 1 {
			return fmt.Errorf("planner.optimizer_mip_rel_gap must be <= 1, got %g", p.OptimizerMIPRelGap)
		}
		if p.OptimizerCVaRAlpha < 0 || p.OptimizerCVaRAlpha >= 1 {
			return fmt.Errorf("planner.optimizer_cvar_alpha must be 0 (default) or in (0,1), got %g", p.OptimizerCVaRAlpha)
		}
		if p.OptimizerRecourseNonAnticipativeSlots < 0 {
			return errors.New("planner.optimizer_recourse_non_anticipative_slots must be non-negative")
		}
		switch p.OptimizerChallengerPolicy {
		case "", "recourse", "multistage":
		default:
			return fmt.Errorf("planner.optimizer_challenger_policy must be recourse or multistage, got %q", p.OptimizerChallengerPolicy)
		}
		if ms := p.OptimizerMultistage; ms != nil {
			ints := []int{ms.ScenarioLimit, ms.BranchIntervalSlots, ms.BranchHorizonSlots,
				ms.MaxBranching, ms.NearHorizonSlots, ms.MidHorizonSlots, ms.MidBlockSlots,
				ms.FarBlockSlots, ms.DecompositionThreshold, ms.PHMaxIterations}
			for _, value := range ints {
				if value < 0 {
					return errors.New("planner.optimizer_multistage integer settings must be non-negative")
				}
			}
			if ms.MaxBranching == 1 {
				return errors.New("planner.optimizer_multistage.max_branching must be 0 (default) or at least 2")
			}
			if (ms.ServiceCVaRWeight != nil && *ms.ServiceCVaRWeight < 0) || ms.EconomicCVaRWeight < 0 || ms.PHRho < 0 || ms.PHToleranceW < 0 {
				return errors.New("planner.optimizer_multistage risk weights, PH rho, and PH tolerance must be non-negative")
			}
			if (ms.ServiceCVaRAlpha < 0 || ms.ServiceCVaRAlpha >= 1) ||
				(ms.EconomicCVaRAlpha < 0 || ms.EconomicCVaRAlpha >= 1) {
				return errors.New("planner.optimizer_multistage CVaR alpha must be 0 (default) or in (0,1)")
			}
			switch ms.DecompositionMethod {
			case "", "auto", "extensive", "progressive_hedging":
			default:
				return fmt.Errorf("planner.optimizer_multistage.decomposition_method is invalid: %q", ms.DecompositionMethod)
			}
		}
	}
	if repoCfg := c.DeviceRepository; repoCfg != nil {
		if repoCfg.RefreshIntervalH < 0 {
			return errors.New("device_repository.refresh_interval_h must be non-negative")
		}
		seen := make(map[string]bool, len(repoCfg.Repositories))
		for _, repo := range repoCfg.Repositories {
			if repo.ID == "" || strings.ContainsAny(repo.ID, "/\\") {
				return fmt.Errorf("device_repository repository has invalid id %q", repo.ID)
			}
			if seen[repo.ID] {
				return fmt.Errorf("device_repository has duplicate repository id %q", repo.ID)
			}
			seen[repo.ID] = true
			if !repo.Enabled {
				continue
			}
			switch repo.Format {
			case "", DriverRepositoryFormatFTWManifestV1, DriverRepositoryFormatSourcefulIndexV1:
			default:
				return fmt.Errorf("device_repository %s has unsupported format %q", repo.ID, repo.Format)
			}
			u, err := url.Parse(repo.ManifestURL)
			if err != nil || u.Scheme == "" {
				return fmt.Errorf("device_repository %s has invalid manifest_url", repo.ID)
			}
			if u.Scheme != "https" && !repo.AllowInsecure {
				return fmt.Errorf("device_repository %s manifest_url must use https", repo.ID)
			}
			if repo.AllowUnsigned && u.Scheme != "file" {
				return fmt.Errorf("device_repository %s allow_unsigned is restricted to local file manifests", repo.ID)
			}
			if repo.Format == DriverRepositoryFormatSourcefulIndexV1 && repo.AllowUnsigned {
				return fmt.Errorf("device_repository %s Sourceful indexes must be signed", repo.ID)
			}
			if !repo.AllowUnsigned && len(repo.TrustedKeys) == 0 {
				return fmt.Errorf("device_repository %s requires at least one trusted Ed25519 key", repo.ID)
			}
		}
	}
	return nil
}

func (c *Config) validateV2XPolicy(driverNames map[string]bool) error {
	p := c.V2X
	if p == nil {
		return nil
	}
	if p.DriverName != "" && !driverNames[p.DriverName] {
		return fmt.Errorf("v2x.driver_name %q: no such driver", p.DriverName)
	}
	for name, value := range map[string]float64{
		"v2x.vehicle_capacity_wh":  p.VehicleCapacityWh,
		"v2x.max_charge_w":         p.MaxChargeW,
		"v2x.max_discharge_w":      p.MaxDischargeW,
		"v2x.cycle_cost_ore_kwh":   p.CycleCostOreKWh,
		"v2x.min_reserve_soc":      p.MinReserveSoC,
		"v2x.departure_target_soc": p.DepartureTargetSoC,
	} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("%s must be finite", name)
		}
	}
	if p.VehicleCapacityWh < 0 {
		return errors.New("v2x.vehicle_capacity_wh must be >= 0")
	}
	if p.MaxChargeW < 0 {
		return errors.New("v2x.max_charge_w must be >= 0")
	}
	if p.MaxDischargeW < 0 {
		return errors.New("v2x.max_discharge_w must be >= 0")
	}
	if p.CycleCostOreKWh < 0 {
		return errors.New("v2x.cycle_cost_ore_kwh must be >= 0")
	}
	if p.MinReserveSoC < 0 || p.MinReserveSoC > 1 {
		return errors.New("v2x.min_reserve_soc must be in [0,1]")
	}
	if p.DepartureTargetSoC < 0 || p.DepartureTargetSoC > 1 {
		return errors.New("v2x.departure_target_soc must be in [0,1]")
	}
	if p.Enabled && p.MinReserveSoC <= 0 {
		return errors.New("v2x.min_reserve_soc must be > 0 when v2x.enabled")
	}
	if p.DepartureTime != "" {
		if err := validateV2XDepartureTime(p.DepartureTime); err != nil {
			return err
		}
	}
	if (p.DepartureTargetSoC > 0) != (p.DepartureTime != "") {
		return errors.New("v2x.departure_target_soc and v2x.departure_time must be set together")
	}
	if p.DepartureTargetSoC > 0 && p.DepartureTargetSoC < p.MinReserveSoC {
		return errors.New("v2x.departure_target_soc must be >= v2x.min_reserve_soc")
	}
	return nil
}

func validateV2XDepartureTime(value string) error {
	if _, err := time.Parse("15:04", value); err == nil {
		return nil
	}
	if _, err := time.Parse(time.RFC3339, value); err == nil {
		return nil
	}
	return fmt.Errorf("v2x.departure_time must be HH:MM or RFC3339, got %q", value)
}

// SiteMeterDriver returns the name of the driver marked is_site_meter.
func (c *Config) SiteMeterDriver() string {
	for _, d := range c.Drivers {
		if d.IsSiteMeter {
			return d.Name
		}
	}
	return ""
}

// configFileMode is owner-only because config.yaml carries MQTT passwords,
// API keys and OAuth refresh tokens. Rename replaces the destination inode, so
// whatever mode the temp file has is the mode the saved config ends up with.
const configFileMode os.FileMode = 0o600

// saveMu serializes config saves. The settings handlers do not hold a write
// lock across a save, so two overlapping requests would otherwise both write
// the shared temp path and rename half of each other's bytes over config.yaml.
var saveMu sync.Mutex

// durableWriter holds the two sync calls that make a save survive power loss.
// They are fields so a test can prove the ordering and force a sync failure;
// production always uses defaultDurableWriter.
type durableWriter struct {
	syncFile func(*os.File) error
	syncDir  func(string) error
}

var defaultDurableWriter = durableWriter{
	syncFile: (*os.File).Sync,
	syncDir:  syncDir,
}

// SaveAtomic writes config to disk via tmp-file + rename. Safe from partial
// writes and from power loss: the temp file is fsynced before the rename and
// the containing directory is fsynced after it.
func SaveAtomic(path string, c *Config) error {
	return saveAtomic(defaultDurableWriter, path, c)
}

func saveAtomic(w durableWriter, path string, c *Config) error {
	// Driver paths are resolved to absolute-ish paths at Load() time.
	// Convert them back to config-relative before writing so that
	// repeated save cycles don't accumulate extra "../" prefixes.
	baseDir := filepath.Dir(path)
	out := *c
	if len(out.Drivers) > 0 {
		drivers := make([]Driver, len(out.Drivers))
		copy(drivers, out.Drivers)
		for i := range drivers {
			drivers[i].Lua = relDriverPath(baseDir, drivers[i].Lua)
		}
		out.Drivers = drivers
	}
	data, err := yaml.Marshal(&out)
	if err != nil {
		return fmt.Errorf("yaml marshal: %w", err)
	}
	saveMu.Lock()
	defer saveMu.Unlock()

	tmp := path + ".tmp"
	// Clear any temp left by an interrupted save, then create with O_EXCL.
	// OpenFile only applies the mode when it creates the file, so reusing a
	// stale 0644 temp would hand the secrets in config.yaml to every user on
	// the box; O_EXCL also refuses to follow a symlink planted at that path.
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear stale tmp: %w", err)
	}
	f, err := createConfigTemp(tmp, configFileMode)
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write tmp: %w", err)
	}
	// fsync before rename: a rename is only atomic for bytes that have already
	// reached the disk. Without this, a power cut mid-save can publish a
	// truncated or zero-length config.yaml — the file the gateway boots from,
	// on a device that is expected to come back up unattended.
	if err := w.syncFile(f); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("sync tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := replaceConfigTemp(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename tmp: %w", err)
	}
	// fsync the directory so the rename itself survives power loss. The
	// caller's contract is "the config is now saved", so this failure is
	// reported rather than swallowed.
	if err := w.syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync config dir: %w", err)
	}
	return nil
}

func relDriverPath(baseDir, p string) string {
	if p == "" {
		return ""
	}
	// Paths resolved through UserDriversDirOverride or DriversDirOverride
	// land outside baseDir, so a straight Rel would emit "../drivers/<name>.lua".
	// Rewrite them as a clean "drivers/<rel>" to keep YAML portable between hosts.
	if UserDriversDirOverride != "" {
		rel, err := filepath.Rel(UserDriversDirOverride, p)
		if err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(filepath.Join("drivers", rel))
		}
	}
	if DriversDirOverride != "" {
		rel, err := filepath.Rel(DriversDirOverride, p)
		if err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(filepath.Join("drivers", rel))
		}
	}
	rel, err := filepath.Rel(baseDir, p)
	if err != nil {
		return p
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	return rel
}
