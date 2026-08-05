// Package appproto speaks the FTW web app's client protocol.
//
// It owns message semantics only: what a hello_ok says, which lane a reply
// belongs on, when a command may be dispatched, what a plan slot means. The
// bytes underneath — frame header, bucket padding and the Noise session —
// belong to go/internal/appwire, which this package reaches through the narrow
// FrameCodec interface below.
//
// Two rules run through everything here and are worth stating once:
//
// Every age is measured against the box's uptime, never its wall clock. A Pi
// has no RTC and reads 1970 until NTP answers, so a wall-clock age would be
// wrong by decades exactly when a user is looking at it after a power cut.
//
// Nothing in this package writes hardware. It validates intent and hands it to
// control, which is where the safety invariants live.
package appproto

import (
	"fmt"

	"github.com/fxamacker/cbor/v2"
)

// Protocol versions. The client sends a range; the box picks
// min(box max, client max) and degrades rather than refusing.
const (
	// ProtoMin is the oldest version this box will speak at all.
	ProtoMin = 0
	// ProtoMax is the newest.
	ProtoMax = 1
	// ProtoFloor is the frozen subset: fields 1-9 and nothing else. An app
	// too old for full mode still draws the core view here instead of
	// showing a white screen, which is what a hard version wall would give
	// anyone whose service worker pinned an old bundle.
	ProtoFloor = 0
)

// Lanes. Lane 0 is one fixed bucket at a fixed cadence for the life of a
// session, because a frame size that varies with what happened in the house
// leaks the household's load pattern through otherwise perfect encryption.
// Anything whose size depends on what the box supports goes on lane 1.
const (
	LaneControl uint8 = 0
	LaneBulk    uint8 = 1
)

// FlagTrunc says the delta did not fit its bucket. The rest follows on the
// next tick or as a fresh snapshot on the bulk lane; the bucket never grows.
const FlagTrunc uint8 = 0x02

// Bulk bucket sizes, smallest first.
var BulkBuckets = []int{1024, 4096, 16384}

// Envelope is one message. Deterministic CBOR with text keys.
//
// Unknown keys are ignored on decode and unknown message types are ignored on
// dispatch, in both directions. That is the single rule that lets a newer app
// talk to an older box and the reverse.
type Envelope struct {
	// T is the message type, a stable string.
	T string `cbor:"t"`
	// ID is a request id, u32. Replies echo it. Nil on unsolicited messages.
	ID *uint32 `cbor:"id,omitempty"`
	// B is the body, left encoded so the handler can unmarshal it into the
	// type the message name implies.
	B cbor.RawMessage `cbor:"b,omitempty"`
}

// Frame is one envelope on one lane. go/internal/appwire owns the byte layout,
// the padding and the Noise session; this package only decides what goes in a
// frame and which lane carries it.
type Frame struct {
	Lane  uint8
	Flags uint8
	// Bucket is the padded size the frame must be encoded to.
	Bucket   int
	Envelope Envelope
}

// FrameCodec is the whole of go/internal/appwire that this package needs.
// It is declared here rather than imported so the message layer builds,
// tests and reviews on its own.
type FrameCodec interface {
	EncodeFrame(f Frame) ([]byte, error)
	DecodeFrame(b []byte) (Frame, error)
}

// Deterministic encoding, per RFC 8949 core determinism. Both ends agree on
// the bytes so a frame length never depends on which map order a runtime
// happened to choose.
var (
	encMode cbor.EncMode
	decMode cbor.DecMode
)

func init() {
	var err error
	if encMode, err = cbor.CoreDetEncOptions().EncMode(); err != nil {
		panic(err)
	}
	// Duplicate keys are refused; unknown ones are not. A duplicate key is
	// two different claims about the same field and there is no safe way to
	// pick one. An unknown key is just a newer peer.
	opts := cbor.DecOptions{DupMapKey: cbor.DupMapKeyEnforcedAPF}
	if decMode, err = opts.DecMode(); err != nil {
		panic(err)
	}
}

// Marshal encodes a body deterministically.
func Marshal(v any) (cbor.RawMessage, error) {
	b, err := encMode.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("appproto: encode body: %w", err)
	}
	return b, nil
}

// Unmarshal decodes a body. Unknown keys are dropped without complaint.
func Unmarshal(raw cbor.RawMessage, v any) error {
	if len(raw) == 0 {
		return fmt.Errorf("appproto: empty body")
	}
	if err := decMode.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("appproto: decode body: %w", err)
	}
	return nil
}

// EncodeEnvelope renders one envelope to CBOR.
func EncodeEnvelope(e Envelope) ([]byte, error) {
	return encMode.Marshal(e)
}

// DecodeEnvelope parses one envelope, leaving the body encoded.
func DecodeEnvelope(b []byte) (Envelope, error) {
	var e Envelope
	if err := decMode.Unmarshal(b, &e); err != nil {
		return Envelope{}, fmt.Errorf("appproto: decode envelope: %w", err)
	}
	if e.T == "" {
		return Envelope{}, fmt.Errorf("appproto: envelope has no type")
	}
	return e, nil
}

// newEnvelope builds an outgoing envelope around a body.
func newEnvelope(t string, id *uint32, body any) (Envelope, error) {
	raw, err := Marshal(body)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{T: t, ID: id, B: raw}, nil
}

// bulkBucketFor is the smallest bulk bucket a payload fits in, or 0 when it
// fits none. Sizing by the encoded body rather than by a guess keeps a large
// mode catalogue from silently overrunning a bucket.
func bulkBucketFor(payloadBytes int) int {
	// The frame header costs bytes the codec adds; leave room for it rather
	// than have appwire discover the overrun.
	const headerBytes = 6
	for _, b := range BulkBuckets {
		if payloadBytes+headerBytes <= b {
			return b
		}
	}
	return 0
}
