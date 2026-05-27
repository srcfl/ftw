// node --test web/components/ftw-pair-card-render.test.mjs
//
// Pure-function tests for the render helpers in
// ftw-pair-card-render.js. No DOM, no Web Components, no browser —
// runs under stock Node 22+. The tests also act as golden-snapshot
// guards: the `friendMessage` string and the `derivePresence` state
// machine are operator-facing surfaces that the rest of the dashboard
// presents verbatim, so anything that changes them should be a
// conscious decision.

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  computeRemaining,
  derivePresence,
  formatAge,
  friendMessage,
  canApprove,
  approveRequest,
  validateTypedCode,
} from "./ftw-pair-card-render.js";

const stateActive = {
  code: "alpha-amber-arrow-atom-axis-bay",
  pair_url: "https://relay.fortytwowatts.com/h/alpha-amber-arrow-atom-axis-bay",
  approval_code: "4827",
  session_state: "active",
  intent: "help me write a sungrow driver",
  started_at: "2026-05-27T10:00:00Z",
  ttl_s: 4 * 3600,
  last_activity_ms: Date.parse("2026-05-27T10:30:15Z"),
};

const stateNoRelay = {
  code: "local:127.0.0.1:9999",
  intent: "local test",
  started_at: "2026-05-27T10:00:00Z",
  ttl_s: 600,
};

describe("computeRemaining", () => {
  const now = Date.parse("2026-05-27T11:30:00Z");

  it("counts down hours and minutes", () => {
    assert.equal(computeRemaining(stateActive, now), "2h 30m");
  });

  it("returns 0h 0m at expiry", () => {
    const exp = Date.parse(stateActive.started_at) + stateActive.ttl_s * 1000;
    assert.equal(computeRemaining(stateActive, exp), "0h 0m");
  });

  it("never goes negative past expiry", () => {
    const exp = Date.parse(stateActive.started_at) + stateActive.ttl_s * 1000;
    assert.equal(computeRemaining(stateActive, exp + 10_000), "0h 0m");
  });

  it("returns em-dash when state is missing fields", () => {
    assert.equal(computeRemaining({}), "—");
    assert.equal(computeRemaining(null), "—");
    assert.equal(computeRemaining({ ttl_s: 60, started_at: "garbage" }), "—");
  });
});

describe("formatAge", () => {
  it("returns 'just now' for sub-second ages", () => {
    assert.equal(formatAge(500), "just now");
  });

  it("uses seconds under 1m", () => {
    assert.equal(formatAge(12_000), "12s ago");
  });

  it("uses minutes under 1h", () => {
    assert.equal(formatAge(180_000), "3m ago");
  });

  it("uses hours after 1h", () => {
    assert.equal(formatAge(3 * 3600 * 1000), "3h ago");
  });
});

describe("derivePresence (state machine across session lifecycle)", () => {
  const now = Date.parse("2026-05-27T10:30:15Z");

  // The state machine is the contract the dashboard renders. Each
  // entry locks in a specific (input → output) pair as a golden
  // snapshot — changes here should be reviewed.
  const cases = [
    {
      name: "no session at all",
      state: null,
      want: { label: "no session", class: "idle" },
    },
    {
      name: "-no-relay (no session_state in payload)",
      state: { code: "local:..." },
      want: { label: "local-only", class: "idle" },
    },
    {
      name: "pending, friend hasn't opened URL yet",
      state: { session_state: "pending", pending_approvals_count: 0 },
      want: { label: "waiting for friend to open the URL", class: "pending" },
    },
    {
      name: "pending, friend opened URL (operator should approve)",
      state: { session_state: "pending", pending_approvals_count: 1 },
      want: {
        label: "friend opened URL — call you with the code",
        class: "pending",
      },
    },
    {
      name: "expired token",
      state: { session_state: "expired" },
      want: { label: "session expired", class: "dead" },
    },
    {
      name: "revoked token",
      state: { session_state: "revoked" },
      want: { label: "session revoked", class: "dead" },
    },
    {
      name: "active, no traffic yet",
      state: { session_state: "active", last_activity_ms: 0 },
      want: { label: "active — no traffic yet", class: "idle" },
    },
    {
      name: "active, friend just hit it (5s ago) → fresh",
      state: { session_state: "active", last_activity_ms: now - 5_000 },
      want: { label: "live · last activity 5s ago", class: "fresh" },
    },
    {
      name: "active, friend last hit 2 min ago → recent",
      state: { session_state: "active", last_activity_ms: now - 2 * 60_000 },
      want: { label: "active · last activity 2m ago", class: "recent" },
    },
    {
      name: "active, friend last hit 10 min ago → idle",
      state: { session_state: "active", last_activity_ms: now - 10 * 60_000 },
      want: { label: "idle · last activity 10m ago", class: "idle" },
    },
  ];

  for (const c of cases) {
    it(c.name, () => {
      assert.deepEqual(derivePresence(c.state, now), c.want);
    });
  }
});

describe("friendMessage (golden snapshot)", () => {
  it("renders the URL flow when pair_url is set", () => {
    const msg = friendMessage(stateActive);
    assert.match(msg, /https:\/\/relay\.fortytwowatts\.com\/h\/alpha-amber/);
    assert.match(msg, /4-digit code/);
    assert.match(msg, /Open the dashboard:/);
    assert.match(msg, /claude mcp add ftw-friend/);
    assert.match(msg, /leaked URL/, "must warn that code-mismatch = leaked URL");
    assert.doesNotMatch(msg, /ftw-connect/, "v2 must not mention the deprecated binary");
    assert.doesNotMatch(msg, /install-ftw-connect/);
    assert.doesNotMatch(msg, /go install/);
  });

  it("falls back to local-only message when no pair_url", () => {
    const msg = friendMessage(stateNoRelay);
    assert.match(msg, /local-only/);
    assert.match(msg, /127\.0\.0\.1:9999/);
  });

  it("returns empty string for null state", () => {
    assert.equal(friendMessage(null), "");
  });
});

describe("canApprove gating", () => {
  it("allows when pending + has url + has code", () => {
    assert.equal(canApprove({
      session_state: "pending",
      approval_code: "1234",
      pair_url: "https://x/h/y",
    }), true);
  });

  it("rejects when not pending", () => {
    assert.equal(canApprove({
      session_state: "active",
      approval_code: "1234",
      pair_url: "https://x/h/y",
    }), false);
  });

  it("rejects in no-relay mode (missing pair_url)", () => {
    assert.equal(canApprove({
      session_state: "pending",
      approval_code: "1234",
    }), false);
  });

  it("rejects null state", () => {
    assert.equal(canApprove(null), false);
    assert.equal(canApprove(undefined), false);
  });
});

describe("approveRequest construction", () => {
  it("targets /h/<token>/approve on the relay", () => {
    const req = approveRequest(stateActive, "4827");
    assert.equal(
      req.url,
      "https://relay.fortytwowatts.com/h/alpha-amber-arrow-atom-axis-bay/approve",
    );
    assert.deepEqual(req.body, { code: "4827" });
  });

  it("returns null without pair_url", () => {
    assert.equal(approveRequest(stateNoRelay, "1234"), null);
  });
});

describe("validateTypedCode (voice-channel cross-check)", () => {
  const state = { approval_code: "4827", pair_url: "x" };

  it("accepts exact match", () => {
    assert.deepEqual(validateTypedCode(state, "4827"), { ok: true });
  });

  it("accepts with surrounding whitespace", () => {
    assert.deepEqual(validateTypedCode(state, "  4827  "), { ok: true });
  });

  it("rejects 3 digits", () => {
    const r = validateTypedCode(state, "482");
    assert.equal(r.ok, false);
    assert.match(r.reason, /expected 4 digits/);
  });

  it("rejects 5 digits", () => {
    const r = validateTypedCode(state, "48275");
    assert.equal(r.ok, false);
    assert.match(r.reason, /expected 4 digits/);
  });

  it("rejects non-numeric", () => {
    const r = validateTypedCode(state, "abcd");
    assert.equal(r.ok, false);
    assert.match(r.reason, /expected 4 digits/);
  });

  it("rejects mismatch with security warning", () => {
    const r = validateTypedCode(state, "9999");
    assert.equal(r.ok, false);
    assert.match(r.reason, /leaked-URL attack/, "operator must see the security implication");
  });

  it("rejects when no active session", () => {
    const r = validateTypedCode(null, "4827");
    assert.equal(r.ok, false);
  });
});
