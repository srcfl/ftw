package modbus

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
)

// testSlave is a tiny Modbus TCP server with holding/input maps. It
// records Accept count and unit IDs so sharing tests can see one socket.
type testSlave struct {
	ln      net.Listener
	accepts atomic.Int32
	mu      sync.Mutex
	holding map[uint16]uint16
	input   map[uint16]uint16
	unitIDs []uint8
	writes  int
	closing atomic.Bool
}

func startTestSlave(t *testing.T) *testSlave {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &testSlave{
		ln:      ln,
		holding: map[uint16]uint16{1: 0x1111, 2: 0x2222, 10: 0},
		input:   map[uint16]uint16{1: 0xAABB},
	}
	go s.loop()
	t.Cleanup(s.Close)
	return s
}

func (s *testSlave) Addr() (host string, port int) {
	host, portStr, _ := net.SplitHostPort(s.ln.Addr().String())
	var p int
	_, _ = fmtSscan(portStr, &p)
	return host, p
}

func (s *testSlave) Accepts() int { return int(s.accepts.Load()) }

func (s *testSlave) Units() []uint8 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]uint8, len(s.unitIDs))
	copy(out, s.unitIDs)
	return out
}

func (s *testSlave) Holding(addr uint16) uint16 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.holding[addr]
}

func (s *testSlave) Writes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

func (s *testSlave) Close() {
	s.closing.Store(true)
	_ = s.ln.Close()
}

func (s *testSlave) loop() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.accepts.Add(1)
		go s.serve(c)
	}
}

func (s *testSlave) serve(c net.Conn) {
	defer c.Close()
	for {
		hdr := make([]byte, 7)
		if _, err := io.ReadFull(c, hdr); err != nil {
			return
		}
		length := int(binary.BigEndian.Uint16(hdr[4:6]))
		if length < 2 {
			return
		}
		pdu := make([]byte, length-1)
		if _, err := io.ReadFull(c, pdu); err != nil {
			return
		}
		unit := hdr[6]
		s.mu.Lock()
		s.unitIDs = append(s.unitIDs, unit)
		respPDU := s.handlePDU(pdu)
		s.mu.Unlock()

		resp := make([]byte, 7+len(respPDU))
		copy(resp[0:2], hdr[0:2])
		binary.BigEndian.PutUint16(resp[4:6], uint16(len(respPDU)+1))
		resp[6] = unit
		copy(resp[7:], respPDU)
		if _, err := c.Write(resp); err != nil {
			return
		}
	}
}

func (s *testSlave) handlePDU(pdu []byte) []byte {
	if len(pdu) == 0 {
		return []byte{0x80, 0x01}
	}
	fc := pdu[0]
	switch fc {
	case modbusReadHoldingRegisters, modbusReadInputRegisters:
		if len(pdu) < 5 {
			return exceptionPDU(fc, 0x03)
		}
		addr := binary.BigEndian.Uint16(pdu[1:3])
		count := binary.BigEndian.Uint16(pdu[3:5])
		src := s.holding
		if fc == modbusReadInputRegisters {
			src = s.input
		}
		out := make([]byte, 2+int(count)*2)
		out[0] = fc
		out[1] = byte(count * 2)
		for i := uint16(0); i < count; i++ {
			binary.BigEndian.PutUint16(out[2+int(i)*2:4+int(i)*2], src[addr+i])
		}
		return out
	case modbusWriteSingleRegister:
		if len(pdu) < 5 {
			return exceptionPDU(fc, 0x03)
		}
		addr := binary.BigEndian.Uint16(pdu[1:3])
		value := binary.BigEndian.Uint16(pdu[3:5])
		s.holding[addr] = value
		s.writes++
		return append([]byte(nil), pdu...)
	case modbusWriteMultipleRegs:
		if len(pdu) < 6 {
			return exceptionPDU(fc, 0x03)
		}
		addr := binary.BigEndian.Uint16(pdu[1:3])
		count := binary.BigEndian.Uint16(pdu[3:5])
		s.writes++
		for i := uint16(0); i < count; i++ {
			off := 6 + int(i)*2
			if off+2 > len(pdu) {
				break
			}
			s.holding[addr+i] = binary.BigEndian.Uint16(pdu[off : off+2])
		}
		return []byte{fc, pdu[1], pdu[2], pdu[3], pdu[4]}
	default:
		return exceptionPDU(fc, 0x01)
	}
}
