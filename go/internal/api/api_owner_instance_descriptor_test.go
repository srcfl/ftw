package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http/httptest"
	"testing"
)

// fakeInstanceSigner is an in-test ES256 signer mirroring *nova.Identity's
// SignRawHex/PublicKeyHex wire contract (raw r||s as 128-char hex; pubkey as
// uncompressed X||Y, 128 lowercase hex chars). It lets the api package test the
// descriptor handler without importing internal/nova (no cycle, matches the
// relaySigner interface pattern in cmd/ftw/owner_relay_register.go).
type fakeInstanceSigner struct{ priv *ecdsa.PrivateKey }

func newFakeInstanceSigner(t *testing.T) *fakeInstanceSigner {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	return &fakeInstanceSigner{priv: priv}
}

func (f *fakeInstanceSigner) PublicKeyHex() string {
	x := f.priv.PublicKey.X.Bytes()
	y := f.priv.PublicKey.Y.Bytes()
	buf := make([]byte, 64)
	copy(buf[32-len(x):32], x)
	copy(buf[64-len(y):64], y)
	return hex.EncodeToString(buf)
}

func (f *fakeInstanceSigner) SignRawHex(msg string) (string, error) {
	h := sha256.Sum256([]byte(msg))
	r, s, err := ecdsa.Sign(rand.Reader, f.priv, h[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	rb, sb := r.Bytes(), s.Bytes()
	copy(sig[32-len(rb):32], rb)
	copy(sig[64-len(sb):64], sb)
	return hex.EncodeToString(sig), nil
}

// verifyInstanceSig checks a base64url (no padding) raw r||s ECDSA-P256 sig of
// msg against an uncompressed X||Y device public key (128 hex chars). Mirrors
// the browser-side verifyEntry the directory blob relies on.
func verifyInstanceSig(t *testing.T, pubKeyHex, msg, sigB64URL string) bool {
	t.Helper()
	pb, err := hex.DecodeString(pubKeyHex)
	if err != nil || len(pb) != 64 {
		return false
	}
	pub := &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(pb[:32]),
		Y:     new(big.Int).SetBytes(pb[32:]),
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64URL)
	if err != nil || len(sig) != 64 {
		return false
	}
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	h := sha256.Sum256([]byte(msg))
	return ecdsa.Verify(pub, h[:], r, s)
}

// The descriptor is owner-authed and signs the EXACT CONTRACT string; the
// signature must verify against pi_pubkey and the JSON fields must match.
func TestInstanceDescriptorSignsContractString(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true // gate inactive in minDeps (TunnelMarker empty), authorizeOwner returns lan-bypass
	signer := newFakeInstanceSigner(t)
	d.InstanceSigner = signer
	d.SiteIdentityPubHex = signer.PublicKeyHex()
	d.SiteID = "site:test-site"
	d.Cfg.Site.Name = "test-site"
	srv := New(d)

	req := httptest.NewRequest("GET", "/api/owner-access/instance-descriptor", nil)
	req.Host = "1.2.3.4"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}

	var out struct {
		SiteID   string `json:"site_id"`
		PiPubkey string `json:"pi_pubkey"`
		Label    string `json:"label"`
		Sig      string `json:"sig"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v body=%q", err, rec.Body.String())
	}
	if out.SiteID != "site:test-site" {
		t.Fatalf("site_id=%q want site:test-site", out.SiteID)
	}
	if out.PiPubkey != signer.PublicKeyHex() || len(out.PiPubkey) != 128 {
		t.Fatalf("pi_pubkey=%q (len %d) want %q", out.PiPubkey, len(out.PiPubkey), signer.PublicKeyHex())
	}
	if out.Label != "test-site" {
		t.Fatalf("label=%q want test-site", out.Label)
	}

	// The signed string MUST be byte-for-byte the CONTRACT format.
	want := "ftw-instance:v1:" + out.SiteID + ":" + out.PiPubkey + ":" + out.Label
	if !verifyInstanceSig(t, out.PiPubkey, want, out.Sig) {
		t.Fatalf("signature does not verify over %q (sig=%q)", want, out.Sig)
	}
	// And sig is base64url (no padding) of a 64-byte raw r||s, never hex.
	if _, err := base64.RawURLEncoding.DecodeString(out.Sig); err != nil {
		t.Fatalf("sig not base64url-no-pad: %q (%v)", out.Sig, err)
	}
}

// Without an owner session AND without LAN-bypass, a remote (tunnelled) caller
// is refused — the descriptor carries the Pi pubkey + a signature and must not
// be an anonymous read over the relay.
func TestInstanceDescriptorRequiresOwner(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = false
	d.TunnelMarker = "marker" // gate active
	signer := newFakeInstanceSigner(t)
	d.InstanceSigner = signer
	d.SiteIdentityPubHex = signer.PublicKeyHex()
	d.SiteID = "site:test-site"
	srv := New(d)

	req := httptest.NewRequest("GET", "/api/owner-access/instance-descriptor", nil)
	req.Host = "127.0.0.1"
	req.Header.Set("X-FTW-Tunnel", "marker") // tunnelled → no LAN bypass, no session → reject
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("status=%d want 401 body=%q", rec.Code, rec.Body.String())
	}
}

// When no identity/signer is wired (e.g. identity load failed on boot), the
// endpoint reports 503 rather than serving an unsigned descriptor.
func TestInstanceDescriptor503WhenNoSigner(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true
	d.InstanceSigner = nil
	d.SiteIdentityPubHex = ""
	srv := New(d)

	req := httptest.NewRequest("GET", "/api/owner-access/instance-descriptor", nil)
	req.Host = "1.2.3.4"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Fatalf("status=%d want 503 body=%q", rec.Code, rec.Body.String())
	}
}
