package drivers

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

type externallyOwnedModbus struct {
	writes atomic.Int32
	reads  chan struct{}
}

func (m *externallyOwnedModbus) Read(uint16, uint16, int32) ([]uint16, error) {
	select {
	case m.reads <- struct{}{}:
	default:
	}
	return []uint16{42}, nil
}
func (m *externallyOwnedModbus) WriteSingle(uint16, uint16) error  { m.writes.Add(1); return nil }
func (m *externallyOwnedModbus) WriteMulti(uint16, []uint16) error { m.writes.Add(1); return nil }
func (m *externallyOwnedModbus) Close() error                      { return nil }
func (m *externallyOwnedModbus) ReadOnly() bool                    { return true }

func TestExternalModbusOwnerBlocksInitPollCommandAndRecoveryWrites(t *testing.T) {
	path := writeTestDriver(t, `
function driver_init(config)
    host.set_poll_interval(100)
    local err = host.modbus_write(10, 1)
    if not err then error("init was allowed to write") end
end
function driver_poll()
    local err = host.modbus_write_multi(10, {1, 2})
    if not err then error("poll was allowed to write") end
    local regs = host.modbus_read(1, 1, "holding")
    host.emit("meter", {w=regs[1]})
    return 3600
end
function driver_command(cmd) host.emit_metric("command_invoked", 1) end
function driver_default_mode() host.emit_metric("default_invoked", 1) end
function driver_cleanup() host.emit_metric("cleanup_invoked", 1) end
`)
	tel := telemetry.NewStore()
	reg := NewRegistry(tel)
	mb := &externallyOwnedModbus{reads: make(chan struct{}, 1)}
	reg.ModbusFactory = func(string, *config.ModbusConfig) (ModbusCap, error) { return mb, nil }
	ctx := context.Background()
	cfg := config.Driver{Name: "external", Lua: path, Capabilities: config.Capabilities{Modbus: &config.ModbusConfig{Host: "127.0.0.1", Port: 502, UnitID: 1}}}
	if err := reg.Add(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	defer reg.Remove("external")
	select {
	case <-mb.reads:
	case <-time.After(time.Second):
		t.Fatal("read-only driver stopped polling")
	}
	if err := reg.Send(ctx, "external", []byte(`{"action":"set"}`)); !errors.Is(err, ErrObserveOnly) {
		t.Fatalf("command: %v", err)
	}
	if err := reg.SendDefault(ctx, "external"); !errors.Is(err, ErrObserveOnly) {
		t.Fatalf("default: %v", err)
	}
	reg.Remove("external")
	if mb.writes.Load() != 0 {
		t.Fatal("FTW wrote while external clients owned control")
	}
	metrics := emittedHooks(tel)
	for _, key := range []string{"command_invoked", "default_invoked", "cleanup_invoked"} {
		if metrics["external:"+key] {
			t.Fatalf("external owner ran %s", key)
		}
	}
}
