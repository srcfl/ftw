// Choosing which driver version runs, and being able to change your mind.
//
// Every driver runs locally; they differ only in where the file came from. So
// what an operator needs is narrow: see what is running, see what else they
// could run, switch, and switch back when the new one misbehaves.
//
// These tests drive the real code with the payload the API really returns.
// The previous suite matched the source with regexes, which is how a picker
// that read the wrong field and rendered zero rows shipped green.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";

// The click handler returns nothing, so awaiting click() does not await the
// fetch chain behind it. Let the microtasks drain.
const settle = () => new Promise((resolve) => setImmediate(resolve));

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
    type: "",
    addEventListener(name, handler) { listeners.set(name, handler); },
    click() { return listeners.get("click")?.({ target: el }); },
    appendChild(child) { el.children.push(child); return child; },
    remove() {},
    // Real enough for markRunning, which finds the label and detail inside a
    // row this way. A stub that always answered null let a test pass while the
    // code under it did nothing at all.
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

// The whole subtree as one string, the way it reads on screen.
function textOf(el) {
  return [el.textContent, ...el.children.map(textOf)].filter(Boolean).join(" ");
}

function buttonsOf(el) {
  return el.children.flatMap((c) => (c.tag === "button" ? [c] : buttonsOf(c)));
}

// What /api/drivers/catalog will say after the switch under test.
let catalogEntry = {};

function load() {
  const calls = [];
  const window = { FTWSettings: { tabs: {} } };
  vm.runInNewContext(source, {
    document: { createElement: element, getElementById: () => null },
    fetch: async (path, options = {}) => {
      calls.push({ path, body: options.body ? JSON.parse(options.body) : null });
      // refreshSummary re-reads the catalog after a switch rather than editing
      // the old line, so the stub has to answer as the server would once the
      // switch has taken effect.
      if (path === "/api/drivers/catalog") {
        return { ok: true, json: async () => ({ entries: [catalogEntry] }) };
      }
      return { ok: true, json: async () => ({ status: "ok" }) };
    },
    window,
  });
  return { api: window.FTWSettings.driverVersions, calls };
}

// Exactly what GET /api/device_repository/drivers/{id}/versions answers with:
// a VersionCandidate carries the version on .driver, and .installed only when
// that version is already on disk.
const PAYLOAD = {
  driver_id: "ferroamp",
  installed: null,
  available: [
    {
      repository_id: "ftw-official",
      driver: {
        version: "1.1.1",
        sha256: "f825…",
        metadata: { verification_status: "experimental" },
      },
    },
    {
      repository_id: "ftw-official",
      driver: {
        version: "1.0.0",
        sha256: "aa11…",
        metadata: { verification_status: "production" },
      },
      installed: { version: "1.0.0", sha256: "aa11…", active: true },
    },
  ],
};

test("the channel payload produces one row per version", () => {
  const { api } = load();
  const rows = api.versionRows(PAYLOAD);

  assert.equal(rows.length, 2,
    "the version lives on candidate.driver, not on the candidate itself; " +
    "reading the outer object filters every row away and the panel renders empty");
  assert.equal(rows.map((r) => r.version).join(" "), "1.1.1 1.0.0");
});

test("a candidate already on disk is marked from .installed, not by matching strings", () => {
  const { api } = load();
  const [newer, running] = api.versionRows(PAYLOAD);

  assert.equal(newer.downloaded, false, "1.1.1 has no install record");
  assert.equal(running.downloaded, true);
  assert.equal(running.active, true, "and it is the one running");
});

test("each row carries how well tested that version is", () => {
  const { api } = load();
  const [newer, running] = api.versionRows(PAYLOAD);

  // Upgrading onto an untested driver is a decision, so it has to be visible
  // at the moment of choosing rather than only in the setup wizard.
  assert.equal(newer.verification, "untested");
  assert.equal(running.verification, "verified on hardware");
});

test("a version on disk the channel no longer lists is still offered", () => {
  const { api } = load();
  const rows = api.versionRows({
    installed: [{ version: "0.9.2", sha256: "bb22…", active: false }],
    available: [],
  });

  assert.equal(rows.map((r) => r.version).join(" "), "0.9.2",
    "an older version whose manifest entry was dropped is exactly what " +
    "someone reaches for when a new driver misbehaves");
  assert.equal(rows[0].downloaded, true);
});

test("the same version is not listed twice when it is both installed and offered", () => {
  const { api } = load();
  const rows = api.versionRows({
    installed: [{ version: "1.0.0", sha256: "aa11…", active: true }],
    available: PAYLOAD.available,
  });

  assert.equal(rows.map((r) => r.version).join(" "), "1.1.1 1.0.0");
});

test("the panel says what is running and what can be switched to", () => {
  const { api } = load();
  const panel = element("div");
  api.render(panel, "ferroamp", PAYLOAD, { runningVersion: "1.0.0" });

  const text = textOf(panel);
  assert.match(text, /v1\.1\.1/);
  assert.match(text, /untested/);
  assert.match(text, /running now/);
  assert.match(text, /verified on hardware/);
});

test("switching to a version that is not downloaded fetches it first", async () => {
  const { api, calls } = load();
  const panel = element("div");
  api.render(panel, "ferroamp", PAYLOAD, { runningVersion: "1.0.0" });

  const [useNewer] = buttonsOf(panel);
  assert.equal(useNewer.textContent, "Use this");
  useNewer.click();
  await settle();

  assert.equal(calls.length, 1);
  assert.equal(calls[0].path, "/api/device_repository/drivers/ferroamp/install",
    "a version that is not on disk has to come down from the channel");
  assert.equal(calls[0].body.version, "1.1.1");

  // POST /install answers 400 "repository_id or channel is required" without
  // it. A version on its own does not say who signed it.
  assert.equal(calls[0].body.repository_id, "ftw-official");
});

test("after switching, undo activates a previous version kept on disk", async () => {
  const { api, calls } = load();
  const panel = element("div");
  api.render(panel, "ferroamp", PAYLOAD, {
    runningVersion: "1.0.0", logicalPath: "drivers/ferroamp.lua",
  });

  buttonsOf(panel)[0].click();
  await settle();
  const undo = buttonsOf(panel).find((b) => b.textContent.startsWith("Undo"));

  // Trying a driver and putting the old one back is the loop that makes
  // testing safe. It must not need a second trip through the list.
  assert.ok(undo, "a switch has to be reversible from where it happened");
  assert.match(undo.textContent, /back to v1\.0\.0/);

  undo.click();
  await settle();
  assert.equal(calls[1].path, "/api/device_repository/drivers/ferroamp/activate",
    "1.0.0 is retained on disk, so switching back needs no network");
  assert.equal(calls[1].body.version, "1.0.0");

  // Labbing means going back and forth, so the switch has to be re-armed
  // rather than left greyed out until the panel is reopened.
  assert.equal(buttonsOf(panel)[0].disabled, false,
    "after undo, trying the other version again is one click");
});

test("undo goes to the bundled copy when that is what was running", async () => {
  const { api, calls } = load();
  const panel = element("div");
  // The running 1.0.0 is the copy shipped with the build. It is not an install
  // and cannot be activated by version, even though a managed artifact of the
  // same version may sit on disk from an earlier trial -- and those two files
  // can differ in whether the driver may control anything.
  api.render(panel, "ferroamp", { installed: [], available: [PAYLOAD.available[0]] }, {
    runningVersion: "1.0.0", runningSource: "bundled", logicalPath: "drivers/ferroamp.lua",
  });

  buttonsOf(panel)[0].click();
  await settle();
  const undo = buttonsOf(panel).find((b) => b.textContent.startsWith("Undo"));
  assert.ok(undo, "installing over the bundled driver is the first thing anyone does");
  assert.match(undo.textContent, /bundled driver/);

  undo.click();
  await settle();
  assert.equal(calls[1].path, "/api/device_repository/drivers/ferroamp/use_bundled");
  assert.equal(calls[1].body.logical_path, "drivers/ferroamp.lua");
});

test("undo does not mistake a same-version managed artifact for the bundled copy", async () => {
  const { api, calls } = load();
  const panel = element("div");
  // Bundled 1.0.0 runs, and a managed 1.0.0 is retained from an earlier trial.
  api.render(panel, "ferroamp", {
    installed: [{version: "1.0.0", sha256: "cc33…", repo_id: "ftw-official", active: false}],
    available: [PAYLOAD.available[0]],
  }, { runningVersion: "1.0.0", runningSource: "bundled", logicalPath: "drivers/ferroamp.lua" });

  buttonsOf(panel)[0].click();
  await settle();
  buttonsOf(panel).find((b) => b.textContent.startsWith("Undo")).click();
  await settle();

  assert.equal(calls[1].path, "/api/device_repository/drivers/ferroamp/use_bundled",
    "activating the managed 1.0.0 would restore a different file than the one that was running");
});

test("switching rewrites the summary line from the catalog, not from the old text", async () => {
  const { api } = load();
  const panel = element("div");
  const headlineEl = element("span");
  const detailEl = element("span");
  const readOnlyEl = element("span");
  readOnlyEl.style.display = "none";
  headlineEl.textContent = "v1.0.0";
  detailEl.textContent = "official, shipped with this build · verified on hardware";

  // 1.1.1 is telemetry-only where 1.0.0 was not. Editing the old line in place
  // would carry the wrong answer to "may this driver control anything".
  catalogEntry = {
    path: "drivers/ferroamp.lua", source: "managed", installed_version: "1.1.1",
    verification_status: "experimental", read_only: true,
  };

  api.render(panel, "ferroamp", PAYLOAD, {
    runningVersion: "1.0.0", runningSource: "bundled",
    logicalPath: "drivers/ferroamp.lua", headlineEl, detailEl, readOnlyEl,
  });

  buttonsOf(panel)[0].click();
  await settle();

  assert.equal(headlineEl.textContent, "v1.1.1");
  assert.equal(detailEl.textContent, "official · untested");
  assert.equal(readOnlyEl.style.display, "", "1.1.1 may only read");
});

test("undo rewrites the line back to what is running again", async () => {
  const { api } = load();
  const panel = element("div");
  const headlineEl = element("span");
  const detailEl = element("span");
  const readOnlyEl = element("span");

  catalogEntry = {
    path: "drivers/ferroamp.lua", source: "managed", installed_version: "1.1.1",
    verification_status: "experimental", read_only: true,
  };
  api.render(panel, "ferroamp", PAYLOAD, {
    runningVersion: "1.0.0", runningSource: "bundled",
    logicalPath: "drivers/ferroamp.lua", headlineEl, detailEl, readOnlyEl,
  });
  buttonsOf(panel)[0].click();
  await settle();

  // The server is back on the bundled copy, and the line follows it.
  catalogEntry = {
    path: "drivers/ferroamp.lua", source: "bundled", version: "1.0.0",
    verification_status: "production", read_only: false,
  };
  buttonsOf(panel).find((b) => b.textContent.startsWith("Undo")).click();
  await settle();

  assert.equal(headlineEl.textContent, "v1.0.0");
  assert.match(detailEl.textContent, /shipped with this build/);
  assert.equal(readOnlyEl.style.display, "none", "the bundled driver may control again");
});

test("the Update shortcut hides once its version is the one running", async () => {
  const { api } = load();
  const panel = element("div");
  const headlineEl = element("span");
  const updateEl = element("button");
  updateEl.dataset.version = "1.1.1";

  // No update left to offer once 1.1.1 is what runs.
  catalogEntry = {
    path: "drivers/ferroamp.lua", source: "managed", installed_version: "1.1.1",
    update_available: false,
  };
  api.render(panel, "ferroamp", PAYLOAD, {
    runningVersion: "1.0.0", runningSource: "bundled",
    logicalPath: "drivers/ferroamp.lua", headlineEl, updateEl,
  });

  buttonsOf(panel)[0].click();
  await settle();
  assert.equal(updateEl.style.display, "none",
    "otherwise it offers to install what is already installed");
});

test("the version that is running is not offered as a switch target", () => {
  const { api } = load();
  const panel = element("div");
  api.render(panel, "ferroamp", PAYLOAD, { runningVersion: "1.0.0", runningSource: "bundled" });

  const labels = buttonsOf(panel).map((b) => b.textContent);
  assert.equal(labels.join(" "), "Use this", "only 1.1.1 is a switch; 1.0.0 already runs");
});

test("a managed driver can always get back to the bundled copy", () => {
  const { api } = load();
  const panel = element("div");
  api.render(panel, "ferroamp", PAYLOAD, {
    runningVersion: "1.1.1", runningSource: "managed", logicalPath: "drivers/ferroamp.lua",
  });

  // /versions lists signed and retained artifacts; the bundled copy is not an
  // install and appears in neither. Without a row for it, an operator who
  // installed one channel version over a bundled driver and then closed this
  // panel has no way back at all.
  assert.match(textOf(panel), /the copy shipped with this build/);
});

test("the bundled row is not offered when the bundled copy is already running", () => {
  const { api } = load();
  const panel = element("div");
  api.render(panel, "ferroamp", PAYLOAD, { runningVersion: "1.0.0", runningSource: "bundled" });

  assert.ok(!/shipped with this build/.test(textOf(panel)),
    "switching to what is already running is not a choice");
});

test("switching to the bundled copy uses its own endpoint, which refuses when there is none", async () => {
  const { api, calls } = load();
  const panel = element("div");
  api.render(panel, "ferroamp", PAYLOAD, {
    runningVersion: "1.1.1", runningSource: "managed", logicalPath: "drivers/ferroamp.lua",
  });

  const bundled = buttonsOf(panel).at(-1);
  bundled.click();
  await settle();

  // Not /rollback: that steps between managed artifacts and would deactivate
  // the only active row, leaving its own recovery path nothing to restore.
  assert.equal(calls[0].path, "/api/device_repository/drivers/ferroamp/use_bundled");
  assert.equal(calls[0].body.logical_path, "drivers/ferroamp.lua");
});

test("only one row claims to be running", async () => {
  const { api } = load();
  const panel = element("div");
  api.render(panel, "ferroamp", PAYLOAD, {
    runningVersion: "1.0.0", runningSource: "bundled", logicalPath: "drivers/ferroamp.lua",
  });

  assert.match(textOf(panel), /running now/);
  buttonsOf(panel)[0].click();
  await settle();

  // 1.0.0 kept its "running now" while 1.1.1 said it was running too, so the
  // panel contradicted itself until it was reopened.
  const running = textOf(panel).split(" ").join(" ").match(/running now/g) || [];
  assert.equal(running.length, 1, "exactly one row runs at a time");
});

test("switching to the bundled copy corrects the line above the panel", async () => {
  const { api } = load();
  const panel = element("div");
  const headlineEl = element("span");
  const detailEl = element("span");
  headlineEl.textContent = "v1.1.1";
  detailEl.textContent = "official · untested";

  catalogEntry = {
    path: "drivers/ferroamp.lua", source: "bundled", version: "1.0.0",
    verification_status: "production",
  };
  api.render(panel, "ferroamp", PAYLOAD, {
    runningVersion: "1.1.1", runningSource: "managed",
    logicalPath: "drivers/ferroamp.lua", headlineEl, detailEl,
  });

  buttonsOf(panel).at(-1).click();
  await settle();

  // This is not an undo -- it can be the first thing done after opening the
  // panel, so there is no earlier line to restore.
  assert.equal(headlineEl.textContent, "v1.0.0");
  assert.match(detailEl.textContent, /shipped with this build/);
});

test("an override downloads without claiming it will take over", async () => {
  const { api, calls } = load();
  const panel = element("div");
  api.render(panel, "ferroamp", PAYLOAD, { overridden: true, runningVersion: "1.0.0" });

  assert.match(textOf(panel), /Your own file runs while it is there/,
    "say why nothing here changes what runs");

  const buttons = buttonsOf(panel);
  assert.equal(buttons.map((b) => b.textContent).join(" "), "Download Downloaded",
    "an override shadows the channel, so 'Use this' would be a lie");
  assert.equal(buttons[1].disabled, true, "already on disk, nothing to fetch");

  buttons[0].click();
  await settle();
  assert.equal(calls[0].path, "/api/device_repository/drivers/ferroamp/install");
});

test("an empty history says so rather than rendering nothing", () => {
  const { api } = load();
  const panel = element("div");
  api.render(panel, "ferroamp", { installed: [], available: [] });

  assert.match(panel.textContent, /No versions found for this driver/,
    "a silently empty panel looks like a broken request");
});

test("what is running reads as words, not as an enum", () => {
  const { api } = load();

  const managed = api.runningSummary({
    source: "managed", installed_version: "1.1.1", verification_status: "production",
  });
  assert.equal(managed.headline, "v1.1.1");
  assert.equal(managed.detail, "official · verified on hardware");

  const bundled = api.runningSummary({
    source: "bundled", version: "1.0.0", verification_status: "experimental",
  });
  assert.equal(bundled.headline, "v1.0.0");
  assert.equal(bundled.detail, "official, shipped with this build · untested");

  // An operator's own file has no version the channel would recognise, so
  // naming one would read as provenance it does not have. Point at the file
  // instead, which is what they need to edit or delete.
  const own = api.runningSummary({
    source: "local", version: "local", path: "drivers/ferroamp.lua",
  });
  assert.equal(own.headline, "your own file");
  assert.equal(own.detail, "drivers/ferroamp.lua");
});

test("an override is not offered an Update button", () => {
  const { api } = load();

  const managed = api.runningSummary({
    source: "managed", version: "1.0.0", update_available: true,
    repository_id: "ftw-official", upstream_version: "1.1.1",
  });
  assert.equal(managed.updatable, true);

  // Installing a channel version while a local file is present changes
  // nothing: the local file still wins.
  const overridden = api.runningSummary({
    source: "local", version: "local", update_available: true,
    repository_id: "ftw-official", upstream_version: "1.1.1",
  });
  assert.equal(overridden.updatable, false);
});

test("manifest data and driver source become text, never markup", () => {
  const { api } = load();
  // Everything between these two builds DOM from remote or operator-supplied
  // data: version strings from a signed manifest, and the Lua of a driver an
  // operator may have written themselves.
  const builders = source.slice(
    source.indexOf("function renderVersionPicker"),
    source.indexOf("function offerUndo"));

  // Assignment, not the word — a comment explaining why innerHTML is avoided
  // is not a use of it.
  assert.ok(!/innerHTML\s*=/.test(builders),
    "a signed manifest is still remote input, and a driver file is whatever " +
    "is on disk; neither may inject markup");
  assert.match(builders, /createElement\("button"\)/);
  assert.match(builders, /pre\.textContent = body\.lua/,
    "driver source is set as text so it renders as code, not as HTML");
});
