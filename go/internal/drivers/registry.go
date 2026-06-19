package drivers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

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
	// ARPLookup resolves a hostname/IP to a MAC for L2-stable identity.
	// Optional — when nil, devices fall back to endpoint-hash IDs.
	ARPLookup func(host string) (mac string, ok bool)

	mu  sync.Mutex
	rec map[string]*runningDriver
}

// NewRegistry builds a driver registry.
func NewRegistry(tel *telemetry.Store) *Registry {
	return &Registry{
		tel: tel,
		rec: map[string]*runningDriver{},
	}
}

// SetTroubleshootingMode updates the global incident-diagnostics flag used for
// subsequently started drivers. Use Reload to restart already-running drivers.
func (r *Registry) SetTroubleshootingMode(enabled bool) {
	r.mu.Lock()
	r.troubleshootingMode = enabled
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
func (l *luaRuntime) DefaultMode(ctx context.Context) error { return l.LuaDriver.DefaultMode() }
func (l *luaRuntime) Cleanup(ctx context.Context) error     { l.LuaDriver.Cleanup(); return nil }
func (l *luaRuntime) Env() *HostEnv                         { return l.LuaDriver.Env }

func driverInitConfigJSON(cfg config.Driver, troubleshootingMode bool) []byte {
	if len(cfg.Config) == 0 && !troubleshootingMode {
		return nil
	}
	m := make(map[string]any, len(cfg.Config)+1)
	for k, v := range cfg.Config {
		m[k] = v
	}
	if troubleshootingMode {
		m["_troubleshooting_mode"] = true
	}
	b, _ := json.Marshal(m)
	return b
}

type runningDriver struct {
	driver driverRuntime
	env    *HostEnv
	cfg    config.Driver
	// Poll loop coordination
	cmdCh chan driverCmd
	stop  chan bool
	done  chan struct{}
}

type driverCmd struct {
	kind    string
	payload []byte
	result  chan error
}

// Add spawns a driver. Returns error if the driver config is invalid or
// the Lua script can't be loaded.
func (r *Registry) Add(ctx context.Context, cfg config.Driver) error {
	r.mu.Lock()
	if _, exists := r.rec[cfg.Name]; exists {
		r.mu.Unlock()
		return fmt.Errorf("driver %q already registered", cfg.Name)
	}
	r.mu.Unlock()

	if cfg.Lua == "" {
		return fmt.Errorf("driver %q: must specify `lua` path", cfg.Name)
	}

	env := NewHostEnv(cfg.Name, r.tel)
	env.BatteryCapacityWh = cfg.BatteryCapacityWh
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
	if cfg.Capabilities.HTTP != nil {
		env.WithHTTP()
		hosts := mergeAllowedHosts(cfg.Capabilities.HTTP.AllowedHosts, cfg.Config)
		if len(hosts) > 0 {
			env.WithHTTPAllowedHosts(hosts)
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

	luaDrv, err := NewLuaDriver(cfg.Lua, env)
	if err != nil {
		return fmt.Errorf("load lua: %w", err)
	}
	var drv driverRuntime = &luaRuntime{LuaDriver: luaDrv}

	r.mu.Lock()
	troubleshootingMode := r.troubleshootingMode
	r.mu.Unlock()

	// Pass the driver's config map as JSON to driver_init, with reserved
	// host-level keys injected only at runtime.
	initCfg := driverInitConfigJSON(cfg, troubleshootingMode)
	if err := drv.Init(ctx, initCfg); err != nil {
		drv.Cleanup(ctx)
		return fmt.Errorf("driver_init: %w", err)
	}

	rd := &runningDriver{
		driver: drv,
		env:    env,
		cfg:    cfg,
		cmdCh:  make(chan driverCmd, 8),
		stop:   make(chan bool, 1),
		done:   make(chan struct{}),
	}
	r.mu.Lock()
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
	ctx := context.Background()
	interval := rd.env.PollInterval()
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case skipDefault := <-rd.stop:
			if !skipDefault {
				_ = rd.driver.DefaultMode(ctx)
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
			if rd.env.WS != nil {
				_ = rd.env.WS.Close()
			}
			if rd.env.TCP != nil {
				_ = rd.env.TCP.Close()
			}
			return
		case cmd := <-rd.cmdCh:
			var err error
			switch cmd.kind {
			case "command":
				err = rd.driver.Command(ctx, cmd.payload)
			case "default":
				err = rd.driver.DefaultMode(ctx)
			}
			if cmd.result != nil {
				cmd.result <- err
			}
		case <-timer.C:
			if _, err := rd.driver.Poll(ctx); err != nil {
				slog.Warn("driver poll failed", "name", rd.cfg.Name, "err", err)
				r.tel.RecordDriverError(rd.cfg.Name, err.Error())
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
			// Re-arm timer at driver's requested interval
			interval = rd.env.PollInterval()
			timer.Reset(interval)
		}
	}
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

func (r *Registry) remove(name string, skipDefault bool) {
	r.mu.Lock()
	rd, ok := r.rec[name]
	if !ok {
		r.mu.Unlock()
		return
	}
	delete(r.rec, name)
	r.mu.Unlock()
	rd.stop <- skipDefault
	<-rd.done
	if r.tel != nil {
		r.tel.Remove(name)
	}
	slog.Info("driver removed", "name", name)
}

// Send dispatches a command JSON blob to a specific driver. Blocks until the
// driver's runLoop processes it or ctx expires.
func (r *Registry) Send(ctx context.Context, name string, payload []byte) error {
	r.mu.Lock()
	rd, ok := r.rec[name]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("driver %q not found", name)
	}
	resCh := make(chan error, 1)
	select {
	case rd.cmdCh <- driverCmd{kind: "command", payload: payload, result: resCh}:
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

// SendDefault sends the default/watchdog command to a driver. Symmetric
// with Send: both the channel-push and the result-wait honour ctx. A
// driver whose cmdCh is full (because its goroutine is slow / stuck mid
// I/O) would otherwise block the caller forever; the watchdog-fallback
// path runs on every dispatch tick, so an unblocked send into a wedged
// driver deadlocks the entire control loop.
func (r *Registry) SendDefault(ctx context.Context, name string) error {
	r.mu.Lock()
	rd, ok := r.rec[name]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("driver %q not found", name)
	}
	resCh := make(chan error, 1)
	select {
	case rd.cmdCh <- driverCmd{kind: "default", result: resCh}:
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
	oldNames := make(map[string]bool, len(r.rec))
	oldCfgs := make(map[string]config.Driver, len(r.rec))
	for n, rd := range r.rec {
		oldNames[n] = true
		oldCfgs[n] = rd.cfg
	}
	r.mu.Unlock()

	newNames := make(map[string]bool, len(active))
	for _, d := range active {
		newNames[d.Name] = true
	}

	// Remove or restart
	for n, old := range oldCfgs {
		newCfg, stillThere := findDriver(active, n)
		if !stillThere {
			r.Remove(n)
		} else if troubleshootingChanged {
			slog.Info("driver troubleshooting mode changed, restarting", "name", n, "enabled", troubleshootingMode)
			r.Remove(n)
		} else if !sameDriverConfig(old, newCfg) {
			slog.Info("driver config changed, restarting", "name", n)
			r.Remove(n)
		}
	}
	// Add new
	for _, d := range active {
		r.mu.Lock()
		_, exists := r.rec[d.Name]
		r.mu.Unlock()
		if exists {
			continue
		}
		if err := r.Add(ctx, d); err != nil {
			slog.Warn("add driver failed", "name", d.Name, "err", err)
		}
	}
}

// Restart stops a driver (if running) and re-adds it with the provided cfg.
// If cfg.Disabled is true, this is a no-op after the stop. Used by the API
// restart endpoint so the driver picks up fresh credentials / re-auths.
func (r *Registry) Restart(ctx context.Context, cfg config.Driver) error {
	r.Remove(cfg.Name)
	if cfg.Disabled {
		return nil
	}
	return r.Add(ctx, cfg)
}

// Restart a driver by name using whatever cfg it was last started with.
// Returns an error if the driver isn't running (use Restart with a cfg
// to spawn from scratch).
func (r *Registry) RestartByName(ctx context.Context, name string) error {
	r.mu.Lock()
	rd, ok := r.rec[name]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("driver %q not running", name)
	}
	cfg := rd.cfg
	return r.Restart(ctx, cfg)
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
		a.Disabled != b.Disabled {
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
