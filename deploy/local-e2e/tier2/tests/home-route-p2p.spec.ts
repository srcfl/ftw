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
// DataChannel formed straight to the Pi (no relay fallback).
async function waitForP2PDirect(page: Page, timeoutMs = 30_000): Promise<void> {
  await page.waitForFunction(
    () => (window as any).ftwP2P && (window as any).ftwP2P.state() === "direct",
    null,
    { timeout: timeoutMs, polling: 250 },
  );
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
  await page.waitForFunction(() => typeof (window as any).ftwP2P !== "undefined", null, { timeout: 15_000 });
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
  await page.waitForFunction(() => typeof (window as any).ftwP2P !== "undefined", null, { timeout: 15_000 });
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

  await cdp.detach().catch(() => { /* best-effort */ });
});
