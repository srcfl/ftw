package cloudwire

import "time"

// The exchange through FTW Cloud: a watch's commands to its box (ADR 0009,
// decisions 1 and 2). docs/protocol-cloud.md, "Exchange", is the normative
// text.
//
// A watch has HTTPS and nothing lower, so it cannot hold a box session the
// way a phone holds one in the tunnel. It runs the box's exchange instead:
// short Noise sessions (ftw.exchange.v1, IK to the box's static key) that
// live on the box, one request and one answer at a time. Each request rides
// one one-shot request to the service as x {d}. The service adds the device
// id its own handshake proved and the request's number, sends
// x {dev, id, d} down /v1/box, and waits for the box's x {dev, id, d} with
// the same number. Its d goes back to the watch in message 2, as x {d}.
// The service keeps nothing after the answer, and reads neither d.
//
// An empty d from the service means no answer came: the box is not
// connected, does not take x, kept silent, or did not answer in time. An
// empty d from the box is its silence: a stranger, a spent session or a
// message it could not read.

// MsgX is the exchange message on /v1/box and in a one-shot request.
const MsgX = "x"

// CapX in hello.caps says the box answers x. The service sends x to no
// other box: an older one closes the uplink on a type it does not know.
const CapX = MsgX

// Sizes. An exchange message is a handshake message 1 with one lane 0 frame
// (608 bytes), a request of one lane 0 frame (536) or an answer of two
// (1,072). XDataMax leaves room above the largest without letting d carry
// anything else.
const (
	XDataMax = 2048
	// MaxXFrame bounds one x frame on the uplink: d, the device id, the
	// number and the envelope around them.
	MaxXFrame = XDataMax + 256
)

// XAnswerWithin is how long the service waits for the box's answer. The box
// answers a command once its result settles, within 3 s, and no sooner than
// 400 ms after it arrived; a read and a handshake at once.
const XAnswerWithin = 5 * time.Second

// XInFlight is how many exchanges the service has open at one box at a time,
// as many as the box answers at once. Past it the service answers an empty
// d at once.
const XInFlight = 8

// X is the exchange message on /v1/box, both ways.
type X struct {
	// Dev is the roster device id of the watch. The service sets it from
	// the key its own handshake with the watch proved.
	Dev string `cbor:"dev"`
	// ID is the service's number for one request, which the box copies
	// into its answer. It means nothing else.
	ID uint32 `cbor:"id"`
	// D is one exchange message, or empty for no answer.
	D []byte `cbor:"d"`
}

// AppX is the exchange message in a one-shot request and its answer. It
// has no device id and no number: the service knows the device from its
// handshake, and one request has one answer.
type AppX struct {
	D []byte `cbor:"d"`
}
