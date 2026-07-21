package drivers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

type readResultModbus struct {
	registers       []uint16
	err             error
	errorsByAddress map[uint16]error
}

func (m readResultModbus) Read(address uint16, _ uint16, _ int32) ([]uint16, error) {
	if err := m.errorsByAddress[address]; err != nil {
		return nil, err
	}
	return m.registers, m.err
}
func (readResultModbus) WriteSingle(uint16, uint16) error  { return nil }
func (readResultModbus) WriteMulti(uint16, []uint16) error { return nil }
func (readResultModbus) Close() error                      { return nil }

func TestModbusDriverDoesNotEmitWhenEveryReadFails(t *testing.T) {
	tel := telemetry.NewStore()
	driver := newModbusEvidenceDriver(t, tel, readResultModbus{err: errors.New("modbus: i/o timeout")})

	_, err := driver.Poll(context.Background())
	if err == nil || !strings.Contains(err.Error(), "2 of 2 modbus reads failed") {
		t.Fatalf("Poll error = %v, want failed-read error", err)
	}
	if health := tel.DriverHealth("modbus-evidence"); health != nil && health.LastSuccess != nil {
		t.Fatalf("failed read advanced LastSuccess: %+v", health)
	}
	if readings := tel.ReadingsByDriver("modbus-evidence"); len(readings) != 0 {
		t.Fatalf("failed read stored %d readings", len(readings))
	}
}

func TestModbusDriverDoesNotEmitWhenOneReadInStreamFails(t *testing.T) {
	tel := telemetry.NewStore()
	driver := newModbusEvidenceDriver(t, tel, readResultModbus{
		registers:       []uint16{321},
		errorsByAddress: map[uint16]error{11: errors.New("optional register timeout")},
	})

	if _, err := driver.Poll(context.Background()); err == nil || !strings.Contains(err.Error(), "1 of 2 modbus reads failed") {
		t.Fatalf("Poll error = %v, want partial-read error", err)
	}
	if readings := tel.ReadingsByDriver("modbus-evidence"); len(readings) != 0 {
		t.Fatalf("partial read failure stored %d readings", len(readings))
	}
	if health := tel.DriverHealth("modbus-evidence"); health != nil && health.LastSuccess != nil {
		t.Fatalf("partial read failure advanced LastSuccess: %+v", health)
	}
}

func TestModbusDriverEmitsAfterSuccessfulRead(t *testing.T) {
	tel := telemetry.NewStore()
	driver := newModbusEvidenceDriver(t, tel, readResultModbus{registers: []uint16{321}})

	if _, err := driver.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	reading := tel.Get("modbus-evidence", telemetry.DerMeter)
	if reading == nil || reading.RawW != 321 {
		t.Fatalf("meter reading = %+v, want 321 W", reading)
	}
	if health := tel.DriverHealth("modbus-evidence"); health == nil || health.LastSuccess == nil {
		t.Fatalf("successful read did not advance LastSuccess: %+v", health)
	}
}

func newModbusEvidenceDriver(t *testing.T, tel *telemetry.Store, modbus ModbusCap) *LuaDriver {
	t.Helper()
	source := `
function driver_init() end
function driver_poll()
    local ok, registers = pcall(host.modbus_read, 10, 1, "holding")
    local watts = 0
    if ok and registers then watts = registers[1] end
    host.emit("meter", { w = watts })
    pcall(host.modbus_read, 11, 1, "holding")
    host.emit("pv", { w = watts })
    return 1000
end
`
	path := filepath.Join(t.TempDir(), "modbus_evidence.lua")
	if err := os.WriteFile(path, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	driver, err := NewLuaDriver(path, NewHostEnv("modbus-evidence", tel).WithModbus(modbus))
	if err != nil {
		t.Fatalf("load driver: %v", err)
	}
	t.Cleanup(driver.Cleanup)
	if err := driver.Init(context.Background(), nil); err != nil {
		t.Fatalf("init driver: %v", err)
	}
	return driver
}
