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

type siteCredentialCoordinator struct {
	operations sync.RWMutex
	blockedMu  sync.RWMutex
	blocked    map[string]struct{}
}

var credentialCoordinators = struct {
	sync.Mutex
	sites map[string]*siteCredentialCoordinator
}{
	sites: make(map[string]*siteCredentialCoordinator),
}

func siteCredentialCoordinatorFor(siteID string) *siteCredentialCoordinator {
	credentialCoordinators.Lock()
	defer credentialCoordinators.Unlock()
	coordinator := credentialCoordinators.sites[siteID]
	if coordinator == nil {
		coordinator = &siteCredentialCoordinator{blocked: make(map[string]struct{})}
		credentialCoordinators.sites[siteID] = coordinator
	}
	return coordinator
}

func forgetSiteCredentialCoordinatorForTest(siteID string) {
	credentialCoordinators.Lock()
	delete(credentialCoordinators.sites, siteID)
	credentialCoordinators.Unlock()
}

func (c *siteCredentialCoordinator) blockCredential(credentialID []byte) {
	c.blockedMu.Lock()
	c.blocked[string(credentialID)] = struct{}{}
	c.blockedMu.Unlock()
}

func (c *siteCredentialCoordinator) credentialBlocked(credentialID []byte) bool {
	c.blockedMu.RLock()
	_, blocked := c.blocked[string(credentialID)]
	c.blockedMu.RUnlock()
	return blocked
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
	coordinator        *siteCredentialCoordinator
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

type siteCoordinatedCredentialAuthority interface {
	credentialSiteID() string
	credentialSiteCoordinator() *siteCredentialCoordinator
	createAssertionSiteLocked(
		context.Context,
		AssertionExpectationBinding,
	) (LocalAssertionChallenge, error)
	verifyAndConsumeAssertionSiteLocked(
		context.Context,
		string,
		PasskeyAssertion,
	) (Principal, AssertionExpectationBinding, error)
	revokeCredentialSiteLocked(context.Context, []byte) error
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
	coordinator := siteCredentialCoordinatorFor(normalized)
	if coordinated, ok := opts.CredentialAuthority.(siteCoordinatedCredentialAuthority); ok {
		if coordinated.credentialSiteID() != normalized {
			return nil, errors.New("local credential authority belongs to another gateway")
		}
		coordinator = coordinated.credentialSiteCoordinator()
	}
	var assertionSession AssertionSession
	if _, err := io.ReadFull(rand.Reader, assertionSession.id[:]); err != nil {
		return nil, fmt.Errorf("create local assertion session: %w", err)
	}
	return &GrantManager{
		enabled: opts.Enabled, gatewayID: normalized, coordinator: coordinator, random: opts.Random,
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
	m.coordinator.operations.RLock()
	defer m.coordinator.operations.RUnlock()
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
	challenge, err := m.createAssertionSiteLocked(ctx, binding)
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

	m.coordinator.operations.RLock()
	defer m.coordinator.operations.RUnlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	principal, binding, err := m.verifyAndConsumeAssertionSiteLocked(
		ctx, challengeID, assertion,
	)
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
		if m.coordinator.credentialBlocked(principal.CredentialID) {
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
	m.coordinator.operations.RLock()
	defer m.coordinator.operations.RUnlock()
	return m.consumeSiteLocked(token, gatewayID, purpose, scope)
}

func (m *GrantManager) consumeForDispatch(
	token string,
	gatewayID string,
	scope Scope,
) (grantRecord, func(), error) {
	m.coordinator.operations.RLock()
	record, err := m.consumeSiteLocked(token, gatewayID, GrantPurposeAccess, scope)
	if err != nil {
		m.coordinator.operations.RUnlock()
		return grantRecord{}, nil, err
	}
	return record, m.coordinator.operations.RUnlock, nil
}

func (m *GrantManager) consumeSiteLocked(
	token string,
	gatewayID string,
	purpose GrantPurpose,
	scope Scope,
) (grantRecord, error) {
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
	if record.purpose == GrantPurposeAccess {
		if m.coordinator.credentialBlocked(record.principal.CredentialID) {
			return grantRecord{}, ErrCredentialRevoked
		}
		if err := record.principal.validate(); err != nil {
			return grantRecord{}, err
		}
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

// RevokeCredential serializes across every live manager for this site. The
// local authority commits durable revoked or uncertain state first. The
// shared coordinator then blocks this credential before any later grant can
// be issued or consumed.
func (m *GrantManager) RevokeCredential(ctx context.Context, credentialID []byte) error {
	if len(credentialID) == 0 || len(credentialID) > maxCredentialIDBytes {
		return errors.New("credential id is invalid")
	}
	id := slices.Clone(credentialID)
	m.coordinator.operations.Lock()
	authorityErr := m.revokeCredentialSiteLocked(ctx, id)
	m.coordinator.blockCredential(id)
	m.coordinator.operations.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
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

func (m *GrantManager) createAssertionSiteLocked(
	ctx context.Context,
	binding AssertionExpectationBinding,
) (LocalAssertionChallenge, error) {
	if authority, ok := m.authority.(siteCoordinatedCredentialAuthority); ok {
		return authority.createAssertionSiteLocked(ctx, binding)
	}
	return m.authority.CreateAssertion(ctx, binding)
}

func (m *GrantManager) verifyAndConsumeAssertionSiteLocked(
	ctx context.Context,
	challengeID string,
	assertion PasskeyAssertion,
) (Principal, AssertionExpectationBinding, error) {
	if authority, ok := m.authority.(siteCoordinatedCredentialAuthority); ok {
		return authority.verifyAndConsumeAssertionSiteLocked(ctx, challengeID, assertion)
	}
	return m.authority.VerifyAndConsumeAssertion(ctx, challengeID, assertion)
}

func (m *GrantManager) revokeCredentialSiteLocked(
	ctx context.Context,
	credentialID []byte,
) error {
	if authority, ok := m.authority.(siteCoordinatedCredentialAuthority); ok {
		return authority.revokeCredentialSiteLocked(ctx, credentialID)
	}
	return m.authority.RevokeCredential(ctx, credentialID)
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
