package modbus

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/drivers"
)

func TestProxyWriteOwnershipBlocksAllLocalHandles(t *testing.T) {
	slave := startTestSlave(t)
	host, port := slave.Addr()
	engine := NewEngine()
	driver, err := engine.Open(host, port, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	defer driver.Close()
	proxy, err := engine.Listen([]Bind{{Listen: "127.0.0.1:0", Host: host, Port: port, UnitIDs: []uint8{1, 2}}}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	probe, err := Dial(host, port, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	for _, cap := range []drivers.ModbusCap{driver, probe} {
		if _, err := cap.Read(1, 1, drivers.ModbusHolding); err != nil {
			t.Fatal(err)
		}
		if err := cap.WriteSingle(10, 99); !errors.Is(err, drivers.ErrObserveOnly) {
			t.Fatalf("single write: %v", err)
		}
		if err := cap.WriteMulti(10, []uint16{99, 100}); !errors.Is(err, drivers.ErrObserveOnly) {
			t.Fatalf("multi write: %v", err)
		}
	}
	if slave.Writes() != 0 {
		t.Fatal("a local write reached the device")
	}
	pHost, pPort := mustAddr(t, proxy.ListenAddrs()[0])
	client, err := Dial(pHost, pPort, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.WriteSingle(10, 0xbeef); err != nil {
		t.Fatal(err)
	}
	if slave.Holding(10) != 0xbeef || slave.Accepts() != 1 {
		t.Fatal("external write did not use the one backend session")
	}
	if err := proxy.Close(); err != nil {
		t.Fatal(err)
	}
	if err := driver.WriteSingle(10, 7); !errors.Is(err, drivers.ErrObserveOnly) {
		t.Fatalf("closing proxy returned write ownership: %v", err)
	}
	if _, err := driver.Read(1, 1, drivers.ModbusHolding); err != nil {
		t.Fatal(err)
	}
}

func TestProxyReadOnlyKeepsLocalWriter(t *testing.T) {
	slave := startTestSlave(t)
	host, port := slave.Addr()
	engine := NewEngine()
	proxy, err := engine.Listen([]Bind{{Listen: "127.0.0.1:0", Host: host, Port: port, UnitIDs: []uint8{1}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	driver, err := engine.Open(host, port, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	defer driver.Close()
	if err := driver.WriteSingle(10, 7); err != nil {
		t.Fatal(err)
	}
	if slave.Holding(10) != 7 {
		t.Fatal("FTW lost write access in read-only proxy mode")
	}
}

func TestProxyFailedListenKeepsExternalOwnership(t *testing.T) {
	slave := startTestSlave(t)
	host, port := slave.Addr()
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	engine := NewEngine()
	if _, err := engine.Listen([]Bind{{Listen: occupied.Addr().String(), Host: host, Port: port, UnitIDs: []uint8{1}}}, true); err == nil {
		t.Fatal("expected bind failure")
	}
	driver, err := engine.Open(host, port, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	defer driver.Close()
	if err := driver.WriteSingle(10, 7); !errors.Is(err, drivers.ErrObserveOnly) {
		t.Fatalf("failed listener returned control: %v", err)
	}
	if _, err := driver.Read(1, 1, drivers.ModbusHolding); err != nil {
		t.Fatal(err)
	}
}

func TestProxyRejectsUnconfiguredUnitsAndMalformedRequests(t *testing.T) {
	slave := startTestSlave(t)
	host, port := slave.Addr()
	engine := NewEngine()
	bind := Bind{Listen: "127.0.0.1:0", Host: host, Port: port, UnitIDs: []uint8{1, 2}}
	proxy, err := engine.Listen([]Bind{bind}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	for _, unit := range []uint8{0, 3, 248, 255} {
		res := proxy.forward(bind, unit, []byte{6, 0, 10, 0, 7})
		if len(res) != 2 || res[1] != modbusExcGWPath {
			t.Fatalf("unit %d: %x", unit, res)
		}
	}
	for _, pdu := range [][]byte{{3}, {3, 0, 1, 0, 0}, {4, 0xff, 0xff, 0, 2}, {6, 0, 1}, {0x10, 0, 1, 0, 2, 2, 0, 1}} {
		res := proxy.forward(bind, 1, pdu)
		if res[1] != modbusExcIllegalValue {
			t.Fatalf("malformed %x: %x", pdu, res)
		}
	}
	if len(slave.Units()) != 0 {
		t.Fatal("denied request reached backend")
	}
	res := proxy.forward(bind, 2, []byte{3, 0, 1, 0, 1})
	if res[0] != 3 {
		t.Fatalf("configured unit denied: %x", res)
	}
	if got := slave.Units(); len(got) != 1 || got[0] != 2 {
		t.Fatalf("backend units %v", got)
	}
}

func TestProxyYieldsToLocalRequests(t *testing.T) {
	slave := startTestSlave(t)
	host, port := slave.Addr()
	engine := NewEngine()
	driver, err := engine.Open(host, port, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	defer driver.Close()
	bind := Bind{Listen: "127.0.0.1:0", Host: host, Port: port, UnitIDs: []uint8{1}}
	proxy, err := engine.Listen([]Bind{bind}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	conn := driver.(*Capability).conn
	request := []byte{3, 0, 1, 0, 1}
	conn.mu.Lock()
	res := proxy.forward(bind, 1, request)
	conn.mu.Unlock()
	if res[1] != modbusExcBusy {
		t.Fatalf("occupied socket response: %x", res)
	}
	conn.mu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := driver.Read(1, 1, drivers.ModbusHolding)
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for conn.localWaiters.Load() == 0 {
		if time.Now().After(deadline) {
			conn.mu.Unlock()
			t.Fatal("driver did not queue a read")
		}
		time.Sleep(time.Millisecond)
	}
	res = proxy.forward(bind, 1, request)
	conn.mu.Unlock()
	if res[1] != modbusExcBusy {
		t.Fatalf("queued driver response: %x", res)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("driver's read did not finish")
	}
	if len(slave.Units()) != 1 {
		t.Fatal("proxy took a local request's turn")
	}
}

func TestProxyCloseUnblocksIdleClients(t *testing.T) {
	slave := startTestSlave(t)
	host, port := slave.Addr()
	proxy, err := NewEngine().Listen([]Bind{{Listen: "127.0.0.1:0", Host: host, Port: port, UnitIDs: []uint8{1}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	client, err := net.Dial("tcp", proxy.ListenAddrs()[0])
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	deadline := time.Now().Add(time.Second)
	for {
		proxy.mu.Lock()
		accepted := len(proxy.clients) == 1
		proxy.mu.Unlock()
		if accepted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("client was not accepted")
		}
		time.Sleep(time.Millisecond)
	}
	done := make(chan error, 1)
	go func() { done <- proxy.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("idle client stalled shutdown")
	}
}

func TestProxySlowBackendDoesNotRetryOrHoldDriverForFiveSeconds(t *testing.T) {
	slave := startTestSlave(t)
	host, port := slave.Addr()
	engine := NewEngine()
	driver, err := engine.Open(host, port, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	defer driver.Close()
	bind := Bind{Listen: "127.0.0.1:0", Host: host, Port: port, UnitIDs: []uint8{1}}
	proxy, err := engine.Listen([]Bind{bind}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	var blocked atomic.Bool
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	slave.mu.Lock()
	slave.beforeReply = func() {
		if blocked.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	}
	slave.mu.Unlock()
	result := make(chan []byte, 1)
	go func() { result <- proxy.forward(bind, 1, []byte{3, 0, 1, 0, 1}) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("proxy did not send the request")
	}
	select {
	case res := <-result:
		if res[1] != modbusExcGWTarget {
			t.Fatalf("timeout response: %x", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("proxy held the shared session for a driver timeout/retry")
	}
	if slave.Accepts() != 1 {
		t.Fatal("proxy reconnected to retry the timed-out read")
	}
	releaseOnce.Do(func() { close(release) })
	if _, err := driver.Read(1, 1, drivers.ModbusHolding); err != nil {
		t.Fatalf("driver could not restore its session: %v", err)
	}
}

type interruptedAcceptListener struct {
	net.Listener
	once sync.Once
}

func (l *interruptedAcceptListener) Accept() (net.Conn, error) {
	interrupted := false
	l.once.Do(func() { interrupted = true })
	if interrupted {
		return nil, errors.New("temporary accept failure")
	}
	return l.Listener.Accept()
}

func TestProxyAcceptFailureDoesNotStopServing(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := &interruptedAcceptListener{Listener: ln}
	proxy := &Proxy{
		listeners: []net.Listener{wrapper},
		clients:   make(map[net.Conn]bool),
		sem:       make(chan struct{}, proxyMaxClients),
		stop:      make(chan struct{}),
	}
	proxy.wg.Add(1)
	go proxy.serve(wrapper, Bind{UnitIDs: []uint8{1}})
	defer proxy.Close()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.SetDeadline(time.Now().Add(time.Second))
	if err := writeMBAP(client, 1, 1, []byte{3, 0, 1, 0, 1}); err != nil {
		t.Fatal(err)
	}
	_, _, res, err := readMBAP(client)
	if err != nil {
		t.Fatalf("listener did not recover: %v", err)
	}
	if res[1] != modbusExcGWPath {
		t.Fatalf("unexpected response: %x", res)
	}
}
