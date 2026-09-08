package notifications

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
)

// Web Push, without a library. The two RFCs it takes are small and stable —
// 8291 is one HKDF chain and one AES-128-GCM record, 8292 is one ES256 JWT —
// and this repository's habit is to drop dependencies, not collect them.
//
// The provider is engine-owned rather than config-selected: a phone enrolls
// by storing a subscription, and the first stored row is the whole opt-in.
// It runs beside whatever provider the config names, so a household using
// ntfy loses nothing when the app enables push. See Service.AddPublisher.

// VAPIDKey signs voluntary application server identification (RFC 8292).
// nova.Identity satisfies it; the key file is the box's own, generated on
// first run beside state.db in the same way nova.key is, and it signs VAPID
// JWTs and nothing else.
type VAPIDKey interface {
	// PublicKeyHex is the uncompressed P-256 point as X||Y, 128 hex chars.
	PublicKeyHex() string
	// SignRawHex signs SHA-256(msg) and returns R||S as 128 hex chars —
	// exactly the raw signature an ES256 JWT carries.
	SignRawHex(msg string) (string, error)
}

// PushSubscription is one browser push endpoint and its encryption keys, as
// the app stored them. The box holds these in state; this package never
// touches storage itself, in keeping with the rest of the service.
type PushSubscription struct {
	ID          string
	Endpoint    string
	P256dh      string // base64url uncompressed P-256 public key, 65 bytes
	Auth        string // base64url auth secret, 16 bytes
	DeviceID    string
	CreatedAtMs int64
}

// SubscriptionStore is the narrow slice of storage this provider needs.
// main.go adapts state.Store onto it.
type SubscriptionStore interface {
	PushSubscriptions() ([]PushSubscription, error)
	DeletePushSubscription(id string) error
}

// ErrNothingToSend says the provider had no subscription to deliver to. The
// dispatcher treats it as absence, not failure: a household that never
// enrolled a phone has not had a push fail.
var ErrNothingToSend = errors.New("webpush: no subscriptions")

const (
	// webPushTTL is how long a push service may hold an undelivered message.
	// An hour: these are "something changed at home" messages, and a phone
	// that was off for longer will read the app instead.
	webPushTTL = 3600
	// vapidTokenLife is well under RFC 8292's 24-hour ceiling.
	vapidTokenLife = 12 * time.Hour
	// deadmanTokenLife is that ceiling, taken whole, for the dead man's
	// rows only: the relay may not fire until long after the box last
	// spoke, so a stored header lives as long as the RFC allows — and the
	// uplink re-signs every six hours, so it is never more than a quarter
	// spent when the relay needs it.
	deadmanTokenLife = 24 * time.Hour
	// vapidSubject identifies the operator of this application server to
	// the push service, as RFC 8292 asks.
	vapidSubject = "https://ftw.energy"
)

// WebPush publishes rendered notifications to every stored subscription.
type WebPush struct {
	key   VAPIDKey
	store SubscriptionStore

	mu   sync.Mutex
	http *http.Client
	now  func() time.Time
}

// NewWebPush builds the provider. Both arguments are required; a nil store
// or key is a provider nobody can use, refused at construction rather than
// discovered per publish.
func NewWebPush(key VAPIDKey, store SubscriptionStore) (*WebPush, error) {
	if key == nil {
		return nil, errors.New("webpush: a VAPID key is required")
	}
	if store == nil {
		return nil, errors.New("webpush: a subscription store is required")
	}
	return &WebPush{
		key:   key,
		store: store,
		http: &http.Client{
			Timeout: 10 * time.Second,
			// Push services do not need redirects. A 307 or 308 would
			// resend this body, so refuse every hop: a stored https
			// endpoint must not bounce the encrypted payload onto the
			// LAN or loopback.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		now: time.Now,
	}, nil
}

// Name identifies this provider in status responses and logs.
func (w *WebPush) Name() string { return "webpush" }

// SetConfig is a no-op: web push has nothing to configure. Subscriptions are
// state, the key is a file, and both outlive any config reload.
func (w *WebPush) SetConfig(cfg *config.Notifications) {}

// PublicKeyB64 is the applicationServerKey the app hands to
// PushManager.subscribe: the uncompressed point, base64url, no padding.
func (w *WebPush) PublicKeyB64() (string, error) {
	return publicKeyB64(w.key)
}

func publicKeyB64(key VAPIDKey) (string, error) {
	raw, err := hex.DecodeString(key.PublicKeyHex())
	if err != nil || len(raw) != 64 {
		return "", errors.New("webpush: the VAPID public key is malformed")
	}
	return base64.RawURLEncoding.EncodeToString(append([]byte{0x04}, raw...)), nil
}

// Publish encrypts the message for every stored subscription and posts it to
// each push service. A 404 or 410 deletes that subscription — the service
// just said the phone is gone, and rows that outlive their phones are what
// makes every later publish slower and noisier.
func (w *WebPush) Publish(ctx context.Context, m Message) error {
	subs, err := w.store.PushSubscriptions()
	if err != nil {
		return fmt.Errorf("webpush: list subscriptions: %w", err)
	}
	if len(subs) == 0 {
		return ErrNothingToSend
	}

	payload, err := json.Marshal(map[string]string{"title": m.Title, "body": m.Body, "kind": m.Kind, "loadpoint_id": m.LoadpointID})
	if err != nil {
		return fmt.Errorf("webpush: encode payload: %w", err)
	}
	urgency := "normal"
	if m.Priority >= 4 {
		urgency = "high"
	}

	var sent int
	var errs []error
	for _, sub := range subs {
		switch err := w.publishOne(ctx, sub, payload, urgency); {
		case err == nil:
			sent++
		case errors.Is(err, errSubscriptionGone):
			if delErr := w.store.DeletePushSubscription(sub.ID); delErr != nil {
				slog.Warn("webpush: could not delete a dead subscription", "id", sub.ID, "err", delErr)
			} else {
				slog.Info("webpush: subscription gone; deleted", "id", sub.ID)
			}
		default:
			errs = append(errs, fmt.Errorf("subscription %s: %w", sub.ID, err))
		}
	}
	if sent > 0 {
		if len(errs) > 0 {
			slog.Warn("webpush: delivered to some subscriptions, not all", "err", errors.Join(errs...))
		}
		return nil
	}
	if len(errs) > 0 {
		return fmt.Errorf("webpush: %w", errors.Join(errs...))
	}
	// Every row turned out to be dead and was swept. Nothing failed; there
	// was simply nobody left to tell.
	return ErrNothingToSend
}

// errSubscriptionGone marks a push service answering 404/410: the endpoint
// no longer names a phone.
var errSubscriptionGone = errors.New("subscription gone")

func (w *WebPush) publishOne(ctx context.Context, sub PushSubscription, payload []byte, urgency string) error {
	body, err := EncryptForSubscription(sub.P256dh, sub.Auth, payload)
	if err != nil {
		return err
	}
	auth, err := vapidAuthorization(w.key, sub.Endpoint, w.now(), vapidTokenLife)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", fmt.Sprintf("%d", webPushTTL))
	req.Header.Set("Urgency", urgency)
	req.Header.Set("Authorization", auth)

	w.mu.Lock()
	httpc := w.http
	w.mu.Unlock()
	resp, err := httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return errSubscriptionGone
	case resp.StatusCode >= 300:
		return fmt.Errorf("push service answered %d", resp.StatusCode)
	}
	return nil
}

// DeadmanAuthorization is the Authorization header a dead man's row stores
// with the relay, built by the box because the relay must never hold the
// signing key: aud is the row's own endpoint origin, exp the full 24 hours.
// Exported with an explicit clock for the same reason EncryptForSubscription
// is — the switch composes its rows in advance, and the uplink's tests
// prove the re-signing cadence by moving the clock.
func DeadmanAuthorization(key VAPIDKey, endpoint string, now time.Time) (string, error) {
	return vapidAuthorization(key, endpoint, now, deadmanTokenLife)
}

// DeadmanAuthorization on the provider signs with its own key and clock;
// main.go's row source calls this once per subscription on every resync.
func (w *WebPush) DeadmanAuthorization(endpoint string) (string, error) {
	return DeadmanAuthorization(w.key, endpoint, w.now())
}

// vapidAuthorization builds the complete RFC 8292 header for one push
// service origin: "vapid t=<ES256 JWT>, k=<public key b64url>". Every
// Authorization this package ever writes comes through here.
func vapidAuthorization(key VAPIDKey, endpoint string, now time.Time, life time.Duration) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("endpoint is not a URL")
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, err := json.Marshal(map[string]any{
		"aud": u.Scheme + "://" + u.Host,
		"exp": now.Add(life).Unix(),
		"sub": vapidSubject,
	})
	if err != nil {
		return "", err
	}
	signingInput := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	sigHex, err := key.SignRawHex(signingInput)
	if err != nil {
		return "", fmt.Errorf("sign VAPID token: %w", err)
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil || len(sig) != 64 {
		return "", errors.New("the VAPID signature is malformed")
	}
	jwt := signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)

	pub, err := publicKeyB64(key)
	if err != nil {
		return "", err
	}
	return "vapid t=" + jwt + ", k=" + pub, nil
}

// ValidateSubscriptionKeys says whether a subscription's keys have the
// shapes RFC 8291 needs, without encrypting anything. The API calls it at
// store time, because a row that can never be encrypted toward is a row
// every later publish stumbles over.
func ValidateSubscriptionKeys(p256dhB64, authB64 string) error {
	pub, err := decodeB64(p256dhB64)
	if err != nil || len(pub) != 65 || pub[0] != 0x04 {
		return errors.New("p256dh is not a 65-byte uncompressed P-256 point")
	}
	if _, err := ecdh.P256().NewPublicKey(pub); err != nil {
		return errors.New("p256dh is not a point on P-256")
	}
	auth, err := decodeB64(authB64)
	if err != nil || len(auth) != 16 {
		return errors.New("auth is not a 16-byte secret")
	}
	return nil
}

// EncryptForSubscription encrypts one plaintext for one subscription per
// RFC 8291: ECDH on P-256 against the browser's key, the auth-secret HKDF
// chain, and a single aes128gcm record with its header. Exported because the
// dead man's switch pre-encrypts the box.unreachable payload with exactly
// this construction before the relay ever holds it.
func EncryptForSubscription(p256dhB64, authB64 string, plaintext []byte) ([]byte, error) {
	uaPubBytes, err := decodeB64(p256dhB64)
	if err != nil || len(uaPubBytes) != 65 {
		return nil, errors.New("webpush: p256dh is not a 65-byte P-256 point")
	}
	authSecret, err := decodeB64(authB64)
	if err != nil || len(authSecret) != 16 {
		return nil, errors.New("webpush: auth is not a 16-byte secret")
	}
	uaPub, err := ecdh.P256().NewPublicKey(uaPubBytes)
	if err != nil {
		return nil, fmt.Errorf("webpush: p256dh: %w", err)
	}

	asPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("webpush: ephemeral key: %w", err)
	}
	var salt [16]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return nil, fmt.Errorf("webpush: salt: %w", err)
	}
	return encryptRecord(asPriv, uaPub, authSecret, salt, plaintext)
}

// encryptRecord is the deterministic core, split out so the round-trip test
// can drive it with a fixed ephemeral key and salt.
func encryptRecord(
	asPriv *ecdh.PrivateKey, uaPub *ecdh.PublicKey, authSecret []byte,
	salt [16]byte, plaintext []byte,
) ([]byte, error) {
	const recordSize = 4096
	// One record only: plaintext, its 0x02 last-record delimiter and the
	// 16-byte tag must fit. Notification sentences are two lines; anything
	// near this ceiling is a bug upstream, not a reason to grow records.
	if len(plaintext) > recordSize-17 {
		return nil, fmt.Errorf("webpush: payload of %d bytes does not fit one record", len(plaintext))
	}

	ecdhSecret, err := asPriv.ECDH(uaPub)
	if err != nil {
		return nil, fmt.Errorf("webpush: ECDH: %w", err)
	}
	asPub := asPriv.PublicKey().Bytes()

	// RFC 8291 §3.3-3.4: the two-stage HKDF chain.
	prkKey, err := hkdf.Extract(sha256.New, ecdhSecret, authSecret)
	if err != nil {
		return nil, err
	}
	keyInfo := "WebPush: info\x00" + string(uaPub.Bytes()) + string(asPub)
	ikm, err := hkdf.Expand(sha256.New, prkKey, keyInfo, 32)
	if err != nil {
		return nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt[:])
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	record := gcm.Seal(nil, nonce, append(append([]byte{}, plaintext...), 0x02), nil)

	// RFC 8188 header: salt, record size, key id length, key id.
	out := make([]byte, 0, 16+4+1+len(asPub)+len(record))
	out = append(out, salt[:]...)
	out = binary.BigEndian.AppendUint32(out, recordSize)
	out = append(out, byte(len(asPub)))
	out = append(out, asPub...)
	return append(out, record...), nil
}

// decodeB64 accepts the encodings browsers actually produce: base64url with
// or without padding, and standard base64 for good measure.
func decodeB64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{
		base64.RawURLEncoding, base64.URLEncoding,
		base64.RawStdEncoding, base64.StdEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("not base64")
}
