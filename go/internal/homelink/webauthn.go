package homelink

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/srcfl/ftw/go/internal/gatewayidentity"
	"github.com/srcfl/ftw/go/internal/state"
)

const (
	HomeLinkRPID               = "home.sourceful.energy"
	HomeLinkOrigin             = "https://home.sourceful.energy"
	maxWebAuthnResponseBytes   = 16 * 1024
	webAuthnChallengeBytes     = 32
	webAuthnExpectationIDBytes = 24
	webAuthnUserHandleBytes    = 32
	maxWebAuthnExpectations    = 32
)

var (
	ErrWebAuthnInput        = errors.New("passkey response is invalid")
	ErrWebAuthnVerification = errors.New("passkey verification failed")
	ErrWebAuthnExpectation  = errors.New("passkey expectation is unknown or consumed")
	ErrCredentialUnknown    = errors.New("passkey credential is not active for this site")
	ErrCredentialUncertain  = errors.New("passkey credential state is uncertain")
	ErrRegistrationDenied   = errors.New("passkey registration was denied")
)

type RegistrationChallenge struct {
	ID                       string
	Challenge                []byte
	RPID                     string
	UserHandle               []byte
	UserVerificationRequired bool
	Attestation              string
	Algorithms               []int
}

type PasskeyRegistration struct {
	ResponseJSON []byte
}

type PersistentCredentialAuthorityOptions struct {
	Store             *state.Store
	SiteID            string
	PairingAuthorizer PairingAuthorizer
	Random            io.Reader
	Now               func() time.Time
	MonotonicNow      func() time.Duration
}

type assertionExpectation struct {
	challenge string
	binding   AssertionExpectationBinding
	deadline  time.Duration
	allowed   map[string]state.HomeLinkCredentialRecord
}

type registrationExpectation struct {
	challenge  string
	deadline   time.Duration
	label      string
	userHandle []byte
}

type verifiedRegistration struct {
	credentialID   []byte
	publicKey      []byte
	signCount      uint32
	backupEligible bool
	backupState    bool
}

type parsedPasskeyAssertion struct {
	credentialID []byte
	userHandle   []byte
	parsed       *protocol.ParsedCredentialAssertionData
}

type verifiedPasskeyAssertion struct {
	signCount      uint32
	backupEligible bool
	backupState    bool
}

// webAuthnProtocolVerifier is the narrow boundary around the pinned protocol
// package. It owns no credential lookup, lifetime, counter, revoke, or grant.
type webAuthnProtocolVerifier interface {
	VerifyRegistration([]byte, string) (verifiedRegistration, error)
	ParseAssertion([]byte) (parsedPasskeyAssertion, error)
	VerifyAssertion(parsedPasskeyAssertion, string, []byte) (verifiedPasskeyAssertion, error)
}

type pinnedWebAuthnProtocolVerifier struct{}

type PersistentCredentialAuthority struct {
	mu                sync.Mutex
	store             *state.Store
	siteID            string
	pairingAuthorizer PairingAuthorizer
	random            io.Reader
	now               func() time.Time
	monotonicNow      func() time.Duration
	clock             monotonicClockState
	assertions        map[string]assertionExpectation
	registrations     map[string]registrationExpectation
	verifier          webAuthnProtocolVerifier
}

func NewPersistentCredentialAuthority(
	opts PersistentCredentialAuthorityOptions,
) (*PersistentCredentialAuthority, error) {
	if opts.Store == nil {
		return nil, errors.New("Home Link credential store is missing")
	}
	siteID, err := gatewayidentity.NormalizeGatewayID(opts.SiteID)
	if err != nil {
		return nil, err
	}
	if opts.Random == nil {
		opts.Random = rand.Reader
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MonotonicNow == nil {
		opts.MonotonicNow = defaultMonotonicNow
	}
	return &PersistentCredentialAuthority{
		store: opts.Store, siteID: siteID, pairingAuthorizer: opts.PairingAuthorizer,
		random: opts.Random, now: opts.Now, monotonicNow: opts.MonotonicNow,
		assertions:    make(map[string]assertionExpectation),
		registrations: make(map[string]registrationExpectation),
		verifier:      pinnedWebAuthnProtocolVerifier{},
	}, nil
}

func (a *PersistentCredentialAuthority) BeginRegistration(
	ctx context.Context,
	proof LocalPairingProof,
	label string,
) (RegistrationChallenge, error) {
	if a.pairingAuthorizer == nil {
		return RegistrationChallenge{}, ErrRegistrationDenied
	}
	if err := a.pairingAuthorizer.AuthorizeLocalPairing(ctx, proof); err != nil {
		return RegistrationChallenge{}, ErrRegistrationDenied
	}
	label = strings.TrimSpace(label)
	if label == "" || len(label) > maxCredentialLabel {
		return RegistrationChallenge{}, ErrRegistrationDenied
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	now, deadline, err := a.expectationDeadlineLocked()
	if err != nil {
		return RegistrationChallenge{}, err
	}
	a.pruneExpectationsLocked(now)
	if len(a.registrations)+len(a.assertions) >= maxWebAuthnExpectations {
		return RegistrationChallenge{}, errors.New("too many local passkey requests")
	}
	active, err := a.store.ActiveHomeLinkCredentials(ctx, a.siteID)
	if err != nil {
		return RegistrationChallenge{}, errors.New("read local passkey verifier state")
	}
	userHandle, err := a.store.HomeLinkSiteUserHandle(ctx, a.siteID)
	if err != nil {
		return RegistrationChallenge{}, errors.New("read local passkey user handle")
	}
	if len(userHandle) == 0 {
		userHandle, err = a.siteUserHandleLocked(active)
	}
	if err != nil {
		return RegistrationChallenge{}, err
	}
	id, challenge, err := a.newExpectationLocked()
	if err != nil {
		return RegistrationChallenge{}, err
	}
	a.registrations[id] = registrationExpectation{
		challenge: base64.RawURLEncoding.EncodeToString(challenge),
		deadline:  deadline, label: label, userHandle: slices.Clone(userHandle),
	}
	return RegistrationChallenge{
		ID: id, Challenge: challenge, RPID: HomeLinkRPID, UserHandle: slices.Clone(userHandle),
		UserVerificationRequired: true, Attestation: "none", Algorithms: []int{-7},
	}, nil
}

func (a *PersistentCredentialAuthority) FinishRegistration(
	ctx context.Context,
	expectationID string,
	response PasskeyRegistration,
) (CredentialVerifier, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	expectation, ok := a.registrations[expectationID]
	delete(a.registrations, expectationID)
	if !ok {
		return CredentialVerifier{}, ErrWebAuthnExpectation
	}
	if err := ctx.Err(); err != nil {
		return CredentialVerifier{}, err
	}
	now, err := a.clock.sample(a.monotonicNow)
	if err != nil {
		return CredentialVerifier{}, err
	}
	if now >= expectation.deadline {
		return CredentialVerifier{}, ErrAssertionExpired
	}
	verified, err := a.verifier.VerifyRegistration(response.ResponseJSON, expectation.challenge)
	if err != nil {
		return CredentialVerifier{}, err
	}
	wallNow := a.now().UTC().UnixMilli()
	record := state.HomeLinkCredentialRecord{
		SiteID: a.siteID, CredentialID: verified.credentialID, PublicKey: verified.publicKey,
		SignCount: verified.signCount,
		Label:     expectation.label, UserHandle: expectation.userHandle,
		BackupEligible: verified.backupEligible, BackupState: verified.backupState,
		Status: state.HomeLinkCredentialActive, Revision: 1,
		CreatedAtMS: wallNow, UpdatedAtMS: wallNow,
	}
	if err := a.store.RegisterHomeLinkCredential(ctx, record); err != nil {
		return CredentialVerifier{}, errors.New("store local passkey verifier")
	}
	return CredentialVerifier{
		CredentialID: slices.Clone(verified.credentialID),
		PublicKey:    slices.Clone(verified.publicKey),
		Counter:      record.SignCount, Label: record.Label,
	}, nil
}

func (a *PersistentCredentialAuthority) CreateAssertion(
	ctx context.Context,
	binding AssertionExpectationBinding,
) (LocalAssertionChallenge, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now, deadline, err := a.expectationDeadlineLocked()
	if err != nil {
		return LocalAssertionChallenge{}, err
	}
	a.pruneExpectationsLocked(now)
	if len(a.registrations)+len(a.assertions) >= maxWebAuthnExpectations {
		return LocalAssertionChallenge{}, errors.New("too many local passkey requests")
	}
	active, err := a.store.ActiveHomeLinkCredentials(ctx, a.siteID)
	if err != nil {
		return LocalAssertionChallenge{}, errors.New("read local passkey verifier state")
	}
	if len(active) == 0 {
		return LocalAssertionChallenge{}, ErrCredentialUnknown
	}
	if _, err := a.siteUserHandleLocked(active); err != nil {
		return LocalAssertionChallenge{}, err
	}
	allowed := make(map[string]state.HomeLinkCredentialRecord, len(active))
	ids := make([][]byte, 0, len(active))
	for _, credential := range active {
		key := string(credential.CredentialID)
		allowed[key] = credential
		ids = append(ids, slices.Clone(credential.CredentialID))
	}
	id, challenge, err := a.newExpectationLocked()
	if err != nil {
		return LocalAssertionChallenge{}, err
	}
	a.assertions[id] = assertionExpectation{
		challenge: base64.RawURLEncoding.EncodeToString(challenge),
		binding:   binding, deadline: deadline, allowed: allowed,
	}
	return LocalAssertionChallenge{
		ID: id, Challenge: challenge, RPID: HomeLinkRPID, AllowCredentials: ids,
		UserVerificationRequired: true,
	}, nil
}

func (a *PersistentCredentialAuthority) VerifyAndConsumeAssertion(
	ctx context.Context,
	expectationID string,
	assertion PasskeyAssertion,
) (Principal, AssertionExpectationBinding, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	expectation, ok := a.assertions[expectationID]
	delete(a.assertions, expectationID)
	if !ok {
		return Principal{}, AssertionExpectationBinding{}, ErrWebAuthnExpectation
	}
	binding := expectation.binding
	if err := ctx.Err(); err != nil {
		return Principal{}, binding, err
	}
	now, err := a.clock.sample(a.monotonicNow)
	if err != nil {
		return Principal{}, binding, err
	}
	if now >= expectation.deadline {
		return Principal{}, binding, ErrAssertionExpired
	}
	if len(assertion.CredentialID) != 0 || len(assertion.ClientDataJSON) != 0 ||
		len(assertion.AuthenticatorData) != 0 || len(assertion.Signature) != 0 {
		return Principal{}, binding, ErrWebAuthnInput
	}
	parsed, err := a.verifier.ParseAssertion(assertion.ResponseJSON)
	if err != nil {
		return Principal{}, binding, err
	}
	record, ok := expectation.allowed[string(parsed.credentialID)]
	if !ok {
		return Principal{}, binding, ErrCredentialUnknown
	}
	if len(parsed.userHandle) != 0 &&
		subtle.ConstantTimeCompare(parsed.userHandle, record.UserHandle) != 1 {
		return Principal{}, binding, ErrWebAuthnVerification
	}
	verified, err := a.verifier.VerifyAssertion(parsed, expectation.challenge, record.PublicKey)
	if err != nil {
		return Principal{}, binding, err
	}
	updated, err := a.store.ApplyHomeLinkAssertion(ctx, state.HomeLinkAssertionUpdate{
		SiteID: a.siteID, CredentialID: parsed.credentialID, ExpectedRevision: record.Revision,
		SignCount:      verified.signCount,
		BackupEligible: verified.backupEligible, BackupState: verified.backupState,
		UpdatedAtMS: a.now().UTC().UnixMilli(),
	})
	if err != nil {
		if errors.Is(err, state.ErrHomeLinkCredentialPolicy) ||
			errors.Is(err, state.ErrHomeLinkCredentialInactive) ||
			errors.Is(err, state.ErrHomeLinkCredentialConflict) {
			return Principal{}, binding, ErrCredentialUncertain
		}
		return Principal{}, binding, errors.New("update local passkey verifier state")
	}
	return Principal{CredentialID: slices.Clone(updated.CredentialID), Label: updated.Label}, binding, nil
}

func (a *PersistentCredentialAuthority) RevokeCredential(
	ctx context.Context,
	credentialID []byte,
) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.store.RevokeHomeLinkCredential(
		ctx, a.siteID, credentialID, a.now().UTC().UnixMilli(),
	); err != nil {
		return errors.New("revoke local passkey credential")
	}
	return nil
}

func parseRegistration(raw []byte) (*protocol.ParsedCredentialCreationData, error) {
	if len(raw) == 0 || len(raw) > maxWebAuthnResponseBytes {
		return nil, ErrWebAuthnInput
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(raw)
	if err != nil {
		return nil, ErrWebAuthnInput
	}
	return parsed, nil
}

func parseAssertion(raw []byte) (*protocol.ParsedCredentialAssertionData, error) {
	if len(raw) == 0 || len(raw) > maxWebAuthnResponseBytes {
		return nil, ErrWebAuthnInput
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(raw)
	if err != nil {
		return nil, ErrWebAuthnInput
	}
	return parsed, nil
}

func (pinnedWebAuthnProtocolVerifier) VerifyRegistration(
	raw []byte,
	challenge string,
) (verifiedRegistration, error) {
	parsed, err := parseRegistration(raw)
	if err != nil {
		return verifiedRegistration{}, err
	}
	if err := validateParsedCredentialID(parsed.ID, parsed.RawID); err != nil {
		return verifiedRegistration{}, err
	}
	authData := parsed.Response.AttestationObject.AuthData
	if !bytes.Equal(parsed.RawID, authData.AttData.CredentialID) ||
		parsed.Response.AttestationObject.Format != "none" ||
		len(parsed.Response.AttestationObject.AttStatement) != 0 ||
		hasWebAuthnExtensions(parsed.ClientExtensionResults, authData) {
		return verifiedRegistration{}, ErrWebAuthnVerification
	}
	if _, err := parsed.Verify(
		challenge, HomeLinkRPID, []string{HomeLinkOrigin}, nil,
		protocol.TopOriginExplicitVerificationMode, false, true, true, nil,
		[]protocol.CredentialParameter{{
			Type: protocol.PublicKeyCredentialType, Algorithm: -7,
		}},
	); err != nil {
		return verifiedRegistration{}, ErrWebAuthnVerification
	}
	backupEligible := authData.Flags.HasBackupEligible()
	backupState := authData.Flags.HasBackupState()
	if backupState && !backupEligible {
		return verifiedRegistration{}, ErrWebAuthnVerification
	}
	return verifiedRegistration{
		credentialID: slices.Clone(parsed.RawID),
		publicKey:    slices.Clone(authData.AttData.CredentialPublicKey),
		signCount:    authData.Counter, backupEligible: backupEligible, backupState: backupState,
	}, nil
}

func (pinnedWebAuthnProtocolVerifier) ParseAssertion(raw []byte) (parsedPasskeyAssertion, error) {
	parsed, err := parseAssertion(raw)
	if err != nil {
		return parsedPasskeyAssertion{}, err
	}
	if err := validateParsedCredentialID(parsed.ID, parsed.RawID); err != nil {
		return parsedPasskeyAssertion{}, err
	}
	if hasWebAuthnExtensions(parsed.ClientExtensionResults, parsed.Response.AuthenticatorData) {
		return parsedPasskeyAssertion{}, ErrWebAuthnVerification
	}
	return parsedPasskeyAssertion{
		credentialID: slices.Clone(parsed.RawID),
		userHandle:   slices.Clone(parsed.Response.UserHandle),
		parsed:       parsed,
	}, nil
}

func (pinnedWebAuthnProtocolVerifier) VerifyAssertion(
	assertion parsedPasskeyAssertion,
	challenge string,
	storedPublicKey []byte,
) (verifiedPasskeyAssertion, error) {
	if assertion.parsed == nil {
		return verifiedPasskeyAssertion{}, ErrWebAuthnInput
	}
	if err := assertion.parsed.Verify(
		challenge, HomeLinkRPID, "", []string{HomeLinkOrigin}, nil,
		protocol.TopOriginExplicitVerificationMode, false, true, true, storedPublicKey,
	); err != nil {
		return verifiedPasskeyAssertion{}, ErrWebAuthnVerification
	}
	flags := assertion.parsed.Response.AuthenticatorData.Flags
	return verifiedPasskeyAssertion{
		signCount:      assertion.parsed.Response.AuthenticatorData.Counter,
		backupEligible: flags.HasBackupEligible(), backupState: flags.HasBackupState(),
	}, nil
}

func validateParsedCredentialID(id string, rawID []byte) error {
	decoded, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil || len(decoded) == 0 ||
		base64.RawURLEncoding.EncodeToString(decoded) != id ||
		!bytes.Equal(decoded, rawID) {
		return ErrWebAuthnVerification
	}
	return nil
}

func hasWebAuthnExtensions(
	client protocol.AuthenticationExtensionsClientOutputs,
	auth protocol.AuthenticatorData,
) bool {
	return len(client) != 0 || auth.Flags.HasExtensions() || len(auth.ExtData) != 0
}

func (a *PersistentCredentialAuthority) expectationDeadlineLocked() (time.Duration, time.Duration, error) {
	now, err := a.clock.sample(a.monotonicNow)
	if err != nil {
		return 0, 0, err
	}
	deadline, err := a.clock.deadline(now, AssertionExpectationMaxAge)
	return now, deadline, err
}

func (a *PersistentCredentialAuthority) newExpectationLocked() (string, []byte, error) {
	for range 4 {
		idRaw := make([]byte, webAuthnExpectationIDBytes)
		challenge := make([]byte, webAuthnChallengeBytes)
		if _, err := io.ReadFull(a.random, idRaw); err != nil {
			return "", nil, errors.New("create local passkey expectation id")
		}
		if _, err := io.ReadFull(a.random, challenge); err != nil {
			return "", nil, errors.New("create local passkey challenge")
		}
		id := base64.RawURLEncoding.EncodeToString(idRaw)
		if _, assertionExists := a.assertions[id]; assertionExists {
			continue
		}
		if _, registrationExists := a.registrations[id]; registrationExists {
			continue
		}
		return id, challenge, nil
	}
	return "", nil, errors.New("could not create a unique local passkey expectation")
}

func (a *PersistentCredentialAuthority) siteUserHandleLocked(
	records []state.HomeLinkCredentialRecord,
) ([]byte, error) {
	if len(records) == 0 {
		handle := make([]byte, webAuthnUserHandleBytes)
		if _, err := io.ReadFull(a.random, handle); err != nil {
			return nil, errors.New("create local passkey user handle")
		}
		return handle, nil
	}
	handle := records[0].UserHandle
	for _, record := range records[1:] {
		if subtle.ConstantTimeCompare(handle, record.UserHandle) != 1 {
			return nil, errors.New("local passkey user handles are inconsistent")
		}
	}
	return slices.Clone(handle), nil
}

func (a *PersistentCredentialAuthority) pruneExpectationsLocked(now time.Duration) {
	for id, expectation := range a.assertions {
		if now >= expectation.deadline {
			delete(a.assertions, id)
		}
	}
	for id, expectation := range a.registrations {
		if now >= expectation.deadline {
			delete(a.registrations, id)
		}
	}
}
