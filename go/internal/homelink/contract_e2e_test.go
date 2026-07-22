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

	access, err := grants.IssueOneUseAccess(
		context.Background(), testExpectation(), testAssertion(), ScopeEnergyHistoryRead, time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	request := ReadRequest{
		Version: ReadContractVersion, GatewayID: testGatewayID,
		Scope: ScopeEnergyHistoryRead, History: validHistoryQuery(),
	}
	authorization, err := grants.VerifyAndConsumeAccess(access.Token, request.GatewayID, request.Scope)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := request.AuthorizedTarget(authorization); err != nil {
		t.Fatal(err)
	}

	challengeManager := NewChallengeManager(ChallengeManagerOptions{
		Enabled: true,
		Random:  bytes.NewReader(bytes.Repeat([]byte{12}, machineChallengeBytes)),
		Now:     func() time.Time { return now },
	})
	identity := testGatewayIdentity(t)
	challenge, err := challengeManager.Issue(identity.GatewayID(), identity.PublicKey(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	message, _ := MachineChallengeMessage(challenge)
	signature, _ := identity.Sign(message)
	if err := challengeManager.VerifyAndConsume(challenge, signature); err != nil {
		t.Fatal(err)
	}
}
