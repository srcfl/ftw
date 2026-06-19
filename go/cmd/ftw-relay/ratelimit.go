package main

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// ratelimit.go — per-source-IP token-bucket limiter for the UNAUTHENTICATED
// browser signaling offer endpoint (FIX-C).
//
// The original offer rate limit lived on the SITE mailbox (signal.lastOfferAt),
// which made it a lockout lever: an attacker posting offers for site:Home every
// 500ms kept the legitimate browser permanently at 429 — the limit fired on the
// SITE, not the abuser. The relay (unlike the Pi) sees the browser's source IP,
// so the right place to bound abuse is HERE, per IP: one attacker IP is throttled
// while a legit browser on a different IP is never affected. The per-site limit is
// kept only as a generous backstop (see signal.go).
//
// RESIDUAL (documented): a DISTRIBUTED flood from many source IPs can still post
// many offers — but each lands in its own per-(site,nonce) mailbox (FIX-4a) and
// can't displace the legit browser's answer, and the per-site nonce cap + waiter
// caps bound memory. Per-IP limiting raises the bar to "needs a botnet"; it is not
// a complete defence against a true DDoS, which is out of scope for an in-process
// limiter (that belongs at the CDN/WAF in front of the relay).

const (
	// offerBucketCapacity is the burst a single source IP may make before the
	// bucket empties. A legit browser issues one offer per connect attempt and
	// retries a few times on a flaky network; a handful of burst tokens covers
	// that with headroom.
	offerBucketCapacity = 8.0
	// offerBucketRefillPerSec is the steady-state offer rate a source IP may
	// sustain once its burst is spent. ~2/s is far above any legitimate
	// reconnect cadence but starves an attacker trying to spin the mailbox.
	offerBucketRefillPerSec = 2.0
	// offerBucketIdleTTL is how long an idle per-IP bucket is kept before GC, so
	// the limiter map doesn't grow without bound under a churn of source IPs.
	offerBucketIdleTTL = 10 * time.Minute
)

// tokenBucket is a classic token bucket: tokens refill at a fixed rate up to a
// cap, and each allowed event spends one.
type tokenBucket struct {
	tokens   float64
	lastFill time.Time
}

// IPRateLimiter throttles events per source IP with an independent token bucket
// each. Safe for concurrent use.
type IPRateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*tokenBucket
	capacity float64
	refill   float64 // tokens added per second
}

func newIPRateLimiter(capacity, refillPerSec float64) *IPRateLimiter {
	return &IPRateLimiter{
		buckets:  make(map[string]*tokenBucket),
		capacity: capacity,
		refill:   refillPerSec,
	}
}

// Allow reports whether an event from ip is permitted right now, spending one
// token if so. Unknown IPs start full (so the first request always passes).
func (l *IPRateLimiter) Allow(ip string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[ip]
	if b == nil {
		// New IP: start at capacity, spend one.
		l.buckets[ip] = &tokenBucket{tokens: l.capacity - 1, lastFill: now}
		return true
	}
	// Refill since last seen, capped.
	elapsed := now.Sub(b.lastFill).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * l.refill
		if b.tokens > l.capacity {
			b.tokens = l.capacity
		}
		b.lastFill = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// GC drops buckets idle longer than ttl (they have refilled to full anyway, so
// forgetting them only resets a now-quiet IP to a fresh full bucket). Returns how
// many were removed.
func (l *IPRateLimiter) GC(ttl time.Duration) int {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	removed := 0
	for ip, b := range l.buckets {
		if now.Sub(b.lastFill) > ttl {
			delete(l.buckets, ip)
			removed++
		}
	}
	return removed
}

// clientIP extracts the source IP the relay should rate-limit on. The relay
// terminates behind Cloudflare (see docs/relay-deploy.md) but there is currently
// NO trusted-proxy header handling wired anywhere in the codebase, so honouring a
// client-supplied X-Forwarded-For / CF-Connecting-IP here would let an attacker
// forge a fresh "source IP" per request and trivially bypass the per-IP limit.
// We therefore use the transport-level RemoteAddr, which an attacker cannot spoof
// on a real TCP connection.
//
// NOTE for operators: if/when a trusted-proxy mode is added (the relay learns it
// sits behind a known CDN and validates the connecting peer), THIS is the single
// place to start trusting CF-Connecting-IP — keep the spoofable headers ignored
// until then.
func clientIP(req *http.Request) string {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		// No port (already a bare host, or malformed) — use it verbatim.
		return req.RemoteAddr
	}
	return host
}
