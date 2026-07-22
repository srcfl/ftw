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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	V2X              *V2XPolicy         `yaml:"v2x,omitempty" json:"v2x,omitempty"`
	Notifications    *Notifications     `yaml:"notifications,omitempty" json:"notifications,omitempty"`
	Nova             *Nova              `yaml:"nova,omitempty" json:"nova,omitempty"`
	DeviceRepository *DeviceRepository  `yaml:"device_repository,omitempty" json:"device_repository,omitempty"`
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
	ID                string    `yaml:"id" json:"id"`
	DriverName        string    `yaml:"driver_name" json:"driver_name"`
	MinChargeW        float64   `yaml:"min_charge_w,omitempty" json:"min_charge_w,omitempty"`
	MaxChargeW        float64   `yaml:"max_charge_w,omitempty" json:"max_charge_w,omitempty"`
	AllowedStepsW     []float64 `yaml:"allowed_steps_w,omitempty" json:"allowed_steps_w,omitempty"`
	VehicleCapacityWh float64   `yaml:"vehicle_capacity_wh,omitempty" json:"vehicle_capacity_wh,omitempty"`
	PluginSoCPct      float64   `yaml:"plugin_soc_pct,omitempty" json:"plugin_soc_pct,omitempty"`

	// PhaseMode selects how the controller picks between 1Φ and 3Φ
	// delivery: "3p" (default) | "1p" | "auto". Empty == "3p" for
	// backward compat with pre-switching configs. See loadpoint.Config.
	PhaseMode     string  `yaml:"phase_mode,omitempty" json:"phase_mode,omitempty"`
	PhaseSplitW   float64 `yaml:"phase_split_w,omitempty" json:"phase_split_w,omitempty"`
	MinPhaseHoldS int     `yaml:"min_phase_hold_s,omitempty" json:"min_phase_hold_s,omitempty"`
	SurplusOnly   bool    `yaml:"surplus_only,omitempty" json:"surplus_only,omitempty"`
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

	// SoC percentages are YAML-facing 0..100 values. Telemetry stays 0..1.
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
	Provider string `yaml:"provider" json:"provider"` // "easee" | "ctek"

	// Connection — populate the block matching the provider's transport.
	HTTP   *EVChargerHTTP   `yaml:"http,omitempty" json:"http,omitempty"`
	Modbus *EVChargerModbus `yaml:"modbus,omitempty" json:"modbus,omitempty"`

	// Optional auth — required by cloud HTTP providers like Easee,
	// unused by local Modbus providers like CTEK.
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

	// EVDefaultTargetSoCPct is used when an EV event's title carries no
	// explicit percentage. Default 80.
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
	DefaultCalDAVEVTargetSoC  = 80.0
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
	if cv.EVDefaultTargetSoCPct < 0 || cv.EVDefaultTargetSoCPct > 100 {
		return errors.New("caldav.ev_default_target_soc_pct must be in [0, 100]")
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
	case "easee":
		// Username/Password are NOT enforced here. The runtime easee
		// driver logs + idles when creds are missing, and the API picker
		// requires both before calling Easee Cloud. Letting a partial
		// ev_charger block load is the original contract — the wizard
		// writes provider intent first, then captures creds in a second
		// API call.
		if e.Modbus != nil {
			return errors.New("ev_charger.modbus: not valid for provider easee (HTTP transport)")
		}
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
		return fmt.Errorf("ev_charger.provider %q: not supported (valid: easee, ctek)", e.Provider)
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
	// Engine selects the primary optimizer: "python" (default) runs the
	// CVXPY/HiGHS worker; "dp" is the legacy in-process rollback engine.
	Engine string `yaml:"engine,omitempty" json:"engine,omitempty"`
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
	// SoC %. Pointer so unset (→ default 1.0) is distinct from an explicit
	// 0 (= raw forecast, no hedge: "use the battery you have").
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

// PVSafetyK resolves the downside-PV haircut scale (forecast − k·σ). Unset
// config (nil Planner or nil field) → default 1.0; an explicit value is
// honored verbatim, including 0 (no hedge — "use the battery you have").
func (p *Planner) PVSafetyK() float64 {
	if p == nil || p.PVForecastSafetyK == nil {
		return 1.0
	}
	return *p.PVForecastSafetyK
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
	// Suggested 88 — leaves 2 pp margin below the planner's typical
	// soc_max_pct = 90 so the absorber doesn't slam into the wall.
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
	MQTT       *MQTTConfig     `yaml:"mqtt,omitempty" json:"mqtt,omitempty"`
	Modbus     *ModbusConfig   `yaml:"modbus,omitempty" json:"modbus,omitempty"`
	Serial     *SerialConfig   `yaml:"serial,omitempty" json:"serial,omitempty"`
	HTTP       *HTTPCapability `yaml:"http,omitempty" json:"http,omitempty"`
	WebSocket  *WSCapability   `yaml:"websocket,omitempty" json:"websocket,omitempty"`
	TCP        *TCPCapability  `yaml:"tcp,omitempty" json:"tcp,omitempty"`
	Standalone bool            `yaml:"standalone,omitempty" json:"standalone,omitempty"`
}

// MQTTConfig grants access to one MQTT broker.
type MQTTConfig struct {
	Host     string `yaml:"host" json:"host"`
	Port     int    `yaml:"port,omitempty" json:"port,omitempty"` // default 1883
	Username string `yaml:"username,omitempty" json:"username,omitempty"`
	Password string `yaml:"password,omitempty" json:"password,omitempty"`
}

// ModbusConfig grants access to one Modbus TCP endpoint.
type ModbusConfig struct {
	Host   string `yaml:"host" json:"host"`
	Port   int    `yaml:"port,omitempty" json:"port,omitempty"`       // default 502
	UnitID int    `yaml:"unit_id,omitempty" json:"unit_id,omitempty"` // default 1
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
	Port int `yaml:"port" json:"port"`
}

// HomeAssistant is the MQTT bridge config.
type HomeAssistant struct {
	Enabled          bool   `yaml:"enabled" json:"enabled"`
	Broker           string `yaml:"broker" json:"broker"`
	Port             int    `yaml:"port,omitempty" json:"port,omitempty"`
	Username         string `yaml:"username,omitempty" json:"username,omitempty"`
	Password         string `yaml:"password,omitempty" json:"password,omitempty"`
	PublishIntervalS int    `yaml:"publish_interval_s,omitempty" json:"publish_interval_s,omitempty"`
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
	Provider         string  `yaml:"provider" json:"provider"` // sourceful | elprisetjustnu | entsoe | none
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
	// When set, PVArrays overrides the legacy single-array fields.
	// Providers that can't use site geometry (met_no, open_meteo)
	// ignore this entirely and just use PVRatedW.
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
// KWp values should match the total PV nameplate at the site.
type PVArray struct {
	Name       string  `yaml:"name,omitempty" json:"name,omitempty"`
	KWp        float64 `yaml:"kwp" json:"kwp"`
	TiltDeg    float64 `yaml:"tilt_deg" json:"tilt_deg"`
	AzimuthDeg float64 `yaml:"azimuth_deg" json:"azimuth_deg"`
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
	if out.Price != nil {
		cp := *out.Price
		cp.APIKey = ""
		out.Price = &cp
	}
	if out.Weather != nil {
		cp := *out.Weather
		cp.APIKey = ""
		out.Weather = &cp
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
	if incoming.HomeAssistant != nil && existing.HomeAssistant != nil && incoming.HomeAssistant.Password == "" {
		incoming.HomeAssistant.Password = existing.HomeAssistant.Password
	}
	if incoming.Price != nil && existing.Price != nil && incoming.Price.APIKey == "" {
		incoming.Price.APIKey = existing.Price.APIKey
	}
	if incoming.Weather != nil && existing.Weather != nil && incoming.Weather.APIKey == "" {
		incoming.Weather.APIKey = existing.Weather.APIKey
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
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}
	applyDefaults(&c)
	if err := c.Validate(); err != nil {
		return nil, err
	}
	c.ResolveDriverPaths(baseDir)
	return &c, nil
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
				if n.Ntfy == nil {
					return errors.New("notifications.ntfy required when provider=ntfy and enabled")
				}
				if strings.TrimSpace(n.Ntfy.Server) == "" {
					return errors.New("notifications.ntfy.server required when enabled")
				}
				if strings.TrimSpace(n.Ntfy.Topic) == "" {
					return errors.New("notifications.ntfy.topic required when enabled")
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
		switch p.Engine {
		case "", "python", "dp":
		default:
			return fmt.Errorf("planner.engine must be \"python\" or \"dp\", got %q", p.Engine)
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
		"v2x.vehicle_capacity_wh":      p.VehicleCapacityWh,
		"v2x.max_charge_w":             p.MaxChargeW,
		"v2x.max_discharge_w":          p.MaxDischargeW,
		"v2x.cycle_cost_ore_kwh":       p.CycleCostOreKWh,
		"v2x.min_reserve_soc_pct":      p.MinReserveSoCPct,
		"v2x.departure_target_soc_pct": p.DepartureTargetSoCPct,
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
	if p.MinReserveSoCPct < 0 || p.MinReserveSoCPct > 100 {
		return errors.New("v2x.min_reserve_soc_pct must be in [0,100]")
	}
	if p.DepartureTargetSoCPct < 0 || p.DepartureTargetSoCPct > 100 {
		return errors.New("v2x.departure_target_soc_pct must be in [0,100]")
	}
	if p.Enabled && p.MinReserveSoCPct <= 0 {
		return errors.New("v2x.min_reserve_soc_pct must be > 0 when v2x.enabled")
	}
	if p.DepartureTime != "" {
		if err := validateV2XDepartureTime(p.DepartureTime); err != nil {
			return err
		}
	}
	if (p.DepartureTargetSoCPct > 0) != (p.DepartureTime != "") {
		return errors.New("v2x.departure_target_soc_pct and v2x.departure_time must be set together")
	}
	if p.DepartureTargetSoCPct > 0 && p.DepartureTargetSoCPct < p.MinReserveSoCPct {
		return errors.New("v2x.departure_target_soc_pct must be >= v2x.min_reserve_soc_pct")
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

// SaveAtomic writes config to disk via tmp-file + rename. Safe from partial writes.
func SaveAtomic(path string, c *Config) error {
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
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	return os.Rename(tmp, path)
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
