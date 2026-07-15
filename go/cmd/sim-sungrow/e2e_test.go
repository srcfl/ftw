package main

import (
	"fmt"
	"math"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/simonvetter/modbus"

	"github.com/srcfl/ftw/go/cmd/sim-sungrow/sungrow"
)

func pickFreePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

// startSim spins up the Modbus server + physics ticker and returns a stop func.
func startSim(t *testing.T, cfg sungrow.Config) (*sungrow.Simulator, string, func()) {
	t.Helper()
	port := pickFreePort(t)
	sim := sungrow.New(cfg)
	bank := sungrow.NewRegisterBank(sim)
	bank.Refresh(sim.Tick(time.Millisecond))

	srv, err := modbus.NewServer(&modbus.ServerConfiguration{
		URL:        "tcp://127.0.0.1:" + port,
		Timeout:    5 * time.Second,
		MaxClients: 4,
	}, &modbusHandler{bank: bank})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}

	stopCh := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tk := time.NewTicker(50 * time.Millisecond)
		defer tk.Stop()
		last := time.Now()
		for {
			select {
			case <-stopCh:
				return
			case now := <-tk.C:
				dt := now.Sub(last)
				last = now
				bank.Refresh(sim.Tick(dt))
			}
		}
	}()

	stop := func() {
		close(stopCh)
		wg.Wait()
		srv.Stop()
	}
	// Small wait for server to be ready
	time.Sleep(100 * time.Millisecond)
	return sim, port, stop
}

func newClient(t *testing.T, port string) *modbus.ModbusClient {
	t.Helper()
	cli, err := modbus.NewClient(&modbus.ClientConfiguration{
		URL:     "tcp://127.0.0.1:" + port,
		Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.Open(); err != nil {
		t.Fatal(err)
	}
	return cli
}

func TestE2E_ReadDeviceMetadata(t *testing.T) {
	cfg := sungrow.Default()
	cfg.SerialNumber = "SH10RT-TEST-0001"
	cfg.DeviceType = 3598
	_, port, stop := startSim(t, cfg)
	defer stop()

	cli := newClient(t, port)
	defer cli.Close()

	// Read 4990-4999 (serial + device type)
	regs, err := cli.ReadRegisters(4990, 10, modbus.INPUT_REGISTER)
	if err != nil {
		t.Fatal(err)
	}
	// Decode first 2 chars from reg 4990
	hi := byte(regs[0] >> 8)
	lo := byte(regs[0] & 0xFF)
	if hi != 'S' || lo != 'H' {
		t.Errorf("expected 'SH' at reg 4990, got %c%c", hi, lo)
	}
	// reg 4999 = device type
	if regs[9] != 3598 {
		t.Errorf("device type: expected 3598, got %d", regs[9])
	}
	t.Logf("serial reg 4990 = 0x%04x ('%c%c'), device type = %d", regs[0], hi, lo, regs[9])
}

func TestE2E_ReadBatteryBlock(t *testing.T) {
	cfg := sungrow.Default()
	cfg.SoC = 0.42
	sim, port, stop := startSim(t, cfg)
	defer stop()

	cli := newClient(t, port)
	defer cli.Close()

	// Let physics settle
	time.Sleep(200 * time.Millisecond)
	_ = sim // keep reference

	regs, err := cli.ReadRegisters(13019, 4, modbus.INPUT_REGISTER)
	if err != nil {
		t.Fatal(err)
	}
	batV := float64(regs[0]) * 0.1
	batA := float64(regs[1]) * 0.1
	batW := int(regs[2])
	batSoC := float64(regs[3]) * 0.001

	t.Logf("battery: V=%.1f A=%.1f W=%d SoC=%.3f", batV, batA, batW, batSoC)
	if math.Abs(batV-48.0) > 1 {
		t.Errorf("battery voltage expected 48V, got %.1f", batV)
	}
	if math.Abs(batSoC-0.42) > 0.01 {
		t.Errorf("SoC expected 0.42, got %.3f", batSoC)
	}
}

func TestE2E_WriteChargeCommandMovesActualPower(t *testing.T) {
	cfg := sungrow.Default()
	cfg.ResponseTauS = 0.2 // fast for test
	_, port, stop := startSim(t, cfg)
	defer stop()

	cli := newClient(t, port)
	defer cli.Close()

	// Issue a force-charge 2000W: write power, then cmd, then mode
	if err := cli.WriteRegister(13051, 2000); err != nil {
		t.Fatal(err)
	}
	if err := cli.WriteRegister(13050, 0xAA); err != nil {
		t.Fatal(err)
	}
	if err := cli.WriteRegister(13049, 2); err != nil {
		t.Fatal(err)
	}

	// Wait for physics to converge
	time.Sleep(1500 * time.Millisecond)

	// Read battery power (reg 13021, unsigned magnitude) + status bit (reg 13000)
	regs, err := cli.ReadRegisters(13000, 1, modbus.INPUT_REGISTER)
	if err != nil {
		t.Fatal(err)
	}
	status := regs[0]
	// Expect bit 1 (0x0002) = charging
	if status&0x0002 == 0 {
		t.Errorf("expected charging bit in status 0x%04x", status)
	}

	pwrRegs, _ := cli.ReadRegisters(13021, 1, modbus.INPUT_REGISTER)
	batAbsW := pwrRegs[0]
	t.Logf("after charge cmd: status=0x%04x bat|W|=%d", status, batAbsW)
	if batAbsW < 1000 {
		t.Errorf("expected charging magnitude > 1000W, got %d", batAbsW)
	}
}

func TestE2E_WriteDischargeCommand(t *testing.T) {
	cfg := sungrow.Default()
	cfg.ResponseTauS = 0.2
	_, port, stop := startSim(t, cfg)
	defer stop()

	cli := newClient(t, port)
	defer cli.Close()

	_ = cli.WriteRegister(13051, 1200)
	_ = cli.WriteRegister(13050, 0xBB)
	_ = cli.WriteRegister(13049, 2)

	time.Sleep(1500 * time.Millisecond)

	regs, _ := cli.ReadRegisters(13000, 1, modbus.INPUT_REGISTER)
	status := regs[0]
	// Expect bit 2 (0x0004) = discharging
	if status&0x0004 == 0 {
		t.Errorf("expected discharging bit in status 0x%04x", status)
	}
}

func TestE2E_GridMeterI32LE(t *testing.T) {
	cfg := sungrow.Default()
	cfg.HouseJitterW = 0
	cfg.HouseBaseW = 1200
	cfg.PVPeakW = 100 // constant, low
	_, port, stop := startSim(t, cfg)
	defer stop()

	cli := newClient(t, port)
	defer cli.Close()

	time.Sleep(200 * time.Millisecond)

	// Read 5600-5601 (I32 LE grid meter)
	regs, err := cli.ReadRegisters(5600, 2, modbus.INPUT_REGISTER)
	if err != nil {
		t.Fatal(err)
	}
	low, high := regs[0], regs[1]
	combined := int32(uint32(high)<<16 | uint32(low))
	t.Logf("grid meter I32 LE (5600-5601): low=0x%04x high=0x%04x → %d W", low, high, combined)
	// With load 1200W, PV 100W, no battery → grid ≈ 1100W import
	// Allow generous tolerance for house-load drift
	if combined < 900 || combined > 1300 {
		t.Errorf("grid meter expected ~1100W, got %d", combined)
	}
}

func TestE2E_WriteMaxChargeLimit(t *testing.T) {
	_, port, stop := startSim(t, sungrow.Default())
	defer stop()

	cli := newClient(t, port)
	defer cli.Close()

	// Write 33046 = 100 (×10W = 1000W)
	if err := cli.WriteRegister(33046, 100); err != nil {
		t.Fatal(err)
	}
	// Read back via holding 33046
	regs, err := cli.ReadRegisters(33046, 1, modbus.HOLDING_REGISTER)
	if err != nil {
		t.Fatal(err)
	}
	if regs[0] != 100 {
		t.Errorf("max_charge readback: expected 100, got %d", regs[0])
	}
}

func TestE2E_ReadPVTopology(t *testing.T) {
	cfg := sungrow.Default()
	cfg.PVPeakW = 3000
	_, port, stop := startSim(t, cfg)
	defer stop()

	cli := newClient(t, port)
	defer cli.Close()

	time.Sleep(200 * time.Millisecond)

	// Read PV power (5016-5017 U32 LE)
	regs, err := cli.ReadRegisters(5016, 2, modbus.INPUT_REGISTER)
	if err != nil {
		t.Fatal(err)
	}
	pv := uint32(regs[1])<<16 | uint32(regs[0])
	t.Logf("PV U32 LE (5016-5017): %d W", pv)
	if pv < 2800 || pv > 3200 {
		t.Errorf("expected PV ~3000W, got %d", pv)
	}
}

// Sanity: the sample register dump in test log shows exactly what the Lua
// driver is about to read. If it looks wrong to a human, it's probably wrong.
func TestE2E_SampleRegisterDump(t *testing.T) {
	cfg := sungrow.Default()
	cfg.ResponseTauS = 0.2
	cfg.SoC = 0.5
	_, port, stop := startSim(t, cfg)
	defer stop()

	cli := newClient(t, port)
	defer cli.Close()

	// Drive a mid-size discharge
	_ = cli.WriteRegister(13051, 800)
	_ = cli.WriteRegister(13050, 0xBB)
	_ = cli.WriteRegister(13049, 2)
	time.Sleep(1500 * time.Millisecond)

	// Read the whole telemetry block used by the Lua driver
	sections := []struct {
		addr, n uint16
		name    string
	}{
		{4990, 10, "serial+devtype"},
		{5000, 1, "rated_power"},
		{5007, 1, "heatsink_temp"},
		{5016, 2, "pv_power U32LE"},
		{5241, 1, "grid_freq"},
		{5600, 2, "grid_meter I32LE"},
		{13000, 1, "status_bits"},
		{13019, 4, "battery V/A/W/SoC"},
	}
	for _, s := range sections {
		regs, err := cli.ReadRegisters(s.addr, s.n, modbus.INPUT_REGISTER)
		if err != nil {
			t.Logf("  %s (reg %d): ERROR %v", s.name, s.addr, err)
			continue
		}
		t.Logf("  %-30s @ %5d → %s", s.name, s.addr, fmtRegs(regs))
	}
}

func fmtRegs(regs []uint16) string {
	s := "["
	for i, r := range regs {
		if i > 0 {
			s += " "
		}
		s += fmt.Sprintf("0x%04x(%d)", r, r)
	}
	return s + "]"
}
