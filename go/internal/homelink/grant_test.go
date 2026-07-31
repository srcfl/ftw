package homelink

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const testGatewayID = "0123dca63201f838f7"

func newGrantTestManager(t *testing.T, enabled bool, now *time.Time, fill byte) *GrantManager {
	t.Helper()
	epoch := *now
	authority := newMemoryCredentialAuthority(newMemoryCredentialState())
	return newGrantTestManagerWithAuthority(
		t, enabled, func() time.Time { return *now },
		func() time.Duration { return now.Sub(epoch) }, fill, authority,
	)
}

func newGrantTestManagerWithAuthority(
	t *testing.T,
	enabled bool,
	now func() time.Time,
	monotonicNow func() time.Duration,
	fill byte,
	authority CredentialAuthority,
) *GrantManager {
	t.Helper()
	return newGrantTestManagerForSiteWithAuthority(
		t, testGatewayID, enabled, now, monotonicNow, fill, authority,
	)
}

func newGrantTestManagerForSiteWithAuthority(
	t *testing.T,
	siteID string,
	enabled bool,
	now func() time.Time,
	monotonicNow func() time.Duration,
	fill byte,
	authority CredentialAuthority,
) *GrantManager {
	t.Helper()
	random := make([]byte, 8*grantTokenBytes)
	for block := range 8 {
		for i := range grantTokenBytes {
			random[block*grantTokenBytes+i] = fill + byte(block)
		}
	}
	manager, err := NewGrantManager(siteID, GrantManagerOptions{
		Enabled: enabled, Random: bytes.NewReader(random), Now: now,
		MonotonicNow: monotonicNow, CredentialAuthority: authority,
		ReadDispatcher: successfulReadDispatcher(),
		PairingAuthorizer: pairingAuthorizerFunc(func(context.Context, LocalPairingProof) error {
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { forgetSiteCredentialCoordinatorForTest(siteID) })
	return manager
}

func TestRemoteDisabledByDefault(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	manager, err := NewGrantManager(testGatewayID, GrantManagerOptions{
		Now:                 func() time.Time { return now },
		CredentialAuthority: newMemoryCredentialAuthority(newMemoryCredentialState()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.IssuePairing(context.Background(), testPairingProof(), time.Minute); !errors.Is(err, ErrRemoteDisabled) {
		t.Fatalf("disabled pairing issue = %v", err)
	}
	if _, err := manager.IssueOneUseAccess(context.Background(), "opaque-id", testAssertion(), ScopePlanRead, time.Minute); !errors.Is(err, ErrRemoteDisabled) {
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

func TestAccessGrantRequiresLocalAuthorityAndReadScope(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	manager := newGrantTestManager(t, true, &now, 5)
	challenge, err := manager.BeginLocalAssertion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.IssueOneUseAccess(context.Background(), challenge.ID, testAssertion(), Scope("ftw.control.write"), time.Minute); err == nil {
		t.Fatal("access grant accepted a write scope")
	}
	if _, err := manager.IssueOneUseAccess(
		context.Background(), challenge.ID, testAssertion(), ScopePlanRead, time.Minute,
	); err == nil || !strings.Contains(err.Error(), "unknown or consumed") {
		t.Fatalf("invalid scope left assertion expectation reusable: %v", err)
	}

	grant := issueTestAccess(t, manager, ScopeEnergyHistoryRead, time.Minute)
	dispatcher := readDispatcherFunc(func(_ context.Context, _ ReadTarget, _ ReadRequest, principal Principal) error {
		if string(principal.CredentialID) != "credential-1" || principal.Label != "phone" {
			t.Fatalf("principal changed: %+v", principal)
		}
		return nil
	})
	manager.readDispatcher = dispatcher
	wrongSite := testReadRequest(ScopeEnergyHistoryRead)
	wrongSite.GatewayID = "0123aabbcc01ddeeff"
	if err := manager.VerifyAndDispatchRead(context.Background(), grant.Token, wrongSite); !errors.Is(err, ErrWrongSite) {
		t.Fatalf("wrong-site access = %v", err)
	}
	if err := manager.VerifyAndDispatchRead(context.Background(), grant.Token, testReadRequest(ScopeHealthRead)); !errors.Is(err, ErrWrongScope) {
		t.Fatalf("wrong-scope access = %v", err)
	}
	if err := manager.VerifyAndDispatchRead(context.Background(), grant.Token, testReadRequest(ScopeEnergyHistoryRead)); err != nil {
		t.Fatal(err)
	}
	if err := manager.VerifyAndDispatchRead(context.Background(), grant.Token, testReadRequest(ScopeEnergyHistoryRead)); !errors.Is(err, ErrGrantConsumed) {
		t.Fatalf("access replay = %v", err)
	}
}

func TestPasskeyExpectationIsLocalAndOneUse(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	manager := newGrantTestManager(t, true, &now, 15)
	challenge, err := manager.BeginLocalAssertion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.IssueOneUseAccess(
		context.Background(), challenge.ID, testAssertion(), ScopePlanRead, time.Minute,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.IssueOneUseAccess(
		context.Background(), challenge.ID, testAssertion(), ScopePlanRead, time.Minute,
	); err == nil || !strings.Contains(err.Error(), "expectation") {
		t.Fatalf("reused local expectation = %v", err)
	}
	if _, err := manager.IssueOneUseAccess(
		context.Background(), "caller-chosen-id", testAssertion(), ScopePlanRead, time.Minute,
	); err == nil || !strings.Contains(err.Error(), "expectation") {
		t.Fatalf("caller-selected expectation = %v", err)
	}

	method, ok := reflect.TypeOf(manager).MethodByName("IssueOneUseAccess")
	if !ok {
		t.Fatal("IssueOneUseAccess is missing")
	}
	if method.Type.NumIn() != 6 || method.Type.In(2).Kind() != reflect.String {
		t.Fatalf("public access issue signature accepts more than an opaque challenge id: %s", method.Type)
	}
	begin, ok := reflect.TypeOf(manager).MethodByName("BeginLocalAssertion")
	if !ok || begin.Type.NumIn() != 2 {
		t.Fatalf("caller can choose assertion timing: %v", begin.Type)
	}
}

func TestFailedPasskeyAssertionConsumesLocalExpectation(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	manager := newGrantTestManager(t, true, &now, 22)
	challenge, err := manager.BeginLocalAssertion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	bad := testAssertion()
	bad.CredentialID = []byte("attacker-credential")
	if _, err := manager.IssueOneUseAccess(
		context.Background(), challenge.ID, bad, ScopePlanRead, time.Minute,
	); err == nil {
		t.Fatal("invalid assertion was accepted")
	}
	if _, err := manager.IssueOneUseAccess(
		context.Background(), challenge.ID, testAssertion(), ScopePlanRead, time.Minute,
	); err == nil || !strings.Contains(err.Error(), "expectation") {
		t.Fatalf("failed assertion left its expectation reusable: %v", err)
	}

	challenge, err = manager.BeginLocalAssertion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.IssueOneUseAccess(
		context.Background(), challenge.ID, testAssertion(), ScopePlanRead, 0,
	); err == nil || !strings.Contains(err.Error(), "lifetime") {
		t.Fatalf("invalid grant lifetime = %v", err)
	}
	if _, err := manager.IssueOneUseAccess(
		context.Background(), challenge.ID, testAssertion(), ScopePlanRead, time.Minute,
	); err == nil || !strings.Contains(err.Error(), "unknown or consumed") {
		t.Fatalf("invalid grant lifetime left expectation reusable: %v", err)
	}

	challenge, err = manager.BeginLocalAssertion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.IssueOneUseAccess(
		canceled, challenge.ID, testAssertion(), ScopePlanRead, time.Minute,
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled assertion = %v", err)
	}
	if _, err := manager.IssueOneUseAccess(
		context.Background(), challenge.ID, testAssertion(), ScopePlanRead, time.Minute,
	); err == nil || !strings.Contains(err.Error(), "unknown or consumed") {
		t.Fatalf("canceled assertion left expectation reusable: %v", err)
	}

	challenge, err = manager.BeginLocalAssertion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	manager.random = bytes.NewReader(nil)
	if _, err := manager.IssueOneUseAccess(
		context.Background(), challenge.ID, testAssertion(), ScopePlanRead, time.Minute,
	); err == nil || !strings.Contains(err.Error(), "create grant") {
		t.Fatalf("grant creation failure = %v", err)
	}
	if _, err := manager.IssueOneUseAccess(
		context.Background(), challenge.ID, testAssertion(), ScopePlanRead, time.Minute,
	); err == nil || !strings.Contains(err.Error(), "unknown or consumed") {
		t.Fatalf("grant creation failure left expectation reusable: %v", err)
	}
}

func TestPasskeyExpectationUsesMonotonicDeadline(t *testing.T) {
	wallNow := time.Unix(1_800_000_000, 0)
	monotonicNow := 100 * time.Second
	state := newMemoryCredentialState()
	manager := newGrantTestManagerWithAuthority(
		t, true, func() time.Time { return wallNow }, func() time.Duration { return monotonicNow }, 30,
		newMemoryCredentialAuthority(state),
	)

	t.Run("one nanosecond before deadline", func(t *testing.T) {
		challenge, err := manager.BeginLocalAssertion(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		monotonicNow += AssertionExpectationMaxAge - time.Nanosecond
		if _, err := manager.IssueOneUseAccess(
			context.Background(), challenge.ID, testAssertion(), ScopePlanRead, time.Minute,
		); err != nil {
			t.Fatalf("assertion before exact deadline = %v", err)
		}
	})

	t.Run("exact deadline", func(t *testing.T) {
		challenge, err := manager.BeginLocalAssertion(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		monotonicNow += AssertionExpectationMaxAge
		if _, err := manager.IssueOneUseAccess(
			context.Background(), challenge.ID, testAssertion(), ScopePlanRead, time.Minute,
		); !errors.Is(err, ErrAssertionExpired) {
			t.Fatalf("assertion at exact deadline = %v", err)
		}
		if _, err := manager.IssueOneUseAccess(
			context.Background(), challenge.ID, testAssertion(), ScopePlanRead, time.Minute,
		); err == nil || !strings.Contains(err.Error(), "unknown or consumed") {
			t.Fatalf("expired assertion remained reusable: %v", err)
		}
	})
}

func TestPasskeyExpectationRejectsPartialWallRollback(t *testing.T) {
	issuedAt := time.Unix(1_800_000_000, 0)
	cases := []struct {
		name       string
		elapsed    time.Duration
		rolledBack time.Duration
	}{
		{name: "small rollback", elapsed: AssertionExpectationMaxAge + time.Nanosecond, rolledBack: AssertionExpectationMaxAge - time.Nanosecond},
		{name: "large rollback", elapsed: 24 * time.Hour, rolledBack: time.Second},
	}
	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wallNow := issuedAt
			monotonicNow := 100 * time.Second
			state := newMemoryCredentialState()
			manager := newGrantTestManagerWithAuthority(
				t, true, func() time.Time { return wallNow }, func() time.Duration { return monotonicNow },
				byte(40+index), newMemoryCredentialAuthority(state),
			)
			challenge, err := manager.BeginLocalAssertion(context.Background())
			if err != nil {
				t.Fatal(err)
			}

			wallNow = issuedAt.Add(tc.elapsed)
			monotonicNow += tc.elapsed
			wallNow = issuedAt.Add(tc.rolledBack)
			if _, err := manager.IssueOneUseAccess(
				context.Background(), challenge.ID, testAssertion(), ScopePlanRead, time.Minute,
			); !errors.Is(err, ErrAssertionExpired) {
				t.Fatalf("wall rollback reopened expired assertion = %v", err)
			}
		})
	}
}

func TestPasskeyExpectationDoesNotSurviveManagerRestart(t *testing.T) {
	wallNow := time.Unix(1_800_000_000, 0)
	monotonicNow := 100 * time.Second
	state := newMemoryCredentialState()
	authority := newMemoryCredentialAuthority(state)
	first := newGrantTestManagerWithAuthority(
		t, true, func() time.Time { return wallNow }, func() time.Duration { return monotonicNow }, 42, authority,
	)
	challenge, err := first.BeginLocalAssertion(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	monotonicNow = 0
	restarted := newGrantTestManagerWithAuthority(
		t, true, func() time.Time { return wallNow }, func() time.Duration { return monotonicNow }, 43, authority,
	)
	if first.assertionSession == restarted.assertionSession {
		t.Fatal("new manager reused the prior assertion session")
	}
	if _, err := restarted.IssueOneUseAccess(
		context.Background(), challenge.ID, testAssertion(), ScopePlanRead, time.Minute,
	); !errors.Is(err, ErrAssertionSession) {
		t.Fatalf("restarted manager accepted old authority state = %v", err)
	}
	if _, err := restarted.IssueOneUseAccess(
		context.Background(), challenge.ID, testAssertion(), ScopePlanRead, time.Minute,
	); err == nil || !strings.Contains(err.Error(), "unknown or consumed") {
		t.Fatalf("session mismatch left old expectation reusable: %v", err)
	}
}

func TestPairingGrantRequiresLocalAuthorization(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	manager, err := NewGrantManager(testGatewayID, GrantManagerOptions{
		Enabled: true, Now: func() time.Time { return now },
		CredentialAuthority: newMemoryCredentialAuthority(newMemoryCredentialState()),
	})
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
	if err := manager.VerifyAndDispatchRead(context.Background(), pairing.Token, testReadRequest(ScopePlanRead)); !errors.Is(err, ErrWrongPurpose) {
		t.Fatalf("pairing consumed as access = %v", err)
	}
	if err := manager.ConsumePairing(pairing.Token, testGatewayID); err != nil {
		t.Fatalf("cross-purpose attempt consumed pairing grant: %v", err)
	}

	access := issueTestAccess(t, manager, ScopePlanRead, time.Minute)
	if err := manager.ConsumePairing(access.Token, testGatewayID); !errors.Is(err, ErrWrongPurpose) {
		t.Fatalf("access consumed as pairing = %v", err)
	}
	if err := manager.VerifyAndDispatchRead(context.Background(), access.Token, testReadRequest(ScopePlanRead)); err != nil {
		t.Fatalf("cross-purpose attempt consumed access grant: %v", err)
	}
}

func TestAccessGrantRequiresCredentialAuthority(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	if _, err := NewGrantManager(testGatewayID, GrantManagerOptions{
		Enabled: true, Now: func() time.Time { return now },
	}); err == nil || !strings.Contains(err.Error(), "credential authority") {
		t.Fatalf("manager accepted a missing credential authority: %v", err)
	}

	authority := credentialAuthorityStub{
		create: func(context.Context, AssertionExpectationBinding) (LocalAssertionChallenge, error) {
			return LocalAssertionChallenge{ID: "local-id", Challenge: []byte("local-challenge")}, nil
		},
		verify: func(context.Context, string, PasskeyAssertion) (Principal, AssertionExpectationBinding, error) {
			return Principal{}, AssertionExpectationBinding{}, errors.New("assertion rejected")
		},
		revoke: func(context.Context, []byte) error { return nil },
	}
	manager := newGrantTestManagerWithAuthority(
		t, true, func() time.Time { return now }, func() time.Duration { return 0 }, 45, authority,
	)
	challenge, err := manager.BeginLocalAssertion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.IssueOneUseAccess(context.Background(), challenge.ID, testAssertion(), ScopePlanRead, time.Minute); err == nil {
		t.Fatal("access grant accepted a rejected assertion")
	}
}

func TestGrantManagerRejectsDecoratedAuthorityFromAnotherSite(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	otherSite := "1123dca63201f838f7"
	authority := blockingVerifyCredentialAuthority{
		CredentialAuthority: newMemoryCredentialAuthorityForSite(
			newMemoryCredentialState(), otherSite,
		),
		verifyStarted:   make(chan struct{}),
		verifyMayFinish: make(chan struct{}),
	}
	if _, err := NewGrantManager(testGatewayID, GrantManagerOptions{
		Enabled: true, Now: func() time.Time { return now },
		CredentialAuthority: authority,
		ReadDispatcher:      successfulReadDispatcher(),
	}); err == nil || !strings.Contains(err.Error(), "another gateway") {
		t.Fatalf("manager accepted decorated authority from %s: %v", otherSite, err)
	}
}

func TestAccessGrantRevocationAndRestart(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	state := newMemoryCredentialState()
	manager := newGrantTestManagerWithAuthority(
		t, true, func() time.Time { return now }, func() time.Duration { return 0 }, 6,
		newMemoryCredentialAuthority(state),
	)
	grant := issueTestAccess(t, manager, ScopeHealthRead, time.Minute)
	if err := manager.RevokeCredential(context.Background(), []byte("credential-1")); err != nil {
		t.Fatal(err)
	}
	if err := manager.VerifyAndDispatchRead(context.Background(), grant.Token, testReadRequest(ScopeHealthRead)); !errors.Is(err, ErrGrantRevoked) {
		t.Fatalf("credential revoke = %v", err)
	}
	if _, err := issueTestAccessResult(manager, ScopeHealthRead, time.Minute); !errors.Is(err, ErrCredentialRevoked) {
		t.Fatalf("revoked credential minted a new grant: %v", err)
	}

	restarted := newGrantTestManagerWithAuthority(
		t, true, func() time.Time { return now }, func() time.Duration { return 0 }, 7,
		newMemoryCredentialAuthority(state),
	)
	if err := restarted.VerifyAndDispatchRead(context.Background(), grant.Token, testReadRequest(ScopeHealthRead)); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("grant survived process restart: %v", err)
	}
	if _, err := issueTestAccessResult(restarted, ScopeHealthRead, time.Minute); !errors.Is(err, ErrCredentialRevoked) {
		t.Fatalf("durable credential revoke did not survive restart: %v", err)
	}
}

func TestCredentialRevokeBlocksGrantIssueUntilDurableCommit(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	state := newMemoryCredentialState()
	commitStarted := make(chan struct{})
	commitMayFinish := make(chan struct{})
	authority := blockingCredentialAuthority{
		CredentialAuthority: newMemoryCredentialAuthority(state),
		revokeStarted:       commitStarted,
		revokeMayFinish:     commitMayFinish,
	}
	manager := newGrantTestManagerWithAuthority(
		t, true, func() time.Time { return now }, func() time.Duration { return 0 }, 23, authority,
	)
	challenge, err := manager.BeginLocalAssertion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	revokeResult := make(chan error, 1)
	go func() {
		revokeResult <- manager.RevokeCredential(context.Background(), testPrincipal().CredentialID)
	}()
	<-commitStarted

	issueResult := make(chan error, 1)
	issueStarted := make(chan struct{})
	go func() {
		close(issueStarted)
		_, err := manager.IssueOneUseAccess(
			context.Background(), challenge.ID, testAssertion(), ScopePlanRead, time.Minute,
		)
		issueResult <- err
	}()
	<-issueStarted
	select {
	case err := <-issueResult:
		t.Fatalf("grant issue passed during revoke commit: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	select {
	case err := <-revokeResult:
		t.Fatalf("revoke returned before durable commit: %v", err)
	default:
	}

	close(commitMayFinish)
	if err := <-revokeResult; err != nil {
		t.Fatal(err)
	}
	if err := <-issueResult; !errors.Is(err, ErrCredentialRevoked) {
		t.Fatalf("grant issue after revoke = %v", err)
	}
}

func TestCredentialRevokeIsSharedAcrossManagers(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	authority := newMemoryCredentialAuthority(newMemoryCredentialState())
	first := newGrantTestManagerWithAuthority(
		t, true, func() time.Time { return now }, func() time.Duration { return 0 }, 60,
		authority,
	)
	second := newGrantTestManagerWithAuthority(
		t, true, func() time.Time { return now }, func() time.Duration { return 0 }, 61,
		authority,
	)
	grant := issueTestAccess(t, first, ScopePlanRead, time.Minute)
	if err := second.RevokeCredential(
		context.Background(), testPrincipal().CredentialID,
	); err != nil {
		t.Fatal(err)
	}
	if err := first.VerifyAndDispatchRead(
		context.Background(), grant.Token, testReadRequest(ScopePlanRead),
	); !errors.Is(err, ErrCredentialRevoked) {
		t.Fatalf("other manager consumed grant after revoke = %v", err)
	}
}

func TestCredentialRevokeDoesNotCrossSites(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	firstSite := newGrantTestManagerForSiteWithAuthority(
		t, testGatewayID, true, func() time.Time { return now },
		func() time.Duration { return 0 }, 65,
		newMemoryCredentialAuthority(newMemoryCredentialState()),
	)
	secondSiteID := "1123dca63201f838f7"
	secondSite := newGrantTestManagerForSiteWithAuthority(
		t, secondSiteID, true, func() time.Time { return now },
		func() time.Duration { return 0 }, 66,
		newMemoryCredentialAuthorityForSite(newMemoryCredentialState(), secondSiteID),
	)
	grant := issueTestAccess(t, secondSite, ScopePlanRead, time.Minute)
	if err := firstSite.RevokeCredential(
		context.Background(), testPrincipal().CredentialID,
	); err != nil {
		t.Fatal(err)
	}
	request := testReadRequest(ScopePlanRead)
	request.GatewayID = secondSiteID
	if err := secondSite.VerifyAndDispatchRead(
		context.Background(), grant.Token, request,
	); err != nil {
		t.Fatalf("revoke crossed site boundary: %v", err)
	}
}

func TestGrantIssueStartedBeforeRevokeCompletesFirst(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	state := newMemoryCredentialState()
	authority := newMemoryCredentialAuthority(state)
	verifyStarted := make(chan struct{})
	verifyMayFinish := make(chan struct{})
	first := newGrantTestManagerWithAuthority(
		t, true, func() time.Time { return now }, func() time.Duration { return 0 }, 62,
		blockingVerifyCredentialAuthority{
			CredentialAuthority: authority,
			verifyStarted:       verifyStarted,
			verifyMayFinish:     verifyMayFinish,
		},
	)
	second := newGrantTestManagerWithAuthority(
		t, true, func() time.Time { return now }, func() time.Duration { return 0 }, 63,
		authority,
	)
	challenge, err := first.BeginLocalAssertion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	type issueOutcome struct {
		grant Grant
		err   error
	}
	issueResult := make(chan issueOutcome, 1)
	go func() {
		grant, err := first.IssueOneUseAccess(
			context.Background(), challenge.ID, testAssertion(), ScopePlanRead, time.Minute,
		)
		issueResult <- issueOutcome{grant: grant, err: err}
	}()
	<-verifyStarted
	revokeResult := make(chan error, 1)
	go func() {
		revokeResult <- second.RevokeCredential(
			context.Background(), testPrincipal().CredentialID,
		)
	}()
	select {
	case err := <-revokeResult:
		close(verifyMayFinish)
		<-issueResult
		t.Fatalf("revoke passed an in-flight grant issue: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(verifyMayFinish)
	issued := <-issueResult
	if issued.err != nil {
		t.Fatalf("grant issue that started first = %v", issued.err)
	}
	if err := <-revokeResult; err != nil {
		t.Fatal(err)
	}
	if err := first.VerifyAndDispatchRead(
		context.Background(), issued.grant.Token, testReadRequest(ScopePlanRead),
	); !errors.Is(err, ErrCredentialRevoked) {
		t.Fatalf("grant minted before revoke remained usable = %v", err)
	}
}

func TestCredentialRevokeFailureStaysClosedAcrossRestart(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	state := newMemoryCredentialState()
	wantErr := errors.New("credential store unavailable")
	authority := newMemoryCredentialAuthority(state)
	authority.revokeErr = wantErr
	manager := newGrantTestManagerWithAuthority(
		t, true, func() time.Time { return now }, func() time.Duration { return 0 }, 16, authority,
	)
	planGrant := issueTestAccess(t, manager, ScopePlanRead, time.Minute)
	healthGrant := issueTestAccess(t, manager, ScopeHealthRead, time.Minute)

	if err := manager.RevokeCredential(context.Background(), []byte("credential-1")); !errors.Is(err, wantErr) {
		t.Fatalf("credential store failure = %v", err)
	}
	for _, item := range []struct {
		grant Grant
		scope Scope
	}{{planGrant, ScopePlanRead}, {healthGrant, ScopeHealthRead}} {
		if err := manager.VerifyAndDispatchRead(
			context.Background(), item.grant.Token, testReadRequest(item.scope),
		); !errors.Is(err, ErrGrantRevoked) {
			t.Fatalf("grant survived failed credential revoke = %v", err)
		}
	}
	if _, err := issueTestAccessResult(manager, ScopePlanRead, time.Minute); !errors.Is(err, ErrCredentialRevoked) {
		t.Fatalf("new grant after failed credential revoke = %v", err)
	}

	restarted := newGrantTestManagerWithAuthority(
		t, true, func() time.Time { return now }, func() time.Duration { return 0 }, 17,
		newMemoryCredentialAuthority(state),
	)
	if _, err := issueTestAccessResult(restarted, ScopePlanRead, time.Minute); !errors.Is(err, ErrCredentialRevoked) {
		t.Fatalf("failed revoke reopened credential after restart: %v", err)
	}
}

func TestAccessGrantExpiryUsesMonotonicDeadline(t *testing.T) {
	wallNow := time.Unix(1_800_000_000, 0)
	monotonicNow := time.Duration(0)
	state := newMemoryCredentialState()
	manager := newGrantTestManagerWithAuthority(
		t, true, func() time.Time { return wallNow }, func() time.Duration { return monotonicNow }, 18,
		newMemoryCredentialAuthority(state),
	)
	grant := issueTestAccess(t, manager, ScopePlanRead, time.Second)
	wallNow = wallNow.Add(-24 * time.Hour)
	monotonicNow = time.Second
	if err := manager.VerifyAndDispatchRead(
		context.Background(), grant.Token, testReadRequest(ScopePlanRead),
	); !errors.Is(err, ErrGrantExpired) {
		t.Fatalf("wall-clock rollback extended access grant: %v", err)
	}
}

func TestGrantManagerMonotonicRegressionBeforeIssueFailsClosed(t *testing.T) {
	wallNow := time.Unix(1_800_000_000, 0)
	monotonicNow := 100 * time.Second
	state := newMemoryCredentialState()
	manager := newGrantTestManagerWithAuthority(
		t, true, func() time.Time { return wallNow }, func() time.Duration { return monotonicNow }, 46,
		newMemoryCredentialAuthority(state),
	)
	challenge, err := manager.BeginLocalAssertion(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	monotonicNow = 50 * time.Second
	if _, err := manager.IssueOneUseAccess(
		context.Background(), challenge.ID, testAssertion(), ScopePlanRead, time.Minute,
	); !errors.Is(err, ErrMonotonicClock) {
		t.Fatalf("100s to 50s before grant issue = %v", err)
	}
	state.mu.Lock()
	remainingExpectations := len(state.expectations)
	state.mu.Unlock()
	if remainingExpectations != 0 {
		t.Fatalf("clock failure left %d assertion expectations", remainingExpectations)
	}

	monotonicNow = 200 * time.Second
	if _, err := manager.BeginLocalAssertion(context.Background()); !errors.Is(err, ErrMonotonicClock) {
		t.Fatalf("clock recovery reopened manager = %v", err)
	}
}

func TestGrantManagerMonotonicRegressionAfterIssueFailsClosed(t *testing.T) {
	wallNow := time.Unix(1_800_000_000, 0)
	monotonicNow := 100 * time.Second
	manager := newGrantTestManagerWithAuthority(
		t, true, func() time.Time { return wallNow }, func() time.Duration { return monotonicNow }, 47,
		newMemoryCredentialAuthority(newMemoryCredentialState()),
	)
	grant := issueTestAccess(t, manager, ScopePlanRead, time.Minute)
	var dispatches int
	manager.readDispatcher = readDispatcherFunc(func(context.Context, ReadTarget, ReadRequest, Principal) error {
		dispatches++
		return nil
	})

	monotonicNow = 50 * time.Second
	request := testReadRequest(ScopePlanRead)
	if err := manager.VerifyAndDispatchRead(context.Background(), grant.Token, request); !errors.Is(err, ErrMonotonicClock) {
		t.Fatalf("100s to 50s after grant issue = %v", err)
	}
	monotonicNow = 200 * time.Second
	if err := manager.VerifyAndDispatchRead(context.Background(), grant.Token, request); !errors.Is(err, ErrMonotonicClock) {
		t.Fatalf("clock recovery reopened issued grant = %v", err)
	}
	if dispatches != 0 {
		t.Fatalf("invalid clock dispatched %d reads", dispatches)
	}
}

func TestGrantManagerRejectsRegressionBelowHighWater(t *testing.T) {
	wallNow := time.Unix(1_800_000_000, 0)
	monotonicNow := 100 * time.Second
	manager := newGrantTestManagerWithAuthority(
		t, true, func() time.Time { return wallNow }, func() time.Duration { return monotonicNow }, 48,
		newMemoryCredentialAuthority(newMemoryCredentialState()),
	)
	grant := issueTestAccess(t, manager, ScopePlanRead, time.Minute)

	monotonicNow = 110 * time.Second
	wrongScope := testReadRequest(ScopeHealthRead)
	if err := manager.VerifyAndDispatchRead(context.Background(), grant.Token, wrongScope); !errors.Is(err, ErrWrongScope) {
		t.Fatalf("high-water advance = %v", err)
	}
	monotonicNow = 109 * time.Second
	if err := manager.VerifyAndDispatchRead(
		context.Background(), grant.Token, testReadRequest(ScopePlanRead),
	); !errors.Is(err, ErrMonotonicClock) {
		t.Fatalf("small regression below high-water = %v", err)
	}
}

func TestGrantManagerAllowsEqualMonotonicSample(t *testing.T) {
	wallNow := time.Unix(1_800_000_000, 0)
	monotonicNow := 100 * time.Second
	manager := newGrantTestManagerWithAuthority(
		t, true, func() time.Time { return wallNow }, func() time.Duration { return monotonicNow }, 49,
		newMemoryCredentialAuthority(newMemoryCredentialState()),
	)
	grant := issueTestAccess(t, manager, ScopePlanRead, time.Minute)
	if err := manager.VerifyAndDispatchRead(
		context.Background(), grant.Token, testReadRequest(ScopePlanRead),
	); err != nil {
		t.Fatalf("equal monotonic sample = %v", err)
	}
}

func TestGrantManagerMonotonicDeadlineOverflowFailsClosed(t *testing.T) {
	wallNow := time.Unix(1_800_000_000, 0)
	monotonicNow := maxMonotonicTime - AssertionExpectationMaxAge
	manager := newGrantTestManagerWithAuthority(
		t, true, func() time.Time { return wallNow }, func() time.Duration { return monotonicNow }, 50,
		newMemoryCredentialAuthority(newMemoryCredentialState()),
	)
	challenge, err := manager.BeginLocalAssertion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	monotonicNow = maxMonotonicTime - time.Minute + 1
	if _, err := manager.IssueOneUseAccess(
		context.Background(), challenge.ID, testAssertion(), ScopePlanRead, time.Minute,
	); !errors.Is(err, ErrMonotonicClock) {
		t.Fatalf("grant deadline overflow = %v", err)
	}
	monotonicNow = maxMonotonicTime
	if _, err := manager.BeginLocalAssertion(context.Background()); !errors.Is(err, ErrMonotonicClock) {
		t.Fatalf("overflow did not keep manager closed = %v", err)
	}
}

func TestGrantManagerStoresNoRawToken(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	manager := newGrantTestManager(t, true, &now, 8)
	grant := issueTestAccess(t, manager, ScopePlanRead, time.Minute)
	state := fmt.Sprintf("%#v", manager.records)
	if strings.Contains(state, grant.Token) {
		t.Fatal("manager retained the raw grant token")
	}
}

func testPrincipal() Principal {
	return Principal{CredentialID: []byte("credential-1"), Label: "phone"}
}

func testAssertion() PasskeyAssertion {
	return PasskeyAssertion{CredentialID: []byte("credential-1"), Signature: []byte("signature")}
}

func testPairingProof() LocalPairingProof {
	return LocalPairingProof{Challenge: []byte("local-challenge"), Response: []byte("one-time-response")}
}

func issueTestAccess(t *testing.T, manager *GrantManager, scope Scope, ttl time.Duration) Grant {
	t.Helper()
	grant, err := issueTestAccessResult(manager, scope, ttl)
	if err != nil {
		t.Fatal(err)
	}
	return grant
}

func issueTestAccessResult(manager *GrantManager, scope Scope, ttl time.Duration) (Grant, error) {
	challenge, err := manager.BeginLocalAssertion(context.Background())
	if err != nil {
		return Grant{}, err
	}
	return manager.IssueOneUseAccess(context.Background(), challenge.ID, testAssertion(), scope, ttl)
}

func testReadRequest(scope Scope) ReadRequest {
	request := ReadRequest{Version: ReadContractVersion, GatewayID: testGatewayID, Scope: scope}
	if scope == ScopeEnergyHistoryRead {
		request.History = validHistoryQuery()
	}
	return request
}

type credentialAuthorityStub struct {
	site   CredentialSite
	create func(context.Context, AssertionExpectationBinding) (LocalAssertionChallenge, error)
	verify func(context.Context, string, PasskeyAssertion) (Principal, AssertionExpectationBinding, error)
	revoke func(context.Context, []byte) error
}

func (s credentialAuthorityStub) CredentialSite() CredentialSite {
	if s.site.coordinator == nil {
		return CredentialSite{
			id: testGatewayID, coordinator: siteCredentialCoordinatorFor(testGatewayID),
		}
	}
	return s.site
}

func (s credentialAuthorityStub) CreateAssertion(
	ctx context.Context,
	binding AssertionExpectationBinding,
	_ ...credentialSiteOperation,
) (LocalAssertionChallenge, error) {
	return s.create(ctx, binding)
}

func (s credentialAuthorityStub) VerifyAndConsumeAssertion(
	ctx context.Context,
	challengeID string,
	assertion PasskeyAssertion,
	_ ...credentialSiteOperation,
) (Principal, AssertionExpectationBinding, error) {
	return s.verify(ctx, challengeID, assertion)
}

func (s credentialAuthorityStub) RevokeCredential(
	ctx context.Context,
	credentialID []byte,
	_ ...credentialSiteOperation,
) error {
	return s.revoke(ctx, credentialID)
}

type memoryCredentialState struct {
	mu           sync.Mutex
	next         int
	expectations map[string]memoryAssertionExpectation
	credentials  map[string]memoryCredentialStatus
}

type memoryCredentialStatus uint8

const (
	memoryCredentialActive memoryCredentialStatus = iota
	memoryCredentialRevoked
	memoryCredentialUncertain
)

type memoryAssertionExpectation struct {
	challenge []byte
	rpID      string
	origin    string
	binding   AssertionExpectationBinding
}

func newMemoryCredentialState() *memoryCredentialState {
	return &memoryCredentialState{
		expectations: make(map[string]memoryAssertionExpectation),
		credentials:  make(map[string]memoryCredentialStatus),
	}
}

type memoryCredentialAuthority struct {
	state     *memoryCredentialState
	revokeErr error
	site      CredentialSite
}

func newMemoryCredentialAuthority(state *memoryCredentialState) *memoryCredentialAuthority {
	return newMemoryCredentialAuthorityForSite(state, testGatewayID)
}

func newMemoryCredentialAuthorityForSite(
	state *memoryCredentialState,
	siteID string,
) *memoryCredentialAuthority {
	return &memoryCredentialAuthority{
		state: state,
		site: CredentialSite{
			id: siteID, coordinator: siteCredentialCoordinatorFor(siteID),
		},
	}
}

func (a *memoryCredentialAuthority) CredentialSite() CredentialSite {
	return a.site
}

func (a *memoryCredentialAuthority) CreateAssertion(
	ctx context.Context,
	binding AssertionExpectationBinding,
	_ ...credentialSiteOperation,
) (LocalAssertionChallenge, error) {
	if err := ctx.Err(); err != nil {
		return LocalAssertionChallenge{}, err
	}
	a.state.mu.Lock()
	defer a.state.mu.Unlock()
	a.state.next++
	id := fmt.Sprintf("local-assertion-%d", a.state.next)
	challenge := []byte(fmt.Sprintf("challenge-%d", a.state.next))
	a.state.expectations[id] = memoryAssertionExpectation{
		challenge: bytes.Clone(challenge), rpID: "ftw.local", origin: "https://ftw.local",
		binding: binding,
	}
	return LocalAssertionChallenge{ID: id, Challenge: challenge}, nil
}

func (a *memoryCredentialAuthority) VerifyAndConsumeAssertion(
	ctx context.Context,
	challengeID string,
	assertion PasskeyAssertion,
	_ ...credentialSiteOperation,
) (Principal, AssertionExpectationBinding, error) {
	a.state.mu.Lock()
	defer a.state.mu.Unlock()
	expectation, ok := a.state.expectations[challengeID]
	delete(a.state.expectations, challengeID)
	if !ok {
		return Principal{}, AssertionExpectationBinding{}, errors.New("local assertion expectation is unknown or consumed")
	}
	if err := ctx.Err(); err != nil {
		return Principal{}, expectation.binding, err
	}
	if len(expectation.challenge) == 0 || expectation.rpID != "ftw.local" || expectation.origin != "https://ftw.local" {
		return Principal{}, expectation.binding, errors.New("local assertion expectation is invalid")
	}
	if !bytes.Equal(assertion.CredentialID, testPrincipal().CredentialID) {
		return Principal{}, expectation.binding, errors.New("assertion credential is invalid")
	}
	if status := a.state.credentials[string(assertion.CredentialID)]; status != memoryCredentialActive {
		return Principal{}, expectation.binding, ErrCredentialRevoked
	}
	return testPrincipal(), expectation.binding, nil
}

func (a *memoryCredentialAuthority) RevokeCredential(
	ctx context.Context,
	credentialID []byte,
	_ ...credentialSiteOperation,
) error {
	a.state.mu.Lock()
	defer a.state.mu.Unlock()
	if err := ctx.Err(); err != nil {
		a.state.credentials[string(credentialID)] = memoryCredentialUncertain
		return err
	}
	if a.revokeErr != nil {
		a.state.credentials[string(credentialID)] = memoryCredentialUncertain
		return a.revokeErr
	}
	a.state.credentials[string(credentialID)] = memoryCredentialRevoked
	return nil
}

type blockingCredentialAuthority struct {
	CredentialAuthority
	revokeStarted   chan<- struct{}
	revokeMayFinish <-chan struct{}
}

func (a blockingCredentialAuthority) RevokeCredential(
	ctx context.Context,
	credentialID []byte,
	operation ...credentialSiteOperation,
) error {
	close(a.revokeStarted)
	select {
	case <-a.revokeMayFinish:
	case <-ctx.Done():
		return ctx.Err()
	}
	return a.CredentialAuthority.RevokeCredential(ctx, credentialID, operation...)
}

type blockingVerifyCredentialAuthority struct {
	CredentialAuthority
	verifyStarted   chan<- struct{}
	verifyMayFinish <-chan struct{}
}

func (a blockingVerifyCredentialAuthority) VerifyAndConsumeAssertion(
	ctx context.Context,
	challengeID string,
	assertion PasskeyAssertion,
	operation ...credentialSiteOperation,
) (Principal, AssertionExpectationBinding, error) {
	close(a.verifyStarted)
	select {
	case <-a.verifyMayFinish:
	case <-ctx.Done():
		return Principal{}, AssertionExpectationBinding{}, ctx.Err()
	}
	return a.CredentialAuthority.VerifyAndConsumeAssertion(
		ctx, challengeID, assertion, operation...,
	)
}

type readDispatcherFunc func(context.Context, ReadTarget, ReadRequest, Principal) error

func (f readDispatcherFunc) DispatchRead(
	ctx context.Context,
	target ReadTarget,
	request ReadRequest,
	principal Principal,
) error {
	return f(ctx, target, request, principal)
}

func successfulReadDispatcher() ReadDispatcher {
	return readDispatcherFunc(func(context.Context, ReadTarget, ReadRequest, Principal) error { return nil })
}

type pairingAuthorizerFunc func(context.Context, LocalPairingProof) error

func (f pairingAuthorizerFunc) AuthorizeLocalPairing(ctx context.Context, proof LocalPairingProof) error {
	return f(ctx, proof)
}
