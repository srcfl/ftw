package appuplink

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The dead man's switch, box side.
//
// The one sentence the box cannot send about itself is "your box is out of
// reach". So it writes that sentence down while it still can — rendered from
// the push catalogue and encrypted per subscription, RFC 8291, before it
// ever leaves home — and hands the relay one row per subscription: an id, a
// push endpoint, the ciphertext, a deadline. While the box's uplink socket
// holds a claim on an id the switch is held; when the socket goes and stays
// gone past the deadline, the relay posts the ciphertext to the endpoint,
// once. The relay can never read what it delivers, and what it delivers was
// composed by the box in better days.
//
// The wire contract, implemented identically by the relay in
// srcfl/ftw-webapp:
//
//	POST <origin>/deadman
//	     {"id": <32 lowercase hex>, "endpoint": <https url>,
//	      "ct": <base64 aes128gcm ciphertext>,
//	      "auth": <the complete VAPID Authorization header value for the
//	      endpoint's origin, signed by the box>,
//	      "deadline_s": <60..86400>}
//	     → 204, upsert keyed on id, persisted by the relay
//	DELETE <origin>/deadman/<id> → 204, idempotent; the id is the bearer
//	     capability and there is no other auth
//	"deadman <id>" as a text frame on the box uplink claims the id for
//	     that socket; disconnect starts the countdown, a fresh claim
//	     cancels it, expiry fires once and re-arms only on the next claim.

// DeadmanDeadlineS is how long the box may be gone before the relay speaks
// for it. Ten minutes rides out a relay restart, an epoch rotation and an
// ISP hiccup; a power cut is still news while it is news. The contract
// allows 60..86400.
const DeadmanDeadlineS = 600

// deadmanRefreshInterval is how often a connected uplink re-posts its rows.
// Each row's auth is a VAPID signature with 24 hours to live (RFC 8292's
// ceiling), and the switch fires within DeadmanDeadlineS of a disconnect —
// so re-signing every six hours means the header the relay holds is never
// more than a quarter spent when it is needed, and a signature that could
// expire in the relay's hands would need the box to be gone for over 18
// hours while somehow never disconnecting.
const deadmanRefreshInterval = 6 * time.Hour

// CtrlDeadman is the claim word the box says on its uplink socket.
const CtrlDeadman = "deadman"

// DeadmanRow is one subscription's pre-encrypted farewell. CT is the
// box.unreachable catalogue entry, rendered and encrypted for exactly this
// subscription's keys — which is why there is one row per subscription and
// never one for the house.
type DeadmanRow struct {
	SubscriptionID string
	Endpoint       string
	CT             []byte
	// Auth is the complete VAPID Authorization header value for Endpoint's
	// origin, signed by the box. The subscription is bound to the box's key
	// and the push service refuses a POST without it — and the relay must
	// never hold the key, so it holds a signature instead, re-signed on
	// every resync. See deadmanRefreshInterval for the arithmetic.
	Auth string
}

// DeadmanSource hands the uplink the current rows. main.go implements it
// over the stored subscriptions and the web-push encryptor; this package
// neither stores nor encrypts anything.
type DeadmanSource interface {
	DeadmanRows() ([]DeadmanRow, error)
}

// DeadmanID derives the row id for one subscription: HMAC of the enrolment's
// rendezvous secret over a versioned purpose tag and the subscription id,
// truncated to 16 bytes — 32 lowercase hex chars, as the contract asks.
// Deterministic so a reboot claims the same ids it posted; keyed and tagged
// so the values alone link neither id to a rendezvous handle nor id to id.
// Source IP and request timing remain visible to the relay.
func DeadmanID(secret []byte, subscriptionID string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("ftw deadman v1\x00" + subscriptionID))
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

// setCommandOrigin records the door that accepted the latest socket, so
// dead-man rows are posted to that door and not to one the box failed to
// reach.
func (u *Uplink) setCommandOrigin(origin string) {
	u.deadmanMu.Lock()
	u.deadmanOrigin = deadmanOrigin(origin)
	u.deadmanMu.Unlock()
}

func (u *Uplink) commandHTTPOrigin() string {
	u.deadmanMu.Lock()
	defer u.deadmanMu.Unlock()
	return u.deadmanOrigin
}

// deadmanOrigin is the HTTPS side of the same host the command socket dials.
func deadmanOrigin(endpoint string) string {
	switch {
	case strings.HasPrefix(endpoint, "wss://"):
		return "https://" + strings.TrimPrefix(endpoint, "wss://")
	case strings.HasPrefix(endpoint, "ws://"):
		return "http://" + strings.TrimPrefix(endpoint, "ws://")
	}
	return endpoint
}

// ResyncDeadman brings the relay's rows and claims up to date with the
// stored subscriptions: posts every current row, deletes rows whose
// subscription is gone, and claims every current id on the live socket if
// one is up. Called on every connect and after every subscription change;
// safe to call while disconnected, when it updates rows and leaves the
// claiming to the next connect.
func (u *Uplink) ResyncDeadman() {
	if u == nil || u.opts.Deadman == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rows, err := u.opts.Deadman.DeadmanRows()
	if err != nil {
		u.log.Warn("dead man's switch: could not list rows", "err", err)
		return
	}
	secret := u.opts.Enroll.RendezvousSecret()
	desired := make(map[string]DeadmanRow, len(rows))
	for _, row := range rows {
		desired[DeadmanID(secret, row.SubscriptionID)] = row
	}

	u.deadmanMu.Lock()
	previous := make([]string, 0, len(u.deadmanPosted))
	for id := range u.deadmanPosted {
		previous = append(previous, id)
	}
	u.deadmanMu.Unlock()

	posted := make(map[string]bool, len(desired))
	for id, row := range desired {
		if err := u.postDeadman(ctx, id, row); err != nil {
			// The next connect re-posts everything, and the uplink
			// reconnects on its own — the switch self-heals on the same
			// cadence as the connection it guards.
			u.log.Warn("dead man's switch: post failed", "err", err)
			continue
		}
		posted[id] = true
	}
	for _, id := range previous {
		if _, still := desired[id]; still {
			continue
		}
		if err := u.deleteDeadman(ctx, id); err != nil {
			u.log.Warn("dead man's switch: delete failed", "err", err)
		}
	}
	u.deadmanMu.Lock()
	u.deadmanPosted = posted
	u.deadmanMu.Unlock()

	// Claim what was posted on the newest socket, if there is one. The
	// claim is per-connection state on the relay, so serve() runs this again
	// on every connect. In the overlap after the hour the older socket keeps
	// the claims it made on its own connect, and the relay counts claims, so
	// closing one socket does not release the other's.
	u.liveMu.Lock()
	var text func(string) error
	if n := len(u.live); n > 0 {
		text = u.live[n-1].text
	}
	u.liveMu.Unlock()
	if text == nil {
		return
	}
	for id := range posted {
		if err := text(CtrlDeadman + " " + id); err != nil {
			u.log.Warn("dead man's switch: claim failed", "err", err)
			return
		}
	}
}

func (u *Uplink) postDeadman(ctx context.Context, id string, row DeadmanRow) error {
	body, err := json.Marshal(map[string]any{
		"id":         id,
		"endpoint":   row.Endpoint,
		"ct":         base64.StdEncoding.EncodeToString(row.CT),
		"auth":       row.Auth,
		"deadline_s": DeadmanDeadlineS,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		u.commandHTTPOrigin()+"/deadman", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return u.doDeadman(req)
}

func (u *Uplink) deleteDeadman(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		u.commandHTTPOrigin()+"/deadman/"+id, nil)
	if err != nil {
		return err
	}
	return u.doDeadman(req)
}

func (u *Uplink) doDeadman(req *http.Request) error {
	resp, err := u.deadmanHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("the relay answered %d", resp.StatusCode)
	}
	return nil
}
