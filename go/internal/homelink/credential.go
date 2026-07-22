package homelink

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const (
	maxCredentialIDBytes = 1024
	maxPublicKeyBytes    = 4096
	maxCredentialLabel   = 80
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

type AssertionExpectation struct {
	Challenge      []byte
	RelyingPartyID string
	Origin         string
}

type PasskeyAssertion struct {
	CredentialID      []byte
	ClientDataJSON    []byte
	AuthenticatorData []byte
	Signature         []byte
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

// AssertionVerifier is the port for a local WebAuthn implementation. The
// first contract PR has no fallback verifier and no cloud verifier.
type AssertionVerifier interface {
	VerifyAssertion(context.Context, AssertionExpectation, PasskeyAssertion) (Principal, error)
}

// CredentialStore is the local storage port. This contract adds no table and
// no cloud-backed store.
type CredentialStore interface {
	Credential(context.Context, []byte) (CredentialVerifier, error)
	SaveCredential(context.Context, CredentialVerifier) error
	// AdvanceCounter must compare the old value and update it in one step.
	AdvanceCounter(context.Context, []byte, uint32, uint32) error
	RevokeCredential(context.Context, []byte) error
}
