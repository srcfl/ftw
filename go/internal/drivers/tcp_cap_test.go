package drivers

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// ---- TCPCap unit tests -----------------------------------------------------

func TestTCPCap_OpenRecvClose(t *testing.T) {
	// Stand up a local TCP server that pushes a known byte payload and
	// blocks until the client closes — exercises both the buffering path
	// in netTCP and the Close → readPump unwind.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	payload := []byte("HELLO P1 TELEGRAM\r\n!ABCD\r\n")
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = c.Write(payload)
		// Block; the test closes the cap which closes the conn and unblocks.
		buf := make([]byte, 16)
		_, _ = c.Read(buf)
		_ = c.Close()
	}()

	cap := NewNetTCP("test", nil)
	if err := cap.Open(ln.Addr().String()); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer cap.Close()

	// Give the read pump a beat to drain the payload.
	deadline := time.Now().Add(2 * time.Second)
	var got []byte
	for time.Now().Before(deadline) {
		got = append(got, cap.PopBytes()...)
		if len(got) >= len(payload) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if string(got) != string(payload) {
		t.Errorf("payload mismatch: got %q want %q", got, payload)
	}
	if !cap.IsOpen() {
		t.Error("expected IsOpen() to be true while server is up")
	}
}

func TestTCPCap_AllowedHosts(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	cases := []struct {
		name    string
		allowed []string
		addr    string
		wantErr bool
	}{
		{"empty allowlist = any host", nil, ln.Addr().String(), false},
		{"bare host match", []string{"127.0.0.1"}, ln.Addr().String(), false},
		{"host:port match", []string{"127.0.0.1:" + port}, ln.Addr().String(), false},
		{"host:port mismatch", []string{"127.0.0.1:1"}, ln.Addr().String(), true},
		{"different host blocked", []string{"10.0.0.1"}, ln.Addr().String(), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cap := NewNetTCP("test", tc.allowed)
			err := cap.Open(tc.addr)
			defer cap.Close()
			if tc.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// ---- Driver-level test: feed a canned DSMR telegram through host.tcp_recv

// fakeTCPCap is a TCPCap that returns a pre-loaded byte payload once and
// then reports EOF. Lets us exercise the Lua driver's framing + parser
// without standing up a network listener.
type fakeTCPCap struct {
	bytes  []byte
	opened bool
	closed bool
}

func (f *fakeTCPCap) Open(addr string) error { f.opened = true; return nil }
func (f *fakeTCPCap) PopBytes() []byte {
	out := f.bytes
	f.bytes = nil
	return out
}
func (f *fakeTCPCap) IsOpen() bool { return f.opened && !f.closed }
func (f *fakeTCPCap) Close() error { f.closed = true; return nil }

// Synthetic DSMR 5.0 telegram body. Values chosen so we can pin every emit:
//   import 1.234 kW, export 0.500 kW   → meter.w = +734 W
//   per-phase voltages 230.1 / 230.2 / 230.3 V
//   per-phase currents 5 / 3 / 7 A
//   import T1 100.000 kWh + T2 200.000 kWh → import_wh = 300_000
//   export T1 10.000 kWh + T2 20.000 kWh → export_wh = 30_000
// CRC is computed at runtime via dsmrCRC16; tests build the full telegram
// with dsmrWrap() so they exercise the same CRC path the live meter does.
const dsmrBody = "/XMX5LGBBFFB231215493\r\n" +
	"\r\n" +
	"1-3:0.2.8(50)\r\n" +
	"0-0:1.0.0(241015095505S)\r\n" +
	"0-0:96.1.1(4530303834303031383239353439393137)\r\n" +
	"1-0:1.8.1(00100.000*kWh)\r\n" +
	"1-0:1.8.2(00200.000*kWh)\r\n" +
	"1-0:2.8.1(00010.000*kWh)\r\n" +
	"1-0:2.8.2(00020.000*kWh)\r\n" +
	"0-0:96.14.0(0002)\r\n" +
	"1-0:1.7.0(01.234*kW)\r\n" +
	"1-0:2.7.0(00.500*kW)\r\n" +
	"0-0:96.7.21(00012)\r\n" +
	"0-0:96.7.9(00003)\r\n" +
	"1-0:32.7.0(230.1*V)\r\n" +
	"1-0:52.7.0(230.2*V)\r\n" +
	"1-0:72.7.0(230.3*V)\r\n" +
	"1-0:31.7.0(005*A)\r\n" +
	"1-0:51.7.0(003*A)\r\n" +
	"1-0:71.7.0(007*A)\r\n" +
	"1-0:21.7.0(00.500*kW)\r\n" +
	"1-0:41.7.0(00.400*kW)\r\n" +
	"1-0:61.7.0(00.334*kW)\r\n" +
	"1-0:22.7.0(00.000*kW)\r\n" +
	"1-0:42.7.0(00.000*kW)\r\n" +
	"1-0:62.7.0(00.500*kW)\r\n" +
	"0-1:24.1.0(003)\r\n" +
	"0-1:96.1.0(4730303132393336353031383039333137)\r\n" +
	"0-1:24.2.1(241015095500S)(00123.456*m3)\r\n"

// dsmrCRC16 computes the CRC16-IBM (poly 0xA001, init 0x0000) used by
// DSMR 5 — same algorithm as the driver's Lua implementation. Kept here
// as a parallel reference so the test can assert end-to-end correctness
// without invoking the Lua VM.
func dsmrCRC16(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		crc ^= uint16(b)
		for i := 0; i < 8; i++ {
			if crc&1 == 1 {
				crc = (crc >> 1) ^ 0xA001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

// dsmrWrap appends '!' + CRC hex + CRLF onto a DSMR body. crcOverride,
// when non-empty, replaces the computed CRC — useful for the bad-CRC
// test. Use "" to get a real, valid trailer.
func dsmrWrap(body, crcOverride string) string {
	if crcOverride == "" {
		crc := dsmrCRC16([]byte(body + "!"))
		return fmt.Sprintf("%s!%04X\r\n", body, crc)
	}
	return fmt.Sprintf("%s!%s\r\n", body, crcOverride)
}

func TestZuidwijkP1Driver_ParsesTelegram(t *testing.T) {
	// Use the real driver file from the repo.
	driverPath := repoDriverPath(t, "zuidwijk_p1.lua")

	tel := telemetry.NewStore()
	env := NewHostEnv("zuidwijk-p1", tel)
	env.WithTCP(&fakeTCPCap{bytes: []byte(dsmrWrap(dsmrBody, ""))})

	d, err := NewLuaDriver(driverPath, env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer d.Cleanup()

	if err := d.Init(context.Background(), map[string]any{"host": "127.0.0.1", "port": 23}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	meter := tel.Get("zuidwijk-p1", telemetry.DerMeter)
	if meter == nil {
		t.Fatal("no meter reading")
	}
	// 1.234 - 0.500 = 0.734 kW = 734 W
	if !nearly(meter.RawW, 734, 0.5) {
		t.Errorf("meter.w: got %v want ~734", meter.RawW)
	}

	mk, sn := env.Identity()
	if mk != "Zuidwijk" {
		t.Errorf("make: %q", mk)
	}
	if sn != "E0084001829549917" {
		t.Errorf("sn: got %q want E0084001829549917", sn)
	}
}

func TestZuidwijkP1Driver_TakesLatestFrameWhenBuffered(t *testing.T) {
	driverPath := repoDriverPath(t, "zuidwijk_p1.lua")

	// Two back-to-back telegrams, differing only in active power. The
	// driver must commit the *latest* one because that's the freshest
	// snapshot of meter state. Both wrapped with valid CRCs so neither
	// gets dropped by the CRC gate.
	staleBody := strings.Replace(dsmrBody, "1-0:1.7.0(01.234*kW)", "1-0:1.7.0(00.100*kW)", 1)
	staleBody = strings.Replace(staleBody, "1-0:2.7.0(00.500*kW)", "1-0:2.7.0(00.000*kW)", 1)
	combined := dsmrWrap(staleBody, "") + dsmrWrap(dsmrBody, "")

	tel := telemetry.NewStore()
	env := NewHostEnv("zuidwijk-p1", tel)
	env.WithTCP(&fakeTCPCap{bytes: []byte(combined)})

	d, err := NewLuaDriver(driverPath, env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer d.Cleanup()
	if err := d.Init(context.Background(), map[string]any{"host": "127.0.0.1"}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	meter := tel.Get("zuidwijk-p1", telemetry.DerMeter)
	if meter == nil {
		t.Fatal("no meter reading")
	}
	if !nearly(meter.RawW, 734, 0.5) {
		t.Errorf("expected the LATER frame (~734 W), got %v", meter.RawW)
	}
}

func TestZuidwijkP1Driver_RejectsBadCRC(t *testing.T) {
	// A telegram with the right body but a deliberately wrong CRC. The
	// driver must drop it and produce no meter reading at all — the
	// dispatch loop reading a corrupt grid value would chase a phantom
	// setpoint until the watchdog catches the stall.
	driverPath := repoDriverPath(t, "zuidwijk_p1.lua")

	tel := telemetry.NewStore()
	env := NewHostEnv("zuidwijk-p1", tel)
	env.WithTCP(&fakeTCPCap{bytes: []byte(dsmrWrap(dsmrBody, "DEAD"))})

	d, err := NewLuaDriver(driverPath, env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer d.Cleanup()
	if err := d.Init(context.Background(), map[string]any{"host": "127.0.0.1"}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if got := tel.Get("zuidwijk-p1", telemetry.DerMeter); got != nil {
		t.Errorf("bad-CRC frame should produce no meter reading, got %+v", got)
	}
}

func TestZuidwijkP1Driver_AcceptsLegacyNoCRC(t *testing.T) {
	// DSMR 4 firmware and pure passthrough proxies emit '!\r\n' with no
	// CRC. The driver must accept these (better unverified than starved)
	// while logging a metric for visibility.
	driverPath := repoDriverPath(t, "zuidwijk_p1.lua")

	tel := telemetry.NewStore()
	env := NewHostEnv("zuidwijk-p1", tel)
	env.WithTCP(&fakeTCPCap{bytes: []byte(dsmrBody + "!\r\n")})

	d, err := NewLuaDriver(driverPath, env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer d.Cleanup()
	if err := d.Init(context.Background(), map[string]any{"host": "127.0.0.1"}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	meter := tel.Get("zuidwijk-p1", telemetry.DerMeter)
	if meter == nil {
		t.Fatal("legacy no-CRC frame should still be accepted")
	}
	if !nearly(meter.RawW, 734, 0.5) {
		t.Errorf("meter.w: got %v want ~734", meter.RawW)
	}
}

// repoDriverPath returns the absolute path to a driver file in the repo's
// drivers/ directory. Walks up from the test's working directory until it
// finds a `drivers/` sibling containing `name`.
func repoDriverPath(t *testing.T, name string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for dir := wd; dir != "/" && dir != ""; dir = filepath.Dir(dir) {
		candidate := filepath.Join(dir, "drivers", name)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	t.Fatalf("could not locate drivers/%s starting from %s", name, wd)
	return ""
}

func nearly(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= eps
}

