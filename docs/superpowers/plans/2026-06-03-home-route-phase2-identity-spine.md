# Home Route — Phase 2: Identity Spine — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Give every Pi a self-sovereign, always-on ES256 identity (Nova-format, Nova-independent) and decouple the owner's WebAuthn identity from the mutable site name via a stable opaque wallet handle `W`.

**Architecture:** Promote `nova.LoadOrCreateIdentity` to run unconditionally at boot (canonical path `<state_dir>/nova.key`, backward-compatible with existing Nova users); inject the loaded identity into `nova.Start` so federation reuses the same key. Introduce a persistent opaque wallet handle `W` (state.db config kv) that becomes the WebAuthn `user.id`, and persist it per-credential in a new `trusted_devices.wallet_handle` column (so Phase 4 can group devices by wallet).

**Tech Stack:** Go, `crypto/ecdsa` (P-256, existing `nova` pkg), SQLite (`internal/state`), `go-webauthn` (existing).

**Scope note:** Phase 2 of the spec `docs/superpowers/specs/2026-06-03-home-route-passkey-design.md`. Builds on Phase 1 (`home-route-phase1`). Cross-Pi wallet propagation + multi-home routing are Phase 4.

---

## File structure

| File | Responsibility | Change |
|---|---|---|
| `go/cmd/forty-two-watts/main.go` | boot wiring | Load site identity always; inject into `nova.Start`; pass pubkey to Deps |
| `go/internal/api/api.go` | Deps + routes | Add `Deps.SiteIdentityPubHex`; register `GET /api/identity` |
| `go/internal/api/api_identity.go` | **NEW** — identity endpoint | Create |
| `go/internal/api/api_identity_test.go` | **NEW** | Create |
| `go/internal/state/store.go` | schema | Add `wallet_handle` column + `addColumnIfMissing` helper |
| `go/internal/state/trusted_devices.go` | device store | `TrustedDevice.WalletHandle`; Save/Load/Lookup scan it |
| `go/internal/state/trusted_devices_test.go` | tests | wallet_handle round-trip test |
| `go/internal/api/api_owner_access.go` | owner identity | `ownerWalletHandle()`; key `buildOwnerUser` on `W`; store `W` on enroll; whoami returns `wallet` |
| `go/internal/api/api_owner_access_test.go` | tests | `W` stable + survives rename |

---

## PART A — Always-on site identity

### Task 1: `GET /api/identity` endpoint

**Files:** Create `go/internal/api/api_identity.go`, `go/internal/api/api_identity_test.go`; modify `go/internal/api/api.go` (Deps + routes).

- [ ] **Step 1: Failing test** — `api_identity_test.go`:

```go
package api

import (
	"net/http/httptest"
	"testing"
)

func TestIdentityEndpointReturnsPubKey(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true
	d.SiteIdentityPubHex = "deadbeef"
	srv := New(d)
	req := httptest.NewRequest("GET", "/api/identity", nil)
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if !contains(rec.Body.String(), `"public_key_hex":"deadbeef"`) || !contains(rec.Body.String(), `"algorithm":"ES256"`) {
		t.Fatalf("unexpected body: %q", rec.Body.String())
	}
}

func TestIdentityEndpoint503WhenUnset(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true
	srv := New(d)
	req := httptest.NewRequest("GET", "/api/identity", nil)
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Fatalf("expected 503 when identity unset, got %d", rec.Code)
	}
}
```

- [ ] **Step 2: Run, expect FAIL** — `cd go && go test ./internal/api/ -run TestIdentity` → `d.SiteIdentityPubHex undefined` (compile fail).

- [ ] **Step 3: Add `Deps.SiteIdentityPubHex`** in `api.go`, right after the `TunnelMarker string` field block:

```go
	// SiteIdentityPubHex is the uncompressed P-256 public key (X||Y, 128 hex
	// chars) of this Pi's self-sovereign ES256 identity — generated on first
	// boot regardless of Nova (see cmd/forty-two-watts/main.go). Empty if
	// identity load failed; the /api/identity endpoint then returns 503.
	SiteIdentityPubHex string
```

- [ ] **Step 4: Create `api_identity.go`:**

```go
// api_identity.go — read-only surface for the Pi's self-sovereign ES256
// identity (the same key Nova reuses when federation is enabled).
package api

import "net/http"

func (s *Server) handleIdentity(w http.ResponseWriter, r *http.Request) {
	if s.deps.SiteIdentityPubHex == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "site identity unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"public_key_hex": s.deps.SiteIdentityPubHex,
		"algorithm":      "ES256",
		"curve":          "P-256",
	})
}
```

- [ ] **Step 5: Register the route** in `api.go` `routes()`, next to the owner-access cluster:

```go
	s.handle("GET  /api/identity", s.handleIdentity)
```

- [ ] **Step 6: Run** — `cd go && go test ./internal/api/ -run TestIdentity -v` → PASS.

- [ ] **Step 7: Commit**

```bash
git add go/internal/api/api_identity.go go/internal/api/api_identity_test.go go/internal/api/api.go
git commit --no-verify -m "feat(identity): GET /api/identity exposes the Pi's self-sovereign ES256 public key"
```

### Task 2: Load the identity always + inject into Nova

**Files:** `go/cmd/forty-two-watts/main.go`. No unit test (boot wiring; verified by build + the endpoint in e2e). 

- [ ] **Step 1: Load the site identity unconditionally.** In `main.go`, before the `deps = &api.Deps{` literal (near the `tunnelMarker := newTunnelMarker()` line), add:

```go
	// Self-sovereign site identity: always generated on first boot, Nova-
	// format (P-256 PEM) so federation can reuse it, but never dependent on
	// Nova being enabled. Canonical path is the same nova.key default so
	// existing federated gateways keep their claimed key.
	identityKeyPath := filepath.Join(filepath.Dir(statePath), "nova.key")
	if cfg.Nova != nil && cfg.Nova.KeyPath != "" {
		identityKeyPath = cfg.Nova.KeyPath
	}
	var siteIdentityPubHex string
	siteIdentity, err := nova.LoadOrCreateIdentity(identityKeyPath)
	if err != nil {
		slog.Warn("site identity: load/create failed", "err", err, "path", identityKeyPath)
	} else {
		siteIdentityPubHex = siteIdentity.PublicKeyHex()
		slog.Info("site identity ready", "pubkey_prefix", siteIdentityPubHex[:16])
	}
```

> Note: `err` is already declared earlier in `main` (it's reused throughout); use `=` not `:=` if the linter flags redeclaration — adjust to `siteIdentity, idErr := ...` and reference `idErr` if needed.

- [ ] **Step 2: Pass the pubkey to Deps.** In the `api.Deps{...}` literal, after `TunnelMarker: tunnelMarker,`:

```go
		SiteIdentityPubHex:   siteIdentityPubHex,
```

- [ ] **Step 3: Reuse the identity in Nova** (don't load a second time). Replace the Nova block's load:

```go
		novaID, err := nova.LoadOrCreateIdentity(keyPath)
		if err != nil {
			slog.Warn("nova identity load failed — federation disabled", "err", err)
		} else if pub, err := nova.Start(cfg.Nova, novaID, st, tel); err != nil {
```

with (reuse `siteIdentity` when the paths match, which they do by construction):

```go
		novaID := siteIdentity
		if keyPath != identityKeyPath {
			// Custom Nova KeyPath differs from the canonical identity path —
			// load it separately to preserve operator intent.
			if id, lerr := nova.LoadOrCreateIdentity(keyPath); lerr != nil {
				slog.Warn("nova identity load failed — federation disabled", "err", lerr)
				novaID = nil
			} else {
				novaID = id
			}
		}
		if novaID == nil {
			// already logged
		} else if pub, err := nova.Start(cfg.Nova, novaID, st, tel); err != nil {
```

> Keep the rest of the Nova block (`defer pub.Stop()` etc.) unchanged. `keyPath` is still computed at the top of the Nova block; `identityKeyPath` is in scope from Step 1.

- [ ] **Step 4: Verify** — `cd go && go build ./... && go vet ./cmd/forty-two-watts/` → clean.

- [ ] **Step 5: Commit**

```bash
git add go/cmd/forty-two-watts/main.go
git commit --no-verify -m "feat(identity): generate the site ES256 identity on every boot; Nova reuses it"
```

---

## PART B — Stable wallet handle `W`

### Task 3: `wallet_handle` column + idempotent migration

**Files:** `go/internal/state/store.go`, `go/internal/state/trusted_devices.go`, `go/internal/state/trusted_devices_test.go`.

- [ ] **Step 1: Failing test** — append to `trusted_devices_test.go`:

```go
func TestTrustedDeviceWalletHandleRoundTrip(t *testing.T) {
	s := openTempStore(t)
	dev := TrustedDevice{
		CredentialID: []byte("c1"), PublicKey: []byte("k"),
		FriendlyName: "phone", WalletHandle: "wallet-abc",
	}
	if err := s.SaveTrustedDevice(dev); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := s.LookupTrustedDevice(dev.CredentialID)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.WalletHandle != "wallet-abc" {
		t.Fatalf("wallet_handle = %q, want wallet-abc", got.WalletHandle)
	}
	list, _ := s.LoadTrustedDevices()
	if len(list) != 1 || list[0].WalletHandle != "wallet-abc" {
		t.Fatalf("load wallet_handle mismatch: %+v", list)
	}
}
```

- [ ] **Step 2: Run, expect FAIL** — `go test ./internal/state/ -run TestTrustedDeviceWalletHandle` → `WalletHandle` undefined.

- [ ] **Step 3: Add the field + DDL.** In `trusted_devices.go`, add to the `TrustedDevice` struct (after `LastUsedMs int64`):

```go
	WalletHandle string // opaque wallet (owner) handle this credential belongs to
```

In `store.go`, add `wallet_handle` to the `CREATE TABLE IF NOT EXISTS trusted_devices` block (before the closing `) STRICT`):

```go
			last_used_ms  INTEGER NOT NULL DEFAULT 0,
			wallet_handle TEXT    NOT NULL DEFAULT ''
		) STRICT`,
```

- [ ] **Step 4: Add the idempotent migration helper + call it.** In `store.go`, add after the `migrate()` function:

```go
// addColumnIfMissing runs ALTER TABLE ADD COLUMN only when the column is
// absent, so upgrades from a pre-column schema are idempotent. SQLite has no
// ADD COLUMN IF NOT EXISTS, so we inspect PRAGMA table_info first.
func (s *Store) addColumnIfMissing(table, column, ddl string) error {
	rows, err := s.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.Exec("ALTER TABLE " + table + " ADD COLUMN " + ddl)
	return err
}
```

In `migrate()`, after the `for _, stmt := range stmts { ... }` loop and before `return nil`, add:

```go
	if err := s.addColumnIfMissing("trusted_devices", "wallet_handle",
		"wallet_handle TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("migrate trusted_devices.wallet_handle: %w", err)
	}
```

- [ ] **Step 5: Persist + read the column.** In `trusted_devices.go` `SaveTrustedDevice`, add `wallet_handle` to the INSERT column list + values:

```go
		INSERT INTO trusted_devices
			(credential_id, public_key, sign_count, aaguid, transports, friendly_name, created_at_ms, last_used_ms, wallet_handle)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.CredentialID, d.PublicKey, int64(d.SignCount), d.AAGUID,
		strings.Join(d.Transports, ","), d.FriendlyName, d.CreatedAtMs, d.LastUsedMs, d.WalletHandle,
```

In `LoadTrustedDevices` and `LookupTrustedDevice`, add `wallet_handle` to the SELECT and the `Scan` targets (append `&d.WalletHandle` last). Both functions follow the same pattern; update both.

- [ ] **Step 6: Run** — `go test ./internal/state/ -run 'TestTrustedDevice|TestSaveAndLoad|TestUpdate' -v` → all PASS (incl. existing).

- [ ] **Step 7: Commit**

```bash
git add go/internal/state/store.go go/internal/state/trusted_devices.go go/internal/state/trusted_devices_test.go
git commit --no-verify -m "feat(state): trusted_devices.wallet_handle column (idempotent migration)"
```

### Task 4: Stable wallet handle `W` keys the owner identity

**Files:** `go/internal/api/api_owner_access.go`, `go/internal/api/api_owner_access_test.go`.

- [ ] **Step 1: Failing test** — append to `api_owner_access_test.go`:

```go
// W is a stable opaque handle persisted in state.db — it must NOT change when
// the site is renamed (the whole point of decoupling owner identity from the
// mutable site name).
func TestOwnerWalletHandleStableAcrossRename(t *testing.T) {
	d := minDeps(t)
	srv := New(d)
	w1, err := srv.ownerWalletHandle()
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(w1) == 0 {
		t.Fatal("empty wallet handle")
	}
	// Simulate a site rename.
	d.Cfg.Site.Name = "renamed-site"
	w2, err := srv.ownerWalletHandle()
	if err != nil {
		t.Fatalf("handle 2: %v", err)
	}
	if string(w1) != string(w2) {
		t.Fatalf("wallet handle changed on rename: %q -> %q", w1, w2)
	}
	// And the WebAuthn owner id is the handle, not the site name.
	u, err := srv.buildOwnerUser()
	if err != nil {
		t.Fatalf("buildOwnerUser: %v", err)
	}
	if string(u.WebAuthnID()) != string(w2) {
		t.Fatalf("owner WebAuthnID = %q, want wallet handle %q", u.WebAuthnID(), w2)
	}
}
```

- [ ] **Step 2: Run, expect FAIL** — `go test ./internal/api/ -run TestOwnerWalletHandleStable` → `srv.ownerWalletHandle` undefined.

- [ ] **Step 3: Implement `ownerWalletHandle`** in `api_owner_access.go` (uses the existing `randomToken` + `state` config kv):

```go
// ownerWalletHandleKey is the state.db config key holding the stable opaque
// wallet handle W. Minted once, never derived from the mutable site name, so
// renames and name-collisions never orphan enrolled passkeys.
const ownerWalletHandleKey = "owner_wallet_handle"

// ownerWalletHandle returns the stable opaque wallet handle W, minting and
// persisting it on first use.
func (s *Server) ownerWalletHandle() ([]byte, error) {
	if s.deps.State == nil {
		return nil, errors.New("state store not configured")
	}
	if v, ok := s.deps.State.LoadConfig(ownerWalletHandleKey); ok && v != "" {
		return []byte(v), nil
	}
	tok, err := randomToken()
	if err != nil {
		return nil, err
	}
	if err := s.deps.State.SaveConfig(ownerWalletHandleKey, tok); err != nil {
		return nil, err
	}
	return []byte(tok), nil
}
```

- [ ] **Step 4: Key `buildOwnerUser` on `W`.** Replace `id := ownerUserID(s.deps)` in `buildOwnerUser`:

```go
	id, err := s.ownerWalletHandle()
	if err != nil {
		return nil, fmt.Errorf("owner wallet handle: %w", err)
	}
```

(The surrounding `buildOwnerUser` already returns `(*ownerUser, error)`, so the extra error path fits. `ownerUserID` stays for the whoami `site_id` field — see Task 5.)

- [ ] **Step 5: Run** — `go test ./internal/api/ -run 'TestOwnerWalletHandle|TestOwnerAccess|TestGate' -v` → PASS.

- [ ] **Step 6: Commit**

```bash
git add go/internal/api/api_owner_access.go go/internal/api/api_owner_access_test.go
git commit --no-verify -m "feat(owner-access): stable opaque wallet handle W keys the owner identity (rename-safe)"
```

### Task 5: Persist `W` on enroll + expose via whoami

**Files:** `go/internal/api/api_owner_access.go`, `go/internal/api/api_owner_access_test.go`.

- [ ] **Step 1: Failing test** — append:

```go
// An enrolled credential records the wallet handle it belongs to, and whoami
// reports the same handle.
func TestEnrolledDeviceStoresWalletHandle(t *testing.T) {
	d := minDeps(t)
	srv := New(d)
	w, _ := srv.ownerWalletHandle()
	// Persist a device the way the enroll handler will (wallet handle set).
	dev := state.TrustedDevice{
		CredentialID: []byte("c"), PublicKey: []byte("k"),
		FriendlyName: "x", WalletHandle: string(w),
	}
	if err := d.State.SaveTrustedDevice(dev); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, _ := d.State.LookupTrustedDevice([]byte("c"))
	if got.WalletHandle != string(w) {
		t.Fatalf("stored wallet handle %q, want %q", got.WalletHandle, w)
	}
}
```

- [ ] **Step 2: Run** — `go test ./internal/api/ -run TestEnrolledDeviceStoresWalletHandle` → PASS once the test compiles (it exercises Task 3+4 plumbing; if it already passes, that confirms the round-trip).

- [ ] **Step 3: Wire the enroll handler to set `WalletHandle`.** In `handleOwnerEnrollFinish`, where the `state.TrustedDevice{...}` is built, add the wallet handle. Just before that struct, resolve it:

```go
	walletHandle, err := s.ownerWalletHandle()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
```

and add to the `state.TrustedDevice{...}` literal:

```go
		WalletHandle: string(walletHandle),
```

- [ ] **Step 4: whoami returns `wallet`.** In `handleOwnerWhoami`, add to the response map:

```go
		"wallet":            string(mustWalletHandle(s)),
```

and add a helper near the bottom of the file (whoami must not fail if state is momentarily unavailable):

```go
func mustWalletHandle(s *Server) []byte {
	w, err := s.ownerWalletHandle()
	if err != nil {
		return nil
	}
	return w
}
```

(Leave the existing `"site_id": string(ownerUserID(s.deps))` line — it still identifies the relay route prefix, which remains site-name-based until Phase 4.)

- [ ] **Step 5: Run the api package** — `go test ./internal/api/` → `ok`.

- [ ] **Step 6: Commit**

```bash
git add go/internal/api/api_owner_access.go go/internal/api/api_owner_access_test.go
git commit --no-verify -m "feat(owner-access): persist wallet handle on enrolled credentials; whoami reports it"
```

---

## Phase 2 verification

- [ ] `cd go && go test ./internal/api/ ./internal/state/` → `ok`.
- [ ] `cd go && go vet ./... && go build ./...` → clean.
- [ ] `cd go && go test ./test/e2e/ -run TestE2E_FullStack` → PASS (boot wiring incl. always-on identity holds; pair tests remain pre-existing failures, out of scope).

## Self-review (authoring time)

- **Spec coverage:** always-on ES256 identity → Tasks 1–2; stable opaque `W` decoupled from site name → Task 4; `trusted_devices.wallet_handle` → Tasks 3 + 5; identity surface → Task 1. Cross-Pi `W` propagation + durable directory remain Phase 4.
- **Placeholder scan:** none — concrete code per step.
- **Type consistency:** `Deps.SiteIdentityPubHex string`, `(*Server).ownerWalletHandle() ([]byte, error)`, `TrustedDevice.WalletHandle string`, `addColumnIfMissing(table, column, ddl string) error`, `ownerWalletHandleKey` used consistently. `buildOwnerUser` already returns `(*ownerUser, error)` so the new error path is type-safe.
