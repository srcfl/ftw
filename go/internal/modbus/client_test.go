package modbus

import (
	"errors"
	"io"
	"net"
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
					hdr[6],      // unit id
					hdr[7],      // function code
					2,           // byte count
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
					hdr[6],   // unit id
					hdr[7],   // function code
					2,        // byte count
					0, 222,   // value = 222
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
