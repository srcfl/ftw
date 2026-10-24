// node --test web/settings/tabs/access.test.mjs
//
// The Access tab lets an owner with no phone app give an AI assistant a way
// into the house. These tests hold the parts that decide what is granted and
// what the owner is told: the grant is never wider than the owner chose, the
// box's names come from the box, and each refusal has words of its own.

import { readFileSync } from "node:fs";
import { describe, it } from "node:test";
import assert from "node:assert/strict";

globalThis.window = {};
await import("./access.js");
const tab = globalThis.window.FTWSettings.tabs.access;
const P = tab._pure;

const read = (path) => readFileSync(new URL(path, import.meta.url), "utf8");
const registryYaml = read("../../../contract/registry.yaml");
const index = read("../../index.html");
const shell = read("../../settings.js");

// The registry slice GET /api/agents carries, as the box builds it.
const registry = {
  agentOps: {
    "site.mode.set": "ftw.goals.request",
    "planner.prefs.set": "ftw.goals.request",
    "loadpoint.hold": "ftw.dispatch.hold",
    "loadpoint.soc.set": "ftw.goals.request",
    "loadpoint.surplus_only.set": "ftw.goals.request",
    "car.goal.set": "ftw.goals.request",
    "car.soc.set": "ftw.goals.request",
  },
  writeScopes: ["ftw.dispatch.write", "ftw.mode.write", "ftw.dispatch.hold", "ftw.goals.request", "ftw.setup.write"],
  callScopes: ["ftw.setup.write"],
  agentArgs: {
    "loadpoint.hold": [
      { name: "id", label: "Charger", kind: "loadpoint" },
      { name: "power_w", label: "Charging power", kind: "power", unit: "W", cap: "maxW" },
      { name: "hold_s", label: "For how long", kind: "duration", unit: "s", cap: "holdS" },
    ],
    "car.goal.set": [
      { name: "id", label: "Car", kind: "car" },
      { name: "target", label: "The level to reach", kind: "ratio" },
      { name: "ready_by_min", label: "Ready by", kind: "number" },
      { name: "surplus_only", label: "Charge from solar only", kind: "bool" },
    ],
  },
};

describe("the page's words follow the registry", () => {
  // The page owns its prose, as the app does. The names it keys them by are
  // the registry's, so a renamed scope or op shows here rather than as a raw
  // name on an owner's screen.
  const agentScopes = /\n  agent:\n    label: [^\n]+\n    scopes: \[([^\]]+)\]/.exec(registryYaml)[1]
    .split(",").map((s) => s.trim());
  const agentOps = [...registryYaml.matchAll(/- \{ name: ([\w.]+), scope: [\w.]+, agent: ([\w.]+)/g)]
    .map((m) => m[1]);

  it("has words for every scope an assistant may hold", () => {
    assert.ok(agentScopes.length >= 8);
    for (const scope of agentScopes) assert.ok(P.SCOPE_WORDS[scope], scope);
    for (const scope of Object.keys(P.SCOPE_WORDS)) assert.ok(agentScopes.includes(scope), scope);
  });

  it("warns that diagnostic access reads full backups that can contain saved credentials", () => {
    const words = P.SCOPE_WORDS["ftw.diag.run"];
    assert.match(words, /full technical diagnostics, source, logs and backups/);
    assert.match(words, /backups can contain saved credentials/);
    assert.doesNotMatch(words, /checks that change nothing/);
  });

  it("has words for every op an assistant may name", () => {
    assert.ok(agentOps.length >= 5);
    for (const op of agentOps) assert.ok(P.OP_WORDS[op], op);
    for (const op of Object.keys(P.OP_WORDS)) assert.ok(agentOps.includes(op), op);
  });
});

describe("a code as an owner types it", () => {
  it("takes any case, a dash and spaces", () => {
    assert.equal(P.normalizeCode("abcd-efgh"), "ABCDEFGH");
    assert.equal(P.normalizeCode(" AB CD EF GH "), "ABCDEFGH");
  });

  it("refuses what cannot be a code before FTW Cloud is asked", () => {
    assert.equal(P.normalizeCode("abc"), "");
    assert.equal(P.normalizeCode("ABCD-EFGH-I"), "");
    assert.equal(P.normalizeCode("ABCD/EFGH"), "");
  });
});

describe("names from outside", () => {
  const rlo = String.fromCharCode(0x202e);
  const zws = String.fromCharCode(0x200b);

  it("lose control and direction marks, and are cut to a row", () => {
    assert.equal(P.cleanName("Helper " + rlo + "exe.pdf"), "Helper exe.pdf");
    assert.equal(P.cleanName("Cla" + zws + "ude\n"), "Claude");
    assert.equal(P.cleanName("x".repeat(100)).length, 64);
  });

  it("lose the marks that hide letters: soft hyphen, joiners, variation selectors and tags", () => {
    const hidden = [0xad, 0x34f, 0x61c, 0xfe0f, 0xe0041, 0xe007f].map((c) => String.fromCodePoint(c)).join("");
    assert.equal(P.cleanName("Cl" + hidden + "aude"), "Claude");
  });

  it("lose double quote marks, so a name cannot end the page's quotes early", () => {
    assert.equal(P.cleanName('Claude" into your home? Yes. "Evil'), "Claude into your home? Yes. Evil");
    assert.equal(P.cleanName("“Claude”"), "Claude");
    assert.equal(P.cleanName("Fred's helper"), "Fred's helper");
  });

  it("are cut by character, never through an emoji", () => {
    const cut = P.cleanName("x".repeat(62) + String.fromCodePoint(0x1f600, 0x1f600, 0x1f600));
    assert.equal(Array.from(cut).length, 64);
    assert.ok(cut.endsWith("…"));
    assert.ok(!/[\ud800-\udbff]…$/.test(cut));
  });

  it("are flagged when an unknown client borrows Sourceful's or FTW's name", () => {
    assert.equal(P.borrowsOurName("FTW Helper", false), true);
    assert.equal(P.borrowsOurName("sourceful support", false), true);
    assert.equal(P.borrowsOurName("FTW Helper", true), false);
    assert.equal(P.borrowsOurName("Claude", false), false);
  });

  it("show a client's host only when its id is a document's address", () => {
    assert.equal(P.clientHost("https://claude.ai/oauth/mcp-oauth-client-metadata"), "claude.ai");
    assert.equal(P.clientHost("dcr_1b2c3d"), "");
    assert.equal(P.clientHost("http://evil.example/x"), "");
  });
});

describe("what the owner grants", () => {
  it("keeps setup separate from reads and only grants the explicit choice", () => {
    const split = P.splitGrant(["ftw.config.read", "ftw.setup.write"], [], registry);
    assert.deepEqual(split.reads, ["ftw.config.read"]);
    assert.deepEqual(split.groups, []);
    assert.deepEqual(split.calls, ["ftw.setup.write"]);
    const base = { reads: split.reads, ops: [], ttlS: 86400 };
    assert.deepEqual(P.chosenGrant(base, 0, registry).scopes, ["ftw.config.read"]);
    assert.deepEqual(P.chosenGrant({ ...base, calls: split.calls }, 0, registry).scopes, ["ftw.config.read", "ftw.setup.write"]);
  });
  const offer = {
    scopes: ["ftw.config.read", "ftw.dispatch.hold", "ftw.goals.request", "ftw.live.read", "ftw.plan.read"],
    ops: ["car.goal.set", "loadpoint.hold", "site.mode.set"],
    caps: { holdS: 1800, maxW: 11000, perMin: 6 },
  };

  it("splits reads from control by the box's own list, in the page's order", () => {
    const split = P.splitGrant(offer.scopes, offer.ops, registry);
    assert.deepEqual(split.reads, ["ftw.live.read", "ftw.plan.read", "ftw.config.read"]);
    assert.deepEqual(split.groups.map((g) => g.scope), ["ftw.dispatch.hold", "ftw.goals.request"]);
    assert.deepEqual(split.groups[1].ops, ["site.mode.set", "car.goal.set"]);
  });

  it("leaves out a control scope with no op, which grants nothing", () => {
    const split = P.splitGrant(["ftw.goals.request", "ftw.live.read"], [], registry);
    assert.deepEqual(split.groups, []);
  });

  it("knows an op runs under caps by its capped arguments", () => {
    assert.equal(P.capped("loadpoint.hold", registry), true);
    assert.equal(P.capped("car.goal.set", registry), false);
  });

  it("grants reads alone with control off: no ops and no caps", () => {
    const g = P.chosenGrant({ reads: ["ftw.plan.read", "ftw.live.read"], ops: [], ttlS: 86400 }, 0, registry);
    assert.deepEqual(g, {
      scopes: ["ftw.live.read", "ftw.plan.read"], ops: [], caps: { holdS: 0, maxW: 0, perMin: 0 }, ttlS: 86400,
    });
  });

  it("adds each ticked op's scope, sorted as the box insists, with the caps the owner set", () => {
    const g = P.chosenGrant({
      reads: ["ftw.live.read"], ops: ["loadpoint.hold", "car.goal.set"], holdMin: "15", maxKW: "7.4", ttlS: "604800",
    }, 6, registry);
    assert.deepEqual(g.scopes, ["ftw.dispatch.hold", "ftw.goals.request", "ftw.live.read"]);
    assert.deepEqual(g.ops, ["car.goal.set", "loadpoint.hold"]);
    assert.deepEqual(g.caps, { holdS: 900, maxW: 7400, perMin: 6 });
    assert.equal(g.ttlS, 604800);
  });

  it("keeps caps at zero when only lasting changes are ticked", () => {
    const g = P.chosenGrant({ reads: [], ops: ["site.mode.set"], holdMin: 30, maxKW: 11, ttlS: 86400 }, 6, registry);
    assert.deepEqual(g.caps, { holdS: 0, maxW: 0, perMin: 6 });
  });

  it("stops a grant of nothing, or caps past the offer, before the box is asked", () => {
    const none = P.chosenGrant({ reads: [], ops: [], ttlS: 86400 }, 0, registry);
    assert.match(P.choiceProblem(none, offer.caps), /at least one/);
    const tooLong = P.chosenGrant({ reads: [], ops: ["loadpoint.hold"], holdMin: 31, maxKW: 11, ttlS: 86400 }, 6, registry);
    assert.match(P.choiceProblem(tooLong, offer.caps), /1 to 30 minutes/);
    const tooStrong = P.chosenGrant({ reads: [], ops: ["loadpoint.hold"], holdMin: 30, maxKW: 12, ttlS: 86400 }, 6, registry);
    assert.match(P.choiceProblem(tooStrong, offer.caps), /up to 11 kW/);
    const fine = P.chosenGrant({ reads: [], ops: ["loadpoint.hold"], holdMin: 30, maxKW: 11, ttlS: 86400 }, 6, registry);
    assert.equal(P.choiceProblem(fine, offer.caps), "");
  });

  it("offers no length longer than the offer asks", () => {
    assert.deepEqual(P.lengthOptions(30 * 86400), [86400, 7 * 86400, 30 * 86400]);
    assert.deepEqual(P.lengthOptions(3 * 86400), [86400]);
    assert.deepEqual(P.lengthOptions(3600), [3600]);
  });

  it("offers up to a year where the box takes one, and 30 days where it says nothing", () => {
    const day = 86400;
    const box = { longestMs: { agent: 365 * day * 1000, support: 14 * day * 1000 } };
    assert.equal(P.longestS(box, "agent"), 365 * day);
    assert.equal(P.longestS(box, "support"), 14 * day);
    assert.equal(P.longestS({}, "agent"), 30 * day);
    assert.equal(P.longestS(null, "agent"), 30 * day);
    assert.equal(P.longestS({}, "support"), 14 * day);
    assert.equal(P.longestS({}, "release"), 90 * day);
    assert.deepEqual(P.lengthOptions(Math.min(365 * day, P.longestS(box, "agent"))),
      [day, 7 * day, 30 * day, 90 * day, 365 * day]);
    assert.deepEqual(P.lengthOptions(Math.min(365 * day, P.longestS({}, "agent"))), [day, 7 * day, 30 * day]);
    // Renewing Sourceful offers nothing past its 14 days.
    assert.deepEqual(P.lengthOptions(P.longestS(box, "support")), [day, 7 * day]);
    assert.equal(P.lengthText(365 * day), "1 year");
    // The page still starts at a week.
    assert.equal(P.defaultLength(P.lengthOptions(365 * day)), 7 * day);
  });

  it("chooses a week unless the offer asks for less", () => {
    assert.equal(P.defaultLength(P.lengthOptions(30 * 86400)), 7 * 86400);
    assert.equal(P.defaultLength(P.lengthOptions(3 * 86400)), 86400);
    assert.equal(P.defaultLength(P.lengthOptions(3600)), 3600);
  });

  it("keeps a power cap exact to the watt, so the default is never refused", () => {
    // A phone may approve 3,680 W, which one decimal of kW would round up to
    // 3,700 W, past the row's own cap.
    for (const w of [3680, 11000, 7400, 1, 99999, 16560]) {
      const g = P.chosenGrant({ reads: [], ops: ["loadpoint.hold"], holdMin: 30, maxKW: P.kwExact(w), ttlS: 86400 }, 6, registry);
      assert.equal(g.caps.maxW, w, String(w));
      assert.equal(P.choiceProblem(g, { holdS: 1800, maxW: w }), "", String(w));
    }
  });
});

describe("what stops a grant before a code is spent", () => {
  const ok = { box: { offers: true, clock: "ok", lookup: "https://cloud.ftw.energy/oauth/requests", widen: true } };
  const on = { lan_auth: true, configured: true };

  it("is nothing once the house password is on and the box can take an offer", () => {
    assert.equal(P.readiness(ok, "", on), null);
    assert.equal(P.readiness(ok, "", { home_assistant: true, lan_auth: false, configured: false }), null);
  });

  it("names each block, in the order to fix them", () => {
    assert.equal(P.readiness(null, "off", on).kind, "unavailable");
    assert.equal(P.readiness(null, "", on).kind, "loading");
    assert.equal(P.readiness(ok, "", { lan_auth: false, configured: true }).kind, "password");
    assert.equal(P.readiness(ok, "", { lan_auth: true, configured: false }).kind, "password");
    assert.equal(P.readiness(ok, "", null).kind, "password");
    // On the box itself, or through a tunnel, the password does not count.
    assert.equal(P.readiness({ box: { ...ok.box, widen: false } }, "", on).kind, "here");
    assert.equal(P.readiness({ box: { ...ok.box, offers: false } }, "", on).kind, "keys");
    assert.equal(P.readiness({ box: { ...ok.box, clock: "behind", nowMs: 1, markMs: 2 } }, "", on).kind, "behind");
    assert.equal(P.readiness({ box: { ...ok.box, clock: "unsynced" } }, "", on).kind, "clock");
  });
});

describe("refusals in the page's words", () => {
  it("tells a wrong or spent code from FTW Cloud's other answers", () => {
    assert.match(P.lookupFailureText(404), /No request is waiting with that code/);
    assert.match(P.lookupFailureText(429), /Too many tries/);
    assert.match(P.lookupFailureText(0), /No answer from FTW Cloud/);
    assert.match(P.lookupFailureText(502), /502/);
  });

  it("names each reason the box will not take an offer", () => {
    const now = 1_800_000_000_000;
    assert.match(P.offerRefusalText({ reason: "no_offer_keys" }, {}, now), /does not know FTW Cloud's signing key/);
    assert.match(P.offerRefusalText({ reason: "unsigned" }, {}, now), /signature/);
    assert.match(P.offerRefusalText({ reason: "other_home" }, {}, now), /another FTW home/);
    assert.match(P.offerRefusalText({ reason: "malformed" }, {}, now), /cannot read this offer/);
    assert.match(P.offerRefusalText({ reason: "expired", boxMs: now }, { expiresAtMs: now - 1 }, now), /has run out/);
  });

  it("blames the box's clock when FTW Cloud and this device still count the offer as good", () => {
    const now = 1_800_000_000_000;
    const text = P.offerRefusalText({ reason: "expired", boxMs: now + 20 * 60_000 }, { expiresAtMs: now + 5 * 60_000 }, now);
    assert.match(text, /The box's clock reads/);
    assert.match(text, /this device reads/);
  });

  it("says what the box's refusal codes mean", () => {
    assert.match(P.refusalText({ error: "E_CAP_EXCEEDED", args: { cap: "agents", max: 16 } }), /16 assistants/);
    assert.match(P.refusalText({ error: "E_CAP_EXCEEDED", args: { cap: "expiresAtMs" } }), /longer than/);
    assert.match(P.refusalText({ error: "E_UNKNOWN_OP", args: { field: "key" } }), /already has access/);
    assert.match(P.refusalText({ error: "E_UNAVAILABLE", args: { subsystem: "clock" } }), /clock is not set/);
    assert.match(P.refusalText({ error: "E_CONFLICT" }), /changed this access/);
    assert.equal(P.refusalText({ error: "give access in the app" }), "give access in the app");
  });
});

describe("a request waiting for the owner", () => {
  it("lists its arguments in the registry's order, by its labels, and the rest last", () => {
    const rows = P.requestArgs({
      op: "car.goal.set",
      args: { surplus_only: false, ready_by_min: 420, target: 0.8, id: "car_7f3a", extra: "x" },
    }, registry);
    assert.deepEqual(rows, [
      { label: "Car", value: "car_7f3a" },
      { label: "The level to reach", value: "80 %" },
      { label: "Ready by", value: "07:00 (box local time)" },
      { label: "Charge from solar only", value: "no" },
      { label: "extra", value: "x" },
    ]);
  });

  it("cleans an argument's name that the registry does not know", () => {
    const rlo = String.fromCharCode(0x202e);
    const rows = P.requestArgs({ op: "x", args: { ["evil" + rlo + "gnp"]: 1 } }, registry);
    assert.equal(rows[0].label, "evilgnp");
  });

  it("names a charger or car by what the box calls it, and by its id when the box has no name", () => {
    const car = P.requestArgs({
      op: "car.goal.set",
      args: { id: "0123456789abcdef0123456789abcdef", target: 0.8 },
      names: { id: "Volvo" + String.fromCharCode(0x200b) + " XC40" },
    }, registry);
    assert.deepEqual(car.map((r) => r.value), ["Volvo XC40", "80 %"]);
    const charger = P.requestArgs({
      op: "loadpoint.hold",
      args: { id: "garage", power_w: 7400 },
      names: { id: "Carport Easee", power_w: "not a name" },
    }, registry);
    assert.deepEqual(charger.map((r) => r.value), ["Carport Easee", "7.4 kW"]);
    const gone = P.requestArgs({ op: "car.goal.set", args: { id: "0123456789abcdef0123456789abcdef" }, names: { id: "" } }, registry);
    assert.equal(gone[0].value, "0123456789abcdef0123456789abcdef");
  });

  it("shows power, length and odd numbers plainly", () => {
    const rows = P.requestArgs({ op: "loadpoint.hold", args: { power_w: 7400, hold_s: 900, id: "lp_1" } }, registry);
    assert.deepEqual(rows.map((r) => r.value), ["lp_1", "7.4 kW", "15 min"]);
    assert.equal(P.requestArgs({ op: "x", args: { n: Infinity } }, registry)[0].value, "?");
  });

  it("says how an approved request ended", () => {
    assert.match(P.resultText({ state: "declined" }), /Declined/);
    assert.match(P.resultText({ state: "approved", result: { State: "applied" } }), /made the change/);
    assert.match(P.resultText({ state: "approved", result: { State: "rejected", Error: { Code: "E_PRECONDITION" } } }), /refused the change \(E_PRECONDITION\)/);
    assert.match(P.resultText({ state: "approved", result: { State: "unconfirmed" } }), /could not confirm/);
  });
});

describe("the tab", () => {
  it("asks for a code with the field off until the box says it can take one", () => {
    const html = tab.render();
    assert.match(html, /id="access-code"[^>]*disabled/);
    assert.match(html, /id="access-lookup"[^>]*disabled/);
    assert.match(html, /No phone app is needed/);
  });

  it("sits in Settings beside the FTW app, without a Save button", () => {
    assert.match(index, /<button data-tab="app">FTW app<\/button>\s*<button data-tab="access">Access<\/button>/);
    assert.match(index, /src="\/settings\/tabs\/access\.js\?v=\w+"/);
    assert.match(shell, /tab === "access"/);
  });
});
