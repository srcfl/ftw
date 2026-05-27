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
          ? "friend opened URL — waiting for them to enter the code"
          : "waiting for friend to open URL + enter code",
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
// Both the URL AND the code are in the same message — the friend
// opens the URL, types the code, done. The 4-digit code is what
// prevents a leaked URL alone from activating the session (the
// attacker also needs the code).
//
// When state has no pair_url (e.g. -no-relay mode) it falls back to
// the local MCP addr stored in `code` so the same template works for
// local-only testing.
export function friendMessage(state) {
  if (!state) return "";
  if (!state.pair_url) {
    return `Local-only pair session running at ${state.code}. No relay URL — share this only with someone on the same network.`;
  }
  const url = state.pair_url;
  const code = state.approval_code || "(no code — bug?)";
  return `I need help with my home energy system. Open this URL and enter the 4-digit code below to activate the session:

  URL:  ${url}
  Code: ${code}

Once active you can:

  - Open the dashboard:   ${url}/web/
  - Add as Claude Code MCP:
      claude mcp add ftw-friend --transport http ${url}/mcp

Session expires when I close it or TTL runs out. Don't share the
URL + code with anyone else — together they're the access grant.`;
}
