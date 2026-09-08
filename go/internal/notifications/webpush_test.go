package notifications

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testVAPIDKey mirrors nova.Identity's signing surface so the provider can
// be driven without a key file on disk.
type testVAPIDKey struct{ priv *ecdsa.PrivateKey }

func newTestVAPIDKey(t *testing.T) *testVAPIDKey {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return &testVAPIDKey{priv: priv}
}

func (k *testVAPIDKey) PublicKeyHex() string {
	out := make([]byte, 64)
	k.priv.X.FillBytes(out[:32])
	k.priv.Y.FillBytes(out[32:])
	return hex.EncodeToString(out)
}

func (k *testVAPIDKey) SignRawHex(msg string) (string, error) {
	hash := sha256.Sum256([]byte(msg))
	r, s, err := ecdsa.Sign(rand.Reader, k.priv, hash[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return hex.EncodeToString(sig), nil
}

// testSubscriber is one pretend browser: its ECDH keypair, its auth secret,
// and the ability to decrypt what the box sent — the other half of RFC 8291,
// which is the only honest way to test the first half.
type testSubscriber struct {
	priv *ecdh.PrivateKey
	auth []byte
}

func newTestSubscriber(t *testing.T) *testSubscriber {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("subscriber key: %v", err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatalf("auth secret: %v", err)
	}
	return &testSubscriber{priv: priv, auth: auth}
}

func (s *testSubscriber) subscription(id, endpoint string) PushSubscription {
	return PushSubscription{
		ID:       id,
		Endpoint: endpoint,
		P256dh:   base64.RawURLEncoding.EncodeToString(s.priv.PublicKey().Bytes()),
		Auth:     base64.RawURLEncoding.EncodeToString(s.auth),
	}
}

// decrypt implements the receiver's side of RFC 8291.
func (s *testSubscriber) decrypt(t *testing.T, body []byte) []byte {
	t.Helper()
	if len(body) < 21 {
		t.Fatalf("body of %d bytes has no header", len(body))
	}
	salt := body[:16]
	rs := binary.BigEndian.Uint32(body[16:20])
	if rs != 4096 {
		t.Fatalf("record size = %d, want 4096", rs)
	}
	idLen := int(body[20])
	if idLen != 65 {
		t.Fatalf("key id length = %d, want 65", idLen)
	}
	asPub, err := ecdh.P256().NewPublicKey(body[21 : 21+idLen])
	if err != nil {
		t.Fatalf("sender public key: %v", err)
	}
	record := body[21+idLen:]

	secret, err := s.priv.ECDH(asPub)
	if err != nil {
		t.Fatalf("ECDH: %v", err)
	}
	prkKey, err := hkdf.Extract(sha256.New, secret, s.auth)
	if err != nil {
		t.Fatal(err)
	}
	keyInfo := "WebPush: info\x00" + string(s.priv.PublicKey().Bytes()) + string(asPub.Bytes())
	ikm, err := hkdf.Expand(sha256.New, prkKey, keyInfo, 32)
	if err != nil {
		t.Fatal(err)
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		t.Fatal(err)
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := gcm.Open(nil, nonce, record, nil)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	// Strip the last-record delimiter and any padding after it.
	i := len(plain) - 1
	for i >= 0 && plain[i] == 0 {
		i--
	}
	if i < 0 || plain[i] != 0x02 {
		t.Fatalf("record has no 0x02 delimiter: % x", plain)
	}
	return plain[:i]
}

// memSubs is an in-memory SubscriptionStore.
type memSubs struct {
	mu   sync.Mutex
	rows []PushSubscription
}

func (m *memSubs) PushSubscriptions() ([]PushSubscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]PushSubscription(nil), m.rows...), nil
}

func (m *memSubs) DeletePushSubscription(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.rows[:0]
	for _, r := range m.rows {
		if r.ID != id {
			kept = append(kept, r)
		}
	}
	m.rows = kept
	return nil
}

func (m *memSubs) ids() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.rows))
	for _, r := range m.rows {
		out = append(out, r.ID)
	}
	return out
}

// The core promise: what the box encrypts, the subscribed browser — and only
// it — can read back.
func TestEncryptForSubscriptionRoundTrips(t *testing.T) {
	sub := newTestSubscriber(t)
	row := sub.subscription("s1", "https://push.example/x")

	plaintext := []byte(`{"title":"Car charged","body":"7.4 kWh delivered — ready to go."}`)
	body, err := EncryptForSubscription(row.P256dh, row.Auth, plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if got := sub.decrypt(t, body); string(got) != string(plaintext) {
		t.Fatalf("round trip = %q, want %q", got, plaintext)
	}

	// Two publishes of the same plaintext must not share bytes: the salt and
	// the ephemeral key are fresh per message, and a repeated ciphertext would
	// hand an observer a correlation for free.
	again, err := EncryptForSubscription(row.P256dh, row.Auth, plaintext)
	if err != nil {
		t.Fatalf("encrypt again: %v", err)
	}
	if string(again) == string(body) {
		t.Fatal("two encryptions produced identical bytes")
	}
}

func TestEncryptRefusesBadSubscriptionKeys(t *testing.T) {
	if _, err := EncryptForSubscription("not-base64!!", "AAAAAAAAAAAAAAAAAAAAAA", []byte("x")); err == nil {
		t.Fatal("accepted a p256dh that is not a key")
	}
	sub := newTestSubscriber(t)
	row := sub.subscription("s1", "https://push.example/x")
	if _, err := EncryptForSubscription(row.P256dh, base64.RawURLEncoding.EncodeToString([]byte("short")), []byte("x")); err == nil {
		t.Fatal("accepted an auth secret that is not 16 bytes")
	}
	if _, err := EncryptForSubscription(row.P256dh, row.Auth, make([]byte, 5000)); err == nil {
		t.Fatal("accepted a payload larger than one record")
	}
}

// Publish must deliver an aes128gcm body under a VAPID header the push
// service can verify against the key the route publishes.
func TestPublishCarriesVAPIDAndDecryptablePayload(t *testing.T) {
	sub := newTestSubscriber(t)
	key := newTestVAPIDKey(t)

	type got struct {
		headers http.Header
		body    []byte
	}
	received := make(chan got, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- got{headers: r.Header.Clone(), body: body}
		w.WriteHeader(201)
	}))
	defer srv.Close()

	store := &memSubs{rows: []PushSubscription{sub.subscription("s1", srv.URL+"/push/abc")}}
	wp, err := NewWebPush(key, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := wp.Publish(context.Background(), Message{Title: "Car charged", Body: "7.4 kWh delivered — ready to go.", Priority: 3, Kind: PushChargingSessionComplete, LoadpointID: "garage"}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	r := <-received
	if ce := r.headers.Get("Content-Encoding"); ce != "aes128gcm" {
		t.Fatalf("Content-Encoding = %q", ce)
	}
	if ttl := r.headers.Get("TTL"); ttl != "3600" {
		t.Fatalf("TTL = %q", ttl)
	}
	if u := r.headers.Get("Urgency"); u != "normal" {
		t.Fatalf("Urgency = %q", u)
	}

	// The payload is the app's two lines, readable only by the subscriber.
	var payload map[string]string
	if err := json.Unmarshal(sub.decrypt(t, r.body), &payload); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if payload["title"] != "Car charged" || !strings.Contains(payload["body"], "7.4 kWh") {
		t.Fatalf("payload = %v", payload)
	}

	if payload["kind"] != PushChargingSessionComplete || payload["loadpoint_id"] != "garage" {
		t.Fatalf("charger destination lost in encryption: %v", payload)
	}

	// The Authorization header verifies against the published key.
	auth := r.headers.Get("Authorization")
	if !strings.HasPrefix(auth, "vapid t=") {
		t.Fatalf("Authorization = %q", auth)
	}
	parts := strings.SplitN(strings.TrimPrefix(auth, "vapid t="), ", k=", 2)
	if len(parts) != 2 {
		t.Fatalf("Authorization = %q", auth)
	}
	jwt, k := parts[0], parts[1]
	pub, err := wp.PublicKeyB64()
	if err != nil {
		t.Fatal(err)
	}
	if k != pub {
		t.Fatalf("k = %q, want the published key %q", k, pub)
	}
	segments := strings.Split(jwt, ".")
	if len(segments) != 3 {
		t.Fatalf("JWT has %d segments", len(segments))
	}
	claimsRaw, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		t.Fatalf("claims: %v", err)
	}
	var claims struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(claimsRaw, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Aud != srv.URL {
		t.Fatalf("aud = %q, want %q", claims.Aud, srv.URL)
	}
	if claims.Sub == "" || claims.Exp == 0 {
		t.Fatalf("claims = %+v", claims)
	}
	sig, err := base64.RawURLEncoding.DecodeString(segments[2])
	if err != nil || len(sig) != 64 {
		t.Fatalf("signature: %v (%d bytes)", err, len(sig))
	}
	hash := sha256.Sum256([]byte(segments[0] + "." + segments[1]))
	rr := new(big.Int).SetBytes(sig[:32])
	ss := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(&key.priv.PublicKey, hash[:], rr, ss) {
		t.Fatal("the VAPID JWT does not verify against the published key")
	}
}

// The dead man's header is the same construction the live publish uses,
// with two facts pinned: aud is the row's own endpoint origin, and exp is
// the full 24 hours from the clock the caller passed — the relay may hold
// it for six hours before the box re-signs, and it must still verify when
// the switch fires.
func TestDeadmanAuthorizationPinsAudAndExp(t *testing.T) {
	key := newTestVAPIDKey(t)
	now := time.Unix(1_750_000_000, 0)

	auth, err := DeadmanAuthorization(key, "https://push.example/p/route/abc", now)
	if err != nil {
		t.Fatalf("DeadmanAuthorization: %v", err)
	}
	if !strings.HasPrefix(auth, "vapid t=") {
		t.Fatalf("auth = %q", auth)
	}
	parts := strings.SplitN(strings.TrimPrefix(auth, "vapid t="), ", k=", 2)
	if len(parts) != 2 {
		t.Fatalf("auth = %q", auth)
	}
	if pub, err := publicKeyB64(key); err != nil || parts[1] != pub {
		t.Fatalf("k = %q, want the published key %q (%v)", parts[1], pub, err)
	}
	segments := strings.Split(parts[0], ".")
	if len(segments) != 3 {
		t.Fatalf("JWT has %d segments", len(segments))
	}
	claimsRaw, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		t.Fatalf("claims: %v", err)
	}
	var claims struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(claimsRaw, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Aud != "https://push.example" {
		t.Fatalf("aud = %q, want the endpoint origin", claims.Aud)
	}
	if claims.Exp != now.Add(24*time.Hour).Unix() {
		t.Fatalf("exp = %d, want the full 24 hours from the caller's clock (%d)",
			claims.Exp, now.Add(24*time.Hour).Unix())
	}
	if claims.Sub != "https://ftw.energy" {
		t.Fatalf("sub = %q", claims.Sub)
	}
	sig, err := base64.RawURLEncoding.DecodeString(segments[2])
	if err != nil || len(sig) != 64 {
		t.Fatalf("signature: %v (%d bytes)", err, len(sig))
	}
	hash := sha256.Sum256([]byte(segments[0] + "." + segments[1]))
	rr := new(big.Int).SetBytes(sig[:32])
	ss := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(&key.priv.PublicKey, hash[:], rr, ss) {
		t.Fatal("the dead man's JWT does not verify against the key")
	}
}

// A push service answering 410 has said the phone is gone; the row goes too,
// and the delivery to everyone else still counts.
func TestPublishSweepsGoneSubscriptions(t *testing.T) {
	alive := newTestSubscriber(t)
	dead := newTestSubscriber(t)
	key := newTestVAPIDKey(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/gone/") {
			w.WriteHeader(http.StatusGone)
			return
		}
		w.WriteHeader(201)
	}))
	defer srv.Close()

	store := &memSubs{rows: []PushSubscription{
		dead.subscription("dead", srv.URL+"/gone/1"),
		alive.subscription("alive", srv.URL+"/push/1"),
	}}
	wp, err := NewWebPush(key, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := wp.Publish(context.Background(), Message{Title: "t", Body: "b"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if ids := store.ids(); len(ids) != 1 || ids[0] != "alive" {
		t.Fatalf("rows after sweep = %v, want [alive]", ids)
	}
}

// A stored https endpoint that 302s onto the LAN must not be followed:
// the encrypted body stays on the enrolled origin, or the publish fails.
func TestPublishDoesNotFollowRedirects(t *testing.T) {
	sub := newTestSubscriber(t)
	key := newTestVAPIDKey(t)

	var loopHits atomic.Int32
	loop := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		loopHits.Add(1)
	}))
	defer loop.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, loop.URL, http.StatusFound)
	}))
	defer srv.Close()

	store := &memSubs{rows: []PushSubscription{sub.subscription("s1", srv.URL+"/push")}}
	wp, err := NewWebPush(key, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := wp.Publish(context.Background(), Message{Title: "t", Body: "b"}); err == nil {
		t.Fatal("a redirect must be refused")
	}
	if got := loopHits.Load(); got != 0 {
		t.Fatalf("the payload was resent to loopback %d time(s)", got)
	}
}

// No subscriptions is absence, not failure.
func TestPublishWithNoSubscriptionsIsNothingToSend(t *testing.T) {
	wp, err := NewWebPush(newTestVAPIDKey(t), &memSubs{})
	if err != nil {
		t.Fatal(err)
	}
	if err := wp.Publish(context.Background(), Message{Title: "t", Body: "b"}); !errors.Is(err, ErrNothingToSend) {
		t.Fatalf("err = %v, want ErrNothingToSend", err)
	}
}
