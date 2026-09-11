package modbus

import (
	"io"
	"net"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/drivers"
)

func TestProxyMultiplexesOntoSharedSession(t *testing.T) {
	slave := startTestSlave(t)
	host, port := slave.Addr()
	engine := NewEngine()

	driver, err := engine.Open(host, port, 1, false)
	if err != nil {
		t.Fatalf("driver open: %v", err)
	}
	defer driver.Close()

	proxy, err := engine.Listen([]Bind{{
		Listen: "127.0.0.1:0",
		Host:   host,
		Port:   port,
	}}, false)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer proxy.Close()
	addrs := proxy.ListenAddrs()
	if len(addrs) != 1 {
		t.Fatalf("listen addrs = %v", addrs)
	}
	pHost, pPort := mustAddr(t, addrs[0])

	client, err := Dial(pHost, pPort, 1)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer client.Close()

	regs, err := client.Read(1, 2, drivers.ModbusHolding)
	if err != nil {
		t.Fatalf("proxy read: %v", err)
	}
	if len(regs) != 2 || regs[0] != 0x1111 || regs[1] != 0x2222 {
		t.Fatalf("proxy read = %v, want [0x1111 0x2222]", regs)
	}

	if _, err := driver.Read(1, 1, drivers.ModbusInput); err != nil {
		t.Fatalf("driver read while proxy is up: %v", err)
	}
	if got := slave.Accepts(); got != 1 {
		t.Fatalf("backend accepts = %d, want 1 (driver+proxy share)", got)
	}

	err = client.WriteSingle(10, 0xBEEF)
	if err == nil {
		t.Fatal("proxy write succeeded; want read-only exception")
	}
	if !strings.Contains(err.Error(), "code=0x01") {
		t.Fatalf("write error = %v, want illegal-function exception", err)
	}
	if slave.Holding(10) != 0 {
		t.Fatalf("backend holding[10] = %v, want unchanged 0", slave.Holding(10))
	}
	if slave.Writes() != 0 {
		t.Fatalf("backend writes = %d, want 0", slave.Writes())
	}
}

func TestProxyAllowWriteForwardsRegisterWrites(t *testing.T) {
	slave := startTestSlave(t)
	host, port := slave.Addr()
	engine := NewEngine()
	proxy, err := engine.Listen([]Bind{{
		Listen: "127.0.0.1:0",
		Host:   host,
		Port:   port,
	}}, true)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer proxy.Close()
	pHost, pPort := mustAddr(t, proxy.ListenAddrs()[0])

	client, err := Dial(pHost, pPort, 1)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer client.Close()
	if err := client.WriteSingle(10, 0xBEEF); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := slave.Holding(10); got != 0xBEEF {
		t.Fatalf("holding[10] = 0x%04x, want 0xBEEF", got)
	}
}

func TestProxyUnknownFunctionIsIllegal(t *testing.T) {
	slave := startTestSlave(t)
	host, port := slave.Addr()
	engine := NewEngine()
	proxy, err := engine.Listen([]Bind{{
		Listen: "127.0.0.1:0",
		Host:   host,
		Port:   port,
	}}, false)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer proxy.Close()

	conn, err := net.Dial("tcp", proxy.ListenAddrs()[0])
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	// FC 0x2B (Encapsulated Interface Transport) is not on the read/write list.
	req := []byte{0, 1, 0, 0, 0, 2, 1, 0x2B}
	if _, err := conn.Write(req); err != nil {
		t.Fatalf("write: %v", err)
	}
	hdr := make([]byte, 9)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		t.Fatalf("read: %v", err)
	}
	if hdr[7] != 0x2B|0x80 || hdr[8] != modbusExcIllegalFn {
		t.Fatalf("response = %x, want exception illegal function", hdr)
	}
}

func mustAddr(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	var port int
	if _, err := fmtSscan(portStr, &port); err != nil {
		t.Fatalf("port %q: %v", portStr, err)
	}
	return host, port
}
