package drivers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	net_url "net/url"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
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

// SerialCap is read-only access to one serial device.
type SerialCap interface {
	Read(maxBytes int, timeout time.Duration) ([]byte, error)
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
	Serial     SerialCap // nil → serial_read returns ErrNoCapability
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
	// HTTPAllowWrite gates host.http_patch, and only http_patch —
	// http_get stays a read and http_post stays under the plain HTTP
	// capability (existing drivers POST to query-style APIs). http_patch
	// returns an error string unless this is set, so granting HTTP for
	// telemetry never implicitly grants device mutation. Populated from
	// driver config `capabilities.http.allow_write`.
	HTTPAllowWrite bool
	WS             WSCap // nil → ws_* calls return ErrNoCapability
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
	// RuntimePolicy is nil for bundled, local and legacy repository drivers. A
	// signed read-only policy denies writes in every phase. A signed v2 control
	// policy also limits writes to a bounded command/default-mode call.
	RuntimePolicy *RuntimePolicy

	// BatteryCapacityWh mirrors the operator's `battery_capacity_wh`
	// declaration for this driver. Zero means "no physical battery
	// wired here" — typical for a hybrid inverter used PV-only. When
	// zero, emitTelemetry drops `host.emit("battery", …)` calls so
	// phantom SoC readings never reach the telemetry store, the
	// /api/status drivers map, or the frontend's Combined view (which
	// would otherwise mean-average a real battery's 24 % SoC with the
	// phantom 0 % from a no-battery hybrid, halving the displayed SoC).
	BatteryCapacityWh float64
	// BatteryTelemetryOnly admits structured battery readings from a
	// read-only gateway while keeping it out of the controllable battery pool.
	// It is intentionally independent of BatteryCapacityWh: the latter is the
	// dispatch opt-in, this flag is only a telemetry admission hint.
	BatteryTelemetryOnly bool

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
	// Model is the device model name reported via host.set_model. It is
	// nameplate, not a reading, so it lives beside Make and SN. It is
	// deliberately NOT part of device_id resolution: a driver often learns
	// the model from a register read that can fail or arrive late, and a
	// device_id that changed when the model finally appeared would orphan
	// every row of persistent state keyed on it.
	Model string
	// RatedW is the device's rated AC power in watts, reported via
	// host.set_rated_w. Nameplate again — it is read off the bus once in
	// driver_init and does not change between polls, so it belongs here
	// rather than in each tick's telemetry. Zero means "not reported".
	RatedW float64
	// WarmupS holds off the first poll for this many seconds after the
	// host started the driver, for devices that answer Modbus before
	// their registers carry meaningful values. Set via host.set_warmup_s
	// in driver_init; read once by the registry's run loop.
	WarmupS float64

	// PersistSecret, when non-nil, lets a driver durably write a config
	// secret (e.g. a rotated OAuth refresh_token) back into its own
	// config block so it survives a restart. nil → host.persist_secret
	// returns ok=false + an error. Wired by the Registry to a per-driver
	// closure (see registry.go SecretPersister). Keep the value small:
	// it is round-tripped through config.yaml as a plain string.
	PersistSecret func(key, value string) error
	writePhase    string
	writeDeadline time.Time
	writeAttempts int
	writeCount    int
	writeEvidence map[string]bool

	// Poll-scoped Modbus evidence prevents a Modbus driver from turning failed
	// reads into fresh zero-valued telemetry. The Lua runtime holds emissions
	// until driver_poll ends, then commits them only when every read succeeded.
	requiresFreshModbusRead bool
	pollActive              bool
	pollModbusAttempts      int
	pollModbusSuccesses     int
	// Failures split by what they prove about the device. Only a transport
	// failure means the readings might be stale; a refusal means the device
	// answered, and a skip never reached the wire at all.
	pollModbusTransportFailures int
	pollModbusRefused           int
	pollModbusSkipped           int
	// lastPollEvidence keeps the finished poll's breakdown so the caller
	// can report what actually went wrong rather than a bare count.
	lastPollEvidence pollEvidence
	pollTelemetry    [][]byte
	pollMetrics      []pollMetric
}

type pollMetric struct {
	name     string
	value    float64
	unit     string
	register string
	title    string
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

func (h *HostEnv) WithRuntimePolicy(policy *RuntimePolicy) *HostEnv {
	h.RuntimePolicy = policy
	return h
}

func (h *HostEnv) permissionAllowed(permission string) bool {
	return h.RuntimePolicy == nil || h.RuntimePolicy.allows(permission)
}

func (h *HostEnv) beginWriteScope(phase string, deadline time.Time) error {
	if h.RuntimePolicy == nil {
		return nil
	}
	if !h.RuntimePolicy.IsControlV2() {
		return errors.New("managed driver has an unsupported control runtime")
	}
	if phase != "command" && phase != "default" {
		return fmt.Errorf("invalid driver write phase %q", phase)
	}
	if deadline.IsZero() || !time.Now().Before(deadline) {
		return errors.New("driver write scope has expired")
	}
	h.mu.Lock()
	h.writePhase = phase
	h.writeDeadline = deadline
	h.writeAttempts = 0
	h.writeCount = 0
	h.writeEvidence = make(map[string]bool)
	h.mu.Unlock()
	return nil
}

func (h *HostEnv) endWriteScope() (int, []string) {
	if h.RuntimePolicy == nil {
		return 0, nil
	}
	h.mu.Lock()
	writes := h.writeCount
	evidence := make([]string, 0, len(h.writeEvidence))
	for _, name := range []string{"write_ack", "vendor_ack", "readback"} {
		if h.writeEvidence[name] {
			evidence = append(evidence, name)
		}
	}
	h.writePhase = ""
	h.writeDeadline = time.Time{}
	h.writeAttempts = 0
	h.writeCount = 0
	h.writeEvidence = nil
	h.mu.Unlock()
	return writes, evidence
}

// allowAuthPost reports whether this POST is the sign-in the signed package
// declared, and so may proceed outside the write phases.
//
// A read-only driver that reads a vendor cloud cannot read anything until it
// has exchanged a token, and it exchanges one from init or poll -- the phases
// allowWrite refuses, for good reason, since nothing there can carry a command
// lease. Authenticating is a precondition for reading rather than a write to
// the device, so it is admitted here instead, confined to the single path the
// signed manifest names and only for a driver published read-only. It does not
// consume the write budget: a token refresh is driven by expiry, not by a
// caller, and spending the budget on it would leave the driver unable to read
// once its token aged out.
func (h *HostEnv) allowAuthPost(rawURL string) bool {
	if h.RuntimePolicy == nil || !h.RuntimePolicy.ReadOnly {
		return false
	}
	declared := h.RuntimePolicy.AuthPostPath
	if declared == "" || !h.RuntimePolicy.allows("http.post") {
		return false
	}
	parsed, err := net_url.Parse(rawURL)
	if err != nil {
		return false
	}
	return parsed.Path == declared
}

func (h *HostEnv) allowWrite(permission string) error {
	if h.RuntimePolicy == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.RuntimePolicy.allows(permission) {
		return fmt.Errorf("%s: permission not granted by signed package", permission)
	}
	if h.writePhase != "command" && h.writePhase != "default" {
		return fmt.Errorf("%s: write is not allowed during init, poll, or cleanup", permission)
	}
	if h.writeDeadline.IsZero() || !time.Now().Before(h.writeDeadline) {
		h.writePhase = ""
		return fmt.Errorf("%s: write scope expired", permission)
	}
	if h.writeAttempts >= h.RuntimePolicy.maxWrites() {
		return fmt.Errorf("%s: write budget exhausted", permission)
	}
	h.writeAttempts++
	return nil
}

func (h *HostEnv) recordWriteEvidence(name string) {
	if h.RuntimePolicy == nil {
		return
	}
	h.mu.Lock()
	if h.writePhase == "command" || h.writePhase == "default" {
		if name == "write_ack" {
			h.writeCount++
		}
		// Readback proves an applied write only when the read happened after
		// at least one successful host write in this scope.
		if name == "readback" && h.writeCount == 0 {
			h.mu.Unlock()
			return
		}
		if h.writeEvidence == nil {
			h.writeEvidence = make(map[string]bool)
		}
		h.writeEvidence[name] = true
	}
	h.mu.Unlock()
}

func (h *HostEnv) beginPollEvidence() {
	h.mu.Lock()
	h.pollActive = true
	h.pollModbusAttempts = 0
	h.pollModbusSuccesses = 0
	h.pollModbusTransportFailures = 0
	h.pollModbusRefused = 0
	h.pollModbusSkipped = 0
	h.pollTelemetry = nil
	h.pollMetrics = nil
	h.mu.Unlock()
}

// pollEvidence summarises one poll's Modbus traffic by what each result
// proves about the device on the other end.
type pollEvidence struct {
	Attempts  int
	Successes int
	Transport int // could not reach the device
	Refused   int // device answered and declined the register
	Skipped   int // never attempted; a reconnect backoff was running
}

// fresh reports whether this poll's readings can be trusted as current.
//
// The rule used to be attempts == successes, which threw away the whole
// poll if a single register missed. That made a driver's own tolerance —
// sungrow.lua marks 19 of its 20 reads optional — count for nothing, and
// it made the driver permanently useless on a string inverter, which has
// no battery registers and refuses them on every poll forever.
//
// What actually matters is whether we reached the device. A refusal is
// proof of life: it replied. So the poll is fresh when something was read
// successfully and nothing failed at the transport. Skips are not counted
// against it either — they are downstream of a transport failure that has
// already been counted once.
func (e pollEvidence) fresh() bool {
	return e.Successes > 0 && e.Transport == 0
}

func (h *HostEnv) endPollEvidence(commit bool) (attempts, successes int, err error) {
	h.mu.Lock()
	ev := pollEvidence{
		Attempts:  h.pollModbusAttempts,
		Successes: h.pollModbusSuccesses,
		Transport: h.pollModbusTransportFailures,
		Refused:   h.pollModbusRefused,
		Skipped:   h.pollModbusSkipped,
	}
	attempts = ev.Attempts
	successes = ev.Successes
	h.lastPollEvidence = ev
	fresh := commit && ev.fresh()
	var pendingTelemetry [][]byte
	var pendingMetrics []pollMetric
	if h.requiresFreshModbusRead && fresh {
		pendingTelemetry = h.pollTelemetry
		pendingMetrics = h.pollMetrics
	}
	h.pollActive = false
	h.pollModbusAttempts = 0
	h.pollModbusSuccesses = 0
	h.pollModbusTransportFailures = 0
	h.pollModbusRefused = 0
	h.pollModbusSkipped = 0
	h.pollTelemetry = nil
	h.pollMetrics = nil
	h.mu.Unlock()

	for _, rawJSON := range pendingTelemetry {
		if emitErr := h.emitTelemetry(rawJSON); emitErr != nil {
			return attempts, successes, emitErr
		}
	}
	for _, metric := range pendingMetrics {
		if emitErr := h.emitMetric(metric.name, metric.value, metric.unit, metric.register, metric.title); emitErr != nil {
			return attempts, successes, emitErr
		}
	}
	return attempts, successes, nil
}

func (h *HostEnv) recordPollModbusRead(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.pollActive {
		return
	}
	h.pollModbusAttempts++
	switch {
	case err == nil:
		h.pollModbusSuccesses++
	case errors.Is(err, ErrModbusBackoff):
		// Never left the host. Counting it as a link failure would turn
		// one dropped packet into however many reads the poll had left.
		h.pollModbusSkipped++
	case errors.Is(err, ErrModbusTransport):
		h.pollModbusTransportFailures++
	default:
		// The device answered and refused — an illegal address on a model
		// that lacks the register, most often. It is alive, so this says
		// nothing about whether the rest of the poll is current.
		h.pollModbusRefused++
	}
}

func (h *HostEnv) bufferPollTelemetry(rawJSON []byte) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.pollActive || !h.requiresFreshModbusRead {
		return false
	}
	h.pollTelemetry = append(h.pollTelemetry, append([]byte(nil), rawJSON...))
	return true
}

func (h *HostEnv) bufferPollMetric(metric pollMetric) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.pollActive || !h.requiresFreshModbusRead {
		return false
	}
	h.pollMetrics = append(h.pollMetrics, metric)
	return true
}

// WithMQTT binds an MQTT capability to this host.
func (h *HostEnv) WithMQTT(m MQTTCap) *HostEnv { h.MQTT = m; return h }

// WithModbus binds a Modbus capability.
func (h *HostEnv) WithModbus(m ModbusCap) *HostEnv { h.Modbus = m; return h }

// WithSerial binds a read-only serial capability.
func (h *HostEnv) WithSerial(s SerialCap) *HostEnv { h.Serial = s; return h }

// WithHTTP enables the HTTP capability.
func (h *HostEnv) WithHTTP() *HostEnv { h.HTTP = true; return h }

// WithHTTPAllowWrite grants the mutating HTTP verb host.http_patch. Scoped
// to http_patch only; http_get/http_post are unaffected. See
// HostEnv.HTTPAllowWrite.
func (h *HostEnv) WithHTTPAllowWrite() *HostEnv { h.HTTPAllowWrite = true; return h }

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

// Why a read failed decides whether the whole poll is worthless or just
// incomplete, and only the transport layer can tell the two apart. It
// wraps its errors with these so the poll-freshness rule can ask.
//
// The distinction is not pedantic. A device answering "illegal data
// address" is stronger proof of life than a register that read fine: it
// replied. A string inverter has no battery registers and will answer
// that way forever, and treating it as a dead link makes the driver
// permanently useless on that hardware.
var (
	// ErrModbusTransport marks a failure to reach the device at all —
	// reset connection, timeout, refused, unreachable. Readings from
	// this poll cannot be trusted to be current.
	ErrModbusTransport = errors.New("modbus transport failure")

	// ErrModbusBackoff marks a read that was never attempted because a
	// reconnect backoff was already running. It is a consequence of an
	// earlier transport failure, not new evidence of one, and counting
	// it as such turns a single dropped packet into a whole poll's worth
	// of apparent failures.
	ErrModbusBackoff = errors.New("modbus reconnect backoff active")
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

		// Canonical @srcful/data-models spellings. srcfl/device-drivers is
		// converting its catalog to these, one driver at a time. Accepting
		// both lets a converted driver run here before the old names are
		// retired, so no site loses telemetry mid-migration.
		CanonicalW   *float64 `json:"W,omitempty"`
		CanonicalSoC *float64 `json:"SoC_nom_fract,omitempty"`
	}
	if err := json.Unmarshal(rawJSON, &env); err != nil {
		return fmt.Errorf("emit_telemetry: invalid json: %w", err)
	}
	t, err := telemetry.ParseDerType(env.Type)
	if err != nil {
		return err
	}
	if env.W == nil {
		env.W = env.CanonicalW
	}
	if env.SoC == nil {
		env.SoC = env.CanonicalSoC
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
	if err := telemetry.ValidateReading(t, rawW, soc); err != nil {
		return fmt.Errorf("emit_telemetry: %w", err)
	}
	// Rewrite alias keys onto the names the Nova payload reads, before the
	// blob is buffered or stored. Without this a driver's energy counters and
	// frequency never leave the gateway.
	rawJSON = normalizeTelemetryKeys(rawJSON)
	rawJSON = h.stampIdentity(rawJSON)

	if h.bufferPollTelemetry(rawJSON) {
		return nil
	}
	// Drop battery emits from drivers the operator declared as no-battery
	// (battery_capacity_wh ≤ 0). Hybrid inverters used PV-only still expose
	// battery registers in firmware, and the driver dutifully emits whatever
	// it reads — but without a physical pack those readings are phantom
	// (typically w=0, soc=0). Letting them through pollutes the telemetry
	// store, /api/status drivers map, and the frontend's Combined view.
	// Health success is still recorded — the driver IS alive, just emitting
	// data the operator told us to ignore.
	if t == telemetry.DerBattery && h.BatteryCapacityWh <= 0 && !h.BatteryTelemetryOnly {
		if h.Telemetry != nil {
			h.Telemetry.RecordDriverSuccess(h.DriverName)
		}
		return nil
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
	if h.bufferPollMetric(pollMetric{name: name, value: value, unit: unit, register: register, title: title}) {
		return nil
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

// setModel records the device model name.
func (h *HostEnv) setModel(m string) {
	h.mu.Lock()
	h.Model = m
	h.mu.Unlock()
}

// setRatedW records the device's rated AC power in watts.
func (h *HostEnv) setRatedW(w float64) {
	h.mu.Lock()
	h.RatedW = w
	h.mu.Unlock()
}

// setWarmupS records a startup hold before the first poll, in seconds.
func (h *HostEnv) setWarmupS(s float64) {
	h.mu.Lock()
	h.WarmupS = s
	h.mu.Unlock()
}

// DeviceModel returns the model name the driver reported, or "" if it
// reported none. Named DeviceModel because the field is Model.
func (h *HostEnv) DeviceModel() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.Model
}

// RatedPowerW returns the rated AC power the driver reported, or 0.
func (h *HostEnv) RatedPowerW() float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.RatedW
}

// Warmup returns the startup hold the driver requested, or 0.
func (h *HostEnv) Warmup() time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.WarmupS <= 0 {
		return 0
	}
	return time.Duration(h.WarmupS * float64(time.Second))
}

// FirstPollDelay is how long the run loop waits before the first poll.
// Normally that is just the poll interval; a driver that asked for a
// warmup gets whatever is left of the warmup window instead, measured
// from host start so the time already spent in driver_init counts
// towards it. A warmup shorter than the cadence changes nothing.
func (h *HostEnv) FirstPollDelay() time.Duration {
	interval := h.PollInterval()
	warmup := h.Warmup()
	if warmup <= 0 {
		return interval
	}
	remaining := warmup - time.Since(h.Start)
	if remaining > interval {
		return remaining
	}
	return interval
}

// stampIdentity adds the nameplate values a driver reported once in
// driver_init — model and rated AC power — to an outgoing telemetry
// blob. nova.assemble unmarshals this blob straight into DerTelemetry,
// so a key added here reaches Nova's `model` and `rated_power_w`
// without every driver repeating the value on every emit.
//
// A key the driver set itself always wins: a live reading is closer to
// the device than a value cached since init.
func (h *HostEnv) stampIdentity(raw []byte) []byte {
	h.mu.Lock()
	model, ratedW := h.Model, h.RatedW
	h.mu.Unlock()
	if model == "" && ratedW == 0 {
		return raw
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw
	}
	changed := false
	set := func(key string, value any) {
		if _, taken := fields[key]; taken {
			return
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return
		}
		fields[key] = encoded
		changed = true
	}
	if model != "" {
		set("model", model)
	}
	// The catalog spells rated power both ways. Honour either as
	// "the driver already said it" so we never contradict a live read.
	if ratedW != 0 {
		if _, taken := fields["rated_power_W"]; !taken {
			set("rated_power_w", ratedW)
		}
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return raw
	}
	return out
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
		h.recordPollModbusRead(ErrNoCapability)
		return nil, ErrNoCapability
	}
	regs, err := h.Modbus.Read(addr, count, kind)
	h.recordPollModbusRead(err)
	return regs, err
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

// describe explains a poll in the terms that decide whether its readings
// were kept. The old message counted every miss the same way and read
// "8 of 20 modbus reads failed" when one packet dropped and the rest were
// never sent — which pointed the reader at the wrong problem.
func (e pollEvidence) describe() string {
	switch {
	case e.Attempts == 0:
		return "no modbus reads attempted"
	case e.Transport > 0:
		msg := fmt.Sprintf("%d of %d modbus reads could not reach the device",
			e.Transport, e.Attempts)
		if e.Skipped > 0 {
			msg += fmt.Sprintf(" (%d more skipped while reconnecting)", e.Skipped)
		}
		if e.Successes > 0 {
			msg += fmt.Sprintf("; %d did read", e.Successes)
		}
		return msg
	case e.Successes == 0 && e.Refused > 0:
		return fmt.Sprintf("the device refused all %d modbus reads", e.Attempts)
	case e.Successes == 0:
		return fmt.Sprintf("none of %d modbus reads returned data", e.Attempts)
	default:
		return fmt.Sprintf("%d of %d modbus reads did not return data",
			e.Attempts-e.Successes, e.Attempts)
	}
}
