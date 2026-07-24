package homelink

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

type webAuthnFixture struct {
	private      *ecdsa.PrivateKey
	credentialID []byte
	publicKey    []byte
}

type cancelAfterAssertionVerification struct {
	pinnedWebAuthnProtocolVerifier
	cancel context.CancelFunc
}

type faultCredentialStateStore struct {
	credentialStateStore
	apply func(
		context.Context,
		state.HomeLinkAssertionUpdate,
	) (state.HomeLinkCredentialRecord, error)
	ensure func(context.Context, string, []byte, int64) error
	read   func(context.Context, string, []byte) (state.HomeLinkCredentialRecord, error)
}

func (s *faultCredentialStateStore) ApplyHomeLinkAssertion(
	ctx context.Context,
	update state.HomeLinkAssertionUpdate,
) (state.HomeLinkCredentialRecord, error) {
	if s.apply != nil {
		return s.apply(ctx, update)
	}
	return s.credentialStateStore.ApplyHomeLinkAssertion(ctx, update)
}

func (s *faultCredentialStateStore) EnsureHomeLinkCredentialPolicyBlock(
	ctx context.Context,
	siteID string,
	credentialID []byte,
	nowMS int64,
) error {
	if s.ensure != nil {
		return s.ensure(ctx, siteID, credentialID, nowMS)
	}
	return s.credentialStateStore.EnsureHomeLinkCredentialPolicyBlock(
		ctx, siteID, credentialID, nowMS,
	)
}

func (s *faultCredentialStateStore) HomeLinkCredential(
	ctx context.Context,
	siteID string,
	credentialID []byte,
) (state.HomeLinkCredentialRecord, error) {
	if s.read != nil {
		return s.read(ctx, siteID, credentialID)
	}
	return s.credentialStateStore.HomeLinkCredential(ctx, siteID, credentialID)
}

func (v cancelAfterAssertionVerification) VerifyAssertion(
	assertion parsedPasskeyAssertion,
	challenge string,
	publicKey []byte,
) (verifiedPasskeyAssertion, error) {
	verified, err := v.pinnedWebAuthnProtocolVerifier.VerifyAssertion(
		assertion, challenge, publicKey,
	)
	if err == nil {
		v.cancel()
	}
	return verified, err
}

func newWebAuthnFixture(t *testing.T, credentialID []byte) webAuthnFixture {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return webAuthnFixture{
		private: private, credentialID: bytes.Clone(credentialID),
		publicKey: coseES256PublicKey(private.PublicKey),
	}
}

func newAuthorityTestStore(t *testing.T) *state.Store {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func newRegisteredAuthority(
	t *testing.T,
	siteID string,
	flags byte,
	counter uint32,
) (*PersistentCredentialAuthority, webAuthnFixture, []byte, *state.Store) {
	t.Helper()
	store := newAuthorityTestStore(t)
	pairing := NewLocalPairingManager(LocalPairingManagerOptions{})
	authority, err := NewPersistentCredentialAuthority(PersistentCredentialAuthorityOptions{
		Store: store, SiteID: siteID, PairingAuthorizer: pairing,
	})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := pairing.Create(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	begin, err := authority.BeginRegistration(context.Background(), LocalPairingProof{
		Challenge: []byte(pair.ID), Response: pair.Secret,
	}, "phone")
	if err != nil {
		t.Fatal(err)
	}
	fixture := newWebAuthnFixture(t, []byte{1, 2, 3, 4})
	response := fixture.registrationResponse(begin.Challenge, flags, counter, "none", HomeLinkOrigin)
	verifier, err := authority.FinishRegistration(
		context.Background(), begin.ID, PasskeyRegistration{ResponseJSON: response},
	)
	if err != nil {
		t.Fatalf("finish registration: %v", err)
	}
	if !bytes.Equal(verifier.PublicKey, fixture.publicKey) {
		t.Fatal("stored verifier key differs from attested key")
	}
	return authority, fixture, begin.UserHandle, store
}

func TestPersistentCredentialAuthorityRegistersAndAsserts(t *testing.T) {
	siteID := "001122334455667788"
	authority, fixture, handle, store := newRegisteredAuthority(
		t, siteID, 0x01|0x04|0x40, 0,
	)
	binding := AssertionExpectationBinding{deadline: time.Hour}
	challenge, err := authority.CreateAssertion(context.Background(), binding)
	if err != nil {
		t.Fatal(err)
	}
	if challenge.RPID != HomeLinkRPID || !challenge.UserVerificationRequired ||
		len(challenge.AllowCredentials) != 1 ||
		!bytes.Equal(challenge.AllowCredentials[0], fixture.credentialID) {
		t.Fatalf("assertion options = %+v", challenge)
	}
	response := fixture.assertionResponse(challenge.Challenge, 1, 0x01|0x04, handle, HomeLinkOrigin)
	principal, returnedBinding, err := authority.VerifyAndConsumeAssertion(
		context.Background(), challenge.ID, PasskeyAssertion{ResponseJSON: response},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(principal.CredentialID, fixture.credentialID) || principal.Label != "phone" {
		t.Fatalf("principal = %+v", principal)
	}
	if returnedBinding != binding {
		t.Fatal("authority changed manager binding")
	}
	stored, err := store.HomeLinkCredential(context.Background(), siteID, fixture.credentialID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.SignCount != 1 || !bytes.Equal(stored.UserHandle, handle) {
		t.Fatalf("stored verifier = %+v", stored)
	}
}

func TestRegistrationOptionsCannotChangeStoredUserHandle(t *testing.T) {
	store := newAuthorityTestStore(t)
	pairing := NewLocalPairingManager(LocalPairingManagerOptions{})
	authority, err := NewPersistentCredentialAuthority(PersistentCredentialAuthorityOptions{
		Store: store, SiteID: "001122334455667788", PairingAuthorizer: pairing,
	})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := pairing.Create(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	begin, err := authority.BeginRegistration(context.Background(), LocalPairingProof{
		Challenge: []byte(pair.ID), Response: pair.Secret,
	}, "phone")
	if err != nil {
		t.Fatal(err)
	}
	expectedHandle := bytes.Clone(begin.UserHandle)
	begin.UserHandle[0] ^= 0xff
	fixture := newWebAuthnFixture(t, []byte{1, 2, 3})
	raw := fixture.registrationResponse(
		begin.Challenge, 0x01|0x04|0x40, 0, "none", HomeLinkOrigin,
	)
	if _, err := authority.FinishRegistration(
		context.Background(), begin.ID, PasskeyRegistration{ResponseJSON: raw},
	); err != nil {
		t.Fatal(err)
	}
	stored, err := store.HomeLinkCredential(
		context.Background(), "001122334455667788", fixture.credentialID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored.UserHandle, expectedHandle) {
		t.Fatal("caller-mutated registration option changed the stored user handle")
	}
}

func TestRegistrationRequiresConsumedLocalPairingProof(t *testing.T) {
	store := newAuthorityTestStore(t)
	withoutPairing, err := NewPersistentCredentialAuthority(PersistentCredentialAuthorityOptions{
		Store: store, SiteID: "001122334455667788",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := withoutPairing.BeginRegistration(
		context.Background(), LocalPairingProof{}, "phone",
	); !errors.Is(err, ErrRegistrationDenied) {
		t.Fatalf("registration without local pairing = %v", err)
	}

	pairing := NewLocalPairingManager(LocalPairingManagerOptions{})
	authority, err := NewPersistentCredentialAuthority(PersistentCredentialAuthorityOptions{
		Store: store, SiteID: "001122334455667788", PairingAuthorizer: pairing,
	})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := pairing.Create(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	wrong := bytes.Clone(pair.Secret)
	wrong[0] ^= 0xff
	proof := LocalPairingProof{Challenge: []byte(pair.ID), Response: wrong}
	if _, err := authority.BeginRegistration(
		context.Background(), proof, "phone",
	); !errors.Is(err, ErrRegistrationDenied) {
		t.Fatalf("wrong local pairing = %v", err)
	}
	proof.Response = pair.Secret
	if _, err := authority.BeginRegistration(
		context.Background(), proof, "phone",
	); !errors.Is(err, ErrRegistrationDenied) {
		t.Fatalf("pairing proof reused after failure = %v", err)
	}
}

func TestRegistrationExpectationIsOneUseExpiresAndDoesNotSurviveRestart(t *testing.T) {
	var monotonic time.Duration
	store := newAuthorityTestStore(t)
	pairing := pairingAuthorizerFunc(func(context.Context, LocalPairingProof) error {
		return nil
	})
	newAuthority := func(t *testing.T) *PersistentCredentialAuthority {
		t.Helper()
		authority, err := NewPersistentCredentialAuthority(PersistentCredentialAuthorityOptions{
			Store: store, SiteID: "001122334455667788", PairingAuthorizer: pairing,
			MonotonicNow: func() time.Duration { return monotonic },
		})
		if err != nil {
			t.Fatal(err)
		}
		return authority
	}
	fixture := newWebAuthnFixture(t, []byte{1, 2, 3})

	t.Run("failure consumes", func(t *testing.T) {
		authority := newAuthority(t)
		begin, err := authority.BeginRegistration(context.Background(), LocalPairingProof{}, "phone")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := authority.FinishRegistration(
			context.Background(), begin.ID, PasskeyRegistration{ResponseJSON: []byte(`{}`)},
		); err == nil {
			t.Fatal("invalid registration unexpectedly succeeded")
		}
		valid := fixture.registrationResponse(
			begin.Challenge, 0x01|0x04|0x40, 0, "none", HomeLinkOrigin,
		)
		if _, err := authority.FinishRegistration(
			context.Background(), begin.ID, PasskeyRegistration{ResponseJSON: valid},
		); !errors.Is(err, ErrWebAuthnExpectation) {
			t.Fatalf("registration expectation reopened after failure: %v", err)
		}
	})

	t.Run("exact deadline expires", func(t *testing.T) {
		monotonic = 0
		authority := newAuthority(t)
		begin, err := authority.BeginRegistration(context.Background(), LocalPairingProof{}, "phone")
		if err != nil {
			t.Fatal(err)
		}
		monotonic = AssertionExpectationMaxAge
		valid := fixture.registrationResponse(
			begin.Challenge, 0x01|0x04|0x40, 0, "none", HomeLinkOrigin,
		)
		if _, err := authority.FinishRegistration(
			context.Background(), begin.ID, PasskeyRegistration{ResponseJSON: valid},
		); !errors.Is(err, ErrAssertionExpired) {
			t.Fatalf("registration at exact deadline = %v", err)
		}
	})

	t.Run("restart denies", func(t *testing.T) {
		monotonic = 0
		authority := newAuthority(t)
		begin, err := authority.BeginRegistration(context.Background(), LocalPairingProof{}, "phone")
		if err != nil {
			t.Fatal(err)
		}
		restarted := newAuthority(t)
		valid := fixture.registrationResponse(
			begin.Challenge, 0x01|0x04|0x40, 0, "none", HomeLinkOrigin,
		)
		if _, err := restarted.FinishRegistration(
			context.Background(), begin.ID, PasskeyRegistration{ResponseJSON: valid},
		); !errors.Is(err, ErrWebAuthnExpectation) {
			t.Fatalf("registration expectation survived restart: %v", err)
		}
	})
}

func TestAssertionUserHandlePolicyAndSiteLocalAllowList(t *testing.T) {
	siteID := "001122334455667788"
	authority, fixture, handle, _ := newRegisteredAuthority(
		t, siteID, 0x01|0x04|0x40, 0,
	)
	for _, test := range []struct {
		name    string
		handle  []byte
		fixture webAuthnFixture
		wantErr error
	}{
		{"empty handle", nil, fixture, nil},
		{"expected handle", handle, fixture, nil},
		{"wrong handle", bytes.Repeat([]byte{9}, len(handle)), fixture, ErrWebAuthnVerification},
		{"another credential", handle, newWebAuthnFixture(t, []byte{9, 9, 9}), ErrCredentialUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			challenge, err := authority.CreateAssertion(
				context.Background(), AssertionExpectationBinding{deadline: time.Hour},
			)
			if err != nil {
				t.Fatal(err)
			}
			response := test.fixture.assertionResponse(
				challenge.Challenge, 0, 0x01|0x04, test.handle, HomeLinkOrigin,
			)
			_, _, err = authority.VerifyAndConsumeAssertion(
				context.Background(), challenge.ID, PasskeyAssertion{ResponseJSON: response},
			)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestAssertionAllowListCannotBeExpandedByCaller(t *testing.T) {
	authority, _, handle, _ := newRegisteredAuthority(
		t, "001122334455667788", 0x01|0x04|0x40, 0,
	)
	challenge, err := authority.CreateAssertion(
		context.Background(), AssertionExpectationBinding{deadline: time.Hour},
	)
	if err != nil {
		t.Fatal(err)
	}
	attacker := newWebAuthnFixture(t, []byte{9, 9, 9})
	challenge.AllowCredentials = append(challenge.AllowCredentials, attacker.credentialID)
	raw := attacker.assertionResponse(
		challenge.Challenge, 0, 0x01|0x04, handle, HomeLinkOrigin,
	)
	if _, _, err := authority.VerifyAndConsumeAssertion(
		context.Background(), challenge.ID, PasskeyAssertion{ResponseJSON: raw},
	); !errors.Is(err, ErrCredentialUnknown) {
		t.Fatalf("caller-expanded allow list = %v", err)
	}
}

func TestAssertionRejectsCredentialFromAnotherLocalSite(t *testing.T) {
	store := newAuthorityTestStore(t)
	handleOne := bytes.Repeat([]byte{1}, webAuthnUserHandleBytes)
	handleTwo := bytes.Repeat([]byte{2}, webAuthnUserHandleBytes)
	first := newWebAuthnFixture(t, []byte{1})
	second := newWebAuthnFixture(t, []byte{2})
	for _, record := range []state.HomeLinkCredentialRecord{
		{
			SiteID: "001122334455667788", CredentialID: first.credentialID,
			PublicKey: first.publicKey, Label: "first", UserHandle: handleOne,
			Status: state.HomeLinkCredentialActive, Revision: 1, CreatedAtMS: 1, UpdatedAtMS: 1,
		},
		{
			SiteID: "112233445566778899", CredentialID: second.credentialID,
			PublicKey: second.publicKey, Label: "second", UserHandle: handleTwo,
			Status: state.HomeLinkCredentialActive, Revision: 1, CreatedAtMS: 1, UpdatedAtMS: 1,
		},
	} {
		if err := store.RegisterHomeLinkCredential(context.Background(), record); err != nil {
			t.Fatal(err)
		}
	}
	authority, err := NewPersistentCredentialAuthority(PersistentCredentialAuthorityOptions{
		Store: store, SiteID: "001122334455667788",
	})
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := authority.CreateAssertion(
		context.Background(), AssertionExpectationBinding{deadline: time.Hour},
	)
	if err != nil {
		t.Fatal(err)
	}
	raw := second.assertionResponse(
		challenge.Challenge, 0, 0x01|0x04, handleTwo, HomeLinkOrigin,
	)
	if _, _, err := authority.VerifyAndConsumeAssertion(
		context.Background(), challenge.ID, PasskeyAssertion{ResponseJSON: raw},
	); !errors.Is(err, ErrCredentialUnknown) {
		t.Fatalf("other-site credential = %v", err)
	}
}

func TestAssertionConsumesExpectationBeforeEveryResultAndRestart(t *testing.T) {
	siteID := "001122334455667788"
	authority, fixture, handle, store := newRegisteredAuthority(
		t, siteID, 0x01|0x04|0x40, 0,
	)
	challenge, err := authority.CreateAssertion(
		context.Background(), AssertionExpectationBinding{deadline: time.Hour},
	)
	if err != nil {
		t.Fatal(err)
	}
	bad := fixture.assertionResponse(challenge.Challenge, 0, 0x01|0x04, handle, "https://wrong.example")
	if _, _, err := authority.VerifyAndConsumeAssertion(
		context.Background(), challenge.ID, PasskeyAssertion{ResponseJSON: bad},
	); !errors.Is(err, ErrWebAuthnVerification) {
		t.Fatalf("wrong origin = %v", err)
	}
	good := fixture.assertionResponse(challenge.Challenge, 0, 0x01|0x04, handle, HomeLinkOrigin)
	if _, _, err := authority.VerifyAndConsumeAssertion(
		context.Background(), challenge.ID, PasskeyAssertion{ResponseJSON: good},
	); !errors.Is(err, ErrWebAuthnExpectation) {
		t.Fatalf("replay after failure = %v", err)
	}

	restartChallenge, err := authority.CreateAssertion(
		context.Background(), AssertionExpectationBinding{deadline: time.Hour},
	)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewPersistentCredentialAuthority(PersistentCredentialAuthorityOptions{
		Store: store, SiteID: siteID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := restarted.VerifyAndConsumeAssertion(
		context.Background(), restartChallenge.ID,
		PasskeyAssertion{ResponseJSON: fixture.assertionResponse(
			restartChallenge.Challenge, 0, 0x01|0x04, handle, HomeLinkOrigin,
		)},
	); !errors.Is(err, ErrWebAuthnExpectation) {
		t.Fatalf("expectation survived restart = %v", err)
	}
}

func TestAssertionRejectsFixedPolicyAndIdentityMismatches(t *testing.T) {
	tests := []struct {
		name   string
		change func([]byte, webAuthnFixture, []byte) []byte
	}{
		{"wrong challenge", func(_ []byte, fixture webAuthnFixture, handle []byte) []byte {
			return fixture.assertionResponse(
				[]byte("wrong challenge"), 0, 0x01|0x04, handle, HomeLinkOrigin,
			)
		}},
		{"wrong ceremony type", func(challenge []byte, fixture webAuthnFixture, handle []byte) []byte {
			return fixture.assertionResponseWithPolicy(
				challenge, 0, 0x01|0x04, handle, HomeLinkOrigin, HomeLinkRPID,
				"webauthn.create", false, "",
			)
		}},
		{"wrong credential type", func(challenge []byte, fixture webAuthnFixture, handle []byte) []byte {
			raw := fixture.assertionResponse(challenge, 0, 0x01|0x04, handle, HomeLinkOrigin)
			return changeCredentialJSON(t, raw, func(object map[string]any) {
				object["type"] = "password"
			})
		}},
		{"wrong scheme", func(challenge []byte, fixture webAuthnFixture, handle []byte) []byte {
			return fixture.assertionResponse(challenge, 0, 0x01|0x04, handle, "http://home.sourceful.energy")
		}},
		{"wrong host", func(challenge []byte, fixture webAuthnFixture, handle []byte) []byte {
			return fixture.assertionResponse(challenge, 0, 0x01|0x04, handle, "https://other.sourceful.energy")
		}},
		{"wrong port", func(challenge []byte, fixture webAuthnFixture, handle []byte) []byte {
			return fixture.assertionResponse(challenge, 0, 0x01|0x04, handle, "https://home.sourceful.energy:8443")
		}},
		{"wrong RP ID hash", func(challenge []byte, fixture webAuthnFixture, handle []byte) []byte {
			return fixture.assertionResponseForRPID(
				challenge, 0, 0x01|0x04, handle, HomeLinkOrigin, "other.sourceful.energy",
			)
		}},
		{"missing UV", func(challenge []byte, fixture webAuthnFixture, handle []byte) []byte {
			return fixture.assertionResponse(challenge, 0, 0x01, handle, HomeLinkOrigin)
		}},
		{"missing UP", func(challenge []byte, fixture webAuthnFixture, handle []byte) []byte {
			return fixture.assertionResponse(challenge, 0, 0x04, handle, HomeLinkOrigin)
		}},
		{"cross origin", func(challenge []byte, fixture webAuthnFixture, handle []byte) []byte {
			return fixture.assertionResponseWithPolicy(
				challenge, 0, 0x01|0x04, handle, HomeLinkOrigin, HomeLinkRPID,
				"webauthn.get", true, "https://embed.example",
			)
		}},
		{"top origin", func(challenge []byte, fixture webAuthnFixture, handle []byte) []byte {
			return fixture.assertionResponseWithPolicy(
				challenge, 0, 0x01|0x04, handle, HomeLinkOrigin, HomeLinkRPID,
				"webauthn.get", false, "https://embed.example",
			)
		}},
		{"client extension output", func(challenge []byte, fixture webAuthnFixture, handle []byte) []byte {
			raw := fixture.assertionResponse(challenge, 0, 0x01|0x04, handle, HomeLinkOrigin)
			return changeCredentialJSON(t, raw, func(object map[string]any) {
				object["clientExtensionResults"] = map[string]any{"appid": false}
			})
		}},
		{"id rawId mismatch", func(challenge []byte, fixture webAuthnFixture, handle []byte) []byte {
			raw := fixture.assertionResponse(challenge, 0, 0x01|0x04, handle, HomeLinkOrigin)
			return changeCredentialJSON(t, raw, func(object map[string]any) {
				object["id"] = base64.RawURLEncoding.EncodeToString([]byte{8, 8, 8})
			})
		}},
		{"bad signature", func(challenge []byte, fixture webAuthnFixture, handle []byte) []byte {
			raw := fixture.assertionResponse(challenge, 0, 0x01|0x04, handle, HomeLinkOrigin)
			return changeCredentialJSON(t, raw, func(object map[string]any) {
				response := object["response"].(map[string]any)
				response["signature"] = base64.RawURLEncoding.EncodeToString([]byte{1, 2, 3})
			})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authority, fixture, handle, _ := newRegisteredAuthority(
				t, "001122334455667788", 0x01|0x04|0x40, 0,
			)
			challenge, err := authority.CreateAssertion(
				context.Background(), AssertionExpectationBinding{deadline: time.Hour},
			)
			if err != nil {
				t.Fatal(err)
			}
			raw := test.change(challenge.Challenge, fixture, handle)
			if _, _, err := authority.VerifyAndConsumeAssertion(
				context.Background(), challenge.ID, PasskeyAssertion{ResponseJSON: raw},
			); err == nil {
				t.Fatal("assertion unexpectedly succeeded")
			}
		})
	}
}

func TestAssertionUsesOnlyStoredCredentialKey(t *testing.T) {
	authority, fixture, handle, _ := newRegisteredAuthority(
		t, "001122334455667788", 0x01|0x04|0x40, 0,
	)
	challenge, err := authority.CreateAssertion(
		context.Background(), AssertionExpectationBinding{deadline: time.Hour},
	)
	if err != nil {
		t.Fatal(err)
	}
	attacker := newWebAuthnFixture(t, fixture.credentialID)
	raw := attacker.assertionResponse(challenge.Challenge, 0, 0x01|0x04, handle, HomeLinkOrigin)
	if _, _, err := authority.VerifyAndConsumeAssertion(
		context.Background(), challenge.ID, PasskeyAssertion{ResponseJSON: raw},
	); !errors.Is(err, ErrWebAuthnVerification) {
		t.Fatalf("attacker-selected key = %v", err)
	}
}

func TestPersistentAuthorityRejectsAmbiguousLegacyAssertionFields(t *testing.T) {
	authority, fixture, handle, _ := newRegisteredAuthority(
		t, "001122334455667788", 0x01|0x04|0x40, 0,
	)
	challenge, err := authority.CreateAssertion(
		context.Background(), AssertionExpectationBinding{deadline: time.Hour},
	)
	if err != nil {
		t.Fatal(err)
	}
	raw := fixture.assertionResponse(challenge.Challenge, 0, 0x01|0x04, handle, HomeLinkOrigin)
	if _, _, err := authority.VerifyAndConsumeAssertion(
		context.Background(), challenge.ID, PasskeyAssertion{
			CredentialID: fixture.credentialID, ResponseJSON: raw,
		},
	); !errors.Is(err, ErrWebAuthnInput) {
		t.Fatalf("ambiguous assertion = %v", err)
	}
	if _, _, err := authority.VerifyAndConsumeAssertion(
		context.Background(), challenge.ID, PasskeyAssertion{ResponseJSON: raw},
	); !errors.Is(err, ErrWebAuthnExpectation) {
		t.Fatalf("ambiguous assertion did not consume expectation = %v", err)
	}
}

func TestCredentialRevokeDeniesPendingAndFutureAssertions(t *testing.T) {
	authority, fixture, handle, store := newRegisteredAuthority(
		t, "001122334455667788", 0x01|0x04|0x40, 0,
	)
	challenge, err := authority.CreateAssertion(
		context.Background(), AssertionExpectationBinding{deadline: time.Hour},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.RevokeCredential(context.Background(), fixture.credentialID); err != nil {
		t.Fatal(err)
	}
	raw := fixture.assertionResponse(challenge.Challenge, 0, 0x01|0x04, handle, HomeLinkOrigin)
	if _, _, err := authority.VerifyAndConsumeAssertion(
		context.Background(), challenge.ID, PasskeyAssertion{ResponseJSON: raw},
	); !errors.Is(err, ErrCredentialUncertain) {
		t.Fatalf("pending assertion after revoke = %v", err)
	}
	if _, err := authority.CreateAssertion(
		context.Background(), AssertionExpectationBinding{deadline: time.Hour},
	); !errors.Is(err, ErrCredentialUnknown) {
		t.Fatalf("future assertion after revoke = %v", err)
	}
	stored, err := store.HomeLinkCredential(
		context.Background(), "001122334455667788", fixture.credentialID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != state.HomeLinkCredentialRevoked {
		t.Fatalf("status = %q", stored.Status)
	}
}

func TestAssertionExpectationUsesLocalMonotonicDeadline(t *testing.T) {
	var monotonic time.Duration
	store := newAuthorityTestStore(t)
	record := state.HomeLinkCredentialRecord{
		SiteID: "001122334455667788", CredentialID: []byte{1}, PublicKey: []byte{1},
		Label: "phone", UserHandle: bytes.Repeat([]byte{1}, webAuthnUserHandleBytes),
		Status: state.HomeLinkCredentialActive, Revision: 1, CreatedAtMS: 1, UpdatedAtMS: 1,
	}
	if err := store.RegisterHomeLinkCredential(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	authority, err := NewPersistentCredentialAuthority(PersistentCredentialAuthorityOptions{
		Store: store, SiteID: record.SiteID,
		MonotonicNow: func() time.Duration { return monotonic },
	})
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := authority.CreateAssertion(
		context.Background(), AssertionExpectationBinding{deadline: time.Hour},
	)
	if err != nil {
		t.Fatal(err)
	}
	monotonic = AssertionExpectationMaxAge
	if _, _, err := authority.VerifyAndConsumeAssertion(
		context.Background(), challenge.ID, PasskeyAssertion{ResponseJSON: []byte(`{}`)},
	); !errors.Is(err, ErrAssertionExpired) {
		t.Fatalf("exact authority deadline = %v", err)
	}
}

func TestCredentialAuthorityMonotonicRegressionAndOverflowFailClosed(t *testing.T) {
	newWithClock := func(t *testing.T, monotonic *time.Duration) *PersistentCredentialAuthority {
		t.Helper()
		store := newAuthorityTestStore(t)
		record := state.HomeLinkCredentialRecord{
			SiteID: "001122334455667788", CredentialID: []byte{1}, PublicKey: []byte{1},
			Label: "phone", UserHandle: bytes.Repeat([]byte{1}, webAuthnUserHandleBytes),
			Status: state.HomeLinkCredentialActive, Revision: 1, CreatedAtMS: 1, UpdatedAtMS: 1,
		}
		if err := store.RegisterHomeLinkCredential(context.Background(), record); err != nil {
			t.Fatal(err)
		}
		authority, err := NewPersistentCredentialAuthority(PersistentCredentialAuthorityOptions{
			Store: store, SiteID: record.SiteID,
			MonotonicNow: func() time.Duration { return *monotonic },
		})
		if err != nil {
			t.Fatal(err)
		}
		return authority
	}
	t.Run("regression poisons authority", func(t *testing.T) {
		monotonic := 100 * time.Second
		authority := newWithClock(t, &monotonic)
		challenge, err := authority.CreateAssertion(
			context.Background(), AssertionExpectationBinding{deadline: time.Hour},
		)
		if err != nil {
			t.Fatal(err)
		}
		monotonic = 50 * time.Second
		if _, _, err := authority.VerifyAndConsumeAssertion(
			context.Background(), challenge.ID, PasskeyAssertion{ResponseJSON: []byte(`{}`)},
		); !errors.Is(err, ErrMonotonicClock) {
			t.Fatalf("clock regression = %v", err)
		}
		monotonic = 101 * time.Second
		if _, err := authority.CreateAssertion(
			context.Background(), AssertionExpectationBinding{deadline: time.Hour},
		); !errors.Is(err, ErrMonotonicClock) {
			t.Fatalf("poisoned clock reopened = %v", err)
		}
	})
	t.Run("deadline overflow poisons authority", func(t *testing.T) {
		monotonic := maxMonotonicTime - AssertionExpectationMaxAge + 1
		authority := newWithClock(t, &monotonic)
		if _, err := authority.CreateAssertion(
			context.Background(), AssertionExpectationBinding{deadline: time.Hour},
		); !errors.Is(err, ErrMonotonicClock) {
			t.Fatalf("deadline overflow = %v", err)
		}
	})
}

func TestAssertionPolicyMakesCredentialUncertain(t *testing.T) {
	tests := []struct {
		name         string
		registerFlag byte
		register     uint32
		assertFlag   byte
		assert       uint32
	}{
		{"positive to zero", 0x01 | 0x04 | 0x40, 2, 0x01 | 0x04, 0},
		{"equal positive", 0x01 | 0x04 | 0x40, 2, 0x01 | 0x04, 2},
		{"regression", 0x01 | 0x04 | 0x40, 2, 0x01 | 0x04, 1},
		{"backup eligibility changed", 0x01 | 0x04 | 0x40, 0, 0x01 | 0x04 | 0x08, 0},
		{"backup state without eligible", 0x01 | 0x04 | 0x40, 0, 0x01 | 0x04 | 0x10, 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			siteID := "001122334455667788"
			authority, fixture, handle, store := newRegisteredAuthority(
				t, siteID, test.registerFlag, test.register,
			)
			challenge, err := authority.CreateAssertion(
				context.Background(), AssertionExpectationBinding{deadline: time.Hour},
			)
			if err != nil {
				t.Fatal(err)
			}
			response := fixture.assertionResponse(
				challenge.Challenge, test.assert, test.assertFlag, handle, HomeLinkOrigin,
			)
			if _, _, err := authority.VerifyAndConsumeAssertion(
				context.Background(), challenge.ID, PasskeyAssertion{ResponseJSON: response},
			); !errors.Is(err, ErrCredentialUncertain) {
				t.Fatalf("assertion = %v", err)
			}
			stored, err := store.HomeLinkCredential(context.Background(), siteID, fixture.credentialID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Status != state.HomeLinkCredentialUncertain {
				t.Fatalf("stored status = %q", stored.Status)
			}
		})
	}
}

func TestAssertionAcceptsAllowedCounterAndBackupTransitions(t *testing.T) {
	tests := []struct {
		name         string
		registerFlag byte
		register     uint32
		assertFlag   byte
		assert       uint32
	}{
		{"zero to zero", 0x01 | 0x04 | 0x40, 0, 0x01 | 0x04, 0},
		{"zero to positive", 0x01 | 0x04 | 0x40, 0, 0x01 | 0x04, 1},
		{"positive increase", 0x01 | 0x04 | 0x40, 3, 0x01 | 0x04, 4},
		{"backup eligible and backed up", 0x01 | 0x04 | 0x08 | 0x40, 0, 0x01 | 0x04 | 0x08 | 0x10, 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authority, fixture, handle, store := newRegisteredAuthority(
				t, "001122334455667788", test.registerFlag, test.register,
			)
			challenge, err := authority.CreateAssertion(
				context.Background(), AssertionExpectationBinding{deadline: time.Hour},
			)
			if err != nil {
				t.Fatal(err)
			}
			raw := fixture.assertionResponse(
				challenge.Challenge, test.assert, test.assertFlag, handle, HomeLinkOrigin,
			)
			if _, _, err := authority.VerifyAndConsumeAssertion(
				context.Background(), challenge.ID, PasskeyAssertion{ResponseJSON: raw},
			); err != nil {
				t.Fatal(err)
			}
			stored, err := store.HomeLinkCredential(
				context.Background(), "001122334455667788", fixture.credentialID,
			)
			if err != nil {
				t.Fatal(err)
			}
			if stored.SignCount != test.assert ||
				stored.BackupState != (test.assertFlag&0x10 != 0) {
				t.Fatalf("stored state = %+v", stored)
			}
		})
	}
}

func TestCanceledAssertionAfterVerificationPersistsStateWithoutPrincipal(t *testing.T) {
	authority, fixture, handle, store := newRegisteredAuthority(
		t, "001122334455667788", 0x01|0x04|0x40, 0,
	)
	challenge, err := authority.CreateAssertion(
		context.Background(), AssertionExpectationBinding{deadline: time.Hour},
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	authority.verifier = cancelAfterAssertionVerification{cancel: cancel}
	raw := fixture.assertionResponse(
		challenge.Challenge, 1, 0x01|0x04, handle, HomeLinkOrigin,
	)
	principal, _, err := authority.VerifyAndConsumeAssertion(
		ctx, challenge.ID, PasskeyAssertion{ResponseJSON: raw},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled assertion = %v", err)
	}
	if len(principal.CredentialID) != 0 || principal.Label != "" {
		t.Fatalf("canceled assertion returned principal: %+v", principal)
	}
	stored, err := store.HomeLinkCredential(
		context.Background(), "001122334455667788", fixture.credentialID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if stored.SignCount != 1 {
		t.Fatalf("counter after post-verification cancel = %d, want 1", stored.SignCount)
	}
	if _, _, err := authority.VerifyAndConsumeAssertion(
		context.Background(), challenge.ID, PasskeyAssertion{ResponseJSON: raw},
	); !errors.Is(err, ErrWebAuthnExpectation) {
		t.Fatalf("canceled assertion expectation reopened = %v", err)
	}
}

func TestCanceledDuringAssertionStateWritePersistsStateWithoutPrincipal(t *testing.T) {
	authority, fixture, handle, store := newRegisteredAuthority(
		t, "001122334455667788", 0x01|0x04|0x40, 0,
	)
	challenge, err := authority.CreateAssertion(
		context.Background(), AssertionExpectationBinding{deadline: time.Hour},
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	authority.store = &faultCredentialStateStore{
		credentialStateStore: store,
		apply: func(
			stateCtx context.Context,
			update state.HomeLinkAssertionUpdate,
		) (state.HomeLinkCredentialRecord, error) {
			cancel()
			if err := stateCtx.Err(); err != nil {
				t.Fatalf("detached state context canceled with request: %v", err)
			}
			return store.ApplyHomeLinkAssertion(stateCtx, update)
		},
	}
	raw := fixture.assertionResponse(
		challenge.Challenge, 1, 0x01|0x04, handle, HomeLinkOrigin,
	)
	principal, _, err := authority.VerifyAndConsumeAssertion(
		ctx, challenge.ID, PasskeyAssertion{ResponseJSON: raw},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("assertion canceled during state write = %v", err)
	}
	if len(principal.CredentialID) != 0 || principal.Label != "" {
		t.Fatalf("state-write cancellation returned principal: %+v", principal)
	}
	stored, err := store.HomeLinkCredential(
		context.Background(), "001122334455667788", fixture.credentialID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if stored.SignCount != 1 {
		t.Fatalf("counter after state-write cancellation = %d, want 1", stored.SignCount)
	}
	if _, _, err := authority.VerifyAndConsumeAssertion(
		context.Background(), challenge.ID, PasskeyAssertion{ResponseJSON: raw},
	); !errors.Is(err, ErrWebAuthnExpectation) {
		t.Fatalf("state-write cancellation expectation reopened = %v", err)
	}
}

func TestCanceledPolicyAssertionPersistsBlockWithoutPrincipal(t *testing.T) {
	authority, fixture, handle, store := newRegisteredAuthority(
		t, "001122334455667788", 0x01|0x04|0x40, 2,
	)
	challenge, err := authority.CreateAssertion(
		context.Background(), AssertionExpectationBinding{deadline: time.Hour},
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	authority.verifier = cancelAfterAssertionVerification{cancel: cancel}
	raw := fixture.assertionResponse(
		challenge.Challenge, 2, 0x01|0x04, handle, HomeLinkOrigin,
	)
	principal, _, err := authority.VerifyAndConsumeAssertion(
		ctx, challenge.ID, PasskeyAssertion{ResponseJSON: raw},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled policy assertion = %v", err)
	}
	if len(principal.CredentialID) != 0 || principal.Label != "" {
		t.Fatalf("canceled policy assertion returned principal: %+v", principal)
	}
	stored, err := store.HomeLinkCredential(
		context.Background(), "001122334455667788", fixture.credentialID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != state.HomeLinkCredentialUncertain {
		t.Fatalf("status after canceled policy assertion = %q", stored.Status)
	}
	if _, err := authority.CreateAssertion(
		context.Background(), AssertionExpectationBinding{deadline: time.Hour},
	); !errors.Is(err, ErrCredentialUnknown) {
		t.Fatalf("canceled policy credential reopened = %v", err)
	}
	if _, _, err := authority.VerifyAndConsumeAssertion(
		context.Background(), challenge.ID, PasskeyAssertion{ResponseJSON: raw},
	); !errors.Is(err, ErrWebAuthnExpectation) {
		t.Fatalf("canceled policy expectation reopened = %v", err)
	}
}

func TestAmbiguousAssertionStoreErrorPersistsConservativeBlock(t *testing.T) {
	authority, fixture, handle, store := newRegisteredAuthority(
		t, "001122334455667788", 0x01|0x04|0x40, 0,
	)
	challenge, err := authority.CreateAssertion(
		context.Background(), AssertionExpectationBinding{deadline: time.Hour},
	)
	if err != nil {
		t.Fatal(err)
	}
	authority.store = &faultCredentialStateStore{
		credentialStateStore: store,
		apply: func(
			context.Context,
			state.HomeLinkAssertionUpdate,
		) (state.HomeLinkCredentialRecord, error) {
			return state.HomeLinkCredentialRecord{}, errors.New("ambiguous assertion commit")
		},
	}
	raw := fixture.assertionResponse(
		challenge.Challenge, 1, 0x01|0x04, handle, HomeLinkOrigin,
	)
	if principal, _, err := authority.VerifyAndConsumeAssertion(
		context.Background(), challenge.ID, PasskeyAssertion{ResponseJSON: raw},
	); err == nil || len(principal.CredentialID) != 0 {
		t.Fatalf("ambiguous assertion returned principal=%+v err=%v", principal, err)
	}
	stored, err := store.HomeLinkCredential(
		context.Background(), "001122334455667788", fixture.credentialID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != state.HomeLinkCredentialUncertain {
		t.Fatalf("status after ambiguous assertion commit = %q", stored.Status)
	}
	restarted, err := NewPersistentCredentialAuthority(PersistentCredentialAuthorityOptions{
		Store: store, SiteID: "001122334455667788",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.CreateAssertion(
		context.Background(), AssertionExpectationBinding{deadline: time.Hour},
	); !errors.Is(err, ErrCredentialUnknown) {
		t.Fatalf("durable conservative block reopened after restart = %v", err)
	}
	if _, _, err := authority.VerifyAndConsumeAssertion(
		context.Background(), challenge.ID, PasskeyAssertion{ResponseJSON: raw},
	); !errors.Is(err, ErrWebAuthnExpectation) {
		t.Fatalf("ambiguous assertion expectation reopened = %v", err)
	}
}

func TestAssertionStoreOutageBlocksCurrentAuthorityInMemory(t *testing.T) {
	authority, fixture, handle, store := newRegisteredAuthority(
		t, "001122334455667788", 0x01|0x04|0x40, 0,
	)
	challenge, err := authority.CreateAssertion(
		context.Background(), AssertionExpectationBinding{deadline: time.Hour},
	)
	if err != nil {
		t.Fatal(err)
	}
	outage := errors.New("credential store unavailable")
	authority.store = &faultCredentialStateStore{
		credentialStateStore: store,
		apply: func(
			context.Context,
			state.HomeLinkAssertionUpdate,
		) (state.HomeLinkCredentialRecord, error) {
			return state.HomeLinkCredentialRecord{}, outage
		},
		ensure: func(context.Context, string, []byte, int64) error {
			return outage
		},
		read: func(
			context.Context,
			string,
			[]byte,
		) (state.HomeLinkCredentialRecord, error) {
			return state.HomeLinkCredentialRecord{}, outage
		},
	}
	raw := fixture.assertionResponse(
		challenge.Challenge, 1, 0x01|0x04, handle, HomeLinkOrigin,
	)
	if principal, _, err := authority.VerifyAndConsumeAssertion(
		context.Background(), challenge.ID, PasskeyAssertion{ResponseJSON: raw},
	); err == nil || len(principal.CredentialID) != 0 {
		t.Fatalf("store outage returned principal=%+v err=%v", principal, err)
	}
	authority.store = store
	if _, err := authority.CreateAssertion(
		context.Background(), AssertionExpectationBinding{deadline: time.Hour},
	); !errors.Is(err, ErrCredentialUnknown) {
		t.Fatalf("in-memory block reopened after store recovery = %v", err)
	}
	if _, _, err := authority.VerifyAndConsumeAssertion(
		context.Background(), challenge.ID, PasskeyAssertion{ResponseJSON: raw},
	); !errors.Is(err, ErrWebAuthnExpectation) {
		t.Fatalf("store-outage expectation reopened = %v", err)
	}
}

func TestWebAuthnRegistrationPolicyAndInputBounds(t *testing.T) {
	siteID := "001122334455667788"
	store := newAuthorityTestStore(t)
	pairing := NewLocalPairingManager(LocalPairingManagerOptions{})
	authority, err := NewPersistentCredentialAuthority(PersistentCredentialAuthorityOptions{
		Store: store, SiteID: siteID, PairingAuthorizer: pairing,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture := newWebAuthnFixture(t, []byte{4, 3, 2, 1})
	for _, test := range []struct {
		name   string
		change func([]byte, []byte) []byte
	}{
		{"wrong challenge", func(raw, _ []byte) []byte {
			return changeClientData(t, raw, func(data map[string]any) {
				data["challenge"] = base64.RawURLEncoding.EncodeToString([]byte("wrong"))
			})
		}},
		{"wrong ceremony type", func(raw, _ []byte) []byte {
			return changeClientData(t, raw, func(data map[string]any) {
				data["type"] = "webauthn.get"
			})
		}},
		{"wrong credential type", func(raw, _ []byte) []byte {
			return changeCredentialJSON(t, raw, func(object map[string]any) {
				object["type"] = "password"
			})
		}},
		{"wrong scheme", func(_ []byte, challenge []byte) []byte {
			return fixture.registrationResponse(
				challenge,
				0x01|0x04|0x40, 0, "none", "http://home.sourceful.energy",
			)
		}},
		{"wrong host", func(_ []byte, challenge []byte) []byte {
			return fixture.registrationResponse(
				challenge,
				0x01|0x04|0x40, 0, "none", "https://other.sourceful.energy",
			)
		}},
		{"wrong port", func(_ []byte, challenge []byte) []byte {
			return fixture.registrationResponse(
				challenge,
				0x01|0x04|0x40, 0, "none", "https://home.sourceful.energy:8443",
			)
		}},
		{"cross origin", func(raw, _ []byte) []byte {
			return changeClientData(t, raw, func(data map[string]any) {
				data["crossOrigin"] = true
				data["topOrigin"] = "https://embed.example"
			})
		}},
		{"top origin", func(raw, _ []byte) []byte {
			return changeClientData(t, raw, func(data map[string]any) {
				data["topOrigin"] = "https://embed.example"
			})
		}},
		{"wrong RP ID hash", func(_ []byte, challenge []byte) []byte {
			return fixture.registrationResponseForRPID(
				challenge, 0x01|0x04|0x40, 0, "none", HomeLinkOrigin,
				"other.sourceful.energy",
			)
		}},
		{"missing UV", func(_ []byte, challenge []byte) []byte {
			return fixture.registrationResponse(
				challenge,
				0x01|0x40, 0, "none", HomeLinkOrigin,
			)
		}},
		{"missing UP", func(_ []byte, challenge []byte) []byte {
			return fixture.registrationResponse(
				challenge,
				0x04|0x40, 0, "none", HomeLinkOrigin,
			)
		}},
		{"backup state without eligible", func(_ []byte, challenge []byte) []byte {
			return fixture.registrationResponse(
				challenge,
				0x01|0x04|0x10|0x40, 0, "none", HomeLinkOrigin,
			)
		}},
		{"other attestation", func(_ []byte, challenge []byte) []byte {
			return fixture.registrationResponse(
				challenge,
				0x01|0x04|0x40, 0, "packed", HomeLinkOrigin,
			)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			pair, err := pairing.Create(time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			begin, err := authority.BeginRegistration(context.Background(), LocalPairingProof{
				Challenge: []byte(pair.ID), Response: pair.Secret,
			}, "phone")
			if err != nil {
				t.Fatal(err)
			}
			response := fixture.registrationResponse(
				begin.Challenge, 0x01|0x04|0x40, 0, "none", HomeLinkOrigin,
			)
			response = test.change(response, begin.Challenge)
			if _, err := authority.FinishRegistration(
				context.Background(), begin.ID, PasskeyRegistration{ResponseJSON: response},
			); err == nil {
				t.Fatal("registration unexpectedly succeeded")
			}
		})
	}
}

func TestRegistrationRejectsIDAlgorithmExtensionsAndTruncatedCBOR(t *testing.T) {
	tests := []struct {
		name   string
		change func([]byte, webAuthnFixture) []byte
	}{
		{"id differs from rawId", func(raw []byte, _ webAuthnFixture) []byte {
			return changeCredentialJSON(t, raw, func(object map[string]any) {
				object["id"] = base64.RawURLEncoding.EncodeToString([]byte{9, 9, 9})
			})
		}},
		{"rawId differs from attested id", func(raw []byte, _ webAuthnFixture) []byte {
			return changeCredentialJSON(t, raw, func(object map[string]any) {
				other := base64.RawURLEncoding.EncodeToString([]byte{8, 8, 8})
				object["id"] = other
				object["rawId"] = other
			})
		}},
		{"extension output", func(raw []byte, _ webAuthnFixture) []byte {
			return changeCredentialJSON(t, raw, func(object map[string]any) {
				object["clientExtensionResults"] = map[string]any{"credProps": true}
			})
		}},
		{"truncated CBOR", func(raw []byte, _ webAuthnFixture) []byte {
			return changeCredentialJSON(t, raw, func(object map[string]any) {
				response := object["response"].(map[string]any)
				response["attestationObject"] = base64.RawURLEncoding.EncodeToString([]byte{0xa3, 0x63})
			})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newAuthorityTestStore(t)
			pairing := NewLocalPairingManager(LocalPairingManagerOptions{})
			authority, err := NewPersistentCredentialAuthority(PersistentCredentialAuthorityOptions{
				Store: store, SiteID: "001122334455667788", PairingAuthorizer: pairing,
			})
			if err != nil {
				t.Fatal(err)
			}
			pair, err := pairing.Create(time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			begin, err := authority.BeginRegistration(context.Background(), LocalPairingProof{
				Challenge: []byte(pair.ID), Response: pair.Secret,
			}, "phone")
			if err != nil {
				t.Fatal(err)
			}
			fixture := newWebAuthnFixture(t, []byte{1, 2, 3})
			raw := fixture.registrationResponse(
				begin.Challenge, 0x01|0x04|0x40, 0, "none", HomeLinkOrigin,
			)
			raw = test.change(raw, fixture)
			if _, err := authority.FinishRegistration(
				context.Background(), begin.ID, PasskeyRegistration{ResponseJSON: raw},
			); err == nil {
				t.Fatal("registration unexpectedly succeeded")
			}
		})
	}

	t.Run("non ES256", func(t *testing.T) {
		store := newAuthorityTestStore(t)
		pairing := NewLocalPairingManager(LocalPairingManagerOptions{})
		authority, err := NewPersistentCredentialAuthority(PersistentCredentialAuthorityOptions{
			Store: store, SiteID: "001122334455667788", PairingAuthorizer: pairing,
		})
		if err != nil {
			t.Fatal(err)
		}
		pair, err := pairing.Create(time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		begin, err := authority.BeginRegistration(context.Background(), LocalPairingProof{
			Challenge: []byte(pair.ID), Response: pair.Secret,
		}, "phone")
		if err != nil {
			t.Fatal(err)
		}
		fixture := newWebAuthnFixture(t, []byte{1, 2, 3})
		fixture.publicKey = cosePublicKey(fixture.private.PublicKey, -8)
		raw := fixture.registrationResponse(
			begin.Challenge, 0x01|0x04|0x40, 0, "none", HomeLinkOrigin,
		)
		if _, err := authority.FinishRegistration(
			context.Background(), begin.ID, PasskeyRegistration{ResponseJSON: raw},
		); err == nil {
			t.Fatal("non-ES256 registration unexpectedly succeeded")
		}
	})
}

func TestRegistrationRejectsNonEC2AndAmbiguousCOSEKeys(t *testing.T) {
	for _, test := range []struct {
		name string
		key  []byte
	}{
		{"OKP labeled ES256", cborMap(
			cborInt(1), cborInt(1),
			cborInt(3), cborInt(-7),
			cborInt(-1), cborInt(6),
			cborInt(-2), cborBytes(bytes.Repeat([]byte{7}, 32)),
		)},
		{"RSA labeled ES256", cborMap(
			cborInt(1), cborInt(3),
			cborInt(3), cborInt(-7),
			cborInt(-1), cborBytes(bytes.Repeat([]byte{7}, 256)),
			cborInt(-2), cborBytes([]byte{1, 0, 1}),
		)},
		{"trailing COSE", append(coseES256PublicKey(
			newWebAuthnFixture(t, []byte{1}).private.PublicKey,
		), 0)},
		{"duplicate COSE label", cborMap(
			cborInt(1), cborInt(2),
			cborInt(1), cborInt(2),
			cborInt(3), cborInt(-7),
			cborInt(-1), cborInt(1),
			cborInt(-2), cborBytes(bytes.Repeat([]byte{1}, 32)),
		)},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newAuthorityTestStore(t)
			pairing := NewLocalPairingManager(LocalPairingManagerOptions{})
			authority, err := NewPersistentCredentialAuthority(PersistentCredentialAuthorityOptions{
				Store: store, SiteID: "001122334455667788", PairingAuthorizer: pairing,
			})
			if err != nil {
				t.Fatal(err)
			}
			pair, err := pairing.Create(time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			begin, err := authority.BeginRegistration(context.Background(), LocalPairingProof{
				Challenge: []byte(pair.ID), Response: pair.Secret,
			}, "phone")
			if err != nil {
				t.Fatal(err)
			}
			fixture := newWebAuthnFixture(t, []byte{1, 2, 3})
			fixture.publicKey = test.key
			if _, err := authority.FinishRegistration(
				context.Background(), begin.ID, PasskeyRegistration{ResponseJSON: fixture.registrationResponse(
					begin.Challenge, 0x01|0x04|0x40, 0, "none", HomeLinkOrigin,
				)},
			); err == nil {
				t.Fatalf("registration error = %v", err)
			}
		})
	}
}

func TestAssertionRejectsInvalidStoredCOSEKey(t *testing.T) {
	tests := []struct {
		name string
		key  []byte
	}{
		{"truncated", []byte{0xa5, 0x01}},
		{"OKP labeled ES256", cborMap(
			cborInt(1), cborInt(1),
			cborInt(3), cborInt(-7),
			cborInt(-1), cborInt(6),
			cborInt(-2), cborBytes(bytes.Repeat([]byte{7}, 32)),
		)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newAuthorityTestStore(t)
			fixture := newWebAuthnFixture(t, []byte{1, 2, 3})
			record := state.HomeLinkCredentialRecord{
				SiteID: "001122334455667788", CredentialID: fixture.credentialID,
				PublicKey: test.key, Label: "phone",
				UserHandle: bytes.Repeat([]byte{2}, webAuthnUserHandleBytes),
				Status:     state.HomeLinkCredentialActive, Revision: 1, CreatedAtMS: 1, UpdatedAtMS: 1,
			}
			if err := store.RegisterHomeLinkCredential(context.Background(), record); err != nil {
				t.Fatal(err)
			}
			authority, err := NewPersistentCredentialAuthority(PersistentCredentialAuthorityOptions{
				Store: store, SiteID: record.SiteID,
			})
			if err != nil {
				t.Fatal(err)
			}
			challenge, err := authority.CreateAssertion(
				context.Background(), AssertionExpectationBinding{deadline: time.Hour},
			)
			if err != nil {
				t.Fatal(err)
			}
			raw := fixture.assertionResponse(
				challenge.Challenge, 0, 0x01|0x04, record.UserHandle, HomeLinkOrigin,
			)
			if _, _, err := authority.VerifyAndConsumeAssertion(
				context.Background(), challenge.ID, PasskeyAssertion{ResponseJSON: raw},
			); !errors.Is(err, ErrWebAuthnVerification) {
				t.Fatalf("stored COSE key = %v", err)
			}
		})
	}
}

func TestWebAuthnStrictJSONAndAttestationCBOR(t *testing.T) {
	fixture := newWebAuthnFixture(t, []byte{1, 2, 3})
	challenge := bytes.Repeat([]byte{1}, webAuthnChallengeBytes)
	assertion := fixture.assertionResponse(challenge, 0, 0x01|0x04, nil, HomeLinkOrigin)
	duplicateOuter := append([]byte(`{"id":"AQ","id":"AQ",`), assertion[1:]...)
	if _, err := parseAssertion(duplicateOuter); !errors.Is(err, ErrWebAuthnInput) {
		t.Fatalf("duplicate outer JSON = %v", err)
	}
	duplicateClient := changeCredentialJSON(t, assertion, func(object map[string]any) {
		response := object["response"].(map[string]any)
		response["clientDataJSON"] = base64.RawURLEncoding.EncodeToString([]byte(
			`{"type":"webauthn.get","type":"webauthn.create","challenge":"AQ","origin":"https://home.sourceful.energy","crossOrigin":false}`,
		))
	})
	if _, err := parseAssertion(duplicateClient); !errors.Is(err, ErrWebAuthnInput) {
		t.Fatalf("duplicate clientData JSON = %v", err)
	}

	registration := fixture.registrationResponse(
		challenge, 0x01|0x04|0x40, 0, "none", HomeLinkOrigin,
	)
	for _, test := range []struct {
		name   string
		change func([]byte) []byte
	}{
		{"trailing value", func(raw []byte) []byte { return append(bytes.Clone(raw), 0) }},
		{"null attStmt", func(authData []byte) []byte {
			return cborMap(
				cborText("fmt"), cborText("none"),
				cborText("attStmt"), []byte{0xf6},
				cborText("authData"), cborBytes(authData),
			)
		}},
		{"nonempty attStmt", func(authData []byte) []byte {
			return cborMap(
				cborText("fmt"), cborText("none"),
				cborText("attStmt"), cborMap(cborText("x"), cborInt(1)),
				cborText("authData"), cborBytes(authData),
			)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := changeCredentialJSON(t, registration, func(object map[string]any) {
				response := object["response"].(map[string]any)
				encoded := response["attestationObject"].(string)
				attestation, err := base64.RawURLEncoding.DecodeString(encoded)
				if err != nil {
					t.Fatal(err)
				}
				if test.name == "trailing value" {
					attestation = test.change(attestation)
				} else {
					parsed, err := parseRegistration(registration)
					if err != nil {
						t.Fatal(err)
					}
					attestation = test.change(parsed.Response.AttestationObject.RawAuthData)
				}
				response["attestationObject"] = base64.RawURLEncoding.EncodeToString(attestation)
			})
			if _, err := parseRegistration(changed); !errors.Is(err, ErrWebAuthnInput) {
				t.Fatalf("attestation CBOR = %v", err)
			}
		})
	}
}

func TestWebAuthnRejectsWrongJSONTypesAndNonCanonicalBase64(t *testing.T) {
	fixture := newWebAuthnFixture(t, []byte{1, 2, 3})
	challenge := bytes.Repeat([]byte{1}, webAuthnChallengeBytes)
	assertion := fixture.assertionResponse(
		challenge, 0, 0x01|0x04, nil, HomeLinkOrigin,
	)
	tests := []struct {
		name   string
		change func(map[string]any)
	}{
		{"numeric id", func(object map[string]any) { object["id"] = float64(1) }},
		{"numeric rawId", func(object map[string]any) { object["rawId"] = float64(1) }},
		{"padded id", func(object map[string]any) { object["id"] = object["id"].(string) + "=" }},
		{"padded rawId", func(object map[string]any) { object["rawId"] = object["rawId"].(string) + "=" }},
		{"numeric type", func(object map[string]any) { object["type"] = float64(1) }},
		{"response is string", func(object map[string]any) { object["response"] = "x" }},
		{"extensions are array", func(object map[string]any) {
			object["clientExtensionResults"] = []any{}
		}},
		{"numeric client data", func(object map[string]any) {
			object["response"].(map[string]any)["clientDataJSON"] = float64(1)
		}},
		{"numeric authenticator data", func(object map[string]any) {
			object["response"].(map[string]any)["authenticatorData"] = float64(1)
		}},
		{"numeric signature", func(object map[string]any) {
			object["response"].(map[string]any)["signature"] = float64(1)
		}},
		{"numeric user handle", func(object map[string]any) {
			object["response"].(map[string]any)["userHandle"] = float64(1)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := changeCredentialJSON(t, assertion, test.change)
			if _, err := parseAssertion(changed); !errors.Is(err, ErrWebAuthnInput) {
				t.Fatalf("wrong JSON type or encoding = %v", err)
			}
		})
	}
}

func TestWebAuthnRequiresEmptyClientExtensionResultsObject(t *testing.T) {
	fixture := newWebAuthnFixture(t, []byte{1, 2, 3})
	challenge := bytes.Repeat([]byte{1}, webAuthnChallengeBytes)
	ceremonies := []struct {
		name  string
		raw   []byte
		parse func([]byte) error
	}{
		{
			name: "registration",
			raw: fixture.registrationResponse(
				challenge, 0x01|0x04|0x40, 0, "none", HomeLinkOrigin,
			),
			parse: func(raw []byte) error {
				_, err := parseRegistration(raw)
				return err
			},
		},
		{
			name: "assertion",
			raw: fixture.assertionResponse(
				challenge, 0, 0x01|0x04, nil, HomeLinkOrigin,
			),
			parse: func(raw []byte) error {
				_, err := parseAssertion(raw)
				return err
			},
		},
	}
	cases := []struct {
		name   string
		change func(map[string]any)
		valid  bool
	}{
		{
			name: "missing",
			change: func(object map[string]any) {
				delete(object, "clientExtensionResults")
			},
		},
		{
			name: "null",
			change: func(object map[string]any) {
				object["clientExtensionResults"] = nil
			},
		},
		{
			name: "string",
			change: func(object map[string]any) {
				object["clientExtensionResults"] = "x"
			},
		},
		{
			name: "number",
			change: func(object map[string]any) {
				object["clientExtensionResults"] = float64(1)
			},
		},
		{
			name: "array",
			change: func(object map[string]any) {
				object["clientExtensionResults"] = []any{}
			},
		},
		{
			name: "empty object",
			change: func(object map[string]any) {
				object["clientExtensionResults"] = map[string]any{}
			},
			valid: true,
		},
		{
			name: "nonempty object",
			change: func(object map[string]any) {
				object["clientExtensionResults"] = map[string]any{"appid": false}
			},
		},
	}
	for _, ceremony := range ceremonies {
		for _, test := range cases {
			t.Run(ceremony.name+"/"+test.name, func(t *testing.T) {
				raw := changeCredentialJSON(t, ceremony.raw, test.change)
				err := ceremony.parse(raw)
				if test.valid {
					if err != nil {
						t.Fatalf("empty extension object = %v", err)
					}
					return
				}
				if !errors.Is(err, ErrWebAuthnInput) {
					t.Fatalf("invalid extension output = %v", err)
				}
			})
		}
	}
}

func TestStrictCOSERejectsNonMinimalEncodings(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
	}{
		{"small integer in uint8", []byte{0x18, 0x17}},
		{"uint8 value in uint16", []byte{0x19, 0x00, 0xff}},
		{"uint16 value in uint32", []byte{0x1a, 0x00, 0x00, 0xff, 0xff}},
		{"uint32 value in uint64", []byte{0x1b, 0x00, 0x00, 0x00, 0x00, 0xff, 0xff, 0xff, 0xff}},
		{"negative integer in uint8", []byte{0x38, 0x00}},
		{"map length in uint8", []byte{0xb8, 0x03}},
		{"text length in uint8", append([]byte{0x78, 0x03}, []byte("fmt")...)},
		{"bytes length in uint8", append([]byte{0x58, 0x03}, []byte{1, 2, 3}...)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := strictCBORReader{raw: test.raw}
			if _, _, err := reader.head(); err == nil {
				t.Fatal("accepted non-minimal CBOR head")
			}
		})
	}

	fixture := newWebAuthnFixture(t, []byte{1, 2, 3})
	challenge := bytes.Repeat([]byte{1}, webAuthnChallengeBytes)
	registration := fixture.registrationResponse(
		challenge, 0x01|0x04|0x40, 0, "none", HomeLinkOrigin,
	)
	nonMinimalAttestation := changeCredentialJSON(t, registration, func(object map[string]any) {
		response := object["response"].(map[string]any)
		encoded := response["attestationObject"].(string)
		attestation, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if len(attestation) == 0 || attestation[0] != 0xa3 {
			t.Fatalf("unexpected attestation fixture %x", attestation)
		}
		attestation = append([]byte{0xb8, 0x03}, attestation[1:]...)
		response["attestationObject"] = base64.RawURLEncoding.EncodeToString(attestation)
	})
	if _, err := parseRegistration(nonMinimalAttestation); !errors.Is(err, ErrWebAuthnInput) {
		t.Fatalf("non-minimal attestation map = %v", err)
	}

	nonMinimalCOSE := append([]byte{0xb8, 0x05}, fixture.publicKey[1:]...)
	if err := validateES256CredentialPublicKey(nonMinimalCOSE); err == nil {
		t.Fatal("accepted non-minimal COSE map")
	}
}

func TestWebAuthnEnvelopeTypesAndCanonicalBase64URL(t *testing.T) {
	fixture := newWebAuthnFixture(t, []byte{1, 2, 3})
	challenge := bytes.Repeat([]byte{1}, webAuthnChallengeBytes)
	assertion := fixture.assertionResponse(
		challenge, 0, 0x01|0x04, nil, HomeLinkOrigin,
	)
	for _, test := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"numeric id", func(object map[string]any) { object["id"] = 1 }},
		{"null rawId", func(object map[string]any) { object["rawId"] = nil }},
		{"array type", func(object map[string]any) { object["type"] = []any{} }},
		{"string response", func(object map[string]any) { object["response"] = "no" }},
		{"numeric clientDataJSON", func(object map[string]any) {
			object["response"].(map[string]any)["clientDataJSON"] = 1
		}},
		{"null authenticatorData", func(object map[string]any) {
			object["response"].(map[string]any)["authenticatorData"] = nil
		}},
		{"object signature", func(object map[string]any) {
			object["response"].(map[string]any)["signature"] = map[string]any{}
		}},
		{"numeric userHandle", func(object map[string]any) {
			object["response"].(map[string]any)["userHandle"] = 1
		}},
		{"padded id", func(object map[string]any) {
			object["id"] = object["id"].(string) + "="
		}},
		{"padded rawId", func(object map[string]any) {
			object["rawId"] = object["rawId"].(string) + "="
		}},
		{"padded clientDataJSON", func(object map[string]any) {
			response := object["response"].(map[string]any)
			response["clientDataJSON"] = response["clientDataJSON"].(string) + "="
		}},
		{"padded authenticatorData", func(object map[string]any) {
			response := object["response"].(map[string]any)
			response["authenticatorData"] = response["authenticatorData"].(string) + "="
		}},
		{"padded signature", func(object map[string]any) {
			response := object["response"].(map[string]any)
			response["signature"] = response["signature"].(string) + "="
		}},
		{"padded userHandle", func(object map[string]any) {
			object["response"].(map[string]any)["userHandle"] = "AQ=="
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := changeCredentialJSON(t, assertion, test.change)
			if _, err := parseAssertion(raw); !errors.Is(err, ErrWebAuthnInput) {
				t.Fatalf("parse assertion = %v", err)
			}
		})
	}
	emptyHandle := changeCredentialJSON(t, assertion, func(object map[string]any) {
		object["response"].(map[string]any)["userHandle"] = ""
	})
	if _, err := parseAssertion(emptyHandle); err != nil {
		t.Fatalf("empty canonical user handle: %v", err)
	}

	registration := fixture.registrationResponse(
		challenge, 0x01|0x04|0x40, 0, "none", HomeLinkOrigin,
	)
	for _, test := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"numeric clientDataJSON", func(object map[string]any) {
			object["response"].(map[string]any)["clientDataJSON"] = 1
		}},
		{"null attestationObject", func(object map[string]any) {
			object["response"].(map[string]any)["attestationObject"] = nil
		}},
		{"padded clientDataJSON", func(object map[string]any) {
			response := object["response"].(map[string]any)
			response["clientDataJSON"] = response["clientDataJSON"].(string) + "="
		}},
		{"padded attestationObject", func(object map[string]any) {
			response := object["response"].(map[string]any)
			response["attestationObject"] = response["attestationObject"].(string) + "="
		}},
	} {
		t.Run("registration "+test.name, func(t *testing.T) {
			raw := changeCredentialJSON(t, registration, test.change)
			if _, err := parseRegistration(raw); !errors.Is(err, ErrWebAuthnInput) {
				t.Fatalf("parse registration = %v", err)
			}
		})
	}
}

func TestStrictCBORRejectsNonMinimalHeads(t *testing.T) {
	fixture := newWebAuthnFixture(t, []byte{1, 2, 3})
	validKey := fixture.publicKey
	coordinateHead := bytes.Index(validKey, []byte{0x58, 0x20})
	if coordinateHead < 0 {
		t.Fatal("fixture lacks 32-byte coordinate")
	}
	for _, test := range []struct {
		name string
		raw  []byte
	}{
		{"map count", append([]byte{0xb8, 0x05}, validKey[1:]...)},
		{"integer", append(append([]byte{0xa5, 0x18, 0x01}, validKey[2:]...), nil...)},
		{"byte string length", append(
			append(bytes.Clone(validKey[:coordinateHead]), 0x59, 0x00, 0x20),
			validKey[coordinateHead+2:]...,
		)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateES256CredentialPublicKey(test.raw); err == nil {
				t.Fatal("non-minimal COSE key accepted")
			}
		})
	}

	challenge := bytes.Repeat([]byte{1}, webAuthnChallengeBytes)
	registration := fixture.registrationResponse(
		challenge, 0x01|0x04|0x40, 0, "none", HomeLinkOrigin,
	)
	overlongMap := changeCredentialJSON(t, registration, func(object map[string]any) {
		response := object["response"].(map[string]any)
		encoded := response["attestationObject"].(string)
		raw, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatal(err)
		}
		raw = append([]byte{0xb8, 0x03}, raw[1:]...)
		response["attestationObject"] = base64.RawURLEncoding.EncodeToString(raw)
	})
	if _, err := parseRegistration(overlongMap); !errors.Is(err, ErrWebAuthnInput) {
		t.Fatalf("overlong attestation map = %v", err)
	}
}

func TestConcurrentAssertionAndRevokeEndsRevoked(t *testing.T) {
	authority, fixture, handle, store := newRegisteredAuthority(
		t, "001122334455667788", 0x01|0x04|0x40, 0,
	)
	challenge, err := authority.CreateAssertion(
		context.Background(), AssertionExpectationBinding{deadline: time.Hour},
	)
	if err != nil {
		t.Fatal(err)
	}
	raw := fixture.assertionResponse(challenge.Challenge, 1, 0x01|0x04, handle, HomeLinkOrigin)
	start := make(chan struct{})
	done := make(chan error, 2)
	go func() {
		<-start
		_, _, err := authority.VerifyAndConsumeAssertion(
			context.Background(), challenge.ID, PasskeyAssertion{ResponseJSON: raw},
		)
		done <- err
	}()
	go func() {
		<-start
		done <- authority.RevokeCredential(context.Background(), fixture.credentialID)
	}()
	close(start)
	<-done
	<-done
	stored, err := store.HomeLinkCredential(
		context.Background(), "001122334455667788", fixture.credentialID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != state.HomeLinkCredentialRevoked {
		t.Fatalf("final status = %q", stored.Status)
	}
}

func TestAssertionStoreErrorConsumesExpectation(t *testing.T) {
	authority, fixture, handle, store := newRegisteredAuthority(
		t, "001122334455667788", 0x01|0x04|0x40, 0,
	)
	challenge, err := authority.CreateAssertion(
		context.Background(), AssertionExpectationBinding{deadline: time.Hour},
	)
	if err != nil {
		t.Fatal(err)
	}
	raw := fixture.assertionResponse(
		challenge.Challenge, 1, 0x01|0x04, handle, HomeLinkOrigin,
	)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := authority.VerifyAndConsumeAssertion(
		context.Background(), challenge.ID, PasskeyAssertion{ResponseJSON: raw},
	); err == nil {
		t.Fatal("closed-store assertion unexpectedly succeeded")
	}
	if _, _, err := authority.VerifyAndConsumeAssertion(
		context.Background(), challenge.ID, PasskeyAssertion{ResponseJSON: []byte(`{}`)},
	); !errors.Is(err, ErrWebAuthnExpectation) {
		t.Fatalf("expectation reopened after store error = %v", err)
	}
}

func TestCredentialStoreContainsNoPairingSecretChallengeOrPrivateKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	pairing := NewLocalPairingManager(LocalPairingManagerOptions{})
	authority, err := NewPersistentCredentialAuthority(PersistentCredentialAuthorityOptions{
		Store: store, SiteID: "001122334455667788", PairingAuthorizer: pairing,
	})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := pairing.Create(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	begin, err := authority.BeginRegistration(context.Background(), LocalPairingProof{
		Challenge: []byte(pair.ID), Response: pair.Secret,
	}, "phone")
	if err != nil {
		t.Fatal(err)
	}
	fixture := newWebAuthnFixture(t, []byte{1, 2, 3})
	if _, err := authority.FinishRegistration(
		context.Background(), begin.ID, PasskeyRegistration{ResponseJSON: fixture.registrationResponse(
			begin.Challenge, 0x01|0x04|0x40, 0, "none", HomeLinkOrigin,
		)},
	); err != nil {
		t.Fatal(err)
	}
	assertion, err := authority.CreateAssertion(
		context.Background(), AssertionExpectationBinding{deadline: time.Hour},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	privateScalar := fixture.private.D.FillBytes(make([]byte, 32))
	for name, secret := range map[string][]byte{
		"pairing secret": pair.Secret, "assertion challenge": assertion.Challenge,
		"private key": privateScalar,
	} {
		if bytes.Contains(raw, secret) {
			t.Fatalf("state.db contains %s", name)
		}
	}
}

func TestWebAuthnResponseSizeAndTrailingData(t *testing.T) {
	fixture := newWebAuthnFixture(t, []byte{1, 2, 3})
	raw := fixture.assertionResponse(
		bytes.Repeat([]byte{1}, webAuthnChallengeBytes), 0, 0x01|0x04, nil, HomeLinkOrigin,
	)
	if len(raw) >= maxWebAuthnResponseBytes {
		t.Fatal("fixture is too large")
	}
	exact := append(bytes.Clone(raw), bytes.Repeat([]byte(" "), maxWebAuthnResponseBytes-len(raw))...)
	if _, err := parseAssertion(exact); err != nil {
		t.Fatalf("exact 16 KiB boundary: %v", err)
	}
	if _, err := parseAssertion(append(exact, ' ')); !errors.Is(err, ErrWebAuthnInput) {
		t.Fatalf("over boundary = %v", err)
	}
	if _, err := parseAssertion(append(raw, []byte(`{}`)...)); !errors.Is(err, ErrWebAuthnInput) {
		t.Fatalf("trailing JSON = %v", err)
	}
	if _, err := parseAssertion([]byte(`{"id":`)); !errors.Is(err, ErrWebAuthnInput) {
		t.Fatalf("malformed JSON = %v", err)
	}
}

func FuzzParseWebAuthnResponses(f *testing.F) {
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"id":"AQ","rawId":"AQ","type":"public-key","response":{}}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > maxWebAuthnResponseBytes+1 {
			raw = raw[:maxWebAuthnResponseBytes+1]
		}
		_, _ = parseAssertion(raw)
		_, _ = parseRegistration(raw)
	})
}

func (f webAuthnFixture) registrationResponse(
	challenge []byte,
	flags byte,
	counter uint32,
	format string,
	origin string,
) []byte {
	return f.registrationResponseForRPID(
		challenge, flags, counter, format, origin, HomeLinkRPID,
	)
}

func (f webAuthnFixture) registrationResponseForRPID(
	challenge []byte,
	flags byte,
	counter uint32,
	format string,
	origin string,
	rpID string,
) []byte {
	clientData := clientDataJSON("webauthn.create", challenge, origin, false, "")
	rpHash := sha256.Sum256([]byte(rpID))
	authData := append([]byte{}, rpHash[:]...)
	authData = append(authData, flags)
	authData = binary.BigEndian.AppendUint32(authData, counter)
	authData = append(authData, make([]byte, 16)...)
	authData = binary.BigEndian.AppendUint16(authData, uint16(len(f.credentialID)))
	authData = append(authData, f.credentialID...)
	authData = append(authData, f.publicKey...)
	attestation := cborMap(
		cborText("fmt"), cborText(format),
		cborText("attStmt"), cborMap(),
		cborText("authData"), cborBytes(authData),
	)
	return credentialJSON(f.credentialID, clientData, attestation, nil, nil, nil)
}

func (f webAuthnFixture) assertionResponse(
	challenge []byte,
	counter uint32,
	flags byte,
	userHandle []byte,
	origin string,
) []byte {
	return f.assertionResponseForRPID(
		challenge, counter, flags, userHandle, origin, HomeLinkRPID,
	)
}

func (f webAuthnFixture) assertionResponseForRPID(
	challenge []byte,
	counter uint32,
	flags byte,
	userHandle []byte,
	origin string,
	rpID string,
) []byte {
	return f.assertionResponseWithPolicy(
		challenge, counter, flags, userHandle, origin, rpID, "webauthn.get", false, "",
	)
}

func (f webAuthnFixture) assertionResponseWithPolicy(
	challenge []byte,
	counter uint32,
	flags byte,
	userHandle []byte,
	origin string,
	rpID string,
	ceremonyType string,
	crossOrigin bool,
	topOrigin string,
) []byte {
	clientData := clientDataJSON(ceremonyType, challenge, origin, crossOrigin, topOrigin)
	rpHash := sha256.Sum256([]byte(rpID))
	authData := append([]byte{}, rpHash[:]...)
	authData = append(authData, flags)
	authData = binary.BigEndian.AppendUint32(authData, counter)
	clientHash := sha256.Sum256(clientData)
	signed := append(bytes.Clone(authData), clientHash[:]...)
	digest := sha256.Sum256(signed)
	r, s, err := ecdsa.Sign(rand.Reader, f.private, digest[:])
	if err != nil {
		panic(err)
	}
	signature := asn1ECDSASignature(r, s)
	return credentialJSON(f.credentialID, clientData, nil, authData, signature, userHandle)
}

func credentialJSON(
	credentialID, clientData, attestation, authData, signature, userHandle []byte,
) []byte {
	response := map[string]any{
		"clientDataJSON": base64.RawURLEncoding.EncodeToString(clientData),
	}
	if attestation != nil {
		response["attestationObject"] = base64.RawURLEncoding.EncodeToString(attestation)
	} else {
		response["authenticatorData"] = base64.RawURLEncoding.EncodeToString(authData)
		response["signature"] = base64.RawURLEncoding.EncodeToString(signature)
		if userHandle != nil {
			response["userHandle"] = base64.RawURLEncoding.EncodeToString(userHandle)
		}
	}
	raw, err := json.Marshal(map[string]any{
		"id":                     base64.RawURLEncoding.EncodeToString(credentialID),
		"rawId":                  base64.RawURLEncoding.EncodeToString(credentialID),
		"type":                   "public-key",
		"response":               response,
		"clientExtensionResults": map[string]any{},
	})
	if err != nil {
		panic(err)
	}
	return raw
}

func clientDataJSON(kind string, challenge []byte, origin string, crossOrigin bool, topOrigin string) []byte {
	data := map[string]any{
		"type": kind, "challenge": base64.RawURLEncoding.EncodeToString(challenge),
		"origin": origin, "crossOrigin": crossOrigin,
	}
	if topOrigin != "" {
		data["topOrigin"] = topOrigin
	}
	raw, err := json.Marshal(data)
	if err != nil {
		panic(err)
	}
	return raw
}

func changeCredentialJSON(t *testing.T, raw []byte, change func(map[string]any)) []byte {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	change(object)
	changed, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return changed
}

func changeClientData(t *testing.T, raw []byte, change func(map[string]any)) []byte {
	t.Helper()
	return changeCredentialJSON(t, raw, func(object map[string]any) {
		response := object["response"].(map[string]any)
		encoded := response["clientDataJSON"].(string)
		clientData, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatal(err)
		}
		var data map[string]any
		if err := json.Unmarshal(clientData, &data); err != nil {
			t.Fatal(err)
		}
		change(data)
		changed, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		response["clientDataJSON"] = base64.RawURLEncoding.EncodeToString(changed)
	})
}

func coseES256PublicKey(key ecdsa.PublicKey) []byte {
	return cosePublicKey(key, -7)
}

func cosePublicKey(key ecdsa.PublicKey, algorithm int) []byte {
	return cborMap(
		cborInt(1), cborInt(2),
		cborInt(3), cborInt(algorithm),
		cborInt(-1), cborInt(1),
		cborInt(-2), cborBytes(key.X.FillBytes(make([]byte, 32))),
		cborInt(-3), cborBytes(key.Y.FillBytes(make([]byte, 32))),
	)
}

func cborMap(items ...[]byte) []byte {
	if len(items)%2 != 0 {
		panic("CBOR map needs pairs")
	}
	out := cborHead(5, uint64(len(items)/2))
	for _, item := range items {
		out = append(out, item...)
	}
	return out
}

func cborText(value string) []byte {
	return append(cborHead(3, uint64(len(value))), value...)
}

func cborBytes(value []byte) []byte {
	return append(cborHead(2, uint64(len(value))), value...)
}

func cborInt(value int) []byte {
	if value >= 0 {
		return cborHead(0, uint64(value))
	}
	return cborHead(1, uint64(-1-value))
}

func cborHead(major byte, value uint64) []byte {
	prefix := major << 5
	switch {
	case value < 24:
		return []byte{prefix | byte(value)}
	case value <= 0xff:
		return []byte{prefix | 24, byte(value)}
	case value <= 0xffff:
		return []byte{prefix | 25, byte(value >> 8), byte(value)}
	default:
		panic("test CBOR value is too large")
	}
}

func asn1ECDSASignature(r, s *big.Int) []byte {
	rb := signedASN1Integer(r.Bytes())
	sb := signedASN1Integer(s.Bytes())
	body := append(append([]byte{}, rb...), sb...)
	return append([]byte{0x30, byte(len(body))}, body...)
}

func signedASN1Integer(value []byte) []byte {
	value = bytes.TrimLeft(value, "\x00")
	if len(value) == 0 {
		value = []byte{0}
	}
	if value[0]&0x80 != 0 {
		value = append([]byte{0}, value...)
	}
	return append([]byte{0x02, byte(len(value))}, value...)
}

func TestWebAuthnFixedErrorsDoNotExposeProtocolValues(t *testing.T) {
	values := []string{
		ErrWebAuthnInput.Error(), ErrWebAuthnVerification.Error(),
		ErrWebAuthnExpectation.Error(), ErrCredentialUnknown.Error(),
	}
	for _, value := range values {
		for _, secret := range []string{
			HomeLinkOrigin, HomeLinkRPID, "sample-challenge-value", "AQIDBA",
		} {
			if strings.Contains(value, secret) {
				t.Fatalf("local error %q exposes %q", value, secret)
			}
		}
	}
}
