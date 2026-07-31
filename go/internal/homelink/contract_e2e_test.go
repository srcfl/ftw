package homelink

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestReadOnlyContractEndToEnd(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	grants := newGrantTestManager(t, true, &now, 11)

	pairing, err := grants.IssuePairing(context.Background(), testPairingProof(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := grants.ConsumePairing(pairing.Token, testGatewayID); err != nil {
		t.Fatal(err)
	}

	access := issueTestAccess(t, grants, ScopeEnergyHistoryRead, time.Minute)
	request := ReadRequest{
		Version: ReadContractVersion, GatewayID: testGatewayID,
		Scope: ScopeEnergyHistoryRead, History: validHistoryQuery(),
	}
	if err := grants.VerifyAndDispatchRead(
		context.Background(), access.Token, request,
	); err != nil {
		t.Fatal(err)
	}

	identity := testGatewayIdentity(t)
	challengeManager := NewChallengeManager(ChallengeManagerOptions{
		Enabled:      true,
		Random:       bytes.NewReader(bytes.Repeat([]byte{12}, machineChallengeBytes)),
		Now:          func() time.Time { return now },
		MonotonicNow: monotonicFromWall(&now),
		Keys:         staticGatewayKeyLookup(identity),
	})
	challenge, err := challengeManager.Issue(context.Background(), identity.GatewayID(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	message, _ := MachineChallengeMessage(challenge)
	signature, _ := identity.Sign(message)
	if err := challengeManager.VerifyAndConsume(challenge, signature); err != nil {
		t.Fatal(err)
	}
}
