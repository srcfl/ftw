package modbus

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/srcfl/ftw/go/internal/drivers"
)

const (
	proxyIdleTimeout   = 90 * time.Second
	proxyMaxPDUSize    = 253
	proxyMaxClients    = 16
	proxyMaxADULength  = 1 + proxyMaxPDUSize // unit ID + PDU
	modbusExcIllegalFn = 0x01
	modbusExcGWPath    = 0x0A
	modbusExcGWTarget  = 0x0B
)

// Bind is one proxy listener attached to a backend already in the Engine.
type Bind struct {
	Listen               string
	Host                 string
	Port                 int
	AllowUnverifiedLocal bool
}

// Proxy accepts Modbus TCP clients and multiplexes their PDUs onto the
// Engine session for that backend. Writes are denied unless allowWrite is
// set: a LAN client writing registers would bypass FTW's control loop.
type Proxy struct {
	engine     *Engine
	allowWrite bool

	mu        sync.Mutex
	listeners []net.Listener
	pins      []*Capability
	byAddr    map[string]*Capability
	wg        sync.WaitGroup
	closing   atomic.Bool
	sem       chan struct{}
}

// Listen pins each backend session and serves Modbus TCP on Bind.Listen.
func (e *Engine) Listen(binds []Bind, allowWrite bool) (*Proxy, error) {
	if e == nil {
		return nil, errors.New("modbus engine is nil")
	}
	p := &Proxy{
		engine:     e,
		allowWrite: allowWrite,
		byAddr:     make(map[string]*Capability),
		sem:        make(chan struct{}, proxyMaxClients),
	}
	for _, b := range binds {
		if err := p.addBind(b); err != nil {
			_ = p.Close()
			return nil, err
		}
	}
	return p, nil
}

func (p *Proxy) addBind(b Bind) error {
	if b.Listen == "" || b.Host == "" || b.Port < 1 {
		return fmt.Errorf("modbus proxy bind incomplete: listen=%q host=%q port=%d", b.Listen, b.Host, b.Port)
	}
	pin, err := DialWithOptions(b.Host, b.Port, 1, b.AllowUnverifiedLocal)
	if err != nil {
		return fmt.Errorf("modbus proxy pin %s:%d: %w", b.Host, b.Port, err)
	}
	ln, err := net.Listen("tcp", b.Listen)
	if err != nil {
		_ = pin.Close()
		return fmt.Errorf("modbus proxy listen %s: %w", b.Listen, err)
	}
	p.mu.Lock()
	p.pins = append(p.pins, pin)
	if p.byAddr == nil {
		p.byAddr = make(map[string]*Capability)
	}
	p.byAddr[sessionKey(b.Host, b.Port)] = pin
	p.listeners = append(p.listeners, ln)
	p.mu.Unlock()

	backend := Bind{Host: b.Host, Port: b.Port, Listen: ln.Addr().String()}
	p.wg.Add(1)
	go p.serve(ln, backend)
	slog.Info("modbus proxy listening",
		"listen", ln.Addr().String(),
		"backend", sessionKey(b.Host, b.Port),
		"allow_write", p.allowWrite)
	return nil
}

func (p *Proxy) serve(ln net.Listener, backend Bind) {
	defer p.wg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if p.closing.Load() {
				return
			}
			slog.Warn("modbus proxy accept", "listen", ln.Addr().String(), "err", err)
			return
		}
		select {
		case p.sem <- struct{}{}:
		default:
			slog.Warn("modbus proxy client limit reached", "listen", ln.Addr().String())
			_ = conn.Close()
			continue
		}
		p.wg.Add(1)
		go func(c net.Conn) {
			defer p.wg.Done()
			defer func() { <-p.sem }()
			p.handleClient(c, backend)
		}(conn)
	}
}

func (p *Proxy) handleClient(conn net.Conn, backend Bind) {
	defer conn.Close()
	for {
		if p.closing.Load() {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(proxyIdleTimeout))
		txID, unitID, pdu, err := readMBAP(conn)
		if err != nil {
			return
		}
		resp := p.forward(backend, unitID, pdu)
		_ = conn.SetWriteDeadline(time.Now().Add(modbusRequestTimeout))
		if err := writeMBAP(conn, txID, unitID, resp); err != nil {
			return
		}
	}
}

func (p *Proxy) forward(backend Bind, unitID uint8, pdu []byte) []byte {
	fc := pdu[0]
	if isModbusWrite(fc) && !p.allowWrite {
		return exceptionPDU(fc, modbusExcIllegalFn)
	}
	if !isModbusRead(fc) && !isModbusWrite(fc) {
		return exceptionPDU(fc, modbusExcIllegalFn)
	}
	p.mu.Lock()
	cap := p.byAddr[sessionKey(backend.Host, backend.Port)]
	p.mu.Unlock()
	if cap == nil {
		return exceptionPDU(fc, modbusExcGWPath)
	}
	res, err := cap.executePDU(unitID, pdu)
	if err != nil {
		if errors.Is(err, drivers.ErrModbusBackoff) || isTransportError(err) {
			return exceptionPDU(fc, modbusExcGWTarget)
		}
		slog.Warn("modbus proxy backend", "backend", sessionKey(backend.Host, backend.Port), "err", err)
		return exceptionPDU(fc, modbusExcGWTarget)
	}
	if len(res) == 0 {
		return exceptionPDU(fc, modbusExcGWTarget)
	}
	return res
}

// ListenAddrs returns the bound address of each listener. Used in tests.
func (p *Proxy) ListenAddrs() []string {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.listeners))
	for i, ln := range p.listeners {
		out[i] = ln.Addr().String()
	}
	return out
}

// Close stops listeners and drops the pin refs. Driver handles keep their
// sessions.
func (p *Proxy) Close() error {
	if p == nil {
		return nil
	}
	p.closing.Store(true)
	p.mu.Lock()
	listeners := p.listeners
	p.listeners = nil
	pins := p.pins
	p.pins = nil
	p.byAddr = nil
	p.mu.Unlock()
	var first error
	for _, ln := range listeners {
		if err := ln.Close(); err != nil && first == nil {
			first = err
		}
	}
	p.wg.Wait()
	for _, pin := range pins {
		if err := pin.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func isModbusRead(fc byte) bool {
	switch fc {
	case 0x01, 0x02, 0x03, 0x04:
		return true
	default:
		return false
	}
}

func isModbusWrite(fc byte) bool {
	switch fc {
	case 0x05, 0x06, 0x0F, 0x10, 0x15, 0x16, 0x17:
		return true
	default:
		return false
	}
}

func exceptionPDU(fc, code byte) []byte {
	return []byte{fc | 0x80, code}
}

func readMBAP(r io.Reader) (txID uint16, unitID uint8, pdu []byte, err error) {
	hdr := make([]byte, 7)
	if _, err = io.ReadFull(r, hdr); err != nil {
		return 0, 0, nil, err
	}
	txID = binary.BigEndian.Uint16(hdr[0:2])
	if proto := binary.BigEndian.Uint16(hdr[2:4]); proto != 0 {
		return 0, 0, nil, fmt.Errorf("modbus proxy protocol id %d", proto)
	}
	length := int(binary.BigEndian.Uint16(hdr[4:6]))
	if length < 2 || length > proxyMaxADULength {
		return 0, 0, nil, fmt.Errorf("modbus proxy invalid length %d", length)
	}
	pdu = make([]byte, length-1)
	if _, err = io.ReadFull(r, pdu); err != nil {
		return 0, 0, nil, err
	}
	return txID, hdr[6], pdu, nil
}

func writeMBAP(w io.Writer, txID uint16, unitID uint8, pdu []byte) error {
	if len(pdu) == 0 || len(pdu) > proxyMaxPDUSize {
		return fmt.Errorf("modbus proxy invalid response pdu length %d", len(pdu))
	}
	buf := make([]byte, 7+len(pdu))
	binary.BigEndian.PutUint16(buf[0:2], txID)
	binary.BigEndian.PutUint16(buf[2:4], 0)
	binary.BigEndian.PutUint16(buf[4:6], uint16(len(pdu)+1))
	buf[6] = unitID
	copy(buf[7:], pdu)
	_, err := w.Write(buf)
	return err
}
