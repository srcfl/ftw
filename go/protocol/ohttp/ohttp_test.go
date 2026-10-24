package ohttp

import (
	"bytes"
	"crypto/ecdh"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// RFC 9458, Appendix A.
const (
	rfcSecretKey  = "3c168975674b2fa8e465970b79c8dcf09f1c741626480bd4c6162fc5b6a98e1a"
	rfcKeyConfig  = "01002031e1f05a740102115220e9af918f738674aec95f54db6e04eb705aae8e79815500080001000100010003"
	rfcRequest    = "00034745540568747470730b6578616d706c652e636f6d012f"
	rfcInfo       = "6d6573736167652f626874747020726571756573740001002000010001"
	rfcEncRequest = "010020000100014b28f881333e7c164ffc499ad9796f877f4e1051ee6d31bad19dec96c208b4726374e469135906992e1268c594d2a10c695d858c40a026e7965e7d86b83dd440b2c0185204b4d63525"
	rfcResponse   = "0140c8"
	rfcSecret     = "62d87a6ba569ee81014c2641f52bea36"
	rfcNonce      = "c789e7151fcba46158ca84b04464910d"
	rfcEncResp    = "c789e7151fcba46158ca84b04464910d86f9013e404feea014e7be4a441f234f857fbd"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func rfcGateway(t *testing.T) *Gateway {
	t.Helper()
	priv, err := ecdh.X25519().NewPrivateKey(unhex(t, rfcSecretKey))
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewGateway(Key{ID: 1, Private: priv})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestRFC9458KeyConfiguration(t *testing.T) {
	cfg, err := ParseKeyConfig(unhex(t, rfcKeyConfig))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ID != 1 || cfg.KEM != KEMX25519 || len(cfg.Algorithms) != 2 ||
		cfg.Algorithms[0] != (Algorithms{KDFHKDFSHA256, AEADAES128GCM}) || cfg.Algorithms[1] != (Algorithms{KDFHKDFSHA256, 0x0003}) {
		t.Fatalf("parsed %+v", cfg)
	}
	if got := hex.EncodeToString(cfg.Marshal()); got != rfcKeyConfig {
		t.Fatalf("marshal %s", got)
	}
	if got := rfcGateway(t).Configs()[0]; !bytes.Equal(got.PublicKey, cfg.PublicKey) || got.ID != 1 {
		t.Fatalf("the secret key does not give the published key: %+v", got)
	}
	if got := hex.EncodeToString(info(header(1))); got != rfcInfo {
		t.Fatalf("info %s", got)
	}
}

// The gateway opens the RFC's request, exports its secret, and seals the
// RFC's response byte for byte under the RFC's nonce.
func TestRFC9458GatewayVectors(t *testing.T) {
	msg, ctx, err := rfcGateway(t).Decapsulate(unhex(t, rfcEncRequest))
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(msg) != rfcRequest {
		t.Fatalf("request %x", msg)
	}
	if hex.EncodeToString(ctx.secret) != rfcSecret {
		t.Fatalf("secret %x", ctx.secret)
	}
	resp, err := sealResponse(ctx.secret, ctx.enc, unhex(t, rfcNonce), unhex(t, rfcResponse))
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(resp) != rfcEncResp {
		t.Fatalf("response %x", resp)
	}
	r, err := ParseRequest(msg)
	if err != nil || r.Method != "GET" || r.Scheme != "https" || r.Authority != "example.com" || r.Path != "/" || len(r.Fields) != 0 || len(r.Content) != 0 {
		t.Fatalf("binary request %+v %v", r, err)
	}
}

// The client half: its request header is the RFC's, and its response key
// schedule opens the RFC's encapsulated response.
func TestRFC9458ClientVectors(t *testing.T) {
	enc := unhex(t, rfcEncRequest)
	if got := hex.EncodeToString(header(1)); got != hex.EncodeToString(enc[:headerLen]) {
		t.Fatalf("header %s", got)
	}
	resp, err := openResponse(unhex(t, rfcSecret), enc[headerLen:headerLen+keyLen], unhex(t, rfcEncResp))
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(resp) != rfcResponse {
		t.Fatalf("response %x", resp)
	}
	r, err := ParseResponse(resp)
	if err != nil || r.Status != 200 {
		t.Fatalf("binary response %+v %v", r, err)
	}

	cfg, err := ParseKeyConfig(unhex(t, rfcKeyConfig))
	if err != nil {
		t.Fatal(err)
	}
	req, cctx, err := Encapsulate(cfg, unhex(t, rfcRequest))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(req[:headerLen], enc[:headerLen]) || len(req) != len(enc) {
		t.Fatalf("request %x", req)
	}
	msg, sctx, err := rfcGateway(t).Decapsulate(req)
	if err != nil || hex.EncodeToString(msg) != rfcRequest {
		t.Fatalf("gateway read %x %v", msg, err)
	}
	sealed, err := sctx.Seal(unhex(t, rfcResponse))
	if err != nil {
		t.Fatal(err)
	}
	got, err := cctx.Open(sealed)
	if err != nil || hex.EncodeToString(got) != rfcResponse {
		t.Fatalf("client read %x %v", got, err)
	}
}

func TestTheGatewayRefusesWhatItCannotOpen(t *testing.T) {
	g := rfcGateway(t)
	good := unhex(t, rfcEncRequest)
	flip := func(i int) []byte {
		b := bytes.Clone(good)
		b[i] ^= 1
		return b
	}
	for name, req := range map[string][]byte{
		"empty":       nil,
		"short":       good[:headerLen+keyLen],
		"key id":      flip(0),
		"kem":         flip(2),
		"kdf":         flip(4),
		"aead":        flip(6),
		"enc":         flip(headerLen),
		"ciphertext":  flip(len(good) - 1),
		"truncated":   good[:len(good)-1],
		"extra bytes": append(bytes.Clone(good), 0),
	} {
		if _, _, err := g.Decapsulate(req); !errors.Is(err, ErrRequest) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// A request sealed to one key cannot be read by a gateway holding another,
// even under the same key id. A box encrypts only to the configuration its
// release pins, so a key swapped in at the service reads nothing.
func TestASwappedKeyReadsNothing(t *testing.T) {
	cfg, err := ParseKeyConfig(unhex(t, rfcKeyConfig))
	if err != nil {
		t.Fatal(err)
	}
	req, _, err := Encapsulate(cfg, []byte("report"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := ecdh.X25519().GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewGateway(Key{ID: 1, Private: other})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := g.Decapsulate(req); !errors.Is(err, ErrRequest) {
		t.Fatalf("a different key opened the request: %v", err)
	}
}

func TestTheClientRefusesOtherSuitesAndForgedResponses(t *testing.T) {
	cfg, err := ParseKeyConfig(unhex(t, rfcKeyConfig))
	if err != nil {
		t.Fatal(err)
	}
	only := cfg
	only.Algorithms = []Algorithms{{KDFHKDFSHA256, 0x0003}}
	if _, _, err := Encapsulate(only, nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("ChaCha20 only: %v", err)
	}
	_, cctx, err := Encapsulate(cfg, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forged := range [][]byte{nil, make([]byte, nonceLen+tagLen), unhex(t, rfcEncResp)} {
		if _, err := cctx.Open(forged); !errors.Is(err, ErrResponse) {
			t.Fatalf("opened %x: %v", forged, err)
		}
	}
}

func TestKeyConfigurationsAreReadStrictly(t *testing.T) {
	good := unhex(t, rfcKeyConfig)
	for name, b := range map[string][]byte{
		"empty":         nil,
		"trailing byte": append(bytes.Clone(good), 0),
		"short key":     append(bytes.Clone(good[:3+31]), good[3+32:]...),
		"no algorithms": append(bytes.Clone(good[:3+32]), 0, 0),
		"odd length":    append(bytes.Clone(good[:3+32]), 0, 6, 0, 1, 0, 1, 0, 1),
	} {
		if _, err := ParseKeyConfig(b); err == nil {
			t.Fatalf("%s parsed", name)
		}
	}
	p256 := bytes.Clone(good)
	p256[2] = 0x10
	if _, err := ParseKeyConfig(p256); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("P-256: %v", err)
	}

	cfg, _ := ParseKeyConfig(good)
	list := MarshalKeys(KeyConfig{ID: 9, KEM: 0x0010, PublicKey: make([]byte, 65), Algorithms: []Algorithms{{1, 1}}}, cfg)
	got, err := ParseKeys(list)
	if err != nil || len(got) != 1 || !bytes.Equal(got[0].Marshal(), good) {
		t.Fatalf("keys %+v %v", got, err)
	}
	if _, err := ParseKeys(list[:len(list)-1]); err == nil {
		t.Fatal("a cut list parsed")
	}
	if _, err := NewGateway(); err == nil {
		t.Fatal("a gateway without keys")
	}
	k, _ := ecdh.X25519().GenerateKey(nil)
	if _, err := NewGateway(Key{ID: 1, Private: k}, Key{ID: 1, Private: k}); err == nil {
		t.Fatal("two keys under one id")
	}
	if !strings.HasPrefix(RequestType, "message/") || KeysType != "application/ohttp-keys" {
		t.Fatal("media types")
	}
}
