// Package subetha — AEAD framing layer.
//
// Wire format: uint32 (big-endian) length prefix followed by ciphertext.
// Each frame is independently sealed with chacha20-poly1305 using a per-direction
// key and a monotonically incrementing 12-byte nonce built from the counter.
//
// Nonce layout (12 bytes):
//
//	[0:4]  zero-padded (unused)
//	[4:12] uint64 counter, big-endian
//
// Replay protection: the counter only goes forward. A frame with a counter equal
// to or below the last-seen counter is rejected. Nonces are never reused within
// a single session.
//
// Max frame payload (before encryption): 64 KB.
// Max frame on the wire: 64 KB + 16 bytes AEAD tag + 4 bytes length prefix.
package subetha

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const (
	// maxFramePayload is the maximum plaintext payload per frame (64 KiB).
	maxFramePayload = 64 * 1024

	// nonceSize is the AEAD nonce length for chacha20-poly1305 (12 bytes).
	nonceSize = 12

	// keySize is the AEAD key length for chacha20-poly1305 (32 bytes).
	keySize = 32
)

// aeadKey derives a 32-byte session key for a given direction from the token.
// direction is a short human-readable label such as "host→relay" or "relay→host".
func aeadKey(token, direction string) ([]byte, error) {
	r := hkdf.New(sha256.New, []byte(token), nil, []byte("ftw-pair v1 "+direction))
	key := make([]byte, keySize)
	if _, err := io.ReadFull(r, key); err != nil {
		return nil, fmt.Errorf("hkdf derive key: %w", err)
	}
	return key, nil
}

// frameWriter wraps an io.Writer and emits length-prefixed AEAD-sealed frames.
// Safe for concurrent use; each Write is a single atomic frame.
type frameWriter struct {
	mu      sync.Mutex
	w       io.Writer
	aead    interface {
		Seal(dst, nonce, plaintext, additionalData []byte) []byte
		Overhead() int
	}
	counter uint64
}

func newFrameWriter(w io.Writer, key []byte) (*frameWriter, error) {
	a, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, fmt.Errorf("create chacha20poly1305 aead: %w", err)
	}
	return &frameWriter{w: w, aead: a}, nil
}

// WriteFrame seals plaintext and writes a length-prefixed frame.
// Returns an error if len(plaintext) > maxFramePayload.
func (fw *frameWriter) WriteFrame(plaintext []byte) error {
	if len(plaintext) > maxFramePayload {
		return fmt.Errorf("frame too large: %d > %d", len(plaintext), maxFramePayload)
	}
	fw.mu.Lock()
	defer fw.mu.Unlock()

	nonce := fw.nonce(fw.counter)
	fw.counter++

	ciphertext := fw.aead.Seal(nil, nonce, plaintext, nil)

	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(ciphertext)))

	if _, err := fw.w.Write(lenBuf[:]); err != nil {
		return err
	}
	if _, err := fw.w.Write(ciphertext); err != nil {
		return err
	}
	return nil
}

func (fw *frameWriter) nonce(counter uint64) []byte {
	n := make([]byte, nonceSize)
	binary.BigEndian.PutUint64(n[4:], counter)
	return n
}

// frameReader wraps an io.Reader and decodes length-prefixed AEAD-sealed frames.
// Not safe for concurrent use (caller must serialize reads).
type frameReader struct {
	r       io.Reader
	aead    interface {
		Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
		Overhead() int
	}
	counter uint64
}

var errReplay = errors.New("frame replay detected: counter did not advance")

func newFrameReader(r io.Reader, key []byte) (*frameReader, error) {
	a, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, fmt.Errorf("create chacha20poly1305 aead: %w", err)
	}
	return &frameReader{r: r, aead: a}, nil
}

// ReadFrame reads the next frame, decrypts it, and returns the plaintext.
// Returns io.EOF when the underlying reader is exhausted.
func (fr *frameReader) ReadFrame() ([]byte, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(fr.r, lenBuf[:]); err != nil {
		return nil, err // propagates io.EOF cleanly
	}
	frameLen := binary.BigEndian.Uint32(lenBuf[:])
	// Maximum wire size = maxFramePayload + AEAD overhead
	maxWire := uint32(maxFramePayload + fr.aead.Overhead())
	if frameLen > maxWire {
		return nil, fmt.Errorf("frame length %d exceeds maximum %d", frameLen, maxWire)
	}

	ciphertext := make([]byte, frameLen)
	if _, err := io.ReadFull(fr.r, ciphertext); err != nil {
		return nil, fmt.Errorf("read frame body: %w", err)
	}

	nonce := fr.nonce(fr.counter)
	plaintext, err := fr.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt frame (counter=%d): %w", fr.counter, err)
	}
	fr.counter++
	return plaintext, nil
}

func (fr *frameReader) nonce(counter uint64) []byte {
	n := make([]byte, nonceSize)
	binary.BigEndian.PutUint64(n[4:], counter)
	return n
}

// aeadPipe wraps a net.Conn-like ReadWriter to provide AEAD-framed I/O.
// Reads and writes are performed through frameReader/frameWriter respectively.
type aeadPipe struct {
	fw        *frameWriter
	fr        *frameReader
	readBuf   []byte
	readPos   int
}

func newAEADPipe(rw io.ReadWriter, writeKey, readKey []byte) (*aeadPipe, error) {
	fw, err := newFrameWriter(rw, writeKey)
	if err != nil {
		return nil, err
	}
	fr, err := newFrameReader(rw, readKey)
	if err != nil {
		return nil, err
	}
	return &aeadPipe{fw: fw, fr: fr}, nil
}

// Write sends p as one or more frames (splitting at maxFramePayload boundaries).
func (p *aeadPipe) Write(b []byte) (int, error) {
	total := 0
	for len(b) > 0 {
		chunk := b
		if len(chunk) > maxFramePayload {
			chunk = b[:maxFramePayload]
		}
		if err := p.fw.WriteFrame(chunk); err != nil {
			return total, err
		}
		total += len(chunk)
		b = b[len(chunk):]
	}
	return total, nil
}

// Read fills buf from the decrypted payload of the next frame(s).
func (p *aeadPipe) Read(buf []byte) (int, error) {
	// Drain the previous frame's leftover bytes first.
	if p.readPos < len(p.readBuf) {
		n := copy(buf, p.readBuf[p.readPos:])
		p.readPos += n
		return n, nil
	}

	plain, err := p.fr.ReadFrame()
	if err != nil {
		return 0, err
	}
	n := copy(buf, plain)
	if n < len(plain) {
		p.readBuf = plain
		p.readPos = n
	} else {
		p.readBuf = nil
		p.readPos = 0
	}
	return n, nil
}
