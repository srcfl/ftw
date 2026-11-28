import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { describe, it } from "node:test";
import vm from "node:vm";

// Sharing, on the box's own settings page.
//
// The device list is where a household decides who can reach their home, so
// these drive the real refreshDevices() and the real buttons against a small
// DOM and read what they built. Asserting on the markup string alone would
// not catch the row that never gets a Remove button.

const source = readFileSync(new URL("./settings/tabs/app.js", import.meta.url), "utf8");

// A DOM small enough to read and real enough to answer the two questions
// here: what did the page build, and what does it say.
function element(tag) {
  const el = {
    tagName: tag,
    className: "",
    type: "",
    hidden: false,
    disabled: false,
    children: [],
    listeners: {},
    appendChild(child) {
      this.children.push(child);
      return child;
    },
    addEventListener(name, fn) {
      this.listeners[name] = fn;
    },
    setAttribute(name, value) { this[name] = value; },
    focus() {},
    select() {},
    click() {
      if (this.listeners.click) this.listeners.click();
    },
    // Everything this element and its descendants say, so a test can assert
    // on wording without walking the tree by hand.
    words() {
      return [this.textContent, ...this.children.map((c) => c.words())].join(" ");
    },
    buttons() {
      const mine = this.tagName === "button" ? [this] : [];
      return mine.concat(...this.children.map((c) => c.buttons()));
    },
  };

  // Setting textContent replaces everything inside, children included. That
  // is how the real DOM clears a slot, and it is what keeps the assertions
  // below about what is on screen now rather than about everything that has
  // ever been on it — a stale sentence left in the tree would let a test pass
  // on the strength of a code that has already been replaced.
  let text = "";
  Object.defineProperty(el, "textContent", {
    get() {
      return text;
    },
    set(value) {
      text = String(value);
      el.children = [];
    },
  });
  return el;
}

// The ids render() reaches for. Registered up front, because the tab wires
// itself to them the moment it is rendered.
const IDS = [
  "app-link-devices",
  "app-link-slot",
  "app-link-status",
  "app-link-pair",
  "app-link-share",
  "app-link-enabled",
  "app-link-code",
  "app-link-restart",
  "app-link-restart-status",
  "app-link-switch",
  "app-link-terms",
  "app-cloud-panel",
  "app-link-off",
  "app-link-off-row",
  "app-link-off-status",
];

// load runs app.js, renders the tab, and hands back the DOM it wired itself
// into along with every request it made.
// cloud is what GET /api/cloud answers, and grant what POST /api/cloud/grant
// answers: { error } refuses it. The default is a box that cannot answer
// /api/cloud, which has nothing to accept.
const POLICY = { cloud_scopes: ["live.read", "history.read"], cloud_policy: "ftw-cloud-2026-10-v3" };

function load({ devices = [], pairing = null, spoken = null, refuse = null, saveConfig = async () => ({ restart_required: false }), confirmRestart = true, resume = false, shellRestart = false, cloud = null, grant = null, saved = true, running = true, ha = false, policy = POLICY } = {}) {
  const byId = new Map();
  for (const id of IDS) {
    byId.set(id, element(id.startsWith("app-link-devices") || id.endsWith("slot") ? "div" : "button"));
  }
  byId.get("app-link-share").hidden = true;

  const document = {
    getElementById: (id) => byId.get(id) ?? null,
    createElement: element,
  };

  const calls = [];
  const confirmed = [];
  const copied = [];
  const opened = [];
  const session = new Map(resume ? [["ftw.pairing.resume", "1"]] : []);
  let shellRestarts = 0;
  if (shellRestart) {
    const restart = element("button");
    restart.addEventListener("click", () => shellRestarts++);
    byId.set("restart-now", restart);
  }
  let reloaded = false;
  const answer = (body, ok = true) => Promise.resolve({ ok, json: () => Promise.resolve(body) });

  const sandbox = {
    window: { FTWSettings: { tabs: {}, open(tab) { opened.push(tab); } }, sessionStorage: { getItem: (key) => session.get(key), setItem: (key, value) => session.set(key, value), removeItem: (key) => session.delete(key) }, location: { origin: "http://ftw.local:8080", reload() { reloaded = true; } }, navigator: { clipboard: { async writeText(value) { copied.push(value); } } } },
    setTimeout: (fn) => {
      fn();
      return 0;
    },
    document,
    // Kept, not just answered. The warning before a removal is the whole of
    // what the page tells a household about a step it cannot undo, and a stub
    // that only said yes let it go blank without a test noticing.
    confirm: (question) => {
      confirmed.push(question);
      return confirmRestart;
    },
    fetch: (path, opts) => {
      calls.push({ path, opts });
      if (refuse && opts && opts.method === refuse.method) {
        return answer({ error: refuse.error }, false);
      }
      if (path === "/api/app-link/devices") return answer({ devices });
      if (path === "/api/app-link/pairing") {
        // The box echoes back the role it minted for, so the stub does too.
        // One that answered a fixed role whatever it was asked would hide the
        // bug this file exists to catch: a code whose screen names a power it
        // does not carry.
        const asked = JSON.parse((opts && opts.body) || "{}");
        const shape = asked.kind === "spoken" ? spoken : pairing;
        return answer(shape && Object.assign({}, shape, { role: asked.role }));
      }
      if (path === "/api/app-link/status") {
        return answer({ enabled: running, paired_devices: devices.length });
      }
      if (path === "/api/cloud") return cloud ? answer(cloud) : answer({ error: "unavailable" }, false);
      if (path === "/api/cloud/stats-policy") return policy ? answer(policy) : answer({ error: "unavailable" }, false);
      if (path === "/api/cloud/grant") {
        if (grant && grant.error) return answer({ error: grant.error }, false);
        cloud = Object.assign({}, cloud, { cloudTermsAccepted: true, link: "on", state: "on" });
        return answer(cloud);
      }
      return answer({});
    },
    JSON,
    Promise,
    Date,
    Math,
    Array,
    Object,
    String,
    console,
  };
  // home-assistant.js sets this flag on pages Core serves through Home
  // Assistant's ingress.
  if (ha) sandbox.window.FTWHomeAssistant = true;
  sandbox.globalThis = sandbox;
  vm.createContext(sandbox);
  vm.runInContext(source, sandbox);

  const tab = sandbox.window.FTWSettings.tabs.app;
  const config = { app_link: { enabled: saved } };
  const html = tab.render({ config, saveConfig });
  return { html, config, calls, confirmed, copied, opened, session, shellRestarts: () => shellRestarts, reloaded: () => reloaded, el: (id) => byId.get(id) };
}

// Lets the promise chains inside the tab settle. Two turns, because a button
// press fetches and then repaints from a second fetch.
const settle = async () => {
  for (let i = 0; i < 4; i++) await new Promise((r) => setTimeout(r, 0));
};

describe("the device list", () => {
  it("says what each phone may do", async () => {
    const { el } = load({
      devices: [
        { id: "aaaa1111", role: "owner", last_seen_ms: Date.now() },
        { id: "bbbb2222", role: "viewer", last_seen_ms: Date.now() },
      ],
    });
    await settle();

    const list = el("app-link-devices");
    assert.equal(list.children.length, 2, "both phones should be listed");
    assert.match(list.children[0].words(), /Can change things/);
    assert.match(list.children[1].words(), /Can look/);
  });

  it("removes a guest with the same button that locks a phone out", async () => {
    const { el, calls } = load({
      devices: [
        { id: "aaaa1111", role: "owner" },
        { id: "bbbb2222", role: "viewer" },
      ],
    });
    await settle();

    const list = el("app-link-devices");
    for (const row of list.children) {
      const labels = row.buttons().map((b) => b.textContent);
      assert.ok(
        labels.includes("Remove"),
        `no Remove on "${row.words()}" — a guest must be removed where a phone is`,
      );
    }

    // And it is the same request, not a sharing-specific one.
    list.children[1].buttons().find((b) => b.textContent === "Remove").click();
    await settle();
    const removal = calls.find((c) => c.opts && c.opts.method === "DELETE");
    assert.ok(removal, "removing a guest sent nothing");
    assert.equal(removal.path, "/api/app-link/devices/bbbb2222");
  });

  // A household that has lost its only phone has to be able to get back to
  // nothing paired. The box allows that from this page and refuses it from the
  // app, because whoever is at the box can mint a fresh code on the same page.
  // So the row offers the button, and the warning carries the cost.
  it("removes the last phone from the box's own page, and says what that costs", async () => {
    const { el, calls, confirmed } = load({
      devices: [{ id: "aaaa1111", role: "owner", last_owner: true }],
    });
    await settle();

    const row = el("app-link-devices").children[0];
    assert.doesNotMatch(
      row.words(),
      /add another before removing/i,
      "the row still tells a household to do something the box no longer asks of it",
    );

    const remove = row.buttons().find((b) => b.textContent === "Remove");
    assert.ok(remove, "the last phone cannot be removed where every other phone can");
    assert.equal(remove.disabled, false, "the button is drawn dead");

    remove.click();
    await settle();

    const said = confirmed.join(" ");
    assert.match(said, /last phone/i, "nothing warned that this is the last one");
    assert.match(said, /see or change this home/i, "the warning never says what is lost");
    assert.match(said, /code from this page/i, "the warning never says how to get back in");

    const removal = calls.find((c) => c.opts && c.opts.method === "DELETE");
    assert.ok(removal, "the button asked and then sent nothing");
    assert.equal(removal.path, "/api/app-link/devices/aaaa1111");
  });

  // Stepping the last owner down is refused at both doors, so that button
  // stays off the row. A button whose only answer is a refusal is worse than
  // no button.
  it("does not offer to step the last owner down", async () => {
    const { el } = load({
      devices: [{ id: "aaaa1111", role: "owner", last_owner: true }],
    });
    await settle();

    const row = el("app-link-devices").children[0];
    const labels = row.buttons().map((b) => b.textContent);
    assert.deepEqual(labels, ["Remove"], `the row offers ${labels.join(", ")}`);
    assert.match(row.words(), /only phone that can change things/);
  });

  // The only owner with guests still paired is a different sentence. Those
  // phones keep looking; what goes is anybody's ability to change anything.
  it("warns about what is left when guests stay behind", async () => {
    const { el, confirmed } = load({
      devices: [
        { id: "aaaa1111", role: "owner", last_owner: true },
        { id: "bbbb2222", role: "viewer" },
      ],
    });
    await settle();

    el("app-link-devices").children[0].buttons().find((b) => b.textContent === "Remove").click();
    await settle();

    const said = confirmed.join(" ");
    assert.match(said, /look but change nothing/i, "the warning claims more is lost than is");
    assert.doesNotMatch(said, /last phone/i, "two phones are paired and one was called the last");
  });

  // A watch sits under the phone that added it. Its reach follows that
  // phone, so it gets Remove and nothing else.
  it("lists a watch under its phone, with no role button", async () => {
    const { el } = load({
      devices: [
        { id: "aaaa1111", role: "owner" },
        { id: "wwww3333", role: "watch", parent: "aaaa1111" },
        { id: "bbbb2222", role: "viewer" },
      ],
    });
    await settle();

    const rows = el("app-link-devices").children;
    assert.equal(rows.length, 3);
    assert.match(rows[1].words(), /Watch wwww3333/);
    assert.match(rows[1].words(), /Can look and charge/);
    assert.match(rows[1].className, /app-device-watch/, "the watch is not set in under its phone");
    assert.doesNotMatch(rows[0].className, /app-device-watch/);
    assert.deepEqual(rows[1].buttons().map((b) => b.textContent), ["Remove"]);
  });

  it("says a phone's watches go with it, and a watch goes alone", async () => {
    const { el, confirmed } = load({
      devices: [
        { id: "aaaa1111", role: "owner", last_owner: true },
        { id: "wwww3333", role: "watch", parent: "aaaa1111" },
      ],
    });
    await settle();

    const rows = el("app-link-devices").children;
    rows[1].buttons().find((b) => b.textContent === "Remove").click();
    await settle();
    assert.match(confirmed[0], /Remove this watch\?/);
    assert.doesNotMatch(confirmed[0], /last phone/i, "removing a watch was called removing the last phone");

    rows[0].buttons().find((b) => b.textContent === "Remove").click();
    await settle();
    assert.match(confirmed[1], /last phone/i, "a phone with a watch was not called the last phone");
    assert.match(confirmed[1], /Its watch goes with it/, "the warning hides that the watch goes too");
  });

  it("hides sharing until there is somebody to share from", async () => {
    // The first phone on a box is its owner whatever code it used, so an
    // invite offered before then would silently make an owner.
    const empty = load({ devices: [] });
    await settle();
    assert.equal(empty.el("app-link-share").hidden, true);

    const paired = load({ devices: [{ id: "aaaa1111", role: "owner" }] });
    await settle();
    assert.equal(paired.el("app-link-share").hidden, false);
  });

  it("changes a role from the list rather than a screen of its own", async () => {
    const { el, calls } = load({
      devices: [
        { id: "aaaa1111", role: "owner" },
        { id: "bbbb2222", role: "viewer" },
      ],
    });
    await settle();

    const row = el("app-link-devices").children[1];
    const promote = row.buttons().find((b) => /change things/.test(b.textContent));
    assert.ok(promote, "a guest cannot be promoted from the list");
    promote.click();
    await settle();

    const patch = calls.find((c) => c.opts && c.opts.method === "PATCH");
    assert.ok(patch, "no role change was sent");
    assert.equal(patch.path, "/api/app-link/devices/bbbb2222");
    assert.equal(JSON.parse(patch.opts.body).role, "owner");
  });

  it("says why when the box refuses", async () => {
    // Without this the button simply does nothing and the household has no
    // idea the box protected anything.
    const { el } = load({
      devices: [
        { id: "aaaa1111", role: "owner" },
        { id: "bbbb2222", role: "owner" },
      ],
      refuse: { method: "DELETE", error: "that is the only phone that can change anything here." },
    });
    await settle();

    const row = el("app-link-devices").children[0];
    row.buttons().find((b) => b.textContent === "Remove").click();
    await settle();

    assert.match(row.words(), /only phone that can change anything/);
  });
});

describe("the code on screen", () => {
  it("names what an invite lets in, above the code", async () => {
    const { el } = load({
      devices: [{ id: "aaaa1111", role: "owner" }],
      pairing: {
        url: "https://app.ftw.energy/p#v2.a.b.c.d",
        role: "viewer",
        expires_at_ms: Date.now() + 600000,
      },
    });
    await settle();

    el("app-link-share").click();
    await settle();

    const said = el("app-link-slot").words();
    assert.match(said, /lets someone see this home/i);
    assert.match(said, /cannot change anything/i);
  });

  it("asks for the role the button promises", async () => {
    const { el, calls } = load({
      devices: [{ id: "aaaa1111", role: "owner" }],
      pairing: { url: "https://app.ftw.energy/p#x", role: "viewer", expires_at_ms: Date.now() + 600000 },
    });
    await settle();

    el("app-link-share").click();
    await settle();
    const invite = calls.filter((c) => c.path === "/api/app-link/pairing").pop();
    assert.equal(JSON.parse(invite.opts.body).role, "viewer");

    el("app-link-pair").click();
    await settle();
    const owner = calls.filter((c) => c.path === "/api/app-link/pairing").pop();
    assert.equal(JSON.parse(owner.opts.body).role, "owner");
  });

  it("shows a box code as readable text", async () => {
    const { el, calls } = load({
      devices: [{ id: "aaaa1111", role: "owner" }],
      pairing: { url: "https://app.ftw.energy/p#x", expires_at_ms: Date.now() + 600000 },
      spoken: { code: "ABCD-EFGH", expires_at_ms: Date.now() + 300000 },
    });
    await settle();

    el("app-link-pair").click();
    await settle();
    el("app-link-slot").buttons()[0].click();
    await settle();

    const asked = calls.filter((c) => c.path === "/api/app-link/pairing").pop();
    assert.equal(JSON.parse(asked.opts.body).kind, "spoken");

    const said = el("app-link-slot").words();
    assert.match(said, /ABCD-EFGH/, "the code was never shown");
    assert.match(said, /Five wrong tries/, "nothing warns that guessing burns the code");
  });

  // The box code is the floor that always works, so it has to reach a guest
  // too — a phone with no camera cannot be shared a home by scanning. It
  // carries the role of the code already on screen, so the sentence the
  // household just read is the one the code obeys.
  it("reads out a code for whoever the code on screen was for", async () => {
    const { el, calls } = load({
      devices: [{ id: "aaaa1111", role: "owner" }],
      pairing: { url: "https://app.ftw.energy/p#x", expires_at_ms: Date.now() + 600000 },
      spoken: { code: "K7M2-9QRT", expires_at_ms: Date.now() + 300000 },
    });
    await settle();

    // The guest's code first, then read it out rather than scan it.
    el("app-link-share").click();
    await settle();
    const fallback = el("app-link-slot").buttons()[0];
    assert.ok(fallback, "a code on screen offers no way to read it out");
    assert.match(fallback.textContent, /read a code out/i);

    fallback.click();
    await settle();

    const asked = JSON.parse(calls.filter((c) => c.path === "/api/app-link/pairing").pop().opts.body);
    assert.deepEqual(
      asked,
      { role: "viewer", kind: "spoken" },
      "the spoken code was minted for a different role than the screen promised",
    );
    assert.match(el("app-link-slot").words(), /K7M2-9QRT/);
    assert.match(
      el("app-link-slot").words(),
      /cannot change anything/i,
      "the spoken code never says it is view only",
    );
  });

  it("explains first pairing on one phone, with the local code and house password", () => {
    const { html } = load({ devices: [{ id: "aaaa1111", role: "owner" }] });
    assert.match(html, /Only one phone/);
    assert.match(html, /home Wi-Fi/);
    assert.match(html, /house password/);
    assert.doesNotMatch(html, /never seen this box has to scan/i);
  });

  it("never prints a scannable payload as text", async () => {
    // The credential stays out of visible text until an explicit copy.
    const payload = "https://app.ftw.energy/p#v2.secret.key.material.here";
    const { el } = load({
      devices: [{ id: "aaaa1111", role: "owner" }],
      pairing: { url: payload, role: "owner", expires_at_ms: Date.now() + 600000 },
    });
    await settle();

    el("app-link-pair").click();
    await settle();

    assert.doesNotMatch(el("app-link-slot").words(), /secret\.key\.material/);
  });
});

describe("pairing and restart on one phone", () => {
  it("opens the full pairing link and copies it only on request", async () => {
    const link = "https://app.ftw.energy/p#v3.box.code.hint.secret.home";
    const { el, copied } = load({ pairing: { url: link, expires_at_ms: Date.now() + 600000 } });
    await settle();
    el("app-link-pair").click();
    await settle();
    const slot = el("app-link-slot");
    assert.equal(slot.children.find((c) => c.tagName === "a").href, link);
    assert.deepEqual(copied, []);
    slot.buttons().find((b) => b.textContent === "Copy pairing link").click();
    await settle();
    assert.deepEqual(copied, [link]);
  });

  it("offers the short code directly and copies the box address with it", async () => {
    const { el, copied, calls } = load({ spoken: { code: "K7M2-9QRT", expires_at_ms: Date.now() + 300000 } });
    await settle();
    el("app-link-code").click();
    await settle();
    assert.deepEqual(JSON.parse(calls.find((c) => c.path === "/api/app-link/pairing").opts.body), { role: "owner", kind: "spoken" });
    assert.match(el("app-link-slot").words(), /http:\/\/ftw.local:8080/);
    el("app-link-slot").buttons().find((b) => b.textContent === "Copy pairing code and address").click();
    await settle();
    assert.deepEqual(copied, ["http://ftw.local:8080#K7M2-9QRT"]);
  });

  it("saves before restarting and waits for the box before reloading", async () => {
    const order = [];
    const page = load({ saveConfig: async () => { order.push("save"); return { restart_required: false }; } });
    page.el("app-link-restart").click();
    await settle();
    assert.deepEqual(order, ["save"]);
    assert.match(page.confirmed[0], /Energy control pauses/);
    assert.ok(page.calls.some((c) => c.path === "/api/restart" && c.opts.method === "POST"));
    assert.ok(page.calls.some((c) => c.path.startsWith("/api/health?")));
    assert.equal(page.reloaded(), true);
    assert.equal(page.session.get("ftw.pairing.resume"), "1");
  });

  it("uses the shell's restart once when Save already opened its dialog", async () => {
    const page = load({ shellRestart: true, saveConfig: async () => ({ restart_required: true }) });
    page.el("app-link-restart").click();
    await settle();
    assert.equal(page.shellRestarts(), 1);
    assert.equal(page.calls.some((c) => c.path === "/api/restart"), false);
  });

  it("returns to the app tab once after the restart reload", () => {
    const page = load({ resume: true });
    assert.deepEqual(page.opened, ["app"]);
    assert.equal(page.session.has("ftw.pairing.resume"), false);
  });

  it("keeps the restart action available after a refusal, and does nothing on cancel", async () => {
    const refused = load({ refuse: { method: "POST", error: "restart not configured" } });
    refused.el("app-link-restart").click();
    await settle();
    assert.match(refused.el("app-link-restart-status").textContent, /restart not configured/);
    assert.equal(refused.el("app-link-restart").disabled, false);
    assert.equal(refused.reloaded(), false);
    const canceled = load({ confirmRestart: false });
    canceled.el("app-link-restart").click();
    await settle();
    assert.equal(canceled.calls.some((c) => c.path === "/api/restart"), false);
  });
});

// Pairing accepts the FTW Cloud terms, as it does in the app (Fredrik,
// 23 November 2026). The box page used to refuse the button until the terms
// were accepted in a separate panel; now one tap accepts them through the
// same route and shows the code. The box stays the judge of both steps.
describe("pairing in one tap", () => {
  const fresh = { available: true, cloudTermsAccepted: false, link: "off", state: "off" };
  const qr = { url: "https://app.ftw.energy/p#v3.a.b.c.d.e", expires_at_ms: Date.now() + 600000 };

  it("accepts the terms, then shows the code", async () => {
    const { el, calls } = load({ cloud: fresh, pairing: qr });
    await settle();
    el("app-link-pair").click();
    await settle();

    const paths = calls.map((c) => c.path);
    const grant = calls.findIndex((c) => c.path === "/api/cloud/grant");
    const mint = calls.findIndex((c) => c.path === "/api/app-link/pairing");
    assert.ok(grant >= 0, `no terms were accepted: ${paths.join(", ")}`);
    assert.ok(mint > grant, "the code was asked for before the terms were accepted");
    assert.equal(calls[grant].opts.method, "POST");
    const body = JSON.parse(calls[grant].opts.body);
    assert.deepEqual(body.scopes, POLICY.cloud_scopes);
    assert.equal(body.policy, POLICY.cloud_policy);
    assert.match(el("app-link-slot").words(), /adds a phone that can change things/);
  });

  it("stops and says why when the box refuses the terms", async () => {
    const { el, calls } = load({
      cloud: fresh,
      pairing: qr,
      grant: { error: "log in with the house password to save this choice, or use the owner's app" },
    });
    await settle();
    el("app-link-pair").click();
    await settle();

    assert.equal(calls.some((c) => c.path === "/api/app-link/pairing"), false, "a code was made after the box refused the terms");
    assert.match(el("app-link-slot").words(), /house password/);
    assert.equal(el("app-link-pair").disabled, false, "the button stays dead after a refusal");
  });

  // Terms the page could not read are not terms accepted. A code shown then
  // would pair a phone while the box still holds no answer.
  it("stops when the box's terms cannot be read", async () => {
    const { el, calls } = load({ cloud: fresh, pairing: qr, policy: null });
    await settle();
    el("app-link-pair").click();
    await settle();

    assert.equal(calls.some((c) => c.path === "/api/cloud/grant"), false);
    assert.equal(calls.some((c) => c.path === "/api/app-link/pairing"), false, "a code was made with the terms unaccepted");
    assert.match(el("app-link-slot").words(), /Could not read the FTW Cloud terms/);
    assert.equal(el("app-link-pair").disabled, false);
  });

  it("does not accept again once the terms are accepted", async () => {
    const { el, calls } = load({ cloud: { available: true, cloudTermsAccepted: true, link: "on", state: "on" }, pairing: qr });
    await settle();
    el("app-link-code").click();
    await settle();
    assert.equal(calls.some((c) => c.path === "/api/cloud/grant"), false);
    assert.ok(calls.some((c) => c.path === "/api/app-link/pairing"));
  });

  it("pairs without a grant on a build with no Cloud service", async () => {
    const { el, calls } = load({ cloud: { available: false, cloudTermsAccepted: false, link: "off", state: "off" }, pairing: qr });
    await settle();
    el("app-link-pair").click();
    await settle();
    assert.equal(calls.some((c) => c.path === "/api/cloud/grant"), false);
    assert.ok(calls.some((c) => c.path === "/api/app-link/pairing"));
  });

  it("shows the terms line by the buttons until the terms are accepted", async () => {
    const page = load({ cloud: fresh, pairing: qr });
    await settle();
    assert.equal(page.el("app-link-terms").hidden, false, "the line that says pairing accepts the terms is missing");
    page.el("app-link-pair").click();
    await settle();
    assert.equal(page.el("app-link-terms").hidden, true, "the line stays after the terms were accepted");

    const accepted = load({ cloud: { available: true, cloudTermsAccepted: true, link: "on", state: "on" } });
    await settle();
    assert.equal(accepted.el("app-link-terms").hidden, true);

    const stopped = load({ cloud: fresh, saved: false, running: false });
    await settle();
    assert.equal(stopped.el("app-link-terms").hidden, true, "a terms line beside buttons that cannot work");
  });

  it("shows the Cloud panel only while Cloud is in use", async () => {
    const off = load({ cloud: fresh });
    await settle();
    assert.equal(off.el("app-cloud-panel").hidden, true, "a panel about Cloud while Cloud is off");

    const on = load({ cloud: { available: true, cloudTermsAccepted: true, link: "on", state: "on" } });
    await settle();
    assert.equal(on.el("app-cloud-panel").hidden, false);

    const ending = load({ cloud: { available: true, cloudTermsAccepted: false, link: "ending", state: "revoking" } });
    await settle();
    assert.equal(ending.el("app-cloud-panel").hidden, false, "Cloud's deletion is hidden");
  });

  it("shows the box address beside the QR", async () => {
    const { el } = load({ pairing: qr });
    await settle();
    el("app-link-pair").click();
    await settle();
    assert.match(el("app-link-slot").words(), /Box address: http:\/\/ftw\.local:8080/);
  });
});

describe("the app link switch", () => {
  it("is hidden while the app link is on and running", async () => {
    const { el } = load();
    await settle();
    assert.equal(el("app-link-switch").hidden, true);
  });

  // The owner can always stop the app link at the box. While it runs, the
  // switch gives way to a quiet action below the pairing, which saves the
  // same setting and restarts.
  it("offers a quiet way to turn it off while it runs", async () => {
    const running = load();
    await settle();
    assert.equal(running.el("app-link-off-row").hidden, false, "no way to turn the app link off on the box");

    const off = load({ saved: false, running: false });
    await settle();
    assert.equal(off.el("app-link-off-row").hidden, true, "two ways to change one switch on screen");
  });

  it("turns it off by saving the setting and restarting", async () => {
    let savedEnabled = null;
    const page = load({
      saveConfig: async () => {
        savedEnabled = page.config.app_link.enabled;
        return { restart_required: false };
      },
    });
    page.el("app-link-enabled").checked = true;
    await settle();

    page.el("app-link-off").click();
    await settle();

    assert.match(page.confirmed[0], /Turn off the app link\?/);
    assert.match(page.confirmed[0], /energy control pauses/);
    assert.equal(page.confirmed.length, 1, "asked twice for one action");
    assert.equal(savedEnabled, false, "the restart came before the setting was saved off");
    assert.equal(page.el("app-link-enabled").checked, false, "the switch still says on");
    assert.ok(page.calls.some((c) => c.path === "/api/restart" && c.opts.method === "POST"));
    assert.match(page.el("app-link-off-status").textContent, /FTW is back|Restarting/);
  });

  // The save carries the setting. When it fails, the page must not go on
  // saying off: a later Save elsewhere would then turn the link off unasked.
  it("puts the switch back when the save fails", async () => {
    const page = load({ saveConfig: async () => { throw new Error("the box said no"); } });
    page.el("app-link-enabled").checked = true;
    await settle();

    page.el("app-link-off").click();
    await settle();

    assert.equal(page.config.app_link.enabled, true, "the page holds off while the box has it on");
    assert.equal(page.el("app-link-enabled").checked, true, "the switch says off while nothing was saved");
    assert.equal(page.calls.some((c) => c.path === "/api/restart"), false, "restarted after a failed save");
    assert.match(page.el("app-link-off-status").textContent, /Could not save settings: the box said no/);
    assert.equal(page.el("app-link-off").disabled, false);
    assert.equal(page.session.has("ftw.pairing.resume"), false);
  });

  it("changes nothing when turning off is cancelled", async () => {
    let saves = 0;
    const page = load({ confirmRestart: false, saveConfig: async () => { saves++; return {}; } });
    await settle();
    page.el("app-link-off").click();
    await settle();
    assert.equal(saves, 0);
    assert.equal(page.config.app_link.enabled, true);
    assert.equal(page.calls.some((c) => c.path === "/api/restart"), false);
  });

  it("shows while the app link is off, or saved and not yet running", async () => {
    const off = load({ saved: false, running: false });
    await settle();
    assert.equal(off.el("app-link-switch").hidden, false);
    assert.match(off.el("app-link-status").textContent, /Off/);

    const pending = load({ saved: true, running: false });
    await settle();
    assert.equal(pending.el("app-link-switch").hidden, false);
    assert.match(pending.el("app-link-status").textContent, /Save and restart FTW/);

    const stopping = load({ saved: false, running: true });
    await settle();
    assert.equal(stopping.el("app-link-switch").hidden, false);
  });
});

// Inside Home Assistant the page's origin is Home Assistant's ingress URL
// and the box's own port is closed. An address there would point nowhere,
// and a short code is traded at that address, so both stay off the page.
// The QR and the link go through FTW Cloud and work in both places.
describe("pairing inside Home Assistant", () => {
  const qr = { url: "https://app.ftw.energy/p#v3.a.b.c.d.e", expires_at_ms: Date.now() + 600000 };

  it("shows the QR and link without an address or a short code", async () => {
    const { el, html } = load({ ha: true, pairing: qr });
    await settle();
    assert.match(html, /id="app-link-code" disabled hidden>/, "the short-code button is offered in Home Assistant");
    assert.doesNotMatch(html, /choose Use a pairing code/);

    el("app-link-pair").click();
    await settle();
    const slot = el("app-link-slot");
    assert.doesNotMatch(slot.words(), /Box address/, "an address that cannot work from Home Assistant");
    const labels = slot.buttons().map((b) => b.textContent);
    assert.ok(!labels.some((l) => /read a code out/i.test(l)), `a short code is offered: ${labels.join(", ")}`);
    assert.ok(labels.includes("Copy pairing link"), "the link is missing");
    assert.ok(slot.children.some((c) => c.tagName === "a" && c.href === qr.url), "the open link is missing");
  });

  it("keeps the address and the short code outside Home Assistant", async () => {
    const { el, html } = load({ pairing: qr });
    await settle();
    assert.match(html, /id="app-link-code" disabled>/);
    assert.match(html, /choose Use a pairing code/);

    el("app-link-pair").click();
    await settle();
    const slot = el("app-link-slot");
    assert.match(slot.words(), /Box address: http:\/\/ftw\.local:8080/);
    assert.ok(slot.buttons().some((b) => /read a code out/i.test(b.textContent)));
  });
});
