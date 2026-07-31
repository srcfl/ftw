// Suggesting a driver back to the repository it came from.
//
// The gateway holds no GitHub token and needs none: GitHub accepts a
// pre-filled issue over a URL, and the operator is already signed in to their
// own browser. What matters is that the link carries enough context to act on
// without a conversation, and that it never opens a page GitHub will reject.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";

const source = readFileSync(new URL("./settings/tabs/devices.js", import.meta.url), "utf8");

function element(tag) {
  const listeners = new Map();
  let text = "";
  const el = {
    tag,
    children: [],
    className: "",
    disabled: false,
    style: {},
    dataset: {},
    href: "",
    type: "",
    spellcheck: true,
    value: "",
    addEventListener(name, handler) { listeners.set(name, handler); },
    click() { return listeners.get("click")?.({ target: el }); },
    appendChild(child) { el.children.push(child); return child; },
    insertBefore(child) { el.children.push(child); return child; },
    remove() {},
    querySelector(selector) {
      const wanted = selector.replace(/^\./, "");
      for (const child of el.children) {
        if (String(child.className).split(" ").includes(wanted)) return child;
        const nested = child.querySelector ? child.querySelector(selector) : null;
        if (nested) return nested;
      }
      return null;
    },
    querySelectorAll() { return []; },
  };
  // Setting textContent clears the children, the way the DOM does. A stub that
  // kept them let a test find a button from a view that had been replaced.
  Object.defineProperty(el, "textContent", {
    get() { return text; },
    set(value) { text = String(value); el.children.length = 0; },
    enumerable: true,
  });
  return el;
}

function load() {
  const opened = [];
  const window = {
    FTWSettings: { tabs: {} },
    open: (url) => { opened.push(url); },
  };
  vm.runInNewContext(source, {
    document: { createElement: element, getElementById: () => null },
    fetch: async () => ({ ok: true, json: async () => ({ running: false }) }),
    window,
  });
  return { api: window.FTWSettings.driverVersions, opened, window };
}

const SOURCE_BODY = {
  id: "sungrow",
  version: "1.5.0",
  filename: "sungrow.lua",
  source: "managed",
  sha256: "abc123def456",
  lua: "DRIVER = { id = \"sungrow\" }\n",
  repository_url: "https://github.com/srcfl/device-drivers/blob/main/drivers/lua/sungrow.lua",
};

function decodedBody(url) {
  return decodeURIComponent(new URL(url).searchParams.get("body"));
}

test("the suggestion opens an issue against the driver's own repository", () => {
  const { api } = load();
  const url = api.suggestUpstreamURL(SOURCE_BODY, "");

  assert.ok(url.startsWith("https://github.com/srcfl/device-drivers/issues/new"),
    "everything points at the repository the driver came from");
  const title = decodeURIComponent(new URL(url).searchParams.get("title"));
  assert.match(title, /sungrow/, "the driver is named before anyone opens it");
});

test("the issue carries what someone needs to act without asking", () => {
  const { api } = load();
  const body = decodedBody(api.suggestUpstreamURL(SOURCE_BODY, ""));

  // Which driver, which version, where the running copy came from, and the
  // hash it was based on — a maintainer can find the exact bytes from that.
  assert.match(body, /sungrow/);
  assert.match(body, /v1\.5\.0/);
  assert.match(body, /official/);
  assert.match(body, /abc123def456/);
});

test("an edit travels as a diff, which is what fits in a URL", () => {
  const { api } = load();
  const edited = SOURCE_BODY.lua + "-- fixed the SG battery registers\n";
  const body = decodedBody(api.suggestUpstreamURL(SOURCE_BODY, edited));

  assert.match(body, /```diff/, "so GitHub renders it as a change, not a file");
  assert.match(body, /\+-- fixed the SG battery registers/,
    "the suggestion is the work, not a description of it");
});

test("the diff carries the change and skips the rest of the driver", () => {
  const { api } = load();
  // A real driver is thousands of lines and a fix is a handful. Sending the
  // whole file put the URL past what GitHub accepts, so the code never
  // travelled at all.
  const before = Array.from({ length: 400 }, (_, i) => "line " + i).join("\n");
  const after = before.replace("line 200", "line 200 -- corrected");

  const diff = api.unifiedDiff(before, after);
  assert.match(diff, /-line 200$/m);
  assert.match(diff, /\+line 200 -- corrected/);
  assert.ok(!diff.includes("line 5"), "untouched regions are elided");
  assert.ok(diff.length < before.length / 4, "a small fix produces a small diff");

  // Three lines of context on each side, so a reviewer can see where it lands.
  assert.match(diff, / line 197/);
  assert.match(diff, / line 203/);
});

test("an unchanged file produces no diff at all", () => {
  const { api } = load();
  const body = decodedBody(api.suggestUpstreamURL(SOURCE_BODY, SOURCE_BODY.lua));
  assert.ok(!body.includes("```diff"), "nothing was changed, so there is nothing to show");
});

test("the read view can suggest without an edit", () => {
  const { api } = load();
  const body = decodedBody(api.suggestUpstreamURL(SOURCE_BODY, ""));

  // A driver that is wrong for your hardware is worth reporting even when you
  // have not written the fix.
  assert.ok(!body.includes("```lua"), "no empty code block when there is no edit");
  assert.match(body, /What I changed and why/);
});

// The editor is its own surface now, so the devices tab hands it the driver
// and a set of actions. These test that boundary: what the tab passes in.
function openEditorFor(api, panel, body) {
  api.renderSource(panel, body);
  findButton(panel, "Edit and try").click();
}

test("opening the editor hands it the driver and the actions it needs", () => {
  const { api, window } = load();
  const opened = [];
  window.FTWSettings.openDriverEditor = (driver, actions) => opened.push({ driver, actions });

  openEditorFor(api, element("div"), SOURCE_BODY);

  assert.equal(opened.length, 1, "the tab opens the editor rather than drawing one");
  const { driver, actions } = opened[0];
  assert.equal(driver.id, "sungrow");
  assert.equal(driver.lua, SOURCE_BODY.lua);
  // Provenance is resolved here, so the editor does not need to know the
  // three overlays exist.
  assert.match(driver.sourceLabel, /official/);
  for (const name of ["runDraft", "keepDraft", "revertDraft", "draftStatus", "lint", "suggest"]) {
    assert.equal(typeof actions[name], "function", `actions.${name} is missing`);
  }
});

test("a driver too large to prefill opens the issue anyway, and says so", () => {
  const { api, opened, window } = load();
  let actions = null;
  window.FTWSettings.openDriverEditor = (_driver, a) => { actions = a; };
  openEditorFor(api, element("div"), SOURCE_BODY);

  // GitHub rejects a URL past roughly 8k, and a real driver is tens of
  // kilobytes — so this is the normal case for an edit, not an edge case.
  const said = [];
  actions.suggest("x".repeat(api.maxSuggestionURL() * 3), (m) => said.push(m));

  assert.equal(opened.length, 1, "one tab, and it is not a page GitHub will reject");
  assert.ok(opened[0].length <= api.maxSuggestionURL(),
    "the oversized draft was dropped from the URL rather than sent");
  assert.ok(!decodedBody(opened[0]).includes("```diff"), "and the diff goes with it");
  assert.match(said.join(" "), /too large to prefill/,
    "silently dropping the operator's edit would look like it was sent");
});

test("an edit that fits is carried in full", () => {
  const { api, opened, window } = load();
  let actions = null;
  window.FTWSettings.openDriverEditor = (_driver, a) => { actions = a; };
  openEditorFor(api, element("div"), SOURCE_BODY);

  actions.suggest(SOURCE_BODY.lua + "-- read holding 5017 instead of 5016\n", () => {});
  assert.match(decodedBody(opened[0]), /read holding 5017 instead of 5016/);
});

function findButton(el, label) {
  for (const child of el.children) {
    if (child.tag === "button" && child.textContent === label) return child;
    const nested = findButton(child, label);
    if (nested) return nested;
  }
  return null;
}
