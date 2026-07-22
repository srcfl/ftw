package homelink

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/gatewayidentity"
)

const (
	MachineChallengeMaxTTL = 30 * time.Second
	machineChallengeBytes  = 32
	machineChallengeDomain = "ftw-home-link-machine-challenge/v1"
)

type RouteStatus string

const (
	RouteDisconnected RouteStatus = "disconnected"
	RouteConnected    RouteStatus = "connected"
)

// RouteRecord is the relay's full record. It has no user or passkey
// field. ActiveRoute is an in-memory routing handle, not a credential.
type RouteRecord struct {
	Alias       string      `json:"alias"`
	GatewayID   string      `json:"gateway_id"`
	PublicKey   []byte      `json:"public_key"`
	ActiveRoute string      `json:"active_route,omitempty"`
	Status      RouteStatus `json:"status"`
	FirstSeenAt time.Time   `json:"first_seen_at"`
	LastSeenAt  time.Time   `json:"last_seen_at"`
}

func (r RouteRecord) Validate() error {
	normalized, err := gatewayidentity.NormalizeGatewayID(r.GatewayID)
	if err != nil {
		return err
	}
	wantAlias, err := gatewayidentity.ThreeWordName(normalized)
	if err != nil {
		return err
	}
	if r.Alias != wantAlias {
		return errors.New("relay alias does not match gateway id")
	}
	if err := gatewayidentity.ValidatePublicKey(r.PublicKey); err != nil {
		return err
	}
	if r.Status != RouteConnected && r.Status != RouteDisconnected {
		return errors.New("relay status is invalid")
	}
	if r.Status == RouteConnected && r.ActiveRoute == "" {
		return errors.New("connected relay record has no active route")
	}
	if r.Status == RouteDisconnected && r.ActiveRoute != "" {
		return errors.New("disconnected relay record has an active route")
	}
	if r.FirstSeenAt.IsZero() || r.LastSeenAt.Before(r.FirstSeenAt) {
		return errors.New("relay times are invalid")
	}
	return nil
}

// MachineChallenge is internal relay-auth state, not a frozen wire envelope.
type MachineChallenge struct {
	Nonce     string
	GatewayID string
	ExpiresAt time.Time
}

type ChallengeManagerOptions struct {
	Enabled bool
	Random  io.Reader
	Now     func() time.Time
}

type ChallengeManager struct {
	mu      sync.Mutex
	enabled bool
	random  io.Reader
	now     func() time.Time
	records map[[sha256.Size]byte]*challengeRecord
}

type challengeRecord struct {
	gatewayID string
	publicKey []byte
	expiresAt time.Time
	consumed  bool
}

func NewChallengeManager(opts ChallengeManagerOptions) *ChallengeManager {
	if opts.Random == nil {
		opts.Random = rand.Reader
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &ChallengeManager{
		enabled: opts.Enabled, random: opts.Random, now: opts.Now,
		records: make(map[[sha256.Size]byte]*challengeRecord),
	}
}

// Issue stores the expected canonical key with the nonce. VerifyAndConsume
// never accepts a key from the connection that answers the challenge.
func (m *ChallengeManager) Issue(gatewayID string, expectedPublicKey []byte, ttl time.Duration) (MachineChallenge, error) {
	if !m.enabled {
		return MachineChallenge{}, ErrRemoteDisabled
	}
	normalized, err := gatewayidentity.NormalizeGatewayID(gatewayID)
	if err != nil {
		return MachineChallenge{}, err
	}
	if ttl <= 0 || ttl > MachineChallengeMaxTTL {
		return MachineChallenge{}, fmt.Errorf("machine challenge lifetime must be from 1ns through %s", MachineChallengeMaxTTL)
	}
	if err := gatewayidentity.ValidatePublicKey(expectedPublicKey); err != nil {
		return MachineChallenge{}, err
	}
	now := m.now().UTC()
	expiresAt := now.Add(ttl)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(now)
	for range 4 {
		raw := make([]byte, machineChallengeBytes)
		if _, err := io.ReadFull(m.random, raw); err != nil {
			return MachineChallenge{}, fmt.Errorf("create machine challenge: %w", err)
		}
		hash := sha256.Sum256(raw)
		if _, exists := m.records[hash]; exists {
			continue
		}
		m.records[hash] = &challengeRecord{
			gatewayID: normalized, publicKey: append([]byte(nil), expectedPublicKey...), expiresAt: expiresAt,
		}
		return MachineChallenge{
			Nonce: base64.RawURLEncoding.EncodeToString(raw), GatewayID: normalized, ExpiresAt: expiresAt,
		}, nil
	}
	return MachineChallenge{}, errors.New("could not create a unique machine challenge")
}

// MachineChallengeMessage is the exact byte string the gateway signs.
func MachineChallengeMessage(challenge MachineChallenge) ([]byte, error) {
	normalized, err := gatewayidentity.NormalizeGatewayID(challenge.GatewayID)
	if err != nil {
		return nil, err
	}
	raw, err := base64.RawURLEncoding.DecodeString(challenge.Nonce)
	if err != nil || len(raw) != machineChallengeBytes {
		return nil, errors.New("machine challenge nonce is invalid")
	}
	if challenge.ExpiresAt.IsZero() {
		return nil, errors.New("machine challenge expiry is missing")
	}
	return []byte(fmt.Sprintf("%s\n%s\n%s\n%d",
		machineChallengeDomain, normalized, challenge.Nonce, challenge.ExpiresAt.UnixMilli())), nil
}

func (m *ChallengeManager) VerifyAndConsume(challenge MachineChallenge, signature []byte) error {
	if !m.enabled {
		return ErrRemoteDisabled
	}
	raw, err := base64.RawURLEncoding.DecodeString(challenge.Nonce)
	if err != nil || len(raw) != machineChallengeBytes {
		return errors.New("machine challenge is invalid")
	}
	hash := sha256.Sum256(raw)
	message, err := MachineChallengeMessage(challenge)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.records[hash]
	if !ok || record.gatewayID != challenge.GatewayID || !record.expiresAt.Equal(challenge.ExpiresAt) {
		return errors.New("machine challenge is unknown")
	}
	if record.consumed {
		return errors.New("machine challenge has already been used")
	}
	if !m.now().UTC().Before(record.expiresAt) {
		return errors.New("machine challenge has expired")
	}
	if !gatewayidentity.Verify(record.publicKey, message, signature) {
		return errors.New("machine challenge signature is invalid")
	}
	record.consumed = true
	return nil
}

func (m *ChallengeManager) pruneLocked(now time.Time) {
	for hash, record := range m.records {
		if now.After(record.expiresAt.Add(MachineChallengeMaxTTL)) {
			delete(m.records, hash)
		}
	}
}
