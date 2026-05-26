package main

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/simonvetter/modbus"
)

type stubHandler struct {
	regs []uint16
}

func (h *stubHandler) HandleHoldingRegisters(req *modbus.HoldingRegistersRequest) ([]uint16, error) {
	out := make([]uint16, req.Quantity)
	for i := range out {
		idx := int(req.Addr) + i
		if idx < len(h.regs) {
			out[i] = h.regs[idx]
		}
		if req.IsWrite && i < len(req.Args) {
			h.regs[idx] = req.Args[i]
		}
	}
	return out, nil
}
func (h *stubHandler) HandleCoils(*modbus.CoilsRequest) ([]bool, error) {
	return nil, nil
}
func (h *stubHandler) HandleDiscreteInputs(*modbus.DiscreteInputsRequest) ([]bool, error) {
	return nil, nil
}
func (h *stubHandler) HandleInputRegisters(*modbus.InputRegistersRequest) ([]uint16, error) {
	return nil, nil
}

func startTestModbus(t *testing.T) string {
	t.Helper()
	h := &stubHandler{regs: []uint16{100, 200, 300, 400}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	srv, err := modbus.NewServer(&modbus.ServerConfiguration{
		URL: "tcp://127.0.0.1:" + strconv.Itoa(port),
	}, h)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	t.Cleanup(func() { srv.Stop() })
	// Brief pause to let the server goroutine bind its listener.
	time.Sleep(100 * time.Millisecond)
	return "127.0.0.1:" + strconv.Itoa(port)
}

func TestModbusProbeRead(t *testing.T) {
	addr := startTestModbus(t)
	host, port, _ := net.SplitHostPort(addr)
	p, _ := strconv.Atoi(port)
	tool := NewModbusProbeTool()
	out, err := tool.Handle(context.Background(), map[string]any{
		"host":     host,
		"port":     float64(p),
		"unit_id":  float64(1),
		"register": float64(0),
		"count":    float64(4),
	})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	vals := out.(map[string]any)["values"].([]uint16)
	if len(vals) != 4 || vals[0] != 100 {
		t.Fatalf("unexpected vals: %v", vals)
	}
}

func TestModbusWriteRegister(t *testing.T) {
	addr := startTestModbus(t)
	host, port, _ := net.SplitHostPort(addr)
	p, _ := strconv.Atoi(port)

	writeTool := NewModbusWriteTool()
	out, err := writeTool.Handle(context.Background(), map[string]any{
		"host":     host,
		"port":     float64(p),
		"unit_id":  float64(1),
		"register": float64(2),
		"value":    float64(999),
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if out.(map[string]any)["ok"] != true {
		t.Fatalf("expected ok=true, got: %v", out)
	}

	// Read back the written value.
	probeTool := NewModbusProbeTool()
	readOut, err := probeTool.Handle(context.Background(), map[string]any{
		"host":     host,
		"port":     float64(p),
		"unit_id":  float64(1),
		"register": float64(2),
		"count":    float64(1),
	})
	if err != nil {
		t.Fatalf("probe after write: %v", err)
	}
	vals := readOut.(map[string]any)["values"].([]uint16)
	if len(vals) != 1 || vals[0] != 999 {
		t.Fatalf("expected [999] after write, got %v", vals)
	}
}
