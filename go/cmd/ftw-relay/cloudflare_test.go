package main

import (
	"net/http"
	"testing"
)

func TestIsCloudflareIP(t *testing.T) {
	for _, ip := range []string{"104.16.0.1", "172.64.0.5", "2606:4700::1"} {
		if !isCloudflareIP(ip) {
			t.Errorf("%s should be a Cloudflare edge IP", ip)
		}
	}
	for _, ip := range []string{"8.8.8.8", "203.0.113.7", "2001:4860:4860::8888", "not-an-ip", ""} {
		if isCloudflareIP(ip) {
			t.Errorf("%s must NOT be a Cloudflare edge IP", ip)
		}
	}
}

func TestOfferClientIP_TrustedProxy(t *testing.T) {
	mk := func(remote, cfHdr string) *http.Request {
		req, _ := http.NewRequest("POST", "/signal/site:Home/offer", nil)
		req.RemoteAddr = remote
		if cfHdr != "" {
			req.Header.Set("CF-Connecting-IP", cfHdr)
		}
		return req
	}

	// trust OFF → always the un-spoofable RemoteAddr, even with a CF header.
	off := &Relay{TrustCFIP: false}
	if got := off.offerClientIP(mk("104.16.0.1:443", "9.9.9.9")); got != "104.16.0.1" {
		t.Errorf("trust off: got %q, want 104.16.0.1 (must ignore CF header)", got)
	}

	on := &Relay{TrustCFIP: true}
	// trust ON + peer IS a Cloudflare edge IP → the real client IP from CF-Connecting-IP.
	if got := on.offerClientIP(mk("104.16.0.1:443", "203.0.113.7")); got != "203.0.113.7" {
		t.Errorf("trust on + CF peer: got %q, want 203.0.113.7", got)
	}
	// trust ON + peer is NOT Cloudflare (attacker hitting the origin directly and
	// spoofing CF-Connecting-IP) → ignore the header, use the un-spoofable RemoteAddr.
	if got := on.offerClientIP(mk("8.8.8.8:443", "203.0.113.7")); got != "8.8.8.8" {
		t.Errorf("trust on + non-CF peer (spoof attempt): got %q, want 8.8.8.8", got)
	}
	// trust ON + CF peer but no header → fall back to the peer.
	if got := on.offerClientIP(mk("104.16.0.1:443", "")); got != "104.16.0.1" {
		t.Errorf("trust on + CF peer, no header: got %q, want 104.16.0.1", got)
	}
}
