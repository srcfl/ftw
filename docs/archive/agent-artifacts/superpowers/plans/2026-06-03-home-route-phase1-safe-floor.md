# Home Route — Phase 1: Safe Floor — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the wide-open-dashboard hole on the existing relay owner path so a relay-tunneled (remote) request can never reach the dashboard or control endpoints without a passkey session — while genuine LAN/loopback access stays frictionless.

**Architecture:** A per-process **unforgeable tunnel marker** (random secret, generated at startup, shared in-memory between the API server and the relay long-poll reverse-proxy) labels every relay-tunneled request. `authorizeOwner` grants LAN-bypass only to requests that are *not* marked. A new **global auth-gate** wraps the whole mux: the passkey login surface stays open, everything else requires an authorized owner; unauthenticated remote HTML navigations are served the passkey landing instead of the dashboard. RP-ID stays `relay.fortytwowatts.com` (the host actually serving Phase 1); the `home.fortytwowatts.com` cutover is Phase 4.

**Tech Stack:** Go 1.22+ method-mux, `go-webauthn/webauthn` (already vendored), `net/http/httputil.ReverseProxy`, `crypto/subtle`. No new dependencies.

**Scope note:** This is Phase 1 of the 5-phase spec `docs/superpowers/specs/2026-06-03-home-route-passkey-design.md`. Phases 2–5 (identity spine, usernameless login, multi-home/wallet, P2P transport) get their own plans. Phase 1 is independently shippable and is the security floor everything else builds on.

---

## File structure

| File | Responsibility | Change |
|---|---|---|
| `go/internal/api/api.go` | `Deps` struct; `Handler()`; route table | Add `Deps.TunnelMarker`; wrap `Handler()` with the gate |
| `go/internal/api/api_owner_access.go` | owner auth: `authorizeOwner`, `enrollAllowed` | Add `isTunneled`; exclude tunneled from bypass; harden bootstrap |
| `go/internal/api/api_owner_gate.go` | **NEW** — the global auth-gate middleware + open-path set + login fallback | Create |
| `go/internal/api/api_owner_gate_test.go` | **NEW** — gate + tunnel-marker tests | Create |
| `go/internal/api/api_owner_access_test.go` | existing owner-access tests | Add tunnel-marker regression tests |
| `go/cmd/forty-two-watts/main.go` | process wiring | Generate marker; pass to `Deps` + registration |
| `go/cmd/forty-two-watts/owner_relay_register.go` | relay long-poll + reverse-proxy | Set `X-FTW-Tunnel` marker on every tunneled request |
| `docs/adr/0001-passkey-rp-id.md` | **NEW** — pin the RP-ID decision | Create |

---

### Task 1: Tunnel-origin marker — `Deps.TunnelMarker` + `isTunneled` + bypass exclusion

**Files:**
- Modify: `go/internal/api/api.go` (`Deps` struct, owner-access section ~line 170)
- Modify: `go/internal/api/api_owner_access.go` (`authorizeOwner` ~line 214; add `isTunneled`; import `crypto/subtle`)
- Test: `go/internal/api/api_owner_access_test.go`

- [ ] **Step 1: Write the failing test** — append to `api_owner_access_test.go`:

```go
// A relay-tunnelled request (carrying the trusted tunnel marker) must NOT
// inherit LAN-bypass even though it arrives at a loopback host. This is the
// single most important regression in the whole feature: without it every
// remote request silently skips the passkey.
func TestOwnerAccessTunneledRequestNeverBypasses(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true // bypass ON
	d.TunnelMarker = "test-marker-secret"
	srv := New(d)

	// Marked + loopback host + no cookie → must be treated as remote.
	req := httptest.NewRequest("GET", "/api/owner-access/devices", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("X-FTW-Tunnel", "test-marker-secret")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("tunnelled request must require auth, got %d body=%q", rec.Code, rec.Body.String())
	}
}

// An UNMARKED loopback/LAN request still bypasses when LANBypass is on.
func TestOwnerAccessUnmarkedRequestStillBypasses(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true
	d.TunnelMarker = "test-marker-secret"
	srv := New(d)

	req := httptest.NewRequest("GET", "/api/owner-access/devices", nil)
	req.Host = "127.0.0.1:8080"
	// no X-FTW-Tunnel header
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("unmarked LAN request should bypass, got %d body=%q", rec.Code, rec.Body.String())
	}
}

// A forged marker that doesn't match the per-process secret is NOT treated
// as a tunnel (constant-time compare); it just behaves like a normal LAN
// client (still bypassed) — never an escalation.
func TestOwnerAccessForgedMarkerIsNotTunnel(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true
	d.TunnelMarker = "the-real-secret"
	srv := New(d)

	req := httptest.NewRequest("GET", "/api/owner-access/devices", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("X-FTW-Tunnel", "a-wrong-guess")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("wrong marker must behave as LAN (bypass), got %d", rec.Code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd go && go test ./internal/api/ -run TestOwnerAccessTunneled -v`
Expected: FAIL — `d.TunnelMarker` undefined (compile error).

- [ ] **Step 3: Add the `TunnelMarker` field** in `go/internal/api/api.go`, immediately after the `OwnerAccessLANBypass bool` field (the block ending ~line 170):

```go
	// TunnelMarker is a per-process random secret. The relay long-poll
	// reverse-proxy (cmd/forty-two-watts/owner_relay_register.go) sets it
	// as the X-FTW-Tunnel header on every request it forwards from the
	// relay to the local API server. A request carrying this exact value
	// is therefore known to have arrived via the relay tunnel (remote) and
	// MUST NOT inherit LAN-bypass — even though it lands on a loopback host.
	// Empty disables tunnel detection (pure-LAN deployments with no relay).
	TunnelMarker string
```

- [ ] **Step 4: Add `isTunneled` and exclude tunneled requests from bypass** in `go/internal/api/api_owner_access.go`. Add `"crypto/subtle"` to the import block, then replace the bypass line in `authorizeOwner` and add the helper:

Replace (current `authorizeOwner` head, ~line 214-217):

```go
func (s *Server) authorizeOwner(r *http.Request) (credentialID []byte, ok bool) {
	if s.deps.OwnerAccessLANBypass && isLoopback(r) {
		return []byte("lan-bypass"), true
	}
```

with:

```go
func (s *Server) authorizeOwner(r *http.Request) (credentialID []byte, ok bool) {
	// LAN-bypass applies to genuinely-local requests only. A relay-tunnelled
	// request also lands on a loopback host (the long-poll reverse-proxy
	// connects from 127.0.0.1), so loopback alone is NOT proof of locality —
	// the unforgeable tunnel marker is what distinguishes them.
	if s.deps.OwnerAccessLANBypass && !s.isTunneled(r) {
		return []byte("lan-bypass"), true
	}
```

Add the helper just below `isLoopback` (~line 242):

```go
// isTunneled reports whether the request arrived via the relay long-poll
// reverse-proxy, which stamps every forwarded request with the per-process
// TunnelMarker secret. Constant-time compare so a direct client cannot probe
// for the secret. A direct client that guesses wrong is simply treated as a
// normal (trusted) LAN client — never an escalation.
func (s *Server) isTunneled(r *http.Request) bool {
	m := s.deps.TunnelMarker
	if m == "" {
		return false
	}
	got := r.Header.Get("X-FTW-Tunnel")
	return subtle.ConstantTimeCompare([]byte(got), []byte(m)) == 1
}
```

> Note: `isLoopback` is now unused by `authorizeOwner` but is kept — Task 3 still references the concept in comments, and removing an exported-adjacent helper is out of scope. Leave it.

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd go && go test ./internal/api/ -run 'TestOwnerAccess' -v`
Expected: PASS for the three new tests AND all pre-existing `TestOwnerAccess*` tests (they set no marker, so behaviour is unchanged).

- [ ] **Step 6: Commit**

```bash
git add go/internal/api/api.go go/internal/api/api_owner_access.go go/internal/api/api_owner_access_test.go
git commit -m "feat(owner-access): unforgeable tunnel marker excludes remote requests from LAN-bypass"
```

---

### Task 2: Wire the marker through the process + stamp tunnelled requests

**Files:**
- Modify: `go/cmd/forty-two-watts/main.go` (Deps literal ~line 1373; registration call ~line 1491)
- Modify: `go/cmd/forty-two-watts/owner_relay_register.go` (`runOwnerRelayRegistration`, `runOwnerLongPoll`)

This task has no unit test (it is process wiring exercised end-to-end in `go/test/e2e` and by Task 1's logic). Verify by build + `go vet`.

- [ ] **Step 1: Generate the marker and put it on `Deps`** in `main.go`. Just before the `deps = &api.Deps{` literal (~line 1373), add:

```go
	// Per-process secret stamped on every relay-tunnelled request so the
	// API auth-gate can tell remote (relay) traffic from genuine LAN/loopback.
	tunnelMarker := newTunnelMarker()
```

Inside the `api.Deps{ ... }` literal, alongside the existing `OwnerAccessLANBypass:` line (~line 1414), add:

```go
		TunnelMarker:         tunnelMarker,
```

- [ ] **Step 2: Add the `newTunnelMarker` helper** in `main.go` (near `deriveOwnerHostID`-style helpers; reuses the existing `cryptoRandRead`). Ensure `encoding/hex` is imported in `main.go` (add if missing):

```go
// newTunnelMarker returns a 256-bit random hex secret used to mark
// relay-tunnelled requests (see api.Deps.TunnelMarker). Generated once per
// process; never persisted, never leaves the host.
func newTunnelMarker() string {
	b := make([]byte, 32)
	_, _ = cryptoRandRead(b)
	return hex.EncodeToString(b)
}
```

- [ ] **Step 3: Thread the marker into the registration + long-poll**. In `main.go`, change the call (~line 1491) from:

```go
		go runOwnerRelayRegistration(ctx, relayURL, "site:"+cfg.Site.Name, deriveOwnerHostID(st, cfg.Site.Name))
```

to:

```go
		go runOwnerRelayRegistration(ctx, relayURL, "site:"+cfg.Site.Name, deriveOwnerHostID(st, cfg.Site.Name), tunnelMarker)
```

- [ ] **Step 4: Accept + stamp the marker** in `owner_relay_register.go`. Change `runOwnerRelayRegistration` signature and its `go runOwnerLongPoll(...)` call:

```go
func runOwnerRelayRegistration(ctx context.Context, relayURL, siteID, hostID, tunnelMarker string) {
```

```go
	go runOwnerLongPoll(ctx, relayURL, hostID, tunnelMarker)
```

Change `runOwnerLongPoll` to accept the marker and set it on the proxy via the Director:

```go
func runOwnerLongPoll(ctx context.Context, relayURL, hostID, tunnelMarker string) {
	target, _ := url.Parse("http://127.0.0.1:8080")
	proxy := httputil.NewSingleHostReverseProxy(target)
	// Stamp every forwarded (relay-tunnelled) request with the per-process
	// marker so the local API auth-gate treats it as remote, not LAN. Set()
	// overwrites any value a malicious browser tried to smuggle through the
	// header-preserving tunnel.
	origDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		origDirector(req)
		req.Header.Set("X-FTW-Tunnel", tunnelMarker)
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		http.Error(w, fmt.Sprintf("local api unavailable: %v", err), http.StatusBadGateway)
	}
	relayBaseURL := relayURL
	h := &ownerProxyHandler{proxy: proxy}
	host := newOwnerTunnelHost(relayBaseURL, hostID, h)
	host.Run(ctx)
}
```

- [ ] **Step 5: Verify build + vet**

Run: `cd go && go build ./... && go vet ./cmd/forty-two-watts/ ./internal/api/`
Expected: no output (success).

- [ ] **Step 6: Commit**

```bash
git add go/cmd/forty-two-watts/main.go go/cmd/forty-two-watts/owner_relay_register.go
git commit -m "feat(owner-access): stamp relay-tunnelled requests with the per-process tunnel marker"
```

---

### Task 3: Global auth-gate middleware over the whole mux

**Files:**
- Create: `go/internal/api/api_owner_gate.go`
- Modify: `go/internal/api/api.go` (`Handler()` ~line 231)
- Create: `go/internal/api/api_owner_gate_test.go`

- [ ] **Step 1: Write the failing test** — create `go/internal/api/api_owner_gate_test.go`:

```go
package api

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// gateDeps = minDeps + a WebDir containing the passkey landing, so the gate's
// HTML fallback has something to serve.
func gateDeps(t *testing.T) *Deps {
	t.Helper()
	d := minDeps(t)
	web := t.TempDir()
	if err := os.MkdirAll(filepath.Join(web, "owner-access"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "owner-access", "index.html"),
		[]byte("<!doctype html><title>passkey login</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "index.html"),
		[]byte("<!doctype html><title>DASHBOARD</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	d.WebDir = web
	d.TunnelMarker = "marker"
	return d
}

// Remote (marked) + unauthenticated + API call → 401, never reaches the handler.
func TestGateBlocksRemoteAPI(t *testing.T) {
	d := gateDeps(t)
	d.OwnerAccessLANBypass = true
	srv := New(d)
	req := httptest.NewRequest("GET", "/api/health", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("X-FTW-Tunnel", "marker")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("remote API must be 401, got %d body=%q", rec.Code, rec.Body.String())
	}
}

// Remote (marked) + unauthenticated + HTML navigation → serves the passkey
// landing (200), NOT the dashboard.
func TestGateServesLoginForRemoteHTML(t *testing.T) {
	d := gateDeps(t)
	d.OwnerAccessLANBypass = true
	srv := New(d)
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("X-FTW-Tunnel", "marker")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200 login page, got %d", rec.Code)
	}
	if body := rec.Body.String(); !contains(body, "passkey login") || contains(body, "DASHBOARD") {
		t.Fatalf("expected passkey landing, not dashboard: %q", body)
	}
}

// Local (unmarked) request reaches the dashboard via bypass.
func TestGateAllowsLocalDashboard(t *testing.T) {
	d := gateDeps(t)
	d.OwnerAccessLANBypass = true
	srv := New(d)
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || !contains(rec.Body.String(), "DASHBOARD") {
		t.Fatalf("local must see dashboard, got %d body=%q", rec.Code, rec.Body.String())
	}
}

// The login ceremony stays reachable even when remote + unauthenticated.
func TestGateLoginSurfaceStaysOpen(t *testing.T) {
	d := gateDeps(t)
	d.OwnerAccessLANBypass = true
	srv := New(d)
	req := httptest.NewRequest("POST", "/api/owner-access/login/start", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("X-FTW-Tunnel", "marker")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	// 404 (no devices enrolled) proves the gate let it through to the handler.
	if rec.Code == 401 {
		t.Fatalf("login/start must not be gated; got 401")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd go && go test ./internal/api/ -run TestGate -v`
Expected: FAIL — gate not wired, remote API returns 200 (or 503) instead of 401.

- [ ] **Step 3: Create the gate** — `go/internal/api/api_owner_gate.go`:

```go
// api_owner_gate.go
//
// Global authentication gate for the owner-access remote path. Wraps the
// entire mux: the passkey login surface is always reachable; everything
// else (the dashboard at "/" and every other /api/*) requires an authorized
// owner. Genuine LAN/loopback requests pass via authorizeOwner's LAN-bypass;
// relay-tunnelled (remote) requests are excluded from bypass (see
// isTunneled) and must carry a valid ftw_owner session.
package api

import (
	"net/http"
	"path/filepath"
	"strings"
)

// gate wraps next with the owner auth-gate. Returned by Server.Handler().
func (s *Server) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isOwnerAccessOpenPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := s.authorizeOwner(r); ok {
			next.ServeHTTP(w, r)
			return
		}
		// Unauthenticated and remote (bypass already declined inside
		// authorizeOwner for tunnelled requests). Serve the passkey landing
		// for top-level navigations; 401 for API/asset calls.
		if r.Method == http.MethodGet && acceptsHTML(r) {
			s.serveOwnerLogin(w, r)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// isOwnerAccessOpenPath lists the paths reachable without an authorized
// session — the passkey login surface and its assets. enroll/* is listed
// here but is independently gated by enrollAllowed (incl. bootstrap
// hardening for remote requests). Paths are what the Pi sees: the relay
// strips its /me/<site_id> prefix before forwarding.
func isOwnerAccessOpenPath(p string) bool {
	switch p {
	case "/api/owner-access/login/start",
		"/api/owner-access/login/finish",
		"/api/owner-access/enroll/start",
		"/api/owner-access/enroll/finish",
		"/api/owner-access/whoami":
		return true
	}
	return strings.HasPrefix(p, "/owner-access/")
}

func acceptsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// serveOwnerLogin serves the passkey landing page without leaking the
// dashboard. Uses a relative-safe file serve (no Location header) so it
// works regardless of the relay's /me/<site_id> prefix.
func (s *Server) serveOwnerLogin(w http.ResponseWriter, r *http.Request) {
	landing := filepath.Clean(filepath.Join(s.deps.WebDir, "owner-access", "index.html"))
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	http.ServeFile(w, r, landing)
}
```

- [ ] **Step 4: Wrap `Handler()`** in `go/internal/api/api.go` (line 231). Change:

```go
func (s *Server) Handler() http.Handler { return s.mux }
```

to:

```go
func (s *Server) Handler() http.Handler { return s.gate(s.mux) }
```

- [ ] **Step 5: Run tests**

Run: `cd go && go test ./internal/api/ -run 'TestGate|TestOwnerAccess' -v`
Expected: PASS — all gate tests + all pre-existing owner-access tests.

- [ ] **Step 6: Run the full api package to catch fallout**

Run: `cd go && go test ./internal/api/`
Expected: `ok` — no other handler test regressed (handlers reached via local/no-marker requests still bypass).

- [ ] **Step 7: Commit**

```bash
git add go/internal/api/api_owner_gate.go go/internal/api/api_owner_gate_test.go go/internal/api/api.go
git commit -m "feat(owner-access): global auth-gate wraps the mux; remote hits require passkey"
```

---

### Task 4: Bootstrap hardening — deny first-enrollment over the tunnel

**Files:**
- Modify: `go/internal/api/api_owner_access.go` (`enrollAllowed` ~line 273)
- Test: `go/internal/api/api_owner_access_test.go`

- [ ] **Step 1: Write the failing test** — append to `api_owner_access_test.go`:

```go
// First-enrollment (zero devices) is trust-on-first-use. Over the relay that
// window is internet-exposed, so a remote (marked) request must be refused —
// the first passkey must be enrolled on the LAN.
func TestOwnerAccessBootstrapBlockedOverTunnel(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true
	d.TunnelMarker = "marker"
	srv := New(d)
	req := httptest.NewRequest("POST", "/api/owner-access/enroll/start", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("X-FTW-Tunnel", "marker")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("remote bootstrap must be 403, got %d body=%q", rec.Code, rec.Body.String())
	}
}

// First-enrollment on the LAN (unmarked) is still allowed.
func TestOwnerAccessBootstrapAllowedOnLAN(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true
	d.TunnelMarker = "marker"
	srv := New(d)
	req := httptest.NewRequest("POST", "/api/owner-access/enroll/start", nil)
	req.Host = "127.0.0.1:8080" // unmarked → LAN
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("LAN bootstrap should be allowed, got %d body=%q", rec.Code, rec.Body.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd go && go test ./internal/api/ -run TestOwnerAccessBootstrap -v`
Expected: FAIL — `TestOwnerAccessBootstrapBlockedOverTunnel` gets 200 (bootstrap currently allowed regardless of origin).

- [ ] **Step 3: Harden `enrollAllowed`** in `api_owner_access.go`. Replace the zero-devices branch:

```go
	if len(devices) == 0 {
		return nil // bootstrap path — first device, no auth required
	}
```

with:

```go
	if len(devices) == 0 {
		// Bootstrap (trust-on-first-use): allowed only from the LAN, never
		// over the relay tunnel where the window would be internet-exposed.
		if s.isTunneled(r) {
			return errors.New("first enrollment must be performed on the local network")
		}
		return nil
	}
```

- [ ] **Step 4: Run tests**

Run: `cd go && go test ./internal/api/ -run 'TestOwnerAccessBootstrap|TestOwnerAccessEnroll' -v`
Expected: PASS — remote bootstrap 403, LAN bootstrap 200, and the existing enroll tests (no marker) unchanged.

- [ ] **Step 5: Commit**

```bash
git add go/internal/api/api_owner_access.go go/internal/api/api_owner_access_test.go
git commit -m "feat(owner-access): gate first-enrollment bootstrap to the LAN (deny over tunnel)"
```

---

### Task 5: Clone-guard regression — sign-count `0→0` is benign

**Files:**
- Test: `go/internal/state/trusted_devices_test.go` (create if absent)

This locks the spec §15 invariant at the storage layer that the owner-access path relies on. The full WebAuthn assertion clone-check lives in the `go-webauthn` library and is exercised in `go/test/e2e`; here we assert our persistence helper treats a constant-zero counter correctly (does not corrupt or reject it).

- [ ] **Step 1: Write the failing/observing test** — append to (or create) `go/internal/state/trusted_devices_test.go`:

```go
package state

import (
	"path/filepath"
	"testing"
)

// Modern synced passkeys (iCloud Keychain, Google Password Manager) report
// signCount == 0 on every login. Persisting 0 after a prior 0 must be a
// no-corruption no-op, never treated as a clone (a clone is a *decrease*
// from a previously-positive value, handled by the webauthn lib upstream).
func TestUpdateSignCountConstantZeroIsBenign(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	cred := []byte("cred-zero")
	if err := st.SaveTrustedDevice(TrustedDevice{
		CredentialID: cred, PublicKey: []byte("k"), SignCount: 0, FriendlyName: "phone",
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Two consecutive logins, both reporting 0.
	if err := st.UpdateTrustedDeviceSignCount(cred, 0, 1000); err != nil {
		t.Fatalf("update 1: %v", err)
	}
	if err := st.UpdateTrustedDeviceSignCount(cred, 0, 2000); err != nil {
		t.Fatalf("update 2: %v", err)
	}
	devs, err := st.LoadTrustedDevices()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(devs) != 1 || devs[0].SignCount != 0 {
		t.Fatalf("expected 1 device with signCount 0, got %+v", devs)
	}
	if devs[0].LastUsedMs != 2000 {
		t.Fatalf("expected LastUsedMs updated to 2000, got %d", devs[0].LastUsedMs)
	}
}
```

- [ ] **Step 2: Run test**

Run: `cd go && go test ./internal/state/ -run TestUpdateSignCountConstantZeroIsBenign -v`
Expected: PASS if `UpdateTrustedDeviceSignCount` already updates `LastUsedMs` and tolerates equal counts. If it FAILS (e.g. it rejects non-increasing counts or doesn't bump `LastUsedMs`), that is a real bug — fix `UpdateTrustedDeviceSignCount` in `go/internal/state/trusted_devices.go` so an equal (0→0) count updates `last_used_ms` without error, then re-run.

- [ ] **Step 3: Commit**

```bash
git add go/internal/state/trusted_devices_test.go go/internal/state/trusted_devices.go
git commit -m "test(state): lock constant-zero sign-count as benign for synced passkeys"
```

---

### Task 6: ADR — pin the WebAuthn RP-ID decision (one-way door)

**Files:**
- Create: `docs/adr/0001-passkey-rp-id.md`

No code, no test — a durable decision record so nobody flips RP-ID to the apex or enrolls real passkeys under `relay.*`.

- [ ] **Step 1: Write the ADR** — `docs/adr/0001-passkey-rp-id.md`:

```markdown
# ADR 0001 — WebAuthn RP-ID for owner remote access

- Status: Accepted (2026-06-03)
- Context: `docs/superpowers/specs/2026-06-03-home-route-passkey-design.md`

## Decision

The production WebAuthn Relying Party ID for owner passkeys is the dedicated
host **`home.fortytwowatts.com`** — never the apex `fortytwowatts.com`.

## Why this is a one-way door

The RP-ID is cryptographically baked into every passkey at creation. Changing
it later silently invalidates every enrolled credential and forces full
re-enrollment. Therefore:

1. **Never set RP-ID to the apex.** An apex RP-ID would place the credential on
   `fortytwowatts.com` and make it presentable on every sibling subdomain —
   exactly what the project's dedicated-domain rule forbids. A host is trivially
   a registrable-domain-suffix of its own origin, so `home.fortytwowatts.com`
   satisfies the WebAuthn suffix rule on its own.
2. **Do NOT enroll real owner passkeys under `relay.fortytwowatts.com`.** A
   passkey created with RP-ID `relay.*` will not work at `home.*`. Phase 1 runs
   on the existing `relay.fortytwowatts.com/me/<site_id>` path purely for
   security-floor hardening; production passkey enrollment begins in Phase 4 on
   the `home.fortytwowatts.com` host.

## Sequencing

- **Phase 1–3:** RP-ID stays `relay.fortytwowatts.com` (the host actually
  serving the page). `FTW_OWNER_ACCESS_RPID` remains the override knob.
- **Phase 4:** when `home.fortytwowatts.com` exists (host + wildcard TLS +
  routing), flip the default to `home.fortytwowatts.com` and serve enrollment
  from that origin. This is the moment real passkeys are first enrolled.
```

- [ ] **Step 2: Commit**

```bash
git add docs/adr/0001-passkey-rp-id.md
git commit -m "docs(adr): pin passkey RP-ID to home.fortytwowatts.com, never the apex"
```

---

## Phase 1 verification (run before declaring done)

- [ ] `cd go && go test ./internal/api/ ./internal/state/` → `ok` both.
- [ ] `cd go && go vet ./... && go build ./...` → clean.
- [ ] Manual reasoning check against the threat model:
  - Remote (relay-tunnelled) request to `/` or `/api/*` with no session → blocked (HTML→login, API→401). ✓ Tasks 1+3.
  - Remote request can't self-bootstrap an owner passkey → 403. ✓ Task 4.
  - Genuine LAN/loopback dashboard still works with no passkey → ✓ Tasks 1+3 (unmarked → bypass).
  - Forged/guessed marker never escalates → ✓ Task 1 (constant-time compare).

---

## Self-review (done at authoring time)

- **Spec coverage (Phase 1 slice of §8 + §15):** §8.1 global gate → Task 3; §8.2 tunnel-vs-LAN → Tasks 1+2; §8.3 bootstrap → Task 4; §15 origin/clone-guard → Task 5 (clone-guard at storage layer; full origin-acceptance test deferred to Phase 3 when discoverable login + the `home.*` origin land, since Phase 1 keeps the existing `relay.*` origin already covered by current tests); RP-ID pin → Task 6 (ADR). The `home.*` default flip, wildcard routing, durable directory, and identity-spine work are explicitly Phases 2–4, not this plan.
- **Placeholder scan:** none — every step has concrete code/commands.
- **Type consistency:** `Deps.TunnelMarker string`, `(*Server).isTunneled(*http.Request) bool`, `X-FTW-Tunnel` header, `isOwnerAccessOpenPath(string) bool`, `(*Server).gate(http.Handler) http.Handler` used consistently across Tasks 1–4. `runOwnerRelayRegistration` / `runOwnerLongPoll` signatures updated in lockstep (Task 2).
```
