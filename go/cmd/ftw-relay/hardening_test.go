package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

// readBodyLimited must reject a body over the cap rather than silently
// truncating, and accept one exactly at the cap.
func TestReadBodyLimited(t *testing.T) {
	if _, err := readBodyLimited(strings.NewReader("hello"), 10); err != nil {
		t.Fatalf("under cap: unexpected err %v", err)
	}
	if _, err := readBodyLimited(strings.NewReader("0123456789"), 10); err != nil {
		t.Fatalf("exactly at cap: unexpected err %v", err)
	}
	if _, err := readBodyLimited(strings.NewReader("0123456789X"), 10); err != errBodyTooLarge {
		t.Fatalf("over cap: err = %v, want errBodyTooLarge", err)
	}
}

// Active reports freshness so homeForward can serve the offline page instead of
// hanging on a dead tunnel.
func TestOwnerRegistryActiveFreshness(t *testing.T) {
	reg := NewOwnerRegistry()
	if _, registered, _ := reg.Active("site-A", time.Minute); registered {
		t.Fatal("unregistered site reported as registered")
	}
	if err := reg.Register("site-A", "host-1", "deadbeef"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if host, registered, fresh := reg.Active("site-A", time.Minute); !registered || !fresh || host != "host-1" {
		t.Fatalf("just-registered: host=%q registered=%v fresh=%v", host, registered, fresh)
	}
	// maxAge 0 → anything older than "now" is stale; proves the staleness branch.
	if _, registered, fresh := reg.Active("site-A", 0); !registered || fresh {
		t.Fatalf("zero maxAge should be registered-but-stale, got registered=%v fresh=%v", registered, fresh)
	}
}

// GC drops expired/revoked tokens and keeps live ones.
func TestTokenRegistryGC(t *testing.T) {
	reg := NewTokenRegistry()
	_, _ = reg.Register(TokenRegistration{HostID: "h", Token: "live", TTL: time.Hour, ApprovalCode: "1"})
	_, _ = reg.Register(TokenRegistration{HostID: "h", Token: "dead", TTL: time.Millisecond, ApprovalCode: "1"})
	_, _ = reg.Register(TokenRegistration{HostID: "h", Token: "revoked", TTL: time.Hour, ApprovalCode: "1"})
	reg.Revoke("revoked")
	time.Sleep(5 * time.Millisecond) // let "dead" expire

	if n := reg.GC(); n != 2 {
		t.Fatalf("GC removed %d, want 2 (dead + revoked)", n)
	}
	if _, err := reg.Get("live"); err != nil {
		t.Fatalf("live token must survive GC: %v", err)
	}
	if _, err := reg.Get("dead"); err == nil {
		t.Fatal("expired token should be gone after GC")
	}
}

// home.* must serve the styled offline page (not a raw timeout) when the Pi has
// never registered.
func TestHomeForwardServesOfflinePage(t *testing.T) {
	relay := &Relay{
		Queue: tunnel.NewQueue(), Tokens: NewTokenRegistry(), Owners: NewOwnerRegistry(),
		HomeHost: "home.test", HomeSite: "site:e2e",
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.Host = "home.test"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("offline status = %d, want 503", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("offline page content-type = %q, want text/html", ct)
	}
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	html := string(body[:n])
	if !strings.Contains(html, "forty-two-watts") || !strings.Contains(html, `id="retry"`) {
		t.Fatalf("offline page missing brand/retry affordance:\n%s", html)
	}
}
