package drivers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// ErrNoCapability is returned by host functions the driver wasn't granted.
var ErrNoCapability = errors.New("capability not granted")

// MQTTCap is the interface the host implements to give a driver MQTT access.
// Each driver gets its own instance bound to its configured broker.
type MQTTCap interface {
	Subscribe(topic string) error
	Publish(topic string, payload []byte) error
	// PopMessages returns and clears any buffered messages received since
	// the last call.
	PopMessages() []MQTTMessage
	// Close disconnects the underlying client. Called by Registry.Remove
	// so a driver restart doesn't leak a paho session under the same
	// clientID. Safe to call on an already-closed cap.
	Close() error
}

// MQTTMessage is one inbound MQTT message.
type MQTTMessage struct {
	Topic   string `json:"topic"`
	Payload string `json:"payload"` // raw bytes as UTF-8 string
}

// ModbusCap is the interface for Modbus TCP access.
type ModbusCap interface {
	Read(addr uint16, count uint16, kind int32) ([]uint16, error)
	WriteSingle(addr uint16, value uint16) error
	WriteMulti(addr uint16, values []uint16) error
	// Close tears down the TCP connection. Called on driver remove.
	Close() error
}

// WSCap is the host's WebSocket capability. One driver = one upstream
// connection (matches MQTTCap's single-broker shape) — drivers that
// need multiple streams can multiplex via GraphQL subscriptions or
// equivalent. The host exposes Open as a Lua-callable so the driver
// chooses when to connect (its init may need to fetch IDs over HTTP
// first); Send + PopMessages drive the inbound/outbound traffic; the
// implementation's background goroutine buffers inbound frames so
// PopMessages is non-blocking.
type WSCap interface {
	Open(url string, headers map[string]string) error
	Send(text string) error
	PopMessages() []string
	IsOpen() bool
	Close() error
}

// HostEnv is the per-driver runtime context. Captures capabilities (potentially
// nil if not granted), the shared telemetry store, and identifying info.
type HostEnv struct {
	DriverName string
	Logger     *slog.Logger
	Telemetry  *telemetry.Store
	MQTT       MQTTCap   // nil → mqtt_* calls return ErrNoCapability
	Modbus     ModbusCap // nil → modbus_* calls return ErrNoCapability
	HTTP       bool      // false → http_* calls return ErrNoCapability
	// HTTPAllowedHosts, when non-empty, restricts which hosts this
	// driver can reach via host.http_get / host.http_post. Each entry
	// is matched case-insensitively against the URL's host component
	// (not the port) — so "192.168.1.50" matches both port 80 and 8080
	// on that host. Empty list (nil or len==0) = any host allowed, for
	// backward compat with existing drivers that didn't declare a list.
	// Populated from driver config `capabilities.http.allowed_hosts`.
	HTTPAllowedHosts []string
	// HTTPTLSPinSHA256, when non-empty, pins the HTTPS leaf certificate to
	// this SHA-256 fingerprint (hex; colons/whitespace ignored, case-
	// insensitive — same value as `openssl x509 -fingerprint -sha256`).
	// When set, the http_* client for THIS driver replaces system-root
	// chain verification with an exact leaf-fingerprint match, so a driver
	// can talk to a self-signed HTTPS endpoint (e.g. a heat pump's local
	// REST API) without trusting any other certificate. Empty = standard
	// verification (unchanged default for all existing HTTP drivers).
	// Populated from driver config `capabilities.http.tls_pin_sha256`.
	HTTPTLSPinSHA256 string
	WS               WSCap // nil → ws_* calls return ErrNoCapability
	// WSAllowedHosts mirrors HTTPAllowedHosts but for ws://+wss:// URLs
	// passed to host.ws_open. Same matching semantics; empty = any host.
	WSAllowedHosts []string
	TCP            TCPCap // nil → tcp_* calls return ErrNoCapability
	// TCPAllowedHosts gates host.tcp_open(addr) the same way
	// HTTPAllowedHosts gates HTTP. Empty = any host:port. The cap impl
	// holds its own copy at construction; this field is informational so
	// callers / tests can inspect what was granted.
	TCPAllowedHosts []string
	Start           time.Time // monotonic start; host.millis() computed from here

	// BatteryCapacityWh mirrors the operator's `battery_capacity_wh`
	// declaration for this driver. Zero means "no physical battery
	// wired here" — typical for a hybrid inverter used PV-only. When
	// zero, emitTelemetry drops `host.emit("battery", …)` calls so
	// phantom SoC readings never reach the telemetry store, the
	// /api/status drivers map, or the frontend's Combined view (which
	// would otherwise mean-average a real battery's 24 % SoC with the
	// phantom 0 % from a no-battery hybrid, halving the displayed SoC).
	BatteryCapacityWh float64

	mu sync.Mutex
	// Desired poll interval — driver can set via host.set_poll_interval OR
	// return it from driver_poll. We persist the last hint here.
	PollIntervalMS int32
	// Identity set by driver / capability layer.
	// Make + SN are reported via host.set_make / host.set_sn.
	// Endpoint is the protocol+host+port string set by the registry when
	// it wires the capability (see WithEndpoint).
	Make     string
	SN       string
	MAC      string // resolved by ARP after first connection (best-effort)
	Endpoint string // e.g. "modbus://192.168.1.1:502" or "mqtt://broker:1883"

	// PersistSecret, when non-nil, lets a driver durably write a config
	// secret (e.g. a rotated OAuth refresh_token) back into its own
	// config block so it survives a restart. nil → host.persist_secret
	// returns ok=false + an error. Wired by the Registry to a per-driver
	// closure (see registry.go SecretPersister). Keep the value small:
	// it is round-tripped through config.yaml as a plain string.
	PersistSecret func(key, value string) error
}

// NewHostEnv creates a fresh host environment for a driver.
func NewHostEnv(name string, tel *telemetry.Store) *HostEnv {
	return &HostEnv{
		DriverName:     name,
		Logger:         slog.With("driver", name),
		Telemetry:      tel,
		Start:          time.Now(),
		PollIntervalMS: 5000,
	}
}

// WithMQTT binds an MQTT capability to this host.
func (h *HostEnv) WithMQTT(m MQTTCap) *HostEnv { h.MQTT = m; return h }

// WithModbus binds a Modbus capability.
func (h *HostEnv) WithModbus(m ModbusCap) *HostEnv { h.Modbus = m; return h }

// WithHTTP enables the HTTP capability.
func (h *HostEnv) WithHTTP() *HostEnv { h.HTTP = true; return h }

// WithHTTPAllowedHosts installs an allowlist. An empty / nil slice
// means "any host" (backward compatible). Matched against URL host.
func (h *HostEnv) WithHTTPAllowedHosts(hosts []string) *HostEnv {
	h.HTTPAllowedHosts = hosts
	return h
}

// WithHTTPTLSPin pins the HTTPS leaf certificate this driver's http_*
// calls will accept, by SHA-256 fingerprint. Empty string = no pin
// (standard system-root verification). See HostEnv.HTTPTLSPinSHA256.
func (h *HostEnv) WithHTTPTLSPin(fp string) *HostEnv {
	h.HTTPTLSPinSHA256 = fp
	return h
}

// WithWS binds a WebSocket capability.
func (h *HostEnv) WithWS(w WSCap) *HostEnv { h.WS = w; return h }

// WithWSAllowedHosts restricts which URLs the driver can ws_open to.
func (h *HostEnv) WithWSAllowedHosts(hosts []string) *HostEnv {
	h.WSAllowedHosts = hosts
	return h
}

// WithTCP binds a raw TCP socket capability.
func (h *HostEnv) WithTCP(t TCPCap) *HostEnv { h.TCP = t; return h }

// WithTCPAllowedHosts records which addresses the driver is permitted to
// host.tcp_open. The cap impl owns the authoritative copy; this field
// just exposes the same list for inspection.
func (h *HostEnv) WithTCPAllowedHosts(hosts []string) *HostEnv {
	h.TCPAllowedHosts = hosts
	return h
}

// millis returns monotonic milliseconds since host startup.
func (h *HostEnv) millis() int64 {
	return time.Since(h.Start).Milliseconds()
}

const (
	logDebug int32 = 0
	logInfo  int32 = 1
	logWarn  int32 = 2
	logError int32 = 3
)

const (
	ModbusCoil     int32 = 0
	ModbusDiscrete int32 = 1
	ModbusHolding  int32 = 2
	ModbusInput    int32 = 3
)

func (h *HostEnv) log(level int32, msg string) {
	switch level {
	case logDebug:
		h.Logger.Debug(msg)
	case logInfo:
		h.Logger.Info(msg)
	case logWarn:
		h.Logger.Warn(msg)
	case logError:
		h.Logger.Error(msg)
	default:
		h.Logger.Info(msg)
	}
}

// setPollInterval records the driver's requested poll interval.
func (h *HostEnv) setPollInterval(ms int32) {
	h.mu.Lock()
	h.PollIntervalMS = ms
	h.mu.Unlock()
}

// emitTelemetry accepts a JSON telemetry blob from the driver and routes it
// into the telemetry store. Expected shape:
//
//	{"type": "meter"|"pv"|"battery"|"ev"|"v2x_charger"|"vehicle", "w": 123.4, "soc": 0.5 (optional), ...}
//
// Extra fields are preserved in the reading's Data payload so the UI/API can
// surface them verbatim.
func (h *HostEnv) emitTelemetry(rawJSON []byte) error {
	var env struct {
		Type            string   `json:"type"`
		W               *float64 `json:"w"`
		SoC             *float64 `json:"soc,omitempty"`
		VehicleSoC      *float64 `json:"vehicle_soc,omitempty"`
		VehicleSoCFract *float64 `json:"vehicle_soc_fract,omitempty"`
	}
	if err := json.Unmarshal(rawJSON, &env); err != nil {
		return fmt.Errorf("emit_telemetry: invalid json: %w", err)
	}
	t, err := telemetry.ParseDerType(env.Type)
	if err != nil {
		return err
	}
	rawW := 0.0
	if env.W != nil {
		rawW = *env.W
	} else if t != telemetry.DerVehicle {
		return fmt.Errorf("emit_telemetry: %s missing required w", t)
	}
	soc := env.SoC
	if t == telemetry.DerV2X && soc == nil {
		switch {
		case env.VehicleSoC != nil:
			soc = env.VehicleSoC
		case env.VehicleSoCFract != nil:
			soc = env.VehicleSoCFract
		}
	}
	// Drop battery emits from drivers the operator declared as no-battery
	// (battery_capacity_wh ≤ 0). Hybrid inverters used PV-only still expose
	// battery registers in firmware, and the driver dutifully emits whatever
	// it reads — but without a physical pack those readings are phantom
	// (typically w=0, soc=0). Letting them through pollutes the telemetry
	// store, /api/status drivers map, and the frontend's Combined view.
	// Health success is still recorded — the driver IS alive, just emitting
	// data the operator told us to ignore.
	if t == telemetry.DerBattery && h.BatteryCapacityWh <= 0 {
		if h.Telemetry != nil {
			h.Telemetry.RecordDriverSuccess(h.DriverName)
		}
		return nil
	}
	if err := telemetry.ValidateReading(t, rawW, soc); err != nil {
		return fmt.Errorf("emit_telemetry: %w", err)
	}
	if h.Telemetry != nil {
		h.Telemetry.Update(h.DriverName, t, rawW, soc, rawJSON)
	}
	// Successful emit counts as a tick for health
	if h.Telemetry != nil {
		h.Telemetry.RecordDriverSuccess(h.DriverName)
	}
	return nil
}

// emitMetric buffers a scalar diagnostic metric for the long-format TS DB.
// Driver authors call this for anything beyond the standard pv/battery/meter
// shape — temperatures, voltages, frequencies, MPPT currents, etc. unit is an
// optional display unit (e.g. "°C", "Hz") used by the UI to group + label.
func (h *HostEnv) emitMetric(name string, value float64, unit, register, title string) error {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return fmt.Errorf("emit_metric: %s is non-finite: %v", name, value)
	}
	if h.Telemetry == nil {
		return nil
	}
	h.Telemetry.EmitMetric(h.DriverName, name, value, unit, register, title)
	// A metric emission is fresh telemetry just like a structured emit, so
	// it counts as a health success. Without this, a read-only driver that
	// only uses emit_metric (e.g. the MyUplink heat-pump telemetry driver)
	// never bumps LastSuccess and the watchdog flips it offline despite
	// live data flowing.
	h.Telemetry.RecordDriverSuccess(h.DriverName)
	return nil
}

// setSN records the device serial number.
func (h *HostEnv) setSN(sn string) {
	h.mu.Lock()
	h.SN = sn
	h.mu.Unlock()
}

// setMake records the device manufacturer.
func (h *HostEnv) setMake(m string) {
	h.mu.Lock()
	h.Make = m
	h.mu.Unlock()
}

// PollInterval returns the driver's current requested poll cadence.
func (h *HostEnv) PollInterval() time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.PollIntervalMS <= 0 {
		return 5 * time.Second
	}
	return time.Duration(h.PollIntervalMS) * time.Millisecond
}

// Identity returns (make, serial) set by the driver.
func (h *HostEnv) Identity() (make, sn string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.Make, h.SN
}

// FullIdentity returns every identity bit known to the host so callers
// (the registry) can compute a stable device_id.
func (h *HostEnv) FullIdentity() (make, sn, mac, endpoint string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.Make, h.SN, h.MAC, h.Endpoint
}

// SetEndpoint records the protocol-specific connection string for this
// driver so it can participate in device_id resolution. Called by main
// when wiring the MQTT/Modbus capability.
func (h *HostEnv) SetEndpoint(ep string) {
	h.mu.Lock()
	h.Endpoint = ep
	h.mu.Unlock()
}

// SetMAC records the L2 hardware address discovered via ARP.
func (h *HostEnv) SetMAC(mac string) {
	h.mu.Lock()
	h.MAC = mac
	h.mu.Unlock()
}

// ---- MQTT proxy ----

func (h *HostEnv) mqttSubscribe(ctx context.Context, topic string) error {
	if h.MQTT == nil {
		return ErrNoCapability
	}
	return h.MQTT.Subscribe(topic)
}

func (h *HostEnv) mqttPublish(ctx context.Context, topic string, payload []byte) error {
	if h.MQTT == nil {
		return ErrNoCapability
	}
	return h.MQTT.Publish(topic, payload)
}

func (h *HostEnv) mqttPollMessages() ([]MQTTMessage, error) {
	if h.MQTT == nil {
		return nil, ErrNoCapability
	}
	return h.MQTT.PopMessages(), nil
}

// ---- Modbus proxy ----

func (h *HostEnv) modbusRead(addr, count uint16, kind int32) ([]uint16, error) {
	if h.Modbus == nil {
		return nil, ErrNoCapability
	}
	return h.Modbus.Read(addr, count, kind)
}

func (h *HostEnv) modbusWriteSingle(addr, value uint16) error {
	if h.Modbus == nil {
		return ErrNoCapability
	}
	return h.Modbus.WriteSingle(addr, value)
}

func (h *HostEnv) modbusWriteMulti(addr uint16, values []uint16) error {
	if h.Modbus == nil {
		return ErrNoCapability
	}
	return h.Modbus.WriteMulti(addr, values)
}
