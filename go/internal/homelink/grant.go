package homelink

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/gatewayidentity"
)

type Scope string

const (
	ScopePasskeyEnroll     Scope = "ftw.passkey.enroll"
	ScopeHealthRead        Scope = "ftw.health.read"
	ScopePlanRead          Scope = "ftw.plan.read"
	ScopeEnergyAssetsRead  Scope = "ftw.energy.assets.read"
	ScopeEnergyHistoryRead Scope = "ftw.energy.history.read"
)

const (
	PairingGrantMaxTTL = 10 * time.Minute
	AccessGrantMaxTTL  = 5 * time.Minute
	grantTokenBytes    = 32
	maxMonotonicTime   = time.Duration(1<<63 - 1)
)

var monotonicEpoch = time.Now()

func defaultMonotonicNow() time.Duration {
	return time.Since(monotonicEpoch)
}

var (
	ErrRemoteDisabled    = errors.New("Home Link is disabled")
	ErrInvalidGrant      = errors.New("grant is invalid")
	ErrGrantExpired      = errors.New("grant has expired")
	ErrGrantRevoked      = errors.New("grant has been revoked")
	ErrGrantConsumed     = errors.New("grant has already been used")
	ErrWrongSite         = errors.New("grant belongs to another gateway")
	ErrWrongScope        = errors.New("grant does not allow this scope")
	ErrWrongPurpose      = errors.New("grant has another purpose")
	ErrCredentialRevoked = errors.New("credential has been revoked")
)

type GrantPurpose string

const (
	GrantPurposePairing GrantPurpose = "pairing"
	GrantPurposeAccess  GrantPurpose = "access"
)

// Grant is returned once to the local caller and permits exactly one use. The
// manager stores only its SHA-256 hash. Refresh requires another local passkey
// check and a new grant.
type Grant struct {
	Token     string
	GatewayID string
	Purpose   GrantPurpose
	Scope     Scope
	ExpiresAt time.Time
}

type GrantManagerOptions struct {
	Enabled             bool
	Random              io.Reader
	Now                 func() time.Time
	MonotonicNow        func() time.Duration
	CredentialAuthority CredentialAuthority
	ReadDispatcher      ReadDispatcher
	PairingAuthorizer   PairingAuthorizer
}

type GrantManager struct {
	mu                 sync.Mutex
	enabled            bool
	gatewayID          string
	random             io.Reader
	now                func() time.Time
	monotonicNow       func() time.Duration
	authority          CredentialAuthority
	readDispatcher     ReadDispatcher
	pairingAuthorizer  PairingAuthorizer
	assertionSession   AssertionSession
	clock              monotonicClockState
	records            map[[sha256.Size]byte]*grantRecord
	blockedCredentials map[string]struct{}
}

type monotonicClockState struct {
	highWater time.Duration
	sampled   bool
	invalid   bool
}

func (s *monotonicClockState) sample(now func() time.Duration) (time.Duration, error) {
	if s.invalid {
		return 0, ErrMonotonicClock
	}
	value := now()
	if value < 0 || s.sampled && value < s.highWater {
		s.invalid = true
		return value, ErrMonotonicClock
	}
	if !s.sampled || value > s.highWater {
		s.highWater = value
	}
	s.sampled = true
	return value, nil
}

func (s *monotonicClockState) deadline(now, ttl time.Duration) (time.Duration, error) {
	if s.invalid || ttl <= 0 || now > maxMonotonicTime-ttl {
		s.invalid = true
		return 0, ErrMonotonicClock
	}
	return now + ttl, nil
}

type grantRecord struct {
	gatewayID string
	purpose   GrantPurpose
	scope     Scope
	principal Principal
	deadline  time.Duration
	revoked   bool
	consumed  bool
}

func NewGrantManager(gatewayID string, opts GrantManagerOptions) (*GrantManager, error) {
	normalized, err := gatewayidentity.NormalizeGatewayID(gatewayID)
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
	if opts.CredentialAuthority == nil {
		return nil, errors.New("local credential authority is missing")
	}
	var assertionSession AssertionSession
	if _, err := io.ReadFull(rand.Reader, assertionSession.id[:]); err != nil {
		return nil, fmt.Errorf("create local assertion session: %w", err)
	}
	return &GrantManager{
		enabled: opts.Enabled, gatewayID: normalized, random: opts.Random,
		now: opts.Now, monotonicNow: opts.MonotonicNow, authority: opts.CredentialAuthority,
		readDispatcher:     opts.ReadDispatcher,
		pairingAuthorizer:  opts.PairingAuthorizer,
		assertionSession:   assertionSession,
		records:            make(map[[sha256.Size]byte]*grantRecord),
		blockedCredentials: make(map[string]struct{}),
	}, nil
}

// IssuePairing asks the local authorizer to consume a one-time LAN proof
// before it creates an enrollment grant.
func (m *GrantManager) IssuePairing(ctx context.Context, proof LocalPairingProof, ttl time.Duration) (Grant, error) {
	if !m.enabled {
		return Grant{}, ErrRemoteDisabled
	}
	if ttl <= 0 || ttl > PairingGrantMaxTTL {
		return Grant{}, fmt.Errorf("grant lifetime must be from 1ns through %s", PairingGrantMaxTTL)
	}
	if m.pairingAuthorizer == nil {
		return Grant{}, errors.New("local pairing authorizer is missing")
	}
	if err := m.pairingAuthorizer.AuthorizeLocalPairing(ctx, proof); err != nil {
		return Grant{}, fmt.Errorf("authorize local pairing: %w", err)
	}
	return m.issue(GrantPurposePairing, Principal{}, ScopePasskeyEnroll, ttl, PairingGrantMaxTTL)
}

// BeginLocalAssertion asks the local credential authority to create and retain
// an assertion expectation. The returned ID is only an opaque lookup key.
func (m *GrantManager) BeginLocalAssertion(ctx context.Context) (LocalAssertionChallenge, error) {
	if !m.enabled {
		return LocalAssertionChallenge{}, ErrRemoteDisabled
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	monotonicNow, err := m.clock.sample(m.monotonicNow)
	if err != nil {
		return LocalAssertionChallenge{}, err
	}
	deadline, err := m.clock.deadline(monotonicNow, AssertionExpectationMaxAge)
	if err != nil {
		return LocalAssertionChallenge{}, err
	}
	binding := AssertionExpectationBinding{session: m.assertionSession, deadline: deadline}
	challenge, err := m.authority.CreateAssertion(ctx, binding)
	if err != nil {
		return LocalAssertionChallenge{}, fmt.Errorf("create local passkey assertion: %w", err)
	}
	challenge.Challenge = slices.Clone(challenge.Challenge)
	if err := challenge.validate(); err != nil {
		return LocalAssertionChallenge{}, err
	}
	return challenge, nil
}

// IssueOneUseAccess serializes local verification with durable credential
// revocation, then returns a grant for one read scope and one use.
func (m *GrantManager) IssueOneUseAccess(
	ctx context.Context,
	challengeID string,
	assertion PasskeyAssertion,
	scope Scope,
	ttl time.Duration,
) (Grant, error) {
	if !m.enabled {
		return Grant{}, ErrRemoteDisabled
	}
	if err := validateAssertionChallengeID(challengeID); err != nil {
		return Grant{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	principal, binding, err := m.authority.VerifyAndConsumeAssertion(ctx, challengeID, assertion)
	if err != nil {
		return Grant{}, fmt.Errorf("verify local passkey: %w", err)
	}
	monotonicNow, err := m.clock.sample(m.monotonicNow)
	if err != nil {
		return Grant{}, err
	}
	if err := binding.validate(m.assertionSession, monotonicNow); err != nil {
		return Grant{}, err
	}
	principal = clonePrincipal(principal)
	if err := principal.validate(); err != nil {
		return Grant{}, err
	}
	if err := validateReadScope(scope); err != nil {
		return Grant{}, err
	}
	if ttl <= 0 || ttl > AccessGrantMaxTTL {
		return Grant{}, fmt.Errorf("grant lifetime must be from 1ns through %s", AccessGrantMaxTTL)
	}
	return m.issueLocked(GrantPurposeAccess, principal, scope, ttl, AccessGrantMaxTTL)
}

func (m *GrantManager) issue(purpose GrantPurpose, principal Principal, scope Scope, ttl, maxTTL time.Duration) (Grant, error) {
	if !m.enabled {
		return Grant{}, ErrRemoteDisabled
	}
	if ttl <= 0 || ttl > maxTTL {
		return Grant{}, fmt.Errorf("grant lifetime must be from 1ns through %s", maxTTL)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.issueLocked(purpose, principal, scope, ttl, maxTTL)
}

func (m *GrantManager) issueLocked(
	purpose GrantPurpose,
	principal Principal,
	scope Scope,
	ttl, maxTTL time.Duration,
) (Grant, error) {
	if ttl <= 0 || ttl > maxTTL {
		return Grant{}, fmt.Errorf("grant lifetime must be from 1ns through %s", maxTTL)
	}
	monotonicNow, err := m.clock.sample(m.monotonicNow)
	if err != nil {
		return Grant{}, err
	}
	deadline, err := m.clock.deadline(monotonicNow, ttl)
	if err != nil {
		return Grant{}, err
	}
	wallNow := m.now().UTC()
	expiresAt := wallNow.Add(ttl)
	m.pruneLocked(monotonicNow)
	if purpose == GrantPurposeAccess {
		if _, revoked := m.blockedCredentials[string(principal.CredentialID)]; revoked {
			return Grant{}, ErrCredentialRevoked
		}
	}
	for range 4 {
		raw := make([]byte, grantTokenBytes)
		if _, err := io.ReadFull(m.random, raw); err != nil {
			return Grant{}, fmt.Errorf("create grant: %w", err)
		}
		hash := sha256.Sum256(raw)
		if _, exists := m.records[hash]; exists {
			continue
		}
		m.records[hash] = &grantRecord{
			gatewayID: m.gatewayID, purpose: purpose, scope: scope,
			principal: clonePrincipal(principal), deadline: deadline,
		}
		return Grant{
			Token: base64.RawURLEncoding.EncodeToString(raw), GatewayID: m.gatewayID,
			Purpose: purpose, Scope: scope, ExpiresAt: expiresAt,
		}, nil
	}
	return Grant{}, errors.New("could not create a unique grant")
}

// ConsumePairing consumes only an enrollment grant and returns no read
// authorization.
func (m *GrantManager) ConsumePairing(token, gatewayID string) error {
	_, err := m.consume(token, gatewayID, GrantPurposePairing, ScopePasskeyEnroll)
	return err
}

func (m *GrantManager) consume(token, gatewayID string, purpose GrantPurpose, scope Scope) (grantRecord, error) {
	if !m.enabled {
		return grantRecord{}, ErrRemoteDisabled
	}
	hash, err := grantHash(token)
	if err != nil {
		return grantRecord{}, ErrInvalidGrant
	}
	normalized, err := gatewayidentity.NormalizeGatewayID(gatewayID)
	if err != nil {
		return grantRecord{}, ErrWrongSite
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.records[hash]
	if !ok {
		return grantRecord{}, ErrInvalidGrant
	}
	if record.revoked {
		return grantRecord{}, ErrGrantRevoked
	}
	if record.consumed {
		return grantRecord{}, ErrGrantConsumed
	}
	monotonicNow, err := m.clock.sample(m.monotonicNow)
	if err != nil {
		return grantRecord{}, err
	}
	if monotonicNow >= record.deadline {
		return grantRecord{}, ErrGrantExpired
	}
	if normalized != record.gatewayID {
		return grantRecord{}, ErrWrongSite
	}
	if record.purpose != purpose {
		return grantRecord{}, ErrWrongPurpose
	}
	if record.scope != scope {
		return grantRecord{}, ErrWrongScope
	}
	record.consumed = true
	result := *record
	result.principal = clonePrincipal(record.principal)
	return result, nil
}

func (m *GrantManager) Revoke(token string) error {
	hash, err := grantHash(token)
	if err != nil {
		return ErrInvalidGrant
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.records[hash]
	if !ok {
		return ErrInvalidGrant
	}
	record.revoked = true
	return nil
}

// RevokeCredential serializes with grant issue and consumption. The local
// authority commits durable revoked or uncertain state first. The manager then
// blocks this credential and all matching grants before returning any result.
func (m *GrantManager) RevokeCredential(ctx context.Context, credentialID []byte) error {
	if len(credentialID) == 0 || len(credentialID) > maxCredentialIDBytes {
		return errors.New("credential id is invalid")
	}
	id := slices.Clone(credentialID)
	m.mu.Lock()
	defer m.mu.Unlock()
	authorityErr := m.authority.RevokeCredential(ctx, id)
	m.blockedCredentials[string(id)] = struct{}{}
	for _, record := range m.records {
		if record.purpose == GrantPurposeAccess && bytes.Equal(record.principal.CredentialID, id) {
			record.revoked = true
		}
	}
	if authorityErr != nil {
		return fmt.Errorf("revoke local credential: %w", authorityErr)
	}
	return nil
}

func (m *GrantManager) pruneLocked(monotonicNow time.Duration) {
	for hash, record := range m.records {
		if monotonicNow >= record.deadline && monotonicNow-record.deadline > PairingGrantMaxTTL {
			delete(m.records, hash)
		}
	}
}

func grantHash(token string) ([sha256.Size]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != grantTokenBytes {
		return [sha256.Size]byte{}, ErrInvalidGrant
	}
	return sha256.Sum256(raw), nil
}

func validateReadScope(scope Scope) error {
	if _, ok := readTargets[scope]; !ok {
		return fmt.Errorf("scope %q is not read-only", scope)
	}
	return nil
}

func clonePrincipal(principal Principal) Principal {
	return Principal{CredentialID: slices.Clone(principal.CredentialID), Label: principal.Label}
}
