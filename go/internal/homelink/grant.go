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
	dispatchMu sync.Mutex
	dispatchID uint64
	dispatches map[uint64]*credentialDispatch
}

type credentialDispatch struct {
	id           uint64
	credentialID string
	cancel       context.CancelFunc
	done         chan struct{}
}

type credentialDispatchContext struct {
	coordinator *siteCredentialCoordinator
	id          uint64
}

type credentialDispatchContextKey struct{}

type credentialSiteLockMode uint8

const (
	credentialSiteReadLock credentialSiteLockMode = iota + 1
	credentialSiteWriteLock
)

type credentialSiteLockContext struct {
	coordinator *siteCredentialCoordinator
	mode        credentialSiteLockMode
}

type credentialSiteLockContextKey struct{}

// CredentialSite binds a credential authority to one normalized gateway and
// its process-wide operation fence. CredentialAuthority includes this method,
// so a decorator that embeds the interface keeps the binding.
type CredentialSite struct {
	id          string
	coordinator *siteCredentialCoordinator
}

// ID returns the normalized gateway identity owned by the authority.
func (s CredentialSite) ID() string {
	return s.id
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
		coordinator = &siteCredentialCoordinator{
			blocked:    make(map[string]struct{}),
			dispatches: make(map[uint64]*credentialDispatch),
		}
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

func (c *siteCredentialCoordinator) startDispatch(
	parent context.Context,
	credentialID []byte,
) (context.Context, *credentialDispatch) {
	ctx, cancel := context.WithCancel(parent)
	c.dispatchMu.Lock()
	c.dispatchID++
	dispatch := &credentialDispatch{
		id: c.dispatchID, credentialID: string(credentialID),
		cancel: cancel, done: make(chan struct{}),
	}
	c.dispatches[dispatch.id] = dispatch
	c.dispatchMu.Unlock()
	return context.WithValue(ctx, credentialDispatchContextKey{}, credentialDispatchContext{
		coordinator: c,
		id:          dispatch.id,
	}), dispatch
}

func (c *siteCredentialCoordinator) finishDispatch(dispatch *credentialDispatch) {
	dispatch.cancel()
	c.dispatchMu.Lock()
	if current := c.dispatches[dispatch.id]; current == dispatch {
		delete(c.dispatches, dispatch.id)
		close(dispatch.done)
	}
	c.dispatchMu.Unlock()
}

func (c *siteCredentialCoordinator) credentialDispatches(
	credentialID []byte,
) []*credentialDispatch {
	c.dispatchMu.Lock()
	defer c.dispatchMu.Unlock()
	var dispatches []*credentialDispatch
	for _, dispatch := range c.dispatches {
		if dispatch.credentialID == string(credentialID) {
			dispatches = append(dispatches, dispatch)
		}
	}
	return dispatches
}

func (c *siteCredentialCoordinator) waitCredentialDispatches(
	ctx context.Context,
	dispatches []*credentialDispatch,
) error {
	ownID := uint64(0)
	if own, ok := ctx.Value(credentialDispatchContextKey{}).(credentialDispatchContext); ok &&
		own.coordinator == c {
		ownID = own.id
	}
	ownDispatch := false
	for _, dispatch := range dispatches {
		if dispatch.id == ownID {
			ownDispatch = true
			dispatch.cancel()
		}
	}
	timer := time.NewTimer(credentialStateTimeout)
	defer timer.Stop()
	for index, dispatch := range dispatches {
		if dispatch.id == ownID {
			continue
		}
		select {
		case <-dispatch.done:
		case <-ctx.Done():
			for _, pending := range dispatches[index:] {
				pending.cancel()
			}
			return ctx.Err()
		case <-timer.C:
			for _, pending := range dispatches[index:] {
				pending.cancel()
			}
			return ErrCredentialDispatchBusy
		}
	}
	if ownDispatch {
		return ErrCredentialDispatchBusy
	}
	return nil
}

func withCredentialSiteLock(
	ctx context.Context,
	coordinator *siteCredentialCoordinator,
	mode credentialSiteLockMode,
) context.Context {
	return context.WithValue(ctx, credentialSiteLockContextKey{}, credentialSiteLockContext{
		coordinator: coordinator,
		mode:        mode,
	})
}

func credentialSiteLockHeld(
	ctx context.Context,
	coordinator *siteCredentialCoordinator,
	mode credentialSiteLockMode,
) bool {
	lock, ok := ctx.Value(credentialSiteLockContextKey{}).(credentialSiteLockContext)
	return ok && lock.coordinator == coordinator && lock.mode == mode
}

var (
	ErrRemoteDisabled         = errors.New("Home Link is disabled")
	ErrInvalidGrant           = errors.New("grant is invalid")
	ErrGrantExpired           = errors.New("grant has expired")
	ErrGrantRevoked           = errors.New("grant has been revoked")
	ErrGrantConsumed          = errors.New("grant has already been used")
	ErrWrongSite              = errors.New("grant belongs to another gateway")
	ErrWrongScope             = errors.New("grant does not allow this scope")
	ErrWrongPurpose           = errors.New("grant has another purpose")
	ErrCredentialRevoked      = errors.New("credential has been revoked")
	ErrCredentialDispatchBusy = errors.New("credential read dispatch did not stop")
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
	site := opts.CredentialAuthority.CredentialSite()
	if site.id != normalized || site.coordinator == nil {
		return nil, errors.New("local credential authority belongs to another gateway")
	}
	coordinator := site.coordinator
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
	challenge, err := m.authority.CreateAssertion(
		withCredentialSiteLock(ctx, m.coordinator, credentialSiteReadLock),
		binding,
	)
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
	principal, binding, err := m.authority.VerifyAndConsumeAssertion(
		withCredentialSiteLock(ctx, m.coordinator, credentialSiteReadLock),
		challengeID, assertion,
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
	ctx context.Context,
	token string,
	gatewayID string,
	scope Scope,
) (grantRecord, context.Context, *credentialDispatch, error) {
	m.coordinator.operations.RLock()
	record, err := m.consumeSiteLocked(token, gatewayID, GrantPurposeAccess, scope)
	if err != nil {
		m.coordinator.operations.RUnlock()
		return grantRecord{}, nil, nil, err
	}
	dispatchCtx, dispatch := m.coordinator.startDispatch(
		ctx, record.principal.CredentialID,
	)
	m.coordinator.operations.RUnlock()
	return record, dispatchCtx, dispatch, nil
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
	m.coordinator.blockCredential(id)
	dispatches := m.coordinator.credentialDispatches(id)
	authorityErr := m.authority.RevokeCredential(
		withCredentialSiteLock(ctx, m.coordinator, credentialSiteWriteLock),
		id,
	)
	m.coordinator.operations.Unlock()
	m.mu.Lock()
	m.blockedCredentials[string(id)] = struct{}{}
	for _, record := range m.records {
		if record.purpose == GrantPurposeAccess && bytes.Equal(record.principal.CredentialID, id) {
			record.revoked = true
		}
	}
	m.mu.Unlock()
	waitErr := m.coordinator.waitCredentialDispatches(ctx, dispatches)
	if authorityErr != nil {
		authorityErr = fmt.Errorf("revoke local credential: %w", authorityErr)
	}
	return errors.Join(authorityErr, waitErr)
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
