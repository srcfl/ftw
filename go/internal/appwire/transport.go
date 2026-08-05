package appwire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
)

// The encrypted channel, once the handshake is done.
//
// One frame in, one transport message out:
//
//	offset  field       note
//	0       seq   u64BE  cleartext, authenticated as associated data
//	8       body        ChaCha20-Poly1305 over the frame, tag included
//
// Why a sequence number is on the wire at all: Noise's own counter is implicit
// and assumes an ordered, lossless carrier. The relay is a WebSocket and gives
// us that, but the LAN carrier is a WebRTC DataChannel, which may reorder and
// may drop. Carrying the counter lets the receiver set the nonce for whatever
// actually arrived, so one implementation serves both carriers and a lost
// frame costs one frame instead of the session.
//
// The eight bytes are a monotonic counter and nothing else, so frame size stays
// constant — the property lane 0 depends on.
const (
	SeqBytes          = 8
	TransportOverhead = SeqBytes + TagBytes

	// ReplayWindow is how far out of order a frame may arrive and still be
	// accepted. At 1 Hz on lane 0 that is a minute of tolerance, far beyond
	// any reordering a DataChannel produces, and small enough that the window
	// is one machine word.
	ReplayWindow = 64
)

// ErrReplay is a frame already accepted, or one older than the window.
var ErrReplay = errors.New("appwire: replayed or out-of-window sequence")

// Transport is a live Noise session: two directional keys, a counter and a
// replay window. It knows nothing about frames beyond their being bytes, so
// the frame codec and this can change independently.
//
// Safe for concurrent use: the carrier reads on one goroutine and the control
// tick writes on another.
type Transport struct {
	mu   sync.Mutex
	send *CipherState
	recv *CipherState

	// highestSeq is only meaningful once haveSeq is set. A flag rather than a
	// sentinel because sequence zero is a real, and the first, frame.
	highestSeq uint64
	haveSeq    bool
	seen       uint64
	closed     bool

	handshakeHash []byte
	remoteStatic  []byte
}

// NewTransport takes ownership of a finished handshake's keys.
func NewTransport(s *Session) *Transport {
	return &Transport{
		send:          s.Send,
		recv:          s.Recv,
		handshakeHash: s.HandshakeHash,
		remoteStatic:  s.RemoteStatic,
	}
}

// HandshakeHash identifies this session and binds external channels to it.
func (t *Transport) HandshakeHash() []byte {
	return append([]byte{}, t.handshakeHash...)
}

// RemoteStatic is the app's static key as the handshake authenticated it.
func (t *Transport) RemoteStatic() []byte {
	return append([]byte{}, t.remoteStatic...)
}

// NextSeq is the sequence number the next outgoing frame will carry.
func (t *Transport) NextSeq() uint64 { return t.send.Nonce() }

// Encrypt wraps one frame for the carrier.
func (t *Transport) Encrypt(frame []byte) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return nil, ErrNoiseClosed
	}

	seq := make([]byte, SeqBytes)
	binary.BigEndian.PutUint64(seq, t.send.Nonce())

	// Fails rather than wrapping the counter. See CipherState.
	body, err := t.send.EncryptWithAD(seq, frame)
	if err != nil {
		return nil, err
	}

	out := make([]byte, SeqBytes+len(body))
	copy(out, seq)
	copy(out[SeqBytes:], body)
	return out, nil
}

// Decrypt unwraps one transport message back into a frame.
func (t *Transport) Decrypt(message []byte) ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return nil, ErrNoiseClosed
	}
	if len(message) < TransportOverhead {
		return nil, fmt.Errorf("%w: transport message is %d bytes", ErrNoiseMessage, len(message))
	}

	seqBytes := message[:SeqBytes]
	seq := binary.BigEndian.Uint64(seqBytes)
	if err := t.checkReplay(seq); err != nil {
		return nil, err
	}

	if err := t.recv.SetNonce(seq); err != nil {
		return nil, err
	}
	// The sequence number is authenticated along with the frame, so an
	// attacker cannot renumber a captured message into the acceptable window.
	frame, err := t.recv.DecryptWithAD(seqBytes, message[SeqBytes:])
	if err != nil {
		return nil, err
	}

	// Only a frame that authenticated moves the window. Otherwise anyone able
	// to inject bytes could burn sequence numbers and lock out the real peer.
	t.markSeen(seq)
	return frame, nil
}

// Close drops both keys. After this the session is unusable, which is the
// point.
func (t *Transport) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.closed = true
	t.send.Destroy()
	t.recv.Destroy()
}

// checkReplay is called before decryption, so a forged frame is refused
// without touching the window. Callers hold t.mu.
func (t *Transport) checkReplay(seq uint64) error {
	if !t.haveSeq || seq > t.highestSeq {
		return nil
	}

	behind := t.highestSeq - seq
	if behind >= ReplayWindow {
		return fmt.Errorf("%w: sequence %d is outside the window", ErrReplay, seq)
	}
	if t.seen>>behind&1 == 1 {
		return fmt.Errorf("%w: sequence %d was already accepted", ErrReplay, seq)
	}
	return nil
}

// markSeen records an authenticated frame. Callers hold t.mu.
func (t *Transport) markSeen(seq uint64) {
	if !t.haveSeq {
		t.haveSeq = true
		t.highestSeq = seq
		t.seen = 1
		return
	}

	if seq > t.highestSeq {
		shift := seq - t.highestSeq
		if shift >= ReplayWindow {
			t.seen = 1
		} else {
			t.seen = t.seen<<shift | 1
		}
		t.highestSeq = seq
		return
	}

	t.seen |= 1 << (t.highestSeq - seq)
}
