package appenroll

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// The enrollment payload, printed as a QR code.
//
//	https://app.ftw.energy/p#v2.<box_noise_pub>.<pairing_code>.<lan_hint>.<rendezvous_secret>
//
// Everything after the '#' is a URL fragment, and a fragment is never sent in
// an HTTP request. That is the whole design: the app's trust anchor reaches it
// optically and reaches no server on the way, so a hostile or compelled cloud
// can deny service but cannot present itself as this box. Nothing here may
// ever move into the path or the query.
//
// The reference implementation is srcfl/ftw-webapp
// src/lib/identity/enrollment.ts, and payload_test.go checks this against the
// lengths and alphabet that parser enforces.
const (
	// PayloadVersion is the first segment. v1 had four segments and no
	// rendezvous secret, so an app that knows only v1 must refuse a v2
	// payload rather than parse three quarters of it.
	PayloadVersion = "v2"

	// PayloadOrigin is where the app is served. It is also the passkey RP
	// ID, which is why it is one constant and not two.
	PayloadOrigin = "https://app.ftw.energy"
	PayloadPath   = "/p"

	// MaxLANHintChars bounds the hint, because the app turns it into a
	// connection target. A bound, not a budget.
	MaxLANHintChars = 64
)

// EnrollmentURL builds the payload for the QR code.
//
// lanHint is where the box believes it can be reached on the local network,
// and is allowed to be empty: a box that does not know its own address should
// say nothing rather than guess, and the app falls back to the relay.
func (i *Identity) EnrollmentURL(code []byte, lanHint string) (string, error) {
	if len(code) != PairingCodeBytes {
		return "", fmt.Errorf("appenroll: pairing code is %d bytes, need %d", len(code), PairingCodeBytes)
	}
	if len(lanHint) > MaxLANHintChars {
		return "", fmt.Errorf("appenroll: lan hint is %d characters, at most %d", len(lanHint), MaxLANHintChars)
	}
	// The app refuses anything outside printable ASCII, and a hint it refuses
	// costs a re-scan the user cannot diagnose. Caught here instead.
	for _, r := range lanHint {
		if r < 0x21 || r > 0x7e {
			return "", fmt.Errorf("appenroll: lan hint is not a plain address")
		}
	}

	enc := base64.RawURLEncoding
	fragment := strings.Join([]string{
		PayloadVersion,
		enc.EncodeToString(i.StaticKey().Public()),
		enc.EncodeToString(code),
		enc.EncodeToString([]byte(lanHint)),
		enc.EncodeToString(i.RendezvousSecret()),
	}, ".")

	return PayloadOrigin + PayloadPath + "#" + fragment, nil
}
