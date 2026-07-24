package homelink

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	maxCredentialIDBytes         = 1024
	maxPublicKeyBytes            = 4096
	maxCredentialLabel           = 80
	maxAssertionChallengeIDBytes = 256
	maxAssertionChallengeBytes   = 1024
	AssertionExpectationMaxAge   = 5 * time.Minute
)

var (
	ErrAssertionExpired = errors.New("passkey assertion expectation has expired")
	ErrAssertionSession = errors.New("passkey assertion expectation belongs to another session")
	ErrMonotonicClock   = errors.New("monotonic clock is invalid")
)

// CredentialVerifier is all FTW may keep for one local passkey. PublicKey is
// the verifier encoding used by the local WebAuthn implementation.
type CredentialVerifier struct {
	CredentialID []byte `json:"credential_id"`
	PublicKey    []byte `json:"public_key"`
	Counter      uint32 `json:"counter"`
	Label        string `json:"label"`
}

func (v CredentialVerifier) Validate() error {
	if len(v.CredentialID) == 0 || len(v.CredentialID) > maxCredentialIDBytes {
		return fmt.Errorf("credential id must contain 1 to %d bytes", maxCredentialIDBytes)
	}
	if len(v.PublicKey) == 0 || len(v.PublicKey) > maxPublicKeyBytes {
		return fmt.Errorf("public key must contain 1 to %d bytes", maxPublicKeyBytes)
	}
	if strings.TrimSpace(v.Label) == "" || len(v.Label) > maxCredentialLabel {
		return fmt.Errorf("credential label must contain 1 to %d bytes", maxCredentialLabel)
	}
	return nil
}

// Principal exists only after a local verifier accepts a WebAuthn assertion.
type Principal struct {
	CredentialID []byte
	Label        string
}

// LocalAssertionChallenge contains the browser-visible challenge and an
// opaque local lookup ID. The verifier keeps the expected RP, origin, expiry,
// and one-use state; no caller can provide them to grant issuance.
type LocalAssertionChallenge struct {
	ID                       string
	Challenge                []byte
	RPID                     string
	AllowCredentials         [][]byte
	UserVerificationRequired bool
}

// AssertionSession binds local assertion expectations to one GrantManager.
// Its value is opaque outside this package and changes on every manager start.
type AssertionSession struct {
	id [32]byte
}

// AssertionExpectationBinding is the local one-session lifetime stored with
// an assertion expectation. Its monotonic deadline never crosses a restart.
type AssertionExpectationBinding struct {
	session  AssertionSession
	deadline time.Duration
}

func (b AssertionExpectationBinding) validate(session AssertionSession, monotonicNow time.Duration) error {
	if b.session != session {
		return ErrAssertionSession
	}
	if monotonicNow >= b.deadline {
		return ErrAssertionExpired
	}
	return nil
}

type PasskeyAssertion struct {
	CredentialID      []byte
	ClientDataJSON    []byte
	AuthenticatorData []byte
	Signature         []byte
	ResponseJSON      []byte
}

func (c LocalAssertionChallenge) validate() error {
	if len(c.ID) == 0 || len(c.ID) > maxAssertionChallengeIDBytes {
		return fmt.Errorf("assertion challenge id must contain 1 to %d bytes", maxAssertionChallengeIDBytes)
	}
	if len(c.Challenge) == 0 || len(c.Challenge) > maxAssertionChallengeBytes {
		return fmt.Errorf("assertion challenge must contain 1 to %d bytes", maxAssertionChallengeBytes)
	}
	return nil
}

func validateAssertionChallengeID(id string) error {
	if len(id) == 0 || len(id) > maxAssertionChallengeIDBytes {
		return fmt.Errorf("assertion challenge id must contain 1 to %d bytes", maxAssertionChallengeIDBytes)
	}
	return nil
}

func (p Principal) validate() error {
	if len(p.CredentialID) == 0 || len(p.CredentialID) > maxCredentialIDBytes {
		return errors.New("verified principal has no valid credential id")
	}
	if strings.TrimSpace(p.Label) == "" || len(p.Label) > maxCredentialLabel {
		return errors.New("verified principal has no valid label")
	}
	return nil
}

// LocalPairingProof carries the short-lived proof from the LAN pairing flow.
// The authorizer owns and consumes the matching local expectation.
type LocalPairingProof struct {
	Challenge []byte
	Response  []byte
}

// PairingAuthorizer is the port that approves the local, one-time start of a
// pairing. A relay cannot implement this authority.
type PairingAuthorizer interface {
	AuthorizeLocalPairing(context.Context, LocalPairingProof) error
}

// CredentialAuthority is the only local owner of verifier data, assertion
// expectations, counters and durable revoke state. CreateAssertion stores the
// opaque manager binding with the expected challenge, RP and origin.
// VerifyAndConsumeAssertion atomically checks durable credential state and
// consumes its expectation before every result. It returns the stored binding
// so Core can reject another manager session or expiry.
// RevokeCredential must durably record revoked or fail-closed uncertain state
// before it returns. It returns nil only after the revoked state is durable.
// Verifier and revoke state survive restart. Assertion expectations do not.
type CredentialAuthority interface {
	CredentialSite() CredentialSite
	CreateAssertion(context.Context, AssertionExpectationBinding) (LocalAssertionChallenge, error)
	VerifyAndConsumeAssertion(context.Context, string, PasskeyAssertion) (Principal, AssertionExpectationBinding, error)
	RevokeCredential(context.Context, []byte) error
}
