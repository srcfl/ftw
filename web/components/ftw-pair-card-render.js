// ftw-pair-card-render.js — pure render helpers for <ftw-pair-card>.
//
// Extracted out of the custom-element so they can be unit-tested in
// Node with `node --test` (no DOM, no Web Components polyfill). The
// element imports these and calls them inside its template literal;
// the tests import them and assert on the returned strings + objects.
//
// Every helper is a pure function of (state, now) where:
//   state  — the latest /api/pair/status JSON (see PairStatus in
//            go/internal/api/api_pair.go)
//   now    — Date.now()-style ms; injectable so tests aren't flaky
//            on system-clock drift.

export const POLL_MS = 5000;
export const FAST_POLL_MS = 1000;
export const FAST_POLL_ROUNDS = 3;

export function escapeHTML(s) {
  return String(s == null ? "" : s)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

// computeRemaining returns the human-readable "h m" time left in
// the session, or "—" if missing data.
export function computeRemaining(state, now = Date.now()) {
  if (!state || !state.started_at || !state.ttl_s) return "—";
  const startedMs = Date.parse(state.started_at);
  if (Number.isNaN(startedMs)) return "—";
  const expiry = startedMs + state.ttl_s * 1000;
  const left = Math.max(0, Math.floor((expiry - now) / 1000));
  const h = Math.floor(left / 3600);
  const m = Math.floor((left % 3600) / 60);
  return `${h}h ${m}m`;
}

// derivePresence converts session_state + last_activity_ms into a UI
// state machine the dashboard can render directly:
//   "no-relay"  — running with -no-relay, nothing to show
//   "pending"   — friend hasn't approved yet
//   "fresh"     — last activity < 30s ago (orange dot, "live")
//   "recent"    — 30s..5min ago (yellow dot, "active")
//   "idle"      — > 5min ago OR never (grey dot, "idle")
//   "expired"   — relay says expired or revoked
//
// Includes a friendly "12s ago" label.
export function derivePresence(state, now = Date.now()) {
  if (!state) return { label: "no session", class: "idle" };
  if (!state.session_state) {
    // -no-relay mode — no presence data available.
    return { label: "local-only", class: "idle" };
  }
  switch (state.session_state) {
    case "pending":
      return {
        label: state.pending_approvals_count > 0
          ? "friend opened URL — call you with the code"
          : "waiting for friend to open the URL",
        class: "pending",
      };
    case "expired":
      return { label: "session expired", class: "dead" };
    case "revoked":
      return { label: "session revoked", class: "dead" };
    case "active":
      const last = state.last_activity_ms || 0;
      if (last === 0) {
        return { label: "active — no traffic yet", class: "idle" };
      }
      const ageMs = now - last;
      if (ageMs < 30_000) {
        return { label: `live · last activity ${formatAge(ageMs)}`, class: "fresh" };
      }
      if (ageMs < 5 * 60_000) {
        return { label: `active · last activity ${formatAge(ageMs)}`, class: "recent" };
      }
      return { label: `idle · last activity ${formatAge(ageMs)}`, class: "idle" };
    default:
      return { label: state.session_state, class: "idle" };
  }
}

export function formatAge(ms) {
  if (ms < 1000) return "just now";
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s ago`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  return `${h}h ago`;
}

// friendMessage is what the operator copies and sends to the friend.
// The URL flow is the v2 friend onboarding — no install required.
// When state has no pair_url (e.g. -no-relay mode) it falls back to
// the local MCP addr stored in `code` so the same template works for
// local-only testing.
export function friendMessage(state) {
  if (!state) return "";
  const url = state.pair_url || ("(local-only: " + state.code + ")");
  const code = state.approval_code || "—";
  return `I need help with my home energy system. I started a pair
session — open this in any browser:

  ${url}

You'll see a 4-digit code on the page. Call/Signal me and read
that code aloud. Once I confirm it matches the code I see here,
the session activates and you can:

  - Open the dashboard:   ${url}/web/
  - Add as Claude Code MCP:
      claude mcp add ftw-friend --transport http ${url}/mcp

Session expires when I close it or TTL runs out. The code I'll be
waiting to hear from you matches what shows up on your screen — if
it doesn't match, do NOT confirm (could be a leaked URL).`;
}

// canApprove returns true when the dashboard should expose the Allow
// form (operator types the friend's 4-digit code → POST /approve).
export function canApprove(state) {
  return Boolean(
    state &&
    state.session_state === "pending" &&
    state.approval_code &&
    state.pair_url
  );
}

// approveRequest constructs the request the Allow button fires.
// Returns { url, body } so the test can assert without a real fetch.
export function approveRequest(state, typedCode) {
  if (!state || !state.pair_url) return null;
  return {
    url: state.pair_url + "/approve",
    body: { code: typedCode },
  };
}

// validateTypedCode: did the operator type the same 4-digit code we
// generated? Comparing strictly (not just suffix) because a stale
// session could leave a different code in scrollback.
export function validateTypedCode(state, typedCode) {
  if (!state || !state.approval_code) return { ok: false, reason: "no active session" };
  if (typeof typedCode !== "string" || !/^\d{4}$/.test(typedCode.trim())) {
    return { ok: false, reason: "expected 4 digits" };
  }
  if (typedCode.trim() !== state.approval_code) {
    return { ok: false, reason: "code mismatch — do NOT approve, this could be a leaked-URL attack" };
  }
  return { ok: true };
}
