package modbus

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	sv "github.com/simonvetter/modbus"

	"github.com/srcfl/ftw/go/internal/drivers"
)

func TestIsTransportError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{io.EOF, true},
		{io.ErrClosedPipe, true},
		{syscall.ECONNRESET, true},
		{syscall.EPIPE, true},
		{syscall.ETIMEDOUT, true},
		{syscall.ECONNREFUSED, true},
		{syscall.EHOSTUNREACH, true},
		{syscall.ENETUNREACH, true},
		{&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("temporary DNS failure")}, true},
		{errors.New("connection reset by peer"), true},
		{errors.New("broken pipe"), true},
		{errors.New("use of closed network connection"), true},
		{errors.New("i/o timeout"), true},
		{errors.New("EOF"), true},
		// simonvetter's own deadline sentinel. The TCP socket can still be
		// ESTABLISHED while the device goes mute on the session (CTEK CSOS
		// chargers do this — a fresh connection answers fine), so a redial is
		// the correct response. See TestReadReconnectsAfterServerTimesOut.
		{sv.ErrRequestTimedOut, true},
		{errors.New("request timed out"), true},
		// Modbus protocol errors — live peer, connection usable, no reconnect.
		{errors.New("illegal data address"), false},
		{errors.New("illegal function"), false},
		{errors.New("slave device busy"), false},
		// Unrelated errors.
		{errors.New("some random error"), false},
	}
	for _, c := range cases {
		if got := isTransportError(c.err); got != c.want {
			t.Errorf("isTransportError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestIsRetryableDialError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{syscall.ECONNREFUSED, true},
		{syscall.EHOSTUNREACH, true},
		{&net.DNSError{Name: "meter.local", IsTimeout: true}, true},
		{&net.DNSError{Name: "meter.local", IsTemporary: true}, true},
		{&net.DNSError{Name: "meter.local", IsNotFound: true}, true},
		{errors.New("bad endpoint configuration"), false},
	}
	for _, tc := range cases {
		if got := isRetryableDialError(tc.err); got != tc.want {
			t.Errorf("isRetryableDialError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestDialRecoversWhenDeviceComesOnlineAfterStartup(t *testing.T) {
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve address: %v", err)
	}
	address := reservation.Addr().String()
	if err := reservation.Close(); err != nil {
		t.Fatalf("release address: %v", err)
	}
	host, portText, _ := net.SplitHostPort(address)
	var port int
	if _, err := fmtSscan(portText, &port); err != nil {
		t.Fatalf("parse listener port: %v", err)
	}

	capability, err := Dial(host, port, 1)
	if err != nil {
		t.Fatalf("Dial while device offline: %v", err)
	}
	defer capability.Close()
	if capability.client != nil {
		t.Fatal("offline Dial kept a client")
	}

	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("bring device online: %v", err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		request := make([]byte, 12)
		if _, err := io.ReadFull(conn, request); err != nil {
			return
		}
		response := []byte{
			request[0], request[1],
			0, 0,
			0, 5,
			request[6],
			request[7],
			2,
			1, 65,
		}
		_, _ = conn.Write(response)
	}()

	registers, err := capability.Read(0, 1, drivers.ModbusInput)
	if err != nil {
		t.Fatalf("Read after device recovery: %v", err)
	}
	if len(registers) != 1 || registers[0] != 321 {
		t.Fatalf("Read after device recovery = %v, want [321]", registers)
	}
}

func TestDialRejectsInvalidEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name   string
		host   string
		port   int
		unitID int
	}{
		{name: "empty host", port: 502, unitID: 1},
		{name: "host with spaces", host: "bad host", port: 502, unitID: 1},
		{name: "host with scheme", host: "tcp://meter", port: 502, unitID: 1},
		{name: "zero port", host: "meter.local", unitID: 1},
		{name: "large port", host: "meter.local", port: 65536, unitID: 1},
		{name: "negative unit", host: "meter.local", port: 502, unitID: -1},
		{name: "large unit", host: "meter.local", port: 502, unitID: 248},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capability, err := Dial(tc.host, tc.port, tc.unitID)
			if err == nil || capability != nil {
				t.Fatalf("Dial(%q, %d, %d) = (%v, %v), want direct config error",
					tc.host, tc.port, tc.unitID, capability, err)
			}
		})
	}
}

func TestValidateEndpointAllowsSupportedHosts(t *testing.T) {
	for _, host := range []string{
		"192.0.2.10",
		"meter.local",
		"meter.local.",
		"2001:db8::10",
		"fe80::10%en0",
	} {
		if err := validateEndpoint(host, 502, 1); err != nil {
			t.Errorf("validateEndpoint(%q): %v", host, err)
		}
	}
}

func TestNeverConnectedCapabilityCloseAndPollRespectBackoff(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	capability := &Capability{
		addr:                         "127.0.0.1:502",
		url:                          "tcp://127.0.0.1:502",
		requestTimeout:               10 * time.Millisecond,
		consecutiveTransportFailures: 2,
		nextReconnectAt:              now.Add(2 * time.Second),
		now:                          func() time.Time { return now },
	}

	started := time.Now()
	for i := 0; i < 100; i++ {
		if _, err := capability.Read(0, 1, drivers.ModbusInput); err == nil ||
			!containsFold(err.Error(), "backoff active") {
			t.Fatalf("poll %d while never connected error = %v, want backoff", i, err)
		}
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("poll during backoff took %v; possible busy/blocking retry", elapsed)
	}
	if capability.client != nil {
		t.Fatal("poll during backoff opened a client")
	}
	if capability.consecutiveTransportFailures != 2 ||
		!capability.nextReconnectAt.Equal(now.Add(2*time.Second)) {
		t.Fatalf("poll during backoff changed retry state: failures=%d next=%v",
			capability.consecutiveTransportFailures, capability.nextReconnectAt)
	}
	if err := capability.Close(); err != nil {
		t.Fatalf("Close before first connection: %v", err)
	}
	if err := capability.Close(); err != nil {
		t.Fatalf("second Close before first connection: %v", err)
	}
}

// TestReadReconnectsAfterServerClosesConnection stands up a TCP
// server that accepts ONE Modbus request on a connection, then drops
// the socket. The Capability should detect the transport error and
// reconnect transparently so the second Read succeeds.
//
// This mirrors the Sungrow incident (2026-04-19): after the inverter
// silently closed our TCP connection following a write command at
// startup, every subsequent read returned transport errors and our
// driver emitted zeros for hours. The fix must reconnect on error.
func TestReadReconnectsAfterServerClosesConnection(t *testing.T) {
	// Toy Modbus TCP server: responds to read-input-registers once per
	// connection with a canned value, then closes. Subsequent reads
	// force the client to reconnect.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	type conn struct{ value uint16 }
	conns := make(chan conn, 4)
	conns <- conn{value: 111}
	conns <- conn{value: 222}

	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				// Canned "one request per connection". Read MBAP header
				// (7 bytes) + PDU (5 for read-registers) = 12. Echo back
				// a one-register response with the queued value.
				hdr := make([]byte, 12)
				if _, err := io.ReadFull(c, hdr); err != nil {
					return
				}
				// Pull next queued value (non-blocking default = 0).
				var v uint16
				select {
				case cv := <-conns:
					v = cv.value
				default:
				}
				// Response: MBAP (transaction id echo, proto 0, len=5, unit id
				// echo) + PDU (fc echo, byte count=2, val hi/lo).
				resp := []byte{
					hdr[0], hdr[1], // tx id
					0, 0, // protocol
					0, 5, // length
					hdr[6], // unit id
					hdr[7], // function code
					2,      // byte count
					byte(v >> 8), byte(v),
				}
				_, _ = c.Write(resp)
				// Close intentionally — mimic Sungrow's behavior.
			}(c)
		}
	}()

	host, portStr, _ := net.SplitHostPort(listener.Addr().String())
	var port int
	if _, err := fmtSscan(portStr, &port); err != nil {
		t.Fatalf("bad listener port: %v", err)
	}

	cap, err := Dial(host, port, 1)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cap.Close()

	// First read — server accepts, responds 111, closes.
	regs, err := cap.Read(0, 1, drivers.ModbusInput)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if len(regs) != 1 || regs[0] != 111 {
		t.Errorf("first read = %v, want [111]", regs)
	}

	// Second read — initial socket is dead. Cap should reconnect and
	// return 222 from the queued-conn value.
	regs, err = cap.Read(0, 1, drivers.ModbusInput)
	if err != nil {
		t.Fatalf("second read (reconnect path): %v", err)
	}
	if len(regs) != 1 || regs[0] != 222 {
		t.Errorf("second read after reconnect = %v, want [222]", regs)
	}
}

// TestReadReconnectsAfterServerTimesOut covers the CTEK CSOS incident
// (2026-06-10, Stefan's Pi): the charger kept the TCP socket ESTABLISHED
// but stopped answering requests on that long-lived session, so every
// WriteRegister returned simonvetter's ErrRequestTimedOut ("request timed
// out") rather than a closed-socket error. The first server connection
// here accepts the request and never replies (forcing the client timeout);
// the second connection answers normally. The Capability must classify the
// timeout as transport, redial, and succeed on the retry.
func TestReadReconnectsAfterServerTimesOut(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	var accepted int
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			accepted++
			mute := accepted == 1
			go func(c net.Conn, mute bool) {
				defer c.Close()
				hdr := make([]byte, 12)
				if _, err := io.ReadFull(c, hdr); err != nil {
					return
				}
				if mute {
					// Mimic the CTEK: read the request, answer nothing, keep
					// the socket open until the client gives up and redials.
					time.Sleep(8 * time.Second)
					return
				}
				resp := []byte{
					hdr[0], hdr[1], // tx id
					0, 0, // protocol
					0, 5, // length
					hdr[6], // unit id
					hdr[7], // function code
					2,      // byte count
					0, 222, // value = 222
				}
				_, _ = c.Write(resp)
			}(c, mute)
		}
	}()

	host, portStr, _ := net.SplitHostPort(listener.Addr().String())
	var port int
	if _, err := fmtSscan(portStr, &port); err != nil {
		t.Fatalf("bad listener port: %v", err)
	}

	cap, err := Dial(host, port, 1)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cap.Close()

	// First read times out on the mute connection; Capability must reconnect
	// and the retry hits the second (responsive) connection.
	regs, err := cap.Read(0, 1, drivers.ModbusInput)
	if err != nil {
		t.Fatalf("read (timeout→reconnect path): %v", err)
	}
	if len(regs) != 1 || regs[0] != 222 {
		t.Errorf("read after reconnect = %v, want [222]", regs)
	}
}

func TestWriteSingleAndMultiEncodeRequests(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	reqs := make(chan []byte, 2)
	go func() {
		c, err := listener.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		for i := 0; i < 2; i++ {
			hdr := make([]byte, 7)
			if _, err := io.ReadFull(c, hdr); err != nil {
				return
			}
			length := int(binary.BigEndian.Uint16(hdr[4:6]))
			pdu := make([]byte, length-1)
			if _, err := io.ReadFull(c, pdu); err != nil {
				return
			}
			reqs <- append([]byte(nil), pdu...)
			var respPDU []byte
			switch pdu[0] {
			case modbusWriteSingleRegister:
				respPDU = append([]byte(nil), pdu...)
			case modbusWriteMultipleRegs:
				respPDU = []byte{pdu[0], pdu[1], pdu[2], pdu[3], pdu[4]}
			default:
				return
			}
			resp := make([]byte, 7+len(respPDU))
			copy(resp[0:2], hdr[0:2])
			binary.BigEndian.PutUint16(resp[4:6], uint16(len(respPDU)+1))
			resp[6] = hdr[6]
			copy(resp[7:], respPDU)
			_, _ = c.Write(resp)
		}
	}()

	host, portStr, _ := net.SplitHostPort(listener.Addr().String())
	var port int
	if _, err := fmtSscan(portStr, &port); err != nil {
		t.Fatalf("bad listener port: %v", err)
	}
	cap, err := Dial(host, port, 7)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cap.Close()

	if err := cap.WriteSingle(0x1234, 0x00ab); err != nil {
		t.Fatalf("write single: %v", err)
	}
	if err := cap.WriteMulti(0x2000, []uint16{0x0102, 0x0304}); err != nil {
		t.Fatalf("write multi: %v", err)
	}

	gotSingle := <-reqs
	wantSingle := []byte{modbusWriteSingleRegister, 0x12, 0x34, 0x00, 0xab}
	if !bytesEqual(gotSingle, wantSingle) {
		t.Fatalf("write single pdu = % x, want % x", gotSingle, wantSingle)
	}
	gotMulti := <-reqs
	wantMulti := []byte{modbusWriteMultipleRegs, 0x20, 0x00, 0x00, 0x02, 0x04, 0x01, 0x02, 0x03, 0x04}
	if !bytesEqual(gotMulti, wantMulti) {
		t.Fatalf("write multi pdu = % x, want % x", gotMulti, wantMulti)
	}
}

func TestConfigureTCPKeepAlive(t *testing.T) {
	conn := &fakeKeepAliveConn{}
	if err := configureTCPKeepAlive(conn, 15*time.Second); err != nil {
		t.Fatalf("configure keepalive: %v", err)
	}
	if !conn.enabled {
		t.Fatal("keepalive was not enabled")
	}
	if conn.period != 15*time.Second {
		t.Fatalf("keepalive period = %v, want 15s", conn.period)
	}
}

func TestReconnectBackoffSchedule(t *testing.T) {
	c := &Capability{}
	want := map[int]time.Duration{
		0: 0,
		1: 0,
		2: 2 * time.Second,
		3: 4 * time.Second,
		4: 8 * time.Second,
		5: 16 * time.Second,
		6: 32 * time.Second,
		7: 60 * time.Second,
		8: 60 * time.Second,
	}
	for failures, delay := range want {
		c.consecutiveTransportFailures = failures
		if got := c.reconnectBackoff(); got != delay {
			t.Errorf("failures=%d: backoff=%v, want %v", failures, got, delay)
		}
	}
}

func TestMuteReconnectBackoffReturnsFastAndRecovers(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	var accepted atomic.Int32
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			attempt := accepted.Add(1)
			go func(conn net.Conn, attempt int32) {
				defer conn.Close()
				request := make([]byte, 12)
				if _, err := io.ReadFull(conn, request); err != nil {
					return
				}
				if attempt <= 2 {
					time.Sleep(250 * time.Millisecond)
					return
				}
				response := []byte{
					request[0], request[1],
					0, 0,
					0, 5,
					request[6],
					request[7],
					2,
					0, 222,
				}
				_, _ = conn.Write(response)
			}(conn, attempt)
		}
	}()

	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	var port int
	if _, err := fmtSscan(portText, &port); err != nil {
		t.Fatalf("parse listener port: %v", err)
	}

	capability, err := Dial(host, port, 1)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer capability.Close()
	const requestTimeout = 40 * time.Millisecond
	capability.requestTimeout = requestTimeout
	capability.client.timeout = requestTimeout

	fakeNow := time.Unix(1_700_000_000, 0)
	capability.now = func() time.Time { return fakeNow }

	if _, err := capability.Read(0, 1, drivers.ModbusInput); err == nil {
		t.Fatal("first mute read succeeded")
	}
	if capability.consecutiveTransportFailures != 2 {
		t.Fatalf("transport failures=%d, want 2", capability.consecutiveTransportFailures)
	}
	if capability.client != nil {
		t.Fatal("mute retry left its socket open")
	}

	started := time.Now()
	if _, err := capability.Read(0, 1, drivers.ModbusInput); err == nil || !containsFold(err.Error(), "backoff active") {
		t.Fatalf("read during cooldown error=%v, want backoff error", err)
	}
	if elapsed := time.Since(started); elapsed > 20*time.Millisecond {
		t.Fatalf("read during cooldown blocked for %v", elapsed)
	}

	fakeNow = fakeNow.Add(2 * time.Second)
	registers, err := capability.Read(0, 1, drivers.ModbusInput)
	if err != nil {
		t.Fatalf("read after cooldown: %v", err)
	}
	if len(registers) != 1 || registers[0] != 222 {
		t.Fatalf("read after cooldown=%v, want [222]", registers)
	}
	if capability.consecutiveTransportFailures != 0 || !capability.nextReconnectAt.IsZero() {
		t.Fatalf("successful recovery kept failure state: failures=%d next=%v",
			capability.consecutiveTransportFailures, capability.nextReconnectAt)
	}
}

// tiny strconv-free int parser to avoid pulling strconv in a single spot.
func fmtSscan(s string, out *int) (int, error) {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, errors.New("bad digit")
		}
		n = n*10 + int(c-'0')
	}
	*out = n
	return len(s), nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type fakeKeepAliveConn struct {
	enabled bool
	period  time.Duration
}

func (f *fakeKeepAliveConn) SetKeepAlive(enabled bool) error {
	f.enabled = enabled
	return nil
}

func (f *fakeKeepAliveConn) SetKeepAlivePeriod(period time.Duration) error {
	f.period = period
	return nil
}

func (f *fakeKeepAliveConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (f *fakeKeepAliveConn) Write(b []byte) (int, error)      { return len(b), nil }
func (f *fakeKeepAliveConn) Close() error                     { return nil }
func (f *fakeKeepAliveConn) LocalAddr() net.Addr              { return fakeAddr("local") }
func (f *fakeKeepAliveConn) RemoteAddr() net.Addr             { return fakeAddr("remote") }
func (f *fakeKeepAliveConn) SetDeadline(time.Time) error      { return nil }
func (f *fakeKeepAliveConn) SetReadDeadline(time.Time) error  { return nil }
func (f *fakeKeepAliveConn) SetWriteDeadline(time.Time) error { return nil }

type fakeAddr string

func (a fakeAddr) Network() string { return string(a) }
func (a fakeAddr) String() string  { return string(a) }
