package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
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

// The public home route must fail closed without a pinned key.
func TestRequireHomePin(t *testing.T) {
	cases := []struct {
		host, site, key string
		tofu            bool
		wantErr         bool
	}{
		{"", "", "", false, false},                    // no home host configured → ok
		{"home.test", "site:x", "abcd", false, false}, // pinned key → ok
		{"home.test", "site:x", "", true, false},      // explicit TOFU override → ok
		{"home.test", "site:x", "", false, true},      // public host, no pin, no override → error
		{"", "site:x", "", false, true},               // site alone without pin → error
	}
	for i, c := range cases {
		err := requireHomePin(c.host, c.site, c.key, c.tofu)
		if (err != nil) != c.wantErr {
			t.Errorf("case %d (%+v): err=%v wantErr=%v", i, c, err, c.wantErr)
		}
	}
}

// An oversize body to an unauthenticated control endpoint must be rejected
// (bounded), never buffered unboundedly.
func TestRelayRejectsOversizeControlBody(t *testing.T) {
	srv := httptest.NewServer(newTestRelay().Handler())
	defer srv.Close()
	big := strings.Repeat("a", 200*1024) // 200 KiB > 64 KiB control cap
	body := `{"host_id":"h","token":"x","junk":"` + big + `"}`
	resp, err := http.Post(srv.URL+"/tunnel/register", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("oversize register body must be rejected, got %d", resp.StatusCode)
	}
}

// TokenRegistry must clamp an attacker's near-infinite TTL and cap the live
// token count so /tunnel/register can't exhaust relay memory.
func TestTokenRegistryCaps(t *testing.T) {
	reg := NewTokenRegistry()
	// TTL clamp: a 100-year TTL is clamped to maxTokenTTL.
	tok, err := reg.Register(TokenRegistration{HostID: "h", Token: "clamp", TTL: 100 * 365 * 24 * time.Hour, ApprovalCode: "1"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if d := time.Until(tok.ExpiresAt()); d > maxTokenTTL+time.Minute {
		t.Fatalf("TTL not clamped: expires in %v, want <= %v", d, maxTokenTTL)
	}
	// Count cap: fill to the limit, then the next distinct token is refused.
	for i := 1; i < maxLiveTokens; i++ {
		if _, err := reg.Register(TokenRegistration{HostID: "h", Token: fmtTok(i), TTL: time.Hour, ApprovalCode: "1"}); err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
	}
	// At the cap, a new registration evicts the OLDEST pending token ("clamp")
	// and succeeds — a flood of unapproved tokens must not permanently lock out
	// real pair sessions.
	if _, err := reg.Register(TokenRegistration{HostID: "h", Token: "newone", TTL: time.Hour, ApprovalCode: "1"}); err != nil {
		t.Fatalf("at-cap register should evict-and-succeed, got %v", err)
	}
	if _, err := reg.Get("newone"); err != nil {
		t.Fatalf("new token should be present after evict: %v", err)
	}
	if _, err := reg.Get("clamp"); err == nil {
		t.Fatal("oldest pending token should have been evicted at cap")
	}
}

func fmtTok(i int) string { return "t-" + strconv.Itoa(i) }

// OwnerRegistry caps TOFU sites and GC evicts stale non-pinned ones, never the
// operator-pinned home site.
func TestOwnerRegistryCapAndGC(t *testing.T) {
	reg := NewOwnerRegistry()
	reg.Pin("site:Home", "homekey")
	if err := reg.Register("site:Home", "host-home", "homekey"); err != nil {
		t.Fatalf("home register: %v", err)
	}
	if err := reg.Register("site:other", "host-o", "otherkey"); err != nil {
		t.Fatalf("other register: %v", err)
	}
	// GC with a zero maxAge evicts the stale non-pinned site but keeps the pin.
	if n := reg.GC(0); n != 1 {
		t.Fatalf("GC removed %d, want 1 (the non-pinned site)", n)
	}
	if _, err := reg.Lookup("site:Home"); err != nil {
		t.Fatalf("home site must survive GC: %v", err)
	}
	if _, err := reg.Lookup("site:other"); err == nil {
		t.Fatal("stale non-pinned site should be evicted")
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
