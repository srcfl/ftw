// sim-sungrow: Modbus TCP server simulating a Sungrow SH-series hybrid inverter.
//
// Serves the SH10RT register map on :5502 (configurable). Reads return live
// values from a physics simulation; writes to 13049/13050/13051/33046/33047
// drive the simulator's battery behavior with first-order response lag.
//
// Run:    go run ./cmd/sim-sungrow
// Debug:  modpoll -m tcp -a 1 -r 13019 -c 4 -p 5502 localhost
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/simonvetter/modbus"

	"github.com/srcfl/ftw/go/cmd/sim-sungrow/sungrow"
)

func main() {
	addr := flag.String("addr", "tcp://0.0.0.0:5502", "Modbus TCP bind URL")
	tick := flag.Duration("tick", 500*time.Millisecond, "physics + register refresh interval")
	initSoC := flag.Float64("soc", 0.5, "starting SoC 0..1")
	capWh := flag.Float64("capacity-wh", 9600, "battery capacity Wh")
	pvPeak := flag.Float64("pv-peak", 0, "override PV power (constant W); 0 = time-of-day curve")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg := sungrow.Default()
	cfg.SoC = *initSoC
	cfg.CapacityWh = *capWh
	cfg.PVPeakW = *pvPeak
	sim := sungrow.New(cfg)
	bank := sungrow.NewRegisterBank(sim)
	// Prime the register bank so reads before first tick don't return all zeros
	bank.Refresh(sim.Tick(time.Millisecond))

	handler := &modbusHandler{bank: bank}

	srv, err := modbus.NewServer(&modbus.ServerConfiguration{
		URL:        *addr,
		Timeout:    10 * time.Second,
		MaxClients: 8,
	}, handler)
	if err != nil {
		slog.Error("failed to create Modbus server", "err", err)
		os.Exit(1)
	}
	if err := srv.Start(); err != nil {
		slog.Error("failed to start Modbus server", "err", err)
		os.Exit(1)
	}
	defer srv.Stop()
	slog.Info("Modbus TCP server listening", "addr", *addr)

	// Physics tick goroutine
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(*tick)
		defer t.Stop()
		last := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				dt := now.Sub(last)
				last = now
				bank.Refresh(sim.Tick(dt))
			}
		}
	}()

	// Graceful shutdown
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	<-sigc
	slog.Info("shutdown")
	cancel()
	wg.Wait()
}

// modbusHandler adapts our RegisterBank to simonvetter/modbus's RequestHandler.
type modbusHandler struct {
	bank *sungrow.RegisterBank
}

func (h *modbusHandler) HandleCoils(_ *modbus.CoilsRequest) ([]bool, error) {
	return nil, modbus.ErrIllegalFunction
}

func (h *modbusHandler) HandleDiscreteInputs(_ *modbus.DiscreteInputsRequest) ([]bool, error) {
	return nil, modbus.ErrIllegalFunction
}

func (h *modbusHandler) HandleHoldingRegisters(req *modbus.HoldingRegistersRequest) ([]uint16, error) {
	if req.IsWrite {
		if err := h.bank.WriteHolding(req.Addr, req.Args); err != nil {
			return nil, err
		}
		slog.Info("write holding", "addr", req.Addr, "values", req.Args)
		return nil, nil
	}
	return h.bank.ReadHolding(req.Addr, req.Quantity), nil
}

func (h *modbusHandler) HandleInputRegisters(req *modbus.InputRegistersRequest) ([]uint16, error) {
	return h.bank.ReadInput(req.Addr, req.Quantity), nil
}
