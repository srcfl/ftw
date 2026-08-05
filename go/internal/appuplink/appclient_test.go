package appuplink

import (
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

// The app's half of Noise_IK_25519_ChaChaPoly_SHA256, written out here.
//
// go/internal/appwire has the responder only, because a box that could
// initiate is a box that could present itself as an app. So the initiator
// lives in this test file, where it does two jobs: it drives the box end to
// end, and it is an independent second implementation of the pattern — a
// responder tested only against a helper that shares its own arithmetic
// proves nothing.
//
// The reference implementation is srcfl/ftw-webapp src/lib/crypto/noise.ts.

const (
	protocolName = "Noise_IK_25519_ChaChaPoly_SHA256"
	dhBytes      = 32
	hashBytes    = 32
)

type appSymmetric struct {
	ck  []byte
	h   []byte
	key []byte
	n   uint64
}

func newAppSymmetric() *appSymmetric {
	h := make([]byte, hashBytes)
	copy(h, protocolName)
	ck := make([]byte, hashBytes)
	copy(ck, h)
	return &appSymmetric{ck: ck, h: h}
}

func (s *appSymmetric) mixHash(data []byte) {
	sum := sha256.Sum256(append(append([]byte{}, s.h...), data...))
	s.h = sum[:]
}

func (s *appSymmetric) mixKey(ikm []byte) error {
	out, err := appHKDF(s.ck, ikm, 2)
	if err != nil {
		return err
	}
	s.ck = out[0]
	s.key = out[1]
	s.n = 0
	return nil
}

func appHKDF(chainingKey, ikm []byte, outputs int) ([][]byte, error) {
	prk, err := hkdf.Extract(sha256.New, ikm, chainingKey)
	if err != nil {
		return nil, err
	}
	okm, err := hkdf.Expand(sha256.New, prk, "", outputs*hashBytes)
	if err != nil {
		return nil, err
	}
	out := make([][]byte, outputs)
	for i := range out {
		out[i] = okm[i*hashBytes : (i+1)*hashBytes]
	}
	return out, nil
}

func appNonce(n uint64) []byte {
	out := make([]byte, chacha20poly1305.NonceSize)
	for i := 4; i < 12; i++ {
		out[i] = byte(n)
		n >>= 8
	}
	return out
}

func (s *appSymmetric) encryptAndHash(plaintext []byte) ([]byte, error) {
	if s.key == nil {
		s.mixHash(plaintext)
		return plaintext, nil
	}
	aead, err := chacha20poly1305.New(s.key)
	if err != nil {
		return nil, err
	}
	ciphertext := aead.Seal(nil, appNonce(s.n), plaintext, s.h)
	s.n++
	s.mixHash(ciphertext)
	return ciphertext, nil
}

func (s *appSymmetric) decryptAndHash(ciphertext []byte) ([]byte, error) {
	if s.key == nil {
		s.mixHash(ciphertext)
		return ciphertext, nil
	}
	aead, err := chacha20poly1305.New(s.key)
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, appNonce(s.n), ciphertext, s.h)
	if err != nil {
		return nil, err
	}
	s.n++
	s.mixHash(ciphertext)
	return plaintext, nil
}

// appClient is one phone: a static key, a Noise session and a sequence
// counter. It carries no state the real app does not.
type appClient struct {
	static *ecdh.PrivateKey
	e      *ecdh.PrivateKey
	sym    *appSymmetric

	send    []byte
	recv    []byte
	sendSeq uint64
	recvSeq uint64
}

func newAppClient() (*appClient, error) {
	static, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &appClient{static: static}, nil
}

func (a *appClient) staticPublic() []byte { return a.static.PublicKey().Bytes() }

// message1 is `-> e, es, s, ss`, carrying the pairing code as its payload.
func (a *appClient) message1(boxStatic, payload []byte) ([]byte, error) {
	rs, err := ecdh.X25519().NewPublicKey(boxStatic)
	if err != nil {
		return nil, err
	}

	a.sym = newAppSymmetric()
	a.sym.mixHash(prologue(boxStatic))
	a.sym.mixHash(boxStatic)

	a.e, err = ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	epub := a.e.PublicKey().Bytes()
	a.sym.mixHash(epub)

	es, err := a.e.ECDH(rs)
	if err != nil {
		return nil, err
	}
	if err := a.sym.mixKey(es); err != nil {
		return nil, err
	}

	encStatic, err := a.sym.encryptAndHash(a.staticPublic())
	if err != nil {
		return nil, err
	}

	ss, err := a.static.ECDH(rs)
	if err != nil {
		return nil, err
	}
	if err := a.sym.mixKey(ss); err != nil {
		return nil, err
	}

	encPayload, err := a.sym.encryptAndHash(payload)
	if err != nil {
		return nil, err
	}

	return append(append(epub, encStatic...), encPayload...), nil
}

// readMessage2 consumes `<- e, ee, se` and splits.
func (a *appClient) readMessage2(message []byte) error {
	if len(message) < dhBytes {
		return fmt.Errorf("message 2 is %d bytes", len(message))
	}

	re, err := ecdh.X25519().NewPublicKey(message[:dhBytes])
	if err != nil {
		return err
	}
	a.sym.mixHash(message[:dhBytes])

	ee, err := a.e.ECDH(re)
	if err != nil {
		return err
	}
	if err := a.sym.mixKey(ee); err != nil {
		return err
	}
	se, err := a.static.ECDH(re)
	if err != nil {
		return err
	}
	if err := a.sym.mixKey(se); err != nil {
		return err
	}
	if _, err := a.sym.decryptAndHash(message[dhBytes:]); err != nil {
		return err
	}

	keys, err := appHKDF(a.sym.ck, nil, 2)
	if err != nil {
		return err
	}
	// The initiator sends under the first key and receives under the second.
	a.send, a.recv = keys[0], keys[1]
	return nil
}

// encrypt wraps a frame as the transport does: an eight-byte counter in the
// clear, authenticated as associated data.
func (a *appClient) encrypt(frame []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(a.send)
	if err != nil {
		return nil, err
	}
	seq := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		seq[i] = byte(a.sendSeq >> (8 * (7 - i)))
	}
	body := aead.Seal(nil, appNonce(a.sendSeq), frame, seq)
	a.sendSeq++
	return append(seq, body...), nil
}

func (a *appClient) decrypt(message []byte) ([]byte, error) {
	if len(message) < 8+16 {
		return nil, fmt.Errorf("transport message is %d bytes", len(message))
	}
	aead, err := chacha20poly1305.New(a.recv)
	if err != nil {
		return nil, err
	}
	var seq uint64
	for _, b := range message[:8] {
		seq = seq<<8 | uint64(b)
	}
	frame, err := aead.Open(nil, appNonce(seq), message[8:], message[:8])
	if err != nil {
		return nil, err
	}
	a.recvSeq = seq
	return frame, nil
}
