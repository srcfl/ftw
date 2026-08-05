package drivers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

var (
	// ErrControlBlocked is returned until the driver's autonomous default has
	// completed successfully after an ambiguous or failed control command.
	ErrControlBlocked = errors.New("driver control is blocked until autonomous default is confirmed")
	// ErrCommandMayHaveRun is returned when the caller canceled while the
	// driver command had already started but its final result was not yet
	// available. The registry restores the driver's default independently.
	ErrCommandMayHaveRun = errors.New("driver command may have run")
	// ErrObserveOnly is returned when a configured telemetry-only driver is
	// reached through a generic command path instead of the API guard.
	ErrObserveOnly = errors.New("driver is observe_only and cannot be controlled")
)

type commandMayHaveRunError struct {
	cause error
}

func (e *commandMayHaveRunError) Error() string {
	return fmt.Sprintf("%s: %v", ErrCommandMayHaveRun, e.cause)
}

func (e *commandMayHaveRunError) Unwrap() error { return e.cause }

func (e *commandMayHaveRunError) Is(target error) bool {
	return target == ErrCommandMayHaveRun
}

const (
	defaultRecoveryTimeout = 5 * time.Second
	defaultRetryInitial    = 100 * time.Millisecond
	defaultRetryMax        = 30 * time.Second
)

// DriverControlStatus describes the safety state that the API may expose to
// an operator. DefaultConfirmed is false unless the registry has completed a
// default-mode call successfully for the current driver instance.
type DriverControlStatus struct {
	Blocked          bool
	DefaultConfirmed bool
	RecoveryPending  bool
	Generation       uint64
}

// Registry manages running Lua driver instances — spawn, poll, command, stop.
// Thread-safe.
type Registry struct {
	tel                 *telemetry.Store
	troubleshootingMode bool

	// MQTTFactory creates an MQTT capability for a driver given its config.
	// Called on Add; the returned MQTTCap belongs to that driver alone.
	MQTTFactory func(name string, c *config.MQTTConfig) (MQTTCap, error)
	// ModbusFactory creates a Modbus capability.
	ModbusFactory func(name string, c *config.ModbusConfig) (ModbusCap, error)
	// SerialFactory opens one read-only local serial device.
	SerialFactory func(name string, c *config.SerialConfig) (SerialCap, error)
	// ARPLookup resolves a hostname/IP to a MAC for L2-stable identity.
	// Optional — when nil, devices fall back to endpoint-hash IDs.
	ARPLookup func(host string) (mac string, ok bool)
	// SecretPersister, when set, durably stores a driver secret (keyed by
	// driver name + key) in the unwatched state KV. Wired by main.go.
	// Optional — when nil, host.persist_secret returns an error and the
	// driver degrades (an OAuth driver re-uses its last in-memory token).
	SecretPersister func(driverName, key, value string) error
	// SecretOverride, when set, returns a durably-persisted secret for a
	// driver (the counterpart to SecretPersister). Applied over the
	// config.yaml value at driver_init so a rotated token survives a
	// restart. Returns ("", false) when no override exists.
	SecretOverride func(driverName, key string) (string, bool)
	// RuntimePolicyResolver returns verified signed policy for a managed
	// artifact. Nil means legacy/bundled/local v1 behavior.
	RuntimePolicyResolver func(config.Driver) (*RuntimePolicy, error)
	// CommandResultSink records completed v2 command and default-mode results.
	// A nil sink keeps tests and legacy setups simple.
	CommandResultSink func(driverName string, result DriverCommandResultV1)

	mu               sync.Mutex
	rec              map[string]*runningDriver
	recoveryRequired map[string]bool
	nextGeneration   uint64
	lifecycleHook    func(name string)
	lifecycleMu      sync.Mutex
	lifecycleGates   map[string]*lifecycleGate
}

// lifecycleGate serializes every lifecycle transition for one driver name.
// The reference count lets unused gates leave the map, while an acquire that
// races the final release still keeps the same gate alive until it owns it.
type lifecycleGate struct {
	mu   sync.Mutex
	refs int
}

// NewRegistry builds a driver registry.
func NewRegistry(tel *telemetry.Store) *Registry {
	return &Registry{
		tel:              tel,
		rec:              map[string]*runningDriver{},
		recoveryRequired: map[string]bool{},
		lifecycleGates:   map[string]*lifecycleGate{},
	}
}

func (r *Registry) acquireLifecycle(name string) func() {
	r.lifecycleMu.Lock()
	if r.lifecycleGates == nil {
		r.lifecycleGates = make(map[string]*lifecycleGate)
	}
	gate := r.lifecycleGates[name]
	if gate == nil {
		gate = &lifecycleGate{}
		r.lifecycleGates[name] = gate
	}
	gate.refs++
	r.lifecycleMu.Unlock()

	gate.mu.Lock()
	return func() {
		gate.mu.Unlock()
		r.lifecycleMu.Lock()
		gate.refs--
		if gate.refs == 0 && r.lifecycleGates[name] == gate {
			delete(r.lifecycleGates, name)
		}
		r.lifecycleMu.Unlock()
	}
}

// SetTroubleshootingMode updates the global incident-diagnostics flag used for
// subsequently started drivers. Use Reload to restart already-running drivers.
func (r *Registry) SetTroubleshootingMode(enabled bool) {
	r.mu.Lock()
	r.troubleshootingMode = enabled
	r.mu.Unlock()
}

// SetLifecycleHook installs the callback used by API owners to clear state
// before a driver instance is removed. The callback runs without r.mu held.
func (r *Registry) SetLifecycleHook(hook func(name string)) {
	r.mu.Lock()
	r.lifecycleHook = hook
	r.mu.Unlock()
}

// driverRuntime abstracts the Lua driver lifecycle so the registry's
// run-loop, command dispatch, and health tracking stay clean.
type driverRuntime interface {
	Init(ctx context.Context, configJSON []byte) error
	Poll(ctx context.Context) (time.Duration, error)
	Command(ctx context.Context, cmdJSON []byte) error
	DefaultMode(ctx context.Context) error
	Cleanup(ctx context.Context) error
	Env() *HostEnv
}

type controlV2Runtime interface {
	CommandV2(context.Context, DriverCommandV1, time.Time) (DriverCommandResultV1, error)
	DefaultModeV2(context.Context, string, string, time.Time) (DriverCommandResultV1, error)
}

// luaRuntime adapts *LuaDriver to driverRuntime. LuaDriver's internal
// signatures take a map (not raw JSON) for ergonomics, so we decode
// once at the boundary.
type luaRuntime struct{ *LuaDriver }

func (l *luaRuntime) Init(ctx context.Context, cfg []byte) error {
	var m map[string]any
	if len(cfg) > 0 {
		_ = json.Unmarshal(cfg, &m)
	}
	return l.LuaDriver.Init(ctx, m)
}
func (l *luaRuntime) DefaultMode(ctx context.Context) error {
	return l.LuaDriver.DefaultModeContext(ctx)
}
func (l *luaRuntime) Cleanup(ctx context.Context) error {
	l.LuaDriver.CleanupContext(ctx)
	return nil
}
func (l *luaRuntime) Env() *HostEnv { return l.LuaDriver.Env }
func (l *luaRuntime) CommandV2(ctx context.Context, cmd DriverCommandV1, now time.Time) (DriverCommandResultV1, error) {
	return l.LuaDriver.CommandV2(ctx, cmd, now)
}
func (l *luaRuntime) DefaultModeV2(ctx context.Context, id, reason string, now time.Time) (DriverCommandResultV1, error) {
	return l.LuaDriver.DefaultModeV2(ctx, id, reason, now)
}

func driverInitConfigJSON(cfg config.Driver, troubleshootingMode bool) []byte {
	if len(cfg.Config) == 0 && !troubleshootingMode && !cfg.SupportsPVCurtail {
		return nil
	}
	m := make(map[string]any, len(cfg.Config)+2)
	for k, v := range cfg.Config {
		m[k] = v
	}
	if troubleshootingMode {
		m["_troubleshooting_mode"] = true
	}
	if cfg.SupportsPVCurtail {
		// Runtime-only ownership signal. A curtail-capable driver may use
		// this to release an FTW-owned stale limit on startup without
		// touching inverter limits when the operator has not opted in.
		m["_supports_pv_curtail"] = true
	}
	b, _ := json.Marshal(m)
	return b
}

type runningDriver struct {
	driver             driverRuntime
	env                *HostEnv
	cfg                config.Driver
	policy             *RuntimePolicy
	leaseExpiresAt     time.Time
	generation         uint64
	statusMu           sync.RWMutex
	controlBlocked     bool
	defaultConfirmed   bool
	recoveryPending    bool
	activeMu           sync.Mutex
	activeCancel       context.CancelFunc
	lifecycleCtx       context.Context
	lifecycleCancel    context.CancelFunc
	shutdownMu         sync.Mutex
	shutdownDefaultErr error
	// Poll loop coordination
	cmdCh chan driverCmd
	stop  chan bool
	done  chan struct{}
}

func (rd *runningDriver) controlStatus() DriverControlStatus {
	rd.statusMu.RLock()
	defer rd.statusMu.RUnlock()
	return DriverControlStatus{
		Blocked:          rd.controlBlocked,
		DefaultConfirmed: rd.defaultConfirmed,
		RecoveryPending:  rd.recoveryPending,
		Generation:       rd.generation,
	}
}

func (rd *runningDriver) controlIsBlocked() bool {
	rd.statusMu.RLock()
	blocked := rd.controlBlocked
	rd.statusMu.RUnlock()
	return blocked
}

func (rd *runningDriver) markCommandApplied() {
	rd.statusMu.Lock()
	rd.defaultConfirmed = false
	rd.statusMu.Unlock()
}

func (rd *runningDriver) markDefaultConfirmed() {
	rd.statusMu.Lock()
	rd.controlBlocked = false
	rd.defaultConfirmed = true
	rd.recoveryPending = false
	rd.statusMu.Unlock()
}

func (rd *runningDriver) markDefaultRecoveryPending() {
	rd.statusMu.Lock()
	rd.controlBlocked = true
	rd.defaultConfirmed = false
	rd.recoveryPending = true
	rd.statusMu.Unlock()
}

func (rd *runningDriver) beginCommand(ctx context.Context) (context.Context, func()) {
	if ctx == nil {
		ctx = context.Background()
	}
	parent := rd.lifecycleCtx
	if parent == nil {
		parent = context.Background()
	}
	commandCtx, cancel := context.WithCancel(parent)
	stopCaller := context.AfterFunc(ctx, cancel)
	rd.activeMu.Lock()
	rd.activeCancel = cancel
	rd.activeMu.Unlock()
	return commandCtx, func() {
		rd.activeMu.Lock()
		rd.activeCancel = nil
		rd.activeMu.Unlock()
		stopCaller()
		cancel()
	}
}

func (rd *runningDriver) cancelActiveCommand() {
	rd.activeMu.Lock()
	cancel := rd.activeCancel
	rd.activeMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (rd *runningDriver) cancelLifecycle() {
	if rd.lifecycleCancel != nil {
		rd.lifecycleCancel()
	}
}

func (rd *runningDriver) setShutdownDefaultError(err error) {
	rd.shutdownMu.Lock()
	rd.shutdownDefaultErr = err
	rd.shutdownMu.Unlock()
}

func (rd *runningDriver) shutdownDefaultFailed() bool {
	rd.shutdownMu.Lock()
	failed := rd.shutdownDefaultErr != nil
	rd.shutdownMu.Unlock()
	return failed
}

type commandState struct {
	mu        sync.Mutex
	started   bool
	completed bool
	err       error
}

func (s *commandState) markStarted() {
	s.mu.Lock()
	s.started = true
	s.mu.Unlock()
}

func (s *commandState) finish(err error) {
	s.mu.Lock()
	s.completed = true
	s.err = err
	s.mu.Unlock()
}

func (s *commandState) snapshot() (err error, started, completed bool) {
	s.mu.Lock()
	err, started, completed = s.err, s.started, s.completed
	s.mu.Unlock()
	return err, started, completed
}

func commandContextError(requestCtx, executionCtx context.Context) error {
	if requestCtx != nil {
		if err := requestCtx.Err(); err != nil {
			return err
		}
	}
	if executionCtx != nil {
		if err := executionCtx.Err(); err != nil {
			return err
		}
	}
	return nil
}

type driverCmd struct {
	kind    string
	ctx     context.Context
	payload []byte
	result  chan error
	state   *commandState
}

// Add spawns an operational driver. Before a legacy driver becomes
// controllable, it must confirm its autonomous default once for this
// process. A failed default leaves the driver registered but blocked so the
// recovery loop can retry without opening a control window.
func (r *Registry) Add(ctx context.Context, cfg config.Driver) error {
	release := r.acquireLifecycle(cfg.Name)
	defer release()
	return r.add(ctx, cfg, true)
}

// AddProbe spawns a short-lived, read-only probe. Probes must not write a
// device's default mode during connection testing, and Send rejects them via
// the observe-only flag on the private config copy.
func (r *Registry) AddProbe(ctx context.Context, cfg config.Driver) error {
	cfg.ObserveOnly = true
	release := r.acquireLifecycle(cfg.Name)
	defer release()
	return r.add(ctx, cfg, false)
}

func legacyDriverDeclaresControls(path string) (bool, error) {
	entry, err := ParseCatalogFile(path)
	if err != nil {
		return false, err
	}
	return len(entry.Controls) > 0, nil
}

// add is the shared driver construction path. The caller must hold the
// lifecycle gate for cfg.Name. startupDefault is false only for connection
// probes; operational drivers use the startup safety gate.
func (r *Registry) add(ctx context.Context, cfg config.Driver, startupDefault bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.Lock()
	if _, exists := r.rec[cfg.Name]; exists {
		r.mu.Unlock()
		return fmt.Errorf("driver %q already registered", cfg.Name)
	}
	r.mu.Unlock()

	if cfg.Lua == "" {
		return fmt.Errorf("driver %q: must specify `lua` path", cfg.Name)
	}
	var policy *RuntimePolicy
	if r.RuntimePolicyResolver != nil {
		resolved, policyErr := r.RuntimePolicyResolver(cfg)
		if policyErr != nil {
			return fmt.Errorf("runtime policy: %w", policyErr)
		}
		policy = resolved
	}

	env := NewHostEnv(cfg.Name, r.tel)
	env.BatteryCapacityWh = cfg.BatteryCapacityWh
	env.BatteryTelemetryOnly = cfg.BatteryTelemetryOnly
	// Wire durable secret write-back (rotated OAuth tokens). The closure
	// reads r.SecretPersister lazily at call time so main.go may set it
	// either before or after the initial Add loop; persists only ever
	// happen at runtime poll, long after wiring completes.
	driverName := cfg.Name
	env.PersistSecret = func(key, value string) error {
		if r.SecretPersister == nil {
			return fmt.Errorf("persist_secret: not supported on this host")
		}
		return r.SecretPersister(driverName, key, value)
	}
	if mq := cfg.EffectiveMQTT(); mq != nil && r.MQTTFactory != nil {
		cap, err := r.MQTTFactory(cfg.Name, mq)
		if err != nil {
			return fmt.Errorf("mqtt capability: %w", err)
		}
		env.WithMQTT(cap)
		env.SetEndpoint(fmt.Sprintf("mqtt://%s:%d", mq.Host, mq.Port))
		// Best-effort MAC resolution. Cross-VLAN devices return ""; that's
		// fine — device_id falls back to the endpoint.
		if r.ARPLookup != nil {
			if mac, ok := r.ARPLookup(mq.Host); ok {
				env.SetMAC(mac)
			}
		}
	}
	if mb := cfg.EffectiveModbus(); mb != nil && r.ModbusFactory != nil {
		cap, err := r.ModbusFactory(cfg.Name, mb)
		if err != nil {
			return fmt.Errorf("modbus capability: %w", err)
		}
		env.WithModbus(cap)
		env.SetEndpoint(fmt.Sprintf("modbus://%s:%d", mb.Host, mb.Port))
		if r.ARPLookup != nil {
			if mac, ok := r.ARPLookup(mb.Host); ok {
				env.SetMAC(mac)
			}
		}
	}
	if serialCfg := cfg.Capabilities.Serial; serialCfg != nil && r.SerialFactory != nil {
		cap, err := r.SerialFactory(cfg.Name, serialCfg)
		if err != nil {
			return fmt.Errorf("serial capability: %w", err)
		}
		env.WithSerial(cap)
		env.SetEndpoint("serial://" + serialCfg.Address)
	}
	if cfg.Capabilities.HTTP != nil {
		env.WithHTTP()
		hosts := mergeAllowedHosts(cfg.Capabilities.HTTP.AllowedHosts, cfg.Config)
		if len(hosts) > 0 {
			env.WithHTTPAllowedHosts(hosts)
		}
		if pin := strings.TrimSpace(cfg.Capabilities.HTTP.TLSPinSHA256); pin != "" {
			env.WithHTTPTLSPin(pin)
		}
		if cfg.Capabilities.HTTP.AllowWrite {
			env.WithHTTPAllowWrite()
		}
	}
	if cfg.Capabilities.WebSocket != nil {
		env.WithWS(NewGorillaWS(cfg.Name))
		if hosts := cfg.Capabilities.WebSocket.AllowedHosts; len(hosts) > 0 {
			env.WithWSAllowedHosts(hosts)
		}
	}
	if cfg.Capabilities.TCP != nil {
		// Start with the explicit allowlist from YAML, then layer the
		// driver's own (host, port) on top as a TIGHT host:port entry —
		// not a bare host the way HTTP/WS do it. Raw TCP can hit any
		// service listening on the device (SSH, web UI, ...), so the
		// safe default is "exactly the port the driver was wired to";
		// the operator can still loosen this by listing bare hosts in
		// capabilities.tcp.allowed_hosts when they want any-port access.
		hosts := tcpAllowedHostsFor(cfg)
		env.WithTCP(NewNetTCP(cfg.Name, hosts))
		if len(hosts) > 0 {
			env.WithTCPAllowedHosts(hosts)
		}
	}

	luaDrv, err := NewLuaDriverWithPolicy(cfg.Lua, env, policy)
	if err != nil {
		return fmt.Errorf("load lua: %w", err)
	}
	if !cfg.ObserveOnly && (policy == nil || !policy.IsControlV2()) {
		declaresControls, catalogErr := legacyDriverDeclaresControls(cfg.Lua)
		if catalogErr != nil {
			luaDrv.CleanupContext(ctx)
			return fmt.Errorf("validate legacy driver controls: %w", catalogErr)
		}
		if declaresControls && !luaDrv.hasEntrypoint("driver_default_mode") {
			luaDrv.CleanupContext(ctx)
			return fmt.Errorf("driver %q declares operator controls but is missing required driver_default_mode", cfg.Name)
		}
	}
	var drv driverRuntime = &luaRuntime{LuaDriver: luaDrv}

	r.mu.Lock()
	troubleshootingMode := r.troubleshootingMode
	r.mu.Unlock()

	// Layer any durably-persisted secret overrides (e.g. a rotated OAuth
	// refresh_token from the unwatched state KV) over the config.yaml
	// values, for driver_init only. rd.cfg below keeps the ORIGINAL cfg so
	// the config-watcher's sameDriverConfig comparison stays stable — a
	// rotated token must never look like an operator edit (that would
	// trigger a restart→re-auth→rotate loop).
	initDriverCfg := cfg
	if r.SecretOverride != nil && len(cfg.Config) > 0 {
		merged := make(map[string]any, len(cfg.Config))
		for k, v := range cfg.Config {
			if ov, ok := r.SecretOverride(cfg.Name, k); ok {
				merged[k] = ov
			} else {
				merged[k] = v
			}
		}
		initDriverCfg.Config = merged
	}

	// Pass the driver's config map as JSON to driver_init, with reserved
	// host-level keys injected only at runtime.
	initCfg := driverInitConfigJSON(initDriverCfg, troubleshootingMode)
	if err := drv.Init(ctx, initCfg); err != nil {
		drv.Cleanup(ctx)
		return fmt.Errorf("driver_init: %w", err)
	}
	if startupDefault && !cfg.ObserveOnly && policy != nil && policy.IsControlV2() {
		v2 := drv.(controlV2Runtime)
		result, defaultErr := v2.DefaultModeV2(ctx, newControlID("default"), "host_start", time.Now())
		r.recordCommandResult(cfg.Name, result)
		if defaultErr != nil {
			drv.Cleanup(ctx)
			return fmt.Errorf("driver_default_mode_v2 on startup: %w", defaultErr)
		}
	}
	var startupDefaultErr error
	if startupDefault && !cfg.ObserveOnly && (policy == nil || !policy.IsControlV2()) {
		defaultCtx, cancel := context.WithTimeout(ctx, defaultRecoveryTimeout)
		startupDefaultErr = drv.DefaultMode(defaultCtx)
		cancel()
		if startupDefaultErr != nil {
			slog.Error("driver failed to confirm autonomous default at startup; control remains blocked", "name", cfg.Name, "err", startupDefaultErr)
		}
	}

	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	rd := &runningDriver{
		driver:          drv,
		env:             env,
		cfg:             cfg,
		policy:          policy,
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
		cmdCh:           make(chan driverCmd, 8),
		stop:            make(chan bool, 1),
		done:            make(chan struct{}),
	}
	r.mu.Lock()
	r.nextGeneration++
	rd.generation = r.nextGeneration
	inheritsRecovery := r.recoveryRequired[cfg.Name]
	if startupDefault && !cfg.ObserveOnly && policy != nil && policy.IsControlV2() {
		rd.defaultConfirmed = true
		// The v2 startup default above is a confirmed recovery for a
		// replacement generation. It does not need the legacy retry path.
		if inheritsRecovery {
			delete(r.recoveryRequired, cfg.Name)
		}
	} else if startupDefault && !cfg.ObserveOnly && (policy == nil || !policy.IsControlV2()) {
		if startupDefaultErr == nil {
			rd.defaultConfirmed = true
			if inheritsRecovery {
				delete(r.recoveryRequired, cfg.Name)
			}
		} else {
			rd.controlBlocked = true
			rd.defaultConfirmed = false
			rd.recoveryPending = true
		}
	} else if inheritsRecovery {
		// A replacement that cannot run a startup default (observe_only or a
		// probe) remains unavailable if its predecessor left a lifecycle gate.
		rd.controlBlocked = true
		rd.defaultConfirmed = false
		rd.recoveryPending = true
	}
	r.rec[cfg.Name] = rd
	r.mu.Unlock()
	// Create the health record eagerly so /api/status reflects
	// "driver is running" the instant Add returns, instead of
	// rendering as `not_running: true` until the first successful
	// emit. The previous lazy-on-emit pattern made a freshly-
	// restarted MQTT driver look dead until the first message
	// arrived (which can be 30+ s for slow telemetry topics), and
	// mis-presented an alive-but-waiting driver as a failed one.
	if r.tel != nil {
		r.tel.EnsureDriverHealth(cfg.Name)
	}
	go r.runLoop(rd)
	slog.Info("driver added", "name", cfg.Name, "path", cfg.Lua)
	return nil
}

// runLoop polls the driver at its requested cadence and handles commands.
func (r *Registry) runLoop(rd *runningDriver) {
	defer close(rd.done)
	ctx := rd.lifecycleCtx
	if ctx == nil {
		ctx = context.Background()
	}
	// The first tick can be held back by host.set_warmup_s for a device
	// that answers Modbus before its registers mean anything. Every
	// later tick uses the plain poll interval.
	interval := rd.env.FirstPollDelay()
	timer := time.NewTimer(interval)
	defer timer.Stop()
	leaseTimer := time.NewTimer(time.Hour)
	if !leaseTimer.Stop() {
		<-leaseTimer.C
	}
	defer leaseTimer.Stop()
	var leaseC <-chan time.Time
	var recoveryTimer *time.Timer
	var recoveryC <-chan time.Time
	retryDelay := defaultRetryInitial
	clearLease := func() {
		rd.leaseExpiresAt = time.Time{}
		if !leaseTimer.Stop() {
			select {
			case <-leaseTimer.C:
			default:
			}
		}
		leaseC = nil
	}
	armLease := func(expiresAt time.Time) {
		clearLease()
		rd.leaseExpiresAt = expiresAt
		d := time.Until(expiresAt)
		if d < time.Millisecond {
			d = time.Millisecond
		}
		leaseTimer.Reset(d)
		leaseC = leaseTimer.C
	}
	clearRecoveryTimer := func() {
		if recoveryTimer != nil {
			if !recoveryTimer.Stop() {
				select {
				case <-recoveryTimer.C:
				default:
				}
			}
		}
		recoveryC = nil
		retryDelay = defaultRetryInitial
	}
	scheduleRecovery := func() {
		d := retryDelay
		if d <= 0 {
			d = defaultRetryInitial
		}
		if recoveryTimer == nil {
			recoveryTimer = time.NewTimer(d)
		} else {
			if !recoveryTimer.Stop() {
				select {
				case <-recoveryTimer.C:
				default:
				}
			}
			recoveryTimer.Reset(d)
		}
		recoveryC = recoveryTimer.C
		rd.markDefaultRecoveryPending()
		if retryDelay < defaultRetryMax {
			retryDelay *= 2
			if retryDelay > defaultRetryMax {
				retryDelay = defaultRetryMax
			}
		}
	}
	defer clearRecoveryTimer()
	if rd.controlIsBlocked() {
		scheduleRecovery()
	}
	attemptDefault := func(reason string) error {
		defaultCtx, cancel := context.WithTimeout(context.Background(), defaultRecoveryTimeout)
		defaultErr := r.defaultDriver(defaultCtx, rd, reason)
		cancel()
		if defaultErr != nil {
			scheduleRecovery()
			return defaultErr
		}
		rd.markDefaultConfirmed()
		clearLease()
		clearRecoveryTimer()
		r.clearRecoveryRequired(rd.cfg.Name, rd)
		return nil
	}
	restoreAfterCommand := func(commandErr error) error {
		clearLease()
		defaultErr := attemptDefault("command_failed")
		commandOutcome := &commandMayHaveRunError{cause: commandErr}
		if defaultErr != nil {
			return errors.Join(commandOutcome, fmt.Errorf("%w: restore default after ambiguous command: %v", ErrControlBlocked, defaultErr))
		}
		return commandOutcome
	}
	for {
		select {
		case skipDefault := <-rd.stop:
			if !skipDefault {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), defaultRecoveryTimeout)
				defaultErr := r.defaultDriver(shutdownCtx, rd, "host_shutdown")
				rd.setShutdownDefaultError(defaultErr)
				if defaultErr != nil {
					slog.Error("driver failed to enter default mode during shutdown", "name", rd.cfg.Name, "err", defaultErr)
				}
				cancel()
			}
			_ = rd.driver.Cleanup(ctx)
			// Tear down capability connections so a subsequent Add
			// with the same driver name doesn't race an old MQTT
			// session (broker resolves the conflict by kicking the
			// newer one on the next connect, and subscribe ACKs get
			// lost). Modbus TCP connections similarly need an explicit
			// close so the server side frees the slot.
			if rd.env.MQTT != nil {
				_ = rd.env.MQTT.Close()
			}
			if rd.env.Modbus != nil {
				_ = rd.env.Modbus.Close()
			}
			if rd.env.Serial != nil {
				_ = rd.env.Serial.Close()
			}
			if rd.env.WS != nil {
				_ = rd.env.WS.Close()
			}
			if rd.env.TCP != nil {
				_ = rd.env.TCP.Close()
			}
			return
		case cmd := <-rd.cmdCh:
			var err error
			cmdCtx := cmd.ctx
			if cmdCtx == nil {
				cmdCtx = ctx
			}
			switch cmd.kind {
			case "command":
				if rd.controlIsBlocked() {
					err = ErrControlBlocked
					break
				}
				if cancelErr := cmdCtx.Err(); cancelErr != nil {
					err = cancelErr
					break
				}
				if cmd.state != nil {
					cmd.state.markStarted()
				}
				commandCtx, finishCommand := rd.beginCommand(cmdCtx)
				if rd.policy != nil && rd.policy.IsControlV2() {
					var result DriverCommandResultV1
					var leaseExpiresAt time.Time
					result, leaseExpiresAt, err = r.dispatchV2Command(commandCtx, rd, cmd.payload)
					r.recordCommandResult(rd.cfg.Name, result)
					if err == nil {
						err = commandContextError(cmdCtx, commandCtx)
					}
					if err == nil && result.Status == "applied" && result.DeviceState == "controlled" {
						armLease(leaseExpiresAt)
						rd.markCommandApplied()
					} else if err == nil && result.Status == "applied" && result.DeviceState == "default" {
						clearLease()
						rd.markDefaultConfirmed()
						clearRecoveryTimer()
					} else if err != nil {
						err = restoreAfterCommand(err)
					}
				} else {
					err = rd.driver.Command(commandCtx, cmd.payload)
					if err == nil {
						err = commandContextError(cmdCtx, commandCtx)
					}
					if err != nil {
						err = restoreAfterCommand(err)
					} else {
						rd.markCommandApplied()
					}
				}
				finishCommand()
			case "default":
				err = r.defaultDriver(cmdCtx, rd, "host_request")
				if err == nil {
					clearLease()
					rd.markDefaultConfirmed()
					clearRecoveryTimer()
					r.clearRecoveryRequired(rd.cfg.Name, rd)
				} else {
					scheduleRecovery()
				}
			}
			if cmd.state != nil {
				cmd.state.finish(err)
			}
			if cmd.result != nil {
				cmd.result <- err
			}
		case <-timer.C:
			pollFailed := false
			if _, err := rd.driver.Poll(ctx); err != nil {
				pollFailed = true
				slog.Warn("driver poll failed", "name", rd.cfg.Name, "err", err)
				if r.tel != nil {
					r.tel.RecordDriverError(rd.cfg.Name, err.Error())
				}
			} else if r.tel != nil {
				// Bump TickCount so the loop is visibly alive in
				// /api/status, but DON'T touch LastSuccess — that
				// happens inside host.emit when the driver actually
				// delivers telemetry. A driver that polls without
				// emitting (waiting for first MQTT message, or feeding
				// stale cache after upstream death) needs to surface
				// as stale to the watchdog; otherwise a dead ferroamp
				// re-stamps LastSuccess every tick from cached values.
				r.tel.RecordDriverTick(rd.cfg.Name)
			}
			if rd.policy != nil && rd.policy.IsControlV2() && !rd.leaseExpiresAt.IsZero() {
				var health *telemetry.DriverHealth
				if r.tel != nil {
					health = r.tel.DriverHealth(rd.cfg.Name)
				}
				if pollFailed || health == nil || !health.IsOnline() {
					clearLease()
					if err := attemptDefault("driver_stale"); err != nil {
						slog.Error("driver stale default mode failed; control blocked", "name", rd.cfg.Name, "err", err)
					}
				}
			}
			// Re-arm timer at driver's requested interval
			interval = rd.env.PollInterval()
			timer.Reset(interval)
		case <-leaseC:
			clearLease()
			if err := attemptDefault("lease_expired"); err != nil {
				slog.Error("driver lease expiry default mode failed; control blocked", "name", rd.cfg.Name, "err", err)
			}
		case <-recoveryC:
			if err := attemptDefault("control_recovery"); err != nil {
				slog.Error("driver default recovery failed; control remains blocked", "name", rd.cfg.Name, "err", err)
			}
		}
	}
}

func (r *Registry) dispatchV2Command(ctx context.Context, rd *runningDriver, payload []byte) (DriverCommandResultV1, time.Time, error) {
	var legacy map[string]interface{}
	if err := json.Unmarshal(payload, &legacy); err != nil {
		return DriverCommandResultV1{}, time.Time{}, err
	}
	action, _ := legacy["action"].(string)
	var declared *RuntimeCommand
	for _, candidate := range rd.policy.Commands {
		if candidate.RuntimeAction == action {
			if declared != nil {
				return DriverCommandResultV1{}, time.Time{}, fmt.Errorf("signed package maps more than one command to runtime action %q", action)
			}
			copy := candidate
			declared = &copy
		}
	}
	if declared == nil {
		return DriverCommandResultV1{}, time.Time{}, fmt.Errorf("runtime action %q is not declared by the signed package", action)
	}
	inputs := make(map[string]interface{}, len(declared.Inputs))
	for name := range declared.Inputs {
		if value, ok := legacy[name]; ok {
			inputs[name] = value
			continue
		}
		if name == "power_w" {
			if value, ok := legacy["w"]; ok {
				inputs[name] = value
			}
		}
	}
	now := time.Now()
	heartbeat := rd.policy.Lease.HeartbeatInterval
	if heartbeat < time.Second {
		heartbeat = time.Second
	}
	leaseDuration := 2 * heartbeat
	if leaseDuration > rd.policy.Lease.MaxDuration {
		leaseDuration = rd.policy.Lease.MaxDuration
	}
	commandDeadline := now.Add(5 * time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(commandDeadline) {
		commandDeadline = ctxDeadline
	}
	cmd := DriverCommandV1{
		SchemaVersion: DriverCommandSchemaV1,
		ID:            newControlID("command"), Command: declared.ID, Source: "ftw.control",
		IssuedAt: now.UTC(), ExpiresAt: commandDeadline.UTC(), Attempt: 1, Inputs: inputs,
		Lease: DriverCommandLeaseV1{
			ID: newControlID("lease"), ExpiresAt: now.Add(leaseDuration).UTC(),
			HeartbeatIntervalMS: heartbeat.Milliseconds(),
		},
	}
	result, err := rd.driver.(controlV2Runtime).CommandV2(ctx, cmd, now)
	if err == nil && result.Status != "applied" {
		err = fmt.Errorf("driver command returned %s, not applied", result.Status)
	}
	return result, cmd.Lease.ExpiresAt, err
}

func (r *Registry) defaultDriver(ctx context.Context, rd *runningDriver, reason string) error {
	if rd.policy == nil || !rd.policy.IsControlV2() {
		return rd.driver.DefaultMode(ctx)
	}
	result, err := rd.driver.(controlV2Runtime).DefaultModeV2(ctx, newControlID("default"), reason, time.Now())
	r.recordCommandResult(rd.cfg.Name, result)
	return err
}

func (r *Registry) recordCommandResult(driverName string, result DriverCommandResultV1) {
	if r.CommandResultSink != nil && result.SchemaVersion != "" {
		result.CompletedAt = time.Now().UTC()
		r.CommandResultSink(driverName, result)
	}
}

func newControlID(kind string) string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Sprintf("ftw.%s:%d", kind, time.Now().UnixNano())
	}
	return "ftw." + kind + ":" + hex.EncodeToString(raw)
}

// Remove stops and cleans up a driver. Idempotent. Also wipes the
// driver's entry from the telemetry store so the API status + UI stop
// showing a stale card for a driver that's no longer in config.
func (r *Registry) Remove(name string) {
	r.remove(name, false)
}

// RemoveProbe stops a short-lived probe driver without sending
// driver_default_mode. Test-connection probes must not change device
// operating mode as a side effect of cleanup.
func (r *Registry) RemoveProbe(name string) {
	r.remove(name, true)
}

// clearRecoveryRequired removes the name-level lifecycle gate only when the
// generation that confirmed default is still the current one. If a new
// generation was added while the old one was shutting down, that replacement
// must keep its own recovery gate and retry.
func (r *Registry) clearRecoveryRequired(name string, rd *runningDriver) {
	r.mu.Lock()
	if current, ok := r.rec[name]; ok && current != rd {
		r.mu.Unlock()
		return
	}
	delete(r.recoveryRequired, name)
	r.mu.Unlock()
}

func (r *Registry) remove(name string, skipDefault bool) {
	release := r.acquireLifecycle(name)
	defer release()
	r.removeLocked(name, skipDefault)
}

// removeLocked stops a driver while its per-name lifecycle gate is held. This
// lets Restart and Reload keep the old generation and its replacement in one
// serialized transition instead of opening an Add/Remove gap.
func (r *Registry) removeLocked(name string, skipDefault bool) {
	r.mu.Lock()
	rd, ok := r.rec[name]
	if !ok {
		r.mu.Unlock()
		return
	}
	if !skipDefault {
		if r.recoveryRequired == nil {
			r.recoveryRequired = make(map[string]bool)
		}
		// Gate a replacement before the old generation is canceled. A
		// concurrent Add must not become controllable while shutdown default
		// is still unknown.
		r.recoveryRequired[name] = true
	}
	delete(r.rec, name)
	hook := r.lifecycleHook
	r.mu.Unlock()
	// A legacy Lua command may be looping after it has already written the
	// device. Cancel it before waiting for the lifecycle callback or stop
	// signal, otherwise restart and shutdown can wait forever for runLoop.
	rd.cancelLifecycle()
	rd.cancelActiveCommand()
	if hook != nil {
		hook(name)
	}
	rd.stop <- skipDefault
	<-rd.done
	if !skipDefault {
		if rd.shutdownDefaultFailed() {
			// A recovery timer may have succeeded while shutdown was
			// waiting. The shutdown result is the final lifecycle boundary,
			// so reassert the gate if that default failed.
			r.mu.Lock()
			if r.recoveryRequired == nil {
				r.recoveryRequired = make(map[string]bool)
			}
			r.recoveryRequired[name] = true
			r.mu.Unlock()
		} else {
			r.clearRecoveryRequired(name, rd)
		}
	}
	if r.tel != nil {
		r.tel.Remove(name)
	}
	slog.Info("driver removed", "name", name)
}

// Send dispatches a command JSON blob to a specific driver. Blocks until the
// driver's runLoop processes it or ctx expires.
func (r *Registry) Send(ctx context.Context, name string, payload []byte) error {
	_, err := r.SendWithGeneration(ctx, name, payload)
	return err
}

// SendWithGeneration dispatches a command to the concrete running driver that
// was selected for this call and returns that instance's generation. Selecting
// the instance and recording its generation under the registry lock prevents a
// caller from pairing a command sent to a replacement with an older status
// snapshot.
func (r *Registry) SendWithGeneration(ctx context.Context, name string, payload []byte) (uint64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	r.mu.Lock()
	rd, ok := r.rec[name]
	generation := uint64(0)
	if ok {
		generation = rd.generation
	}
	r.mu.Unlock()
	if !ok {
		return 0, fmt.Errorf("driver %q not found", name)
	}
	if rd.cfg.ObserveOnly {
		return generation, ErrObserveOnly
	}
	if rd.controlIsBlocked() {
		return generation, ErrControlBlocked
	}
	if err := ctx.Err(); err != nil {
		return generation, err
	}
	resCh := make(chan error, 1)
	state := &commandState{}
	select {
	case rd.cmdCh <- driverCmd{kind: "command", ctx: ctx, payload: payload, result: resCh, state: state}:
	case <-ctx.Done():
		return generation, ctx.Err()
	}
	select {
	case err := <-resCh:
		return generation, err
	case <-ctx.Done():
		err, started, completed := state.snapshot()
		if completed {
			return generation, err
		}
		if started {
			return generation, &commandMayHaveRunError{cause: ctx.Err()}
		}
		return generation, ctx.Err()
	}
}

// SendDefault sends the default/watchdog command to a driver. Symmetric
// with Send: both the channel-push and the result-wait honour ctx. A
// driver whose cmdCh is full (because its goroutine is slow / stuck mid
// I/O) would otherwise block the caller forever; the watchdog-fallback
// path runs on every dispatch tick, so an unblocked send into a wedged
// driver deadlocks the entire control loop.
func (r *Registry) SendDefault(ctx context.Context, name string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.Lock()
	rd, ok := r.rec[name]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("driver %q not found", name)
	}
	resCh := make(chan error, 1)
	select {
	case rd.cmdCh <- driverCmd{kind: "default", ctx: ctx, result: resCh}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-resCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ControlStatus returns the command safety state for the current driver
// generation without creating or changing any state.
func (r *Registry) ControlStatus(name string) (DriverControlStatus, bool) {
	r.mu.Lock()
	rd, ok := r.rec[name]
	r.mu.Unlock()
	if !ok {
		return DriverControlStatus{}, false
	}
	return rd.controlStatus(), true
}

// Names returns the currently registered driver names.
// Env returns the HostEnv for a driver, or nil if not registered.
// Used by main to read identity (make/sn/mac/endpoint) after init.
func (r *Registry) Env(name string) *HostEnv {
	r.mu.Lock()
	rd, ok := r.rec[name]
	r.mu.Unlock()
	if !ok {
		return nil
	}
	return rd.env
}

func (r *Registry) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.rec))
	for n := range r.rec {
		out = append(out, n)
	}
	return out
}

// ShutdownAll stops every driver. Blocks until all poll loops exit.
func (r *Registry) ShutdownAll() {
	r.mu.Lock()
	names := make([]string, 0, len(r.rec))
	for n := range r.rec {
		names = append(names, n)
	}
	r.mu.Unlock()
	for _, n := range names {
		r.Remove(n)
	}
}

// Reload diffs a new driver list against running state and applies add/
// remove/restart. Drivers with changed lua path, capabilities, or config
// map are restarted. Drivers marked Disabled are treated as "not in the
// new list" — running ones get stopped, missing ones are not added.
func (r *Registry) Reload(ctx context.Context, newDrivers []config.Driver, troubleshootingMode bool) {
	// Filter out disabled drivers — they behave like removed from the
	// registry's perspective but remain in config.yaml for re-enable.
	active := make([]config.Driver, 0, len(newDrivers))
	for _, d := range newDrivers {
		if d.Disabled {
			continue
		}
		active = append(active, d)
	}

	r.mu.Lock()
	troubleshootingChanged := r.troubleshootingMode != troubleshootingMode
	r.troubleshootingMode = troubleshootingMode
	oldCfgs := make(map[string]config.Driver, len(r.rec))
	for n, rd := range r.rec {
		oldCfgs[n] = rd.cfg
	}
	r.mu.Unlock()

	// Remove or restart. Keep the same name gate through the whole transition;
	// otherwise an Add can register between Remove and the replacement Add.
	for n, old := range oldCfgs {
		newCfg, stillThere := findDriver(active, n)
		requiresRestart := stillThere && (troubleshootingChanged || !sameDriverConfig(old, newCfg))
		if !stillThere {
			release := r.acquireLifecycle(n)
			r.removeLocked(n, false)
			release()
		} else if requiresRestart {
			if troubleshootingChanged {
				slog.Info("driver troubleshooting mode changed, restarting", "name", n, "enabled", troubleshootingMode)
			} else {
				slog.Info("driver config changed, restarting", "name", n)
			}
			release := r.acquireLifecycle(n)
			r.removeLocked(n, false)
			if err := r.add(ctx, newCfg, true); err != nil {
				slog.Warn("reload driver failed", "name", n, "err", err)
			}
			release()
		}
	}
	// Add new
	for _, d := range active {
		release := r.acquireLifecycle(d.Name)
		r.mu.Lock()
		current, exists := r.rec[d.Name]
		var currentCfg config.Driver
		if exists {
			currentCfg = current.cfg
		}
		r.mu.Unlock()
		if exists && !troubleshootingChanged && sameDriverConfig(currentCfg, d) {
			release()
			continue
		}
		if exists {
			r.removeLocked(d.Name, false)
		}
		if err := r.add(ctx, d, true); err != nil {
			slog.Warn("add driver failed", "name", d.Name, "err", err)
		}
		release()
	}
}

// Restart stops a driver (if running) and re-adds it with the provided cfg.
// If cfg.Disabled is true, this is a no-op after the stop. Used by the API
// restart endpoint so the driver picks up fresh credentials / re-auths.
func (r *Registry) Restart(ctx context.Context, cfg config.Driver) error {
	release := r.acquireLifecycle(cfg.Name)
	defer release()
	r.removeLocked(cfg.Name, false)
	if cfg.Disabled {
		return nil
	}
	return r.add(ctx, cfg, true)
}

// Restart a driver by name using whatever cfg it was last started with.
// Returns an error if the driver isn't running (use Restart with a cfg
// to spawn from scratch).
func (r *Registry) RestartByName(ctx context.Context, name string) error {
	release := r.acquireLifecycle(name)
	defer release()
	r.mu.Lock()
	rd, ok := r.rec[name]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("driver %q not running", name)
	}
	cfg := rd.cfg
	r.removeLocked(name, false)
	if cfg.Disabled {
		return nil
	}
	return r.add(ctx, cfg, true)
}

// PollInterval returns the currently requested cadence for a running driver.
// Repository activation uses it to wait for two fresh cycles before declaring
// a new artifact healthy.
func (r *Registry) PollInterval(name string) (time.Duration, bool) {
	r.mu.Lock()
	rd, ok := r.rec[name]
	r.mu.Unlock()
	if !ok {
		return 0, false
	}
	return rd.env.PollInterval(), true
}

// tcpAllowedHostsFor builds the effective allowlist for a TCP-capable
// driver. Explicit `capabilities.tcp.allowed_hosts` entries come first
// (verbatim — operator can write either "host" or "host:port"). The
// driver's own `config.host` is then auto-added as a tight `host:port`
// entry when `config.port` is also set, falling back to bare host
// otherwise. The tight default is deliberate: raw TCP can poke any
// service on the same IP, so "P1 reader on :23" should not also grant
// access to SSH on :22 of the same device.
func tcpAllowedHostsFor(cfg config.Driver) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, 4)
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		if _, ok := seen[v]; ok {
			return
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	for _, h := range cfg.Capabilities.TCP.AllowedHosts {
		add(h)
	}
	if cfg.Config != nil {
		host, _ := cfg.Config["host"].(string)
		var port int
		switch p := cfg.Config["port"].(type) {
		case int:
			port = p
		case int64:
			port = int(p)
		case float64:
			port = int(p)
		}
		if host != "" && port > 0 {
			add(fmt.Sprintf("%s:%d", host, port))
		} else if host != "" {
			add(host)
		}
	}
	return out
}

// mergeAllowedHosts returns the explicit allowlist plus any host implied
// by the driver's free-form config (`host` or `url` keys), deduplicated.
// Saves the user from listing the same IP under both `config.host` and
// `capabilities.http.allowed_hosts` — the common foot-gun when a driver
// only talks to one device.
func mergeAllowedHosts(explicit []string, drvCfg map[string]any) []string {
	seen := make(map[string]struct{}, len(explicit)+2)
	out := make([]string, 0, len(explicit)+2)
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		if _, ok := seen[v]; ok {
			return
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	for _, h := range explicit {
		add(h)
	}
	if drvCfg != nil {
		if v, ok := drvCfg["host"].(string); ok {
			add(v)
		}
		if v, ok := drvCfg["url"].(string); ok {
			if u, err := url.Parse(v); err == nil && u.Host != "" {
				add(u.Host)
			}
		}
	}
	return out
}

func findDriver(list []config.Driver, name string) (config.Driver, bool) {
	for _, d := range list {
		if d.Name == name {
			return d, true
		}
	}
	return config.Driver{}, false
}

func sameDriverConfig(a, b config.Driver) bool {
	if a.Lua != b.Lua ||
		a.IsSiteMeter != b.IsSiteMeter ||
		a.BatteryCapacityWh != b.BatteryCapacityWh ||
		a.BatteryTelemetryOnly != b.BatteryTelemetryOnly ||
		a.ObserveOnly != b.ObserveOnly ||
		a.Disabled != b.Disabled ||
		!reflect.DeepEqual(a.Control, b.Control) {
		return false
	}
	aMq, bMq := a.EffectiveMQTT(), b.EffectiveMQTT()
	if (aMq == nil) != (bMq == nil) {
		return false
	}
	if aMq != nil && (aMq.Host != bMq.Host || aMq.Port != bMq.Port ||
		aMq.Username != bMq.Username || aMq.Password != bMq.Password) {
		return false
	}
	aMb, bMb := a.EffectiveModbus(), b.EffectiveModbus()
	if (aMb == nil) != (bMb == nil) {
		return false
	}
	if aMb != nil && (aMb.Host != bMb.Host || aMb.Port != bMb.Port || aMb.UnitID != bMb.UnitID) {
		return false
	}
	if !reflect.DeepEqual(a.Capabilities.Serial, b.Capabilities.Serial) ||
		a.Capabilities.Standalone != b.Capabilities.Standalone {
		return false
	}
	aTCP, bTCP := a.Capabilities.TCP, b.Capabilities.TCP
	if (aTCP == nil) != (bTCP == nil) {
		return false
	}
	if aTCP != nil && !reflect.DeepEqual(aTCP.AllowedHosts, bTCP.AllowedHosts) {
		return false
	}
	// Compare the free-form Config map. Previously omitted, so a changed
	// cloud-driver password in drivers[i].config.password was silently
	// ignored by the hot-reload diff — the driver kept running with the
	// stale credentials. DeepEqual also treats nil and empty maps as equal.
	if len(a.Config) == 0 && len(b.Config) == 0 {
		return true
	}
	return reflect.DeepEqual(a.Config, b.Config)
}
