// owner-fetch.js — the single owner/control fetch entry point for the
// owner-access ceremony pages (login.html, enroll.html). FIX-B.
//
// Owner/control API calls (WebAuthn assertion/attestation, ceremony token, PIN,
// the minted owner session) must NEVER traverse the untrusted relay in cleartext.
// They ride the DTLS DataChannel via window.p2pFetchStrict, which fails closed
// (synthetic 503) on a public origin when the channel is down.
//
// The gap this closes: if p2p.js failed to LOAD at all (a network error fetching
// the script), window.p2pFetchStrict is undefined and the page used to fall back
// to a raw fetch() — which on the public home host sends the owner body to the
// relay. ownerFetch here refuses that: with no transport, a PUBLIC origin fails
// closed; only a genuine-LAN origin (the Pi serves this page directly, the relay
// is not in the path) may raw-fetch.

import { apiBase } from "./webauthn.js";

// isPrivateIPv4 mirrors the same check in p2p.js (kept dependency-free here so it
// works even when p2p.js never loaded).
function isPrivateIPv4(h) {
  const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(h);
  if (!m) return false;
  const a = +m[1], b = +m[2];
  if (a === 10) return true;                        // 10.0.0.0/8
  if (a === 127) return true;                       // loopback
  if (a === 192 && b === 168) return true;          // 192.168.0.0/16
  if (a === 172 && b >= 16 && b <= 31) return true; // 172.16.0.0/12
  if (a === 169 && b === 254) return true;          // link-local
  return false;
}

// isLanOrigin: the page is served DIRECTLY by the Pi (genuine LAN), so "the relay"
// is not in the path and a raw fetch is safe. Mirrors p2p.js's isLanOrigin so the
// no-transport fallback makes the SAME LAN/not-LAN decision. When in doubt → not
// LAN → fail closed. Prefer p2p.js's own isLanOrigin when it is available (single
// source of truth); fall back to this copy only when p2p.js never loaded.
export function isLanOrigin() {
  if (window.ftwP2P && typeof window.ftwP2P.isLanOrigin === "function") {
    try { return window.ftwP2P.isLanOrigin(); } catch (e) { /* fall through */ }
  }
  // A relay tunnel prefix (/me/<site>/) is, by definition, the relay in the path.
  if (apiBase() !== "") return false;
  const h = (location.hostname || "").toLowerCase();
  if (h === "localhost" || h === "::1" || h === "[::1]") return true;
  // A bare single-label host (e.g. "fortytwowatts", "raspberrypi") or *.local is a
  // direct-LAN name — never the public home host (which has dots).
  if (h.slice(-6) === ".local" || h.indexOf(".") === -1) return true;
  if (isPrivateIPv4(h)) return true;
  const hv6 = h.replace(/^\[|\]$/g, "");
  if (/^f[cd][0-9a-f]{2}:/.test(hv6)) return true; // fc00::/7 ULA
  if (/^fe[89ab][0-9a-f]:/.test(hv6)) return true; // fe80::/10 link-local
  // Anything dotted-public (home.fortytwowatts.com, etc.) → NOT LAN → fail closed.
  return false;
}

// failClosedResponse synthesises a 503-like response so callers handle the
// no-transport case uniformly (same shape p2p.js uses), WITHOUT the owner body
// ever leaving the browser.
function failClosedResponse(path) {
  const msg = "Secure channel unavailable — reconnecting. The login/enroll request was NOT sent to the relay. Retry in a moment.";
  return {
    ok: false,
    status: 503,
    url: path,
    headers: new Headers(),
    json: () => Promise.resolve({ error: msg, retry: true }),
    text: () => Promise.resolve(msg),
  };
}

// ownerFetch is the ONLY way the ceremony pages should make owner/control API
// calls. It routes over the strict P2P transport when present, fails closed on a
// public origin when no transport loaded, and raw-fetches only on a genuine LAN.
export function ownerFetch(path, opts) {
  opts = opts || {};
  if (typeof window.p2pFetchStrict === "function") {
    return window.p2pFetchStrict(path, opts);
  }
  // No transport (p2p.js failed to load). Fail closed unless this is a genuine LAN
  // origin where the Pi serves the page directly (relay not in the path).
  if (!isLanOrigin()) {
    return Promise.resolve(failClosedResponse(path));
  }
  return fetch(apiBase() + path, opts);
}
