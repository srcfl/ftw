// Lua driver host.
//
// Drivers are plain .lua files that implement the driver lifecycle:
//
//	driver_init()          — subscribe MQTT topics, read Modbus SN, etc.
//	driver_poll()          — called every N seconds; emit telemetry
//	driver_command(c)      — receive a control command (JSON table)
//	driver_cleanup()       — optional, called on shutdown
//	driver_default_mode()  — optional, called when driver goes offline
//
// The host exposes a capability-gated API surfaced as a `host` global in
// the Lua VM:
//
//	host.log(level, msg)            -- level: "debug"|"info"|"warn"|"error"
//	host.emit(type, table)          -- type: "meter"|"pv"|"battery"|"ev"|"v2x_charger"
//	host.millis()                   -- ms since driver start
//	host.sleep(ms)                  -- block driver goroutine for ms (inter-write pacing)
//	host.set_poll_interval(ms)
//	host.set_sn(s)                  -- device serial (metadata)
//	host.set_make(s)                -- manufacturer name
//	host.mqtt_sub(topic)            -- subscribe
//	host.mqtt_pub(topic, payload)   -- publish
//	host.mqtt_messages()            -- array of {topic, payload} since last call
//	host.modbus_read(addr, count, kind)  -- kind: "coil"|"discrete"|"holding"|"input"
//	host.modbus_write(addr, value)
//	host.modbus_write_multi(addr, values)
//	host.json_decode(s)             -- convenience JSON → Lua table
//	host.json_encode(t)             -- Lua table → JSON string
//	host.http_get(url, headers)     -- HTTP GET, returns (body, nil) or (nil, err)
//	host.http_post(url, body, headers) -- HTTP POST, returns (body, nil) or (nil, err)
//	host.ws_open(url, headers)      -- open WebSocket; (true, nil) or (nil, err)
//	host.ws_send(text)              -- send one text frame; (true, nil) or (nil, err)
//	host.ws_messages()              -- drain inbound frames; "" entry = EOF
//	host.ws_is_open()               -- boolean
//	host.ws_close()                 -- close + free
//	host.tcp_open(addr)             -- open raw TCP socket "host:port"; (true, nil) or (nil, err)
//	host.tcp_recv()                 -- drain inbound bytes as a Lua string ("" if nothing)
//	host.tcp_is_open()              -- boolean
//	host.tcp_close()                -- close + free
//
// Lua 5.1 via yuin/gopher-lua — pure Go, zero CGo, one allocation-aware
// interpreter per driver.
package drivers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	net_http "net/http"
	net_url "net/url"
	"os"
	"strings"
	"sync"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// LuaDriver wraps a running Lua VM bound to a HostEnv.
type LuaDriver struct {
	Env  *HostEnv
	Path string

	mu sync.Mutex
	L  *lua.LState
}

// NewLuaDriver loads the file at path and runs it in a fresh Lua VM.
// The driver's top-level is executed once so `driver_init` etc. become
// callable globals. Returns an error if the file fails to load/execute.
func NewLuaDriver(path string, env *HostEnv) (*LuaDriver, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	L := lua.NewState(lua.Options{SkipOpenLibs: false})
	d := &LuaDriver{Env: env, Path: path, L: L}
	registerHost(L, env)
	if err := L.DoString(string(src)); err != nil {
		L.Close()
		return nil, fmt.Errorf("execute %s: %w", path, err)
	}
	return d, nil
}

// Init calls driver_init(config) if defined. config is the optional
// driver-specific JSON-shaped table the operator can put in the
// driver's YAML block.
func (d *LuaDriver) Init(ctx context.Context, config map[string]any) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	fn := d.L.GetGlobal("driver_init")
	if fn == lua.LNil {
		return nil
	}
	var arg lua.LValue = lua.LNil
	if config != nil {
		arg = goToLua(d.L, config)
	}
	return d.L.CallByParam(lua.P{Fn: fn, NRet: 0, Protect: true}, arg)
}

// Poll calls driver_poll() and returns the requested next-poll
// interval (0 if unspecified → host uses its default).
func (d *LuaDriver) Poll(ctx context.Context) (time.Duration, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fn := d.L.GetGlobal("driver_poll")
	if fn == lua.LNil {
		return 0, nil
	}
	if err := d.L.CallByParam(lua.P{Fn: fn, NRet: 1, Protect: true}); err != nil {
		return 0, err
	}
	ret := d.L.Get(-1)
	d.L.Pop(1)
	// Driver may return an int number of milliseconds.
	if n, ok := ret.(lua.LNumber); ok && n > 0 {
		return time.Duration(n) * time.Millisecond, nil
	}
	return 0, nil
}

// Command sends a command to the driver. Matches the existing Lua
// driver signature:  driver_command(action_string, power_w, full_cmd)
// where full_cmd is the original decoded table (for drivers that want
// extra fields).
func (d *LuaDriver) Command(ctx context.Context, cmdJSON []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	fn := d.L.GetGlobal("driver_command")
	if fn == lua.LNil {
		return nil
	}
	var cmd map[string]any
	if err := json.Unmarshal(cmdJSON, &cmd); err != nil {
		return err
	}
	action, _ := cmd["action"].(string)
	power, ok := cmd["power_w"].(float64)
	if !ok {
		power, _ = cmd["w"].(float64)
	}
	t := goToLua(d.L, cmd)
	if err := d.L.CallByParam(lua.P{Fn: fn, NRet: 1, Protect: true},
		lua.LString(action), lua.LNumber(power), t); err != nil {
		return err
	}
	ret := d.L.Get(-1)
	d.L.Pop(1)
	return luaReturnError("driver_command", ret)
}

// Cleanup calls driver_cleanup() and closes the VM.
func (d *LuaDriver) Cleanup() {
	_ = d.call("driver_cleanup")
	d.mu.Lock()
	d.L.Close()
	d.mu.Unlock()
}

// DefaultMode calls driver_default_mode() — typically tells the device
// to revert to autonomous self-consumption when the EMS is offline.
func (d *LuaDriver) DefaultMode() error {
	return d.call("driver_default_mode")
}

// call is a convenience for parameter-less void-returning lifecycle funcs.
func (d *LuaDriver) call(name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	fn := d.L.GetGlobal(name)
	if fn == lua.LNil {
		return nil
	}
	if err := d.L.CallByParam(lua.P{Fn: fn, NRet: 1, Protect: true}); err != nil {
		return err
	}
	ret := d.L.Get(-1)
	d.L.Pop(1)
	return luaReturnError(name, ret)
}

func luaReturnError(name string, ret lua.LValue) error {
	switch v := ret.(type) {
	case lua.LBool:
		if !bool(v) {
			return fmt.Errorf("%s returned false", name)
		}
	case lua.LString:
		if s := string(v); s != "" {
			return fmt.Errorf("%s: %s", name, s)
		}
	}
	return nil
}

// ---- host.* API exposed to Lua ----

func registerHost(L *lua.LState, env *HostEnv) {
	host := L.NewTable()

	host.RawSetString("log", L.NewFunction(func(L *lua.LState) int {
		level := L.CheckString(1)
		msg := L.CheckString(2)
		lvl := int32(1) // info
		switch level {
		case "debug":
			lvl = 0
		case "warn":
			lvl = 2
		case "error":
			lvl = 3
		}
		env.log(lvl, msg)
		return 0
	}))

	// host.emit("meter"|"pv"|"battery"|"ev"|"v2x_charger"|"vehicle", { w=…, soc=…, … })
	// The type string is prepended to the table as a `type` field and
	// the whole thing is serialized as JSON before hitting the telemetry store.
	// Allowed fields per type:
	//   meter   -> w, l1_w, l2_w, l3_w, l1_v, l2_v, l3_v, l1_a, l2_a, l3_a, freq_hz
	//   pv      -> w, mppt1_v, mppt1_a, mppt2_v, mppt2_a, dc_v
	//   battery -> w, soc, dc_v, dc_a, temp_c
	//   ev      -> w (charge power, positive when charging),
	//              connected (bool, plug inserted),
	//              charging (bool, current flowing),
	//              session_wh (optional, kWh for current session * 1000),
	//              max_a (optional, charger current limit),
	//              phases (optional, 1 or 3)
	//   v2x_charger -> w (positive = vehicle charging, negative = V2X discharge),
	//              vehicle_soc (0..1 fraction), connected,
	//              dc_w, dc_v, dc_a, session_charge_wh, session_discharge_wh,
	//              rated_power_w, status, control_mode
	//   vehicle -> soc (required, vehicle battery level % 0-100),
	//              charge_limit_pct (optional, vehicle-configured limit),
	//              charging_state (optional, e.g. "Charging"|"Stopped"|"Complete"),
	//              time_to_full_min (optional),
	//              stale (optional bool, true when data hasn't refreshed
	//              since stale_after_s — UI should de-emphasize).
	//              w is unused for vehicle readings (charger's DerEV owns
	//              the power number).
	host.RawSetString("emit", L.NewFunction(func(L *lua.LState) int {
		typ := L.CheckString(1)
		tbl := L.CheckTable(2)
		m, ok := luaToGo(tbl).(map[string]any)
		if !ok {
			m = map[string]any{}
		}
		m["type"] = typ
		blob, err := json.Marshal(m)
		if err != nil {
			L.Push(lua.LString("encode failed: " + err.Error()))
			return 1
		}
		if err := env.emitTelemetry(blob); err != nil {
			L.Push(lua.LString(err.Error()))
			return 1
		}
		return 0
	}))

	// host.emit_metric("battery_temp_c", 23.5) — record an arbitrary
	// scalar diagnostic into the long-format TS DB. Use for anything that
	// doesn't fit the structured pv/battery/meter shape: temperatures, DC
	// voltages, MPPT currents, grid frequency, inverter heat-sink, etc.
	// The metric name is the column name in the time-series — pick a stable
	// snake_case identifier with the unit as a suffix.
	host.RawSetString("emit_metric", L.NewFunction(func(L *lua.LState) int {
		name := L.CheckString(1)
		val := float64(L.CheckNumber(2))
		if err := env.emitMetric(name, val); err != nil {
			L.Push(lua.LString(err.Error()))
			return 1
		}
		return 0
	}))

	host.RawSetString("millis", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LNumber(env.millis()))
		return 1
	}))

	// host.sleep(ms) — block the driver goroutine for ms milliseconds.
	// Used for vendor-required inter-write pacing (Solis 100ms, Deye 50ms);
	// safe because each driver has its own goroutine and VM lock.
	host.RawSetString("sleep", L.NewFunction(func(L *lua.LState) int {
		ms := L.CheckInt(1)
		if ms > 0 {
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
		return 0
	}))

	host.RawSetString("set_poll_interval", L.NewFunction(func(L *lua.LState) int {
		ms := L.CheckInt(1)
		env.setPollInterval(int32(ms))
		return 0
	}))

	// host.set_watchdog_timeout_s(seconds) — install a per-driver
	// override so the site watchdog flags this driver stale only after
	// `seconds` since the last successful emit. Used by drivers whose
	// natural poll cadence is too slow for the site-wide 60 s default
	// (Tesla BLE proxy, cloud EV APIs). Calling with 0 clears the
	// override and reverts to the default.
	host.RawSetString("set_watchdog_timeout_s", L.NewFunction(func(L *lua.LState) int {
		secs := L.CheckInt(1)
		if env.Telemetry != nil {
			env.Telemetry.SetDriverWatchdogTimeout(env.DriverName, time.Duration(secs)*time.Second)
		}
		return 0
	}))

	host.RawSetString("set_sn", L.NewFunction(func(L *lua.LState) int {
		env.setSN(L.CheckString(1))
		return 0
	}))

	host.RawSetString("set_make", L.NewFunction(func(L *lua.LState) int {
		env.setMake(L.CheckString(1))
		return 0
	}))

	mqttSubscribe := L.NewFunction(func(L *lua.LState) int {
		topic := L.CheckString(1)
		if env.MQTT == nil {
			L.Push(lua.LString("no mqtt capability"))
			return 1
		}
		if err := env.MQTT.Subscribe(topic); err != nil {
			L.Push(lua.LString(err.Error()))
			return 1
		}
		return 0
	})
	host.RawSetString("mqtt_subscribe", mqttSubscribe)
	host.RawSetString("mqtt_sub", mqttSubscribe) // alias

	mqttPublish := L.NewFunction(func(L *lua.LState) int {
		topic := L.CheckString(1)
		payload := L.CheckString(2)
		if env.MQTT == nil {
			L.Push(lua.LString("no mqtt capability"))
			return 1
		}
		if err := env.MQTT.Publish(topic, []byte(payload)); err != nil {
			L.Push(lua.LString(err.Error()))
			return 1
		}
		return 0
	})
	host.RawSetString("mqtt_publish", mqttPublish)
	host.RawSetString("mqtt_pub", mqttPublish) // alias

	host.RawSetString("mqtt_messages", L.NewFunction(func(L *lua.LState) int {
		if env.MQTT == nil {
			L.Push(L.NewTable())
			return 1
		}
		msgs := env.MQTT.PopMessages()
		t := L.NewTable()
		for i, m := range msgs {
			row := L.NewTable()
			row.RawSetString("topic", lua.LString(m.Topic))
			row.RawSetString("payload", lua.LString(m.Payload))
			t.RawSetInt(i+1, row)
		}
		L.Push(t)
		return 1
	}))

	host.RawSetString("modbus_read", L.NewFunction(func(L *lua.LState) int {
		addr := L.CheckInt(1)
		count := L.CheckInt(2)
		kindS := L.CheckString(3)
		if env.Modbus == nil {
			L.Push(lua.LNil)
			L.Push(lua.LString("no modbus capability"))
			return 2
		}
		kind, ok := modbusKindFromString(kindS)
		if !ok {
			L.Push(lua.LNil)
			L.Push(lua.LString("unknown modbus kind: " + kindS))
			return 2
		}
		regs, err := env.Modbus.Read(uint16(addr), uint16(count), kind)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		t := L.NewTable()
		for i, r := range regs {
			t.RawSetInt(i+1, lua.LNumber(r))
		}
		L.Push(t)
		return 1
	}))

	host.RawSetString("modbus_write", L.NewFunction(func(L *lua.LState) int {
		addr := L.CheckInt(1)
		val := L.CheckInt(2)
		if env.Modbus == nil {
			L.Push(lua.LString("no modbus capability"))
			return 1
		}
		if err := env.Modbus.WriteSingle(uint16(addr), uint16(val)); err != nil {
			L.Push(lua.LString(err.Error()))
			return 1
		}
		return 0
	}))

	host.RawSetString("modbus_write_multi", L.NewFunction(func(L *lua.LState) int {
		addr := L.CheckInt(1)
		t := L.CheckTable(2)
		if env.Modbus == nil {
			L.Push(lua.LString("no modbus capability"))
			return 1
		}
		vals := make([]uint16, 0, t.Len())
		t.ForEach(func(_ lua.LValue, v lua.LValue) {
			if n, ok := v.(lua.LNumber); ok {
				vals = append(vals, uint16(n))
			}
		})
		if err := env.Modbus.WriteMulti(uint16(addr), vals); err != nil {
			L.Push(lua.LString(err.Error()))
			return 1
		}
		return 0
	}))

	// Decode helpers for Modbus registers. Drivers read raw u16[] and
	// need to combine pairs back into u32/i32. LE = little-endian
	// (Sungrow's habit: lo-word first, then hi-word).
	host.RawSetString("decode_u32_le", L.NewFunction(func(L *lua.LState) int {
		lo := uint32(L.CheckInt(1)) & 0xFFFF
		hi := uint32(L.CheckInt(2)) & 0xFFFF
		L.Push(lua.LNumber(lo | (hi << 16)))
		return 1
	}))
	host.RawSetString("decode_u32_be", L.NewFunction(func(L *lua.LState) int {
		hi := uint32(L.CheckInt(1)) & 0xFFFF
		lo := uint32(L.CheckInt(2)) & 0xFFFF
		L.Push(lua.LNumber(lo | (hi << 16)))
		return 1
	}))
	host.RawSetString("decode_i32_le", L.NewFunction(func(L *lua.LState) int {
		lo := uint32(L.CheckInt(1)) & 0xFFFF
		hi := uint32(L.CheckInt(2)) & 0xFFFF
		L.Push(lua.LNumber(int32(lo | (hi << 16))))
		return 1
	}))
	host.RawSetString("decode_i32_be", L.NewFunction(func(L *lua.LState) int {
		hi := uint32(L.CheckInt(1)) & 0xFFFF
		lo := uint32(L.CheckInt(2)) & 0xFFFF
		L.Push(lua.LNumber(int32(lo | (hi << 16))))
		return 1
	}))
	host.RawSetString("decode_i16", L.NewFunction(func(L *lua.LState) int {
		v := int16(L.CheckInt(1))
		L.Push(lua.LNumber(v))
		return 1
	}))

	host.RawSetString("json_decode", L.NewFunction(func(L *lua.LState) int {
		s := L.CheckString(1)
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		L.Push(goToLua(L, v))
		return 1
	}))

	host.RawSetString("json_encode", L.NewFunction(func(L *lua.LState) int {
		v := L.Get(1)
		b, err := json.Marshal(luaToGo(v))
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		L.Push(lua.LString(string(b)))
		return 1
	}))

	// ---- HTTP capability ----
	// host.http_get(url, headers?) → (body, nil) or (nil, error_string)
	// host.http_post(url, body, headers?) → (body, nil) or (nil, error_string)
	// headers is an optional Lua table {["Content-Type"]="application/json", ...}
	// hostAllowed checks the URL's host component against the
	// per-driver allowlist. Empty allowlist = any host (legacy
	// behaviour). Matched case-insensitively.
	//
	// Allowlist entry semantics:
	//   "192.168.1.50"        → host-only match (any port allowed —
	//                           backward-compatible default)
	//   "192.168.1.50:8080"   → host AND port must match
	//
	// The port-aware form lets operators tighten an allowlist so a
	// driver granted access to a single proxy can't probe other ports
	// on the same host (e.g. SSH/22, Redis/6379, internal admin UIs).
	// Existing configs that don't specify a port keep working.
	//
	// Schemes other than http/https are rejected outright — file://,
	// data://, ftp:// etc. have no business here and would otherwise
	// produce opaque "unsupported protocol scheme" errors from the
	// stdlib client.
	hostAllowed := func(rawURL string) (bool, string) {
		u, err := net_url.Parse(rawURL)
		if err != nil || u.Host == "" {
			return false, "invalid URL"
		}
		switch strings.ToLower(u.Scheme) {
		case "http", "https":
		default:
			return false, fmt.Sprintf("scheme %q not supported (http/https only)", u.Scheme)
		}
		if len(env.HTTPAllowedHosts) == 0 {
			return true, ""
		}
		host := strings.ToLower(u.Hostname())
		port := u.Port()
		if port == "" {
			if u.Scheme == "https" {
				port = "443"
			} else {
				port = "80"
			}
		}
		for _, raw := range env.HTTPAllowedHosts {
			entry := strings.ToLower(strings.TrimSpace(raw))
			if entry == "" {
				continue
			}
			eHost, ePort, hasPort := splitHostPortLower(entry)
			if !hasPort {
				if entry == host {
					return true, ""
				}
				continue
			}
			if eHost == host && ePort == port {
				return true, ""
			}
		}
		return false, fmt.Sprintf("host %q (port %s) not in allowed_hosts", host, port)
	}

	httpClient := &net_http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(req *net_http.Request, via []*net_http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			if ok, reason := hostAllowed(req.URL.String()); !ok {
				return fmt.Errorf("redirect blocked: %s", reason)
			}
			return nil
		},
	}

	applyHeaders := func(req *net_http.Request, L *lua.LState, argIdx int) {
		tbl := L.OptTable(argIdx, nil)
		if tbl == nil {
			return
		}
		tbl.ForEach(func(k, v lua.LValue) {
			if ks, ok := k.(lua.LString); ok {
				req.Header.Set(string(ks), v.String())
			}
		})
	}

	host.RawSetString("http_get", L.NewFunction(func(L *lua.LState) int {
		if !env.HTTP {
			L.Push(lua.LNil)
			L.Push(lua.LString("http: capability not granted"))
			return 2
		}
		url := L.CheckString(1)
		if ok, reason := hostAllowed(url); !ok {
			L.Push(lua.LNil)
			L.Push(lua.LString("http: " + reason))
			return 2
		}
		req, err := net_http.NewRequest("GET", url, nil)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		applyHeaders(req, L, 2)
		resp, err := httpClient.Do(req)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB cap
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		if resp.StatusCode >= 400 {
			L.Push(lua.LNil)
			L.Push(lua.LString(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body))))
			return 2
		}
		L.Push(lua.LString(string(body)))
		return 1
	}))

	host.RawSetString("http_post", L.NewFunction(func(L *lua.LState) int {
		if !env.HTTP {
			L.Push(lua.LNil)
			L.Push(lua.LString("http: capability not granted"))
			return 2
		}
		url := L.CheckString(1)
		if ok, reason := hostAllowed(url); !ok {
			L.Push(lua.LNil)
			L.Push(lua.LString("http: " + reason))
			return 2
		}
		payload := L.CheckString(2)
		req, err := net_http.NewRequest("POST", url, strings.NewReader(payload))
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		req.Header.Set("Content-Type", "application/json")
		applyHeaders(req, L, 3)
		resp, err := httpClient.Do(req)
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		if resp.StatusCode >= 400 {
			L.Push(lua.LNil)
			L.Push(lua.LString(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body))))
			return 2
		}
		L.Push(lua.LString(string(body)))
		return 1
	}))

	// ---- WebSocket capability ----
	// host.ws_open(url, headers?)      → (true, nil) or (nil, error_string)
	// host.ws_send(text)               → (true, nil) or (nil, error_string)
	// host.ws_messages()               → table of inbound text frames (oldest first).
	//                                    Drained on each call; empty table when idle.
	//                                    An empty-string entry "" is the EOF sentinel:
	//                                    the read pump exited and the driver should
	//                                    ws_close + ws_open again on the next tick.
	// host.ws_is_open()                → boolean
	// host.ws_close()                  → nil
	host.RawSetString("ws_open", L.NewFunction(func(L *lua.LState) int {
		if env.WS == nil {
			L.Push(lua.LNil)
			L.Push(lua.LString("ws: capability not granted"))
			return 2
		}
		url := L.CheckString(1)
		if ok, reason := wsHostAllowed(url, env.WSAllowedHosts); !ok {
			L.Push(lua.LNil)
			L.Push(lua.LString("ws: " + reason))
			return 2
		}
		// Optional headers table {"Header-Name"="value", ...}
		headers := map[string]string{}
		if tbl := L.OptTable(2, nil); tbl != nil {
			tbl.ForEach(func(k, v lua.LValue) {
				if ks, ok := k.(lua.LString); ok {
					headers[string(ks)] = v.String()
				}
			})
		}
		if err := env.WS.Open(url, headers); err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		L.Push(lua.LBool(true))
		return 1
	}))

	host.RawSetString("ws_send", L.NewFunction(func(L *lua.LState) int {
		if env.WS == nil {
			L.Push(lua.LNil)
			L.Push(lua.LString("ws: capability not granted"))
			return 2
		}
		text := L.CheckString(1)
		if err := env.WS.Send(text); err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		L.Push(lua.LBool(true))
		return 1
	}))

	host.RawSetString("ws_messages", L.NewFunction(func(L *lua.LState) int {
		if env.WS == nil {
			// Match mqtt_messages contract: return an empty table rather
			// than nil so drivers can iterate without a nil check.
			L.Push(L.NewTable())
			return 1
		}
		msgs := env.WS.PopMessages()
		tbl := L.NewTable()
		for i, m := range msgs {
			tbl.RawSetInt(i+1, lua.LString(m))
		}
		L.Push(tbl)
		return 1
	}))

	host.RawSetString("ws_is_open", L.NewFunction(func(L *lua.LState) int {
		if env.WS == nil {
			L.Push(lua.LBool(false))
			return 1
		}
		L.Push(lua.LBool(env.WS.IsOpen()))
		return 1
	}))

	host.RawSetString("ws_close", L.NewFunction(func(L *lua.LState) int {
		if env.WS == nil {
			return 0
		}
		_ = env.WS.Close()
		return 0
	}))

	// host.tcp_open("host:port")      → (true, nil) or (nil, error_string)
	// host.tcp_recv()                 → string of buffered bytes since last
	//                                   call ("" when nothing arrived). The
	//                                   driver does its own framing (P1
	//                                   telegrams use ! as the end-of-frame
	//                                   marker, for instance).
	// host.tcp_is_open()              → boolean — read pump alive. Flips to
	//                                   false on EOF / read error; the
	//                                   driver re-opens on the next poll.
	// host.tcp_close()                → nil
	host.RawSetString("tcp_open", L.NewFunction(func(L *lua.LState) int {
		if env.TCP == nil {
			L.Push(lua.LNil)
			L.Push(lua.LString("tcp: capability not granted"))
			return 2
		}
		addr := L.CheckString(1)
		if err := env.TCP.Open(addr); err != nil {
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))
			return 2
		}
		L.Push(lua.LBool(true))
		return 1
	}))

	host.RawSetString("tcp_recv", L.NewFunction(func(L *lua.LState) int {
		if env.TCP == nil {
			// Mirror ws_messages / mqtt_messages contract: return an empty
			// value rather than nil so drivers can concatenate without a
			// nil guard.
			L.Push(lua.LString(""))
			return 1
		}
		b := env.TCP.PopBytes()
		L.Push(lua.LString(string(b)))
		return 1
	}))

	host.RawSetString("tcp_is_open", L.NewFunction(func(L *lua.LState) int {
		if env.TCP == nil {
			L.Push(lua.LBool(false))
			return 1
		}
		L.Push(lua.LBool(env.TCP.IsOpen()))
		return 1
	}))

	host.RawSetString("tcp_close", L.NewFunction(func(L *lua.LState) int {
		if env.TCP == nil {
			return 0
		}
		_ = env.TCP.Close()
		return 0
	}))

	L.SetGlobal("host", host)
}

// splitHostPortLower parses an allowlist entry as "host" or "host:port".
// Returns lowercased host, port string, and a flag indicating whether a
// port was present. Bracketed IPv6 ([::1]:8080) is supported via the
// explicit "]" boundary; unbracketed IPv6 ("fe80::1") is treated as
// host-only because the trailing ":1" would otherwise be misread as
// port 1.
func splitHostPortLower(s string) (host, port string, hasPort bool) {
	s = strings.ToLower(s)
	if strings.HasPrefix(s, "[") {
		end := strings.Index(s, "]")
		if end < 0 {
			return s, "", false
		}
		h := s[1:end]
		rest := s[end+1:]
		if strings.HasPrefix(rest, ":") && len(rest) > 1 {
			return h, rest[1:], true
		}
		return h, "", false
	}
	// Two or more colons → unbracketed IPv6, treat as host-only.
	if strings.Count(s, ":") >= 2 {
		return s, "", false
	}
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return s, "", false
	}
	maybePort := s[i+1:]
	if maybePort == "" {
		return s, "", false
	}
	for _, r := range maybePort {
		if r < '0' || r > '9' {
			return s, "", false
		}
	}
	return s[:i], maybePort, true
}

func modbusKindFromString(s string) (int32, bool) {
	switch s {
	case "coil":
		return 0, true
	case "discrete":
		return 1, true
	case "holding":
		return 2, true
	case "input":
		return 3, true
	}
	return 0, false
}

// goToLua / luaToGo — minimal JSON-shaped bridge (string, number, bool,
// array, object, nil). Not for arbitrary Go types.

func goToLua(L *lua.LState, v any) lua.LValue {
	switch x := v.(type) {
	case nil:
		return lua.LNil
	case bool:
		return lua.LBool(x)
	case float64:
		return lua.LNumber(x)
	case int:
		return lua.LNumber(x)
	case int64:
		return lua.LNumber(x)
	case string:
		return lua.LString(x)
	case []any:
		t := L.NewTable()
		for i, e := range x {
			t.RawSetInt(i+1, goToLua(L, e))
		}
		return t
	case map[string]any:
		t := L.NewTable()
		for k, e := range x {
			t.RawSetString(k, goToLua(L, e))
		}
		return t
	}
	return lua.LNil
}

func luaToGo(v lua.LValue) any {
	switch x := v.(type) {
	case lua.LBool:
		return bool(x)
	case lua.LNumber:
		return float64(x)
	case lua.LString:
		return string(x)
	case *lua.LTable:
		// Detect array vs object by checking numeric-sequential keys.
		n := x.Len()
		isArray := true
		x.ForEach(func(k, _ lua.LValue) {
			if kn, ok := k.(lua.LNumber); !ok || float64(kn) != float64(int(kn)) {
				isArray = false
			}
		})
		if isArray && n > 0 {
			arr := make([]any, 0, n)
			for i := 1; i <= n; i++ {
				arr = append(arr, luaToGo(x.RawGetInt(i)))
			}
			return arr
		}
		obj := map[string]any{}
		x.ForEach(func(k, v lua.LValue) {
			obj[k.String()] = luaToGo(v)
		})
		return obj
	}
	return nil
}
