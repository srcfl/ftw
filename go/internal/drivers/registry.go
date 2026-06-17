package drivers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// Registry manages running Lua driver instances — spawn, poll, command, stop.
// Thread-safe.
type Registry struct {
	tel *telemetry.Store

	// MQTTFactory creates an MQTT capability for a driver given its config.
	// Called on Add; the returned MQTTCap belongs to that driver alone.
	MQTTFactory func(name string, c *config.MQTTConfig) (MQTTCap, error)
	// ModbusFactory creates a Modbus capability.
	ModbusFactory func(name string, c *config.ModbusConfig) (ModbusCap, error)
	// MatterFactory creates a Matter capability backed by the Matter sidecar.
	MatterFactory func(name string, c *config.MatterConfig) (MatterCap, error)
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

type runningDriver struct {
	driver driverRuntime
	env    *HostEnv
	cfg    config.Driver
	// Poll loop coordination
	cmdCh chan driverCmd
	stop  chan struct{}
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
			if mac, ok := r.ARPLookup(mq.Host); ok { env.SetMAC(mac) }
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
			if mac, ok := r.ARPLookup(mb.Host); ok { env.SetMAC(mac) }
		}
	}
	if cfg.Capabilities.HTTP != nil {
		env.WithHTTP()
		if hosts := cfg.Capabilities.HTTP.AllowedHosts; len(hosts) > 0 {
			env.WithHTTPAllowedHosts(hosts)
		}
	}
	if mt := cfg.Capabilities.Matter; mt != nil && r.MatterFactory != nil {
		cap, err := r.MatterFactory(cfg.Name, mt)
		if err != nil {
			return fmt.Errorf("matter capability: %w", err)
		}
		env.WithMatter(cap)
		p := mt.Port
		if p == 0 { p = 5580 }
		env.SetEndpoint(fmt.Sprintf("matter://%s:%d", mt.Host, p))
	}

	luaDrv, err := NewLuaDriver(cfg.Lua, env)
	if err != nil {
		return fmt.Errorf("load lua: %w", err)
	}
	var drv driverRuntime = &luaRuntime{LuaDriver: luaDrv}

	// Pass the driver's config map as JSON to driver_init.
	var initCfg []byte
	if cfg.Config != nil {
		initCfg, _ = json.Marshal(cfg.Config)
	}
	if err := drv.Init(ctx, initCfg); err != nil {
		drv.Cleanup(ctx)
		return fmt.Errorf("driver_init: %w", err)
	}

	rd := &runningDriver{
		driver: drv,
		env:    env,
		cfg:    cfg,
		cmdCh:  make(chan driverCmd, 8),
		stop:   make(chan struct{}),
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
		r.tel.DriverHealthMut(cfg.Name)
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
		case <-rd.stop:
			_ = rd.driver.DefaultMode(ctx)
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
			if rd.env.Matter != nil {
				_ = rd.env.Matter.Close()
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
				r.tel.DriverHealthMut(rd.cfg.Name).RecordError(err.Error())
			} else if r.tel != nil {
				// Record the successful tick so driver_poll bumps the
				// health record's TickCount even when the driver itself
				// has nothing to emit yet (e.g. ferroamp between MQTT
				// subscribe and the first inbound message). Without
				// this, drivers that wait on slow telemetry topics
				// are indistinguishable from ones that crashed.
				r.tel.DriverHealthMut(rd.cfg.Name).RecordSuccess()
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
	r.mu.Lock()
	rd, ok := r.rec[name]
	if !ok {
		r.mu.Unlock()
		return
	}
	delete(r.rec, name)
	r.mu.Unlock()
	close(rd.stop)
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

// SendDefault sends the default/watchdog command to a driver.
func (r *Registry) SendDefault(ctx context.Context, name string) error {
	r.mu.Lock()
	rd, ok := r.rec[name]
	r.mu.Unlock()
	if !ok { return fmt.Errorf("driver %q not found", name) }
	resCh := make(chan error, 1)
	rd.cmdCh <- driverCmd{kind: "default", result: resCh}
	select {
	case err := <-resCh: return err
	case <-ctx.Done(): return ctx.Err()
	}
}

// Names returns the currently registered driver names.
// Env returns the HostEnv for a driver, or nil if not registered.
// Used by main to read identity (make/sn/mac/endpoint) after init.
func (r *Registry) Env(name string) *HostEnv {
	r.mu.Lock()
	rd, ok := r.rec[name]
	r.mu.Unlock()
	if !ok { return nil }
	return rd.env
}

func (r *Registry) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.rec))
	for n := range r.rec { out = append(out, n) }
	return out
}

// ShutdownAll stops every driver. Blocks until all poll loops exit.
func (r *Registry) ShutdownAll() {
	r.mu.Lock()
	names := make([]string, 0, len(r.rec))
	for n := range r.rec { names = append(names, n) }
	r.mu.Unlock()
	for _, n := range names {
		r.Remove(n)
	}
}

// Reload diffs a new driver list against running state and applies add/
// remove/restart. Drivers with changed lua path, capabilities, or config
// map are restarted. Drivers marked Disabled are treated as "not in the
// new list" — running ones get stopped, missing ones are not added.
func (r *Registry) Reload(ctx context.Context, newDrivers []config.Driver) {
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
	oldNames := make(map[string]bool, len(r.rec))
	oldCfgs := make(map[string]config.Driver, len(r.rec))
	for n, rd := range r.rec {
		oldNames[n] = true
		oldCfgs[n] = rd.cfg
	}
	r.mu.Unlock()

	newNames := make(map[string]bool, len(active))
	for _, d := range active { newNames[d.Name] = true }

	// Remove or restart
	for n, old := range oldCfgs {
		newCfg, stillThere := findDriver(active, n)
		if !stillThere {
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
		if exists { continue }
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

func findDriver(list []config.Driver, name string) (config.Driver, bool) {
	for _, d := range list {
		if d.Name == name { return d, true }
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
	if (aMq == nil) != (bMq == nil) { return false }
	if aMq != nil && (aMq.Host != bMq.Host || aMq.Port != bMq.Port ||
		aMq.Username != bMq.Username || aMq.Password != bMq.Password) {
		return false
	}
	aMb, bMb := a.EffectiveModbus(), b.EffectiveModbus()
	if (aMb == nil) != (bMb == nil) { return false }
	if aMb != nil && (aMb.Host != bMb.Host || aMb.Port != bMb.Port || aMb.UnitID != bMb.UnitID) {
		return false
	}
	aMt, bMt := a.Capabilities.Matter, b.Capabilities.Matter
	if (aMt == nil) != (bMt == nil) { return false }
	if aMt != nil && (aMt.Host != bMt.Host || aMt.Port != bMt.Port) {
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
