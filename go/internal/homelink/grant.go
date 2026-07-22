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
	ScopeStatusRead        Scope = "ftw.status.read"
	ScopeHealthRead        Scope = "ftw.health.read"
	ScopePlanRead          Scope = "ftw.plan.read"
	ScopeEnergyAssetsRead  Scope = "ftw.energy.assets.read"
	ScopeEnergyHistoryRead Scope = "ftw.energy.history.read"
)

const (
	PairingGrantMaxTTL = 10 * time.Minute
	AccessGrantMaxTTL  = 5 * time.Minute
	grantTokenBytes    = 32
)

var (
	ErrRemoteDisabled = errors.New("Home Link is disabled")
	ErrInvalidGrant   = errors.New("grant is invalid")
	ErrGrantExpired   = errors.New("grant has expired")
	ErrGrantRevoked   = errors.New("grant has been revoked")
	ErrGrantConsumed  = errors.New("grant has already been used")
	ErrWrongSite      = errors.New("grant belongs to another gateway")
	ErrWrongScope     = errors.New("grant does not allow this scope")
	ErrWrongPurpose   = errors.New("grant has another purpose")
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

type Authorization struct {
	gatewayID string
	purpose   GrantPurpose
	scope     Scope
	principal Principal
}

func (a Authorization) GatewayID() string     { return a.gatewayID }
func (a Authorization) Purpose() GrantPurpose { return a.purpose }
func (a Authorization) Scope() Scope          { return a.scope }
func (a Authorization) Principal() Principal  { return clonePrincipal(a.principal) }
func (a Authorization) valid() error {
	if a.purpose != GrantPurposeAccess {
		return errors.New("authorization is not for a remote read")
	}
	if _, err := gatewayidentity.NormalizeGatewayID(a.gatewayID); err != nil {
		return err
	}
	if err := validateReadScope(a.scope); err != nil {
		return err
	}
	return a.principal.validate()
}

type GrantManagerOptions struct {
	Enabled           bool
	Random            io.Reader
	Now               func() time.Time
	Verifier          AssertionVerifier
	PairingAuthorizer PairingAuthorizer
}

type GrantManager struct {
	mu                sync.Mutex
	enabled           bool
	gatewayID         string
	random            io.Reader
	now               func() time.Time
	verifier          AssertionVerifier
	pairingAuthorizer PairingAuthorizer
	records           map[[sha256.Size]byte]*grantRecord
}

type grantRecord struct {
	gatewayID string
	purpose   GrantPurpose
	scope     Scope
	principal Principal
	expiresAt time.Time
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
	return &GrantManager{
		enabled: opts.Enabled, gatewayID: normalized, random: opts.Random,
		now: opts.Now, verifier: opts.Verifier, pairingAuthorizer: opts.PairingAuthorizer,
		records: make(map[[sha256.Size]byte]*grantRecord),
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

// IssueOneUseAccess asks the configured local verifier to check one passkey
// assertion, then returns a grant for exactly one read scope and one use.
func (m *GrantManager) IssueOneUseAccess(
	ctx context.Context,
	expectation AssertionExpectation,
	assertion PasskeyAssertion,
	scope Scope,
	ttl time.Duration,
) (Grant, error) {
	if !m.enabled {
		return Grant{}, ErrRemoteDisabled
	}
	if err := validateReadScope(scope); err != nil {
		return Grant{}, err
	}
	if ttl <= 0 || ttl > AccessGrantMaxTTL {
		return Grant{}, fmt.Errorf("grant lifetime must be from 1ns through %s", AccessGrantMaxTTL)
	}
	if m.verifier == nil {
		return Grant{}, errors.New("local assertion verifier is missing")
	}
	principal, err := m.verifier.VerifyAssertion(ctx, expectation, assertion)
	if err != nil {
		return Grant{}, fmt.Errorf("verify local passkey: %w", err)
	}
	if err := principal.validate(); err != nil {
		return Grant{}, err
	}
	return m.issue(GrantPurposeAccess, principal, scope, ttl, AccessGrantMaxTTL)
}

func (m *GrantManager) issue(purpose GrantPurpose, principal Principal, scope Scope, ttl, maxTTL time.Duration) (Grant, error) {
	if !m.enabled {
		return Grant{}, ErrRemoteDisabled
	}
	if ttl <= 0 || ttl > maxTTL {
		return Grant{}, fmt.Errorf("grant lifetime must be from 1ns through %s", maxTTL)
	}
	now := m.now().UTC()
	expiresAt := now.Add(ttl)

	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(now)
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
			principal: clonePrincipal(principal), expiresAt: expiresAt,
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

// VerifyAndConsumeAccess consumes only a read grant and returns the matching
// unforgeable Core authorization.
func (m *GrantManager) VerifyAndConsumeAccess(token, gatewayID string, scope Scope) (Authorization, error) {
	if err := validateReadScope(scope); err != nil {
		return Authorization{}, err
	}
	record, err := m.consume(token, gatewayID, GrantPurposeAccess, scope)
	if err != nil {
		return Authorization{}, err
	}
	return Authorization{
		gatewayID: record.gatewayID, purpose: record.purpose, scope: scope,
		principal: clonePrincipal(record.principal),
	}, nil
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
	now := m.now().UTC()

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
	if !now.Before(record.expiresAt) {
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

func (m *GrantManager) RevokeCredential(credentialID []byte) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	revoked := 0
	for _, record := range m.records {
		if record.purpose == GrantPurposeAccess && bytes.Equal(record.principal.CredentialID, credentialID) && !record.revoked {
			record.revoked = true
			revoked++
		}
	}
	return revoked
}

func (m *GrantManager) pruneLocked(now time.Time) {
	for hash, record := range m.records {
		if now.After(record.expiresAt.Add(PairingGrantMaxTTL)) {
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
