package main

import (
	"context"
	"fmt"
	"time"

	"github.com/simonvetter/modbus"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Compile-time checks: both types must satisfy the Tool interface.
var _ Tool = (*ModbusProbeTool)(nil)
var _ Tool = (*ModbusWriteTool)(nil)

// ModbusProbeTool reads one or more holding registers from a Modbus TCP device.
// Each call opens a fresh client connection, performs the read, and closes.
type ModbusProbeTool struct{}

func NewModbusProbeTool() *ModbusProbeTool { return &ModbusProbeTool{} }

func (t *ModbusProbeTool) Name() string { return "modbus_probe" }

func (t *ModbusProbeTool) Schema() *mcpsdk.Tool {
	return &mcpsdk.Tool{
		Name:        "modbus_probe",
		Description: "Read one or more holding registers from a Modbus TCP device. Returns the raw uint16 register values.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"host":     map[string]any{"type": "string", "description": "IP address or hostname of the Modbus TCP device"},
				"port":     map[string]any{"type": "integer", "description": "TCP port (default 502)"},
				"unit_id":  map[string]any{"type": "integer", "description": "Modbus unit/slave ID (1–247)"},
				"register": map[string]any{"type": "integer", "description": "Starting register address (0-based)"},
				"count":    map[string]any{"type": "integer", "description": "Number of registers to read (1–125)"},
			},
			"required": []string{"host", "port", "unit_id", "register", "count"},
		},
	}
}

func (t *ModbusProbeTool) Handle(_ context.Context, args map[string]any) (any, error) {
	host, _ := args["host"].(string)
	port := int(args["port"].(float64))
	unitID := uint8(args["unit_id"].(float64))
	reg := uint16(args["register"].(float64))
	count := uint16(args["count"].(float64))

	if host == "" {
		return nil, fmt.Errorf("host is required")
	}

	c, err := modbus.NewClient(&modbus.ClientConfiguration{
		URL:     fmt.Sprintf("tcp://%s:%d", host, port),
		Timeout: 5 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("modbus_probe: new client: %w", err)
	}
	if err := c.Open(); err != nil {
		return nil, fmt.Errorf("modbus_probe: open: %w", err)
	}
	defer c.Close()

	if err := c.SetUnitId(unitID); err != nil {
		return nil, fmt.Errorf("modbus_probe: set unit id: %w", err)
	}

	values, err := c.ReadRegisters(reg, count, modbus.HOLDING_REGISTER)
	if err != nil {
		return nil, fmt.Errorf("modbus_probe: read registers: %w", err)
	}

	return map[string]any{"values": values}, nil
}

// ModbusWriteTool writes a single holding register to a Modbus TCP device.
// Each call opens a fresh client connection, performs the write, and closes.
type ModbusWriteTool struct{}

func NewModbusWriteTool() *ModbusWriteTool { return &ModbusWriteTool{} }

func (t *ModbusWriteTool) Name() string { return "modbus_write" }

func (t *ModbusWriteTool) Schema() *mcpsdk.Tool {
	return &mcpsdk.Tool{
		Name:        "modbus_write",
		Description: "Write a single holding register to a Modbus TCP device.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"host":     map[string]any{"type": "string", "description": "IP address or hostname of the Modbus TCP device"},
				"port":     map[string]any{"type": "integer", "description": "TCP port (default 502)"},
				"unit_id":  map[string]any{"type": "integer", "description": "Modbus unit/slave ID (1–247)"},
				"register": map[string]any{"type": "integer", "description": "Register address (0-based)"},
				"value":    map[string]any{"type": "integer", "description": "Value to write (0–65535)"},
			},
			"required": []string{"host", "port", "unit_id", "register", "value"},
		},
	}
}

func (t *ModbusWriteTool) Handle(_ context.Context, args map[string]any) (any, error) {
	host, _ := args["host"].(string)
	port := int(args["port"].(float64))
	unitID := uint8(args["unit_id"].(float64))
	reg := uint16(args["register"].(float64))
	value := uint16(args["value"].(float64))

	if host == "" {
		return nil, fmt.Errorf("host is required")
	}

	c, err := modbus.NewClient(&modbus.ClientConfiguration{
		URL:     fmt.Sprintf("tcp://%s:%d", host, port),
		Timeout: 5 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("modbus_write: new client: %w", err)
	}
	if err := c.Open(); err != nil {
		return nil, fmt.Errorf("modbus_write: open: %w", err)
	}
	defer c.Close()

	if err := c.SetUnitId(unitID); err != nil {
		return nil, fmt.Errorf("modbus_write: set unit id: %w", err)
	}

	if err := c.WriteRegister(reg, value); err != nil {
		return nil, fmt.Errorf("modbus_write: write register: %w", err)
	}

	return map[string]any{"ok": true}, nil
}
