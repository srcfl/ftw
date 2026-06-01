package drivers

// Driver-level device fingerprinting.
//
// Given an open endpoint discovered by a network scan (e.g. "10.0.0.7:502
// is listening"), we want to ask every driver that speaks that protocol:
// "is this you?". Each driver answers by implementing an optional Lua
// lifecycle hook:
//
//	function driver_fingerprint(target)
//	    -- target = { host=, port=, protocol=, base_url= } describes the
//	    -- endpoint under test. Modbus drivers can ignore it (host.modbus_read
//	    -- already targets the wired device); HTTP drivers build their URL
//	    -- from target.base_url (e.g. base_url .. "/api/devices").
//	    -- talk to the device through host.modbus_read / host.http_get / …
//	    -- return true   → I positively recognise this device's signature
//	    -- return false  → I talked to it and it is NOT one of mine
//	    -- return nil     → I can't tell / I don't support fingerprinting
//	    --
//	    -- Optional second return: an identity hint table the host folds
//	    -- into the result so the UI can pre-fill make/model/serial:
//	    --   return true, { make="SolarEdge", model="SE7K", serial="…",
//	    --                  confidence=0.95 }
//	end
//
// The verdict is deliberately tri-state. A driver should only return
// `true` when it has read a discriminating signature (a SunSpec marker, a
// vendor device-type code, a known register that reads back a magic
// value). It should return `false` only when it got a clean response that
// it can affirmatively attribute to a *different* device. Anything
// else — a read error, an ambiguous value, a missing hook — is `unknown`,
// never a false positive.
//
// Fingerprinting NEVER runs driver_init or driver_cleanup: those can
// reconfigure the device (Sungrow rewrites power limits, SolarEdge clears
// curtail registers on cleanup). A fingerprint must be a passive probe.

import (
	"context"
	"fmt"

	lua "github.com/yuin/gopher-lua"
)

// MatchState is a driver's verdict about whether an endpoint is one of its
// own devices.
type MatchState string

const (
	// MatchYes — the driver read a discriminating signature and is
	// confident the device is one it supports.
	MatchYes MatchState = "match"
	// MatchNo — the driver got a clean response it can attribute to a
	// different device; this is positively not its hardware.
	MatchNo MatchState = "no_match"
	// MatchUnknown — inconclusive: read error, ambiguous value, or the
	// driver doesn't implement driver_fingerprint at all.
	MatchUnknown MatchState = "unknown"
)

// Fingerprint is the result of asking one driver to identify an endpoint.
// Driver/Name are filled in by the orchestrator (the catalog metadata);
// the Match + identity hints come from the driver's driver_fingerprint.
type Fingerprint struct {
	Driver     string     `json:"driver,omitempty"` // catalog filename, e.g. "solaredge.lua"
	Name       string     `json:"name,omitempty"`   // catalog display name
	Match      MatchState `json:"match"`
	Make       string     `json:"make,omitempty"`
	Model      string     `json:"model,omitempty"`
	Serial     string     `json:"serial,omitempty"`
	Confidence float64    `json:"confidence,omitempty"`
	// Err is set when driver_fingerprint raised a Lua error. The verdict
	// is forced to MatchUnknown in that case — an erroring probe must
	// never be reported as a match or a confident no-match.
	Err string `json:"error,omitempty"`
}

// FingerprintTarget describes the endpoint being probed. It is handed to
// the Lua driver_fingerprint hook as a table so protocol-agnostic drivers
// (HTTP) can build their request URL; Modbus drivers ignore it because
// their capability is already wired to the endpoint.
type FingerprintTarget struct {
	Host     string
	Port     int
	Protocol string
}

// luaTable renders the target as the Lua argument table passed to
// driver_fingerprint. For HTTP it precomputes a base_url convenience
// (scheme://host[:port], omitting the default port) so drivers don't
// reassemble it.
func (t FingerprintTarget) luaTable(L *lua.LState) *lua.LTable {
	tbl := L.NewTable()
	tbl.RawSetString("host", lua.LString(t.Host))
	tbl.RawSetString("port", lua.LNumber(t.Port))
	tbl.RawSetString("protocol", lua.LString(t.Protocol))
	if t.Protocol == "http" || t.Protocol == "https" {
		base := fmt.Sprintf("%s://%s", t.Protocol, t.Host)
		defaultPort := (t.Protocol == "http" && t.Port == 80) || (t.Protocol == "https" && t.Port == 443)
		if t.Port != 0 && !defaultPort {
			base = fmt.Sprintf("%s://%s:%d", t.Protocol, t.Host, t.Port)
		}
		tbl.RawSetString("base_url", lua.LString(base))
	}
	return tbl
}

// Fingerprint calls the driver's optional driver_fingerprint(target) hook
// and maps its return into a Fingerprint verdict. A missing hook yields
// MatchUnknown with no error. A Lua error yields MatchUnknown with Err set
// (and a non-nil returned error so callers that care can log it).
func (d *LuaDriver) Fingerprint(ctx context.Context, target FingerprintTarget) (Fingerprint, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	fn := d.L.GetGlobal("driver_fingerprint")
	if fn == lua.LNil {
		return Fingerprint{Match: MatchUnknown}, nil
	}
	if err := d.L.CallByParam(lua.P{Fn: fn, NRet: 2, Protect: true}, target.luaTable(d.L)); err != nil {
		return Fingerprint{Match: MatchUnknown, Err: err.Error()}, err
	}
	// CallByParam normalises the stack to exactly NRet values, padding
	// with nil — so Get(-2)/Get(-1) are always safe here.
	hint := d.L.Get(-1)
	verdict := d.L.Get(-2)
	d.L.Pop(2)

	fp := Fingerprint{Match: MatchUnknown}
	switch v := verdict.(type) {
	case lua.LBool:
		if bool(v) {
			fp.Match = MatchYes
		} else {
			fp.Match = MatchNo
		}
	case *lua.LNilType:
		fp.Match = MatchUnknown
	}

	if tbl, ok := hint.(*lua.LTable); ok {
		fp.Make = luaFieldString(tbl, "make")
		fp.Model = luaFieldString(tbl, "model")
		fp.Serial = luaFieldString(tbl, "serial")
		if n, ok := tbl.RawGetString("confidence").(lua.LNumber); ok {
			fp.Confidence = float64(n)
		}
	}
	return fp, nil
}

// Discard closes the underlying Lua VM WITHOUT running driver_cleanup.
// Used by the fingerprint orchestrator: the probe never called
// driver_init, so the matching teardown is "throw the VM away", not the
// driver's cleanup (which may write to the device).
func (d *LuaDriver) Discard() {
	d.mu.Lock()
	d.L.Close()
	d.mu.Unlock()
}

// RunFingerprint loads the driver at luaPath into a fresh VM wired to the
// given (capability-bearing) HostEnv, runs its driver_fingerprint hook,
// and discards the VM. driver_init / driver_cleanup are intentionally not
// invoked — fingerprinting is a passive probe and must not reconfigure the
// device. A driver that fails to load yields MatchUnknown + error.
func RunFingerprint(luaPath string, env *HostEnv, target FingerprintTarget) (Fingerprint, error) {
	d, err := NewLuaDriver(luaPath, env)
	if err != nil {
		return Fingerprint{Match: MatchUnknown, Err: err.Error()}, err
	}
	defer d.Discard()
	return d.Fingerprint(context.Background(), target)
}

// luaFieldString reads a string-valued field from a Lua table, returning
// "" when absent or not a string.
func luaFieldString(t *lua.LTable, key string) string {
	if s, ok := t.RawGetString(key).(lua.LString); ok {
		return string(s)
	}
	return ""
}
