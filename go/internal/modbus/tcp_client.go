package modbus

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

const (
	modbusReadHoldingRegisters byte = 0x03
	modbusReadInputRegisters   byte = 0x04
	modbusWriteSingleRegister  byte = 0x06
	modbusWriteMultipleRegs    byte = 0x10

	modbusRequestTimeout = 5 * time.Second
	modbusTCPKeepAlive   = 15 * time.Second
	modbusDialTimeout    = 5 * time.Second
)

type tcpClient struct {
	addr      string
	timeout   time.Duration
	keepAlive time.Duration
	unitID    uint8
	txID      uint16
	conn      net.Conn
}

func newTCPClient(addr string, timeout, keepAlive time.Duration) *tcpClient {
	return &tcpClient{
		addr:      addr,
		timeout:   timeout,
		keepAlive: keepAlive,
		unitID:    1,
	}
}

func (c *tcpClient) Open() error {
	dialer := net.Dialer{
		Timeout:   modbusDialTimeout,
		KeepAlive: c.keepAlive,
	}
	conn, err := dialer.Dial("tcp", c.addr)
	if err != nil {
		return err
	}
	if err := configureTCPKeepAlive(conn, c.keepAlive); err != nil {
		_ = conn.Close()
		return err
	}
	c.conn = conn
	return nil
}

func (c *tcpClient) Close() error {
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

func (c *tcpClient) SetUnitId(id uint8) {
	c.unitID = id
}

func (c *tcpClient) ReadRegisters(addr, count uint16, fc byte) ([]uint16, error) {
	if fc != modbusReadHoldingRegisters && fc != modbusReadInputRegisters {
		return nil, fmt.Errorf("unsupported modbus read function 0x%02x", fc)
	}
	if count == 0 {
		return nil, errors.New("modbus read count must be > 0")
	}
	pdu := []byte{
		fc,
		byte(addr >> 8), byte(addr),
		byte(count >> 8), byte(count),
	}
	res, err := c.execute(pdu)
	if err != nil {
		return nil, err
	}
	if len(res) < 2 || res[0] != fc {
		return nil, fmt.Errorf("unexpected modbus read response function 0x%02x", responseFunction(res))
	}
	byteCount := int(res[1])
	if byteCount != int(count)*2 || len(res) != 2+byteCount {
		return nil, fmt.Errorf("unexpected modbus read response length %d for %d registers", byteCount, count)
	}
	regs := make([]uint16, count)
	for i := range regs {
		off := 2 + i*2
		regs[i] = binary.BigEndian.Uint16(res[off : off+2])
	}
	return regs, nil
}

func (c *tcpClient) WriteRegister(addr, value uint16) error {
	pdu := []byte{
		modbusWriteSingleRegister,
		byte(addr >> 8), byte(addr),
		byte(value >> 8), byte(value),
	}
	res, err := c.execute(pdu)
	if err != nil {
		return err
	}
	if len(res) != len(pdu) || res[0] != modbusWriteSingleRegister ||
		binary.BigEndian.Uint16(res[1:3]) != addr ||
		binary.BigEndian.Uint16(res[3:5]) != value {
		return fmt.Errorf("unexpected modbus write-single response")
	}
	return nil
}

func (c *tcpClient) WriteRegisters(addr uint16, values []uint16) error {
	if len(values) == 0 {
		return errors.New("modbus write-multiple values must be non-empty")
	}
	if len(values) > 123 {
		return fmt.Errorf("modbus write-multiple values exceeds protocol limit: %d", len(values))
	}
	count := uint16(len(values))
	pdu := make([]byte, 6+len(values)*2)
	pdu[0] = modbusWriteMultipleRegs
	binary.BigEndian.PutUint16(pdu[1:3], addr)
	binary.BigEndian.PutUint16(pdu[3:5], count)
	pdu[5] = byte(len(values) * 2)
	for i, v := range values {
		binary.BigEndian.PutUint16(pdu[6+i*2:8+i*2], v)
	}
	res, err := c.execute(pdu)
	if err != nil {
		return err
	}
	if len(res) != 5 || res[0] != modbusWriteMultipleRegs ||
		binary.BigEndian.Uint16(res[1:3]) != addr ||
		binary.BigEndian.Uint16(res[3:5]) != count {
		return fmt.Errorf("unexpected modbus write-multiple response")
	}
	return nil
}

func (c *tcpClient) execute(pdu []byte) ([]byte, error) {
	if c.conn == nil {
		return nil, io.ErrClosedPipe
	}
	c.txID++
	txID := c.txID
	req := make([]byte, 7+len(pdu))
	binary.BigEndian.PutUint16(req[0:2], txID)
	binary.BigEndian.PutUint16(req[2:4], 0)
	binary.BigEndian.PutUint16(req[4:6], uint16(len(pdu)+1))
	req[6] = c.unitID
	copy(req[7:], pdu)

	deadline := time.Now().Add(c.timeout)
	_ = c.conn.SetDeadline(deadline)
	if _, err := c.conn.Write(req); err != nil {
		return nil, err
	}
	hdr := make([]byte, 7)
	if _, err := io.ReadFull(c.conn, hdr); err != nil {
		return nil, err
	}
	if got := binary.BigEndian.Uint16(hdr[0:2]); got != txID {
		return nil, fmt.Errorf("modbus transaction id mismatch: got %d want %d", got, txID)
	}
	if proto := binary.BigEndian.Uint16(hdr[2:4]); proto != 0 {
		return nil, fmt.Errorf("modbus protocol id mismatch: got %d", proto)
	}
	length := int(binary.BigEndian.Uint16(hdr[4:6]))
	if length < 2 {
		return nil, fmt.Errorf("modbus invalid response length %d", length)
	}
	res := make([]byte, length-1)
	if _, err := io.ReadFull(c.conn, res); err != nil {
		return nil, err
	}
	if len(res) == 0 {
		return nil, errors.New("modbus empty response pdu")
	}
	if res[0]&0x80 != 0 {
		code := byte(0)
		if len(res) > 1 {
			code = res[1]
		}
		return nil, modbusException{function: res[0] &^ 0x80, code: code}
	}
	return res, nil
}

func responseFunction(pdu []byte) byte {
	if len(pdu) == 0 {
		return 0
	}
	return pdu[0]
}

type modbusException struct {
	function byte
	code     byte
}

func (e modbusException) Error() string {
	return fmt.Sprintf("modbus exception function=0x%02x code=0x%02x", e.function, e.code)
}

type tcpKeepAliveConn interface {
	SetKeepAlive(bool) error
	SetKeepAlivePeriod(time.Duration) error
}

func configureTCPKeepAlive(conn net.Conn, period time.Duration) error {
	if period <= 0 {
		return nil
	}
	tcp, ok := conn.(tcpKeepAliveConn)
	if !ok {
		return nil
	}
	if err := tcp.SetKeepAlive(true); err != nil {
		return err
	}
	return tcp.SetKeepAlivePeriod(period)
}
