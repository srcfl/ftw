package homelink

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestLocalPairingOneUseExpiryRevokeAndRestart(t *testing.T) {
	var monotonic time.Duration
	manager := NewLocalPairingManager(LocalPairingManagerOptions{
		Random:       bytes.NewReader(bytes.Repeat([]byte{7}, 1024)),
		Now:          func() time.Time { return time.Unix(100, 0) },
		MonotonicNow: func() time.Duration { return monotonic },
	})
	first, err := manager.Create(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Secret) != pairingSecretBytes {
		t.Fatalf("secret bytes = %d", len(first.Secret))
	}
	proof := LocalPairingProof{Challenge: []byte(first.ID), Response: first.Secret}
	if err := manager.AuthorizeLocalPairing(context.Background(), proof); err != nil {
		t.Fatal(err)
	}
	if err := manager.AuthorizeLocalPairing(context.Background(), proof); !errors.Is(err, ErrPairingInvalid) {
		t.Fatalf("replay = %v", err)
	}

	expired, err := manager.Create(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	monotonic = time.Second
	if err := manager.AuthorizeLocalPairing(context.Background(), LocalPairingProof{
		Challenge: []byte(expired.ID), Response: expired.Secret,
	}); !errors.Is(err, ErrPairingExpired) {
		t.Fatalf("exact deadline = %v", err)
	}

	monotonic = 2 * time.Second
	revoked, err := manager.Create(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	manager.Revoke(revoked.ID)
	if err := manager.AuthorizeLocalPairing(context.Background(), LocalPairingProof{
		Challenge: []byte(revoked.ID), Response: revoked.Secret,
	}); !errors.Is(err, ErrPairingInvalid) {
		t.Fatalf("revoked proof = %v", err)
	}

	restart, err := manager.Create(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	newManager := NewLocalPairingManager(LocalPairingManagerOptions{
		Random:       bytes.NewReader(bytes.Repeat([]byte{8}, 1024)),
		MonotonicNow: func() time.Duration { return monotonic },
	})
	if err := newManager.AuthorizeLocalPairing(context.Background(), LocalPairingProof{
		Challenge: []byte(restart.ID), Response: restart.Secret,
	}); !errors.Is(err, ErrPairingInvalid) {
		t.Fatalf("proof survived restart = %v", err)
	}
}

func TestLocalPairingConsumesWrongProofAndClockFailure(t *testing.T) {
	var monotonic time.Duration
	manager := NewLocalPairingManager(LocalPairingManagerOptions{
		Random:       bytes.NewReader(bytes.Repeat([]byte{9}, 1024)),
		MonotonicNow: func() time.Duration { return monotonic },
	})
	challenge, err := manager.Create(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	wrong := bytes.Clone(challenge.Secret)
	wrong[0] ^= 0xff
	proof := LocalPairingProof{Challenge: []byte(challenge.ID), Response: wrong}
	if err := manager.AuthorizeLocalPairing(context.Background(), proof); !errors.Is(err, ErrPairingInvalid) {
		t.Fatalf("wrong proof = %v", err)
	}
	proof.Response = challenge.Secret
	if err := manager.AuthorizeLocalPairing(context.Background(), proof); !errors.Is(err, ErrPairingInvalid) {
		t.Fatalf("right proof after wrong attempt = %v", err)
	}

	clocked, err := manager.Create(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	monotonic--
	if err := manager.AuthorizeLocalPairing(context.Background(), LocalPairingProof{
		Challenge: []byte(clocked.ID), Response: clocked.Secret,
	}); !errors.Is(err, ErrMonotonicClock) {
		t.Fatalf("clock regression = %v", err)
	}
}

func TestLocalPairingStoresOnlySecretHash(t *testing.T) {
	manager := NewLocalPairingManager(LocalPairingManagerOptions{})
	challenge, err := manager.Create(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	record := manager.records[challenge.ID]
	manager.mu.Unlock()
	if bytes.Contains(record.secretHash[:], challenge.Secret) ||
		bytes.Equal(record.secretHash[:], challenge.Secret) {
		t.Fatal("pairing manager retained the raw secret")
	}
}

func TestLocalPairingRejectsInvalidLifetimeAndConsumesOnContextError(t *testing.T) {
	manager := NewLocalPairingManager(LocalPairingManagerOptions{})
	for _, ttl := range []time.Duration{0, -1, PairingGrantMaxTTL + 1} {
		if _, err := manager.Create(ttl); err == nil {
			t.Fatalf("accepted lifetime %s", ttl)
		}
	}
	challenge, err := manager.Create(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	proof := LocalPairingProof{Challenge: []byte(challenge.ID), Response: challenge.Secret}
	if err := manager.AuthorizeLocalPairing(ctx, proof); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled pairing = %v", err)
	}
	if err := manager.AuthorizeLocalPairing(context.Background(), proof); !errors.Is(err, ErrPairingInvalid) {
		t.Fatalf("pairing survived canceled attempt = %v", err)
	}
}
