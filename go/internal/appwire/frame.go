// Package appwire is the box's half of the wire the FTW app speaks: the frame
// codec and the Noise session that carries it.
//
// The box is the Noise responder. The app holds the box's static key, read
// optically off the QR code, and initiates; that half lives in the app and has
// no counterpart here, because a relay that can present itself as a box is the
// one failure this pattern exists to prevent.
//
// The protocol is specified in srcfl/ftw-webapp docs/protocol.md, and the
// reference implementation is that repository's src/lib. Bytes produced by it
// are frozen in testdata/webapp-vectors.json and replayed by interop_test.go,
// so drift between the two fails a test here rather than a connection in
// somebody's house.
//
// Nothing in this package knows the fifteen message types. It moves opaque
// bodies, which is what lets the message layer change without touching crypto.
package appwire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/fxamacker/cbor/v2"
)

const (
	// FrameVersion is the frame layout version, byte 0 of every frame.
	FrameVersion = 1
	// HeaderBytes is the fixed header: ver, lane, flags, rsvd, len u16BE.
	HeaderBytes = 6
	// MaxPayload is what the u16 length field can describe.
	MaxPayload = 0xffff

	// LaneControl carries tick, delta, cmd, cmd.ack, cmd.result and event.
	LaneControl uint8 = 0
	// LaneBulk carries snapshots and history chunks.
	LaneBulk uint8 = 1

	// FlagTrunc says the delta did not fit and the rest follows.
	FlagTrunc uint8 = 0x02
)

// ControlBuckets are the only sizes lane 0 may take. One of them is chosen for
// the life of a session and never changes.
//
// The constancy is the privacy control, not a performance choice. A 1 Hz
// stream whose frame length follows what happened in the house tells the relay
// operator when the household wakes, cooks, leaves and comes home — through
// otherwise perfect encryption. So lane 0 is exactly one bucket, always, and
// never fragments.
//
// An array rather than a slice: the box is the end that emits the 1 Hz stream,
// so nothing downstream gets to edit the table this guarantee rests on.
var ControlBuckets = [2]int{256, 512}

// BulkBuckets step, because bulk already leaks that a transfer is happening.
// That is unavoidable and harmless; the 1 Hz stream is what must not leak, and
// it lives on lane 0.
var BulkBuckets = [3]int{1024, 4096, 16384}

// Frame codec failures. They never cross the wire, so they are not names from
// contract/registry.yaml; the carrier decides what a user is told.
var (
	ErrFrameTooLarge      = errors.New("appwire: payload exceeds the u16 length field")
	ErrFrameExceedsBucket = errors.New("appwire: frame does not fit its bucket")
	ErrFrameBucket        = errors.New("appwire: not a bucket this lane may use")
	ErrFrameShort         = errors.New("appwire: frame is shorter than its header")
	ErrFrameVersion       = errors.New("appwire: unsupported frame version")
	ErrFrameTruncated     = errors.New("appwire: declared length overruns the frame")
	ErrFrameCBOR          = errors.New("appwire: payload is not valid CBOR")
	ErrFrameEnvelope      = errors.New("appwire: envelope is not a map with a string type")
)

// Deterministic so a body encoded twice from equal values is equal on the
// wire; the relay must not be able to tell two identical readings apart by
// their key order.
var cborEnc = mustEncMode()

func mustEncMode() cbor.EncMode {
	mode, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic("appwire: " + err.Error())
	}
	return mode
}

// Envelope is the CBOR map every frame carries: {t, id?, b?}.
//
// Field names match go/internal/appproto's, which is the layer that fills
// these in. Two spellings of one wire format is how the two drift.
type Envelope struct {
	// T is the message type. A type this box has never heard of is not an
	// error — see DecodeFrame.
	T string
	// ID is the request id; replies echo it. Nil when absent.
	ID *uint32
	// B is the body, left encoded. Keeping it raw is what lets the message
	// layer own the fifteen shapes and this layer own none of them.
	B cbor.RawMessage
}

// MarshalBody encodes a message body with the same deterministic settings the
// envelope uses. The message layer goes through here so there is one encoder.
func MarshalBody(v any) (cbor.RawMessage, error) {
	b, err := cborEnc.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("appwire: encoding body: %w", err)
	}
	return b, nil
}

// Frame is one Noise transport message.
type Frame struct {
	Lane  uint8
	Flags uint8
	// Bucket is the padded size on the wire. DecodeFrame fills it in from what
	// arrived, so a caller can tell a 512-byte lane 0 frame from a 256-byte one
	// without measuring the slice itself.
	Bucket   int
	Envelope Envelope
}

// Truncated reports whether this frame's payload continues in a later one.
func (f Frame) Truncated() bool { return f.Flags&FlagTrunc != 0 }

// BulkBucketFor returns the smallest bulk bucket that holds the payload and
// its header. ok is false when no bucket does.
func BulkBucketFor(payloadBytes int) (bucket int, ok bool) {
	needed := payloadBytes + HeaderBytes
	for _, b := range BulkBuckets {
		if b >= needed {
			return b, true
		}
	}
	return 0, false
}

// EncodeFrame writes one frame, zero-padded to f.Bucket.
//
// It fails rather than growing the bucket. Silently emitting a larger frame
// would defeat the padding entirely — and the padding is the integrity check
// on the privacy claim, not spare room — so a caller that overruns lane 0 hears
// about it at the point it happens.
func EncodeFrame(f Frame) ([]byte, error) {
	// Checked here rather than left to the session layer because the box is
	// the end that emits the 1 Hz stream: an off-bucket lane 0 frame makes
	// frame length a function of what happened in the house, which is the one
	// thing the padding exists to prevent.
	if f.Lane == LaneControl && !slices.Contains(ControlBuckets[:], f.Bucket) {
		return nil, fmt.Errorf("%w: lane 0 bucket %d is not %v", ErrFrameBucket, f.Bucket, ControlBuckets)
	}

	payload, err := f.Envelope.marshal()
	if err != nil {
		return nil, err
	}

	if len(payload) > MaxPayload {
		return nil, fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, len(payload))
	}
	if needed := HeaderBytes + len(payload); needed > f.Bucket {
		return nil, fmt.Errorf("%w: needs %d bytes, bucket is %d", ErrFrameExceedsBucket, needed, f.Bucket)
	}

	out := make([]byte, f.Bucket) // zero-filled, which is the padding
	out[0] = FrameVersion
	out[1] = f.Lane
	out[2] = f.Flags
	out[3] = 0
	binary.BigEndian.PutUint16(out[4:6], uint16(len(payload)))
	copy(out[HeaderBytes:], payload)

	return out, nil
}

// DecodeFrame reads one frame. Everything past the declared length is ignored
// without inspection: AEAD already covers the padding, so validating it would
// only add a way to reject a frame that is fine.
//
// Unknown envelope keys and unknown message types are not errors. That is the
// rule that lets an app pinned in a service worker degrade instead of showing
// a white screen when it meets a newer box, and the reverse.
func DecodeFrame(b []byte) (Frame, error) {
	if len(b) < HeaderBytes {
		return Frame{}, fmt.Errorf("%w: %d bytes, need at least %d", ErrFrameShort, len(b), HeaderBytes)
	}
	if b[0] != FrameVersion {
		return Frame{}, fmt.Errorf("%w: %d", ErrFrameVersion, b[0])
	}

	length := int(binary.BigEndian.Uint16(b[4:6]))
	if HeaderBytes+length > len(b) {
		return Frame{}, fmt.Errorf("%w: declared %d in a %d-byte frame", ErrFrameTruncated, length, len(b))
	}

	f := Frame{Lane: b[1], Flags: b[2], Bucket: len(b)}
	if err := f.Envelope.unmarshal(b[HeaderBytes : HeaderBytes+length]); err != nil {
		return Frame{}, err
	}
	return f, nil
}

// Envelope key order is t, id, b — the order the reference implementation
// emits, so a frame this box builds is byte-identical to one the app builds
// from the same values. Decoding accepts any order, from either side.
var (
	keyType = []byte{0x61, 0x74}       // "t"
	keyID   = []byte{0x62, 0x69, 0x64} // "id"
	keyBody = []byte{0x61, 0x62}       // "b"
)

func (e Envelope) marshal() ([]byte, error) {
	entries := 1
	if e.ID != nil {
		entries++
	}
	if e.B != nil {
		entries++
	}

	typ, err := cborEnc.Marshal(e.T)
	if err != nil {
		return nil, fmt.Errorf("appwire: encoding envelope type: %w", err)
	}

	out := make([]byte, 0, len(typ)+len(e.B)+16)
	out = append(out, byte(0xa0|entries)) // a map of at most three pairs
	out = append(out, keyType...)
	out = append(out, typ...)

	if e.ID != nil {
		id, err := cborEnc.Marshal(*e.ID)
		if err != nil {
			return nil, fmt.Errorf("appwire: encoding envelope id: %w", err)
		}
		out = append(out, keyID...)
		out = append(out, id...)
	}
	if e.B != nil {
		out = append(out, keyBody...)
		out = append(out, e.B...)
	}

	return out, nil
}

func (e *Envelope) unmarshal(payload []byte) error {
	var fields map[string]cbor.RawMessage
	if err := cbor.Unmarshal(payload, &fields); err != nil {
		// A CBOR map with non-text keys lands here too, and it is the same
		// answer: this is not an envelope.
		return fmt.Errorf("%w: %v", ErrFrameCBOR, err)
	}

	raw, ok := fields["t"]
	if !ok {
		return fmt.Errorf("%w: no type", ErrFrameEnvelope)
	}
	var typ string
	if err := cbor.Unmarshal(raw, &typ); err != nil {
		return fmt.Errorf("%w: type is not a string", ErrFrameEnvelope)
	}
	e.T = typ

	// An id that is not a u32 is dropped rather than fatal, for the same
	// reason unknown keys are: a peer of another vintage still gets read.
	e.ID = nil
	if raw, ok := fields["id"]; ok {
		var id uint32
		if err := cbor.Unmarshal(raw, &id); err == nil {
			e.ID = &id
		}
	}

	e.B = fields["b"]
	return nil
}
