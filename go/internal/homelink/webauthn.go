package homelink

import (
	"bytes"
	"context"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/big"
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
	credentialStateTimeout     = 2 * time.Second
)

var (
	ErrWebAuthnInput        = errors.New("passkey response is invalid")
	ErrWebAuthnVerification = errors.New("passkey verification failed")
	ErrWebAuthnExpectation  = errors.New("passkey expectation is unknown or consumed")
	ErrCredentialUnknown    = errors.New("passkey credential is not active for this site")
	ErrCredentialUncertain  = errors.New("passkey credential state is uncertain")
	errCredentialUnproven   = errors.New("passkey credential could not be proven for this site")
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

type credentialStateStore interface {
	ActiveHomeLinkCredentials(context.Context, string) ([]state.HomeLinkCredentialRecord, error)
	HomeLinkSiteUserHandle(context.Context, string) ([]byte, error)
	RegisterHomeLinkCredential(context.Context, state.HomeLinkCredentialRecord) error
	HomeLinkCredential(context.Context, string, []byte) (state.HomeLinkCredentialRecord, error)
	ApplyHomeLinkAssertion(
		context.Context,
		state.HomeLinkAssertionUpdate,
	) (state.HomeLinkCredentialRecord, error)
	EnsureHomeLinkCredentialPolicyBlock(context.Context, string, []byte, int64) error
	EnsureHomeLinkCredentialEmergencyBlock(context.Context, string, []byte, int64) error
	RevokeHomeLinkCredential(context.Context, string, []byte, int64) error
}

type PersistentCredentialAuthority struct {
	mu                 sync.Mutex
	store              credentialStateStore
	siteID             string
	coordinator        *siteCredentialCoordinator
	pairingAuthorizer  PairingAuthorizer
	random             io.Reader
	now                func() time.Time
	monotonicNow       func() time.Duration
	clock              monotonicClockState
	assertions         map[string]assertionExpectation
	registrations      map[string]registrationExpectation
	blockedCredentials map[string]struct{}
	blockedAll         bool
	knownCredentials   map[string]struct{}
	verifier           webAuthnProtocolVerifier
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
	authority := &PersistentCredentialAuthority{
		store: opts.Store, siteID: siteID,
		coordinator: siteCredentialCoordinatorFor(siteID), pairingAuthorizer: opts.PairingAuthorizer,
		random: opts.Random, now: opts.Now, monotonicNow: opts.MonotonicNow,
		assertions:         make(map[string]assertionExpectation),
		registrations:      make(map[string]registrationExpectation),
		blockedCredentials: make(map[string]struct{}),
		knownCredentials:   make(map[string]struct{}),
		verifier:           pinnedWebAuthnProtocolVerifier{},
	}
	ctx, cancel := context.WithTimeout(context.Background(), credentialStateTimeout)
	active, err := opts.Store.ActiveHomeLinkCredentials(ctx, siteID)
	cancel()
	if err != nil {
		return nil, errors.New("read local passkey verifier state")
	}
	authority.rememberCredentialsLocked(active)
	return authority, nil
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
	if len(active) >= state.MaxHomeLinkActiveCredentials {
		return RegistrationChallenge{}, ErrRegistrationDenied
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
		if errors.Is(err, state.ErrHomeLinkCredentialLimit) {
			return CredentialVerifier{}, ErrRegistrationDenied
		}
		return CredentialVerifier{}, errors.New("store local passkey verifier")
	}
	a.knownCredentials[string(record.CredentialID)] = struct{}{}
	return CredentialVerifier{
		CredentialID: slices.Clone(verified.credentialID),
		PublicKey:    slices.Clone(verified.publicKey),
		Counter:      record.SignCount, Label: record.Label,
	}, nil
}

func (a *PersistentCredentialAuthority) CreateAssertion(
	ctx context.Context,
	binding AssertionExpectationBinding,
	operation ...credentialSiteOperation,
) (LocalAssertionChallenge, error) {
	if credentialSiteOperationHeld(operation, a.coordinator, false) {
		return a.createAssertionSiteLocked(ctx, binding)
	}
	a.coordinator.operations.RLock()
	defer a.coordinator.operations.RUnlock()
	return a.createAssertionSiteLocked(ctx, binding)
}

func (a *PersistentCredentialAuthority) createAssertionSiteLocked(
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
	a.rememberCredentialsLocked(active)
	active = a.filterMemoryBlockedCredentialsLocked(active)
	if len(active) > state.MaxHomeLinkActiveCredentials {
		return LocalAssertionChallenge{}, ErrCredentialUncertain
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
	operation ...credentialSiteOperation,
) (Principal, AssertionExpectationBinding, error) {
	if credentialSiteOperationHeld(operation, a.coordinator, false) {
		return a.verifyAndConsumeAssertionSiteLocked(ctx, expectationID, assertion)
	}
	a.coordinator.operations.RLock()
	defer a.coordinator.operations.RUnlock()
	return a.verifyAndConsumeAssertionSiteLocked(ctx, expectationID, assertion)
}

func (a *PersistentCredentialAuthority) verifyAndConsumeAssertionSiteLocked(
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
	if a.credentialMemoryBlockedLocked(parsed.credentialID) {
		return Principal{}, binding, ErrCredentialUncertain
	}
	if len(parsed.userHandle) != 0 &&
		subtle.ConstantTimeCompare(parsed.userHandle, record.UserHandle) != 1 {
		return Principal{}, binding, ErrWebAuthnVerification
	}
	verified, err := a.verifier.VerifyAssertion(parsed, expectation.challenge, record.PublicKey)
	if err != nil {
		return Principal{}, binding, err
	}
	wallNow := a.now().UTC().UnixMilli()
	stateCtx, cancelState := credentialStateContext(ctx)
	updated, err := a.store.ApplyHomeLinkAssertion(stateCtx, state.HomeLinkAssertionUpdate{
		SiteID: a.siteID, CredentialID: parsed.credentialID, ExpectedRevision: record.Revision,
		SignCount:      verified.signCount,
		BackupEligible: verified.backupEligible, BackupState: verified.backupState,
		UpdatedAtMS: wallNow,
	})
	cancelState()
	if err != nil {
		a.secureCredentialAfterStateErrorLocked(ctx, parsed.credentialID, wallNow)
		if requestErr := ctx.Err(); requestErr != nil {
			return Principal{}, binding, requestErr
		}
		if errors.Is(err, state.ErrHomeLinkCredentialPolicy) ||
			errors.Is(err, state.ErrHomeLinkCredentialInactive) ||
			errors.Is(err, state.ErrHomeLinkCredentialConflict) {
			return Principal{}, binding, ErrCredentialUncertain
		}
		return Principal{}, binding, errors.New("update local passkey verifier state")
	}
	if requestErr := ctx.Err(); requestErr != nil {
		return Principal{}, binding, requestErr
	}
	return Principal{CredentialID: slices.Clone(updated.CredentialID), Label: updated.Label}, binding, nil
}

func credentialStateContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), credentialStateTimeout)
}

func (a *PersistentCredentialAuthority) CredentialSite() CredentialSite {
	return CredentialSite{id: a.siteID, coordinator: a.coordinator}
}

func (a *PersistentCredentialAuthority) secureCredentialAfterStateErrorLocked(
	requestCtx context.Context,
	credentialID []byte,
	nowMS int64,
) {
	a.coordinator.blockCredential(credentialID)
	emergencyCtx, cancelEmergency := credentialStateContext(requestCtx)
	_ = a.store.EnsureHomeLinkCredentialEmergencyBlock(
		emergencyCtx, a.siteID, credentialID, nowMS,
	)
	cancelEmergency()
	ctx, cancel := credentialStateContext(requestCtx)
	_ = a.store.EnsureHomeLinkCredentialPolicyBlock(
		ctx, a.siteID, credentialID, nowMS,
	)
	cancel()
	readCtx, cancelRead := credentialStateContext(requestCtx)
	record, err := a.store.HomeLinkCredential(readCtx, a.siteID, credentialID)
	cancelRead()
	if err == nil && record.Status != state.HomeLinkCredentialActive {
		return
	}
	a.blockCredentialLocked(credentialID)
}

func (a *PersistentCredentialAuthority) credentialMemoryBlockedLocked(
	credentialID []byte,
) bool {
	if _, blocked := a.blockedCredentials[string(credentialID)]; blocked ||
		a.blockedAll {
		return true
	}
	return a.coordinator.credentialBlocked(credentialID)
}

func (a *PersistentCredentialAuthority) blockCredentialLocked(
	credentialID []byte,
) {
	if a.blockedAll {
		return
	}
	key := string(credentialID)
	if _, blocked := a.blockedCredentials[key]; blocked {
		return
	}
	if len(a.blockedCredentials) >= maxCredentialProcessBlocks {
		a.blockedAll = true
		return
	}
	a.blockedCredentials[key] = struct{}{}
}

func (a *PersistentCredentialAuthority) filterMemoryBlockedCredentialsLocked(
	records []state.HomeLinkCredentialRecord,
) []state.HomeLinkCredentialRecord {
	filtered := records[:0]
	for _, record := range records {
		if !a.credentialMemoryBlockedLocked(record.CredentialID) {
			filtered = append(filtered, record)
		}
	}
	return filtered
}

func (a *PersistentCredentialAuthority) rememberCredentialsLocked(
	records []state.HomeLinkCredentialRecord,
) {
	for _, record := range records {
		a.knownCredentials[string(record.CredentialID)] = struct{}{}
	}
}

func (a *PersistentCredentialAuthority) credentialKnownLocked(
	ctx context.Context,
	credentialID []byte,
) (bool, error) {
	if len(credentialID) == 0 || len(credentialID) > maxCredentialIDBytes {
		return false, errors.New("credential id is invalid")
	}
	if _, known := a.knownCredentials[string(credentialID)]; known {
		return true, nil
	}
	record, err := a.store.HomeLinkCredential(ctx, a.siteID, credentialID)
	if errors.Is(err, state.ErrHomeLinkCredentialNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	a.knownCredentials[string(record.CredentialID)] = struct{}{}
	return true, nil
}

func (a *PersistentCredentialAuthority) RevokeCredential(
	ctx context.Context,
	credentialID []byte,
	operation ...credentialSiteOperation,
) error {
	delegated := credentialSiteOperationHeld(operation, a.coordinator, true)
	if !delegated {
		a.coordinator.operations.Lock()
	}
	a.mu.Lock()
	known, err := a.credentialKnownLocked(ctx, credentialID)
	a.mu.Unlock()
	if err != nil {
		if !delegated {
			a.coordinator.operations.Unlock()
		}
		return errors.Join(
			errCredentialUnproven,
			errors.New("read local passkey credential"),
		)
	}
	if !known {
		if !delegated {
			a.coordinator.operations.Unlock()
		}
		return ErrCredentialUnknown
	}
	a.coordinator.blockCredential(credentialID)
	dispatches := a.coordinator.credentialDispatches(credentialID)
	err = a.revokeCredentialSiteLocked(ctx, credentialID)
	if delegated {
		return err
	}
	a.coordinator.operations.Unlock()
	return errors.Join(err, a.coordinator.waitCredentialDispatches(ctx, dispatches))
}

func credentialSiteOperationHeld(
	operations []credentialSiteOperation,
	coordinator *siteCredentialCoordinator,
	write bool,
) bool {
	return len(operations) == 1 && operations[0].coordinator == coordinator &&
		operations[0].write == write
}

func (a *PersistentCredentialAuthority) revokeCredentialSiteLocked(
	ctx context.Context,
	credentialID []byte,
) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.coordinator.blockCredential(credentialID)
	nowMS := a.now().UTC().UnixMilli()
	emergencyCtx, cancelEmergency := credentialStateContext(ctx)
	emergencyErr := a.store.EnsureHomeLinkCredentialEmergencyBlock(
		emergencyCtx, a.siteID, credentialID, nowMS,
	)
	cancelEmergency()
	if emergencyErr != nil {
		a.secureCredentialAfterStateErrorLocked(ctx, credentialID, nowMS)
		if requestErr := ctx.Err(); requestErr != nil {
			return requestErr
		}
		return errors.New("persist local passkey emergency revoke block")
	}
	stateCtx, cancelState := credentialStateContext(ctx)
	err := a.store.RevokeHomeLinkCredential(
		stateCtx, a.siteID, credentialID, nowMS,
	)
	cancelState()
	if err != nil {
		a.secureCredentialAfterStateErrorLocked(ctx, credentialID, nowMS)
		if requestErr := ctx.Err(); requestErr != nil {
			return requestErr
		}
		return errors.New("revoke local passkey credential")
	}
	if requestErr := ctx.Err(); requestErr != nil {
		return requestErr
	}
	return nil
}

func parseRegistration(raw []byte) (*protocol.ParsedCredentialCreationData, error) {
	if len(raw) == 0 || len(raw) > maxWebAuthnResponseBytes {
		return nil, ErrWebAuthnInput
	}
	if err := validateStrictJSON(raw); err != nil {
		return nil, ErrWebAuthnInput
	}
	if err := validateCredentialEnvelope(raw, true); err != nil {
		return nil, ErrWebAuthnInput
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(raw)
	if err != nil {
		return nil, ErrWebAuthnInput
	}
	if err := validateStrictJSON(parsed.Raw.AttestationResponse.ClientDataJSON); err != nil {
		return nil, ErrWebAuthnInput
	}
	if err := validateNoneAttestationCBOR(
		parsed.Raw.AttestationResponse.AttestationObject,
		parsed.Response.AttestationObject.RawAuthData,
	); err != nil {
		return nil, ErrWebAuthnInput
	}
	return parsed, nil
}

func parseAssertion(raw []byte) (*protocol.ParsedCredentialAssertionData, error) {
	if len(raw) == 0 || len(raw) > maxWebAuthnResponseBytes {
		return nil, ErrWebAuthnInput
	}
	if err := validateStrictJSON(raw); err != nil {
		return nil, ErrWebAuthnInput
	}
	if err := validateCredentialEnvelope(raw, false); err != nil {
		return nil, ErrWebAuthnInput
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(raw)
	if err != nil {
		return nil, ErrWebAuthnInput
	}
	if err := validateStrictJSON(parsed.Raw.AssertionResponse.ClientDataJSON); err != nil {
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
	if err := validateES256CredentialPublicKey(authData.AttData.CredentialPublicKey); err != nil {
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
	if err := validateES256CredentialPublicKey(storedPublicKey); err != nil {
		return verifiedPasskeyAssertion{}, ErrWebAuthnVerification
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

func validateStrictJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := readStrictJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("JSON has trailing data")
		}
		return err
	}
	return nil
}

func validateCredentialEnvelope(raw []byte, registration bool) error {
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(raw, &outer); err != nil || outer == nil {
		return errors.New("credential JSON is not an object")
	}
	id, err := requiredJSONString(outer, "id")
	if err != nil {
		return err
	}
	rawID, err := requiredJSONString(outer, "rawId")
	if err != nil {
		return err
	}
	if _, err := canonicalRawURLValue(id, false); err != nil {
		return err
	}
	if _, err := canonicalRawURLValue(rawID, false); err != nil {
		return err
	}
	if _, err := requiredJSONString(outer, "type"); err != nil {
		return err
	}
	extensionsRaw, ok := outer["clientExtensionResults"]
	if !ok {
		return errors.New("credential extension results are missing")
	}
	var extensions map[string]json.RawMessage
	if err := json.Unmarshal(extensionsRaw, &extensions); err != nil ||
		extensions == nil || len(extensions) != 0 {
		return errors.New("credential extension results must be an empty object")
	}
	responseRaw, ok := outer["response"]
	if !ok {
		return errors.New("credential response is missing")
	}
	var response map[string]json.RawMessage
	if err := json.Unmarshal(responseRaw, &response); err != nil || response == nil {
		return errors.New("credential response is not an object")
	}
	required := []string{"clientDataJSON", "authenticatorData", "signature"}
	if registration {
		required = []string{"clientDataJSON", "attestationObject"}
	}
	for _, name := range required {
		value, err := requiredJSONString(response, name)
		if err != nil {
			return err
		}
		if _, err := canonicalRawURLValue(value, false); err != nil {
			return err
		}
	}
	if !registration {
		if rawHandle, exists := response["userHandle"]; exists {
			handle, err := jsonString(rawHandle)
			if err != nil {
				return err
			}
			if _, err := canonicalRawURLValue(handle, true); err != nil {
				return err
			}
		}
	}
	return nil
}

func requiredJSONString(object map[string]json.RawMessage, name string) (string, error) {
	raw, exists := object[name]
	if !exists {
		return "", errors.New("required credential JSON field is missing")
	}
	value, err := jsonString(raw)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", errors.New("required credential JSON string is empty")
	}
	return value, nil
}

func jsonString(raw json.RawMessage) (string, error) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", errors.New("credential JSON field is not a string")
	}
	return value, nil
}

func canonicalRawURLValue(value string, allowEmpty bool) ([]byte, error) {
	if value == "" && !allowEmpty {
		return nil, errors.New("base64url value is empty")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return nil, errors.New("base64url value is not canonical")
	}
	return decoded, nil
}

func readStrictJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		names := make(map[string]struct{})
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := nameToken.(string)
			if !ok {
				return errors.New("JSON object name is invalid")
			}
			if _, exists := names[name]; exists {
				return errors.New("JSON object has a duplicate name")
			}
			names[name] = struct{}{}
			if err := readStrictJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("JSON object is not complete")
		}
	case '[':
		for decoder.More() {
			if err := readStrictJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("JSON array is not complete")
		}
	default:
		return errors.New("JSON delimiter is invalid")
	}
	return nil
}

type strictCBORReader struct {
	raw    []byte
	offset int
}

func (r *strictCBORReader) head() (byte, uint64, error) {
	if r.offset >= len(r.raw) {
		return 0, 0, io.ErrUnexpectedEOF
	}
	initial := r.raw[r.offset]
	r.offset++
	major := initial >> 5
	additional := initial & 0x1f
	switch {
	case additional < 24:
		return major, uint64(additional), nil
	case additional == 24:
		value, err := r.take(1)
		if err != nil {
			return 0, 0, err
		}
		if value[0] < 24 {
			return 0, 0, errors.New("CBOR head is not minimally encoded")
		}
		return major, uint64(value[0]), nil
	case additional == 25:
		value, err := r.take(2)
		if err != nil {
			return 0, 0, err
		}
		decoded := binary.BigEndian.Uint16(value)
		if decoded <= math.MaxUint8 {
			return 0, 0, errors.New("CBOR head is not minimally encoded")
		}
		return major, uint64(decoded), nil
	case additional == 26:
		value, err := r.take(4)
		if err != nil {
			return 0, 0, err
		}
		decoded := binary.BigEndian.Uint32(value)
		if decoded <= math.MaxUint16 {
			return 0, 0, errors.New("CBOR head is not minimally encoded")
		}
		return major, uint64(decoded), nil
	case additional == 27:
		value, err := r.take(8)
		if err != nil {
			return 0, 0, err
		}
		decoded := binary.BigEndian.Uint64(value)
		if decoded <= math.MaxUint32 {
			return 0, 0, errors.New("CBOR head is not minimally encoded")
		}
		return major, decoded, nil
	default:
		return 0, 0, errors.New("indefinite or reserved CBOR value")
	}
}

func (r *strictCBORReader) take(size uint64) ([]byte, error) {
	if size > uint64(len(r.raw)-r.offset) {
		return nil, io.ErrUnexpectedEOF
	}
	start := r.offset
	r.offset += int(size)
	return r.raw[start:r.offset], nil
}

func (r *strictCBORReader) integer() (int64, error) {
	major, value, err := r.head()
	if err != nil {
		return 0, err
	}
	if value > math.MaxInt64 {
		return 0, errors.New("CBOR integer overflows")
	}
	switch major {
	case 0:
		return int64(value), nil
	case 1:
		return -1 - int64(value), nil
	default:
		return 0, errors.New("CBOR value is not an integer")
	}
}

func (r *strictCBORReader) bytes(majorWant byte) ([]byte, error) {
	major, size, err := r.head()
	if err != nil {
		return nil, err
	}
	if major != majorWant {
		return nil, errors.New("CBOR value has the wrong type")
	}
	return r.take(size)
}

func validateNoneAttestationCBOR(raw, parsedAuthData []byte) error {
	reader := strictCBORReader{raw: raw}
	major, count, err := reader.head()
	if err != nil || major != 5 || count != 3 {
		return errors.New("attestation object is not the required map")
	}
	seen := make(map[string]struct{}, 3)
	for range count {
		nameRaw, err := reader.bytes(3)
		if err != nil {
			return err
		}
		name := string(nameRaw)
		if _, exists := seen[name]; exists {
			return errors.New("attestation object has a duplicate field")
		}
		seen[name] = struct{}{}
		switch name {
		case "fmt":
			value, err := reader.bytes(3)
			if err != nil || string(value) != "none" {
				return errors.New("attestation format is not none")
			}
		case "authData":
			value, err := reader.bytes(2)
			if err != nil || !bytes.Equal(value, parsedAuthData) {
				return errors.New("attestation auth data does not match")
			}
		case "attStmt":
			valueMajor, valueCount, err := reader.head()
			if err != nil || valueMajor != 5 || valueCount != 0 {
				return errors.New("none attestation statement is not an empty map")
			}
		default:
			return errors.New("attestation object has an unknown field")
		}
	}
	if len(seen) != 3 || reader.offset != len(raw) {
		return errors.New("attestation object has missing or trailing data")
	}
	return nil
}

func validateES256CredentialPublicKey(raw []byte) error {
	reader := strictCBORReader{raw: raw}
	major, count, err := reader.head()
	if err != nil || major != 5 || count != 5 {
		return errors.New("credential public key is not the required COSE map")
	}
	seen := make(map[int64]struct{}, 5)
	var keyType, algorithm, curve int64
	var x, y []byte
	for range count {
		label, err := reader.integer()
		if err != nil {
			return err
		}
		if _, exists := seen[label]; exists {
			return errors.New("credential public key has a duplicate label")
		}
		seen[label] = struct{}{}
		switch label {
		case 1:
			keyType, err = reader.integer()
		case 3:
			algorithm, err = reader.integer()
		case -1:
			curve, err = reader.integer()
		case -2:
			x, err = reader.bytes(2)
		case -3:
			y, err = reader.bytes(2)
		default:
			return errors.New("credential public key has an unknown label")
		}
		if err != nil {
			return err
		}
	}
	if reader.offset != len(raw) || len(seen) != 5 ||
		keyType != 2 || algorithm != -7 || curve != 1 ||
		len(x) != 32 || len(y) != 32 {
		return errors.New("credential public key is not EC2 ES256 P-256")
	}
	if !elliptic.P256().IsOnCurve(new(big.Int).SetBytes(x), new(big.Int).SetBytes(y)) {
		return errors.New("credential public key point is not on P-256")
	}
	return nil
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
