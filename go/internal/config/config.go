// Package config parses and validates the top-level YAML config.
//
// This is the single source of truth that the file-watcher re-parses on
// every change and that the settings UI writes back. All fields are
// hot-reloadable unless noted otherwise. See docs/configuration.md.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the full application config.
type Config struct {
	Site          Site               `yaml:"site" json:"site"`
	Fuse          Fuse               `yaml:"fuse" json:"fuse"`
	Drivers       []Driver           `yaml:"drivers" json:"drivers"`
	API           API                `yaml:"api" json:"api"`
	HomeAssistant *HomeAssistant     `yaml:"homeassistant,omitempty" json:"homeassistant,omitempty"`
	State         *StateConf         `yaml:"state,omitempty" json:"state,omitempty"`
	Price         *Price             `yaml:"price,omitempty" json:"price,omitempty"`
	Weather       *Weather           `yaml:"weather,omitempty" json:"weather,omitempty"`
	Planner       *Planner           `yaml:"planner,omitempty" json:"planner,omitempty"`
	Batteries     map[string]Battery `yaml:"batteries,omitempty" json:"batteries,omitempty"`
	OCPP          *OCPP              `yaml:"ocpp,omitempty" json:"ocpp,omitempty"`
	EVCharger     *EVCharger         `yaml:"ev_charger,omitempty" json:"ev_charger,omitempty"`
	Loadpoints    []Loadpoint        `yaml:"loadpoints,omitempty" json:"loadpoints,omitempty"`
	FlexLoads     []FlexLoad         `yaml:"flexloads,omitempty" json:"flexloads,omitempty"`
	Notifications *Notifications     `yaml:"notifications,omitempty" json:"notifications,omitempty"`
	Nova          *Nova              `yaml:"nova,omitempty" json:"nova,omitempty"`
}

// FlexLoad declares a price-responsive flexible load the flex-load
// scheduler optimizes against the MPC price/PV forecast — independently of
// the battery DP. Two types:
//
//   - "thermostat": a Matter thermostat (or any driver accepting a
//     setpoint-write command). The scheduler pre-heats toward MaxC in cheap
//     / PV-surplus hours and coasts toward MinC in expensive ones, with a
//     learned RC model guaranteeing the comfort floor. Requires a driver
//     metric carrying the measured indoor temperature.
//   - "deferrable": an interruptible on/off load on a smart plug (water
//     heater, pool pump, dehumidifier). The scheduler runs it in the
//     cheapest slots that meet its daily energy budget before the deadline.
//
// DriverName must match a running driver (typically drivers/matter.lua). The
// action names map to that driver's `config.commands` entries.
type FlexLoad struct {
	Type       string `yaml:"type" json:"type"` // "thermostat" | "deferrable"
	DriverName string `yaml:"driver_name" json:"driver_name"`

	// Mode selects the control strategy for a thermostat:
	//   "planner" (default) — horizon-optimised setpoint schedule against
	//                the MPC price/PV curve (pre-heat in cheap hours).
	//   "simple"  — standalone block/heat rule needing no MPC: heat to keep
	//                TargetC, but block heating while the price is above
	//                PriceThresholdOre when the building's own inertia keeps
	//                the target for BlockHorizonH hours. Works with a fixed
	//                threshold and no forecast at all.
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`

	// IndoorDriver lets the indoor temperature come from a *separate* driver
	// — e.g. a dedicated Matter Temperature Measurement sensor (cluster
	// 0x0402) rather than the thermostat's own probe, which is often biased
	// by its mounting location. Empty = read IndoorMetric off DriverName.
	IndoorDriver string `yaml:"indoor_driver,omitempty" json:"indoor_driver,omitempty"`

	// ---- simple-mode fields ----
	TargetC           float64 `yaml:"target_c,omitempty" json:"target_c,omitempty"`                         // comfort target to maintain
	PriceThresholdOre float64 `yaml:"price_threshold_ore,omitempty" json:"price_threshold_ore,omitempty"` // "expensive" cutoff; 0 = derive from forecast
	BlockHorizonH     float64 `yaml:"block_horizon_h,omitempty" json:"block_horizon_h,omitempty"`         // target must hold this long to allow a block (default 1h)

	// ---- thermostat fields ----
	// HeatingKind selects the power model:
	//   "electric"  (default) — direct electric radiator or resistive floor
	//                heating; electricity → heat 1:1 (COP=1). MaxHeatW is the
	//                electrical nameplate and the load is directly meterable.
	//   "hydronic"  — a thermostatic valve on a water loop fed by a heat
	//                pump. MaxHeatW is the zone's *thermal* output; the
	//                electrical draw the EMS pays for is MaxHeatW/COP. The
	//                per-zone valve isn't itself an electrical load — the
	//                shiftable power lives at the central heat source, so
	//                set HeatSourceDriver to attribute it (see notes).
	HeatingKind     string  `yaml:"heating_kind,omitempty" json:"heating_kind,omitempty"`
	COP             float64 `yaml:"cop,omitempty" json:"cop,omitempty"` // hydronic only; default 3.0 when kind=hydronic, 1.0 electric
	// HeatSourceDriver is reserved for a future feature: attributing a
	// hydronic zone's electrical load to the central HP/boiler driver.
	// It is declared in config but not yet read or wired in the service.
	HeatSourceDriver string `yaml:"heat_source_driver,omitempty" json:"heat_source_driver,omitempty"`

	// FlowDriver/FlowMetric read the heat pump's supply (flow) temperature
	// (°C), typically from a Nibe/Thermia/etc integration. It refines the
	// reheat-cost side of the pause economics: a hot loop means the heat pump
	// already produced the heat, so recovering after a pause is nearly free;
	// a cold loop means reheating must run the compressor and is costly.
	// FlowDriver empty = read FlowMetric off DriverName. Only meaningful for
	// hydronic zones.
	FlowDriver string `yaml:"flow_driver,omitempty" json:"flow_driver,omitempty"`
	FlowMetric string `yaml:"flow_metric,omitempty" json:"flow_metric,omitempty"`
	// NominalFlowDeltaC is the design flow-above-room temperature delta at
	// which the loop holds a full charge of usable heat (floor heating ≈ 15,
	// radiators ≈ 25-30). Used to scale the stored-heat credit. Default 15.
	NominalFlowDeltaC float64 `yaml:"nominal_flow_delta_c,omitempty" json:"nominal_flow_delta_c,omitempty"`
	MinC            float64 `yaml:"min_c,omitempty" json:"min_c,omitempty"`
	MaxC            float64 `yaml:"max_c,omitempty" json:"max_c,omitempty"`
	MaxHeatW        float64 `yaml:"max_heat_w,omitempty" json:"max_heat_w,omitempty"`
	IndoorMetric    string  `yaml:"indoor_metric,omitempty" json:"indoor_metric,omitempty"`
	HeatMetric      string  `yaml:"heat_metric,omitempty" json:"heat_metric,omitempty"` // optional: metered heating power for RC training
	SetpointAction  string  `yaml:"setpoint_action,omitempty" json:"setpoint_action,omitempty"`
	PreHeatFraction float64 `yaml:"preheat_fraction,omitempty" json:"preheat_fraction,omitempty"`

	// SlabDriver/SlabMetric provide the floor/slab temperature — a floor probe
	// (common on electric floor thermostats) or the hydronic flow temperature
	// as a proxy. When set, the zone uses a two-mass (slab + room) thermal
	// model instead of the single-mass RC fit, which captures how a charged
	// slab keeps the room warm for hours after the element switches off — a
	// far more accurate coast/forecast for floor heating. SlabDriver empty =
	// read SlabMetric off DriverName.
	SlabDriver string `yaml:"slab_driver,omitempty" json:"slab_driver,omitempty"`
	SlabMetric string `yaml:"slab_metric,omitempty" json:"slab_metric,omitempty"`

	// ---- deferrable fields ----
	// PowerMetric is the driver metric carrying the plug's measured power
	// (W) — e.g. a Matter smart plug's ActivePower. When set, the scheduler
	// learns the appliance's actual run power and daily energy from it, so
	// EnergyWh / PowerW become optional (learned when left 0). This is what
	// lets one generic "deferrable" handle a spa, a water heater, or a pump
	// without the operator characterising each by hand.
	PowerMetric string  `yaml:"power_metric,omitempty" json:"power_metric,omitempty"`
	EnergyWh    float64 `yaml:"energy_wh,omitempty" json:"energy_wh,omitempty"`
	PowerW      float64 `yaml:"power_w,omitempty" json:"power_w,omitempty"`
	OnAction     string  `yaml:"on_action,omitempty" json:"on_action,omitempty"`
	OffAction    string  `yaml:"off_action,omitempty" json:"off_action,omitempty"`
	PreferPV     bool    `yaml:"prefer_pv,omitempty" json:"prefer_pv,omitempty"`
	EarliestHour int     `yaml:"earliest_hour,omitempty" json:"earliest_hour,omitempty"` // local hour-of-day window start (0 = none)
	DeadlineHour int     `yaml:"deadline_hour,omitempty" json:"deadline_hour,omitempty"` // local hour-of-day deadline (0 = none)
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
	Type          string `yaml:"type" json:"type"`
	Enabled       bool   `yaml:"enabled" json:"enabled"`
	ThresholdS    int    `yaml:"threshold_s,omitempty" json:"threshold_s,omitempty"`
	Priority      int    `yaml:"priority,omitempty" json:"priority,omitempty"`
	Tags          string `yaml:"tags,omitempty" json:"tags,omitempty"`
	TitleTemplate string `yaml:"title_template,omitempty" json:"title_template,omitempty"`
	BodyTemplate  string `yaml:"body_template,omitempty" json:"body_template,omitempty"`
	CooldownS     int    `yaml:"cooldown_s,omitempty" json:"cooldown_s,omitempty"`
}

// Nova is the opt-in Sourceful Nova Core federation config. When enabled,
// forty-two-watts publishes telemetry to Nova's MQTT broker (NATS MQTT
// adapter) and reconciles device/DER registrations via Nova's core-api.
//
// Identity is an ES256 keypair generated on first run and stored at
// KeyPath (default <state.path sibling>/nova.key). The public key is
// registered in Nova via the claim flow; the private key signs a short-
// lived JWT used as the MQTT password.
//
// SchemaMode controls the wire format sent to Nova:
//   - "legacy"  (default): translate forty-two-watts' native clean payload
//                to the current Nova wire shape (battery sign flip,
//                PascalCase fields, pv→solar, ev→ev_port). The translation
//                layer is in internal/nova and is designed to be deleted
//                once Nova adopts the unified schema.
//   - "unified": publish forty-two-watts' clean payload directly. Enable
//                once the Nova schema-alignment PR lands.
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
// Phase 3 introduces the schema + observable surface; Phase 4 wires
// it into the DP state space so the MPC optimizes battery + EV jointly.
// See docs/plan-ems-contract.md + go/internal/loadpoint.
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
}

// OCPP configures the embedded OCPP 1.6J Central System for EV chargers.
// When enabled, EV chargers connect to ws://<bind>:<port>/<chargerId>
// and their power readings flow into telemetry.Store as DerEV samples,
// which the dispatch clamp uses to keep home batteries from feeding
// the car. See go/internal/ocpp.
type OCPP struct {
	Enabled            bool   `yaml:"enabled" json:"enabled"`
	Bind               string `yaml:"bind,omitempty" json:"bind,omitempty"`
	Port               int    `yaml:"port,omitempty" json:"port,omitempty"`
	Path               string `yaml:"path,omitempty" json:"path,omitempty"`
	Username           string `yaml:"username,omitempty" json:"username,omitempty"`
	Password           string `yaml:"password,omitempty" json:"password,omitempty"`
	HeartbeatIntervalS int    `yaml:"heartbeat_interval_s,omitempty" json:"heartbeat_interval_s,omitempty"`
}

// EVCharger is the high-level EV charger config written by the Settings UI.
// The backend auto-generates a Lua driver entry from this on startup so
// users never touch raw driver YAML for their EV charger.
//
// Password is stored in state.db (key "ev_charger_password"), NOT in config.yaml.
// It is populated at runtime by main.go after loading state and by the API
// handler on POST /api/config.
type EVCharger struct {
	Provider string `yaml:"provider" json:"provider"` // "easee" (only option for now)
	Email    string `yaml:"email" json:"email"`
	Password string `yaml:"-" json:"password"` // persisted in state.db, not YAML
	Serial   string `yaml:"serial,omitempty" json:"serial,omitempty"`
}

// Planner configures the MPC scheduler (optional — disabled if omitted).
// Mode: "self_consumption" (default) | "cheap_charge" | "arbitrage".
type Planner struct {
	Enabled             bool    `yaml:"enabled" json:"enabled"`
	Mode                string  `yaml:"mode,omitempty" json:"mode,omitempty"`
	BaseLoadW           float64 `yaml:"base_load_w,omitempty" json:"base_load_w,omitempty"`
	HorizonHours        int     `yaml:"horizon_hours,omitempty" json:"horizon_hours,omitempty"`
	IntervalMin         int     `yaml:"interval_min,omitempty" json:"interval_min,omitempty"`
	SoCMinPct           float64 `yaml:"soc_min_pct,omitempty" json:"soc_min_pct,omitempty"`
	SoCMaxPct           float64 `yaml:"soc_max_pct,omitempty" json:"soc_max_pct,omitempty"`
	ChargeEfficiency    float64 `yaml:"charge_efficiency,omitempty" json:"charge_efficiency,omitempty"`
	DischargeEfficiency float64 `yaml:"discharge_efficiency,omitempty" json:"discharge_efficiency,omitempty"`
	ExportOrePerKWh     float64 `yaml:"export_ore_per_kwh,omitempty" json:"export_ore_per_kwh,omitempty"` // 0 = use mean spot
	// LegacyDispatch reverts the control loop from the default
	// energy-allocation path back to the legacy PI-on-grid-target
	// path. Provided for emergency rollback only — the energy path
	// respects the principle "plan allocates energy, EMS reacts to
	// live data" and is the correct architecture (see
	// docs/plan-ems-contract.md).
	LegacyDispatch bool `yaml:"legacy_dispatch,omitempty" json:"legacy_dispatch,omitempty"`

	// UseEnergyDispatch is the deprecated inverse of LegacyDispatch.
	// Pointer so we can distinguish "unset" (nil) from "explicitly
	// false" (*false) — the latter matters because an operator who
	// previously picked legacy dispatch must not be silently flipped
	// to the energy path on upgrade. Honored with a startup WARN
	// and will be removed after one release.
	UseEnergyDispatch *bool `yaml:"use_energy_dispatch,omitempty" json:"use_energy_dispatch,omitempty"`
}

// Site is the top-level control loop config.
type Site struct {
	Name                 string  `yaml:"name" json:"name"`
	ControlIntervalS     int     `yaml:"control_interval_s" json:"control_interval_s"`
	GridTargetW          float64 `yaml:"grid_target_w" json:"grid_target_w"`
	GridToleranceW       float64 `yaml:"grid_tolerance_w" json:"grid_tolerance_w"`
	WatchdogTimeoutS     int     `yaml:"watchdog_timeout_s" json:"watchdog_timeout_s"`
	SmoothingAlpha       float64 `yaml:"smoothing_alpha" json:"smoothing_alpha"`
	Gain                 float64 `yaml:"gain" json:"gain"`
	SlewRateW            float64 `yaml:"slew_rate_w" json:"slew_rate_w"`
	MinDispatchIntervalS int     `yaml:"min_dispatch_interval_s" json:"min_dispatch_interval_s"`
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
	Name               string  `yaml:"name" json:"name"`
	Lua                string  `yaml:"lua,omitempty" json:"lua,omitempty"` // path to .lua file
	IsSiteMeter        bool    `yaml:"is_site_meter,omitempty" json:"is_site_meter,omitempty"`
	BatteryCapacityWh  float64 `yaml:"battery_capacity_wh,omitempty" json:"battery_capacity_wh,omitempty"`
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
	// behavior. See issue #143 and docs/configuration.md.
	InverterGroup string `yaml:"inverter_group,omitempty" json:"inverter_group,omitempty"`
	// Disabled skips this driver at startup / reload. Set via the UI when
	// you want to temporarily take a driver out without editing yaml.
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`
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

// Capabilities explicitly scope what host resources a driver can access.
type Capabilities struct {
	MQTT   *MQTTConfig    `yaml:"mqtt,omitempty" json:"mqtt,omitempty"`
	Modbus *ModbusConfig  `yaml:"modbus,omitempty" json:"modbus,omitempty"`
	HTTP   *HTTPCapability `yaml:"http,omitempty" json:"http,omitempty"`
	Matter *MatterConfig  `yaml:"matter,omitempty" json:"matter,omitempty"`
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
	Port   int    `yaml:"port,omitempty" json:"port,omitempty"`   // default 502
	UnitID int    `yaml:"unit_id,omitempty" json:"unit_id,omitempty"` // default 1
}

// HTTPCapability grants HTTP access to specific hostnames (future).
type HTTPCapability struct {
	AllowedHosts []string `yaml:"allowed_hosts" json:"allowed_hosts"`
}

// MatterConfig grants access to a python-matter-server instance.
// Host is required; Port defaults to 5580.
type MatterConfig struct {
	Host string `yaml:"host" json:"host"`
	Port int    `yaml:"port,omitempty" json:"port,omitempty"` // default 5580
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
type StateConf struct {
	Path    string `yaml:"path" json:"path"`
	ColdDir string `yaml:"cold_dir" json:"cold_dir"`
}

// Price is the spot-price source config.
type Price struct {
	Provider         string  `yaml:"provider" json:"provider"` // elprisetjustnu | entsoe | none
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
	if out.HomeAssistant != nil {
		cp := *out.HomeAssistant
		cp.Password = ""
		out.HomeAssistant = &cp
	}
	if out.OCPP != nil {
		cp := *out.OCPP
		cp.Password = ""
		out.OCPP = &cp
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
	if incoming.HomeAssistant != nil && existing.HomeAssistant != nil && incoming.HomeAssistant.Password == "" {
		incoming.HomeAssistant.Password = existing.HomeAssistant.Password
	}
	if incoming.OCPP != nil && existing.OCPP != nil && incoming.OCPP.Password == "" {
		incoming.OCPP.Password = existing.OCPP.Password
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

// ResolveDriverPaths joins relative Lua driver paths with baseDir, or
// with DriversDirOverride when the relative path starts with "drivers/".
func (c *Config) ResolveDriverPaths(baseDir string) {
	for i := range c.Drivers {
		c.Drivers[i].Lua = stripLeadingDotDot(c.Drivers[i].Lua)
		p := c.Drivers[i].Lua
		if p == "" || filepath.IsAbs(p) {
			continue
		}
		if DriversDirOverride != "" && strings.HasPrefix(p, "drivers/") {
			c.Drivers[i].Lua = filepath.Join(DriversDirOverride, strings.TrimPrefix(p, "drivers/"))
			continue
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
		if DriversDirOverride != "" && p != "" {
			rel, err := filepath.Rel(DriversDirOverride, p)
			if err == nil && !strings.HasPrefix(rel, "..") {
				c.Drivers[i].Lua = filepath.ToSlash(filepath.Join("drivers", rel))
				continue
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
	if c.Site.ControlIntervalS == 0 {
		c.Site.ControlIntervalS = 5
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
		c.Site.SlewRateW = 500
	}
	if c.Site.MinDispatchIntervalS == 0 {
		c.Site.MinDispatchIntervalS = 5
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
			if cap.Port == 0 { cap.Port = 502 }
			if cap.UnitID == 0 { cap.UnitID = 1 }
		}
		if cap := d.MQTT; cap != nil && cap.Port == 0 {
			cap.Port = 1883
		}
		if cap := d.Modbus; cap != nil {
			if cap.Port == 0 { cap.Port = 502 }
			if cap.UnitID == 0 { cap.UnitID = 1 }
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

// Validate ensures the config is internally consistent and safe to run with.
func (c *Config) Validate() error {
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
		if d.EffectiveMQTT() == nil && d.EffectiveModbus() == nil && d.Capabilities.HTTP == nil && d.Capabilities.Matter == nil {
			return fmt.Errorf("driver %q: must have mqtt, modbus, http, or matter capability", d.Name)
		}
	}
	if len(c.Drivers) > 0 && siteMeters == 0 {
		return errors.New("at least one driver must be is_site_meter: true")
	}

	if c.Site.SmoothingAlpha <= 0 || c.Site.SmoothingAlpha > 1 {
		return errors.New("site.smoothing_alpha must be in (0, 1]")
	}
	if c.Fuse.MaxAmps <= 0 {
		return errors.New("fuse.max_amps must be > 0")
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
	for i, fl := range c.FlexLoads {
		if fl.DriverName == "" {
			return fmt.Errorf("flexloads[%d]: driver_name is required", i)
		}
		switch fl.Type {
		case "thermostat", "deferrable":
		default:
			return fmt.Errorf("flexloads[%d] %q: type must be \"thermostat\" or \"deferrable\"", i, fl.DriverName)
		}
		if fl.Type == "thermostat" {
			if fl.MinC != 0 || fl.MaxC != 0 {
				if fl.MinC >= fl.MaxC {
					return fmt.Errorf("flexloads[%d] %q: min_c (%.1f) must be < max_c (%.1f)", i, fl.DriverName, fl.MinC, fl.MaxC)
				}
			}
			if fl.COP < 0 {
				return fmt.Errorf("flexloads[%d] %q: cop must be >= 0", i, fl.DriverName)
			}
			switch fl.Mode {
			case "", "planner", "simple":
			default:
				return fmt.Errorf("flexloads[%d] %q: mode must be \"planner\" or \"simple\"", i, fl.DriverName)
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
			return errors.New("nova.gateway_serial is required when nova.enabled — run `forty-two-watts nova-claim`")
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
	return nil
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
	// Paths resolved through DriversDirOverride land outside baseDir, so a
	// straight Rel would emit "../drivers/<name>.lua" — preserved across
	// saves via stripLeadingDotDot but ugly. Rewrite them as a clean
	// "drivers/<basename>" to keep YAML portable between hosts.
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
	return rel
}
