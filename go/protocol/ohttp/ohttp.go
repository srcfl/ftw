// Package ohttp is Oblivious HTTP (RFC 9458) for one suite: DHKEM(X25519,
// HKDF-SHA256), HKDF-SHA256 and AES-128-GCM, with the known-length Binary
// HTTP messages (RFC 9292) it carries.
//
// A box sends its anonymous daily energy report this way: sealed to FTW
// Cloud's gateway key and posted to an independent relay. The relay sees the
// box's address and a ciphertext of fixed size. The gateway sees the report
// and the relay's address. Neither sees both.
//
// HPKE is the standard library's crypto/hpke. This package adds only the
// framing RFC 9458 puts around it and the response key schedule, and it
// speaks one suite so that no message can pick a weaker one.
package ohttp

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/hpke"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"slices"
)

// The one suite, by its HPKE ids.
const (
	KEMX25519     uint16 = 0x0020
	KDFHKDFSHA256 uint16 = 0x0001
	AEADAES128GCM uint16 = 0x0001
)

// Media types of RFC 9458, section 9.
const (
	RequestType  = "message/ohttp-req"
	ResponseType = "message/ohttp-res"
	KeysType     = "application/ohttp-keys"
)

const (
	// keyLen is Npk and Nenc for X25519.
	keyLen = 32
	// headerLen is the request header: key id, KEM, KDF and AEAD.
	headerLen = 7
	// The response key schedule for AES-128-GCM: Nk 16, Nn 12, and a
	// response nonce of max(Nn, Nk).
	aeadKeyLen   = 16
	aeadNonceLen = 12
	nonceLen     = 16
	tagLen       = 16

	requestLabel  = "message/bhttp request"
	responseLabel = "message/bhttp response"
)

var (
	ErrConfig      = errors.New("ohttp: invalid key configuration")
	ErrUnsupported = errors.New("ohttp: key configuration offers no supported suite")
	ErrRequest     = errors.New("ohttp: cannot decapsulate the request")
	ErrResponse    = errors.New("ohttp: cannot open the response")
)

// Algorithms is one KDF and AEAD pair a key configuration offers.
type Algorithms struct {
	KDF, AEAD uint16
}

var suite = Algorithms{KDF: KDFHKDFSHA256, AEAD: AEADAES128GCM}

// KeyConfig is a gateway's public key and what it accepts with it
// (RFC 9458, section 3).
type KeyConfig struct {
	ID         uint8
	KEM        uint16
	PublicKey  []byte
	Algorithms []Algorithms
}

// Marshal writes the configuration as RFC 9458 section 3.1 lays it out.
func (c KeyConfig) Marshal() []byte {
	b := make([]byte, 0, 3+len(c.PublicKey)+2+4*len(c.Algorithms))
	b = append(b, c.ID)
	b = binary.BigEndian.AppendUint16(b, c.KEM)
	b = append(b, c.PublicKey...)
	b = binary.BigEndian.AppendUint16(b, uint16(4*len(c.Algorithms)))
	for _, a := range c.Algorithms {
		b = binary.BigEndian.AppendUint16(b, a.KDF)
		b = binary.BigEndian.AppendUint16(b, a.AEAD)
	}
	return b
}

// ParseKeyConfig reads exactly one configuration. Only the X25519 KEM is
// known, because a key length cannot be read for a KEM this package does not
// speak.
func ParseKeyConfig(b []byte) (KeyConfig, error) {
	if len(b) < 3 {
		return KeyConfig{}, ErrConfig
	}
	c := KeyConfig{ID: b[0], KEM: binary.BigEndian.Uint16(b[1:3])}
	if c.KEM != KEMX25519 {
		return KeyConfig{}, ErrUnsupported
	}
	b = b[3:]
	if len(b) < keyLen+2 {
		return KeyConfig{}, ErrConfig
	}
	c.PublicKey = bytes.Clone(b[:keyLen])
	n := int(binary.BigEndian.Uint16(b[keyLen:]))
	b = b[keyLen+2:]
	if n < 4 || n%4 != 0 || len(b) != n {
		return KeyConfig{}, ErrConfig
	}
	for ; len(b) > 0; b = b[4:] {
		c.Algorithms = append(c.Algorithms, Algorithms{KDF: binary.BigEndian.Uint16(b), AEAD: binary.BigEndian.Uint16(b[2:])})
	}
	if _, err := hpke.DHKEM(ecdh.X25519()).NewPublicKey(c.PublicKey); err != nil {
		return KeyConfig{}, ErrConfig
	}
	return c, nil
}

// MarshalKeys writes application/ohttp-keys: each configuration after its
// two-byte length.
func MarshalKeys(configs ...KeyConfig) []byte {
	var b []byte
	for _, c := range configs {
		m := c.Marshal()
		b = binary.BigEndian.AppendUint16(b, uint16(len(m)))
		b = append(b, m...)
	}
	return b
}

// ParseKeys reads application/ohttp-keys. A configuration for another KEM is
// skipped, as RFC 9458 section 3.2 allows; a malformed one fails the whole
// list.
func ParseKeys(b []byte) ([]KeyConfig, error) {
	var out []KeyConfig
	for len(b) > 0 {
		if len(b) < 2 {
			return nil, ErrConfig
		}
		n := int(binary.BigEndian.Uint16(b))
		if len(b) < 2+n {
			return nil, ErrConfig
		}
		c, err := ParseKeyConfig(b[2 : 2+n])
		switch {
		case errors.Is(err, ErrUnsupported):
		case err != nil:
			return nil, err
		default:
			out = append(out, c)
		}
		b = b[2+n:]
	}
	return out, nil
}

func header(id uint8) []byte {
	h := []byte{id}
	h = binary.BigEndian.AppendUint16(h, KEMX25519)
	h = binary.BigEndian.AppendUint16(h, suite.KDF)
	return binary.BigEndian.AppendUint16(h, suite.AEAD)
}

func info(hdr []byte) []byte {
	return append(append([]byte(requestLabel), 0), hdr...)
}

// ClientContext opens the one response to one request.
type ClientContext struct {
	enc    []byte
	sender *hpke.Sender
}

// Encapsulate seals msg to the gateway that published cfg (RFC 9458,
// section 4.3). A configuration that does not offer this package's suite is
// refused, never downgraded.
func Encapsulate(cfg KeyConfig, msg []byte) ([]byte, *ClientContext, error) {
	if cfg.KEM != KEMX25519 || !slices.Contains(cfg.Algorithms, suite) {
		return nil, nil, ErrUnsupported
	}
	pk, err := hpke.DHKEM(ecdh.X25519()).NewPublicKey(cfg.PublicKey)
	if err != nil {
		return nil, nil, ErrConfig
	}
	hdr := header(cfg.ID)
	enc, sender, err := hpke.NewSender(pk, hpke.HKDFSHA256(), hpke.AES128GCM(), info(hdr))
	if err != nil {
		return nil, nil, err
	}
	ct, err := sender.Seal(nil, msg)
	if err != nil {
		return nil, nil, err
	}
	out := slices.Concat(hdr, enc, ct)
	return out, &ClientContext{enc: enc, sender: sender}, nil
}

// Open reads the gateway's answer (RFC 9458, section 4.4).
func (c *ClientContext) Open(encResp []byte) ([]byte, error) {
	secret, err := c.sender.Export(responseLabel, nonceLen)
	if err != nil {
		return nil, ErrResponse
	}
	return openResponse(secret, c.enc, encResp)
}

// Key is one private key a gateway decapsulates with, under its key id.
type Key struct {
	ID      uint8
	Private *ecdh.PrivateKey
}

// Gateway decapsulates requests sealed to its keys.
type Gateway struct {
	keys    map[uint8]hpke.PrivateKey
	configs []KeyConfig
}

// NewGateway holds X25519 keys under distinct ids. Each is published with
// this package's one suite.
func NewGateway(keys ...Key) (*Gateway, error) {
	g := &Gateway{keys: map[uint8]hpke.PrivateKey{}}
	for _, k := range keys {
		if k.Private == nil || k.Private.Curve() != ecdh.X25519() {
			return nil, ErrConfig
		}
		if _, dup := g.keys[k.ID]; dup {
			return nil, ErrConfig
		}
		priv, err := hpke.NewDHKEMPrivateKey(k.Private)
		if err != nil {
			return nil, ErrConfig
		}
		g.keys[k.ID] = priv
		g.configs = append(g.configs, KeyConfig{ID: k.ID, KEM: KEMX25519, PublicKey: k.Private.PublicKey().Bytes(), Algorithms: []Algorithms{suite}})
	}
	if len(g.keys) == 0 {
		return nil, ErrConfig
	}
	return g, nil
}

// Configs are the gateway's key configurations, in the order given.
func (g *Gateway) Configs() []KeyConfig { return slices.Clone(g.configs) }

// ServerContext seals the one response to one request.
type ServerContext struct {
	enc, secret []byte
}

// Decapsulate opens a request (RFC 9458, section 4.3). Every failure is the
// same error, so an answer says nothing about which check refused it.
func (g *Gateway) Decapsulate(req []byte) ([]byte, *ServerContext, error) {
	if len(req) < headerLen+keyLen+tagLen {
		return nil, nil, ErrRequest
	}
	key, ok := g.keys[req[0]]
	if !ok || !bytes.Equal(req[:headerLen], header(req[0])) {
		return nil, nil, ErrRequest
	}
	hdr, enc, ct := req[:headerLen], req[headerLen:headerLen+keyLen], req[headerLen+keyLen:]
	r, err := hpke.NewRecipient(enc, key, hpke.HKDFSHA256(), hpke.AES128GCM(), info(hdr))
	if err != nil {
		return nil, nil, ErrRequest
	}
	msg, err := r.Open(nil, ct)
	if err != nil {
		return nil, nil, ErrRequest
	}
	secret, err := r.Export(responseLabel, nonceLen)
	if err != nil {
		return nil, nil, ErrRequest
	}
	return msg, &ServerContext{enc: bytes.Clone(enc), secret: secret}, nil
}

// Seal encapsulates the response under a fresh nonce (RFC 9458, section 4.4).
func (s *ServerContext) Seal(resp []byte) ([]byte, error) {
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return sealResponse(s.secret, s.enc, nonce, resp)
}

func responseAEAD(secret, enc, nonce []byte) (cipher.AEAD, []byte, error) {
	prk, err := hkdf.Extract(sha256.New, secret, slices.Concat(enc, nonce))
	if err != nil {
		return nil, nil, err
	}
	key, err := hkdf.Expand(sha256.New, prk, "key", aeadKeyLen)
	if err != nil {
		return nil, nil, err
	}
	iv, err := hkdf.Expand(sha256.New, prk, "nonce", aeadNonceLen)
	if err != nil {
		return nil, nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}
	aead, err := cipher.NewGCM(block)
	return aead, iv, err
}

func sealResponse(secret, enc, nonce, resp []byte) ([]byte, error) {
	aead, iv, err := responseAEAD(secret, enc, nonce)
	if err != nil {
		return nil, err
	}
	return aead.Seal(bytes.Clone(nonce), iv, resp, nil), nil
}

func openResponse(secret, enc, encResp []byte) ([]byte, error) {
	if len(encResp) < nonceLen+tagLen {
		return nil, ErrResponse
	}
	aead, iv, err := responseAEAD(secret, enc, encResp[:nonceLen])
	if err != nil {
		return nil, ErrResponse
	}
	msg, err := aead.Open(nil, iv, encResp[nonceLen:], nil)
	if err != nil {
		return nil, ErrResponse
	}
	return msg, nil
}
