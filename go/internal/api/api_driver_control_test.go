package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A driver that declares one control, records what it was commanded and
// counts its own default-mode calls, so a test can tell "Core sent it" from
// "Core said it sent it".
const controlProbeLua = `DRIVER = {
  id      = "probe",
  name    = "Probe",
  version = "1.0.0",
  controls = {
    {
      id       = "set_offset",
      label    = "Offset",
      evidence = "readback",
      input    = { type = "number", min = -3, max = 3, step = 1, unit = "C" },
    },
  },
}

local applied   = nil
local defaulted = 0
local startup_default = true

function driver_init(config)
    host.set_make("Probe")
    -- The default first poll is 5 s away, which would make every assertion
    -- here a five-second wait for a value that was already set.
    host.set_poll_interval(100)
end

function driver_poll()
    if applied ~= nil then host.emit_metric("applied", applied, "C") end
    host.emit_metric("defaulted", defaulted, "n")
    return 100
end

function driver_command(action, power_w, cmd)
    if action == "set_offset" then
        applied = tonumber(cmd and (cmd.offset or cmd.value))
        return true
    end
    return false
end

function driver_default_mode()
    if startup_default then
        startup_default = false
        applied = 0
        return true
    end
    defaulted = defaulted + 1
    applied   = 0
end
`

const controlTypesProbeLua = `DRIVER = {
  id      = "probe_types",
  name    = "Probe types",
  version = "1.0.0",
  controls = {
    { id = "set_boost", input = { type = "boolean" } },
    { id = "set_mode", input = { type = "string" } },
  },
}

local applied = 0

function driver_init(config)
    host.set_make("Probe types")
    host.set_poll_interval(100)
end

function driver_poll()
    host.emit_metric("applied", applied, "n")
    return 100
end

function driver_command(action, power_w, cmd)
    if action == "set_boost" then
        if cmd.value == true then applied = 1 else applied = 2 end
        return true
    end
    if action == "set_mode" then
        if cmd.value == "eco" then applied = 3 else applied = 4 end
        return true
    end
    return false
end

function driver_default_mode()
    applied = 0
end
`

const controlSafetyProbeLua = `DRIVER = {
  id      = "probe_safety",
  name    = "Probe safety",
  version = "1.0.0",
  controls = {
    { id = "set_offset", input = { type = "number", min = -3, max = 3 } },
    { id = "set_offset_fail", input = { type = "number", min = -3, max = 3 } },
  },
}

local applied   = nil
local defaulted = 0
local startup_default = true

function driver_init(config)
    host.set_make("Probe safety")
    host.set_poll_interval(100)
end

function driver_poll()
    if applied ~= nil then host.emit_metric("applied", applied, "n") end
    host.emit_metric("defaulted", defaulted, "n")
    return 100
end

function driver_command(action, power_w, cmd)
    if action == "set_offset" then
        applied = tonumber(cmd.value)
        return true
    end
    if action == "set_offset_fail" then
        applied = tonumber(cmd.value)
        host.sleep(100)
        return false
    end
    return false
end

function driver_default_mode()
    if startup_default then
        startup_default = false
        applied = 0
        return true
    end
    defaulted = defaulted + 1
    host.emit_metric("default_started", defaulted, "n")
    host.sleep(200)
    applied = 0
end
`

const controlRecoveryProbeLua = `DRIVER = {
  id      = "probe_recovery",
  name    = "Probe recovery",
  version = "1.0.0",
  controls = {
    { id = "set_offset", input = { type = "number", min = -3, max = 3 } },
  },
}

local applied = nil
local defaults = 0
local startup_default = true

function driver_init(config)
    host.set_make("Probe recovery")
    host.set_poll_interval(100)
end

function driver_poll()
    if applied ~= nil then host.emit_metric("applied", applied, "n") end
    host.emit_metric("defaulted", defaults, "n")
    return 100
end

function driver_command(action, power_w, cmd)
    applied = tonumber(cmd and (cmd.value or cmd.offset))
    host.emit_metric("command_applied", applied, "n")
    if defaults < 2 then return false end
    return true
end

function driver_default_mode()
    if startup_default then
        startup_default = false
        applied = 0
        return true
    end
    defaults = defaults + 1
    host.emit_metric("default_attempt", defaults, "n")
    if defaults == 1 then return false end
    if defaults == 2 then host.sleep(500) end
    applied = 0
end
`

const controlExpiryRecoveryProbeLua = `DRIVER = {
  id      = "probe_expiry_recovery",
  name    = "Probe expiry recovery",
  version = "1.0.0",
  controls = {
    { id = "set_offset", input = { type = "number", min = -3, max = 3 } },
  },
}

local applied = nil
local defaults = 0
local startup_default = true

function driver_init(config)
    host.set_poll_interval(100)
end

function driver_poll()
    if applied ~= nil then host.emit_metric("applied", applied, "n") end
    return 100
end

function driver_command(action, power_w, cmd)
    applied = tonumber(cmd and (cmd.value or cmd.offset))
    return true
end

function driver_default_mode()
    if startup_default then
        startup_default = false
        applied = 0
        return true
    end
    defaults = defaults + 1
    host.emit_metric("default_attempt", defaults, "n")
    if defaults == 1 then return false end
    if defaults == 2 then host.sleep(500) end
    applied = 0
end
`

func controlServer(t *testing.T) (*Server, *telemetry.Store) {
	return controlServerWithLua(t, controlProbeLua)
}

func controlServerWithLua(t *testing.T, source string) (*Server, *telemetry.Store) {
	return controlServerWithLuaConfig(t, source, config.Driver{Name: "heat"})
}

func controlServerWithLuaConfig(t *testing.T, source string, cfg config.Driver) (*Server, *telemetry.Store) {
	t.Helper()
	dir := t.TempDir()
	lua := filepath.Join(dir, "probe.lua")
	if err := os.WriteFile(lua, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	tel := telemetry.NewStore()
	reg := drivers.NewRegistry(tel)
	cfg.Lua = lua
	if err := reg.Add(context.Background(), cfg); err != nil {
		t.Fatalf("add driver: %v", err)
	}
	t.Cleanup(reg.ShutdownAll)
	srv := New(&Deps{
		Tel:        tel,
		Registry:   reg,
		Cfg:        &config.Config{Drivers: []config.Driver{cfg}},
		CfgMu:      &sync.RWMutex{},
		DriverDir:  dir,
		ConfigPath: filepath.Join(dir, "config.yaml"),
	})
	return srv, tel
}

func post(t *testing.T, srv *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// waitMetric polls for a metric to reach want, so the test follows the
// driver's own poll loop rather than a sleep chosen by guess.
func waitMetric(t *testing.T, tel *telemetry.Store, driver, metric string, want float64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last float64
	for time.Now().Before(deadline) {
		if got, _, ok := tel.LatestMetric(driver, metric); ok {
			last = got
			if got == want {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s/%s = %v, want %v", driver, metric, last, want)
}

// The value reaches the driver, and Core clamps it to the declared bound
// rather than trusting the Lua to do it.
func TestDriverControlClampsAndReachesDriver(t *testing.T) {
	srv, tel := controlServer(t)

	rec := post(t, srv, "/api/drivers/heat/control",
		`{"control":"set_offset","value":99,"duration_s":600}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Applied  *float64 `json:"applied"`
		Evidence string   `json:"evidence"`
		Expires  int64    `json:"expires_at_ms"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Applied == nil || *resp.Applied != 3 {
		t.Errorf("applied = %v, want 3 (clamped from 99)", resp.Applied)
	}
	if resp.Evidence != "readback" {
		t.Errorf("evidence = %q", resp.Evidence)
	}
	if resp.Expires <= time.Now().UnixMilli() {
		t.Errorf("expires_at_ms = %d, want in the future", resp.Expires)
	}
	waitMetric(t, tel, "heat", "applied", 3)
}

// The declaration is the allowlist. A control the driver never declared is a
// 400, not a 200 for a command the Lua silently ignored.
func TestDriverControlRejectsUndeclared(t *testing.T) {
	srv, _ := controlServer(t)

	rec := post(t, srv, "/api/drivers/heat/control", `{"control":"set_fan","value":1}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown control = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	rec = post(t, srv, "/api/drivers/heat/control", `{"control":"set_offset"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing value = %d, want 400", rec.Code)
	}
	rec = post(t, srv, "/api/drivers/nosuch/control", `{"control":"set_offset","value":1}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown driver = %d, want 404", rec.Code)
	}
}

func TestDriverControlPreservesDeclaredBooleanAndStringValues(t *testing.T) {
	srv, tel := controlServerWithLua(t, controlTypesProbeLua)

	if rec := post(t, srv, "/api/drivers/heat/control",
		`{"control":"set_boost","value":true,"duration_s":600}`); rec.Code != http.StatusOK {
		t.Fatalf("boolean POST = %d, body %s", rec.Code, rec.Body.String())
	}
	waitMetric(t, tel, "heat", "applied", 1)
	if hold := srv.activeControlHold("heat"); hold == nil || hold.Value != true {
		t.Fatalf("boolean hold = %+v, want true", hold)
	}

	if rec := post(t, srv, "/api/drivers/heat/control",
		`{"control":"set_mode","value":"eco","duration_s":600}`); rec.Code != http.StatusOK {
		t.Fatalf("string POST = %d, body %s", rec.Code, rec.Body.String())
	}
	waitMetric(t, tel, "heat", "applied", 3)
	if hold := srv.activeControlHold("heat"); hold == nil || hold.Value != "eco" {
		t.Fatalf("string hold = %+v, want eco", hold)
	}
}

func TestDriverControlHoldIsVisibleAndReleasable(t *testing.T) {
	srv, tel := controlServer(t)

	if rec := post(t, srv, "/api/drivers/heat/control",
		`{"control":"set_offset","value":2,"duration_s":600}`); rec.Code != http.StatusOK {
		t.Fatalf("POST = %d", rec.Code)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/drivers/heat", nil))
	var detail driverDetailResp
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Hold == nil || detail.Hold.Control != "set_offset" {
		t.Fatalf("hold = %+v, want set_offset", detail.Hold)
	}
	value, ok := detail.Hold.Value.(float64)
	if !ok || value != 2 {
		t.Errorf("hold value = %v, want 2", detail.Hold.Value)
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec,
		httptest.NewRequest(http.MethodDelete, "/api/drivers/heat/control", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE = %d, body %s", rec.Code, rec.Body.String())
	}
	// Releasing calls the driver's own default mode, not a value this
	// package invented.
	waitMetric(t, tel, "heat", "defaulted", 1)
	if hold := srv.activeControlHold("heat"); hold != nil {
		t.Errorf("hold survived release: %+v", hold)
	}
}

// The whole reason a hold is bounded: it has to end by itself. An offset that
// outlives the browser tab that set it heats a house wrong for weeks.
func TestDriverControlHoldExpiresIntoDefault(t *testing.T) {
	srv, tel := controlServer(t)

	if rec := post(t, srv, "/api/drivers/heat/control",
		`{"control":"set_offset","value":2,"duration_s":1}`); rec.Code != http.StatusOK {
		t.Fatalf("POST = %d", rec.Code)
	}
	waitMetric(t, tel, "heat", "applied", 2)
	waitMetric(t, tel, "heat", "defaulted", 1)
	if hold := srv.activeControlHold("heat"); hold != nil {
		t.Errorf("hold survived expiry: %+v", hold)
	}
}

// The request may be paused after validation while a lifecycle reload swaps
// the running instance. The hold must use the generation that received the
// command, so expiry defaults that instance instead of discarding the hold as
// stale.
func TestDriverControlBindsHoldToRestartedGeneration(t *testing.T) {
	srv, tel := controlServer(t)
	cfg := srv.deps.Cfg.Drivers[0]
	oldStatus, ok := srv.deps.Registry.ControlStatus("heat")
	if !ok {
		t.Fatal("driver is not running before generation race")
	}

	paused := make(chan struct{})
	resume := make(chan struct{})
	var pauseOnce sync.Once
	srv.beforeDriverControlSend = func() {
		pauseOnce.Do(func() {
			close(paused)
			<-resume
		})
	}

	postDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		postDone <- post(t, srv, "/api/drivers/heat/control",
			`{"control":"set_offset","value":2,"duration_s":600}`)
	}()
	select {
	case <-paused:
	case <-time.After(time.Second):
		t.Fatal("control request did not reach the pre-send pause")
	}

	if err := srv.deps.Registry.Restart(context.Background(), cfg); err != nil {
		t.Fatalf("restart during control dispatch = %v", err)
	}
	newStatus, ok := srv.deps.Registry.ControlStatus("heat")
	if !ok {
		t.Fatal("replacement driver is not running")
	}
	if newStatus.Generation == oldStatus.Generation {
		t.Fatalf("restart kept generation %d", newStatus.Generation)
	}
	close(resume)

	var rec *httptest.ResponseRecorder
	select {
	case rec = <-postDone:
	case <-time.After(2 * time.Second):
		t.Fatal("control request did not finish after restart")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("POST after restart race = %d, body %s", rec.Code, rec.Body.String())
	}
	waitMetric(t, tel, "heat", "applied", 2)
	hold := srv.activeControlHold("heat")
	if hold == nil {
		t.Fatal("control request did not create a hold")
	}
	if hold.Generation != newStatus.Generation {
		t.Fatalf("hold generation = %d, want replacement generation %d", hold.Generation, newStatus.Generation)
	}

	state := srv.peekControlState("heat")
	if state == nil {
		t.Fatal("control state disappeared before expiry")
	}
	state.mu.Lock()
	fired := state.hold
	state.mu.Unlock()
	if err := srv.expireControlHold("heat", state, fired); err != nil {
		t.Fatalf("expiry after generation-bound command = %v", err)
	}
	waitMetric(t, tel, "heat", "defaulted", 1)
	waitMetric(t, tel, "heat", "applied", 0)
	if got := srv.activeControlHold("heat"); got != nil {
		t.Fatalf("hold survived successful expiry: %+v", got)
	}
}

func TestDriverControlLookupRaceCannotOrphanStateOrTimer(t *testing.T) {
	tests := []struct {
		name      string
		lifecycle func(*Server, config.Driver) error
	}{
		{
			name: "remove",
			lifecycle: func(srv *Server, _ config.Driver) error {
				srv.deps.Registry.Remove("heat")
				return nil
			},
		},
		{
			name: "restart",
			lifecycle: func(srv *Server, cfg config.Driver) error {
				return srv.deps.Registry.Restart(context.Background(), cfg)
			},
		},
		{
			name: "reload",
			lifecycle: func(srv *Server, cfg config.Driver) error {
				srv.deps.Registry.Reload(context.Background(), []config.Driver{cfg}, true)
				return nil
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			srv, tel := controlServerWithLua(t, controlSafetyProbeLua)
			cfg := srv.deps.Cfg.Drivers[0]
			lookedUp := make(chan struct{})
			resume := make(chan struct{})
			var pauseOnce sync.Once
			var resumeOnce sync.Once
			resumeRequest := func() { resumeOnce.Do(func() { close(resume) }) }
			defer resumeRequest()
			srv.beforeDriverControlStateLock = func() {
				pauseOnce.Do(func() {
					close(lookedUp)
					<-resume
				})
			}

			postDone := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				postDone <- post(t, srv, "/api/drivers/heat/control",
					`{"control":"set_offset","value":1,"duration_s":1}`)
			}()
			select {
			case <-lookedUp:
			case <-time.After(time.Second):
				t.Fatal("control request did not reach the post-lookup pause")
			}

			lifecycleDone := make(chan error, 1)
			go func() { lifecycleDone <- test.lifecycle(srv, cfg) }()
			mapProbeDone := make(chan bool, 1)
			go func() { mapProbeDone <- srv.peekControlState("heat") == nil }()
			select {
			case removed := <-mapProbeDone:
				if removed {
					t.Fatalf("%s removed control state between lookup and state.mu", test.name)
				}
			case <-time.After(100 * time.Millisecond):
			}

			resumeRequest()
			var rec *httptest.ResponseRecorder
			select {
			case rec = <-postDone:
			case <-time.After(2 * time.Second):
				t.Fatal("control request did not finish after lifecycle release")
			}
			if rec.Code != http.StatusNotFound {
				t.Fatalf("raced POST = %d, body %s; request must not arm a stale state", rec.Code, rec.Body.String())
			}
			select {
			case err := <-lifecycleDone:
				if err != nil {
					t.Fatalf("%s = %v", test.name, err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("lifecycle deadlocked after the request released state.mu")
			}

			if got := srv.activeControlHold("heat"); got != nil {
				t.Fatalf("%s left an orphaned hold: %+v", test.name, got)
			}
			if got := driverDetail(t, srv, "heat"); got.Hold != nil {
				t.Fatalf("GET after %s exposed an orphaned hold: %+v", test.name, got.Hold)
			}

			if test.name == "remove" {
				time.Sleep(1500 * time.Millisecond)
				if got, _, ok := tel.LatestMetric("heat", "defaulted"); ok && got != 0 {
					t.Fatalf("removed driver received orphaned timer default: %v", got)
				}
				return
			}

			if rec := post(t, srv, "/api/drivers/heat/control",
				`{"control":"set_offset","value":-2,"duration_s":600}`); rec.Code != http.StatusOK {
				t.Fatalf("new-generation POST after %s = %d, body %s", test.name, rec.Code, rec.Body.String())
			}
			waitMetric(t, tel, "heat", "applied", -2)
			if got := driverDetail(t, srv, "heat"); got.Hold == nil {
				t.Fatalf("GET after %s lost the valid replacement hold", test.name)
			}
			time.Sleep(1500 * time.Millisecond)
			if got, _, ok := tel.LatestMetric("heat", "defaulted"); ok && got != 0 {
				t.Fatalf("old timer defaulted the new generation after %s: %v", test.name, got)
			}
			if got, _, ok := tel.LatestMetric("heat", "applied"); !ok || got != -2 {
				t.Fatalf("new generation after %s applied %v/%v, want -2", test.name, got, ok)
			}
		})
	}
}

func TestDriverControlExpiryDefaultFailureBlocksUntilRecovery(t *testing.T) {
	srv, _ := controlServerWithLua(t, controlExpiryRecoveryProbeLua)

	if rec := post(t, srv, "/api/drivers/heat/control",
		`{"control":"set_offset","value":2,"duration_s":600}`); rec.Code != http.StatusOK {
		t.Fatalf("POST = %d, body %s", rec.Code, rec.Body.String())
	}
	state := srv.peekControlState("heat")
	if state == nil {
		t.Fatal("missing control state")
	}
	state.mu.Lock()
	hold := state.hold
	state.mu.Unlock()
	if hold == nil {
		t.Fatal("missing control hold")
	}

	if err := srv.expireControlHold("heat", state, hold); err == nil {
		t.Fatal("expiry hid a failed default")
	}
	if got := srv.activeControlHold("heat"); got != nil {
		t.Fatalf("failed expiry left hold active: %+v", got)
	}
	status, ok := srv.deps.Registry.ControlStatus("heat")
	if !ok || !status.Blocked || !status.RecoveryPending || status.DefaultConfirmed {
		t.Fatalf("status after failed expiry default = %+v, running=%v", status, ok)
	}
	if rec := post(t, srv, "/api/drivers/heat/control",
		`{"control":"set_offset","value":3,"duration_s":600}`); rec.Code != http.StatusConflict {
		t.Fatalf("control during expiry recovery = %d, body %s", rec.Code, rec.Body.String())
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status, ok = srv.deps.Registry.ControlStatus("heat")
		if ok && !status.Blocked && status.DefaultConfirmed && !status.RecoveryPending {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ok || status.Blocked || !status.DefaultConfirmed || status.RecoveryPending {
		t.Fatalf("status after expiry default recovery = %+v, running=%v", status, ok)
	}
	if rec := post(t, srv, "/api/drivers/heat/control",
		`{"control":"set_offset","value":3,"duration_s":600}`); rec.Code != http.StatusOK {
		t.Fatalf("control after expiry default recovery = %d, body %s", rec.Code, rec.Body.String())
	}
}

// Replacing a hold must not leave the old timer able to default the device
// out from under the new setting.
func TestDriverControlReplacingHoldCancelsTheOldTimer(t *testing.T) {
	srv, tel := controlServer(t)

	if rec := post(t, srv, "/api/drivers/heat/control",
		`{"control":"set_offset","value":1,"duration_s":1}`); rec.Code != http.StatusOK {
		t.Fatalf("first POST = %d", rec.Code)
	}
	if rec := post(t, srv, "/api/drivers/heat/control",
		`{"control":"set_offset","value":-2,"duration_s":600}`); rec.Code != http.StatusOK {
		t.Fatalf("second POST = %d", rec.Code)
	}
	waitMetric(t, tel, "heat", "applied", -2)

	// Past when the first hold would have fired.
	time.Sleep(1500 * time.Millisecond)
	if got, _, ok := tel.LatestMetric("heat", "defaulted"); ok && got != 0 {
		t.Errorf("defaulted = %v, want 0 — the replaced timer still fired", got)
	}
	if got, _, ok := tel.LatestMetric("heat", "applied"); !ok || got != -2 {
		t.Errorf("applied = %v, want -2 to survive", got)
	}
}

func TestDriverControlAmbiguousCommandRestoresDefault(t *testing.T) {
	srv, tel := controlServerWithLua(t, controlSafetyProbeLua)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/drivers/heat/control", strings.NewReader(
		`{"control":"set_offset_fail","value":2,"duration_s":600}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("ambiguous POST = %d, body %s", rec.Code, rec.Body.String())
	}
	waitMetric(t, tel, "heat", "defaulted", 1)
	if hold := srv.activeControlHold("heat"); hold != nil {
		t.Fatalf("ambiguous command left hold active: %+v", hold)
	}
	waitMetric(t, tel, "heat", "applied", 0)
}

func TestDriverControlDefaultPathClearsHold(t *testing.T) {
	srv, tel := controlServerWithLua(t, controlSafetyProbeLua)

	if rec := post(t, srv, "/api/drivers/heat/control",
		`{"control":"set_offset","value":2,"duration_s":600}`); rec.Code != http.StatusOK {
		t.Fatalf("POST = %d, body %s", rec.Code, rec.Body.String())
	}
	if err := srv.SendDriverDefault(context.Background(), "heat"); err != nil {
		t.Fatalf("SendDriverDefault = %v", err)
	}
	if hold := srv.activeControlHold("heat"); hold != nil {
		t.Fatalf("default path left hold active: %+v", hold)
	}
	if state := srv.peekControlState("heat"); state == nil {
		t.Fatal("default path removed the existing control state")
	}
	waitMetric(t, tel, "heat", "defaulted", 1)
}

func TestSendDriverDefaultDoesNotRecreateStateAfterRemove(t *testing.T) {
	srv, _ := controlServer(t)
	srv.controlState("heat")

	paused := make(chan struct{})
	resume := make(chan struct{})
	srv.beforeDriverDefaultStateLock = func() {
		close(paused)
		<-resume
	}

	result := make(chan error, 1)
	go func() {
		result <- srv.SendDriverDefault(context.Background(), "heat")
	}()
	select {
	case <-paused:
	case <-time.After(time.Second):
		t.Fatal("SendDriverDefault did not reach the state-lock seam")
	}

	removed := make(chan struct{})
	go func() {
		srv.deps.Registry.RemoveProbe("heat")
		close(removed)
	}()
	select {
	case <-removed:
	case <-time.After(time.Second):
		close(resume)
		t.Fatal("RemoveProbe deadlocked behind SendDriverDefault")
	}
	if state := srv.peekControlState("heat"); state != nil {
		t.Fatalf("removed driver still has control state before default resumes: %p", state)
	}
	close(resume)

	select {
	case err := <-result:
		if err == nil {
			t.Fatal("SendDriverDefault succeeded for removed driver")
		}
	case <-time.After(time.Second):
		t.Fatal("SendDriverDefault did not finish after lifecycle removal")
	}
	if state := srv.peekControlState("heat"); state != nil {
		t.Fatalf("default path recreated removed control state: %p", state)
	}
}

func TestSendDriverDefaultDoesNotAccumulateRemovedStates(t *testing.T) {
	srv, _ := controlServer(t)
	srv.controlState("heat")
	base := srv.deps.Cfg.Drivers[0]

	for i := 0; i < 64; i++ {
		name := "removed-" + strconv.Itoa(i)
		cfg := base
		cfg.Name = name
		if err := srv.deps.Registry.Add(context.Background(), cfg); err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
		srv.controlState(name)

		paused := make(chan struct{})
		resume := make(chan struct{})
		srv.beforeDriverDefaultStateLock = func() {
			close(paused)
			<-resume
		}
		result := make(chan error, 1)
		go func() {
			result <- srv.SendDriverDefault(context.Background(), name)
		}()
		select {
		case <-paused:
		case <-time.After(time.Second):
			close(resume)
			t.Fatalf("SendDriverDefault(%s) did not reach the state-lock seam", name)
		}
		srv.deps.Registry.RemoveProbe(name)
		close(resume)
		select {
		case err := <-result:
			if err == nil {
				t.Fatalf("SendDriverDefault(%s) succeeded after removal", name)
			}
		case <-time.After(time.Second):
			t.Fatalf("SendDriverDefault(%s) did not finish", name)
		}
		srv.beforeDriverDefaultStateLock = nil
		if state := srv.peekControlState(name); state != nil {
			t.Fatalf("removed driver %s left a control state", name)
		}
	}

	srv.controlStateMu.Lock()
	defer srv.controlStateMu.Unlock()
	if got := len(srv.controlStates); got != 1 {
		t.Fatalf("control state map has %d entries after removed names, want only heat", got)
	}
}

// Expiry must hold the per-driver lock through the actual default command.
// Otherwise a replacement can be sent after the old hold is deleted but
// before its default reaches the device, and the old default wins last.
func TestDriverControlSerializesExpiryAndReplacement(t *testing.T) {
	srv, tel := controlServerWithLua(t, controlSafetyProbeLua)

	if rec := post(t, srv, "/api/drivers/heat/control",
		`{"control":"set_offset","value":1,"duration_s":600}`); rec.Code != http.StatusOK {
		t.Fatalf("POST = %d, body %s", rec.Code, rec.Body.String())
	}
	state := srv.controlState("heat")
	state.mu.Lock()
	hold := state.hold
	state.mu.Unlock()
	if hold == nil {
		t.Fatal("missing hold")
	}

	done := make(chan struct{})
	go func() {
		srv.expireControlHold("heat", state, hold)
		close(done)
	}()
	waitMetric(t, tel, "heat", "default_started", 1)

	if rec := post(t, srv, "/api/drivers/heat/control",
		`{"control":"set_offset","value":-2,"duration_s":600}`); rec.Code != http.StatusOK {
		t.Fatalf("replacement POST = %d, body %s", rec.Code, rec.Body.String())
	}
	<-done
	waitMetric(t, tel, "heat", "applied", -2)
}

func TestDriverControlRejectsObserveOnlyWithoutSending(t *testing.T) {
	srv, tel := controlServerWithLuaConfig(t, controlProbeLua, config.Driver{
		Name:        "heat",
		ObserveOnly: true,
	})

	rec := post(t, srv, "/api/drivers/heat/control",
		`{"control":"set_offset","value":2,"duration_s":600}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("observe_only POST = %d, body %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["observe_only"] != true {
		t.Fatalf("observe_only response = %v", body)
	}
	if _, _, ok := tel.LatestMetric("heat", "applied"); ok {
		t.Fatal("observe_only control reached the driver")
	}
	if err := srv.deps.Registry.Send(context.Background(), "heat", []byte(`{"action":"set_offset","value":2}`)); err != drivers.ErrObserveOnly {
		t.Fatalf("direct observe_only Send = %v, want %v", err, drivers.ErrObserveOnly)
	}
}

func TestDriverControlBlocksUntilDefaultRecovery(t *testing.T) {
	srv, tel := controlServerWithLua(t, controlRecoveryProbeLua)

	rec := post(t, srv, "/api/drivers/heat/control",
		`{"control":"set_offset","value":2,"duration_s":600}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("partial-effect POST = %d, body %s", rec.Code, rec.Body.String())
	}
	var blockedBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &blockedBody); err != nil {
		t.Fatal(err)
	}
	if blockedBody["control_blocked"] != true || blockedBody["default_confirmed"] != false {
		t.Fatalf("partial-effect safety response = %v", blockedBody)
	}
	waitMetric(t, tel, "heat", "command_applied", 2)
	waitMetric(t, tel, "heat", "default_attempt", 1)

	status, ok := srv.deps.Registry.ControlStatus("heat")
	if !ok || !status.Blocked || !status.RecoveryPending || status.DefaultConfirmed {
		t.Fatalf("control status after failed default = %+v, running=%v", status, ok)
	}
	rec = post(t, srv, "/api/drivers/heat/control",
		`{"control":"set_offset","value":3,"duration_s":600}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("POST while default recovery is pending = %d, body %s", rec.Code, rec.Body.String())
	}

	detail := driverDetail(t, srv, "heat")
	if detail.ControlState.State != "default_recovery" || !detail.ControlState.Blocked ||
		detail.ControlState.DefaultConfirmed || !detail.ControlState.RecoveryPending {
		t.Fatalf("recovery control state = %+v", detail.ControlState)
	}

	waitMetric(t, tel, "heat", "default_attempt", 2)
	waitMetric(t, tel, "heat", "defaulted", 2)
	status, ok = srv.deps.Registry.ControlStatus("heat")
	if !ok || status.Blocked || !status.DefaultConfirmed || status.RecoveryPending {
		t.Fatalf("control status after recovery = %+v, running=%v", status, ok)
	}
	rec = post(t, srv, "/api/drivers/heat/control",
		`{"control":"set_offset","value":3,"duration_s":600}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST after default recovery = %d, body %s", rec.Code, rec.Body.String())
	}
	waitMetric(t, tel, "heat", "applied", 3)
}

func TestDriverControlClearsHoldAcrossLifecycle(t *testing.T) {
	srv, tel := controlServer(t)
	cfg := srv.deps.Cfg.Drivers[0]

	rec := post(t, srv, "/api/drivers/heat/control",
		`{"control":"set_offset","value":2,"duration_s":600}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("initial POST = %d, body %s", rec.Code, rec.Body.String())
	}
	waitMetric(t, tel, "heat", "applied", 2)
	oldState := srv.peekControlState("heat")
	if oldState == nil {
		t.Fatal("initial control state is missing")
	}
	oldState.mu.Lock()
	oldHold := oldState.hold
	oldState.mu.Unlock()
	if oldHold == nil {
		t.Fatal("initial control hold is missing")
	}

	if err := srv.deps.Registry.Restart(context.Background(), cfg); err != nil {
		t.Fatalf("restart = %v", err)
	}
	if got := srv.activeControlHold("heat"); got != nil {
		t.Fatalf("hold survived restart: %+v", got)
	}
	if got := srv.peekControlState("heat"); got != nil {
		t.Fatal("control state map entry survived restart")
	}
	if got := driverDetail(t, srv, "heat"); got.Hold != nil {
		t.Fatalf("GET after restart exposed old hold: %+v", got.Hold)
	}

	rec = post(t, srv, "/api/drivers/heat/control",
		`{"control":"set_offset","value":-2,"duration_s":600}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST after restart = %d, body %s", rec.Code, rec.Body.String())
	}
	waitMetric(t, tel, "heat", "applied", -2)
	// Simulate a timer callback that was already in flight when the old
	// generation was removed. It must not default the new instance.
	srv.expireControlHold("heat", oldState, oldHold)
	time.Sleep(250 * time.Millisecond)
	if got, _, ok := tel.LatestMetric("heat", "defaulted"); !ok || got != 0 {
		t.Fatalf("old timer changed the replacement: defaulted=%v/%v", got, ok)
	}
	if got, _, ok := tel.LatestMetric("heat", "applied"); !ok || got != -2 {
		t.Fatalf("replacement applied value = %v/%v, want -2", got, ok)
	}

	disabled := cfg
	disabled.Disabled = true
	srv.deps.Registry.Reload(context.Background(), []config.Driver{disabled}, false)
	if got := srv.peekControlState("heat"); got != nil {
		t.Fatal("control state map entry survived disable")
	}
	if got := driverDetail(t, srv, "heat"); got.Hold != nil {
		t.Fatalf("GET while disabled exposed a hold: %+v", got.Hold)
	}

	srv.deps.Registry.Reload(context.Background(), []config.Driver{cfg}, false)
	if got := srv.peekControlState("heat"); got != nil {
		t.Fatal("control state map entry survived re-add")
	}
	if got := driverDetail(t, srv, "heat"); got.Hold != nil {
		t.Fatalf("GET after re-add exposed an old hold: %+v", got.Hold)
	}
}

func TestDriverDetailUnknownNamesDoNotCreateControlState(t *testing.T) {
	srv, _ := controlServer(t)
	for i := 0; i < 1000; i++ {
		name := "missing-" + strconv.Itoa(i)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/drivers/"+name, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, body %s", name, rec.Code, rec.Body.String())
		}
	}
	srv.controlStateMu.Lock()
	defer srv.controlStateMu.Unlock()
	if got := len(srv.controlStates); got != 0 {
		t.Fatalf("unknown GETs created %d control states", got)
	}
}
