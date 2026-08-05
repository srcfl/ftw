package appwire

import (
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"
)

// Noise_IK_25519_ChaChaPoly_SHA256, responder half.
//
//	IK:
//	  <- s
//	  ...
//	  -> e, es, s, ss
//	  <- e, ee, se
//
// IK is the pattern because the initiator already knows the responder's static
// key: the app reads it off the QR code on the box, so the trust anchor never
// travels through Sourceful's cloud. A hostile or compelled relay can drop
// frames, but it cannot present itself as a box — it would have to produce a
// valid DH against a key it has never seen.
//
// Spec: https://noiseprotocol.org/noise.html sections 5 and 7. Verified
// against the Cacophony vector in noise_test.go, which is what proves this
// agrees with every other implementation rather than only with itself.
const (
	// ProtocolName is hashed into the handshake, so both ends must spell it
	// identically or nothing decrypts.
	ProtocolName = "Noise_IK_25519_ChaChaPoly_SHA256"

	DHBytes   = 32
	KeyBytes  = 32
	HashBytes = 32
	TagBytes  = 16

	// MaxNonce is reserved by the spec and must never encrypt.
	MaxNonce = uint64(math.MaxUint64)

	// Message1Overhead is the first handshake message before any payload.
	Message1Overhead = DHBytes + DHBytes + TagBytes + TagBytes
	// Message2Overhead is the second, before any payload.
	Message2Overhead = DHBytes + TagBytes
)

// Session failures. Like the frame errors these stay local; the carrier maps
// them to whatever the app is told.
var (
	ErrNoiseKey       = errors.New("appwire: bad key")
	ErrNoiseDH        = errors.New("appwire: X25519 failed")
	ErrNoiseState     = errors.New("appwire: handshake step")
	ErrNoiseMessage   = errors.New("appwire: malformed message")
	ErrNoiseClosed    = errors.New("appwire: session is closed")
	ErrNonceExhausted = errors.New("appwire: nonce space exhausted")

	// ErrNoiseAuth is deliberately the only thing said about a decryption
	// failure. Which check failed is exactly what a chosen-ciphertext attacker
	// is probing for.
	ErrNoiseAuth = errors.New("appwire: authentication failed")
)

// KeyPair is one X25519 static or ephemeral key. The zero value is unusable.
type KeyPair struct {
	priv *ecdh.PrivateKey
}

// GenerateKeyPair draws a fresh key from the system source.
func GenerateKeyPair() (KeyPair, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return KeyPair{}, fmt.Errorf("%w: %v", ErrNoiseKey, err)
	}
	return KeyPair{priv: priv}, nil
}

// KeyPairFromSecret loads a stored static key.
func KeyPairFromSecret(secret []byte) (KeyPair, error) {
	if len(secret) != DHBytes {
		return KeyPair{}, fmt.Errorf("%w: static key is %d bytes, need %d", ErrNoiseKey, len(secret), DHBytes)
	}
	priv, err := ecdh.X25519().NewPrivateKey(secret)
	if err != nil {
		return KeyPair{}, fmt.Errorf("%w: %v", ErrNoiseKey, err)
	}
	return KeyPair{priv: priv}, nil
}

// Public returns the public half, freshly copied so a caller cannot edit it.
func (k KeyPair) Public() []byte {
	if k.priv == nil {
		return nil
	}
	pub := k.priv.PublicKey().Bytes()
	out := make([]byte, len(pub))
	copy(out, pub)
	return out
}

func (k KeyPair) dh(peer []byte) ([]byte, error) {
	if k.priv == nil {
		return nil, fmt.Errorf("%w: no key", ErrNoiseKey)
	}
	pub, err := ecdh.X25519().NewPublicKey(peer)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoiseDH, err)
	}
	// crypto/ecdh rejects an all-zero shared secret. The spec permits
	// accepting it; rejecting is strictly safer and only fires on a low-order
	// point, which an honest peer never sends.
	shared, err := k.priv.ECDH(pub)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoiseDH, err)
	}
	return shared, nil
}

// CipherState is one direction's key and counter.
//
// The counter never wraps. Reusing a (key, nonce) pair under ChaCha20-Poly1305
// hands an observer the XOR of two plaintexts and, worse, makes the Poly1305
// authenticator forgeable — so exhaustion fails and the session dies. Rekey is
// where that limit would be lifted and it is not in v1: at 1 Hz on lane 0,
// 2^64 frames is longer than the universe has run.
type CipherState struct {
	mu        sync.Mutex
	key       []byte
	nonce     uint64
	destroyed bool
}

// NewCipherState wraps a 32-byte key. A nil key is the handshake's
// before-first-mixKey state, where the spec passes data through.
func NewCipherState(key []byte) *CipherState { return &CipherState{key: key} }

// HasKey reports whether this cipher encrypts anything.
func (c *CipherState) HasKey() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.key != nil
}

// Nonce is the next counter value, and also the sequence number Transport puts
// on the wire.
func (c *CipherState) Nonce() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nonce
}

// SetNonce jumps the counter, for a carrier that may reorder or drop. It only
// ever moves to a sequence number actually received; which numbers are
// acceptable is Transport's decision, not this one's.
func (c *CipherState) SetNonce(n uint64) error {
	if n >= MaxNonce {
		return fmt.Errorf("%w: nonce %d is reserved", ErrNonceExhausted, n)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nonce = n
	return nil
}

// EncryptWithAD encrypts under the next counter value, authenticating ad.
func (c *CipherState) EncryptWithAD(ad, plaintext []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.destroyed {
		return nil, ErrNoiseClosed
	}
	// Before the first mixKey there is no key. That is a handshake-only state
	// and unreachable after Split, which is why destroyed is a separate flag
	// rather than a nil key — a destroyed cipher must never quietly emit
	// plaintext.
	if c.key == nil {
		return plaintext, nil
	}

	aead, nonce, err := c.take()
	if err != nil {
		return nil, err
	}
	return aead.Seal(nil, nonce, plaintext, ad), nil
}

// DecryptWithAD decrypts under the next counter value, authenticating ad.
func (c *CipherState) DecryptWithAD(ad, ciphertext []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.destroyed {
		return nil, ErrNoiseClosed
	}
	if c.key == nil {
		return ciphertext, nil
	}

	aead, nonce, err := c.take()
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, ad)
	if err != nil {
		return nil, ErrNoiseAuth
	}
	return plaintext, nil
}

// Destroy wipes the key. A closed session must not leave one reachable.
func (c *CipherState) Destroy() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.key {
		c.key[i] = 0
	}
	c.key = nil
	c.destroyed = true
}

// take consumes one counter value. Callers hold c.mu.
func (c *CipherState) take() (cipher.AEAD, []byte, error) {
	if c.nonce >= MaxNonce {
		return nil, nil, ErrNonceExhausted
	}
	aead, err := chacha20poly1305.New(c.key)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrNoiseKey, err)
	}
	nonce := noiseNonce(c.nonce)
	c.nonce++
	return aead, nonce, nil
}

// noiseNonce is ChaChaPoly's 96-bit nonce as Noise encodes it: four zero
// bytes, then the 64-bit counter little-endian.
func noiseNonce(n uint64) []byte {
	out := make([]byte, chacha20poly1305.NonceSize)
	for i := 4; i < 12; i++ {
		out[i] = byte(n)
		n >>= 8
	}
	return out
}

// symmetricState is the chaining key, the handshake hash, and the cipher that
// protects handshake payloads.
type symmetricState struct {
	ck     []byte
	h      []byte
	cipher *CipherState
}

func newSymmetricState() *symmetricState {
	name := []byte(ProtocolName)
	h := make([]byte, HashBytes)
	if len(name) <= HashBytes {
		copy(h, name)
	} else {
		sum := sha256.Sum256(name)
		copy(h, sum[:])
	}
	ck := make([]byte, HashBytes)
	copy(ck, h)
	return &symmetricState{ck: ck, h: h, cipher: NewCipherState(nil)}
}

func (s *symmetricState) mixHash(data []byte) {
	sum := sha256.Sum256(append(append([]byte{}, s.h...), data...))
	s.h = sum[:]
}

func (s *symmetricState) mixKey(ikm []byte) error {
	out, err := hkdfN(s.ck, ikm, 2)
	if err != nil {
		return err
	}
	s.ck = out[0]
	s.cipher = NewCipherState(out[1])
	return nil
}

// hkdfN is Noise's HKDF: one extract keyed by the chaining key, then expand
// with empty info. Taking outputs*32 off a single expand is byte-identical to
// the spec's chained HMAC construction.
func hkdfN(chainingKey, ikm []byte, outputs int) ([][]byte, error) {
	prk, err := hkdf.Extract(sha256.New, ikm, chainingKey)
	if err != nil {
		return nil, fmt.Errorf("appwire: hkdf extract: %w", err)
	}
	okm, err := hkdf.Expand(sha256.New, prk, "", outputs*HashBytes)
	if err != nil {
		return nil, fmt.Errorf("appwire: hkdf expand: %w", err)
	}

	out := make([][]byte, outputs)
	for i := range out {
		out[i] = okm[i*HashBytes : (i+1)*HashBytes : (i+1)*HashBytes]
	}
	return out, nil
}

func (s *symmetricState) encryptAndHash(plaintext []byte) ([]byte, error) {
	ciphertext, err := s.cipher.EncryptWithAD(s.h, plaintext)
	if err != nil {
		return nil, err
	}
	s.mixHash(ciphertext)
	return ciphertext, nil
}

func (s *symmetricState) decryptAndHash(ciphertext []byte) ([]byte, error) {
	plaintext, err := s.cipher.DecryptWithAD(s.h, ciphertext)
	if err != nil {
		return nil, err
	}
	s.mixHash(ciphertext)
	return plaintext, nil
}

func (s *symmetricState) split() ([]*CipherState, error) {
	out, err := hkdfN(s.ck, nil, 2)
	if err != nil {
		return nil, err
	}
	return []*CipherState{NewCipherState(out[0]), NewCipherState(out[1])}, nil
}

// Session is what a finished handshake hands over.
type Session struct {
	// Send encrypts what this box sends.
	Send *CipherState
	// Recv decrypts what it receives.
	Recv *CipherState
	// HandshakeHash binds an external channel to this session, and is also a
	// session id.
	HandshakeHash []byte
	// RemoteStatic is the app's static key as the handshake authenticated it,
	// not as it was claimed.
	RemoteStatic []byte
}

// HandshakeOptions configures the responder.
type HandshakeOptions struct {
	// StaticKey is the box's long-term key, the one printed in the QR code.
	StaticKey KeyPair
	// Prologue is mixed in before anything else. Both ends must agree on it
	// byte for byte.
	Prologue []byte
	// Ephemeral fixes the ephemeral key. Test vectors only — generating it is
	// the entire source of forward secrecy.
	Ephemeral *KeyPair
}

type step int

const (
	stepRead1 step = iota
	stepWrite2
	stepDone
	// stepSplit is terminal: the keys have been handed over and cannot be
	// minted again.
	stepSplit
)

// HandshakeState runs the responder side of IK. Drive it with ReadMessage then
// WriteMessage then Split; anything else fails rather than quietly producing a
// message the peer cannot read.
type HandshakeState struct {
	mu   sync.Mutex
	sym  *symmetricState
	s    KeyPair
	e    KeyPair
	rs   []byte
	re   []byte
	step step
}

// NewResponder starts a handshake with the box's static key.
func NewResponder(opts HandshakeOptions) (*HandshakeState, error) {
	if opts.StaticKey.priv == nil {
		return nil, fmt.Errorf("%w: responder needs a static key", ErrNoiseKey)
	}

	h := &HandshakeState{sym: newSymmetricState(), s: opts.StaticKey, step: stepRead1}
	if opts.Ephemeral != nil {
		h.e = *opts.Ephemeral
	}

	h.sym.mixHash(opts.Prologue)
	// IK's pre-message `<- s`: both ends hash the responder's static key
	// before the first byte moves. That is what makes a relay substituting its
	// own key fail at the first decryption instead of somewhere further up.
	h.sym.mixHash(opts.StaticKey.Public())
	return h, nil
}

// ReadMessage consumes `-> e, es, s, ss` and returns its payload.
func (h *HandshakeState) ReadMessage(message []byte) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.step != stepRead1 {
		return nil, fmt.Errorf("%w: cannot read now", ErrNoiseState)
	}

	staticEnd := DHBytes + DHBytes + TagBytes
	if len(message) < staticEnd+TagBytes {
		return nil, fmt.Errorf("%w: handshake message 1 is %d bytes", ErrNoiseMessage, len(message))
	}

	h.re = append([]byte{}, message[:DHBytes]...)
	h.sym.mixHash(h.re)

	es, err := h.s.dh(h.re)
	if err != nil {
		return nil, err
	}
	if err := h.sym.mixKey(es); err != nil {
		return nil, err
	}

	// Fails here when the initiator pinned somebody else's static key, which
	// is exactly the relay-impersonation case.
	rs, err := h.sym.decryptAndHash(message[DHBytes:staticEnd])
	if err != nil {
		return nil, err
	}
	h.rs = rs

	ss, err := h.s.dh(h.rs)
	if err != nil {
		return nil, err
	}
	if err := h.sym.mixKey(ss); err != nil {
		return nil, err
	}

	payload, err := h.sym.decryptAndHash(message[staticEnd:])
	if err != nil {
		return nil, err
	}

	h.step = stepWrite2
	return payload, nil
}

// WriteMessage produces `<- e, ee, se` carrying payload.
func (h *HandshakeState) WriteMessage(payload []byte) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.step != stepWrite2 {
		return nil, fmt.Errorf("%w: cannot write now", ErrNoiseState)
	}

	if h.e.priv == nil {
		e, err := GenerateKeyPair()
		if err != nil {
			return nil, err
		}
		h.e = e
	}

	epub := h.e.Public()
	h.sym.mixHash(epub)

	ee, err := h.e.dh(h.re)
	if err != nil {
		return nil, err
	}
	if err := h.sym.mixKey(ee); err != nil {
		return nil, err
	}

	se, err := h.e.dh(h.rs)
	if err != nil {
		return nil, err
	}
	if err := h.sym.mixKey(se); err != nil {
		return nil, err
	}

	encrypted, err := h.sym.encryptAndHash(payload)
	if err != nil {
		return nil, err
	}

	h.step = stepDone
	return append(epub, encrypted...), nil
}

// Complete reports whether the pattern has run to its end.
func (h *HandshakeState) Complete() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.step == stepDone
}

// Split ends the handshake and hands over the transport keys.
//
// It consumes the handshake: a second call fails. Splitting twice would mint
// two send ciphers holding the same key, both starting at nonce 0, so the
// first frame of each would encrypt under an identical (key, nonce) pair —
// which hands an observer the XOR of two plaintexts and makes Poly1305
// authenticators forgeable. The obvious way to write reconnection is to keep
// the handshake and split again, so it has to be refused here rather than
// remembered by every future caller.
func (h *HandshakeState) Split() (*Session, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.step == stepSplit {
		return nil, fmt.Errorf("%w: handshake already split", ErrNoiseState)
	}
	if h.step != stepDone {
		return nil, fmt.Errorf("%w: handshake is not finished", ErrNoiseState)
	}

	keys, err := h.sym.split()
	if err != nil {
		return nil, err
	}

	// The initiator sends under the first key and receives under the second,
	// so the responder is the other way round.
	session := &Session{
		Send:          keys[1],
		Recv:          keys[0],
		HandshakeHash: append([]byte{}, h.sym.h...),
		RemoteStatic:  append([]byte{}, h.rs...),
	}

	h.step = stepSplit
	return session, nil
}
