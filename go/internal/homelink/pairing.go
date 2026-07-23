package homelink

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

const (
	pairingSecretBytes = 32
	pairingIDBytes     = 24
	maxPairingRecords  = 16
)

var (
	ErrPairingInvalid = errors.New("local pairing proof is invalid")
	ErrPairingExpired = errors.New("local pairing proof has expired")
)

type PairingChallenge struct {
	ID        string
	Secret    []byte
	ExpiresAt time.Time
}

type pairingRecord struct {
	secretHash [sha256.Size]byte
	deadline   time.Duration
}

type LocalPairingManagerOptions struct {
	Random       io.Reader
	Now          func() time.Time
	MonotonicNow func() time.Duration
}

// LocalPairingManager keeps only short-lived secret hashes in memory. A
// restart, revoke, failed attempt, or expiry makes the proof unusable.
type LocalPairingManager struct {
	mu           sync.Mutex
	random       io.Reader
	now          func() time.Time
	monotonicNow func() time.Duration
	clock        monotonicClockState
	records      map[string]pairingRecord
}

func NewLocalPairingManager(opts LocalPairingManagerOptions) *LocalPairingManager {
	if opts.Random == nil {
		opts.Random = rand.Reader
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MonotonicNow == nil {
		opts.MonotonicNow = defaultMonotonicNow
	}
	return &LocalPairingManager{
		random: opts.Random, now: opts.Now, monotonicNow: opts.MonotonicNow,
		records: make(map[string]pairingRecord),
	}
}

func (m *LocalPairingManager) Create(ttl time.Duration) (PairingChallenge, error) {
	if ttl <= 0 || ttl > PairingGrantMaxTTL {
		return PairingChallenge{}, fmt.Errorf("pairing lifetime must be from 1ns through %s", PairingGrantMaxTTL)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now, err := m.clock.sample(m.monotonicNow)
	if err != nil {
		return PairingChallenge{}, err
	}
	deadline, err := m.clock.deadline(now, ttl)
	if err != nil {
		return PairingChallenge{}, err
	}
	m.pruneLocked(now)
	if len(m.records) >= maxPairingRecords {
		return PairingChallenge{}, errors.New("too many local pairing requests")
	}
	for range 4 {
		idRaw := make([]byte, pairingIDBytes)
		secret := make([]byte, pairingSecretBytes)
		if _, err := io.ReadFull(m.random, idRaw); err != nil {
			return PairingChallenge{}, errors.New("create local pairing id")
		}
		if _, err := io.ReadFull(m.random, secret); err != nil {
			return PairingChallenge{}, errors.New("create local pairing secret")
		}
		id := base64.RawURLEncoding.EncodeToString(idRaw)
		if _, exists := m.records[id]; exists {
			continue
		}
		m.records[id] = pairingRecord{secretHash: sha256.Sum256(secret), deadline: deadline}
		return PairingChallenge{
			ID: id, Secret: secret, ExpiresAt: m.now().UTC().Add(ttl),
		}, nil
	}
	return PairingChallenge{}, errors.New("could not create a unique local pairing request")
}

func (m *LocalPairingManager) Revoke(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.records, id)
}

func (m *LocalPairingManager) AuthorizeLocalPairing(ctx context.Context, proof LocalPairingProof) error {
	id := string(proof.Challenge)
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.records[id]
	delete(m.records, id)
	if !ok {
		return ErrPairingInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	now, err := m.clock.sample(m.monotonicNow)
	if err != nil {
		return err
	}
	if now >= record.deadline {
		return ErrPairingExpired
	}
	hash := sha256.Sum256(proof.Response)
	if subtle.ConstantTimeCompare(hash[:], record.secretHash[:]) != 1 {
		return ErrPairingInvalid
	}
	return nil
}

func (m *LocalPairingManager) pruneLocked(now time.Duration) {
	for id, record := range m.records {
		if now >= record.deadline {
			delete(m.records, id)
		}
	}
}
