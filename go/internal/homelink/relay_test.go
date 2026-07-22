package homelink

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/gatewayidentity"
)

func TestMachineChallengeSignatureAndReplay(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	manager := NewChallengeManager(ChallengeManagerOptions{
		Enabled: true,
		Random:  bytes.NewReader(bytes.Repeat([]byte{9}, machineChallengeBytes)),
		Now:     func() time.Time { return now },
	})
	identity := testGatewayIdentity(t)
	challenge, err := manager.Issue(identity.GatewayID(), identity.PublicKey(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	message, err := MachineChallengeMessage(challenge)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := identity.Sign(message)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.VerifyAndConsume(challenge, signature); err != nil {
		t.Fatal(err)
	}
	if err := manager.VerifyAndConsume(challenge, signature); err == nil {
		t.Fatal("machine challenge replay was accepted")
	}
}

func TestMachineChallengeExpiry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	manager := NewChallengeManager(ChallengeManagerOptions{
		Enabled: true,
		Random:  bytes.NewReader(bytes.Repeat([]byte{10}, machineChallengeBytes)),
		Now:     func() time.Time { return now },
	})
	identity := testGatewayIdentity(t)
	challenge, err := manager.Issue(identity.GatewayID(), identity.PublicKey(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	message, _ := MachineChallengeMessage(challenge)
	signature, _ := identity.Sign(message)
	now = now.Add(time.Second)
	if err := manager.VerifyAndConsume(challenge, signature); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired machine challenge = %v", err)
	}
}

func TestMachineChallengeIsDisabledByDefault(t *testing.T) {
	manager := NewChallengeManager(ChallengeManagerOptions{})
	if _, err := manager.Issue(testGatewayID, testGatewayIdentity(t).PublicKey(), time.Second); err != ErrRemoteDisabled {
		t.Fatalf("disabled machine challenge = %v", err)
	}
}

func TestMachineChallengeRejectsAnotherValidKey(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	manager := NewChallengeManager(ChallengeManagerOptions{
		Enabled: true,
		Random:  bytes.NewReader(bytes.Repeat([]byte{13}, machineChallengeBytes)),
		Now:     func() time.Time { return now },
	})
	expected := testGatewayIdentity(t)
	attacker := testGatewayIdentityForMAC(t, []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff})
	challenge, err := manager.Issue(expected.GatewayID(), expected.PublicKey(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	message, _ := MachineChallengeMessage(challenge)
	attackerSignature, _ := attacker.Sign(message)
	if err := manager.VerifyAndConsume(challenge, attackerSignature); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("valid signature from wrong key = %v", err)
	}
	expectedSignature, _ := expected.Sign(message)
	if err := manager.VerifyAndConsume(challenge, expectedSignature); err != nil {
		t.Fatalf("wrong-key attempt consumed challenge: %v", err)
	}
}

func TestRelayRouteContainsOnlyPublicMachineState(t *testing.T) {
	identity := testGatewayIdentity(t)
	alias, err := gatewayidentity.ThreeWordName(identity.GatewayID())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	record := RouteRecord{
		Alias: alias, GatewayID: identity.GatewayID(), PublicKey: identity.PublicKey(),
		ActiveRoute: "route-1", Status: RouteConnected, FirstSeenAt: now, LastSeenAt: now.Add(time.Second),
	}
	if err := record.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"alias": true, "gateway_id": true, "public_key": true, "active_route": true,
		"status": true, "first_seen_at": true, "last_seen_at": true,
	}
	if len(fields) != len(want) {
		t.Fatalf("relay record has extra state: %s", raw)
	}
	for field := range fields {
		if !want[field] {
			t.Fatalf("relay record contains %q: %s", field, raw)
		}
	}
}

func testGatewayIdentity(t *testing.T) gatewayidentity.Identity {
	return testGatewayIdentityForMAC(t, net.HardwareAddr{0xdc, 0xa6, 0x32, 0xf8, 0x38, 0xf7})
}

func testGatewayIdentityForMAC(t *testing.T, mac net.HardwareAddr) gatewayidentity.Identity {
	t.Helper()
	provider := gatewayidentity.SoftwareProvider{
		KeyPath: t.TempDir() + "/nova.key",
		GatewayID: func(context.Context) (string, error) {
			return gatewayidentity.GatewayIDFromMAC(append(net.HardwareAddr(nil), mac...))
		},
	}
	identity, err := provider.Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return identity
}
