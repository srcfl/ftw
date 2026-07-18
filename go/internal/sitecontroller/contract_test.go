package sitecontroller

import (
	"bytes"
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/nova"
)

func TestPairingMessageWireContract(t *testing.T) {
	anchor := "zap-04772a97"
	payload := PairingPayload{
		ContractVersion: PairingContractVersion,
		ControllerKind:  ControllerKindFTW,
		PublicKey:       strings.Repeat("a", 128),
		Nonce:           "nonce",
		ExpiresAtMS:     1234,
		AnchorGatewayID: &anchor,
	}
	got, err := PairingMessage(payload)
	if err != nil {
		t.Fatal(err)
	}
	want := `srcful.site-controller.pairing.v1
{"contract_version":"ftw-site-controller-pairing/v1","controller_kind":"ftw","public_key":"` + strings.Repeat("a", 128) + `","nonce":"nonce","expires_at_ms":1234,"anchor_gateway_id":"zap-04772a97"}`
	if string(got) != want {
		t.Fatalf("canonical pairing message changed:\n got %s\nwant %s", got, want)
	}
}

func TestNewPairingAndSnapshotUseDedicatedSignedIdentity(t *testing.T) {
	identity, err := nova.LoadOrCreateIdentity(filepath.Join(t.TempDir(), "ftw-site-controller.key"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	random := bytes.NewReader(make([]byte, 64))
	anchor := "zap-04772a97"
	pairing, err := NewPairing(identity, &anchor, now, random)
	if err != nil {
		t.Fatal(err)
	}
	if pairing.Payload.PublicKey != identity.PublicKeyHex() || pairing.Payload.ExpiresAtMS != now.Add(PairingTTL).UnixMilli() {
		t.Fatalf("unexpected pairing payload: %+v", pairing.Payload)
	}
	nonce, err := base64.RawURLEncoding.DecodeString(pairing.Payload.Nonce)
	if err != nil || len(nonce) != 32 {
		t.Fatalf("nonce = %q, err=%v len=%d", pairing.Payload.Nonce, err, len(nonce))
	}
	if len(pairing.Signature) != 128 {
		t.Fatalf("pairing signature length = %d", len(pairing.Signature))
	}

	actions := make([]PlanAction, MaxActions+5)
	for i := range actions {
		actions[i] = PlanAction{StartMS: now.Add(time.Duration(i) * time.Minute).UnixMilli(), EndMS: now.Add(time.Duration(i+1) * time.Minute).UnixMilli()}
	}
	snapshot, err := NewSnapshot(identity, "sit-019b952c-1484-7994-83b9-f6198b192f3a",
		StatusSnapshot{SoftwareVersion: "1.4.0", Mode: "self_consumption"},
		HealthSnapshot{State: "ok", DriversOK: 2},
		PlanSnapshot{Enabled: true, ActionCount: len(actions), Actions: actions},
		now, bytes.NewReader(make([]byte, 18)))
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Payload.Plan.Actions) != MaxActions || snapshot.Payload.Plan.ActionCount != len(actions) {
		t.Fatalf("plan bounds not preserved: %+v", snapshot.Payload.Plan)
	}
	if len(snapshot.Signature) != 128 || !strings.HasPrefix(snapshot.Payload.SnapshotID, "snp-") {
		t.Fatalf("invalid signed snapshot: %+v", snapshot)
	}
}

func TestNewSnapshotRejectsUnscopedSite(t *testing.T) {
	identity, err := nova.LoadOrCreateIdentity(filepath.Join(t.TempDir(), "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewSnapshot(identity, "", StatusSnapshot{}, HealthSnapshot{}, PlanSnapshot{}, time.Now(), nil); err == nil {
		t.Fatal("empty site identity accepted")
	}
}
