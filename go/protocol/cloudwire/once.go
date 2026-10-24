package cloudwire

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"time"
)

// The one-shot read: a watch reads its home from FTW Cloud in one HTTPS
// request (ADR 0009, decision 7). watchOS gives an app HTTPS and nothing
// lower, so the app session's handshake runs inside a single POST.
//
// The watch posts to OncePath?home=<home id>, with content type
// application/octet-stream. The body is IK message 1 to a service Noise key,
// with OncePrologue(home) as the prologue, so a once handshake and an app
// session handshake can never stand in for each other. Message 1 carries one
// unsigned envelope: read, token.get, push.register, push.unregister or x
// (exchange.go). The answer body is message 2 with the reply and then, for a
// read, the
// envelopes that did not fit, each message after its length as u16 big
// endian. The service keeps no session and no Noise state after the answer.

// OncePath is the route, on cloud.ftw.energy.
const OncePath = "/v1/app/once"

// OncePrologueTag starts the prologue; the home id follows it.
const OncePrologueTag = "ftw.cloud.once.v1:"

// OncePrologue is the Noise prologue for one home's one-shot read.
func OncePrologue(home string) []byte {
	return append([]byte(OncePrologueTag), home...)
}

// Watch (or phone) to service, in message 1. Push registration uses
// MsgPushRegister and MsgPushUnregister from push.go.
const (
	// MsgRead asks for the home as the app session's snapshot shows it.
	MsgRead = "read"
	// MsgTokenGet asks for the device's extension token.
	MsgTokenGet = "token.get"
)

// Service to watch, in message 2.
const (
	// MsgToken answers token.get, as on the app session.
	MsgToken = "token"
)

// OnceMessages are the types message 1 may carry. The service refuses every
// other type, an owner's suspend among them, before message 2.
var OnceMessages = []string{MsgRead, MsgTokenGet, MsgPushRegister, MsgPushUnregister, MsgX}

// Read asks for the snapshot, less what the watch already holds.
type Read struct {
	Have Have `cbor:"have"`
	// Hz 1 keeps the box's watch in force for 60 s, as a looking phone does.
	// 0 leaves it as it is.
	Hz int `cbor:"hz"`
}

// Have is what a watch holds: the boot its envelopes come from and, per
// class, the highest seq it holds. Empty means nothing.
type Have struct {
	Boot []byte            `cbor:"boot"`
	Seq  map[string]uint64 `cbor:"seq"`
}

// Covers reports whether the watch already holds a box envelope: one from
// the boot it names, with a seq no higher than the highest it holds for that
// class. An envelope that cannot be read is not covered, so it is sent.
func (h Have) Covers(envelope []byte) bool {
	if len(h.Boot) == 0 {
		return false
	}
	env, err := DecodeEnvelope(envelope)
	if err != nil {
		return false
	}
	var header Header
	if err := Unmarshal(env.B, &header); err != nil {
		return false
	}
	held, ok := h.Seq[env.T]
	return ok && bytes.Equal(header.Boot, h.Boot) && header.Seq <= held
}

// Token is the extension token a device reads GET /v1/now with.
type Token struct {
	Token      string `cbor:"token"`
	ExpiresInS int64  `cbor:"expiresInS"`
}

// Limits on the one-shot read. Past them the service answers 429, or 400 for
// a message 1 that is too long.
const (
	OnceMessage1MaxBytes = 4096
	OnceReadsPerMinute   = 30
	// OnceInFlight is how many reads one device may have open at once.
	OnceInFlight = 1
	// OnceAnswerWithin bounds the whole request at the service.
	OnceAnswerWithin = 10 * time.Second
)

// OnceAnswer frames an answer body: message 2, then each transport message,
// every one after its length as u16 big endian.
func OnceAnswer(messages ...[]byte) ([]byte, error) {
	if len(messages) == 0 {
		return nil, fmt.Errorf("%w: an answer needs message 2", ErrFrame)
	}
	var out []byte
	for i, m := range messages {
		if len(m) > MaxPayload {
			return nil, fmt.Errorf("%w: message %d is %d bytes", ErrFrame, i, len(m))
		}
		out = binary.BigEndian.AppendUint16(out, uint16(len(m)))
		out = append(out, m...)
	}
	return out, nil
}

// SplitOnceAnswer reads an answer body back into message 2 and the transport
// messages after it.
func SplitOnceAnswer(body []byte) ([][]byte, error) {
	var out [][]byte
	for len(body) > 0 {
		if len(body) < 2 {
			return nil, fmt.Errorf("%w: a length cut short", ErrFrame)
		}
		n := int(binary.BigEndian.Uint16(body))
		if len(body) < 2+n {
			return nil, fmt.Errorf("%w: a %d-byte message overruns the body", ErrFrame, n)
		}
		out = append(out, body[2:2+n])
		body = body[2+n:]
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: an empty answer", ErrFrame)
	}
	return out, nil
}
