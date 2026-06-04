package main

import (
	"sync"
	"time"
)

// signal.go — the blind WebRTC signaling rendezvous (P2P-only home route,
// slice 4). It REPLACES carrying the SDP offer over the owner HTTP tunnel.
//
// The relay holds a tiny per-site 2-slot mailbox: one parked browser->Pi OFFER
// and one parked Pi->browser ANSWER. It is NOT a tunnel.Queue — there is no
// request/response correlation, no body forwarding, no long-lived per-request
// goroutine fan-out. The relay forwards opaque SDP + signature blobs and never
// sees plaintext: the DataChannel that results is DTLS-encrypted end to end and
// its fingerprint is signed by the Pi's identity key (verified browser-side).
//
// Slots are single-occupancy with overwrite (a fresh offer supersedes a stale
// one) and a short TTL so a parked-but-never-drained blob self-evicts. Pollers
// (browser for the answer, Pi for the offer) block until a slot fills or a
// per-call deadline elapses, so signaling is near-instant without busy-polling.

const (
	// signalSlotTTL bounds how long a parked offer/answer stays valid. WebRTC
	// signaling completes in well under a second on a live pair; 30s tolerates a
	// slow ICE gather on the Pi while ensuring a stale blob never lingers.
	signalSlotTTL = 30 * time.Second
	// signalPollTimeout bounds one long-poll for a slot before returning 204 so
	// the caller re-polls. Kept under the relay's overall request budget.
	signalPollTimeout = 25 * time.Second
	// maxSignalOfferBytes caps a parked SDP offer. A WebRTC offer with embedded
	// ICE candidates is a few KiB; 64 KiB is generous headroom and rejects abuse.
	maxSignalOfferBytes = 64 << 10
	// maxSignalAnswerBytes caps a parked answer blob (SDP + signature + ts).
	maxSignalAnswerBytes = 64 << 10
	// signalOfferMinInterval rate-limits how often a single site may park a new
	// offer, so an unauthenticated browser endpoint can't be used to spin the
	// mailbox. One offer per 500ms per site is far above any legitimate retry.
	signalOfferMinInterval = 500 * time.Millisecond
	// maxSignalSites bounds the number of distinct sites the mailbox tracks, so a
	// flood of offers for random site_ids can't grow relay memory without limit.
	maxSignalSites = 4096
)

// signalSlot is one parked blob with its parking time (for TTL) and a waiter
// channel that pollers select on to be woken the instant the slot fills.
type signalSlot struct {
	data     []byte
	parkedAt time.Time
}

// siteMailbox is the per-site 2-slot mailbox. offer is browser->Pi; answer is
// Pi->browser. waiters are closed-on-park broadcast channels so a long-poller is
// woken immediately rather than polling on a timer.
type siteMailbox struct {
	offer  *signalSlot
	answer *signalSlot

	offerWaiters  []chan struct{} // woken when an offer is parked (Pi pollers)
	answerWaiters []chan struct{} // woken when an answer is parked (browser pollers)

	lastOfferAt time.Time // for the per-site offer rate limit
}

// SignalMailbox is the relay-wide rendezvous: a map of site_id -> siteMailbox
// guarded by one mutex. Safe for concurrent use. In-memory and ephemeral; a
// relay restart drops parked blobs and both ends simply re-signal.
type SignalMailbox struct {
	mu     sync.Mutex
	bySite map[string]*siteMailbox
}

func NewSignalMailbox() *SignalMailbox {
	return &SignalMailbox{bySite: make(map[string]*siteMailbox)}
}

// box returns the mailbox for siteID, creating it if absent. create=false never
// allocates (used on the drain side so a Pi polling an idle site doesn't grow
// the map). Caller holds m.mu.
func (m *SignalMailbox) box(siteID string, create bool) *siteMailbox {
	b := m.bySite[siteID]
	if b == nil && create {
		if len(m.bySite) >= maxSignalSites {
			return nil // at capacity — refuse to allocate a new site
		}
		b = &siteMailbox{}
		m.bySite[siteID] = b
	}
	return b
}

// errSignalRateLimited is returned by ParkOffer when a site parks offers faster
// than signalOfferMinInterval.
type signalError string

func (e signalError) Error() string { return string(e) }

const (
	errSignalRateLimited = signalError("offer rate limit exceeded")
	errSignalAtCapacity  = signalError("signaling at capacity")
)

// ParkOffer stores a browser SDP offer for siteID, overwriting any prior one,
// and wakes any Pi offer-poller. Rate-limited per site.
func (m *SignalMailbox) ParkOffer(siteID string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.box(siteID, true)
	if b == nil {
		return errSignalAtCapacity
	}
	now := time.Now()
	if !b.lastOfferAt.IsZero() && now.Sub(b.lastOfferAt) < signalOfferMinInterval {
		return errSignalRateLimited
	}
	b.lastOfferAt = now
	b.offer = &signalSlot{data: data, parkedAt: now}
	wakeAll(&b.offerWaiters)
	return nil
}

// ParkAnswer stores the Pi's answer blob for siteID, overwriting any prior one,
// and wakes any browser answer-poller.
func (m *SignalMailbox) ParkAnswer(siteID string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.box(siteID, true)
	if b == nil {
		return // at capacity — the browser will re-offer
	}
	b.answer = &signalSlot{data: data, parkedAt: time.Now()}
	wakeAll(&b.answerWaiters)
}

// TakeOffer long-polls for a parked offer for siteID, consuming it on success.
// Returns (data, true) when an offer is available within the deadline, else
// (nil, false). The Pi calls this; consuming the slot means one offer yields one
// answer round.
func (m *SignalMailbox) TakeOffer(siteID string, timeout time.Duration) ([]byte, bool) {
	return m.take(siteID, timeout, true)
}

// TakeAnswer long-polls for a parked answer for siteID, consuming it on success.
// The browser calls this after parking its offer.
func (m *SignalMailbox) TakeAnswer(siteID string, timeout time.Duration) ([]byte, bool) {
	return m.take(siteID, timeout, false)
}

func (m *SignalMailbox) take(siteID string, timeout time.Duration, wantOffer bool) ([]byte, bool) {
	deadline := time.After(timeout)
	for {
		m.mu.Lock()
		b := m.box(siteID, false)
		if b != nil {
			if data, ok := b.consumeFresh(wantOffer); ok {
				m.mu.Unlock()
				return data, true
			}
		}
		// Nothing fresh yet: register a waiter and unlock so a parker can wake us.
		wake := make(chan struct{})
		if b == nil {
			b = m.box(siteID, true)
			if b == nil {
				m.mu.Unlock()
				return nil, false // at capacity
			}
		}
		if wantOffer {
			b.offerWaiters = append(b.offerWaiters, wake)
		} else {
			b.answerWaiters = append(b.answerWaiters, wake)
		}
		m.mu.Unlock()

		select {
		case <-wake:
			// A slot was parked — loop and try to consume it. (Another poller may
			// have raced us; consumeFresh handles the empty case by re-waiting.)
		case <-deadline:
			return nil, false
		}
	}
}

// consumeFresh returns and clears the requested slot iff it is present and not
// past its TTL. An expired slot is cleared and reported as absent. Caller holds
// m.mu.
func (b *siteMailbox) consumeFresh(wantOffer bool) ([]byte, bool) {
	slotp := &b.answer
	if wantOffer {
		slotp = &b.offer
	}
	s := *slotp
	if s == nil {
		return nil, false
	}
	if time.Since(s.parkedAt) > signalSlotTTL {
		*slotp = nil
		return nil, false
	}
	*slotp = nil
	return s.data, true
}

// wakeAll closes and clears every registered waiter channel, broadcasting that a
// slot was parked. Closed channels are one-shot; pollers re-register if they
// lose the race for the slot. Caller holds m.mu.
func wakeAll(waiters *[]chan struct{}) {
	for _, w := range *waiters {
		close(w)
	}
	*waiters = nil
}

// GC drops mailboxes that have no parked blobs and no recent offer activity,
// so the map self-heals after sites go quiet. Returns how many were removed.
func (m *SignalMailbox) GC(maxAge time.Duration) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	removed := 0
	for site, b := range m.bySite {
		if len(b.offerWaiters) > 0 || len(b.answerWaiters) > 0 {
			continue // a poller is parked here
		}
		offerStale := b.offer == nil || now.Sub(b.offer.parkedAt) > signalSlotTTL
		answerStale := b.answer == nil || now.Sub(b.answer.parkedAt) > signalSlotTTL
		activityStale := b.lastOfferAt.IsZero() || now.Sub(b.lastOfferAt) > maxAge
		if offerStale && answerStale && activityStale {
			delete(m.bySite, site)
			removed++
		}
	}
	return removed
}
