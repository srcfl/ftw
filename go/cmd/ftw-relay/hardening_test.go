package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/tunnel"
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
		host, site, key, web string
		tofu                 bool
		wantErr              bool
	}{
		{"", "", "", "", false, false},                        // no home host configured → ok
		{"home.test", "site:x", "abcd", "/web", false, false}, // pinned key + web → ok
		{"home.test", "site:x", "abcd", "", false, true},      // pinned key but NO -home-web → error
		{"home.test", "site:x", "", "/web", true, false},      // explicit TOFU override → ok
		{"home.test", "site:x", "", "/web", false, true},      // has web but no pin → error
		{"", "site:x", "", "", false, true},                   // site alone without pin/web → error
	}
	for i, c := range cases {
		err := requireHomePin(c.host, c.site, c.key, c.web, c.tofu)
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

// The host poll endpoints require the registration-minted poll token, so a
// caller that only learned host_id can't poll for (and steal) the host's
// tunneled traffic, nor can an unregistered host_id create waiters.
func TestTunnelPollRequiresToken(t *testing.T) {
	r := newTestRelay()
	r.PollTimeout = 50 * time.Millisecond
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	secret := mustIssue(t, r.Polls, "host-x")

	poll := func(host, token string) int {
		req, _ := http.NewRequest("GET", srv.URL+"/tunnel/"+host+"/next", nil)
		if token != "" {
			req.Header.Set(tunnel.PollSecretHeader, token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if code := poll("host-x", ""); code != http.StatusUnauthorized {
		t.Errorf("poll without token: got %d, want 401", code)
	}
	if code := poll("host-x", "wrong-token"); code != http.StatusUnauthorized {
		t.Errorf("poll with wrong token: got %d, want 401", code)
	}
	if code := poll("never-registered", secret); code != http.StatusUnauthorized {
		t.Errorf("poll for unregistered host: got %d, want 401", code)
	}
	if code := poll("host-x", secret); code != http.StatusNoContent {
		t.Errorf("poll with correct token: got %d, want 204", code)
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
	if !strings.Contains(html, "FTW") || !strings.Contains(html, `id="retry"`) {
		t.Fatalf("offline page missing brand/retry affordance:\n%s", html)
	}
}

// PublicKeyForSite returns the pinned/TOFU'd ES256 key for a site (the public
// key the relay holds), or ok=false when the site has no key. signalIdentity
// (the per-site /signal/{site}/identity convenience read) is built on it.
func TestOwnerRegistryPublicKeyForSite(t *testing.T) {
	reg := NewOwnerRegistry()
	if _, ok := reg.PublicKeyForSite("site:unknown"); ok {
		t.Fatal("unknown site must report ok=false")
	}
	// TOFU register pins the key.
	if err := reg.Register("site:A", "host-1", "deadbeefkey"); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, ok := reg.PublicKeyForSite("site:A")
	if !ok || got != "deadbeefkey" {
		t.Fatalf("PublicKeyForSite(site:A) = %q,%v want deadbeefkey,true", got, ok)
	}
	// Pin (operator) is also visible even with no host mapping yet.
	reg.Pin("site:P", "pinnedkey")
	if got, ok := reg.PublicKeyForSite("site:P"); !ok || got != "pinnedkey" {
		t.Fatalf("PublicKeyForSite(site:P) = %q,%v want pinnedkey,true", got, ok)
	}
}

// The Relay carries the multi-tenant toggle and the wallet-blob store so the
// handler layer can branch on them. This compiles only once the fields exist.
func TestRelayMultiTenantFields(t *testing.T) {
	dir := t.TempDir()
	store, err := NewWalletBlobStore(dir, 65536, 1024)
	if err != nil {
		t.Fatalf("NewWalletBlobStore: %v", err)
	}
	r := &Relay{MultiTenant: true, WalletBlobs: store}
	if !r.MultiTenant {
		t.Fatal("MultiTenant field not set")
	}
	if r.WalletBlobs == nil {
		t.Fatal("WalletBlobs field not set")
	}
}

// Under -multi-tenant the relay still requires -home-web (the tiny bootstrap
// loader must be relay-served so an anonymous GET never reaches a Pi), but
// device-key enforcement is optional: when it is off, the relay forwards
// signaling by site_id and the Pi gates access via passkey over the E2E channel.
// -home-site/-home-pubkey are NOT required.
func TestRequireMultiTenant(t *testing.T) {
	cases := []struct {
		name             string
		multiTenant      bool
		requireDeviceKey bool
		homeWeb          string
		wantErr          bool
	}{
		{"off — not multi-tenant", false, false, "", false},
		{"multi-tenant + device-key + web → ok", true, true, "/web", false},
		{"multi-tenant WITHOUT device-key → ok", true, false, "/web", false},
		{"multi-tenant WITHOUT -home-web → refuse", true, true, "", true},
		{"multi-tenant missing both → refuse", true, false, "", true},
	}
	for _, c := range cases {
		err := requireMultiTenant(c.multiTenant, c.requireDeviceKey, c.homeWeb)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v wantErr=%v", c.name, err, c.wantErr)
		}
	}
}

// user_handle is an opaque base64url token (WebAuthn userHandle): [A-Za-z0-9_-],
// length 43..86. Anything else is rejected BEFORE it is used as a store/file key
// (no path traversal, no oversize).
func TestValidUserHandle(t *testing.T) {
	ok43 := strings.Repeat("a", 43)
	ok86 := strings.Repeat("Z", 86)
	cases := []struct {
		in   string
		want bool
	}{
		{ok43, true},
		{ok86, true},
		{strings.Repeat("a", 42), false},         // too short
		{strings.Repeat("a", 87), false},         // too long
		{"", false},                              // empty
		{strings.Repeat("a", 42) + ".", false},   // '.' not in charset
		{strings.Repeat("a", 42) + "/", false},   // '/' traversal char
		{"../" + strings.Repeat("a", 40), false}, // traversal attempt
		{strings.Repeat("a", 42) + "+", false},   // std-b64 '+' not allowed
		{strings.Repeat("-", 43), true},          // '-' and '_' allowed
		{strings.Repeat("_", 43), true},
	}
	for _, c := range cases {
		if got := validUserHandle(c.in); got != c.want {
			t.Errorf("validUserHandle(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// Smoke: a multi-tenant Relay built the way main() builds it serves the landing,
// the wallet endpoints, and the per-site identity together — the full multi-tenant
// surface wired on one mux.
func TestMultiTenantSurfaceWiredTogether(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "remote-loader.html"), []byte("<h1>LOADER</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := NewWalletBlobStore(t.TempDir(), 65536, 1024)
	if err != nil {
		t.Fatalf("NewWalletBlobStore: %v", err)
	}
	id := newTestIdentity(t)
	owners := NewOwnerRegistry()
	if err := owners.Register("site:Z", "host-z", id.PublicKeyHex()); err != nil {
		t.Fatalf("register: %v", err)
	}
	relay := &Relay{
		Queue: tunnel.NewQueue(), Tokens: NewTokenRegistry(), Owners: owners,
		Polls: NewPollSecrets(), Signals: NewSignalMailbox(), Challenges: NewSignalChallenges(),
		MultiTenant: true, RequireDeviceKey: true, HomeHost: "home.test", HomeWeb: dir, WalletBlobs: store,
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()
	get := func(path string) int {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.Host = "home.test"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := get("/"); c != 200 {
		t.Fatalf(`"/" = %d, want 200`, c)
	}
	if c := get("/signal/site:Z/identity"); c != 200 {
		t.Fatalf("/signal/site:Z/identity = %d, want 200", c)
	}
	if c := get("/api/identity"); c != http.StatusForbidden {
		t.Fatalf("/api/identity = %d, want 403", c)
	}
}
