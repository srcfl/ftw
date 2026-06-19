# Home Route — Phase 3: Usernameless Login — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Make owner sign-in usernameless — discoverable resident-key passkeys + Conditional-UI autofill, with the existing button as a modal fallback — and add a light "add a backup passkey" recovery nudge. Minimal-visual (keep the current pages); get it working first.

**Architecture:** One pair of endpoints (`/api/owner-access/login/{start,finish}`) serves both flows. The server switches to `BeginDiscoverableLogin` (empty `allowCredentials`) + `FinishDiscoverableLogin` with a `DiscoverableUserHandler` that resolves the single owner from the assertion's `userHandle` (== the Phase-2 wallet handle `W`). Credentials are created as resident keys (`ResidentKey: Required`). The client adds a Conditional-UI `navigator.credentials.get({mediation:'conditional'})` on load against an `autocomplete="username webauthn"` field; the existing button stays as the fallback.

**Tech Stack:** `go-webauthn/webauthn` v0.17.4 (`BeginDiscoverableLogin`/`FinishDiscoverableLogin`/`DiscoverableUserHandler`), vanilla JS in `web/owner-access/`.

**Scope note:** Phase 3 of the spec. Builds on Phases 1–2 (`home-route-phase1`). One-time recovery *code* generation is deferred (the LAN-bypass path + a backup passkey are the v1 recovery story). RP-ID stays `relay.fortytwowatts.com` until Phase 4.

---

## File structure

| File | Change |
|---|---|
| `go/internal/api/api_owner_access.go` | `ResidentKey: Required`; `login/start`→discoverable; `login/finish`→discoverable; add `resolveDiscoverableOwner` |
| `go/internal/api/api_owner_access_test.go` | discoverable-resolution + discoverable-start tests |
| `web/owner-access/login.html` | Conditional-UI autofill field + on-load conditional get; keep button fallback |
| `web/owner-access/index.html` | light "add a backup passkey" nudge when exactly 1 device enrolled |

---

### Task 1: Server — discoverable login

**Files:** `go/internal/api/api_owner_access.go`, `go/internal/api/api_owner_access_test.go`.

- [ ] **Step 1: Failing tests** — append to `api_owner_access_test.go`:

```go
// The discoverable-login handler resolves the single owner from the
// assertion's userHandle (== the wallet handle W), and rejects any other.
func TestResolveDiscoverableOwner(t *testing.T) {
	d := minDeps(t)
	srv := New(d)
	w, _ := srv.ownerWalletHandle()
	u, err := srv.resolveDiscoverableOwner([]byte("rawid"), w)
	if err != nil {
		t.Fatalf("resolve with correct handle: %v", err)
	}
	if string(u.WebAuthnID()) != string(w) {
		t.Fatalf("resolved wrong user: %q", u.WebAuthnID())
	}
	if _, err := srv.resolveDiscoverableOwner([]byte("rawid"), []byte("not-the-wallet")); err == nil {
		t.Fatal("expected error for unknown wallet handle")
	}
}

// login/start must be discoverable: 200 with NO allowCredentials leaking the
// enrolled credential id (BeginLogin would include it; BeginDiscoverableLogin
// must not). 404 stays when nothing is enrolled.
func TestLoginStartIsDiscoverable(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true
	srv := New(d)
	if err := d.State.SaveTrustedDevice(state.TrustedDevice{
		CredentialID: []byte("seed"), PublicKey: []byte("k"), FriendlyName: "x",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	req := httptest.NewRequest("POST", "/api/owner-access/login/start", nil)
	req.Host = "127.0.0.1"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	// base64url("seed") == "c2VlZA" — must NOT appear in allowCredentials.
	if contains(rec.Body.String(), "c2VlZA") {
		t.Fatalf("allowCredentials leaked credential id — not discoverable: %q", rec.Body.String())
	}
}
```

- [ ] **Step 2: Run, expect FAIL** — `cd go && go test ./internal/api/ -run 'TestResolveDiscoverableOwner|TestLoginStartIsDiscoverable'` → `resolveDiscoverableOwner` undefined; the start test will then fail (allowCredentials still present).

- [ ] **Step 3: ResidentKey → Required.** In `webauthnLib`, change:

```go
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementPreferred,
			UserVerification: protocol.VerificationPreferred,
		},
```

to:

```go
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationPreferred,
		},
```

- [ ] **Step 4: Discoverable `login/start`.** Replace the body of `handleOwnerLoginStart` from the `BeginLogin` part. Keep the device-count 404 guard, swap to discoverable:

```go
func (s *Server) handleOwnerLoginStart(w http.ResponseWriter, r *http.Request) {
	oa := s.ownerAccess()
	wa, err := oa.webauthnLib(s.deps)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Still 404 when nothing is enrolled so the landing page shows the
	// "enroll on LAN first" panel.
	devices, err := s.deps.State.LoadTrustedDevices()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(devices) == 0 {
		http.Error(w, "no devices enrolled yet", http.StatusNotFound)
		return
	}
	// Usernameless: empty allowCredentials, resolve the user from the
	// assertion's userHandle at finish time.
	options, sessionData, err := wa.BeginDiscoverableLogin()
	if err != nil {
		http.Error(w, fmt.Sprintf("begin discoverable login: %v", err), http.StatusInternalServerError)
		return
	}
	tok, err := randomToken()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	oa.mu.Lock()
	oa.gcCeremonies()
	oa.loginSessions[tok] = ceremonySession{data: sessionData, createdAt: time.Now()}
	oa.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ceremony_token": tok,
		"options":        options,
	})
}
```

- [ ] **Step 5: Discoverable `login/finish` + resolver.** In `handleOwnerLoginFinish`, replace the `FinishLogin` block (which builds `user` then calls `wa.FinishLogin(user, *sess.data, r)`) with the discoverable form:

```go
	cred, err := wa.FinishDiscoverableLogin(s.resolveDiscoverableOwner, *sess.data, r)
	if err != nil {
		http.Error(w, fmt.Sprintf("finish login: %v", err), http.StatusUnauthorized)
		return
	}
```

Remove the now-unused `user, err := s.buildOwnerUser()` lines from `handleOwnerLoginFinish` (the resolver builds the user). Add the resolver near `buildOwnerUser`:

```go
// resolveDiscoverableOwner is the DiscoverableUserHandler for usernameless
// login: it returns the single owner iff the assertion's userHandle matches
// the stable wallet handle W. The library then matches the credential rawID
// against that owner's enrolled credentials and verifies the signature.
func (s *Server) resolveDiscoverableOwner(rawID, userHandle []byte) (webauthn.User, error) {
	user, err := s.buildOwnerUser()
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare(userHandle, user.WebAuthnID()) != 1 {
		return nil, errors.New("owner-access: unknown wallet handle")
	}
	return user, nil
}
```

> Note: `subtle` and `webauthn` are already imported in this file. The sign-count clone-guard persistence (`UpdateTrustedDeviceSignCount`) and `issueOwnerSession` that follow in `handleOwnerLoginFinish` stay unchanged — `cred` has the same shape.

- [ ] **Step 6: Run** — `cd go && go test ./internal/api/ -run 'TestResolveDiscoverableOwner|TestLoginStart|TestOwnerAccess|TestGate' -v` → PASS.

- [ ] **Step 7: Full api package** — `go test ./internal/api/` → `ok`.

- [ ] **Step 8: Commit**

```bash
git add go/internal/api/api_owner_access.go go/internal/api/api_owner_access_test.go
git commit --no-verify -m "feat(owner-access): usernameless discoverable login (resident keys, userHandle->wallet)"
```

---

### Task 2: Client — Conditional-UI autofill (login.html)

**Files:** `web/owner-access/login.html`. No Go test (browser); verified by build of the page + manual. Keep it minimal.

- [ ] **Step 1: Add the autofill field + conditional get.** In `login.html`, replace the `.panel` body and the inline `<script>`’s sign-in wiring so that:
  1. An `autocomplete="username webauthn"` input sits above the button.
  2. On load, if `PublicKeyCredential.isConditionalMediationAvailable?.()` resolves true, start the ceremony with `mediation:'conditional'`.
  3. The button still calls the same ceremony without conditional mediation (modal fallback).

Replace the panel markup:

```html
  <div class="panel">
    <input id="passkey-field" name="username" autocomplete="username webauthn"
           placeholder="Sign in with your passkey…" autofocus
           style="width:100%; font:inherit; padding:.6rem; border:1px solid var(--line,#ddd); border-radius:.3rem; margin-bottom:.8rem;" />
    <button id="signin" class="primary">Sign in with passkey</button>
    <button onclick="location.href='./'">Cancel</button>
    <div id="msg"></div>
  </div>
```

Replace the `signin` function and bootstrap with a shared `runLogin(mediation)`:

```javascript
    async function runLogin(mediation) {
      msg.innerHTML = "";
      try {
        const start = await fetch(base + "/api/owner-access/login/start", { method: "POST" });
        if (start.status === 404) {
          msg.innerHTML = '<div class="err">No passkeys enrolled. Enroll one on the LAN dashboard first.</div>';
          return;
        }
        if (!start.ok) {
          msg.innerHTML = `<div class="err">Start failed (${start.status}): ${await start.text()}</div>`;
          return;
        }
        const { ceremony_token, options } = await start.json();
        const credOpts = decodeAssertionOptions(options);
        const getOpts = { publicKey: credOpts };
        if (mediation) getOpts.mediation = mediation;
        const cred = await navigator.credentials.get(getOpts);
        if (!cred) return; // conditional aborted / no selection
        const finish = await fetch(
          base + "/api/owner-access/login/finish?ceremony_token=" + encodeURIComponent(ceremony_token),
          { method: "POST", headers: { "Content-Type": "application/json" },
            body: JSON.stringify(encodeAssertionResult(cred)), credentials: "same-origin" }
        );
        if (!finish.ok) {
          msg.innerHTML = `<div class="err">Finish failed (${finish.status}): ${await finish.text()}</div>`;
          return;
        }
        msg.innerHTML = '<div class="ok">Signed in. Redirecting…</div>';
        setTimeout(() => location.href = "./", 800);
      } catch (e) {
        if (e.name === "AbortError") return; // conditional UI cancelled — silent
        msg.innerHTML = `<div class="err">${e.name}: ${e.message}</div>`;
      }
    }

    document.getElementById("signin").onclick = () => runLogin(undefined);

    // Conditional UI: silently offer the passkey on the autofill field when
    // supported. The button remains the fallback for browsers without it.
    (async () => {
      try {
        if (await PublicKeyCredential.isConditionalMediationAvailable?.()) {
          runLogin("conditional");
        }
      } catch { /* feature-detect failure → button only */ }
    })();
```

(Delete the old `signin()` function, its `document.getElementById("signin").onclick = signin;`, and the `document.referrer`-based auto-trigger — `runLogin` replaces them.)

- [ ] **Step 2: Sanity-check the page parses.** Run a quick Node-free check that the file has balanced script tags and the new symbols:

```bash
grep -c "runLogin" web/owner-access/login.html   # expect 3+
grep -c "isConditionalMediationAvailable" web/owner-access/login.html  # expect 1
```

- [ ] **Step 3: Commit**

```bash
git add web/owner-access/login.html
git commit --no-verify -m "feat(owner-access): Conditional-UI passkey autofill on the login page (button fallback kept)"
```

---

### Task 3: Client — backup-passkey recovery nudge (index.html)

**Files:** `web/owner-access/index.html`. Minimal; no Go test.

- [ ] **Step 1: Add a nudge in the signed-in panel when only one device is enrolled.** In `index.html`, inside the `#signed-in` panel, after the `Open dashboard / Sign out` row, add:

```html
    <div id="backup-nudge" class="muted" style="display:none; margin-top:1rem; border-top:1px solid var(--line,#ddd); padding-top:1rem;">
      <strong>Add a backup passkey.</strong> You have one passkey. If you lose this device, a second passkey (another phone, a laptop, or a security key) keeps you in — otherwise your only way back is on your home WiFi.
      <div class="row"><button id="add-backup-btn">Add a backup passkey</button></div>
    </div>
```

- [ ] **Step 2: Wire it in `refresh()`** — in the `if (me) { … show("signed-in"); return; }` branch, before `show("signed-in")`:

```javascript
        const nudge = document.getElementById("backup-nudge");
        nudge.style.display = (me.devices_enrolled === 1) ? "block" : "none";
```

And register the button near the other handlers:

```javascript
    document.getElementById("add-backup-btn").onclick = () => location.href = "./enroll.html";
```

- [ ] **Step 3: Sanity check** — `grep -c "backup-nudge" web/owner-access/index.html` → expect 2.

- [ ] **Step 4: Commit**

```bash
git add web/owner-access/index.html
git commit --no-verify -m "feat(owner-access): nudge a backup passkey when only one is enrolled"
```

---

## Phase 3 verification

- [ ] `cd go && go test ./internal/api/` → `ok`.
- [ ] `cd go && go vet ./... && go build ./...` → clean.
- [ ] `cd go && go test ./test/e2e/ -run TestE2E_FullStack` → PASS.
- [ ] Manual (documented, not blocking): on a Conditional-UI browser the passkey chip appears on the field; the button works where it doesn't; `login/start` returns no `allowCredentials`.

## Self-review (authoring time)

- **Spec coverage:** discoverable + Conditional-UI + modal fallback → Tasks 1–2; resident keys → Task 1 (ResidentKey Required); recovery nudge → Task 3. One-time recovery *code* explicitly deferred (noted in scope).
- **Placeholder scan:** none.
- **Type consistency:** `resolveDiscoverableOwner(rawID, userHandle []byte) (webauthn.User, error)` matches `DiscoverableUserHandler`; `FinishDiscoverableLogin(handler, session, r)` matches the verified v0.17.4 signature; `runLogin(mediation)` used consistently in the client.
