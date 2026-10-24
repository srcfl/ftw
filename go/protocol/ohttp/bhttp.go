package ohttp

import (
	"encoding/binary"
	"errors"
)

// Known-length Binary HTTP (RFC 9292), the message inside a request and its
// response. Indeterminate-length messages are not read: nothing here sends
// them.

// ErrMessage is a malformed Binary HTTP message.
var ErrMessage = errors.New("ohttp: invalid binary HTTP message")

// Field is one header or trailer line.
type Field struct {
	Name, Value string
}

// Request is a known-length request. Trailers are always empty.
type Request struct {
	Method, Scheme, Authority, Path string
	Fields                          []Field
	Content                         []byte
}

// Response is a known-length final response. Trailers are always empty, and
// informational responses are not carried.
type Response struct {
	Status  int
	Fields  []Field
	Content []byte
}

const (
	knownRequest  = 0
	knownResponse = 1
)

// Marshal writes the request, then zero padding up to size bytes. Padding
// keeps every request the same length on the wire (RFC 9292, section 3.8).
func (r Request) Marshal(size int) []byte {
	b := appendVarint(nil, knownRequest)
	for _, s := range []string{r.Method, r.Scheme, r.Authority, r.Path} {
		b = appendBytes(b, []byte(s))
	}
	b = appendFields(b, r.Fields)
	b = appendBytes(b, r.Content)
	b = appendVarint(b, 0)
	return pad(b, size)
}

// Marshal writes the response, then zero padding up to size bytes.
func (r Response) Marshal(size int) []byte {
	b := appendVarint(nil, knownResponse)
	b = appendVarint(b, uint64(r.Status))
	b = appendFields(b, r.Fields)
	b = appendBytes(b, r.Content)
	b = appendVarint(b, 0)
	return pad(b, size)
}

// ParseRequest reads a known-length request. Sections after the control data
// may be truncated, and anything after the trailers must be zero padding.
func ParseRequest(b []byte) (Request, error) {
	d := decoder{b: b}
	if d.varint() != knownRequest {
		return Request{}, ErrMessage
	}
	var r Request
	r.Method, r.Scheme, r.Authority, r.Path = string(d.bytes()), string(d.bytes()), string(d.bytes()), string(d.bytes())
	if d.err != nil {
		return Request{}, ErrMessage
	}
	r.Fields = d.fields()
	r.Content = d.optional()
	trailers := d.fields()
	if d.err != nil || len(trailers) != 0 || !d.padding() {
		return Request{}, ErrMessage
	}
	return r, nil
}

// ParseResponse reads a known-length final response.
func ParseResponse(b []byte) (Response, error) {
	d := decoder{b: b}
	if d.varint() != knownResponse {
		return Response{}, ErrMessage
	}
	status := d.varint()
	if d.err != nil || status < 200 || status > 599 {
		return Response{}, ErrMessage
	}
	r := Response{Status: int(status)}
	r.Fields = d.fields()
	r.Content = d.optional()
	trailers := d.fields()
	if d.err != nil || len(trailers) != 0 || !d.padding() {
		return Response{}, ErrMessage
	}
	return r, nil
}

func appendVarint(b []byte, v uint64) []byte {
	switch {
	case v < 1<<6:
		return append(b, byte(v))
	case v < 1<<14:
		return binary.BigEndian.AppendUint16(b, uint16(v)|0x4000)
	case v < 1<<30:
		return binary.BigEndian.AppendUint32(b, uint32(v)|0x8000_0000)
	default:
		return binary.BigEndian.AppendUint64(b, v|0xc000_0000_0000_0000)
	}
}

func appendBytes(b, v []byte) []byte {
	return append(appendVarint(b, uint64(len(v))), v...)
}

func appendFields(b []byte, fields []Field) []byte {
	var lines []byte
	for _, f := range fields {
		lines = appendBytes(lines, []byte(f.Name))
		lines = appendBytes(lines, []byte(f.Value))
	}
	return appendBytes(b, lines)
}

func pad(b []byte, size int) []byte {
	if len(b) < size {
		b = append(b, make([]byte, size-len(b))...)
	}
	return b
}

type decoder struct {
	b   []byte
	err error
}

func (d *decoder) varint() uint64 {
	if d.err != nil || len(d.b) == 0 {
		d.err = ErrMessage
		return 0
	}
	n := 1 << (d.b[0] >> 6)
	if len(d.b) < n {
		d.err = ErrMessage
		return 0
	}
	v := uint64(d.b[0] & 0x3f)
	for _, c := range d.b[1:n] {
		v = v<<8 | uint64(c)
	}
	d.b = d.b[n:]
	return v
}

func (d *decoder) bytes() []byte {
	n := d.varint()
	if d.err != nil || n > uint64(len(d.b)) {
		d.err = ErrMessage
		return nil
	}
	v := d.b[:n]
	d.b = d.b[n:]
	return v
}

// optional reads a length-prefixed part that a truncated message leaves out.
func (d *decoder) optional() []byte {
	if d.err != nil || len(d.b) == 0 {
		return nil
	}
	return d.bytes()
}

func (d *decoder) fields() []Field {
	section := decoder{b: d.optional()}
	var out []Field
	for d.err == nil && len(section.b) > 0 {
		name, value := section.bytes(), section.bytes()
		if section.err != nil || len(name) == 0 {
			d.err = ErrMessage
			return nil
		}
		out = append(out, Field{Name: string(name), Value: string(value)})
	}
	return out
}

func (d *decoder) padding() bool {
	for _, c := range d.b {
		if c != 0 {
			return false
		}
	}
	return true
}
