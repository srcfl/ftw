package homelink

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const testGatewayID = "0123dca63201f838f7"

func newGrantTestManager(t *testing.T, enabled bool, now *time.Time, fill byte) *GrantManager {
	t.Helper()
	random := make([]byte, 8*grantTokenBytes)
	for block := range 8 {
		for i := range grantTokenBytes {
			random[block*grantTokenBytes+i] = fill + byte(block)
		}
	}
	manager, err := NewGrantManager(testGatewayID, GrantManagerOptions{
		Enabled: enabled,
		Random:  bytes.NewReader(random),
		Now:     func() time.Time { return *now },
		Verifier: assertionVerifierFunc(func(context.Context, AssertionExpectation, PasskeyAssertion) (Principal, error) {
			return testPrincipal(), nil
		}),
		PairingAuthorizer: pairingAuthorizerFunc(func(context.Context, LocalPairingProof) error {
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func TestRemoteDisabledByDefault(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	manager, err := NewGrantManager(testGatewayID, GrantManagerOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.IssuePairing(context.Background(), testPairingProof(), time.Minute); !errors.Is(err, ErrRemoteDisabled) {
		t.Fatalf("disabled pairing issue = %v", err)
	}
	if _, err := manager.IssueOneUseAccess(context.Background(), testExpectation(), testAssertion(), ScopeStatusRead, time.Minute); !errors.Is(err, ErrRemoteDisabled) {
		t.Fatalf("disabled access issue = %v", err)
	}
}

func TestPairingGrantIsOneUseSiteBoundAndRevocable(t *testing.T) {
	t.Run("one use", func(t *testing.T) {
		now := time.Unix(1_800_000_000, 0)
		manager := newGrantTestManager(t, true, &now, 1)
		grant, err := manager.IssuePairing(context.Background(), testPairingProof(), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.ConsumePairing(grant.Token, testGatewayID); err != nil {
			t.Fatalf("pairing consume = %v", err)
		}
		if err := manager.ConsumePairing(grant.Token, testGatewayID); !errors.Is(err, ErrGrantConsumed) {
			t.Fatalf("pairing replay = %v", err)
		}
	})

	t.Run("site", func(t *testing.T) {
		now := time.Unix(1_800_000_000, 0)
		manager := newGrantTestManager(t, true, &now, 2)
		grant, err := manager.IssuePairing(context.Background(), testPairingProof(), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.ConsumePairing(grant.Token, "0123aabbcc01ddeeff"); !errors.Is(err, ErrWrongSite) {
			t.Fatalf("wrong site = %v", err)
		}
	})

	t.Run("expiry", func(t *testing.T) {
		now := time.Unix(1_800_000_000, 0)
		manager := newGrantTestManager(t, true, &now, 3)
		grant, err := manager.IssuePairing(context.Background(), testPairingProof(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
		if err := manager.ConsumePairing(grant.Token, testGatewayID); !errors.Is(err, ErrGrantExpired) {
			t.Fatalf("expired pairing = %v", err)
		}
	})

	t.Run("revoke", func(t *testing.T) {
		now := time.Unix(1_800_000_000, 0)
		manager := newGrantTestManager(t, true, &now, 4)
		grant, err := manager.IssuePairing(context.Background(), testPairingProof(), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.Revoke(grant.Token); err != nil {
			t.Fatal(err)
		}
		if err := manager.ConsumePairing(grant.Token, testGatewayID); !errors.Is(err, ErrGrantRevoked) {
			t.Fatalf("revoked pairing = %v", err)
		}
	})
}

func TestAccessGrantRequiresLocalVerifierAndReadScope(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	manager := newGrantTestManager(t, true, &now, 5)
	if _, err := manager.IssueOneUseAccess(context.Background(), testExpectation(), testAssertion(), Scope("ftw.control.write"), time.Minute); err == nil {
		t.Fatal("access grant accepted a write scope")
	}

	grant, err := manager.IssueOneUseAccess(context.Background(), testExpectation(), testAssertion(), ScopeEnergyHistoryRead, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.VerifyAndConsumeAccess(grant.Token, "0123aabbcc01ddeeff", ScopeEnergyHistoryRead); !errors.Is(err, ErrWrongSite) {
		t.Fatalf("wrong-site access = %v", err)
	}
	if _, err := manager.VerifyAndConsumeAccess(grant.Token, testGatewayID, ScopeHealthRead); !errors.Is(err, ErrWrongScope) {
		t.Fatalf("wrong-scope access = %v", err)
	}
	authorization, err := manager.VerifyAndConsumeAccess(grant.Token, testGatewayID, ScopeEnergyHistoryRead)
	if err != nil {
		t.Fatal(err)
	}
	principal := authorization.Principal()
	if string(principal.CredentialID) != "credential-1" || principal.Label != "phone" {
		t.Fatalf("principal changed: %+v", principal)
	}
	if _, err := manager.VerifyAndConsumeAccess(grant.Token, testGatewayID, ScopeEnergyHistoryRead); !errors.Is(err, ErrGrantConsumed) {
		t.Fatalf("access replay = %v", err)
	}
}

func TestPairingGrantRequiresLocalAuthorization(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	manager, err := NewGrantManager(testGatewayID, GrantManagerOptions{Enabled: true, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.IssuePairing(context.Background(), testPairingProof(), time.Minute); err == nil {
		t.Fatal("pairing grant accepted a missing local authorizer")
	}

	wantErr := errors.New("local proof rejected")
	manager.pairingAuthorizer = pairingAuthorizerFunc(func(context.Context, LocalPairingProof) error { return wantErr })
	if _, err := manager.IssuePairing(context.Background(), testPairingProof(), time.Minute); !errors.Is(err, wantErr) {
		t.Fatalf("rejected local pairing proof = %v, want %v", err, wantErr)
	}
	if len(manager.records) != 0 {
		t.Fatal("failed local pairing authorization minted a grant")
	}
}

func TestGrantConsumptionRejectsCrossedPurposeWithoutConsuming(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	manager := newGrantTestManager(t, true, &now, 9)
	pairing, err := manager.IssuePairing(context.Background(), testPairingProof(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.VerifyAndConsumeAccess(pairing.Token, testGatewayID, ScopeStatusRead); !errors.Is(err, ErrWrongPurpose) {
		t.Fatalf("pairing consumed as access = %v", err)
	}
	if err := manager.ConsumePairing(pairing.Token, testGatewayID); err != nil {
		t.Fatalf("cross-purpose attempt consumed pairing grant: %v", err)
	}

	access, err := manager.IssueOneUseAccess(context.Background(), testExpectation(), testAssertion(), ScopeStatusRead, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ConsumePairing(access.Token, testGatewayID); !errors.Is(err, ErrWrongPurpose) {
		t.Fatalf("access consumed as pairing = %v", err)
	}
	if _, err := manager.VerifyAndConsumeAccess(access.Token, testGatewayID, ScopeStatusRead); err != nil {
		t.Fatalf("cross-purpose attempt consumed access grant: %v", err)
	}
}

func TestAccessGrantRejectsFailedOrMissingLocalVerifier(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	manager, err := NewGrantManager(testGatewayID, GrantManagerOptions{Enabled: true, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.IssueOneUseAccess(context.Background(), testExpectation(), testAssertion(), ScopeStatusRead, time.Minute); err == nil {
		t.Fatal("access grant accepted a missing local verifier")
	}

	manager.verifier = assertionVerifierFunc(func(context.Context, AssertionExpectation, PasskeyAssertion) (Principal, error) {
		return Principal{}, errors.New("assertion rejected")
	})
	if _, err := manager.IssueOneUseAccess(context.Background(), testExpectation(), testAssertion(), ScopeStatusRead, time.Minute); err == nil {
		t.Fatal("access grant accepted a rejected assertion")
	}
}

func TestAccessGrantRevocationAndRestart(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	manager := newGrantTestManager(t, true, &now, 6)
	grant, err := manager.IssueOneUseAccess(context.Background(), testExpectation(), testAssertion(), ScopeHealthRead, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got := manager.RevokeCredential([]byte("credential-1")); got != 1 {
		t.Fatalf("revoked grants = %d, want 1", got)
	}
	if _, err := manager.VerifyAndConsumeAccess(grant.Token, testGatewayID, ScopeHealthRead); !errors.Is(err, ErrGrantRevoked) {
		t.Fatalf("credential revoke = %v", err)
	}

	restarted := newGrantTestManager(t, true, &now, 7)
	if _, err := restarted.VerifyAndConsumeAccess(grant.Token, testGatewayID, ScopeHealthRead); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("grant survived process restart: %v", err)
	}
}

func TestGrantManagerStoresNoRawToken(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	manager := newGrantTestManager(t, true, &now, 8)
	grant, err := manager.IssueOneUseAccess(context.Background(), testExpectation(), testAssertion(), ScopePlanRead, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	state := fmt.Sprintf("%#v", manager.records)
	if strings.Contains(state, grant.Token) {
		t.Fatal("manager retained the raw grant token")
	}
}

func testPrincipal() Principal {
	return Principal{CredentialID: []byte("credential-1"), Label: "phone"}
}

func testExpectation() AssertionExpectation {
	return AssertionExpectation{Challenge: []byte("challenge"), RelyingPartyID: "ftw.local", Origin: "https://ftw.local"}
}

func testAssertion() PasskeyAssertion {
	return PasskeyAssertion{CredentialID: []byte("credential-1"), Signature: []byte("signature")}
}

func testPairingProof() LocalPairingProof {
	return LocalPairingProof{Challenge: []byte("local-challenge"), Response: []byte("one-time-response")}
}

type assertionVerifierFunc func(context.Context, AssertionExpectation, PasskeyAssertion) (Principal, error)

func (f assertionVerifierFunc) VerifyAssertion(
	ctx context.Context,
	expectation AssertionExpectation,
	assertion PasskeyAssertion,
) (Principal, error) {
	return f(ctx, expectation, assertion)
}

type pairingAuthorizerFunc func(context.Context, LocalPairingProof) error

func (f pairingAuthorizerFunc) AuthorizeLocalPairing(ctx context.Context, proof LocalPairingProof) error {
	return f(ctx, proof)
}
