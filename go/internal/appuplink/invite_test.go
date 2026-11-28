package appuplink

import (
	"bytes"
	"encoding/base64"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appenroll/approvaltest"
	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

func TestInvitationWaitsForNoiseTransportProofOnTheRealTunnel(t *testing.T) {
	r := newTunRig(t)
	now := time.Now()
	home := "abcdefghijklmnopqrstuvwx23"
	r.enroll.SetClock(func() (time.Time, bool) { return now, true })
	r.enroll.SetHome(func() string { return home })
	signer := approvaltest.New()
	r.enroll.SetAgentOfferKeys(map[string][]byte{"test": signer.Pub()})
	secret := bytes.Repeat([]byte{0x92}, 32)
	_, err := r.enroll.CreateAgentInvite(appenroll.AtTheBoxPage, protocol.AgentInviteCreate{Home: home, ID: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x63}, 16)), Hash: protocol.AgentInviteHash(home, secret), Scopes: []string{protocol.ScopeLiveRead}, ClaimUntilMs: now.Add(time.Hour).UnixMilli(), ExpiresAtMs: now.Add(24 * time.Hour).UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	app, err := newAppClient()
	if err != nil {
		t.Fatal(err)
	}
	offer := protocol.GrantOffer{V: 1, Key: app.staticPublic(), Role: protocol.RoleAgent, Client: "Test assistant", Scopes: []string{protocol.ScopeLiveRead}, TTLS: 86400, ExpMs: now.Add(time.Minute).UnixMilli()}
	raw, _ := protocol.Marshal(offer)
	signed, _ := protocol.OfferBytes(raw)
	payload, err := (protocol.AgentInviteClaim{Secret: secret, Offer: protocol.SignedOffer{O: raw, Kid: "test", Sig: signer.Sign(signed)}}).Payload()
	if err != nil {
		t.Fatal(err)
	}
	dev := cloudwire.DeviceID(app.staticPublic())
	r.open(t, app, dev, 91, payload)
	if len(r.enroll.Agents()) != 0 || r.enroll.AgentInvites()[0].State != "pending" {
		t.Fatal("message 1 consumed invitation")
	}
	// An authenticated transport message proves the recipient's private key.
	// No hardware command or valid application request is needed for proof.
	proof, err := app.encrypt([]byte{0})
	if err != nil {
		t.Fatal(err)
	}
	r.tunnel.Deliver(cloudwire.Tun{Dev: dev, Sid: 91, D: proof})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rows := r.enroll.Agents(); len(rows) == 1 {
			if rows[0].Role != protocol.RoleAgent || rows[0].ID != dev {
				t.Fatalf("wrong agent: %+v", rows[0])
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("proved invitation did not commit")
}
