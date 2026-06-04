package main

import (
	"sync"
	"time"
)

// signal.go — the blind WebRTC signaling rendezvous (P2P-only home route,
// slice 4). It REPLACES carrying the SDP offer over the owner HTTP tunnel.
//
// The relay holds a tiny per-site, per-nonce 2-slot mailbox: one parked
// browser->Pi OFFER and one parked Pi->browser ANSWER. It is NOT a tunnel.Queue
// — there is no request/response correlation, no body forwarding, no long-lived
// per-request goroutine fan-out. The relay forwards opaque SDP + signature blobs
// and never sees plaintext: the DataChannel that results is DTLS-encrypted end
// to end and its fingerprint is signed by the Pi's identity key (verified
// browser-side).
//
// Slots are single-occupancy with overwrite (a fresh offer supersedes a stale
// one) and a short TTL so a parked-but-never-drained blob self-evicts. Pollers
// (browser for the answer, Pi for the offer) block until a slot fills or a
// per-call deadline elapses, so signaling is near-instant without busy-polling.
//
// FIX-3: every long-poll waiter is REMOVED on timeout/return (mirrors
// tunnel.Queue.abandonWaiter), and the total + per-site waiter counts are
// bounded — so an unauthenticated GET /signal/<random>/answer flood across many
// site_ids can't leave permanent waiters and exhaust maxSignalSites. An
// answer-poll for a site/nonce that has no parked offer returns immediately
// WITHOUT allocating a mailbox, so the unauthenticated answer path can't grow
// the map at all.
//
// FIX-4a: the mailbox is keyed by (site, nonce). The nonce is an OPAQUE routing
// key the browser generates (crypto.getRandomValues, hex); the relay never
// parses the SDP. The browser parks its offer under a nonce and polls only that
// nonce's answer; the Pi echoes the nonce on the answer. So an attacker's offers
// land in their own nonce slot and can't displace or steal the legit browser's
// answer. Per-site nonces are bounded (oldest evicted) so the keyspace can't be
// flooded.

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
	// FIX-C: the per-site offer limit is now a GENEROUS token-bucket BACKSTOP, not
	// the primary throttle. The old per-site min-interval (one offer / 500ms / site)
	// was a lockout lever: an attacker spamming offers for site:Home kept the legit
	// browser permanently at 429, because the limit fired on the SITE, not the
	// abuser. The primary bound is now per-SOURCE-IP at the relay handler
	// (IPRateLimiter), which the Pi can't see but the relay can. This per-site
	// bucket only caps absolute per-site mailbox churn so even a distributed flood
	// can't spin one site's mailbox unboundedly; its burst is wide enough that
	// several legit concurrent tabs/retries never trip it.
	//
	// siteOfferBucketCap is the per-site burst; siteOfferRefillPerSec the sustained
	// rate. ~10 burst + ~5/s is far above any honest browser's offer cadence but
	// keeps a single site from being used to churn the mailbox without limit.
	siteOfferBucketCap      = 10.0
	siteOfferRefillPerSec   = 5.0
	// maxSignalSites bounds the number of distinct sites the mailbox tracks, so a
	// flood of offers for random site_ids can't grow relay memory without limit.
	maxSignalSites = 4096
	// maxSignalWaiters bounds the TOTAL number of concurrently parked long-poll
	// waiters across every site. /signal/{site}/answer is unauthenticated, so this
	// caps the goroutines/channels an attacker can pin by long-polling many sites.
	// Legitimate browsers hold ~1 in-flight poll each, so this is far above use.
	maxSignalWaiters = 1024
	// maxSignalWaitersPerSite bounds the waiters parked on ONE site, so a single
	// site_id can't monopolise the global budget. A legit pair holds one browser
	// answer-poll + one Pi offer-poll; this tolerates a few concurrent tabs.
	maxSignalWaitersPerSite = 8
	// maxSignalNonces bounds how many distinct rendezvous nonces a single site may
	// track at once (FIX-4a). Each nonce is one in-flight browser connect attempt;
	// the oldest idle one is evicted past this so an attacker spamming offers under
	// fresh nonces can't grow a site's mailbox without bound.
	maxSignalNonces = 8
)

// signalSlot is one parked blob with its parking time (for TTL).
type signalSlot struct {
	data     []byte
	parkedAt time.Time
}

// nonceMailbox is the per-(site,nonce) offer+answer pair plus the browser's
// answer-waiters for that nonce. The Pi's offer-waiters live at the SITE level
// (it drains any nonce), so they are not here.
type nonceMailbox struct {
	offer  *signalSlot
	answer *signalSlot

	answerWaiters []chan struct{} // browser pollers waiting for THIS nonce's answer

	createdAt time.Time // for oldest-nonce eviction
}

// empty reports whether a nonce mailbox can be reclaimed. A nonce is kept alive
// for signalSlotTTL from creation even after its offer is drained, so the Pi's
// answer (parked AFTER the offer is consumed) and the browser's answer-poll still
// find the mailbox — otherwise an immediate GC after offer-drain would race the
// answer and drop it. Waiters always keep it alive.
func (nb *nonceMailbox) empty(now time.Time) bool {
	if len(nb.answerWaiters) > 0 {
		return false
	}
	if now.Sub(nb.createdAt) <= signalSlotTTL {
		return false // still within the rendezvous window — an answer may arrive
	}
	offerStale := nb.offer == nil || now.Sub(nb.offer.parkedAt) > signalSlotTTL
	answerStale := nb.answer == nil || now.Sub(nb.answer.parkedAt) > signalSlotTTL
	return offerStale && answerStale
}

// siteMailbox holds the bounded set of in-flight rendezvous nonces for one site,
// the Pi's site-wide offer-waiters (the Pi drains any nonce), and the per-site
// offer rate-limit state (a generous token-bucket BACKSTOP — the primary throttle
// is per-source-IP at the handler; FIX-C).
type siteMailbox struct {
	byNonce      map[string]*nonceMailbox
	offerWaiters []chan struct{} // Pi pollers waiting for ANY offer on this site

	// Per-site offer token bucket (FIX-C backstop). offerTokens refills at
	// siteOfferRefillPerSec up to siteOfferBucketCap; each parked offer spends one.
	// lastOfferAt doubles as the bucket's last-fill clock AND the GC idle marker.
	offerTokens float64
	lastOfferAt time.Time
}

// allowOfferLocked spends one per-site offer token, refilling first. Returns
// false when the site's generous backstop bucket is empty. Caller holds m.mu.
// A brand-new mailbox (zero lastOfferAt) starts at full capacity.
func (s *siteMailbox) allowOfferLocked(now time.Time) bool {
	if s.lastOfferAt.IsZero() {
		s.offerTokens = siteOfferBucketCap
	} else if d := now.Sub(s.lastOfferAt).Seconds(); d > 0 {
		s.offerTokens += d * siteOfferRefillPerSec
		if s.offerTokens > siteOfferBucketCap {
			s.offerTokens = siteOfferBucketCap
		}
	}
	s.lastOfferAt = now
	if s.offerTokens >= 1 {
		s.offerTokens--
		return true
	}
	return false
}

func (s *siteMailbox) waiterCount() int {
	n := len(s.offerWaiters)
	for _, nb := range s.byNonce {
		n += len(nb.answerWaiters)
	}
	return n
}

// hasLiveNonces reports whether any nonce in this site is still within its
// rendezvous window or holds a waiter/slot — i.e. not yet reclaimable.
func (s *siteMailbox) hasLiveNonces(now time.Time) bool {
	for _, nb := range s.byNonce {
		if !nb.empty(now) {
			return true
		}
	}
	return false
}

// SignalMailbox is the relay-wide rendezvous: a map of site_id -> siteMailbox
// guarded by one mutex. Safe for concurrent use. In-memory and ephemeral; a
// relay restart drops parked blobs and both ends simply re-signal.
type SignalMailbox struct {
	mu          sync.Mutex
	bySite      map[string]*siteMailbox
	waiterCount int // total live waiters across all sites (bounded by maxSignalWaiters)
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
		b = &siteMailbox{byNonce: make(map[string]*nonceMailbox)}
		m.bySite[siteID] = b
	}
	return b
}

// nonceBox returns the per-nonce mailbox under a site, creating it when
// create=true. Past maxSignalNonces it first evicts the oldest IDLE nonce (one
// with no parked answer-waiter); if every nonce is busy with a live poll it
// returns nil rather than stranding a waiter — the caller surfaces that as a
// refusal. Caller holds m.mu.
func (b *siteMailbox) nonceBox(nonce string, create bool) *nonceMailbox {
	nb := b.byNonce[nonce]
	if nb == nil && create {
		if len(b.byNonce) >= maxSignalNonces && !b.evictOldestIdleNonce() {
			return nil // all nonces busy with live polls — refuse, don't strand
		}
		nb = &nonceMailbox{createdAt: time.Now()}
		b.byNonce[nonce] = nb
	}
	return nb
}

// evictOldestIdleNonce drops the oldest nonce with no parked answer-waiter, so an
// offer flood under fresh nonces can't grow the site without bound. It NEVER
// evicts a nonce with a live waiter (that would strand the waiter and corrupt the
// global waiter count). Returns whether it evicted one. Caller holds m.mu.
func (b *siteMailbox) evictOldestIdleNonce() bool {
	var key string
	var oldest time.Time
	for k, nb := range b.byNonce {
		if len(nb.answerWaiters) != 0 {
			continue // busy — must not strand
		}
		if key == "" || nb.createdAt.Before(oldest) {
			key, oldest = k, nb.createdAt
		}
	}
	if key == "" {
		return false
	}
	delete(b.byNonce, key)
	return true
}

// signalError is a small typed error so the handler can distinguish causes.
type signalError string

func (e signalError) Error() string { return string(e) }

const (
	errSignalRateLimited = signalError("offer rate limit exceeded")
	errSignalAtCapacity  = signalError("signaling at capacity")
)

// ParkOffer stores a browser SDP offer for (siteID, nonce), overwriting any
// prior one for that nonce, and wakes any Pi offer-poller. Rate-limited per site.
func (m *SignalMailbox) ParkOffer(siteID, nonce string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.box(siteID, true)
	if b == nil {
		return errSignalAtCapacity
	}
	now := time.Now()
	// Per-site generous token-bucket backstop (FIX-C). The primary per-IP throttle
	// runs at the handler; this only caps absolute per-site mailbox churn so even a
	// distributed flood can't spin one site without limit. A legit browser's offer
	// cadence is far under this, so it is never the lockout lever the old per-site
	// min-interval was.
	if !b.allowOfferLocked(now) {
		return errSignalRateLimited
	}
	nb := b.nonceBox(nonce, true)
	if nb == nil {
		// Every nonce slot is busy with a live answer-poll — refuse rather than
		// strand one. The browser retries under a fresh attempt.
		return errSignalAtCapacity
	}
	nb.offer = &signalSlot{data: data, parkedAt: now}
	// Wake the Pi's site-wide offer-waiters — any of them can drain this offer.
	m.wakeAll(&b.offerWaiters)
	return nil
}

// ParkAnswer stores the Pi's answer blob for (siteID, nonce), overwriting any
// prior one, and wakes that nonce's browser answer-poller. It never allocates a
// new site or nonce: an answer with no matching in-flight offer-nonce is dropped
// (the browser will re-offer under a fresh nonce).
func (m *SignalMailbox) ParkAnswer(siteID, nonce string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.box(siteID, false)
	if b == nil {
		return
	}
	nb := b.byNonce[nonce]
	if nb == nil {
		return // no in-flight offer for this nonce; nothing to answer
	}
	nb.answer = &signalSlot{data: data, parkedAt: time.Now()}
	m.wakeAll(&nb.answerWaiters)
}

// TakeOffer long-polls for a parked offer for siteID, consuming it on success and
// returning the nonce it was parked under so the Pi echoes it on the answer.
// Returns (data, nonce, true) on success, else (nil, "", false). The Pi calls
// this; consuming the slot means one offer yields one answer round.
func (m *SignalMailbox) TakeOffer(siteID string, timeout time.Duration) ([]byte, string, bool) {
	return m.takeOffer(siteID, timeout)
}

// TakeAnswer long-polls for a parked answer for (siteID, nonce), consuming it on
// success. The browser calls this after parking its offer under nonce. It never
// allocates a mailbox for an unknown site/nonce — an answer-poll for a site that
// never received a matching offer returns (nil, false) immediately.
func (m *SignalMailbox) TakeAnswer(siteID, nonce string, timeout time.Duration) ([]byte, bool) {
	return m.takeAnswer(siteID, nonce, timeout)
}

// takeOffer is the Pi-side (authenticated) drain: it waits for any fresh offer
// on the site across nonces.
func (m *SignalMailbox) takeOffer(siteID string, timeout time.Duration) ([]byte, string, bool) {
	deadline := time.After(timeout)
	for {
		m.mu.Lock()
		b := m.box(siteID, false)
		if b != nil {
			if data, nonce, ok := b.consumeFreshOffer(); ok {
				m.gcEmptyLocked(siteID, b)
				m.mu.Unlock()
				return data, nonce, true
			}
		}
		if b == nil {
			b = m.box(siteID, true)
			if b == nil {
				m.mu.Unlock()
				return nil, "", false // at site capacity
			}
		}
		if m.waiterCount >= maxSignalWaiters || b.waiterCount() >= maxSignalWaitersPerSite {
			m.gcEmptyLocked(siteID, b)
			m.mu.Unlock()
			return nil, "", false // surfaced as 204 (re-poll) by the handler
		}
		wake := make(chan struct{})
		b.offerWaiters = append(b.offerWaiters, wake)
		m.waiterCount++
		m.mu.Unlock()

		select {
		case <-wake:
			// woken by a ParkOffer; loop and consume
		case <-deadline:
			m.abandonOfferWaiter(siteID, wake)
			return nil, "", false
		}
	}
}

// takeAnswer is the browser-side (unauthenticated) drain for one nonce. It never
// allocates a mailbox for an unknown site/nonce.
func (m *SignalMailbox) takeAnswer(siteID, nonce string, timeout time.Duration) ([]byte, bool) {
	deadline := time.After(timeout)
	for {
		m.mu.Lock()
		b := m.bySite[siteID]
		if b == nil {
			m.mu.Unlock()
			return nil, false // unknown site → 204, no allocation
		}
		nb := b.byNonce[nonce]
		if nb == nil {
			m.gcEmptyLocked(siteID, b)
			m.mu.Unlock()
			return nil, false // unknown nonce → 204, no allocation
		}
		if data, ok := nb.consumeFreshAnswer(); ok {
			m.gcEmptyLocked(siteID, b)
			m.mu.Unlock()
			return data, true
		}
		if m.waiterCount >= maxSignalWaiters || b.waiterCount() >= maxSignalWaitersPerSite {
			m.gcEmptyLocked(siteID, b)
			m.mu.Unlock()
			return nil, false // capacity → 204, re-poll
		}
		wake := make(chan struct{})
		nb.answerWaiters = append(nb.answerWaiters, wake)
		m.waiterCount++
		m.mu.Unlock()

		select {
		case <-wake:
			// woken by a ParkAnswer; loop and consume
		case <-deadline:
			m.abandonAnswerWaiter(siteID, nonce, wake)
			return nil, false
		}
	}
}

// abandonOfferWaiter removes a timed-out Pi offer-waiter and GCs the now-empty
// site. Mirrors tunnel.Queue.abandonWaiter. Idempotent against a wakeAll that
// already removed the waiter.
func (m *SignalMailbox) abandonOfferWaiter(siteID string, wake chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.bySite[siteID]
	if b == nil {
		return
	}
	for i, c := range b.offerWaiters {
		if c == wake {
			b.offerWaiters = append(b.offerWaiters[:i], b.offerWaiters[i+1:]...)
			m.waiterCount--
			break
		}
	}
	m.gcEmptyLocked(siteID, b)
}

// abandonAnswerWaiter removes a timed-out browser answer-waiter for a nonce and
// GCs the now-empty site/nonce.
func (m *SignalMailbox) abandonAnswerWaiter(siteID, nonce string, wake chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.bySite[siteID]
	if b == nil {
		return
	}
	if nb := b.byNonce[nonce]; nb != nil {
		for i, c := range nb.answerWaiters {
			if c == wake {
				nb.answerWaiters = append(nb.answerWaiters[:i], nb.answerWaiters[i+1:]...)
				m.waiterCount--
				break
			}
		}
	}
	m.gcEmptyLocked(siteID, b)
}

// gcEmptyLocked drops empty nonce mailboxes, then the site mailbox if it has no
// nonces, no offer-waiters, and no recent offer activity. Caller holds m.mu.
func (m *SignalMailbox) gcEmptyLocked(siteID string, b *siteMailbox) {
	now := time.Now()
	for k, nb := range b.byNonce {
		if nb.empty(now) {
			delete(b.byNonce, k)
		}
	}
	if len(b.byNonce) == 0 && len(b.offerWaiters) == 0 &&
		(b.lastOfferAt.IsZero() || now.Sub(b.lastOfferAt) > signalSlotTTL) {
		delete(m.bySite, siteID)
	}
}

// consumeFreshOffer finds and clears the oldest fresh offer across this site's
// nonces, returning the nonce it was parked under. An expired offer is cleared
// and skipped. Caller holds m.mu.
func (b *siteMailbox) consumeFreshOffer() ([]byte, string, bool) {
	now := time.Now()
	var pickKey string
	var pickAt time.Time
	for k, nb := range b.byNonce {
		if nb.offer == nil {
			continue
		}
		if now.Sub(nb.offer.parkedAt) > signalSlotTTL {
			nb.offer = nil
			continue
		}
		if pickKey == "" || nb.offer.parkedAt.Before(pickAt) {
			pickKey, pickAt = k, nb.offer.parkedAt
		}
	}
	if pickKey == "" {
		return nil, "", false
	}
	nb := b.byNonce[pickKey]
	s := nb.offer
	nb.offer = nil
	return s.data, pickKey, true
}

// consumeFreshAnswer returns and clears this nonce's answer if fresh. Caller
// holds m.mu.
func (nb *nonceMailbox) consumeFreshAnswer() ([]byte, bool) {
	if nb.answer == nil {
		return nil, false
	}
	s := nb.answer
	nb.answer = nil
	if time.Since(s.parkedAt) > signalSlotTTL {
		return nil, false
	}
	return s.data, true
}

// wakeAll closes and clears every registered waiter channel, broadcasting that a
// slot was parked, and decrements the global waiter count for each. Closed
// channels are one-shot; pollers re-register if they lose the race for the slot.
// Caller holds m.mu.
func (m *SignalMailbox) wakeAll(waiters *[]chan struct{}) {
	for _, w := range *waiters {
		close(w)
		m.waiterCount--
	}
	*waiters = nil
}

// GC drops mailboxes that have no parked blobs, no waiters, and no recent offer
// activity, so the map self-heals after sites go quiet. Returns how many were
// removed.
func (m *SignalMailbox) GC(maxAge time.Duration) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	removed := 0
	for site, b := range m.bySite {
		if b.waiterCount() > 0 {
			continue // a poller is parked here
		}
		// Reclaim empty nonces first; if any remain live, keep the site.
		for k, nb := range b.byNonce {
			if nb.empty(now) {
				delete(b.byNonce, k)
			}
		}
		if b.hasLiveNonces(now) {
			continue
		}
		if b.lastOfferAt.IsZero() || now.Sub(b.lastOfferAt) > maxAge {
			delete(m.bySite, site)
			removed++
		}
	}
	return removed
}
