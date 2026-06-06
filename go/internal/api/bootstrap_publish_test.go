package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/state"
)

// bootstrapPublishSigningStringForTest reconstructs the relay's canonical signing
// string (cmd/ftw-relay/bootstrap_http.go bootstrapPublishSigningString) so the
// test verifies the OUTER publish sig over exactly the bytes the relay checks.
func bootstrapPublishSigningStringForTest(siteID, pinHash string, descriptor []byte) string {
	dh := sha256.Sum256(descriptor)
	return "ftw-bootstrap:v1:" + siteID + ":" + pinHash + ":" + hex.EncodeToString(dh[:])
}

// verifyES256HexForTest mirrors the relay's verifyES256Hex: a raw r||s HEX sig of
// SHA-256(msg) against an uncompressed X||Y pubkey (128 hex chars).
func verifyES256HexForTest(t *testing.T, pubKeyHex, msg, sigHex string) bool {
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
	sig, err := hex.DecodeString(sigHex)
	if err != nil || len(sig) != 64 {
		return false
	}
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	h := sha256.Sum256([]byte(msg))
	return ecdsa.Verify(pub, h[:], r, s)
}

// fakeBootstrapRelay captures the PUT /bootstrap/{site_id} body.
type fakeBootstrapRelay struct {
	mu       sync.Mutex
	siteID   string
	body     bootstrapPublishIO
	gotPut   bool
}

func newFakeBootstrapRelay(t *testing.T) (*fakeBootstrapRelay, *httptest.Server) {
	t.Helper()
	fr := &fakeBootstrapRelay{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || !strings.HasPrefix(r.URL.Path, "/bootstrap/") {
			http.NotFound(w, r)
			return
		}
		fr.mu.Lock()
		defer fr.mu.Unlock()
		fr.siteID = strings.TrimPrefix(r.URL.Path, "/bootstrap/")
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &fr.body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		fr.gotPut = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return fr, srv
}

// TestBootstrapPublishSignsBothWireForms is the heart of Task 5: the Pi parks a
// signed descriptor on /bootstrap and BOTH signatures must verify in the exact
// wire form their respective consumers expect — the OUTER publish sig in HEX
// (relay verifyES256Hex) and the INNER descriptor sig in base64url
// (browser verifyEntry).
func TestBootstrapPublishSignsBothWireForms(t *testing.T) {
	d := minDeps(t)
	signer := newFakeInstanceSigner(t)
	d.InstanceSigner = signer
	d.SiteIdentityPubHex = signer.PublicKeyHex()
	d.SiteID = "site:test-site"
	d.Cfg.Site.Name = "test-site"
	fr, relaySrv := newFakeBootstrapRelay(t)
	d.RelayBaseURL = relaySrv.URL
	srv := New(d)

	const pin = "042315"
	srv.publishBootstrapDescriptor(pin)

	fr.mu.Lock()
	defer fr.mu.Unlock()
	if !fr.gotPut {
		t.Fatalf("relay never received the PUT /bootstrap/{site_id}")
	}
	if fr.siteID != "site:test-site" {
		t.Fatalf("PUT site_id=%q want site:test-site", fr.siteID)
	}

	// pin_hash is hex(sha256(pin)).
	wantPinHash := sha256.Sum256([]byte(pin))
	if fr.body.PinHash != hex.EncodeToString(wantPinHash[:]) {
		t.Fatalf("pin_hash=%q want %q", fr.body.PinHash, hex.EncodeToString(wantPinHash[:]))
	}

	// descriptor is std-base64 of the marshaled descriptor JSON.
	descJSON, err := base64.StdEncoding.DecodeString(fr.body.Descriptor)
	if err != nil {
		t.Fatalf("descriptor not std-base64: %v", err)
	}

	// --- OUTER sig (HEX) verifies over the relay's signing string. ---
	outerMsg := bootstrapPublishSigningStringForTest("site:test-site", fr.body.PinHash, descJSON)
	if !verifyES256HexForTest(t, signer.PublicKeyHex(), outerMsg, fr.body.Sig) {
		t.Fatalf("OUTER publish sig (hex) does not verify over %q (sig=%q)", outerMsg, fr.body.Sig)
	}
	if _, err := hex.DecodeString(fr.body.Sig); err != nil {
		t.Fatalf("OUTER sig not hex: %q (%v)", fr.body.Sig, err)
	}

	// --- INNER descriptor sig (base64url) verifies over the instance string. ---
	var desc struct {
		SiteID   string `json:"site_id"`
		PiPubkey string `json:"pi_pubkey"`
		Label    string `json:"label"`
		Sig      string `json:"sig"`
	}
	if err := json.Unmarshal(descJSON, &desc); err != nil {
		t.Fatalf("descriptor JSON: %v", err)
	}
	if desc.SiteID != "site:test-site" || desc.Label != "test-site" || desc.PiPubkey != signer.PublicKeyHex() {
		t.Fatalf("descriptor fields wrong: %+v", desc)
	}
	innerMsg := instanceDescriptorSigningString(desc.SiteID, desc.PiPubkey, desc.Label)
	if !verifyInstanceSig(t, desc.PiPubkey, innerMsg, desc.Sig) {
		t.Fatalf("INNER descriptor sig (base64url) does not verify over %q (sig=%q)", innerMsg, desc.Sig)
	}
	if _, err := base64.RawURLEncoding.DecodeString(desc.Sig); err != nil {
		t.Fatalf("INNER sig not base64url-no-pad: %q (%v)", desc.Sig, err)
	}
}

// TestBootstrapPublishSkippedWhenDevicesEnrolled guards the zero-device window:
// once a passkey exists, the Pi must NOT re-park a descriptor (the bootstrap
// window is closed).
func TestBootstrapPublishSkippedWhenDevicesEnrolled(t *testing.T) {
	d := minDeps(t)
	signer := newFakeInstanceSigner(t)
	d.InstanceSigner = signer
	d.SiteIdentityPubHex = signer.PublicKeyHex()
	d.SiteID = "site:test-site"
	d.Cfg.Site.Name = "test-site"
	fr, relaySrv := newFakeBootstrapRelay(t)
	d.RelayBaseURL = relaySrv.URL

	// One trusted device enrolled → window closed.
	if err := d.State.SaveTrustedDevice(state.TrustedDevice{
		CredentialID: []byte("cred-1"),
		PublicKey:    []byte("pub-1"),
		FriendlyName: "phone",
		CreatedAtMs:  time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("save trusted device: %v", err)
	}
	srv := New(d)

	srv.publishBootstrapDescriptor("042315")

	fr.mu.Lock()
	defer fr.mu.Unlock()
	if fr.gotPut {
		t.Fatalf("publish must be SKIPPED once a device is enrolled, but relay got a PUT")
	}
}

// TestBootstrapPublishSkippedWhenNoRelay confirms the no-op when the Pi has no
// relay URL configured (LAN-only deploy): nothing to publish to, so it skips
// without panicking.
func TestBootstrapPublishSkippedWhenNoRelay(t *testing.T) {
	d := minDeps(t)
	signer := newFakeInstanceSigner(t)
	d.InstanceSigner = signer
	d.SiteIdentityPubHex = signer.PublicKeyHex()
	d.SiteID = "site:test-site"
	d.Cfg.Site.Name = "test-site"
	d.RelayBaseURL = "" // LAN-only
	srv := New(d)

	// Must not panic / block. No relay to assert against; the contract is just
	// "returns cleanly".
	srv.publishBootstrapDescriptor("042315")
}
