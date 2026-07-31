package drivers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

type controlV2Modbus struct {
	registers map[uint16]uint16
	writes    int
	writeErr  error
}

func (m *controlV2Modbus) Read(addr, count uint16, _ int32) ([]uint16, error) {
	out := make([]uint16, count)
	for i := range out {
		out[i] = m.registers[addr+uint16(i)]
	}
	return out, nil
}

func (m *controlV2Modbus) WriteSingle(addr, value uint16) error {
	if m.writeErr != nil {
		return m.writeErr
	}
	m.registers[addr] = value
	m.writes++
	return nil
}

func (m *controlV2Modbus) WriteMulti(addr uint16, values []uint16) error {
	if m.writeErr != nil {
		return m.writeErr
	}
	for i, value := range values {
		m.registers[addr+uint16(i)] = value
	}
	m.writes++
	return nil
}

func (m *controlV2Modbus) Close() error { return nil }

const controlV2Lua = `
assert(os == nil and io == nil and debug == nil and package == nil)
assert(load == nil and loadfile == nil and dofile == nil and require == nil and getfenv == nil and setfenv == nil)

init_write_error = nil
poll_write_error = nil

function driver_init(config)
    init_write_error = host.modbus_write(10, 99)
end

function driver_poll()
    poll_write_error = host.modbus_write(10, 98)
    return 1000
end

function driver_command_v2(command)
    local power_w = command.inputs.power_w
    local err = host.modbus_write(10, power_w)
    if err then
        return {status="failed", code="write_failed", message=err, device_state="unchanged"}
    end
    local values, read_err = host.modbus_read(10, 1, "holding")
    if read_err or values[1] ~= power_w then
        return {status="failed", code="readback_failed", message=read_err or "mismatch", device_state="unknown"}
    end
    return {
        status="applied", code="ok", device_state="controlled",
        evidence={"write_ack", "readback"}, applied={power_w=power_w}
    }
end

function driver_default_mode_v2(context)
    local err = host.modbus_write(10, 0)
    if err then
        return {status="failed", code="default_write_failed", message=err, device_state="unknown"}
    end
    local values, read_err = host.modbus_read(10, 1, "holding")
    if read_err or values[1] ~= 0 then
        return {status="failed", code="default_readback_failed", message=read_err or "mismatch", device_state="unknown"}
    end
    return {status="defaulted", code="default_restored", device_state="default", evidence={"write_ack", "readback"}}
end
`

func testControlV2Policy(enabled bool) *RuntimePolicy {
	return &RuntimePolicy{
		PackageID: "com.sourceful.driver.test", Version: "1.0.0",
		ArtifactSHA256: strings.Repeat("a", 64), RuntimeABI: ControlRuntimeABIV2,
		HostAPIProfile: ControlHostAPIProfileV2,
		Permissions:    map[string]bool{"modbus.read": true, "modbus.write": true},
		Commands: map[string]RuntimeCommand{
			"battery.set_power": {
				ID: "battery.set_power", RuntimeAction: "battery",
				Inputs: map[string]RuntimeCommandInput{"power_w": {Type: "number", Required: true}},
			},
		},
		DefaultMode: "driver_default_mode_v2",
		Lease: RuntimeLeasePolicy{
			MaxDuration: 30 * time.Second, HeartbeatInterval: 10 * time.Second, ExpiryAction: "return_to_default",
		},
		SiteEnabled: enabled,
	}
}

func TestControlV2PolicyRejectsNetworkPermissions(t *testing.T) {
	for _, permission := range []string{"http.get", "http.post", "mqtt.subscribe", "mqtt.publish"} {
		policy := testControlV2Policy(true)
		policy.Permissions[permission] = true
		if err := policy.validate(); err == nil || !strings.Contains(err.Error(), "only supports Modbus") {
			t.Fatalf("permission %q validation error = %v", permission, err)
		}
	}
}

func loadControlV2Driver(t *testing.T, policy *RuntimePolicy) (*LuaDriver, *controlV2Modbus) {
	return loadControlV2DriverSource(t, policy, controlV2Lua)
}

func loadControlV2DriverSource(t *testing.T, policy *RuntimePolicy, source string) (*LuaDriver, *controlV2Modbus) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "control.lua")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	modbus := &controlV2Modbus{registers: make(map[uint16]uint16)}
	env := NewHostEnv("control-v2", telemetry.NewStore()).WithModbus(modbus)
	driver, err := NewLuaDriverWithPolicy(path, env, policy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(driver.Cleanup)
	return driver, modbus
}

func commandV2(now time.Time) DriverCommandV1 {
	return DriverCommandV1{
		SchemaVersion: DriverCommandSchemaV1,
		ID:            "ftw.command:0123456789abcdef", Command: "battery.set_power", Source: "ftw.control",
		IssuedAt: now, ExpiresAt: now.Add(5 * time.Second), Attempt: 1,
		Lease: DriverCommandLeaseV1{
			ID: "ftw.lease:0123456789abcdef", ExpiresAt: now.Add(20 * time.Second), HeartbeatIntervalMS: 10000,
		},
		Inputs: map[string]interface{}{"power_w": float64(750)},
	}
}

func TestControlV2RestrictsWritesAndReturnsHostEvidence(t *testing.T) {
	driver, modbus := loadControlV2Driver(t, testControlV2Policy(true))
	if err := driver.Init(context.Background(), map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	if modbus.writes != 0 {
		t.Fatalf("init wrote to hardware: %d writes", modbus.writes)
	}
	if _, err := driver.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if modbus.writes != 0 {
		t.Fatalf("poll wrote to hardware: %d writes", modbus.writes)
	}

	now := time.Now()
	result, err := driver.CommandV2(context.Background(), commandV2(now), now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "applied" || result.DeviceState != "controlled" || result.Writes != 1 {
		t.Fatalf("unexpected command result: %+v", result)
	}
	if modbus.registers[10] != 750 {
		t.Fatalf("register = %d, want 750", modbus.registers[10])
	}

	defaultResult, err := driver.DefaultModeV2(context.Background(), "ftw.default:0123456789abcdef", "test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if defaultResult.Status != "defaulted" || defaultResult.DeviceState != "default" || modbus.registers[10] != 0 {
		t.Fatalf("unexpected default result: %+v register=%d", defaultResult, modbus.registers[10])
	}
}

func TestSignedReadOnlyPolicyDeniesWritesInEveryLifecyclePhase(t *testing.T) {
	source := `
function driver_init(config) host.modbus_write(10, 1) end
function driver_poll() host.modbus_write(10, 2) return 1000 end
function driver_command(action, value, command) host.modbus_write(10, 3) end
function driver_default_mode() host.modbus_write(10, 4) end
function driver_cleanup() host.modbus_write(10, 5) end
`
	path := filepath.Join(t.TempDir(), "read-only.lua")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	modbus := &controlV2Modbus{registers: make(map[uint16]uint16)}
	policy := &RuntimePolicy{
		PackageID: "com.sourceful.driver.read-only", Version: "1.0.0",
		ArtifactSHA256: strings.Repeat("b", 64), RuntimeABI: "gopher-lua-source-v1",
		HostAPIProfile: "sourceful.host/ftw-core/v1", ReadOnly: true,
		Permissions: map[string]bool{"modbus.read": true},
	}
	env := NewHostEnv("read-only", telemetry.NewStore()).WithModbus(modbus)
	driver, err := NewLuaDriverWithPolicy(path, env, policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Init(context.Background(), map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	if _, err := driver.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.Command(context.Background(), []byte(`{"action":"test","power_w":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := driver.DefaultMode(); err != nil {
		t.Fatal(err)
	}
	driver.Cleanup()
	if modbus.writes != 0 {
		t.Fatalf("signed read-only driver reached hardware: %d writes", modbus.writes)
	}
}

func TestControlV2RequiresExactSiteOptIn(t *testing.T) {
	driver, modbus := loadControlV2Driver(t, testControlV2Policy(false))
	now := time.Now()
	result, err := driver.CommandV2(context.Background(), commandV2(now), now)
	if err == nil || result.Status != "rejected" || modbus.writes != 0 {
		t.Fatalf("result=%+v err=%v writes=%d", result, err, modbus.writes)
	}
}

func TestControlV2RejectsExpiredCommand(t *testing.T) {
	driver, modbus := loadControlV2Driver(t, testControlV2Policy(true))
	now := time.Now()
	cmd := commandV2(now.Add(-time.Minute))
	result, err := driver.CommandV2(context.Background(), cmd, now)
	if err == nil || result.Status != "expired" || modbus.writes != 0 {
		t.Fatalf("result=%+v err=%v writes=%d", result, err, modbus.writes)
	}
}

func TestControlV2RejectsReadbackTakenBeforeWrite(t *testing.T) {
	source := `
function driver_command_v2(command)
    local before, read_err = host.modbus_read(10, 1, "holding")
    if read_err then error(read_err) end
    local write_err = host.modbus_write(10, command.inputs.power_w)
    if write_err then error(write_err) end
    return {status="applied", code="ok", device_state="controlled", evidence={"readback"}}
end
function driver_default_mode_v2(context)
    local write_err = host.modbus_write(10, 0)
    if write_err then error(write_err) end
    local value, read_err = host.modbus_read(10, 1, "holding")
    if read_err then error(read_err) end
    return {status="defaulted", code="ok", device_state="default", evidence={"readback"}}
end
`
	driver, _ := loadControlV2DriverSource(t, testControlV2Policy(true), source)
	now := time.Now()
	result, err := driver.CommandV2(context.Background(), commandV2(now), now)
	if err == nil || result.Status != "failed" || result.Code != "evidence_unproven" || result.Writes != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestControlV2RejectsReadbackAfterFailedWrite(t *testing.T) {
	source := `
function driver_command_v2(command)
    host.modbus_write(10, command.inputs.power_w)
    local value = host.modbus_read(10, 1, "holding")
    return {status="applied", code="ok", device_state="controlled", evidence={"write_ack", "readback"}}
end
function driver_default_mode_v2(context)
    return {status="failed", code="unused", device_state="unknown"}
end
`
	driver, modbus := loadControlV2DriverSource(t, testControlV2Policy(true), source)
	modbus.registers[10] = 750
	modbus.writeErr = errors.New("device write failed")
	now := time.Now()
	result, err := driver.CommandV2(context.Background(), commandV2(now), now)
	if err == nil || result.Status != "failed" || result.Code != "evidence_unproven" || result.Writes != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestControlV2LifecycleHonorsCanceledContext(t *testing.T) {
	source := `
function driver_init(config)
    while true do end
end
function driver_default_mode_v2(context)
    return {status="failed", code="unused", device_state="unknown"}
end
`
	driver, _ := loadControlV2DriverSource(t, testControlV2Policy(true), source)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := driver.Init(ctx, nil); err == nil {
		t.Fatal("canceled v2 init was not interrupted")
	}
}

func TestControlV2RegistryRestoresDefaultAfterPartialFailure(t *testing.T) {
	source := `
function driver_init(config) end
function driver_poll() return 60000 end
function driver_command_v2(command)
    local write_err = host.modbus_write(10, command.inputs.power_w)
    if write_err then error(write_err) end
    local value, read_err = host.modbus_read(10, 1, "holding")
    if read_err then error(read_err) end
    return {status="failed", code="device_rejected", device_state="unknown", evidence={"write_ack", "readback"}}
end
function driver_default_mode_v2(context)
    local write_err = host.modbus_write(10, 0)
    if write_err then error(write_err) end
    local value, read_err = host.modbus_read(10, 1, "holding")
    if read_err then error(read_err) end
    return {status="defaulted", code="default_restored", device_state="default", evidence={"write_ack", "readback"}}
end
`
	path := filepath.Join(t.TempDir(), "control.lua")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	modbus := &controlV2Modbus{registers: make(map[uint16]uint16)}
	registry := NewRegistry(telemetry.NewStore())
	registry.RuntimePolicyResolver = func(config.Driver) (*RuntimePolicy, error) {
		return testControlV2Policy(true), nil
	}
	registry.ModbusFactory = func(string, *config.ModbusConfig) (ModbusCap, error) {
		return modbus, nil
	}
	var results []DriverCommandResultV1
	registry.CommandResultSink = func(_ string, result DriverCommandResultV1) {
		results = append(results, result)
	}
	cfg := config.Driver{
		Name: "control-v2", Lua: path,
		Modbus: &config.ModbusConfig{Host: "device", Port: 502},
	}
	if err := registry.Add(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { registry.remove(cfg.Name, true) })
	if err := registry.Send(context.Background(), cfg.Name, []byte(`{"action":"battery","power_w":750}`)); err == nil {
		t.Fatal("partly applied failed command was accepted")
	}
	if modbus.registers[10] != 0 || modbus.writes != 3 {
		t.Fatalf("register=%d writes=%d, want default register and startup/command/fallback writes", modbus.registers[10], modbus.writes)
	}
	if len(results) != 3 || results[1].Status != "failed" || results[2].Status != "defaulted" {
		t.Fatalf("command results = %+v", results)
	}
}

func TestControlV2RegistryExpiresLeaseIntoDefault(t *testing.T) {
	source := `
function driver_init(config) end
function driver_poll() return 60000 end
function driver_command_v2(command)
    local write_err = host.modbus_write(10, command.inputs.power_w)
    if write_err then error(write_err) end
    local value, read_err = host.modbus_read(10, 1, "holding")
    if read_err then error(read_err) end
    return {status="applied", code="ok", device_state="controlled", evidence={"write_ack", "readback"}}
end
function driver_default_mode_v2(context)
    local write_err = host.modbus_write(10, 0)
    if write_err then error(write_err) end
    local value, read_err = host.modbus_read(10, 1, "holding")
    if read_err then error(read_err) end
    return {status="defaulted", code="default_restored", device_state="default", evidence={"write_ack", "readback"}}
end
`
	path := filepath.Join(t.TempDir(), "control.lua")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	modbus := &controlV2Modbus{registers: make(map[uint16]uint16)}
	policy := testControlV2Policy(true)
	policy.Lease.MaxDuration = 2 * time.Second
	policy.Lease.HeartbeatInterval = time.Second
	registry := NewRegistry(telemetry.NewStore())
	registry.RuntimePolicyResolver = func(config.Driver) (*RuntimePolicy, error) { return policy, nil }
	registry.ModbusFactory = func(string, *config.ModbusConfig) (ModbusCap, error) { return modbus, nil }
	resultCh := make(chan DriverCommandResultV1, 4)
	registry.CommandResultSink = func(_ string, result DriverCommandResultV1) { resultCh <- result }
	cfg := config.Driver{
		Name: "control-v2", Lua: path,
		Modbus: &config.ModbusConfig{Host: "device", Port: 502},
	}
	if err := registry.Add(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { registry.remove(cfg.Name, true) })
	<-resultCh // startup default
	if err := registry.Send(context.Background(), cfg.Name, []byte(`{"action":"battery","power_w":750}`)); err != nil {
		t.Fatal(err)
	}
	if modbus.registers[10] != 750 {
		t.Fatalf("controlled register = %d, want 750", modbus.registers[10])
	}
	deadline := time.After(4 * time.Second)
	for {
		select {
		case result := <-resultCh:
			if result.Status == "defaulted" {
				if modbus.registers[10] != 0 {
					t.Fatalf("expired lease left register at %d", modbus.registers[10])
				}
				return
			}
		case <-deadline:
			t.Fatal("lease did not restore default mode")
		}
	}
}
