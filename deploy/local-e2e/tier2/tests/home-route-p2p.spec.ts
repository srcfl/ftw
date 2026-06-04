// home-route-p2p.spec.ts — tier-2 end-to-end proof, all inside docker.
//
// Drives the REAL home route through the relay with a headless Chromium that
//   1. enrolls a passkey via a CDP VIRTUAL WebAuthn authenticator (unattended),
//   2. logs in with that passkey,
//   3. brings up the REAL P2P WebRTC DataChannel and asserts it reaches
//      `direct` (container-to-container ICE: host candidates are directly
//      routable on the docker bridge, so no NAT/STUN/TURN is involved), and
//   4. makes one authenticated owner API call over that DataChannel.
//
// The relay's home host (home.fortytwowatts.localhost) is mapped to the relay
// container by Chromium's --host-resolver-rules (see playwright.config.ts), so
// the page origin — and therefore clientDataJSON.origin and the WebAuthn RP-ID
// — stay the home host while the TCP bytes go to the relay. The Pi runs with
// FTW_OWNER_ACCESS_RPID=home.fortytwowatts.localhost so the origin check passes.
//
// The first enrollment over the relay tunnel needs the LAN PIN. The browser is
// tunnelled, so it can't mint it; the test process (running in the same docker
// net) fetches it straight from the Pi's LAN port — a genuine private-range
// source, exactly the local-presence proof the PIN exists to verify.

import { test, expect, type Page, type CDPSession } from "@playwright/test";

const HOME_ORIGIN = process.env.HOME_ORIGIN ?? "http://home.fortytwowatts.localhost:7378";
// Direct LAN address of the Pi on the docker bridge net (NOT through the relay):
// used only to mint the first-enrollment PIN, which is LAN-only by design.
const PI_LAN_URL = process.env.PI_LAN_URL ?? "http://forty-two-watts:8080";

// ---- helpers --------------------------------------------------------------

// waitForHomeReady polls the home route until the Pi answers through the relay
// (the relay serves a 503 "home offline" page until the Pi has registered and
// the tunnel is live). Returns once /owner-access/ comes back 200.
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
// and returns it. Over the relay this same endpoint is 403 by design.
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

// pollP2PState resolves once window.ftwP2P.state() equals `want` (or rejects on
// a terminal mismatch after the timeout). `direct` is the success signal: a
// DTLS DataChannel formed straight to the Pi.
async function waitForP2PState(page: Page, want: string, timeoutMs = 30_000): Promise<void> {
  await page.waitForFunction(
    (w) => (window as any).ftwP2P && (window as any).ftwP2P.state() === w,
    want,
    { timeout: timeoutMs, polling: 250 },
  );
}

// ---- the test -------------------------------------------------------------

test("home route: virtual passkey login + direct container-to-container P2P", async ({ page }) => {
  const cdp = await addVirtualAuthenticator(page);

  // Capture p2p.js's live RTCPeerConnection so we can read getStats() and prove
  // the SELECTED candidate pair is host/srflx — NOT a relay (TURN) pair. p2p.js
  // keeps `pc` in a closure, so wrap the constructor before any page script runs
  // and stash the most-recent instance on window.__ftwLastPC.
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

  // 0. Wait for the relay→Pi tunnel to be live (else the relay serves the
  //    offline page and the owner-access flow can't start).
  await waitForHomeReady(page);

  // 1. Mint the first-enrollment PIN from the Pi's LAN port.
  const pin = await mintEnrollPin(page);

  // 2. ENROLL a passkey over the relay tunnel. The page origin is the home host
  //    (RP-ID match); the virtual authenticator answers create() unattended.
  await page.goto(`${HOME_ORIGIN}/owner-access/enroll.html`);
  await page.locator("#name").fill("tier2-virtual-authenticator");
  await page.locator("#pin").fill(pin);
  await page.locator("#enroll").click();
  await expect(page.locator("#msg .ok"), "enrollment should report success").toBeVisible({
    timeout: 30_000,
  });

  // The enroll flow drops the session cookie and redirects to ./ — prove we land
  // signed-in, then SIGN OUT so the subsequent login ceremony is exercised for
  // real (not just riding the enroll cookie).
  await page.goto(`${HOME_ORIGIN}/owner-access/`);
  await expect(page.locator("#signed-in"), "enrolled → signed-in panel").toBeVisible({
    timeout: 15_000,
  });
  await page.locator("#signout").click();
  await expect(page.locator("#choose"), "after sign-out → choose panel").toBeVisible({
    timeout: 15_000,
  });

  // 3. LOG IN with the enrolled passkey (discoverable / usernameless). The
  //    virtual authenticator answers get() unattended.
  await page.goto(`${HOME_ORIGIN}/owner-access/login.html`);
  await page.locator("#signin").click();
  await expect(page.locator("#msg .ok"), "login should report success").toBeVisible({
    timeout: 30_000,
  });

  // Confirm the owner session is live via whoami (authenticated, over the tunnel).
  const who = await page.request.get(`${HOME_ORIGIN}/api/owner-access/whoami`);
  expect(who.status(), "whoami should be authenticated after login").toBe(200);
  expect((await who.json()).authenticated).toBe(true);

  // 4. Open the dashboard. p2p.js auto-connects on load; assert it reaches
  //    `direct` — the real container-to-container DTLS DataChannel.
  await page.goto(`${HOME_ORIGIN}/`);
  await page.waitForFunction(() => typeof (window as any).ftwP2P !== "undefined", null, {
    timeout: 15_000,
  });
  await waitForP2PState(page, "direct", 30_000);

  // Belt-and-braces: read getStats() off the live RTCPeerConnection (captured by
  // the init-script constructor wrap) and assert the SELECTED candidate pair is
  // direct — local and remote candidates are `host` or `srflx`, never `relay`
  // (TURN). On the docker bridge both peers expose routable host candidates, so
  // the nominated pair is host↔host. This is the explicit "NOT relayed" proof.
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
    // Fallback: any succeeded pair (some impls don't flag `nominated` on a DC).
    if (!sel) stats.forEach((r: any) => { if (r.type === "candidate-pair" && r.state === "succeeded") sel = r; });
    if (!sel) return { found: false } as const;
    const local = byId.get(sel.localCandidateId);
    const remote = byId.get(sel.remoteCandidateId);
    return {
      found: true,
      localType: local?.candidateType ?? null,
      remoteType: remote?.candidateType ?? null,
    };
  });
  expect(pair.found, "a succeeded ICE candidate pair must exist").toBe(true);
  expect(["host", "srflx", "prflx"], "local candidate must be direct, not relay").toContain(pair.localType);
  expect(["host", "srflx", "prflx"], "remote candidate must be direct, not relay").toContain(pair.remoteType);

  // 5. Make ONE authenticated owner API call OVER the DataChannel and assert it
  //    succeeds. /api/status is owner-gated (not an open path), so a 200 proves
  //    the replayed request carried the owner session through the P2P bridge.
  const status = await page.evaluate(async () => {
    const r = await (window as any).p2pFetch("/api/status", { method: "GET" });
    let body: unknown = null;
    try { body = await r.json(); } catch { /* tolerate non-JSON */ }
    return { ok: r.ok, status: r.status, body };
  });
  expect(status.status, "/api/status over p2pFetch should be 200").toBe(200);
  expect(status.ok, "/api/status response.ok over p2pFetch").toBe(true);

  await cdp.detach().catch(() => { /* best-effort */ });
});
