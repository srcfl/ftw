package main

import (
	"net"
	"net/http"
	"strings"
)

// cloudflare.go — trusted-proxy mode for the per-source-IP signaling throttle.
//
// The production relay sits BEHIND Cloudflare (home.fortytwowatts.com is proxied,
// see docs/relay-deploy.md). At the relay, req.RemoteAddr is then a Cloudflare
// EDGE IP, shared by every client — so the per-IP offer throttle (FIX-C) would
// collapse to one bucket and re-open the site-lockout / owner-denial DoS.
//
// When -trust-cf-ip is set, offerClientIP trusts Cloudflare's CF-Connecting-IP
// header — but ONLY when the immediate TCP peer (RemoteAddr) is itself a published
// Cloudflare IP. That validation is what makes it safe: an attacker who reaches
// the origin directly (RemoteAddr NOT a CF IP) can set CF-Connecting-IP to
// anything, and we ignore it, falling back to their un-spoofable RemoteAddr.
// Operators should ALSO firewall the origin to Cloudflare's ranges (or use
// Authenticated Origin Pulls); this header check is defence in depth.

// cloudflareCIDRs is Cloudflare's published edge ranges (www.cloudflare.com/ips).
// Stable for years; refresh if Cloudflare ever expands them.
var cloudflareCIDRs = []string{
	// IPv4
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	// IPv6
	"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32",
	"2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
}

// cloudflareNets is cloudflareCIDRs parsed once at init.
var cloudflareNets = func() []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cloudflareCIDRs))
	for _, c := range cloudflareCIDRs {
		if _, n, err := net.ParseCIDR(c); err == nil {
			out = append(out, n)
		}
	}
	return out
}()

// isCloudflareIP reports whether host (a bare IP string) is within a published
// Cloudflare edge range.
func isCloudflareIP(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range cloudflareNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// offerClientIP is the source IP the per-IP offer throttle keys on. It is the
// transport-level RemoteAddr by default (un-spoofable); only when -trust-cf-ip is
// set AND the peer is a validated Cloudflare edge IP does it trust the real
// client IP from CF-Connecting-IP.
func (r *Relay) offerClientIP(req *http.Request) string {
	peer := clientIP(req) // RemoteAddr host
	if !r.TrustCFIP || !isCloudflareIP(peer) {
		return peer
	}
	if cf := strings.TrimSpace(req.Header.Get("CF-Connecting-IP")); cf != "" {
		if ip := net.ParseIP(cf); ip != nil {
			return ip.String()
		}
	}
	return peer
}
