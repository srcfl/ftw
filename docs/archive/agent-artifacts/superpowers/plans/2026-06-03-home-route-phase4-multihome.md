# Home Route — Phase 4: Multi-home on a stateless relay — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One usernameless login at `home.fortytwowatts.com` resolves a *person* (wallet `W`) to **all their currently-online homes** — with the relay still a pure **stateless byte-pipe**. The relay routes the assertion by wallet to whichever Pis are connected; each Pi verifies the passkey itself; an offline Pi simply isn't in the list. Enrolling once and *claiming* a second/third home replicates the one passkey's public key to each claimed Pi.

**Architecture:** Each Pi, on its periodic relay registration, announces `{site_label, wallet W}` (the Phase-2 wallet handle, known once a passkey is enrolled). The relay holds these announces as **live connection metadata only** (gone on disconnect — never a persistent directory). To resolve a person → homes: the browser's assertion carries `userHandle = W`; the relay groups its live Pis by `W` (routing, unverified); routes the assertion to one live Pi which **verifies** (it holds the replicated pubkey); and **only after** one verification reveals `W`'s live homes (verify-then-reveal — an unverified `userHandle` never discloses ownership). Claiming a new home requires *both* LAN presence at the new Pi *and* a verified `home.*` session; the relay brokers a one-shot ephemeral Pi→Pi credential transfer. RP-ID cuts over from `relay.fortytwowatts.com` to `home.fortytwowatts.com` — the ADR's one-way door, clean because no *real* passkeys exist under `relay.*` (Phases 1–3 were hardening).

**Tech Stack:** Go 1.22+ method-mux, `go-webauthn/webauthn` (already vendored), the existing `internal/tunnel` long-poll queue, SQLite via `internal/state`, `crypto/subtle` / `crypto/rand`. No new dependencies. The relay rides the existing `internal/tunnel.Queue`; the new `home.*` vhost is additional handlers on the same `Relay` struct, not a second binary.

**Scope note:** This is Phase 4 of the 5-phase spec family. It builds directly on Phases 1–3 (`go/internal/api/api_owner_access.go`, `go/internal/api/api_owner_gate.go`, the relay `/me/*` family in `go/cmd/ftw-relay/`, and `docs/adr/0001-passkey-rp-id.md`). Phase 5 (WebRTC/QUIC P2P transport) is out of scope and rides on the same long-poll tunnel for now. Source design: `docs/superpowers/specs/2026-06-03-home-route-phase4-multihome-design.md`.

---

## File structure

| File | Responsibility | Change |
|---|---|---|
| `go/internal/state/trusted_devices.go` | passkey persistence | Add `Origin` field (`local`/`replicated`); thread through Save/Load/Lookup |
| `go/internal/state/store.go` | migrations | Add `trusted_devices.origin` column via `addColumnIfMissing` |
| `go/internal/state/trusted_devices_test.go` | persistence tests | Test origin round-trips + default = `local` |
| `go/internal/api/api_owner_access.go` | enroll/finish; replicate-store; site-label getter | Stamp `Origin: local` on enroll; add replicated-credential ingest helper + `siteLabel()` |
| `go/internal/api/api_owner_announce.go` | **NEW** — host-side announce payload + replicate-claim handlers | Create |
| `go/internal/api/api_owner_announce_test.go` | **NEW** — announce + replicate handler tests | Create |
| `go/cmd/forty-two-watts/owner_relay_register.go` | host-side relay registration | Send `{site_label, wallet}` on `/me/register`; gate on having ≥1 passkey |
| `go/cmd/ftw-relay/owners.go` | relay live announce table | Extend registration to carry `site_label` + `wallet`; group-by-wallet lookup |
| `go/cmd/ftw-relay/owners_test.go` | **NEW** — registry group-by-wallet tests | Create |
| `go/cmd/ftw-relay/handlers.go` | relay `Relay` struct + mux | `/me/register` accepts new fields; register `home.*` routes |
| `go/cmd/ftw-relay/home.go` | **NEW** — `home.*` vhost: login surface, verify-then-reveal, selector, claim broker | Create |
| `go/cmd/ftw-relay/home_test.go` | **NEW** — verify-then-reveal + selector + claim tests | Create |
| `go/cmd/ftw-relay/main.go` | relay process wiring | Add `-home-host` flag + vhost host-routing |
| `go/cmd/forty-two-watts/main.go` | host process wiring | Default `OwnerAccessRPID` → `home.fortytwowatts.com`; pass site label to registration |
| `web/owner-access/index.html` | selector UI | Add the live-homes selector list after verify-then-reveal |
| `config.example.yaml` | operator docs | Document the `home.*` RP-ID + `FTW_OWNER_ACCESS_RPID` cutover |
| `docs/relay-deploy.md` | operator docs | `home.fortytwowatts.com` vhost + DNS A-record + TLS |
| `docs/adr/0001-passkey-rp-id.md` | decision record | Flip the "Phase 4 cutover" note from pending → done |

> **Ordering rationale.** Storage (Task 1) → host announce/replicate surface (Tasks 2–3) → relay live table (Task 4) → relay `home.*` vhost incl. verify-then-reveal + selector + claim broker (Tasks 5–7) → process wiring + RP-ID cutover (Task 8) → selector UI (Task 9) → docs/ADR (Task 10). Each task is independently testable; the relay vhost tasks (5–7) use an in-process fake host so they don't need a live Pi.

---

## Open questions (resolve before / during the named task — do NOT guess silently)

These are the genuinely-hard bits the design doc flagged. Each is tied to the task where it must be answered; if the answer turns out differently from the assumption below, adjust that task and note it in the task's self-review.

- **OQ-1 (Task 4/5) — announce wire format & freshness.** The design says "extend the existing `/me/register`". The host already POSTs `/me/register` every 60s (`runOwnerRelayRegistration`, `owner_relay_register.go:88`). **Assumption:** add optional `site_label` + `wallet` fields to `meRegisterRequest`; the relay stores them as live metadata keyed on `host_id`, refreshed on every re-register, and expired when the announce is older than a TTL (≈3× the 60s register interval = 180s) so a crashed Pi that never closed its tunnel ages out of the selector. **Open:** is 60s register cadence + 180s TTL tight enough that the selector never shows a dead home for more than one register interval? If a snappier liveness signal is wanted, drive the announce off the long-poll connection (every `/tunnel/<host>/next` re-arm) instead of the 60s register tick — but that couples the announce to the tunnel package. Decide in Task 4; the plan codes the 180s-TTL-on-register version and leaves a `// OQ-1` marker.

- **OQ-2 (Task 5) — verify-then-reveal round trip over the tunnel.** The relay must route the browser's assertion to *one* live Pi of `W`, get a boolean verify result, and only then reveal the home list. **Assumption:** the relay forwards the standard `POST /api/owner-access/login/finish` body to the chosen Pi *through the existing `Queue.Enqueue`* (same mechanism `meForward` already uses), and treats HTTP 200 from the Pi as "verified". The Pi's existing `handleOwnerLoginFinish` already does the WebAuthn verification and returns 200 + sets the `ftw_owner` cookie. **Open:** the Pi sets a session cookie scoped to *that Pi*. For the selector step the relay only needs the *verify boolean* + the wallet, not that Pi's session — but the browser will later "open" a chosen home, which needs *that* home's own login ceremony (P4-1, one tap per home). So the verify-then-reveal `login/finish` is a *throwaway* verification whose cookie we discard; the real per-home session is minted when the user taps "open". Confirm this is acceptable UX (two Face-ID taps when opening the first home: one to reveal the list, one to open the chosen home) — the design's Flow A shows exactly this. If we want to collapse to one tap for the common single-home case, see OQ-5.

- **OQ-3 (Task 6) — the claim ephemeral channel.** "A one-shot, authenticated, non-persisted message Pi→Pi." **Assumption:** broker it as two tunneled requests with the relay holding **zero** durable state: (a) browser (proven wallet `W` via a fresh verify) + 4-digit LAN-presence code from the new Pi → relay routes a `POST /api/owner-access/replicate/export` to an *old* live Pi of `W`, which returns the wallet's credential records `{credId, pubkey, aaguid, sign_count, transports, friendly_name}`; (b) relay immediately routes `POST /api/owner-access/replicate/import` (carrying those records + the LAN code) to the *new* Pi, which validates the code and stores them with `Origin: replicated`. The records live only in-flight in the relay's `Queue` (already the case for every tunneled body) — nothing is persisted. **Open:** what proves "old Pi" trust to hand out pubkeys? The export request must arrive *via the relay carrying the tunnel marker* (so it's the relay asking, on behalf of a wallet the relay just verified) AND the exporting Pi must confirm the requested wallet matches its own `ownerWalletHandle()`. Pubkeys are not secret (they verify, they don't sign), so the sensitive part is the *import* side gating on LAN presence, not the export side. Confirm: is exporting your own wallet's pubkeys to a relay-brokered request acceptable, or do we additionally require the *old* Pi to have a live verified browser session too? The design's Flow B proves `W` once (the browser's `home.*` session); the plan assumes that single proof + the new-Pi LAN code is sufficient, and marks `// OQ-3`.

- **OQ-4 (Task 6) — LAN-presence code lifecycle.** The new Pi shows a 4-digit code. **Assumption:** reuse the *shape* of the ftw-pair / relay 4-digit code (`MaxApprovalAttempts = 5`, short TTL), minted by the new Pi's API on `POST /api/owner-access/claim/begin` (LAN-only, gated by `isTunneled` == false like enroll bootstrap), displayed on its local enroll page, and verified inside `replicate/import`. **Open:** the code must be entered in the *browser* (which is on the new Pi's LAN per Flow B) and travel to the *new Pi* via the relay — but the browser is talking to `home.*`, not the new Pi's LAN address. So the code is the shared secret proving "the human is physically at the new Pi": the new Pi mints it locally, shows it on its LAN screen, the human types it into `home.*`, the relay carries it to the new Pi's `import`, the new Pi checks it. This is exactly the existing relay landing-page pattern (`publicApprove`) lifted to the claim flow. Confirm the threat model: a remote attacker who proved a wallet but is NOT at the new Pi's LAN cannot read the code → cannot complete import. ✓ assuming the code is shown only on the new Pi's LAN UI.

- **OQ-5 (Task 5, optional) — single-home fast path.** When `W` has exactly one live home, the reveal+open could be one tap. **Assumption:** ship the always-two-tap version (simpler, matches Flow A literally); leave a `// OQ-5` marker for a later "if len(homes)==1 mint session directly" optimization. Do NOT build it in Phase 4 unless the reviewer asks.

- **OQ-6 (Task 8) — RP-ID cutover blast radius.** Flipping the default RP-ID invalidates any passkey enrolled under `relay.*`. Per ADR-0001 + the design, *no real passkeys exist under `relay.*`* (Phases 1–3 were hardening). **Assumption:** safe to flip the default. **Open:** confirm no field tester (e.g. Erik) enrolled a *real* passkey under `relay.*` during Phase 1–3 dogfooding. If any did, they must re-enroll on LAN after the cutover — call it out in the changeset + `docs/relay-deploy.md` upgrade note. This is a *coordination* question, not a code one; resolve with Fredrik before merging Task 8.

- **OQ-7 (Task 5/7) — revocation propagation.** Un-claiming (removing a wallet from a Pi) and propagating a credential removal across your Pis is "best-effort; offline Pis reconcile on reconnect." **Assumption:** Phase 4 ships *storage* support (the `Origin` flag makes replicated creds auditable + individually deletable via the existing `DELETE /api/owner-access/devices/{id}`) but does **NOT** build cross-Pi revocation fan-out. A replicated credential deleted on one Pi stays on the others until manually removed. Flag this clearly in the changeset as a known limitation; full reconcile-on-reconnect is a follow-up. Do NOT build the fan-out in Phase 4.

---

### Task 1: Storage — `Origin` flag on `trusted_devices` (locally-enrolled vs replicated)

**Files:**
- Modify: `go/internal/state/trusted_devices.go` (`TrustedDevice` struct + Save/Load/Lookup)
- Modify: `go/internal/state/store.go` (`trusted_devices` table + `addColumnIfMissing`, ~line 453-473)
- Test: `go/internal/state/trusted_devices_test.go`

This is the foundation: a replicated credential must be distinguishable from a locally-enrolled one for auditing + per-credential revoke (OQ-7). No behavioural change yet — every existing call site defaults to `local`.

- [ ] **Step 1: Write the failing test** — append to `go/internal/state/trusted_devices_test.go`:

```go
// A device saved without an explicit Origin defaults to "local"
// (locally-enrolled). A device saved with Origin "replicated" round-trips.
func TestTrustedDeviceOriginRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	// No Origin set → defaults to "local".
	local := TrustedDevice{
		CredentialID: []byte("cred-local"), PublicKey: []byte("k"),
		FriendlyName: "this phone",
	}
	if err := st.SaveTrustedDevice(local); err != nil {
		t.Fatalf("save local: %v", err)
	}
	// Explicit replicated.
	repl := TrustedDevice{
		CredentialID: []byte("cred-repl"), PublicKey: []byte("k2"),
		FriendlyName: "phone (replicated from Villa)", Origin: OriginReplicated,
	}
	if err := st.SaveTrustedDevice(repl); err != nil {
		t.Fatalf("save replicated: %v", err)
	}

	got, err := st.LookupTrustedDevice([]byte("cred-local"))
	if err != nil {
		t.Fatalf("lookup local: %v", err)
	}
	if got.Origin != OriginLocal {
		t.Fatalf("local device Origin = %q, want %q", got.Origin, OriginLocal)
	}
	got2, err := st.LookupTrustedDevice([]byte("cred-repl"))
	if err != nil {
		t.Fatalf("lookup replicated: %v", err)
	}
	if got2.Origin != OriginReplicated {
		t.Fatalf("replicated device Origin = %q, want %q", got2.Origin, OriginReplicated)
	}

	// LoadTrustedDevices also carries Origin.
	all, err := st.LoadTrustedDevices()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 devices, got %d", len(all))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd go && go test ./internal/state/ -run TestTrustedDeviceOriginRoundTrip -v`
Expected: FAIL — `TrustedDevice.Origin` / `OriginLocal` / `OriginReplicated` undefined (compile error).

- [ ] **Step 3: Add the `Origin` field + constants** in `go/internal/state/trusted_devices.go`. Add the constants above the `TrustedDevice` struct:

```go
// Credential origin — how a passkey landed on this Pi. A locally-enrolled
// credential was registered on this device's own LAN; a replicated one was
// copied here by a claim handshake (Phase 4 multi-home). Stored so the
// operator can audit and individually revoke replicated credentials.
const (
	OriginLocal      = "local"
	OriginReplicated = "replicated"
)
```

Add the field to the struct (after `WalletHandle`):

```go
	WalletHandle string // opaque wallet (owner) handle this credential belongs to
	Origin       string // "local" (enrolled here) or "replicated" (copied by a claim)
```

- [ ] **Step 4: Default + persist Origin in `SaveTrustedDevice`**. Inside `SaveTrustedDevice`, after the `FriendlyName` guard, add the default:

```go
	if d.Origin == "" {
		d.Origin = OriginLocal
	}
```

Update the INSERT column list + placeholders + args to include `origin`:

```go
	_, err := s.db.Exec(`
		INSERT INTO trusted_devices
			(credential_id, public_key, sign_count, aaguid, transports, friendly_name, created_at_ms, last_used_ms, wallet_handle, origin)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.CredentialID, d.PublicKey, int64(d.SignCount), d.AAGUID,
		strings.Join(d.Transports, ","), d.FriendlyName, d.CreatedAtMs, d.LastUsedMs, d.WalletHandle, d.Origin,
	)
```

- [ ] **Step 5: Read Origin in `LoadTrustedDevices` + `LookupTrustedDevice`**. In both functions add `origin` to the SELECT column list and scan it into `&d.Origin` (last scan target). Example for `LoadTrustedDevices`:

```go
	rows, err := s.db.Query(`
		SELECT credential_id, public_key, sign_count, aaguid, transports, friendly_name, created_at_ms, last_used_ms, wallet_handle, origin
		FROM trusted_devices
		ORDER BY created_at_ms DESC`)
```

and in the `rows.Scan(...)` append `&d.Origin` as the final arg. Mirror the same two changes (SELECT + Scan) in `LookupTrustedDevice`.

- [ ] **Step 6: Add the migration column** in `go/internal/state/store.go`. Add `origin` to the `CREATE TABLE IF NOT EXISTS trusted_devices` block (after `wallet_handle`, ~line 462):

```go
			wallet_handle TEXT    NOT NULL DEFAULT '',
			origin        TEXT    NOT NULL DEFAULT 'local'
```

Then, immediately after the existing `addColumnIfMissing("trusted_devices", "wallet_handle", ...)` block (~line 470-473), add the upgrade path for pre-existing DBs:

```go
	if err := s.addColumnIfMissing("trusted_devices", "origin",
		"origin TEXT NOT NULL DEFAULT 'local'"); err != nil {
		return fmt.Errorf("migrate trusted_devices.origin: %w", err)
	}
```

- [ ] **Step 7: Run tests**

Run: `cd go && go test ./internal/state/ -run 'TrustedDevice|TestUpdateSignCount' -v`
Expected: PASS — new origin round-trip + all pre-existing trusted-device tests (they save without Origin → default `local`).

- [ ] **Step 8: Run the full state package to catch fallout**

Run: `cd go && go test ./internal/state/`
Expected: `ok`.

- [ ] **Step 9: Commit**

```bash
git add go/internal/state/trusted_devices.go go/internal/state/store.go go/internal/state/trusted_devices_test.go
git commit -m "feat(state): origin flag on trusted_devices (local vs replicated credential)"
```

---

### Task 2: Host — announce payload (`site_label` + `wallet`) on relay registration

**Files:**
- Modify: `go/internal/api/api_owner_access.go` (add `siteLabel()` + stamp `Origin: OriginLocal` on enroll)
- Create: `go/internal/api/api_owner_announce.go` (the announce DTO + a host getter the registration goroutine reads)
- Create: `go/internal/api/api_owner_announce_test.go`
- Modify: `go/cmd/forty-two-watts/owner_relay_register.go` (send the new fields; gate on ≥1 passkey)

The Pi must tell the relay `{site_label, wallet W}` so the relay can group by wallet — but **only once a passkey is enrolled** (no wallet to announce before that). Note: `enroll/finish` already persists `WalletHandle`; here we also stamp `Origin: OriginLocal` so locally-enrolled creds are explicit (Task 1's default would do it, but be explicit at the call site for audit clarity).

- [ ] **Step 1: Write the failing test** — create `go/internal/api/api_owner_announce_test.go`:

```go
package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// The announce endpoint reports site_label + wallet + a passkey-enrolled flag
// so the relay-registration goroutine knows whether (and what) to announce.
func TestOwnerAnnounceReportsWalletAndLabel(t *testing.T) {
	d := minDeps(t) // minDeps wires a real state.Store with a site name
	srv := New(d)

	// Before any enrollment: enrolled=false, wallet present (minted lazily),
	// label = site name.
	req := httptest.NewRequest("GET", "/api/owner-access/announce", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("announce status=%d body=%q", rec.Code, rec.Body.String())
	}
	var got struct {
		SiteLabel string `json:"site_label"`
		Wallet    string `json:"wallet"`
		Enrolled  bool   `json:"enrolled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SiteLabel == "" {
		t.Fatalf("expected non-empty site_label")
	}
	if got.Wallet == "" {
		t.Fatalf("expected non-empty wallet handle")
	}
	if got.Enrolled {
		t.Fatalf("expected enrolled=false before any passkey")
	}
}
```

> **Note:** if `minDeps` does not already set a site name on `Deps.Cfg`, set `d.Cfg.Site.Name = "Villa"` in the test (check the existing `minDeps` in `api_owner_access_test.go` — Phase 1 used it, so it exists). `siteLabel()` falls back to a generic label if no name is set, so the test still passes, but a named site makes the assertion meaningful.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd go && go test ./internal/api/ -run TestOwnerAnnounceReportsWalletAndLabel -v`
Expected: FAIL — route `/api/owner-access/announce` not registered → 404 (or the gate returns 401; if so, the route must be added to the open-path set — see Step 5).

- [ ] **Step 3: Add `siteLabel()`** in `go/internal/api/api_owner_access.go`, near `ownerDisplayName`:

```go
// siteLabel is the human-readable home name the relay shows in the
// multi-home selector. Falls back to a generic label so an unnamed site
// still appears (never empty — the selector renders it verbatim).
func siteLabel(deps *Deps) string {
	if deps != nil && deps.Cfg != nil && deps.Cfg.Site.Name != "" {
		return deps.Cfg.Site.Name
	}
	return "forty-two-watts home"
}
```

- [ ] **Step 4: Stamp `Origin: OriginLocal` on enroll** in `handleOwnerEnrollFinish`. In the `state.TrustedDevice{...}` literal (~line 451), add:

```go
		WalletHandle: string(walletHandle),
		Origin:       state.OriginLocal,
```

- [ ] **Step 5: Create the announce handler** — `go/internal/api/api_owner_announce.go`:

```go
// api_owner_announce.go
//
// Phase 4 multi-home: the Pi tells the relay {site_label, wallet W} so the
// relay can group live Pis by wallet (routing only — never a persistent
// directory). This endpoint is the host-side source of that announce; the
// relay-registration goroutine (cmd/forty-two-watts/owner_relay_register.go)
// reads it locally and forwards the values to /me/register.
//
// It is LAN-only data — read by the registration goroutine over loopback —
// but it is also harmless to expose: site_label is a chosen name and the
// wallet handle is the public userHandle already sent in every assertion.
package api

import (
	"net/http"
)

// handleOwnerAnnounce returns the live-announce payload: the home's label,
// its wallet handle W, and whether at least one passkey is enrolled (the
// relay must NOT list a home that can't verify anything, so an un-enrolled
// Pi reports enrolled=false and the registration goroutine omits the wallet).
func (s *Server) handleOwnerAnnounce(w http.ResponseWriter, r *http.Request) {
	wallet, err := s.ownerWalletHandle()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	enrolled := false
	if devices, err := s.deps.State.LoadTrustedDevices(); err == nil {
		enrolled = len(devices) > 0
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"site_label": siteLabel(s.deps),
		"wallet":     string(wallet),
		"enrolled":   enrolled,
	})
}
```

> Confirm `writeJSON` exists with signature `writeJSON(w http.ResponseWriter, status int, v any)` (api.go:144 per the package CLAUDE.md). If it takes a different arg order, match it.

- [ ] **Step 6: Register the route + keep it open** in `go/internal/api/api.go`. Add to the owner-access route block (~line 349):

```go
	s.handle("GET  /api/owner-access/announce", s.handleOwnerAnnounce)
```

And add it to the gate's open-path set in `go/internal/api/api_owner_gate.go` (`isOwnerAccessOpenPath`) so the loopback registration goroutine can read it without a session. **Insert** the new case into the existing `switch` (which already has `enroll-pin`, `login/*`, `enroll/*`, `whoami`) — add one line, do not rewrite the block:

```go
		"/api/owner-access/announce",
```

so the switch reads (existing lines + the new one):

```go
	switch p {
	case "/api/owner-access/enroll-pin",
		"/api/owner-access/login/start",
		"/api/owner-access/login/finish",
		"/api/owner-access/enroll/start",
		"/api/owner-access/enroll/finish",
		"/api/owner-access/announce",
		"/api/owner-access/whoami":
		return true
	}
```

- [ ] **Step 7: Run tests**

Run: `cd go && go test ./internal/api/ -run 'TestOwnerAnnounce|TestGate|TestOwnerAccess' -v`
Expected: PASS — announce returns the payload; gate still blocks the dashboard; existing owner-access tests unchanged.

- [ ] **Step 8: Commit**

```bash
git add go/internal/api/api_owner_access.go go/internal/api/api_owner_announce.go go/internal/api/api_owner_announce_test.go go/internal/api/api.go go/internal/api/api_owner_gate.go
git commit -m "feat(owner-access): host announce endpoint (site_label + wallet + enrolled flag)"
```

---

### Task 3: Host — replicate export/import handlers + LAN-presence claim code

**Files:**
- Modify: `go/internal/api/api_owner_announce.go` (add the replicate + claim handlers — they're part of the same multi-home surface)
- Modify: `go/internal/api/api_owner_announce_test.go` (export/import + claim-code tests)
- Modify: `go/internal/api/api.go` (register the routes; gate decisions)

This is the trust-sensitive path (OQ-3, OQ-4). Three host-side endpoints:
1. `POST /api/owner-access/claim/begin` — **LAN-only** (new Pi), mints + shows a 4-digit presence code, returns it for the LAN UI.
2. `POST /api/owner-access/replicate/export` — **tunnel-only** (old Pi), returns *this Pi's wallet's* credential records iff the requested wallet matches `ownerWalletHandle()`.
3. `POST /api/owner-access/replicate/import` — **tunnel-only** (new Pi), validates the 4-digit code, stores the records with `Origin: OriginReplicated`.

- [ ] **Step 1: Write the failing tests** — append to `api_owner_announce_test.go`:

```go
// replicate/export returns this Pi's wallet credentials only when the
// requested wallet matches, and only over the tunnel (the relay asks on
// behalf of a verified wallet). Pubkeys are not secret; the import side is
// where LAN presence is enforced.
func TestReplicateExportMatchesWallet(t *testing.T) {
	d := minDeps(t)
	d.TunnelMarker = "marker"
	srv := New(d)

	// Enroll one local passkey so there is something to export.
	enrollOneTestPasskey(t, srv) // helper from existing owner-access tests

	wallet := mustWallet(t, srv) // reads /api/owner-access/announce → wallet

	body := []byte(`{"wallet":"` + wallet + `"}`)
	req := httptest.NewRequest("POST", "/api/owner-access/replicate/export", bytesReader(body))
	req.Header.Set("X-FTW-Tunnel", "marker") // arrived via relay
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("export status=%d body=%q", rec.Code, rec.Body.String())
	}
	// Wrong wallet → 403, no records leaked.
	bad := httptest.NewRequest("POST", "/api/owner-access/replicate/export", bytesReader([]byte(`{"wallet":"not-mine"}`)))
	bad.Header.Set("X-FTW-Tunnel", "marker")
	badRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(badRec, bad)
	if badRec.Code != 403 {
		t.Fatalf("export of wrong wallet must be 403, got %d", badRec.Code)
	}
}

// claim/begin is LAN-only: a tunnelled request must be refused (the human
// must be physically at the new Pi to read the code).
func TestClaimBeginIsLANOnly(t *testing.T) {
	d := minDeps(t)
	d.TunnelMarker = "marker"
	srv := New(d)
	req := httptest.NewRequest("POST", "/api/owner-access/claim/begin", nil)
	req.Header.Set("X-FTW-Tunnel", "marker") // remote → must be refused
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("remote claim/begin must be 403, got %d body=%q", rec.Code, rec.Body.String())
	}
}

// import rejects a wrong presence code, accepts the right one, and stores the
// credential with Origin=replicated.
func TestReplicateImportGatesOnPresenceCode(t *testing.T) {
	d := minDeps(t)
	d.TunnelMarker = "marker"
	srv := New(d)

	// Begin a claim on the LAN → get the code.
	beginReq := httptest.NewRequest("POST", "/api/owner-access/claim/begin", nil) // unmarked = LAN
	beginRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(beginReq, beginRec) // NOTE: ServeHTTP(rec, req) — fix arg order in real test
	var begun struct{ Code string `json:"code"` }
	_ = json.Unmarshal(beginRec.Body.Bytes(), &begun)

	records := `[{"credential_id_b64":"YWJj","public_key_b64":"a2V5","friendly_name":"phone"}]`

	// Wrong code → 403.
	wrong := []byte(`{"code":"0000","wallet":"w","records":` + records + `}`)
	wreq := httptest.NewRequest("POST", "/api/owner-access/replicate/import", bytesReader(wrong))
	wreq.Header.Set("X-FTW-Tunnel", "marker")
	wrec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(wrec, wreq)
	if wrec.Code != 403 {
		t.Fatalf("import with wrong code must be 403, got %d", wrec.Code)
	}

	// Right code → 204 + stored replicated.
	good := []byte(`{"code":"` + begun.Code + `","wallet":"w","records":` + records + `}`)
	greq := httptest.NewRequest("POST", "/api/owner-access/replicate/import", bytesReader(good))
	greq.Header.Set("X-FTW-Tunnel", "marker")
	grec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(grec, greq)
	if grec.Code != 204 {
		t.Fatalf("import with right code must be 204, got %d body=%q", grec.Code, grec.Body.String())
	}
	devs, _ := d.State.LoadTrustedDevices()
	found := false
	for _, dv := range devs {
		if dv.Origin == "replicated" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a replicated credential after import")
	}
}
```

> **Test-helper note.** `enrollOneTestPasskey`, `mustWallet`, `bytesReader` are small helpers; if the existing owner-access test file doesn't already have equivalents, add them in this test file. `bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }`. `mustWallet` GETs `/api/owner-access/announce` and pulls `wallet`. Fix the deliberate `ServeHTTP(rec, req)` arg order shown wrong above — Go is `ServeHTTP(w, r)`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd go && go test ./internal/api/ -run 'TestReplicate|TestClaimBegin' -v`
Expected: FAIL — routes unregistered / handlers undefined.

- [ ] **Step 3: Add a claim-code store** to `ownerAccessState` in `go/internal/api/api_owner_access.go`. Add a field + helpers (mirrors the relay's token-approval shape, OQ-4):

```go
	claimCodes map[string]claimCode // code → presence-claim metadata
```

and init it in `ownerAccess()` alongside the other maps. Define near `authSession`:

```go
// claimCode is a one-shot LAN-presence code shown on the new Pi's local UI
// during a multi-home claim. The human reads it off the screen (proving
// physical presence) and types it into home.* ; it travels back to this Pi
// via the relay inside replicate/import. Short-lived, attempt-capped.
type claimCode struct {
	code      string
	expiresAt time.Time
	attempts  int
}
```

- [ ] **Step 4: Implement the three handlers** in `go/internal/api/api_owner_announce.go`:

```go
// claimCodeTTL bounds how long a LAN-presence code is valid. The human reads
// it off the new Pi's screen and types it into home.* within this window.
const claimCodeTTL = 5 * time.Minute

// maxClaimAttempts caps wrong presence-code guesses on a single code.
const maxClaimAttempts = 5

// handleOwnerClaimBegin mints + shows a 4-digit LAN-presence code on the NEW
// Pi. LAN-only: a tunnelled (remote) request is refused — the human must be
// physically at the Pi to read the code (the whole point of LAN presence).
func (s *Server) handleOwnerClaimBegin(w http.ResponseWriter, r *http.Request) {
	if s.isTunneled(r) {
		http.Error(w, "claim must be started on the new home's local network", http.StatusForbidden)
		return
	}
	code, err := fourDigitCode()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	oa := s.ownerAccess()
	oa.mu.Lock()
	oa.gcClaimCodes()
	oa.claimCodes[code] = claimCode{code: code, expiresAt: time.Now().Add(claimCodeTTL)}
	oa.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"code": code, "expires_in_s": int(claimCodeTTL.Seconds())})
}

// replicateRecord is one credential the OLD Pi exports / the NEW Pi imports.
type replicateRecord struct {
	CredentialIDB64 string   `json:"credential_id_b64"`
	PublicKeyB64    string   `json:"public_key_b64"`
	AAGUIDB64       string   `json:"aaguid_b64,omitempty"`
	SignCount       uint32   `json:"sign_count,omitempty"`
	Transports      []string `json:"transports,omitempty"`
	FriendlyName    string   `json:"friendly_name"`
}

// handleOwnerReplicateExport returns THIS Pi's wallet credential records.
// Tunnel-only (the relay asks, on behalf of a wallet it just verified) AND
// the requested wallet must equal this Pi's own wallet handle. Pubkeys verify
// but never sign, so exporting them is not a secret-leak; the import side is
// where LAN presence gates the write.
func (s *Server) handleOwnerReplicateExport(w http.ResponseWriter, r *http.Request) {
	if !s.isTunneled(r) {
		http.Error(w, "replicate/export is relay-only", http.StatusForbidden)
		return
	}
	var body struct {
		Wallet string `json:"wallet"`
	}
	if err := readJSON(r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	wallet, err := s.ownerWalletHandle()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.Wallet), wallet) != 1 { // OQ-3
		http.Error(w, "wallet mismatch", http.StatusForbidden)
		return
	}
	devices, err := s.deps.State.LoadTrustedDevices()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]replicateRecord, 0, len(devices))
	for _, d := range devices {
		out = append(out, replicateRecord{
			CredentialIDB64: base64.RawURLEncoding.EncodeToString(d.CredentialID),
			PublicKeyB64:    base64.RawURLEncoding.EncodeToString(d.PublicKey),
			AAGUIDB64:       base64.RawURLEncoding.EncodeToString(d.AAGUID),
			SignCount:       d.SignCount,
			Transports:      d.Transports,
			FriendlyName:    d.FriendlyName,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"wallet": string(wallet), "records": out})
}

// handleOwnerReplicateImport stores credential records on the NEW Pi, gated
// on the LAN-presence code (proving the human is physically here) AND arrival
// via the tunnel (the relay brokered it). Records land with Origin=replicated.
func (s *Server) handleOwnerReplicateImport(w http.ResponseWriter, r *http.Request) {
	if !s.isTunneled(r) {
		http.Error(w, "replicate/import is relay-only", http.StatusForbidden)
		return
	}
	var body struct {
		Code    string            `json:"code"`
		Wallet  string            `json:"wallet"`
		Records []replicateRecord `json:"records"`
	}
	if err := readJSON(r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !s.consumeClaimCode(body.Code) { // OQ-4
		http.Error(w, "wrong or expired presence code", http.StatusForbidden)
		return
	}
	for _, rec := range body.Records {
		credID, err1 := base64.RawURLEncoding.DecodeString(rec.CredentialIDB64)
		pub, err2 := base64.RawURLEncoding.DecodeString(rec.PublicKeyB64)
		if err1 != nil || err2 != nil || len(credID) == 0 || len(pub) == 0 {
			continue // skip malformed; never abort the whole import on one bad row
		}
		aaguid, _ := base64.RawURLEncoding.DecodeString(rec.AAGUIDB64)
		dev := state.TrustedDevice{
			CredentialID: credID,
			PublicKey:    pub,
			SignCount:    rec.SignCount,
			AAGUID:       aaguid,
			Transports:   rec.Transports,
			FriendlyName: rec.FriendlyName,
			CreatedAtMs:  time.Now().UnixMilli(),
			WalletHandle: body.Wallet,
			Origin:       state.OriginReplicated,
		}
		// Idempotent: a re-claim of an already-present credential should not
		// error. Ignore duplicate-PK insert failures.
		_ = s.deps.State.SaveTrustedDevice(dev)
	}
	w.WriteHeader(http.StatusNoContent)
}
```

Add the small helpers (`fourDigitCode`, `gcClaimCodes`, `consumeClaimCode`) — `fourDigitCode` uses `crypto/rand` to pick `0000`-`9999`; `consumeClaimCode` does an attempt-capped, expiry-checked, delete-on-success lookup under `oa.mu`. Add the needed imports (`crypto/subtle` already present; add `encoding/base64`, `time`, `state` if not already imported in `api_owner_announce.go`).

- [ ] **Step 5: Register the routes** in `go/internal/api/api.go` (owner-access block):

```go
	s.handle("POST /api/owner-access/claim/begin", s.handleOwnerClaimBegin)
	s.handle("POST /api/owner-access/replicate/export", s.handleOwnerReplicateExport)
	s.handle("POST /api/owner-access/replicate/import", s.handleOwnerReplicateImport)
```

**Gate decision (important):** these three handlers do their *own* origin gating (`isTunneled` checks inside the handler), so they must be reachable by the gate. **Insert** these three cases into the existing `isOwnerAccessOpenPath` `switch` (alongside the `announce` case added in Task 2) so the gate forwards them to the handler, which then enforces tunnel-only / LAN-only itself:

```go
		"/api/owner-access/claim/begin",
		"/api/owner-access/replicate/export",
		"/api/owner-access/replicate/import",
```

> Rationale: the gate's job is "is this an authenticated owner?" — but these endpoints are authenticated by *presence* (LAN code) / *relay brokerage* (tunnel marker), not the `ftw_owner` cookie. Letting them through the gate and self-gating inside the handler matches how `enroll/start` already self-gates via `enrollAllowed`.

- [ ] **Step 6: Run tests**

Run: `cd go && go test ./internal/api/ -run 'TestReplicate|TestClaimBegin|TestGate|TestOwnerAccess|TestOwnerAnnounce' -v`
Expected: PASS — export matches/rejects wallet, claim/begin LAN-only, import gates on code + stores replicated; gate + existing tests unchanged.

- [ ] **Step 7: Run the full api package**

Run: `cd go && go test ./internal/api/`
Expected: `ok`.

- [ ] **Step 8: Commit**

```bash
git add go/internal/api/api_owner_announce.go go/internal/api/api_owner_access.go go/internal/api/api_owner_announce_test.go go/internal/api/api.go go/internal/api/api_owner_gate.go
git commit -m "feat(owner-access): replicate export/import + LAN-presence claim code for multi-home"
```

---

### Task 4: Relay — live announce table grouped by wallet

**Files:**
- Modify: `go/cmd/ftw-relay/owners.go` (extend the registry with announce metadata + group-by-wallet)
- Create: `go/cmd/ftw-relay/owners_test.go`
- Modify: `go/cmd/ftw-relay/handlers.go` (`meRegisterRequest` accepts the new fields; pass through)

The relay must store `{site_label, wallet, host_id}` as **live metadata** and answer "which live hosts belong to wallet `W`?" — with stale entries (crashed Pi, OQ-1) ageing out.

- [ ] **Step 1: Write the failing test** — create `go/cmd/ftw-relay/owners_test.go`:

```go
package main

import (
	"testing"
	"time"
)

// The registry groups live announces by wallet and drops stale ones.
func TestOwnerRegistryGroupsByWallet(t *testing.T) {
	r := NewOwnerRegistry()
	r.Announce(Announce{SiteID: "site-villa", HostID: "h-villa", SiteLabel: "Villa", Wallet: "W"})
	r.Announce(Announce{SiteID: "site-stuga", HostID: "h-stuga", SiteLabel: "Stuga", Wallet: "W"})
	r.Announce(Announce{SiteID: "site-other", HostID: "h-other", SiteLabel: "Other", Wallet: "X"})

	homes := r.HomesForWallet("W")
	if len(homes) != 2 {
		t.Fatalf("wallet W should have 2 live homes, got %d: %+v", len(homes), homes)
	}
	if len(r.HomesForWallet("X")) != 1 {
		t.Fatalf("wallet X should have 1 live home")
	}
	if len(r.HomesForWallet("nobody")) != 0 {
		t.Fatalf("unknown wallet should have 0 live homes")
	}
}

// A stale announce (older than the liveness TTL) is excluded.
func TestOwnerRegistryExpiresStaleAnnounces(t *testing.T) {
	r := NewOwnerRegistry()
	r.announceTTL = 50 * time.Millisecond
	r.Announce(Announce{SiteID: "s", HostID: "h", SiteLabel: "L", Wallet: "W"})
	if len(r.HomesForWallet("W")) != 1 {
		t.Fatalf("fresh announce should be live")
	}
	time.Sleep(70 * time.Millisecond)
	if got := r.HomesForWallet("W"); len(got) != 0 {
		t.Fatalf("stale announce should age out, got %+v", got)
	}
}

// A Pi that re-registers without a wallet (no passkey yet) is NOT listed for
// any wallet but its site_id → host_id mapping still works for /me/<site>.
func TestOwnerRegistryUnenrolledNotListed(t *testing.T) {
	r := NewOwnerRegistry()
	r.Announce(Announce{SiteID: "s", HostID: "h", SiteLabel: "L", Wallet: ""})
	if len(r.HomesForWallet("")) != 0 {
		t.Fatalf("empty-wallet announce must never group under a wallet")
	}
	if hostID, err := r.Lookup("s"); err != nil || hostID != "h" {
		t.Fatalf("site→host lookup must still work, got %q %v", hostID, err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd go && go test ./cmd/ftw-relay/ -run TestOwnerRegistry -v`
Expected: FAIL — `Announce`, `r.Announce`, `r.HomesForWallet`, `r.announceTTL` undefined.

- [ ] **Step 3: Extend the registry** in `go/cmd/ftw-relay/owners.go`. Replace the struct + constructor and add the announce types/methods. Keep the existing `Register`/`Lookup`/`Unregister`/`List` working (so the Phase 3 `/me/<site>` path is untouched):

```go
// Announce is one Pi's live multi-home announce: its stable site_id, the
// host_id it polls under, a human label, and the wallet handle W it belongs
// to (empty until a passkey is enrolled). Held as live metadata only.
type Announce struct {
	SiteID    string
	HostID    string
	SiteLabel string
	Wallet    string
}

// Home is a wallet's currently-online home, returned to the selector.
type Home struct {
	SiteID    string `json:"site_id"`
	HostID    string `json:"-"` // routing only — never serialized to the browser
	SiteLabel string `json:"site_label"`
}

type announceEntry struct {
	Announce
	seen time.Time
}

// OwnerRegistry maps site_id → host_id (Phase 3 /me/* routing) and ALSO holds
// live wallet announces (Phase 4 multi-home). Both are in-memory + ephemeral:
// a relay restart drops everything and hosts re-register on their next loop.
type OwnerRegistry struct {
	mu          sync.Mutex
	bySite      map[string]string         // site_id → host_id (Phase 3)
	announces   map[string]announceEntry  // site_id → live announce (Phase 4)
	announceTTL time.Duration             // liveness window; 0 → default
}
```

Update `NewOwnerRegistry`:

```go
func NewOwnerRegistry() *OwnerRegistry {
	return &OwnerRegistry{
		bySite:      make(map[string]string),
		announces:   make(map[string]announceEntry),
		announceTTL: 180 * time.Second, // OQ-1: 3× the 60s register cadence
	}
}
```

Add the announce methods (and keep `Register` as a thin wrapper so Phase 3 callers/tests still pass):

```go
// Announce records (or refreshes) a Pi's live announce. Also keeps the
// site→host mapping current so /me/<site> routing and multi-home share one
// source of truth.
func (r *OwnerRegistry) Announce(a Announce) {
	r.mu.Lock()
	r.bySite[a.SiteID] = a.HostID
	r.announces[a.SiteID] = announceEntry{Announce: a, seen: time.Now()}
	r.mu.Unlock()
}

func (r *OwnerRegistry) ttl() time.Duration {
	if r.announceTTL > 0 {
		return r.announceTTL
	}
	return 180 * time.Second
}

// HomesForWallet returns the wallet's currently-live homes (fresh announces
// with a non-empty matching wallet). Routing-only metadata — the caller
// (verify-then-reveal) must not expose it until one Pi has verified W.
func (r *OwnerRegistry) HomesForWallet(wallet string) []Home {
	if wallet == "" {
		return nil
	}
	cutoff := time.Now().Add(-r.ttl())
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Home
	for _, e := range r.announces {
		if e.Wallet == wallet && e.Wallet != "" && e.seen.After(cutoff) {
			out = append(out, Home{SiteID: e.SiteID, HostID: e.HostID, SiteLabel: e.SiteLabel})
		}
	}
	return out
}
```

Keep `Register` as a compatibility shim (existing Phase 3 tests call `Register(siteID, hostID)`):

```go
// Register is the Phase 3 site→host mapping. Retained as a thin wrapper over
// Announce with an empty wallet (an un-enrolled or pre-Phase-4 host).
func (r *OwnerRegistry) Register(siteID, hostID string) {
	r.Announce(Announce{SiteID: siteID, HostID: hostID})
}
```

`Lookup`, `Unregister`, `List` stay as-is (they only touch `bySite`).

- [ ] **Step 4: Extend `/me/register`** in `go/cmd/ftw-relay/handlers.go`. Add the new fields to `meRegisterRequest` (~line 410) and call `Announce`:

```go
type meRegisterRequest struct {
	SiteID    string `json:"site_id"`
	HostID    string `json:"host_id"`
	SiteLabel string `json:"site_label,omitempty"`
	Wallet    string `json:"wallet,omitempty"`
}
```

In `meRegister`, replace `r.Owners.Register(reg.SiteID, reg.HostID)` with:

```go
	r.Owners.Announce(Announce{
		SiteID:    reg.SiteID,
		HostID:    reg.HostID,
		SiteLabel: reg.SiteLabel,
		Wallet:    reg.Wallet,
	})
```

(The `SiteID == "" || HostID == ""` guard stays; `site_label` + `wallet` are optional.)

- [ ] **Step 5: Run tests**

Run: `cd go && go test ./cmd/ftw-relay/ -run 'TestOwnerRegistry|TestMe' -v`
Expected: PASS — group-by-wallet + stale-expiry + un-enrolled, and the existing `/me/*` register/forward tests (they call `Register` / POST `meRegisterRequest` without the new fields, which still works).

- [ ] **Step 6: Run the full relay package**

Run: `cd go && go test ./cmd/ftw-relay/`
Expected: `ok`.

- [ ] **Step 7: Commit**

```bash
git add go/cmd/ftw-relay/owners.go go/cmd/ftw-relay/owners_test.go go/cmd/ftw-relay/handlers.go
git commit -m "feat(relay): live wallet announce table grouped by wallet with liveness TTL"
```

---

### Task 5: Relay — `home.*` vhost: login surface + verify-then-reveal selector assembly

**Files:**
- Create: `go/cmd/ftw-relay/home.go` (the `home.*` handlers: login page, login-start/finish proxy, verify-then-reveal, selector JSON)
- Create: `go/cmd/ftw-relay/home_test.go`
- Modify: `go/cmd/ftw-relay/handlers.go` (register the `home.*` routes on the mux)

This is the heart of Phase 4. The relay:
1. serves the login page at `GET /` on the `home.*` vhost (Conditional-UI passkey),
2. proxies `POST /api/owner-access/login/start` to *one* live Pi of the asserted wallet so the browser gets a challenge (any live Pi can issue a discoverable-login challenge),
3. on `POST /api/owner-access/login/finish`, routes the assertion to one live Pi to **verify** (OQ-2); on 200 it reveals the wallet's live homes as the selector.

> **Design constraint kept honest:** the relay never sees the wallet *before* the assertion. `login/start` is discoverable (empty allowCredentials), so it can go to *any* live Pi — but the relay doesn't know the wallet yet. We solve this by having the browser send the wallet `userHandle` it intends to use is NOT known pre-assertion either. **Resolution (matches the design):** `login/start` is wallet-agnostic — route it to *any* currently-live Pi (they all issue an equivalent discoverable challenge bound to the `home.*` RP-ID). The wallet only becomes known at `login/finish` (the assertion carries `userHandle = W`); the relay parses `W` out of the finish body, routes the *finish* to a live Pi of `W`, and on 200 reveals `W`'s homes. See OQ-2.

- [ ] **Step 1: Write the failing test** — create `go/cmd/ftw-relay/home_test.go`:

```go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

// homeTestRelay stands up a relay + two fake live Pis for wallet W, both
// answering login/finish with 200 (verified). Asserts verify-then-reveal:
// the selector lists both homes only after a successful verify.
func TestHomeVerifyThenRevealListsLiveHomes(t *testing.T) {
	relay := &Relay{
		Queue:       tunnel.NewQueue(),
		Tokens:      NewTokenRegistry(),
		Owners:      NewOwnerRegistry(),
		PollTimeout: 500 * time.Millisecond,
		HomeHost:    "home.fortytwowatts.com",
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	// Two fake Pis: each verifies login/finish (200) and echoes wallet.
	piHandler := func(label string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/login/finish") {
				w.WriteHeader(200)
				_, _ = w.Write([]byte(`{"credential_id_b64":"x"}`))
				return
			}
			w.WriteHeader(200)
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tunnel.NewHost(srv.URL, "h-villa", piHandler("Villa")).Run(ctx)
	go tunnel.NewHost(srv.URL, "h-stuga", piHandler("Stuga")).Run(ctx)

	// Announce both under wallet W.
	announce(t, srv.URL, meRegisterRequest{SiteID: "site-villa", HostID: "h-villa", SiteLabel: "Villa", Wallet: "W"})
	announce(t, srv.URL, meRegisterRequest{SiteID: "site-stuga", HostID: "h-stuga", SiteLabel: "Stuga", Wallet: "W"})

	// Verify-then-reveal: POST the (fake) assertion carrying userHandle W to
	// the home.* finish endpoint.
	body := []byte(`{"response":{"userHandle":"Vw"}}`) // base64url("W") = "Vw"
	req, _ := http.NewRequest("POST", srv.URL+"/api/home/login/finish", bytes.NewReader(body))
	req.Host = "home.fortytwowatts.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("finish status=%d body=%q", resp.StatusCode, b)
	}
	var out struct {
		Homes []struct {
			SiteID    string `json:"site_id"`
			SiteLabel string `json:"site_label"`
		} `json:"homes"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if len(out.Homes) != 2 {
		t.Fatalf("expected 2 revealed homes, got %d: %+v", len(out.Homes), out.Homes)
	}
}

// An unverified wallet (no live Pi verifies) reveals NOTHING (privacy).
func TestHomeUnverifiedWalletRevealsNothing(t *testing.T) {
	relay := &Relay{
		Queue: tunnel.NewQueue(), Tokens: NewTokenRegistry(), Owners: NewOwnerRegistry(),
		PollTimeout: 200 * time.Millisecond, HomeHost: "home.fortytwowatts.com",
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()
	// No live Pi announced for W → finish cannot verify → 401/404, no homes.
	body := []byte(`{"response":{"userHandle":"Vw"}}`)
	req, _ := http.NewRequest("POST", srv.URL+"/api/home/login/finish", bytes.NewReader(body))
	req.Host = "home.fortytwowatts.com"
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatalf("unverified wallet must not get 200 + homes")
	}
}

// helpers
func announce(t *testing.T, base string, reg meRegisterRequest) {
	t.Helper()
	b, _ := json.Marshal(reg)
	resp, err := http.Post(base+"/me/register", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}
```

> **userHandle parsing note.** The browser assertion JSON nests `userHandle` under `response`. The relay only needs to *extract W* to route the finish — it does NOT verify the signature (the Pi does). Confirm the exact JSON shape the `go-webauthn` browser helper posts (it base64url-encodes `userHandle`); the relay base64url-decodes it to get `W`. If the shape differs, adjust `parseUserHandle` in Step 3. The test's `"Vw"` is `base64url("W")`.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd go && go test ./cmd/ftw-relay/ -run TestHome -v`
Expected: FAIL — `Relay.HomeHost` field + `/api/home/login/finish` route undefined.

- [ ] **Step 3: Create `home.go`** — `go/cmd/ftw-relay/home.go`:

```go
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"

	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

// homeLoginStart proxies the discoverable-login challenge request to ANY one
// live Pi. login/start is wallet-agnostic (empty allowCredentials), so any
// live Pi bound to the home.* RP-ID issues an equivalent challenge. If no Pi
// is online at all, there is nothing to log into → 503.
func (r *Relay) homeLoginStart(w http.ResponseWriter, req *http.Request) {
	hostID, ok := r.Owners.anyLiveHost()
	if !ok {
		http.Error(w, "no homes online", http.StatusServiceUnavailable)
		return
	}
	r.proxyToHost(w, req, hostID, "/api/owner-access/login/start")
}

// homeLoginFinish is verify-then-reveal. It extracts the asserted wallet W
// from the assertion, routes the finish to ONE live Pi of W to VERIFY, and on
// a 200 verify reveals W's live homes. An unverified W reveals nothing.
func (r *Relay) homeLoginFinish(w http.ResponseWriter, req *http.Request) {
	body, _ := readBody(req.Body)
	wallet := parseUserHandle(body)
	if wallet == "" {
		http.Error(w, "missing userHandle", http.StatusBadRequest)
		return
	}
	homes := r.Owners.HomesForWallet(wallet) // routing only — not yet revealed
	if len(homes) == 0 {
		http.Error(w, "unknown wallet", http.StatusNotFound)
		return
	}
	// Route the assertion to one live Pi of W to verify (OQ-2). 200 == verified.
	verifyResp, err := r.Queue.Enqueue(req.Context(), homes[0].HostID, tunnel.TunneledRequest{
		Method: http.MethodPost,
		Path:   "/api/owner-access/login/finish?ceremony_token=" + req.URL.Query().Get("ceremony_token"),
		Header: req.Header,
		Body:   body,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if verifyResp.Status != http.StatusOK {
		// Verification failed → reveal nothing (privacy).
		w.WriteHeader(verifyResp.Status)
		_, _ = w.Write(verifyResp.Body)
		return
	}
	// Verified. NOW reveal the live homes.
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"homes": homes})
}

// parseUserHandle extracts the base64url userHandle (wallet W) from a
// WebAuthn assertion body, without verifying the signature (the Pi does that).
func parseUserHandle(body []byte) string {
	var a struct {
		Response struct {
			UserHandle string `json:"userHandle"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &a); err != nil {
		return ""
	}
	if a.Response.UserHandle == "" {
		return ""
	}
	dec, err := base64.RawURLEncoding.DecodeString(a.Response.UserHandle)
	if err != nil {
		// Some browser libs use std (padded) base64url; try that too.
		dec, err = base64.StdEncoding.DecodeString(a.Response.UserHandle)
		if err != nil {
			return ""
		}
	}
	return string(dec)
}

// proxyToHost enqueues a request to a specific host and writes the response.
func (r *Relay) proxyToHost(w http.ResponseWriter, req *http.Request, hostID, innerPath string) {
	body, _ := readBody(req.Body)
	if q := req.URL.RawQuery; q != "" {
		innerPath = innerPath + "?" + q
	}
	resp, err := r.Queue.Enqueue(req.Context(), hostID, tunnel.TunneledRequest{
		Method: req.Method, Path: innerPath, Header: req.Header, Body: body,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	for k, vv := range resp.Header {
		w.Header()[k] = vv
	}
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

// homeOpen routes "open this home" to the chosen Pi's login/finish so that Pi
// mints ITS OWN ftw_owner session (P4-1: one tap per home). The Set-Cookie
// from the Pi flows back to the browser unchanged.
func (r *Relay) homeOpen(w http.ResponseWriter, req *http.Request) {
	siteID := req.PathValue("site_id")
	hostID, err := r.Owners.Lookup(siteID)
	if err != nil {
		http.Error(w, "home offline", http.StatusServiceUnavailable)
		return
	}
	r.proxyToHost(w, req, hostID, "/api/owner-access/login/finish")
}

var _ = context.Background // keep import if unused after edits
```

Add `anyLiveHost` to `go/cmd/ftw-relay/owners.go`:

```go
// anyLiveHost returns one currently-live host_id (any wallet), for the
// wallet-agnostic discoverable login/start challenge. False if none online.
func (r *OwnerRegistry) anyLiveHost() (string, bool) {
	cutoff := time.Now().Add(-r.ttl())
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.announces {
		if e.seen.After(cutoff) {
			return e.HostID, true
		}
	}
	return "", false
}
```

- [ ] **Step 4: Add `HomeHost` to the `Relay` struct + register `home.*` routes** in `go/cmd/ftw-relay/handlers.go`. Add the field:

```go
type Relay struct {
	Queue       *tunnel.Queue
	Tokens      *TokenRegistry
	Owners      *OwnerRegistry
	PollTimeout time.Duration // 0 → 25s default
	// HomeHost is the vhost serving the multi-home login surface
	// (home.fortytwowatts.com). Requests with this Host are routed to the
	// home.* handlers; everything else keeps the Phase 1-3 /h /me behaviour.
	HomeHost string
}
```

Register the routes in `Handler()` (after the `/me/*` block):

```go
	// Phase 4 multi-home — home.fortytwowatts.com surface. These paths are
	// only meaningful on the HomeHost vhost; main.go's host-router (or a
	// reverse proxy) directs that vhost here. Registered unconditionally so
	// tests can exercise them via Host header.
	mux.HandleFunc("POST /api/home/login/start", r.homeLoginStart)
	mux.HandleFunc("POST /api/home/login/finish", r.homeLoginFinish)
	mux.HandleFunc("POST /api/home/open/{site_id}", r.homeOpen)
	mux.HandleFunc("GET /home", r.homeLanding)
```

> Add a minimal `homeLanding` (serves the Conditional-UI login HTML — see Task 9 for the page; for now a placeholder `homeLandingHTML` const is fine, wired to the real selector page in Task 9). Keep it small here; Task 9 fills the UI.

- [ ] **Step 5: Run tests**

Run: `cd go && go test ./cmd/ftw-relay/ -run 'TestHome|TestOwnerRegistry|TestMe' -v`
Expected: PASS — verify-then-reveal lists both homes on 200; unverified wallet reveals nothing; existing tests unchanged.

- [ ] **Step 6: Run the full relay package + vet**

Run: `cd go && go test ./cmd/ftw-relay/ && go vet ./cmd/ftw-relay/`
Expected: `ok`, no vet output.

- [ ] **Step 7: Commit**

```bash
git add go/cmd/ftw-relay/home.go go/cmd/ftw-relay/home_test.go go/cmd/ftw-relay/handlers.go go/cmd/ftw-relay/owners.go
git commit -m "feat(relay): home.* vhost — verify-then-reveal selector assembly from live announces"
```

---

### Task 6: Relay — claim broker (LAN presence + wallet proof → ephemeral Pi→Pi credential transfer)

**Files:**
- Modify: `go/cmd/ftw-relay/home.go` (add the claim-broker handler)
- Modify: `go/cmd/ftw-relay/home_test.go` (claim end-to-end with two fake Pis)
- Modify: `go/cmd/ftw-relay/handlers.go` (register the claim route)

The relay brokers the claim (OQ-3): browser (proven wallet `W` via a fresh verify) + the new Pi's 4-digit presence code → relay calls `replicate/export` on an *old* live Pi of `W` and immediately `replicate/import` on the *new* Pi. The relay holds **zero** durable state — records exist only in-flight in the `Queue`.

- [ ] **Step 1: Write the failing test** — append to `home_test.go`:

```go
// End-to-end claim: an OLD Pi exports its wallet's credential records, the
// relay brokers them to a NEW Pi which imports them under the presence code.
// The relay persists nothing.
func TestHomeClaimBrokersCredentialTransfer(t *testing.T) {
	relay := &Relay{
		Queue: tunnel.NewQueue(), Tokens: NewTokenRegistry(), Owners: NewOwnerRegistry(),
		PollTimeout: 500 * time.Millisecond, HomeHost: "home.fortytwowatts.com",
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	var imported []byte
	oldPi := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// OLD Pi: export returns one credential record.
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"wallet":"W","records":[{"credential_id_b64":"YWJj","public_key_b64":"a2V5","friendly_name":"phone"}]}`))
	})
	newPi := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// NEW Pi: import echoes the body it received, then 204.
		imported, _ = io.ReadAll(r.Body)
		w.WriteHeader(204)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tunnel.NewHost(srv.URL, "h-old", oldPi).Run(ctx)
	go tunnel.NewHost(srv.URL, "h-new", newPi).Run(ctx)

	announce(t, srv.URL, meRegisterRequest{SiteID: "site-old", HostID: "h-old", SiteLabel: "Villa", Wallet: "W"})
	announce(t, srv.URL, meRegisterRequest{SiteID: "site-new", HostID: "h-new", SiteLabel: "Stuga", Wallet: ""}) // not yet enrolled

	// Browser (proven W) submits the new Pi's presence code + target site.
	claim := []byte(`{"wallet":"W","new_site_id":"site-new","code":"1234"}`)
	req, _ := http.NewRequest("POST", srv.URL+"/api/home/claim", bytes.NewReader(claim))
	req.Host = "home.fortytwowatts.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 204 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("claim status=%d body=%q", resp.StatusCode, b)
	}
	if !bytes.Contains(imported, []byte(`"code":"1234"`)) {
		t.Fatalf("new Pi import should carry the presence code, got %q", imported)
	}
	if !bytes.Contains(imported, []byte(`YWJj`)) {
		t.Fatalf("new Pi import should carry the exported credential, got %q", imported)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd go && go test ./cmd/ftw-relay/ -run TestHomeClaim -v`
Expected: FAIL — `/api/home/claim` route undefined.

- [ ] **Step 3: Add the claim-broker handler** in `go/cmd/ftw-relay/home.go`:

```go
// homeClaim brokers a multi-home claim: it asks an OLD live Pi of wallet W to
// export its credential records, then delivers them to the NEW Pi (identified
// by new_site_id) which imports them under the 4-digit presence code. The
// relay holds nothing durable — records live only in-flight in the Queue.
//
// Trust requires BOTH: the wallet must have at least one OTHER live Pi to
// export from (proves W exists + is online), AND the import must succeed under
// the new Pi's LAN-presence code (proves the human is at the new Pi). See
// OQ-3 / OQ-4. NOTE (OQ-2): the caller is expected to have just completed a
// verify-then-reveal for W in this browser session; binding that proof to this
// request (e.g. a short-lived relay-minted claim ticket) is the remaining
// hardening — for Phase 4 the export-side wallet-match + import-side LAN code
// are the two enforced gates. Flag to reviewer.
func (r *Relay) homeClaim(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Wallet    string `json:"wallet"`
		NewSiteID string `json:"new_site_id"`
		Code      string `json:"code"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if body.Wallet == "" || body.NewSiteID == "" || body.Code == "" {
		http.Error(w, "wallet, new_site_id and code required", http.StatusBadRequest)
		return
	}
	newHost, err := r.Owners.Lookup(body.NewSiteID)
	if err != nil {
		http.Error(w, "new home offline", http.StatusServiceUnavailable)
		return
	}
	// Find an OLD live Pi of W to export from (any one that isn't the new Pi).
	var oldHost string
	for _, h := range r.Owners.HomesForWallet(body.Wallet) {
		if h.SiteID != body.NewSiteID {
			oldHost = h.HostID
			break
		}
	}
	if oldHost == "" {
		http.Error(w, "no existing home for this wallet to copy from", http.StatusConflict)
		return
	}

	// 1. Export from the old Pi.
	exportReq, _ := json.Marshal(map[string]string{"wallet": body.Wallet})
	exportResp, err := r.Queue.Enqueue(req.Context(), oldHost, tunnel.TunneledRequest{
		Method: http.MethodPost, Path: "/api/owner-access/replicate/export",
		Header: http.Header{"Content-Type": []string{"application/json"}}, Body: exportReq,
	})
	if err != nil || exportResp.Status != http.StatusOK {
		http.Error(w, "export failed", http.StatusBadGateway)
		return
	}
	var exported struct {
		Wallet  string            `json:"wallet"`
		Records []json.RawMessage `json:"records"`
	}
	if err := json.Unmarshal(exportResp.Body, &exported); err != nil {
		http.Error(w, "bad export payload", http.StatusBadGateway)
		return
	}

	// 2. Import into the new Pi under the presence code.
	importReq, _ := json.Marshal(map[string]any{
		"code": body.Code, "wallet": body.Wallet, "records": exported.Records,
	})
	importResp, err := r.Queue.Enqueue(req.Context(), newHost, tunnel.TunneledRequest{
		Method: http.MethodPost, Path: "/api/owner-access/replicate/import",
		Header: http.Header{"Content-Type": []string{"application/json"}}, Body: importReq,
	})
	if err != nil {
		http.Error(w, "import failed", http.StatusBadGateway)
		return
	}
	w.WriteHeader(importResp.Status)
	_, _ = w.Write(importResp.Body)
}
```

- [ ] **Step 4: Register the route** in `go/cmd/ftw-relay/handlers.go` (after the other `home` routes):

```go
	mux.HandleFunc("POST /api/home/claim", r.homeClaim)
```

- [ ] **Step 5: Run tests**

Run: `cd go && go test ./cmd/ftw-relay/ -run 'TestHome' -v`
Expected: PASS — claim brokers the export→import; the new Pi's import body carries both the presence code and the exported credential.

- [ ] **Step 6: Run the full relay package**

Run: `cd go && go test ./cmd/ftw-relay/`
Expected: `ok`.

- [ ] **Step 7: Commit**

```bash
git add go/cmd/ftw-relay/home.go go/cmd/ftw-relay/home_test.go go/cmd/ftw-relay/handlers.go
git commit -m "feat(relay): claim broker — ephemeral Pi-to-Pi credential transfer (LAN code + wallet proof)"
```

---

### Task 7: Relay — `home.*` vhost host-routing in the process + flag

**Files:**
- Modify: `go/cmd/ftw-relay/main.go` (add `-home-host` flag; set `Relay.HomeHost`)
- Modify: `go/cmd/ftw-relay/handlers.go` (host-router: serve the landing on `GET /` for the HomeHost vhost)

The `home.*` paths are registered (Task 5/6); this task makes the bare `home.fortytwowatts.com/` host serve the login surface and documents the single-cert/single-vhost deployment (P4-3). Since the relay already serves `relay.*` on the same listener, route by `Host` header: requests to `HomeHost` get the home landing at `/`; everything else keeps the Phase 1-3 behaviour.

- [ ] **Step 1: Write the failing test** — append to `home_test.go`:

```go
// GET / on the HomeHost vhost serves the multi-home login surface (not a 404).
func TestHomeVhostServesLandingAtRoot(t *testing.T) {
	relay := &Relay{
		Queue: tunnel.NewQueue(), Tokens: NewTokenRegistry(), Owners: NewOwnerRegistry(),
		HomeHost: "home.fortytwowatts.com",
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.Host = "home.fortytwowatts.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("home vhost / status=%d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(b, []byte("passkey")) { // landing mentions passkey login
		t.Fatalf("expected home landing at /, got %q", b)
	}
}

// GET / WITHOUT the HomeHost header keeps the existing behaviour (404 from the
// bare relay mux — the tunnel routes are explicit).
func TestNonHomeVhostRootUnchanged(t *testing.T) {
	relay := &Relay{
		Queue: tunnel.NewQueue(), Tokens: NewTokenRegistry(), Owners: NewOwnerRegistry(),
		HomeHost: "home.fortytwowatts.com",
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/") // Host = 127.0.0.1
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatalf("non-home root should not serve the home landing")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd go && go test ./cmd/ftw-relay/ -run 'TestHomeVhostServesLandingAtRoot|TestNonHomeVhostRootUnchanged' -v`
Expected: FAIL — bare `/` on the home host 404s (no host-router yet).

- [ ] **Step 3: Add the host-router** in `go/cmd/ftw-relay/handlers.go`. Wrap the returned mux so a `GET /` carrying the `HomeHost` Host header serves the landing. At the end of `Handler()`, replace `return mux` with:

```go
	return r.hostRouter(mux)
}

// hostRouter serves the home.* landing for a bare GET / on the HomeHost vhost,
// and otherwise delegates to the standard mux (Phase 1-3 /h, /me, /tunnel and
// the explicit /api/home/* routes registered above).
func (r *Relay) hostRouter(mux http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if r.HomeHost != "" && req.Host == r.HomeHost && req.Method == http.MethodGet && req.URL.Path == "/" {
			r.homeLanding(w, req)
			return
		}
		mux.ServeHTTP(w, req)
	})
}
```

> Confirm `homeLanding` (Task 5 Step 4 placeholder) serves an HTML page containing the word "passkey". Task 9 replaces its body with the full selector UI; this task only needs it to respond 200 + HTML on the vhost root.

- [ ] **Step 4: Add the flag + wire it** in `go/cmd/ftw-relay/main.go`. Add the flag next to the others (~line 28):

```go
	homeHost := flag.String("home-host", "", "vhost for the multi-home login surface (e.g. home.fortytwowatts.com); empty disables it")
```

Set it on the `Relay` literal (~line 36):

```go
	r := &Relay{
		Queue:       tunnel.NewQueue(),
		Tokens:      NewTokenRegistry(),
		Owners:      NewOwnerRegistry(),
		PollTimeout: *pollTimeout,
		HomeHost:    *homeHost,
	}
```

- [ ] **Step 5: Run tests + vet**

Run: `cd go && go test ./cmd/ftw-relay/ && go vet ./cmd/ftw-relay/`
Expected: `ok`, no vet output.

- [ ] **Step 6: Commit**

```bash
git add go/cmd/ftw-relay/main.go go/cmd/ftw-relay/handlers.go go/cmd/ftw-relay/home_test.go
git commit -m "feat(relay): -home-host vhost routing serves the multi-home landing at /"
```

---

### Task 8: Host wiring — RP-ID cutover to `home.fortytwowatts.com` + announce the wallet on registration

**Files:**
- Modify: `go/cmd/forty-two-watts/main.go` (default `OwnerAccessRPID` → `home.fortytwowatts.com`; pass site label to registration)
- Modify: `go/cmd/forty-two-watts/owner_relay_register.go` (read the local announce; send `site_label` + `wallet` to `/me/register` when a passkey exists)
- Modify: `config.example.yaml` (document the cutover)
- Modify: `docs/adr/0001-passkey-rp-id.md` (flip the Phase-4 note)

**This is the one-way door (OQ-6) — resolve OQ-6 with Fredrik before merging.** Flips the production RP-ID and makes the Pi announce its wallet so it appears in selectors.

- [ ] **Step 1: Flip the RP-ID default** in `go/cmd/forty-two-watts/main.go` (~line 1433):

```go
		OwnerAccessRPID:      envOr("FTW_OWNER_ACCESS_RPID", "home.fortytwowatts.com"),
```

> The env override stays the dev/escape knob. No code other than the default string changes — Phases 1-3 already plumbed `OwnerAccessRPID` end-to-end.

- [ ] **Step 2: Send the announce on registration** in `go/cmd/forty-two-watts/owner_relay_register.go`. The registration goroutine already runs over loopback to the local API. Before each `registerOnce`, read `/api/owner-access/announce` to learn `{site_label, wallet, enrolled}`, and include them in the `/me/register` body only when `enrolled` is true (OQ-1: never announce a wallet the Pi can't verify). Change the `registerOnce` closure:

```go
	registerOnce := func() {
		label, wallet := "", ""
		if a, err := fetchLocalAnnounce(ctx); err == nil && a.Enrolled {
			label, wallet = a.SiteLabel, a.Wallet
		}
		body, _ := json.Marshal(map[string]string{
			"site_id": siteID, "host_id": hostID,
			"site_label": label, "wallet": wallet,
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, relayURL+"/me/register", bytes.NewReader(body))
		// ... unchanged from here ...
	}
```

Add the local-announce fetcher near the bottom of the file:

```go
type localAnnounce struct {
	SiteLabel string `json:"site_label"`
	Wallet    string `json:"wallet"`
	Enrolled  bool   `json:"enrolled"`
}

// fetchLocalAnnounce reads the host's own /api/owner-access/announce over
// loopback so the relay registration can carry {site_label, wallet}. Loopback
// is unmarked (no X-FTW-Tunnel), so the gate's open-path lets it through.
func fetchLocalAnnounce(ctx context.Context) (localAnnounce, error) {
	var a localAnnounce
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:8080/api/owner-access/announce", nil)
	if err != nil {
		return a, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return a, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return a, fmt.Errorf("announce status %d", resp.StatusCode)
	}
	return a, json.NewDecoder(resp.Body).Decode(&a)
}
```

> `encoding/json`, `fmt`, `net/http`, `context` are already imported in this file. The `:8080` literal matches the existing `runOwnerLongPoll` assumption (`owner_relay_register.go:143`); if the API port is ever made configurable, both must change together — leave a `// OQ-1` note.

- [ ] **Step 3: Build + vet (no unit test — process wiring, exercised in e2e)**

Run: `cd go && go build ./... && go vet ./cmd/forty-two-watts/ ./cmd/ftw-relay/`
Expected: no output (success).

- [ ] **Step 4: Document the cutover** in `config.example.yaml`. Find the owner-access / relay env documentation block (search for `FTW_OWNER_ACCESS_RPID`) and update it to state the production default is now `home.fortytwowatts.com`, with the one-way-door warning (cross-reference `docs/adr/0001-passkey-rp-id.md`). If no such block exists, add a short commented section near the relay config.

- [ ] **Step 5: Flip the ADR's Phase-4 note** in `docs/adr/0001-passkey-rp-id.md`. Change the Sequencing section's Phase-4 bullet from a future-tense "when home.* exists, flip the default" to past-tense "Phase 4 (done): default is `home.fortytwowatts.com`; real passkey enrollment begins here." Keep the one-way-door warning intact.

- [ ] **Step 6: Commit**

```bash
git add go/cmd/forty-two-watts/main.go go/cmd/forty-two-watts/owner_relay_register.go config.example.yaml docs/adr/0001-passkey-rp-id.md
git commit -m "feat(owner-access): RP-ID cutover to home.fortytwowatts.com + announce wallet on registration"
```

---

### Task 9: Selector UI — live-homes list after verify-then-reveal

**Files:**
- Modify: `web/owner-access/index.html` (selector list)
- Modify: `go/cmd/ftw-relay/home.go` (`homeLanding` serves the Conditional-UI page + selector, or serves the embedded HTML)

**Follow `DESIGN.md`** — colour tokens only, mono eyebrow labels, 1px hairlines, no drop-shadows, one amber accent, on-accent text `#0a0a0a`. Reuse the Phase-3 `web/owner-access/index.html` panel vocabulary (`.panel`, `.row`, `.muted`, `.code`, `var(--accent-e)` etc.).

> **Where the page lives.** The Phase-3 `web/owner-access/index.html` is served *by the Pi* (through the tunnel). The Phase-4 `home.*` landing is served *by the relay* (`homeLanding`) because it must exist before any Pi is chosen. Decision: embed the selector HTML as a `const homeLandingHTML` string in `home.go` (the relay has no static-file dir; the existing relay landing page `landingHTML` in `handlers.go` is already an embedded const — follow that pattern). The page calls the relay's `/api/home/login/{start,finish}` + `/api/home/open/{site_id}` + `/api/home/claim`.

- [ ] **Step 1: Build the selector flow in `homeLandingHTML`** (in `home.go`). The page must:
  1. On load, run Conditional-UI passkey autofill (reuse the Phase-3 login.html JS shape): `navigator.credentials.get({ publicKey, mediation: "conditional" })` against a challenge from `POST /api/home/login/start`.
  2. POST the assertion to `POST /api/home/login/finish`. On 200 with `{homes:[...]}`, render the selector list — one row per home: `SiteLabel ●` + an "Open" button.
  3. "Open" → `POST /api/home/open/{site_id}` (re-runs that Pi's verification — one tap per home, P4-1) → on success, redirect the browser to that home's dashboard via the relay's existing `/me/<site_id>/` path (or surface the Set-Cookie + a dashboard link).
  4. A "Claim another home" affordance: prompt for the new Pi's 4-digit presence code + target, `POST /api/home/claim`.

Structure the markup to match Phase-3 panels:

```html
<div id="selector" class="panel" style="display:none">
  <p class="eyebrow">YOUR HOMES</p>
  <ul id="home-list"></ul>
  <p class="muted">Opening a home asks for your passkey once more — that's one tap per home, by design.</p>
</div>
```

with `.eyebrow` using `var(--mono)`, UPPERCASE, `letter-spacing: 0.18em`, and the status dot `●` using the sanctioned accent-glow on a 6px dot (per `DESIGN.md`). Render each home:

```js
function renderHomes(homes) {
  const ul = document.getElementById("home-list");
  ul.innerHTML = "";
  for (const h of homes) {
    const li = document.createElement("li");
    li.className = "row";
    li.innerHTML = `<span class="dot"></span><span>${escapeHTML(h.site_label)}</span>`;
    const btn = document.createElement("button");
    btn.className = "primary";
    btn.textContent = "Open";
    btn.onclick = () => openHome(h.site_id);
    li.appendChild(btn);
    ul.appendChild(li);
  }
  show("selector");
}
```

- [ ] **Step 2: Update `web/owner-access/index.html`** so the Pi-served page links to the `home.*` selector when reached remotely (a one-line "Open your homes at home.fortytwowatts.com" panel), keeping the existing single-home flow for LAN. Do not duplicate the selector logic in two places — the relay's `homeLandingHTML` is the canonical multi-home surface; `index.html` just points to it when remote.

- [ ] **Step 3: Manual verification (no unit test for HTML — verify by build + visual reasoning)**

Run: `cd go && go build ./cmd/ftw-relay/` (the embedded HTML compiles as part of the package).
Then re-run the relay tests that assert the landing contains "passkey" / serves 200:

Run: `cd go && go test ./cmd/ftw-relay/ -run 'TestHomeVhostServesLandingAtRoot|TestHomeVerifyThenReveal' -v`
Expected: PASS — the landing serves and the verify-then-reveal JSON shape matches what the page consumes.

- [ ] **Step 4: DESIGN.md compliance self-check** — grep the new HTML for hard-coded hex colours that aren't inside a `var(--… , #fallback)` default:

Run: `cd go && grep -nE '#[0-9a-fA-F]{3,6}' cmd/ftw-relay/home.go | grep -v 'var(--' | grep -v '0a0a0a'`
Expected: only sanctioned values (`#0a0a0a` on-accent text, fallbacks inside `var(--x, #fallback)`). Anything else must be tokenized.

- [ ] **Step 5: Commit**

```bash
git add web/owner-access/index.html go/cmd/ftw-relay/home.go
git commit -m "feat(home): multi-home selector UI — verify-then-reveal list with one-tap-per-home open"
```

---

### Task 10: Docs + changeset + ADR finalization

**Files:**
- Modify: `docs/relay-deploy.md` (the `home.fortytwowatts.com` vhost, DNS A-record, single TLS cert, `-home-host` flag)
- Modify: `docs/nova-integration.md` or a new `docs/home-route.md` (cross-link the multi-home flow) — optional, only if a natural home exists
- Create: `.changeset/<name>.md` (minor — new endpoints + new relay surface + RP-ID cutover note)

- [ ] **Step 1: Document the relay vhost** in `docs/relay-deploy.md`. Add a "Multi-home (`home.fortytwowatts.com`)" section: DNS A-record pointing at the relay VM, the single TLS cert covering `home.*` (Caddy/Let's Encrypt or Cloudflare origin cert — OQ resolved at deploy time), and the `-home-host home.fortytwowatts.com` flag. State the privacy property (verify-then-reveal) and the one-tap-per-home property as deliberate.

- [ ] **Step 2: Note the limitations** explicitly in the deploy doc: (a) revocation does NOT fan out across Pis yet (OQ-7 — a replicated credential deleted on one Pi remains on others), (b) stale-announce window is up to one register interval (OQ-1), (c) RP-ID cutover invalidates any passkey enrolled under `relay.*` (OQ-6 — none should exist).

- [ ] **Step 3: Write the changeset** from the repo root:

```bash
npx changeset
```

Pick **minor** (new endpoints, new relay surface, new device support for multi-home). Summary (English — this is user-visible release content):

```
Multi-home owner access: one passkey, all your online homes. Sign in once at
home.fortytwowatts.com to see every Pi that's currently online for your
wallet; claim additional homes by entering a presence code shown on the new
Pi's local screen — the passkey is replicated Pi-to-Pi, brokered by the
stateless relay (which stores nothing). RP-ID cutover to
home.fortytwowatts.com; passkeys enrolled under the old relay.* host must be
re-enrolled on the LAN. Known limitation: credential revocation does not yet
propagate across your Pis.
```

- [ ] **Step 4: Commit**

```bash
git add docs/relay-deploy.md .changeset/
git commit -m "docs(home): multi-home relay deploy + limitations + changeset (minor)"
```

---

## Phase 4 verification (run before declaring done)

- [ ] `cd go && go test ./internal/state/ ./internal/api/ ./cmd/ftw-relay/` → `ok` all three.
- [ ] `cd go && go vet ./... && go build ./...` → clean.
- [ ] `make verify` (vet + test + build) → green, mirroring CI.
- [ ] Manual reasoning check against the design's properties:
  - **Stateless relay:** the relay persists nothing — announces are live metadata (TTL-expired), claim records exist only in-flight in the `Queue`. ✓ Tasks 4+6.
  - **Verify-then-reveal:** an unverified `userHandle` reveals zero homes; the list appears only after one live Pi returns 200. ✓ Task 5 (`TestHomeUnverifiedWalletRevealsNothing`).
  - **One tap per home:** opening a chosen home re-runs that Pi's `login/finish`. ✓ Task 5 (`homeOpen`).
  - **Claim needs BOTH LAN presence AND wallet proof:** import gates on the new Pi's LAN-only presence code; export gates on wallet-match + tunnel arrival. ✓ Tasks 3+6.
  - **Replicated vs local creds distinguishable + revocable:** `Origin` flag persisted; existing `DELETE /devices/{id}` removes either. ✓ Task 1.
  - **RP-ID one-way door:** default flipped to `home.*`; ADR + changeset warn re-enrollment. ✓ Task 8 (gated on OQ-6 with Fredrik).
  - **Stale home does not 503 the selector forever:** announces age out at `announceTTL`. ✓ Task 4 (`TestOwnerRegistryExpiresStaleAnnounces`).

---

## Self-review (done at authoring time)

- **Design coverage (the six "New (Phase 4)" items):**
  1. Relay `home.*` vhost (login surface + wallet-routing + selector assembly) → Tasks 5, 7, 9.
  2. Pi → relay announce protocol (`{site_label, wallet}` extending `/me/register`) → Tasks 2, 4, 8.
  3. Verify-then-reveal → Task 5.
  4. Claim handshake (LAN presence + wallet proof → ephemeral Pi-to-Pi replication) → Tasks 3, 6.
  5. RP-ID cutover → Task 8.
  6. Selector UI → Task 9.
- **Open-question discipline:** every genuinely-hard/uncertain bit is an explicit `OQ-N` tied to its task, NOT a silent guess — announce freshness (OQ-1), verify-then-reveal round trip & two-tap UX (OQ-2, OQ-5), claim ephemeral channel trust binding (OQ-3), LAN-presence code lifecycle (OQ-4), RP-ID cutover coordination (OQ-6), revocation propagation scope (OQ-7). OQ-2/OQ-3/OQ-6/OQ-7 are flagged for reviewer/Fredrik sign-off; the rest are coded with a stated assumption + a `// OQ-N` marker.
- **Placeholder scan:** none — every step has concrete code/commands or a clearly-scoped HTML/doc edit. The only deliberately-deferred behaviour (cross-Pi revocation fan-out, single-home one-tap fast path) is called out as out-of-scope, not stubbed.
- **Type / signature consistency (against real code read):**
  - `state.TrustedDevice.Origin string` + `state.OriginLocal` / `state.OriginReplicated` (Task 1) used by `handleOwnerEnrollFinish`, `handleOwnerReplicateImport` (Tasks 2, 3).
  - `meRegisterRequest{SiteID, HostID, SiteLabel, Wallet}` (Task 4) matches the host's `/me/register` body (Task 8) and the relay's `Announce{SiteID, HostID, SiteLabel, Wallet}`.
  - `OwnerRegistry.Announce(Announce)`, `.HomesForWallet(string) []Home`, `.anyLiveHost() (string, bool)`, `.Lookup(string) (string, error)` (existing) used consistently across Tasks 4, 5, 6.
  - `Relay.HomeHost string` + `Relay.Queue.Enqueue(ctx, hostID, tunnel.TunneledRequest{...}) (tunnel.TunneledResponse, error)` (real signature, `queue.go:43`) used in `homeLoginFinish`, `homeOpen`, `homeClaim`, `proxyToHost`.
  - `(*Server).isTunneled(*http.Request) bool` (existing, `api_owner_access.go:296`) reused by the new replicate/claim handlers (Task 3).
  - `readBody(io.Reader) ([]byte, error)`, `esc`, `writeJSON`/`readJSON` reused, not re-implemented.
- **Reuse discipline:** the claim 4-digit code mirrors the existing relay token-approval shape (`MaxApprovalAttempts`, TTL) rather than inventing a new mechanism; the `home.*` landing follows the embedded-`const`-HTML pattern of the existing relay `landingHTML`; `proxyToHost` factors the enqueue-and-write logic shared with the Phase-3 `meForward`.
- **Security review hooks:** RP-ID cutover (one-way door) gated on OQ-6 human sign-off; replicate/export over tunnel-only + wallet-match; replicate/import LAN-presence-code-gated; verify-then-reveal privacy property tested; constant-time wallet compare in export.
```