// home-route-p2p.spec.ts — tier-2 end-to-end proof, all inside docker.
//
// Proves the P2P-ONLY home route (slices 4-6) with a headless Chromium:
//   1. enrolls a passkey via a CDP VIRTUAL WebAuthn authenticator (unattended),
//      with the WHOLE owner ceremony riding the DTLS DataChannel (P2P), not the
//      relay — the relay now REFUSES the owner API (403/405);
//   2. proves the channel reaches `direct` and the SELECTED ICE pair is
//      host/srflx (NOT a TURN relay pair);
//   3. makes an authenticated owner API call (/api/status) over the SAME
//      DataChannel the login ran on — the channel-scoped session;
//   4. independently logs in over a FRESH channel and repeats the authenticated
//      call, proving login-over-P2P stands on its own.
//
// Why single-channel: in the P2P-only route the owner session is captured
// per-channel by the Pi's Bridge from the login-finish Set-Cookie and never
// leaves DTLS (never readable by JS, never on the relay). So the authenticated
// owner call must ride the SAME channel the ceremony ran on. (Cross-page
// session continuity is a known v1 residual — see the design's deferred list.)
//
// The relay's home host (home.fortytwowatts.localhost) is mapped to the relay
// container by Chromium's --host-resolver-rules (see playwright.config.ts), so
// the page origin — and therefore clientDataJSON.origin and the WebAuthn RP-ID
// — stay the home host while the TCP bytes go to the relay. The Pi runs with
// FTW_OWNER_ACCESS_RPID=home.fortytwowatts.localhost so the origin check passes.
//
// The first enrollment over the relay needs the LAN PIN. The browser is remote
// (P2P marker stamped), so it can't mint it; the test process (same docker net)
// fetches it straight from the Pi's LAN port — a genuine private-range source,
// exactly the local-presence proof the PIN exists to verify.

import { test, expect, type Page, type CDPSession } from "@playwright/test";

const HOME_ORIGIN = process.env.HOME_ORIGIN ?? "http://home.fortytwowatts.localhost:7378";
// Direct LAN address of the Pi on the docker bridge net (NOT through the relay):
// used only to mint the first-enrollment PIN, which is LAN-only by design.
const PI_LAN_URL = process.env.PI_LAN_URL ?? "http://forty-two-watts:8080";

// ---- helpers --------------------------------------------------------------

// waitForHomeReady polls the home route until the Pi answers through the relay
// (the relay serves a 503 "home offline" page until the Pi has registered and
// the static-asset tunnel is live). Returns once /owner-access/ comes back 200.
async function waitForHomeReady(page: Page): Promise<void> {
  const deadline = Date.now() + 90_000;
  let lastStatus = 0;
  while (Date.now() < deadline) {
    const resp = await page.request.get(`${HOME_ORIGIN}/owner-access/`, {
      failOnStatusCode: false,
      timeout: 15_000,
    });
    lastStatus = resp.status();
    if (lastStatus === 200) return;
    await page.waitForTimeout(1000);
  }
  throw new Error(`home route never became ready (last status ${lastStatus})`);
}

// mintEnrollPin fetches the first-enrollment PIN from the Pi's LAN port. This is
// the local-presence path: the request originates from the Playwright container
// (a private-range bridge IP, not loopback, no tunnel marker), so the Pi mints
// and returns it. Over the relay this same endpoint is unreachable by design.
async function mintEnrollPin(page: Page): Promise<string> {
  const resp = await page.request.get(`${PI_LAN_URL}/api/owner-access/enroll-pin`, {
    failOnStatusCode: false,
    timeout: 15_000,
  });
  expect(resp.status(), "enroll-pin must be LAN-mintable from the bridge net").toBe(200);
  const body = await resp.json();
  expect(body.pin, "enroll-pin response carries a 6-digit pin").toMatch(/^\d{6}$/);
  return body.pin;
}

// addVirtualAuthenticator installs a CTAP2 platform authenticator that
// auto-approves user verification, so navigator.credentials.{create,get} run
// without any human prompt. Returns the CDP session (kept alive for the page).
async function addVirtualAuthenticator(page: Page): Promise<CDPSession> {
  const cdp = await page.context().newCDPSession(page);
  await cdp.send("WebAuthn.enable");
  await cdp.send("WebAuthn.addVirtualAuthenticator", {
    options: {
      protocol: "ctap2",
      transport: "internal",
      hasResidentKey: true,
      hasUserVerification: true,
      isUserVerified: true,
      automaticPresenceSimulation: true,
    },
  });
  return cdp;
}

// captureLastPC wraps the RTCPeerConnection constructor so the most-recent
// instance is stashed on window.__ftwLastPC for getStats() introspection. Must
// run BEFORE any page script (addInitScript), and is re-applied per navigation.
async function captureLastPC(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const Orig = window.RTCPeerConnection;
    if (!Orig) return;
    // @ts-expect-error — re-wrapping the constructor for test introspection.
    window.RTCPeerConnection = function (...args: unknown[]) {
      // @ts-expect-error — spread into the native constructor.
      const pc = new Orig(...args);
      (window as any).__ftwLastPC = pc;
      return pc;
    };
    (window as any).RTCPeerConnection.prototype = Orig.prototype;
  });
}

// waitForP2PDirect resolves once window.ftwP2P.state() === 'direct' — a DTLS
// DataChannel formed straight to the Pi (no relay fallback). p2p.js auto-connects
// on load and, on a failed attempt, backs off 30s before retrying; we GENTLY nudge
// a reconnect (setEnabled(true) clears that backoff) at a 6s cadence so a single
// early attempt that raced the Pi's signaling loop doesn't make the test wait out
// the full cooldown — while staying well clear of the relay's per-site offer rate
// limit and avoiding the reconnect churn that aggressive nudging would cause.
async function waitForP2PDirect(page: Page, timeoutMs = 45_000): Promise<void> {
  await page.waitForFunction(() => typeof (window as any).ftwP2P !== "undefined", null, { timeout: 15_000 });
  const deadline = Date.now() + timeoutMs;
  let lastNudge = 0;
  while (Date.now() < deadline) {
    const state = await page.evaluate(() => (window as any).ftwP2P?.state());
    if (state === "direct") return;
    if (Date.now() - lastNudge > 6000) {
      lastNudge = Date.now();
      await page.evaluate(() => { try { (window as any).ftwP2P?.setEnabled(true); } catch (e) { /* ignore */ } });
    }
    await page.waitForTimeout(500);
  }
  throw new Error("P2P channel never reached `direct`");
}

// assertSelectedPairDirect reads getStats() off the live RTCPeerConnection and
// asserts the SELECTED candidate pair is direct (host/srflx/prflx), never a TURN
// relay pair — the explicit "NOT relayed" proof.
async function assertSelectedPairDirect(page: Page): Promise<void> {
  const pair = await page.evaluate(async () => {
    const pc: RTCPeerConnection | undefined = (window as any).__ftwLastPC;
    if (!pc || typeof pc.getStats !== "function") return { found: false } as const;
    const stats = await pc.getStats();
    const byId = new Map<string, any>();
    stats.forEach((r: any) => byId.set(r.id, r));
    let sel: any = null;
    stats.forEach((r: any) => {
      if (r.type === "candidate-pair" && (r.nominated || r.selected) && r.state === "succeeded") {
        sel = r;
      }
    });
    if (!sel) stats.forEach((r: any) => { if (r.type === "candidate-pair" && r.state === "succeeded") sel = r; });
    if (!sel) return { found: false } as const;
    const local = byId.get(sel.localCandidateId);
    const remote = byId.get(sel.remoteCandidateId);
    return { found: true, localType: local?.candidateType ?? null, remoteType: remote?.candidateType ?? null };
  });
  expect(pair.found, "a succeeded ICE candidate pair must exist").toBe(true);
  expect(["host", "srflx", "prflx"], "local candidate must be direct, not relay").toContain(pair.localType);
  expect(["host", "srflx", "prflx"], "remote candidate must be direct, not relay").toContain(pair.remoteType);
}

// statusOverP2P calls the owner-gated /api/status over the DataChannel and
// returns its status. A 200 proves the channel carried the captured owner
// session through the Pi's Bridge.
async function statusOverP2P(page: Page): Promise<{ ok: boolean; status: number }> {
  return page.evaluate(async () => {
    const r = await (window as any).p2pFetch("/api/status", { method: "GET" });
    return { ok: r.ok, status: r.status };
  });
}

// strictStatusWithChannelDown disables P2P (tears the channel down + blocks
// reconnect via the backoff) and then issues a STRICT owner fetch. On the public
// home host (relay in the path, NOT a LAN origin) it must FAIL CLOSED with the
// synthetic 503 — proving the owner body never silently fell back to the relay
// (FIX-2). It returns the status the strict call produced.
async function strictStatusWithChannelDown(page: Page): Promise<number> {
  return page.evaluate(async () => {
    // Tear the channel down and force the relay path off so no channel is up.
    (window as any).ftwP2P.setEnabled(false);
    // isLanOrigin must be false on the home host, so strict cannot fall back.
    if ((window as any).ftwP2P.isLanOrigin()) {
      throw new Error("home host wrongly classified as LAN — strict would leak to relay");
    }
    const r = await (window as any).p2pFetchStrict("/api/status", { method: "GET" });
    return r.status;
  });
}

// controlPostOverP2P makes a state-changing CONTROL write (POST /api/mode) over
// the live DataChannel. A 200 proves a control write rides the same authenticated
// channel as the reads — the dashboard's setMode/postJson path (FIX-B).
async function controlPostOverP2P(page: Page, mode: string): Promise<{ ok: boolean; status: number }> {
  return page.evaluate(async (m) => {
    const r = await (window as any).p2pFetchStrict("/api/mode", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ mode: m }),
    });
    return { ok: r.ok, status: r.status };
  }, mode);
}

// strictControlPostWithChannelDown is the CONTROL-write counterpart of the FIX-2
// status guard, for FIX-B: a state-changing owner POST (/api/mode) issued strict
// with the channel down on the public home host must FAIL CLOSED (synthetic 503),
// so the control body + owner session never fall back to the cleartext relay.
async function strictControlPostWithChannelDown(page: Page): Promise<number> {
  return page.evaluate(async () => {
    (window as any).ftwP2P.setEnabled(false);
    if ((window as any).ftwP2P.isLanOrigin()) {
      throw new Error("home host wrongly classified as LAN — strict control POST would leak to relay");
    }
    const r = await (window as any).p2pFetchStrict("/api/mode", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ mode: "self_consumption" }),
    });
    return r.status;
  });
}

// ---- the test -------------------------------------------------------------

test("home route: P2P-only passkey ceremony + authenticated owner API over the DataChannel", async ({ page }) => {
  const cdp = await addVirtualAuthenticator(page);
  await captureLastPC(page);

  // 0. Wait for the relay→Pi static tunnel to be live (else the relay serves the
  //    offline page and the owner-access flow can't start).
  await waitForHomeReady(page);

  // The owner API must NEVER be served over the relay (P2P-only). Prove it: a
  // direct request to an owner endpoint via the relay home host is refused
  // (403 = owner API is P2P-only). This is the "no cleartext owner path" guard.
  const relayOwner = await page.request.get(`${HOME_ORIGIN}/api/status`, { failOnStatusCode: false });
  expect(relayOwner.status(), "owner API over the relay must be refused (P2P-only)").toBe(403);

  // 1. Mint the first-enrollment PIN from the Pi's LAN port.
  const pin = await mintEnrollPin(page);

  // 2. ENROLL a passkey — over the P2P DataChannel. Load enroll.html, wait for
  //    the channel to reach `direct`, THEN run the ceremony so enroll/start +
  //    enroll/finish ride P2P (the relay refuses them). The virtual authenticator
  //    answers create() unattended. enroll/finish mints the session, captured by
  //    the Pi's Bridge on THIS channel.
  await page.goto(`${HOME_ORIGIN}/owner-access/enroll.html`);
  await waitForP2PDirect(page);
  await page.locator("#name").fill("tier2-virtual-authenticator");
  await page.locator("#pin").fill(pin);
  await page.locator("#enroll").click();
  await expect(page.locator("#msg .ok"), "enrollment over P2P should report success").toBeVisible({
    timeout: 30_000,
  });

  // 3. Prove the channel is a DIRECT (non-relayed) DTLS DataChannel.
  await assertSelectedPairDirect(page);

  // 4. Make an authenticated owner API call OVER the SAME DataChannel the enroll
  //    ceremony ran on. /api/status is owner-gated (not an open path), so a 200
  //    proves the Bridge captured the enroll session and stamps it on the channel.
  //    enroll.html redirects to ./ ~1.5s after success; we make the call promptly
  //    on the SAME (still-open) channel before that navigation.
  const enrollStatus = await statusOverP2P(page);
  expect(enrollStatus.status, "/api/status over the enroll channel should be 200").toBe(200);
  expect(enrollStatus.ok).toBe(true);

  // 5. INDEPENDENTLY prove login-over-P2P on a FRESH channel: navigate to the
  //    login page (new RTCPeerConnection), wait for `direct`, sign in over P2P,
  //    then make the authenticated owner call over that channel. (A fresh page =
  //    a fresh channel with no session, so this proves login-over-P2P stands on
  //    its own — not riding the enroll channel's captured session.)
  await page.goto(`${HOME_ORIGIN}/owner-access/login.html`);
  await waitForP2PDirect(page);
  await page.locator("#signin").click();
  await expect(page.locator("#msg .ok"), "login over P2P should report success").toBeVisible({
    timeout: 30_000,
  });
  // login.html redirects to / ~0.8s after success; make the authenticated call
  // promptly on the same login channel.
  const loginStatus = await statusOverP2P(page);
  expect(loginStatus.status, "/api/status over the login channel should be 200").toBe(200);
  expect(loginStatus.ok).toBe(true);

  // 5b. CONTROL WRITE over P2P (FIX-B): a state-changing owner POST (/api/mode)
  //     rides the SAME authenticated channel as the reads — proving the dashboard's
  //     setMode/postJson strict path reaches the Pi over DTLS, not the relay.
  //     "self_consumption" is a canonical mode (control.ModeSelfConsumption).
  const controlPost = await controlPostOverP2P(page, "self_consumption");
  expect(controlPost.status, "POST /api/mode over the login channel should be 200").toBe(200);
  expect(controlPost.ok).toBe(true);

  // 6. STRICT FAIL-CLOSED (FIX-2 + FIX-B): with the channel torn down on the public
  //    home host, BOTH a strict owner READ and a strict CONTROL WRITE must FAIL
  //    CLOSED with the synthetic 503 — never silently falling back to the cleartext
  //    relay. (A relay fallback would surface the relay's 403 P2P-only refusal or
  //    leak the body; 503 is the in-browser fail-closed marker proving nothing left
  //    the page.)
  const strictStatus = await strictStatusWithChannelDown(page);
  expect(strictStatus,
    "strict owner READ with the channel down must fail closed (503), never relay-fallback")
    .toBe(503);
  // Re-tear (the read above already disabled P2P, but be explicit) and prove the
  // control WRITE fails closed identically.
  const strictControl = await strictControlPostWithChannelDown(page);
  expect(strictControl,
    "strict CONTROL write with the channel down must fail closed (503), never relay-fallback")
    .toBe(503);

  await cdp.detach().catch(() => { /* best-effort */ });
});
