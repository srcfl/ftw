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
// Both the URL AND the code travel together — the friend opens the URL,
// types the 4-digit code, and the landing page then hands them the exact
// `claude mcp add …` command with a one-time access token baked in.
//
// The MCP command is deliberately NOT pre-baked here: the access token
// (grant) is minted by the relay only when the code is accepted, so it
// cannot exist before approval. Pre-baking a token-less command would
// just hand the friend something that gets rejected with 401. See the
// grant-exchange model in
// docs/archive/agent-artifacts/goals/relay-subdomain-sessions.md.
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
  return `I need help with my home energy system. Open this link and enter the 4-digit code to start the session:

  URL:  ${url}
  Code: ${code}

After you enter the code, the page shows a "claude mcp add …" command with a
one-time access token baked in — copy that into Claude Code to connect. The
token only appears once you've entered the code, so I can't include it here.

The session expires when I close it or the timer runs out. Don't forward the
URL + code to anyone else — together they start the session.`;
}
