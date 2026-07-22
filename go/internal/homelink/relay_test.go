package homelink

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/gatewayidentity"
)

func TestMachineChallengeSignatureAndReplay(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	identity := testGatewayIdentity(t)
	manager := NewChallengeManager(ChallengeManagerOptions{
		Enabled:      true,
		Random:       bytes.NewReader(bytes.Repeat([]byte{9}, machineChallengeBytes)),
		Now:          func() time.Time { return now },
		MonotonicNow: monotonicFromWall(&now),
		Keys:         staticGatewayKeyLookup(identity),
	})
	challenge, err := manager.Issue(context.Background(), identity.GatewayID(), time.Second)
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
	identity := testGatewayIdentity(t)
	manager := NewChallengeManager(ChallengeManagerOptions{
		Enabled:      true,
		Random:       bytes.NewReader(bytes.Repeat([]byte{10}, machineChallengeBytes)),
		Now:          func() time.Time { return now },
		MonotonicNow: monotonicFromWall(&now),
		Keys:         staticGatewayKeyLookup(identity),
	})
	challenge, err := manager.Issue(context.Background(), identity.GatewayID(), time.Second)
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

func TestMachineChallengeExpiryUsesMonotonicDeadline(t *testing.T) {
	wallNow := time.Unix(1_800_000_000, 0)
	monotonicNow := time.Duration(0)
	identity := testGatewayIdentity(t)
	manager := NewChallengeManager(ChallengeManagerOptions{
		Enabled:      true,
		Random:       bytes.NewReader(bytes.Repeat([]byte{16}, machineChallengeBytes)),
		Now:          func() time.Time { return wallNow },
		MonotonicNow: func() time.Duration { return monotonicNow },
		Keys:         staticGatewayKeyLookup(identity),
	})
	challenge, err := manager.Issue(context.Background(), identity.GatewayID(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	message, _ := MachineChallengeMessage(challenge)
	signature, _ := identity.Sign(message)
	wallNow = wallNow.Add(-24 * time.Hour)
	monotonicNow = time.Second
	if err := manager.VerifyAndConsume(challenge, signature); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("wall-clock rollback extended machine challenge: %v", err)
	}
}

func TestMachineChallengeMonotonicRegressionAfterIssueFailsClosed(t *testing.T) {
	wallNow := time.Unix(1_800_000_000, 0)
	monotonicNow := 100 * time.Second
	identity := testGatewayIdentity(t)
	manager := NewChallengeManager(ChallengeManagerOptions{
		Enabled:      true,
		Random:       bytes.NewReader(bytes.Repeat([]byte{17}, machineChallengeBytes)),
		Now:          func() time.Time { return wallNow },
		MonotonicNow: func() time.Duration { return monotonicNow },
		Keys:         staticGatewayKeyLookup(identity),
	})
	challenge, err := manager.Issue(context.Background(), identity.GatewayID(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	message, _ := MachineChallengeMessage(challenge)
	signature, _ := identity.Sign(message)

	monotonicNow = 50 * time.Second
	if err := manager.VerifyAndConsume(challenge, signature); !errors.Is(err, ErrMonotonicClock) {
		t.Fatalf("100s to 50s after challenge issue = %v", err)
	}
	monotonicNow = 200 * time.Second
	if err := manager.VerifyAndConsume(challenge, signature); !errors.Is(err, ErrMonotonicClock) {
		t.Fatalf("clock recovery reopened challenge = %v", err)
	}
}

func TestMachineChallengeMonotonicRegressionBeforeIssueFailsClosed(t *testing.T) {
	wallNow := time.Unix(1_800_000_000, 0)
	monotonicNow := 100 * time.Second
	identity := testGatewayIdentity(t)
	random := append(
		bytes.Repeat([]byte{18}, machineChallengeBytes),
		bytes.Repeat([]byte{19}, machineChallengeBytes)...,
	)
	manager := NewChallengeManager(ChallengeManagerOptions{
		Enabled:      true,
		Random:       bytes.NewReader(random),
		Now:          func() time.Time { return wallNow },
		MonotonicNow: func() time.Duration { return monotonicNow },
		Keys:         staticGatewayKeyLookup(identity),
	})
	if _, err := manager.Issue(context.Background(), identity.GatewayID(), time.Second); err != nil {
		t.Fatal(err)
	}

	monotonicNow = 50 * time.Second
	if _, err := manager.Issue(context.Background(), identity.GatewayID(), time.Second); !errors.Is(err, ErrMonotonicClock) {
		t.Fatalf("100s to 50s before challenge issue = %v", err)
	}
	monotonicNow = 200 * time.Second
	if _, err := manager.Issue(context.Background(), identity.GatewayID(), time.Second); !errors.Is(err, ErrMonotonicClock) {
		t.Fatalf("clock recovery reopened challenge manager = %v", err)
	}
}

func TestMachineChallengeRejectsRegressionBelowHighWater(t *testing.T) {
	wallNow := time.Unix(1_800_000_000, 0)
	monotonicNow := 100 * time.Second
	identity := testGatewayIdentity(t)
	random := append(
		bytes.Repeat([]byte{20}, machineChallengeBytes),
		bytes.Repeat([]byte{21}, machineChallengeBytes)...,
	)
	manager := NewChallengeManager(ChallengeManagerOptions{
		Enabled:      true,
		Random:       bytes.NewReader(random),
		Now:          func() time.Time { return wallNow },
		MonotonicNow: func() time.Duration { return monotonicNow },
		Keys:         staticGatewayKeyLookup(identity),
	})
	challenge, err := manager.Issue(context.Background(), identity.GatewayID(), MachineChallengeMaxTTL)
	if err != nil {
		t.Fatal(err)
	}
	monotonicNow = 110 * time.Second
	if _, err := manager.Issue(context.Background(), identity.GatewayID(), MachineChallengeMaxTTL); err != nil {
		t.Fatal(err)
	}

	message, _ := MachineChallengeMessage(challenge)
	signature, _ := identity.Sign(message)
	monotonicNow = 109 * time.Second
	if err := manager.VerifyAndConsume(challenge, signature); !errors.Is(err, ErrMonotonicClock) {
		t.Fatalf("small regression below challenge high-water = %v", err)
	}
}

func TestMachineChallengeAllowsEqualMonotonicSample(t *testing.T) {
	wallNow := time.Unix(1_800_000_000, 0)
	monotonicNow := 100 * time.Second
	identity := testGatewayIdentity(t)
	manager := NewChallengeManager(ChallengeManagerOptions{
		Enabled:      true,
		Random:       bytes.NewReader(bytes.Repeat([]byte{22}, machineChallengeBytes)),
		Now:          func() time.Time { return wallNow },
		MonotonicNow: func() time.Duration { return monotonicNow },
		Keys:         staticGatewayKeyLookup(identity),
	})
	challenge, err := manager.Issue(context.Background(), identity.GatewayID(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	message, _ := MachineChallengeMessage(challenge)
	signature, _ := identity.Sign(message)
	if err := manager.VerifyAndConsume(challenge, signature); err != nil {
		t.Fatalf("equal monotonic sample = %v", err)
	}
}

func TestMachineChallengeMonotonicDeadlineOverflowFailsClosed(t *testing.T) {
	wallNow := time.Unix(1_800_000_000, 0)
	monotonicNow := maxMonotonicTime - time.Second + 1
	identity := testGatewayIdentity(t)
	manager := NewChallengeManager(ChallengeManagerOptions{
		Enabled:      true,
		Random:       bytes.NewReader(bytes.Repeat([]byte{23}, machineChallengeBytes)),
		Now:          func() time.Time { return wallNow },
		MonotonicNow: func() time.Duration { return monotonicNow },
		Keys:         staticGatewayKeyLookup(identity),
	})
	if _, err := manager.Issue(context.Background(), identity.GatewayID(), time.Second); !errors.Is(err, ErrMonotonicClock) {
		t.Fatalf("challenge deadline overflow = %v", err)
	}
	monotonicNow = maxMonotonicTime
	if _, err := manager.Issue(context.Background(), identity.GatewayID(), time.Second); !errors.Is(err, ErrMonotonicClock) {
		t.Fatalf("overflow did not keep challenge manager closed = %v", err)
	}
}

func TestMachineChallengeTTLStartsAfterLookupAndManagerLock(t *testing.T) {
	wallNow := time.Unix(1_800_000_000, 0)
	monotonicNow := 100 * time.Second
	identity := testGatewayIdentity(t)
	lookupDone := make(chan struct{})
	sampled := make(chan struct{}, 1)
	manager := NewChallengeManager(ChallengeManagerOptions{
		Enabled: true,
		Random:  bytes.NewReader(bytes.Repeat([]byte{24}, machineChallengeBytes)),
		Now:     func() time.Time { return wallNow },
		MonotonicNow: func() time.Duration {
			sampled <- struct{}{}
			return monotonicNow
		},
		Keys: gatewayPublicKeyLookupFunc(func(context.Context, string) ([]byte, error) {
			close(lookupDone)
			return identity.PublicKey(), nil
		}),
	})

	manager.mu.Lock()
	type issueResult struct {
		challenge MachineChallenge
		err       error
	}
	result := make(chan issueResult, 1)
	go func() {
		challenge, err := manager.Issue(context.Background(), identity.GatewayID(), time.Second)
		result <- issueResult{challenge: challenge, err: err}
	}()
	<-lookupDone
	sampledBeforeLock := false
	select {
	case <-sampled:
		sampledBeforeLock = true
	case <-time.After(50 * time.Millisecond):
	}
	monotonicNow = 200 * time.Second
	manager.mu.Unlock()
	if sampledBeforeLock {
		t.Fatal("challenge issue sampled monotonic time before acquiring the manager lock")
	}
	issued := <-result
	if issued.err != nil {
		t.Fatal(issued.err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(issued.challenge.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	record := manager.records[sha256.Sum256(raw)]
	if record == nil || record.deadline != 200*time.Second+time.Second {
		t.Fatalf("challenge deadline = %v, want 201s", record)
	}
}

func TestMachineChallengeIsDisabledByDefault(t *testing.T) {
	manager := NewChallengeManager(ChallengeManagerOptions{})
	if _, err := manager.Issue(context.Background(), testGatewayID, time.Second); err != ErrRemoteDisabled {
		t.Fatalf("disabled machine challenge = %v", err)
	}
}

func TestMachineChallengeRequiresTrustedKeyLookup(t *testing.T) {
	manager := NewChallengeManager(ChallengeManagerOptions{Enabled: true})
	if _, err := manager.Issue(context.Background(), testGatewayID, time.Second); err == nil || !strings.Contains(err.Error(), "lookup is missing") {
		t.Fatalf("missing canonical key lookup = %v", err)
	}

	wantErr := errors.New("gateway is unknown")
	manager.keys = gatewayPublicKeyLookupFunc(func(context.Context, string) ([]byte, error) {
		return nil, wantErr
	})
	if _, err := manager.Issue(context.Background(), testGatewayID, time.Second); !errors.Is(err, wantErr) {
		t.Fatalf("canonical key lookup failure = %v", err)
	}

	method, ok := reflect.TypeOf(manager).MethodByName("Issue")
	if !ok {
		t.Fatal("Issue is missing")
	}
	if method.Type.NumIn() != 4 || method.Type.In(2).Kind() != reflect.String || method.Type.In(3) != reflect.TypeOf(time.Duration(0)) {
		t.Fatalf("machine challenge issue accepts caller key data: %s", method.Type)
	}
}

func TestMachineChallengeSnapshotsCanonicalKey(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	identity := testGatewayIdentity(t)
	lookupKey := identity.PublicKey()
	manager := NewChallengeManager(ChallengeManagerOptions{
		Enabled:      true,
		Random:       bytes.NewReader(bytes.Repeat([]byte{15}, machineChallengeBytes)),
		Now:          func() time.Time { return now },
		MonotonicNow: monotonicFromWall(&now),
		Keys: gatewayPublicKeyLookupFunc(func(context.Context, string) ([]byte, error) {
			return lookupKey, nil
		}),
	})
	challenge, err := manager.Issue(context.Background(), identity.GatewayID(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	clear(lookupKey)
	message, _ := MachineChallengeMessage(challenge)
	signature, _ := identity.Sign(message)
	if err := manager.VerifyAndConsume(challenge, signature); err != nil {
		t.Fatalf("lookup key mutation changed issued challenge: %v", err)
	}
}

func TestMachineChallengeRejectsAnotherValidKey(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	expected := testGatewayIdentity(t)
	manager := NewChallengeManager(ChallengeManagerOptions{
		Enabled:      true,
		Random:       bytes.NewReader(bytes.Repeat([]byte{13}, machineChallengeBytes)),
		Now:          func() time.Time { return now },
		MonotonicNow: monotonicFromWall(&now),
		Keys:         staticGatewayKeyLookup(expected),
	})
	attacker := testGatewayIdentityForMAC(t, []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff})
	challenge, err := manager.Issue(context.Background(), expected.GatewayID(), time.Second)
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

func TestMachineChallengeRejectsCallerSelectedSite(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	expected := testGatewayIdentity(t)
	attacker := testGatewayIdentityForMAC(t, []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff})
	lookups := 0
	manager := NewChallengeManager(ChallengeManagerOptions{
		Enabled:      true,
		Random:       bytes.NewReader(bytes.Repeat([]byte{14}, machineChallengeBytes)),
		Now:          func() time.Time { return now },
		MonotonicNow: monotonicFromWall(&now),
		Keys: gatewayPublicKeyLookupFunc(func(_ context.Context, gatewayID string) ([]byte, error) {
			lookups++
			if gatewayID != expected.GatewayID() {
				return nil, errors.New("gateway is unknown")
			}
			return expected.PublicKey(), nil
		}),
	})
	challenge, err := manager.Issue(context.Background(), expected.GatewayID(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tampered := challenge
	tampered.GatewayID = attacker.GatewayID()
	message, _ := MachineChallengeMessage(tampered)
	signature, _ := attacker.Sign(message)
	if err := manager.VerifyAndConsume(tampered, signature); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("caller-selected site = %v", err)
	}
	if lookups != 1 {
		t.Fatalf("canonical key lookups = %d, want 1", lookups)
	}

	message, _ = MachineChallengeMessage(challenge)
	signature, _ = expected.Sign(message)
	if err := manager.VerifyAndConsume(challenge, signature); err != nil {
		t.Fatalf("site tamper consumed original challenge: %v", err)
	}
}

func monotonicFromWall(now *time.Time) func() time.Duration {
	epoch := *now
	return func() time.Duration { return now.Sub(epoch) }
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
	typeOf := reflect.TypeOf(record)
	want := []string{"Alias", "GatewayID", "PublicKey", "ActiveRoute", "Status", "FirstSeenAt", "LastSeenAt"}
	if typeOf.NumField() != len(want) {
		t.Fatalf("relay record field count = %d, want %d", typeOf.NumField(), len(want))
	}
	for index, fieldName := range want {
		field := typeOf.Field(index)
		if field.Name != fieldName {
			t.Fatalf("relay record field %d = %q, want %q", index, field.Name, fieldName)
		}
	}
}

type gatewayPublicKeyLookupFunc func(context.Context, string) ([]byte, error)

func (f gatewayPublicKeyLookupFunc) CanonicalPublicKey(ctx context.Context, gatewayID string) ([]byte, error) {
	return f(ctx, gatewayID)
}

func staticGatewayKeyLookup(identity gatewayidentity.Identity) GatewayPublicKeyLookup {
	return gatewayPublicKeyLookupFunc(func(_ context.Context, gatewayID string) ([]byte, error) {
		if gatewayID != identity.GatewayID() {
			return nil, errors.New("gateway is unknown")
		}
		return identity.PublicKey(), nil
	})
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
