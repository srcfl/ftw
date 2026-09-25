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

// The panel's rows are told apart by what they are, not by their position:
// the release's own copy comes first and "Check for new versions" last.
function rowOf(panel, key) {
  return panel.children.find((c) => c.dataset && c.dataset.row === key);
}

function switchButtons(panel) {
  return buttonsOf(panel).filter((b) => b.textContent !== "Check for new versions");
}

// What /api/drivers/catalog will say after the switch under test.
let catalogEntry = {};

function load(mutationBody = { status: "ok", runtime_verified: true, restarted_drivers: ["p1"] }) {
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
      return { ok: true, json: async () => mutationBody };
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
  assert.match(text, /selected/);
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
  assert.match(undo.textContent, /this release's driver/);

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
  assert.equal(detailEl.textContent, "from the driver channel · untested");
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
  assert.match(detailEl.textContent, /this release/);
  assert.equal(readOnlyEl.style.display, "none", "the bundled driver may control again");
});

test("the version that is running is not offered as a switch target", () => {
  const { api } = load();
  const panel = element("div");
  api.render(panel, "ferroamp", PAYLOAD, { runningVersion: "1.0.0", runningSource: "bundled" });

  const labels = switchButtons(panel).map((b) => b.textContent);
  assert.equal(labels.join(" "), "Use this", "only 1.1.1 is a switch; 1.0.0 already runs");
});

test("a managed driver can always get back to the release's copy", () => {
  const { api } = load();
  const panel = element("div");
  api.render(panel, "ferroamp", { ...PAYLOAD, release_version: "1.0.2" }, {
    runningVersion: "1.1.1", runningSource: "managed", logicalPath: "drivers/ferroamp.lua",
  });

  // /versions lists signed and retained artifacts; the bundled copy is not an
  // install and appears in neither. Without a row for it, an operator who
  // installed one channel version over a bundled driver and then closed this
  // panel has no way back at all.
  const release = rowOf(panel, "release");
  assert.ok(release, "the release's copy has its own row");
  assert.equal(panel.children[0], release, "and it comes first");
  assert.match(textOf(release), /v1\.0\.2 this release/);
  assert.equal(buttonsOf(release).map((b) => b.textContent).join(" "), "Use this");
});

test("the release's row runs without a switch when its copy is running", () => {
  const { api } = load();
  const panel = element("div");
  api.render(panel, "ferroamp", { ...PAYLOAD, release_version: "1.0.2" }, { runningVersion: "1.0.2", runningSource: "bundled" });

  const release = rowOf(panel, "release");
  assert.match(textOf(release), /running now · this release/);
  assert.equal(buttonsOf(release).length, 0, "switching to what is already running is not a choice");
});

test("a driver the release does not carry gets no release row", () => {
  const { api } = load();
  const panel = element("div");
  api.render(panel, "ferroamp", PAYLOAD, { runningVersion: "1.1.1", runningSource: "managed" });
  assert.equal(rowOf(panel, "release"), undefined, "use_bundled would refuse; do not offer it");
});

test("switching to the bundled copy uses its own endpoint, which refuses when there is none", async () => {
  const { api, calls } = load();
  const panel = element("div");
  api.render(panel, "ferroamp", { ...PAYLOAD, release_version: "1.0.2" }, {
    runningVersion: "1.1.1", runningSource: "managed", logicalPath: "drivers/ferroamp.lua",
  });

  const bundled = buttonsOf(rowOf(panel, "release"))[0];
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

  assert.match(textOf(panel), /selected/);
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
  api.render(panel, "ferroamp", { ...PAYLOAD, release_version: "1.0.0" }, {
    runningVersion: "1.1.1", runningSource: "managed",
    logicalPath: "drivers/ferroamp.lua", headlineEl, detailEl,
  });

  buttonsOf(rowOf(panel, "release"))[0].click();
  await settle();

  // This is not an undo -- it can be the first thing done after opening the
  // panel, so there is no earlier line to restore.
  assert.equal(headlineEl.textContent, "v1.0.0");
  assert.match(detailEl.textContent, /this release/);
});

test("an override downloads without claiming it will take over", async () => {
  const { api, calls } = load();
  const panel = element("div");
  api.render(panel, "ferroamp", PAYLOAD, { overridden: true, runningVersion: "1.0.0" });

  assert.match(textOf(panel), /Your own file runs while it is there/,
    "say why nothing here changes what runs");

  const buttons = switchButtons(panel);
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
  assert.equal(managed.detail, "from the driver channel · verified on hardware");

  const bundled = api.runningSummary({
    source: "bundled", version: "1.0.0", verification_status: "experimental",
  });
  assert.equal(bundled.headline, "v1.0.0");
  assert.equal(bundled.detail, "this release · untested");

  // Whether an override stays across a Core update is part of what it is.
  const chosen = api.runningSummary({ source: "managed", version: "1.3.2", chosen: true, release_version: "1.3.3" });
  assert.equal(chosen.detail, "chosen, kept across updates · release has v1.3.3");
  const early = api.runningSummary({ source: "managed", version: "1.3.4", release_version: "1.3.3" });
  assert.equal(early.detail, "until a release has it · release has v1.3.3");

  // An operator's own file has no version the channel would recognise, so
  // naming one would read as provenance it does not have. Point at the file
  // instead, which is what they need to edit or delete.
  const own = api.runningSummary({
    source: "local", version: "local", path: "drivers/ferroamp.lua",
  });
  assert.equal(own.headline, "your own file");
  assert.equal(own.detail, "drivers/ferroamp.lua");
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

for (const result of [
  {status: "installed"},
  {runtime_verified: false, restarted_drivers: []},
  {runtime_verified: true, restarted_drivers: []},
  {runtime_verified: true, restarted_drivers: ["another-device"]},
]) {
  test(`a successful download needs proof for this runtime: ${JSON.stringify(result)}`, async () => {
    const {api} = load(result);
    const panel = element("div");
    api.render(panel, "ferroamp", PAYLOAD, {driverName: "p1", runningVersion: "1.0.0", runningSource: "bundled"});
    buttonsOf(panel)[0].click();
    await settle();
    assert.match(textOf(panel), /Installed. No running instance was verified/);
    assert.doesNotMatch(textOf(panel), /is running|fresh telemetry verified|Undo/);
  });
}

test("a filename change refreshes the summary from the verified target path", async () => {
  const {api, calls} = load({runtime_verified: true, restarted_drivers: ["p1"], logical_path: "drivers/esphome-dsmr.lua"});
  const panel = element("div");
  const headlineEl = element("span");
  headlineEl.textContent = "v1.0.2";
  catalogEntry = {id: "esphome-dsmr", path: "drivers/esphome-dsmr.lua", version: "1.0.3", source: "managed"};
  const pathChanges = [];
  const opts = {driverName: "p1", runningVersion: "1.0.2", runningSource: "bundled", logicalPath: "drivers/esphome_dsmr.lua", headlineEl, onPathChanged: (a,b) => pathChanges.push([a,b])};
  api.render(panel, "esphome-dsmr", {available: [{repository_id: "test", driver: {version: "1.0.3"}}]}, opts);
  buttonsOf(panel)[0].click();
  await settle();
  assert.match(headlineEl.textContent, /1.0.3/);
  assert.deepEqual(pathChanges, [["drivers/esphome_dsmr.lua", "drivers/esphome-dsmr.lua"]]);
  assert.ok(calls.some(c => c.path === "/api/drivers/catalog"));
});


test("a saved filename change tells the open Settings dialog to reload before Save", async () => {
  const {api} = load({runtime_verified: true, restarted_drivers: ["p1"], config_changed: true});
  const panel = element("div");
  api.render(panel, "ferroamp", PAYLOAD, {driverName: "p1", runningVersion: "1.0.0"});
  buttonsOf(panel)[0].click();
  await settle();
  assert.match(textOf(panel), /fresh telemetry verified. Settings changed. Close and reopen Settings before saving/);
});

test("a channel file of the release's version is listed only while it runs", () => {
  const { api } = load();
  const payload = { ...PAYLOAD, release_version: "1.1.1" };
  const rows = api.versionRows(payload);
  assert.deepEqual([...rows].map((r) => r.version), ["1.0.0"], "the release's row stands for 1.1.1");

  const running = api.versionRows({
    release_version: "1.0.0", installed: null, available: PAYLOAD.available,
  });
  assert.deepEqual([...running].map((r) => r.version), ["1.1.1", "1.0.0"],
    "a managed 1.0.0 that runs stays visible, or the panel hides what is running");
});

test("a beta version is marked and installs through the beta channel", async () => {
  const { api, calls } = load();
  const panel = element("div");
  api.render(panel, "ferroamp", {
    installed: null,
    available: [{ repository_id: "ftw-official-beta", channel: "beta", driver: { version: "1.2.0-beta.1", sha256: "bb22…" } }],
  }, { runningVersion: "1.0.0", runningSource: "managed", logicalPath: "drivers/ferroamp.lua" });

  const row = rowOf(panel, "v1.2.0-beta.1");
  assert.match(textOf(row), /beta/);
  buttonsOf(row)[0].click();
  await settle();
  assert.equal(calls[0].path, "/api/device_repository/drivers/ferroamp/install");
  assert.deepEqual(calls[0].body, { version: "1.2.0-beta.1", channel: "beta" });
});

test("the owner's choice is marked where the versions are", () => {
  const { api } = load();
  const panel = element("div");
  api.render(panel, "ferroamp", { ...PAYLOAD, release_version: "1.1.1", chosen_version: "1.0.0" }, {
    runningVersion: "1.0.0", runningSource: "managed", logicalPath: "drivers/ferroamp.lua",
  });
  assert.match(textOf(rowOf(panel, "v1.0.0")), /chosen, kept across updates/);
});

test("checking for new versions refreshes both channels and redraws the list", async () => {
  const refreshed = { ...PAYLOAD, available: [...PAYLOAD.available,
    { repository_id: "ftw-official-beta", channel: "beta", driver: { version: "1.2.0-beta.1", sha256: "bb22…" } }] };
  const { api, calls } = load(refreshed);
  const panel = element("div");
  api.render(panel, "ferroamp", PAYLOAD, { runningVersion: "1.0.0", runningSource: "managed", logicalPath: "drivers/ferroamp.lua" });

  buttonsOf(panel).find((b) => b.textContent === "Check for new versions").click();
  await settle();
  await settle();
  assert.equal(calls[0].path, "/api/device_repository/refresh");
  assert.equal(calls[1].path, "/api/device_repository/drivers/ferroamp/versions");
  assert.ok(rowOf(panel, "v1.2.0-beta.1"), "the new beta row is drawn");
});

test("each version links to what changed, and only to its GitHub source", () => {
  const { api } = load();
  const panel = element("div");
  api.render(panel, "goodwe", {
    logical_path: "drivers/goodwe.lua",
    release_version: "2.1.2",
    release_source: { repository: "https://github.com/srcfl/device-drivers", commit: "489c9373be1fb391d885b948bf736399b3e40f2e" },
    installed: null,
    available: [
      { repository_id: "ftw-official", channel: "stable", repository: "https://github.com/srcfl/device-drivers",
        driver: { version: "2.1.1", sha256: "0dc9…", filename: "goodwe.lua", source_commit: "f18ceef" } },
      { repository_id: "mine", channel: "stable", repository: "javascript:alert(1)",
        driver: { version: "2.0.0", sha256: "ee00…", filename: "goodwe.lua", source_commit: "f18ceef" } },
    ],
  }, { runningVersion: "2.1.2", runningSource: "bundled", logicalPath: "drivers/goodwe.lua" });

  const linkOf = (row) => row.children.find((c) => c.tag === "a");
  assert.equal(linkOf(rowOf(panel, "release")).href,
    "https://github.com/srcfl/device-drivers/commits/489c9373be1fb391d885b948bf736399b3e40f2e/drivers/lua/goodwe.lua");
  assert.equal(linkOf(rowOf(panel, "v2.1.1")).href,
    "https://github.com/srcfl/device-drivers/commits/f18ceef/drivers/lua/goodwe.lua");
  assert.equal(linkOf(rowOf(panel, "v2.1.1")).textContent, "What changed");
  assert.equal(linkOf(rowOf(panel, "v2.0.0")), undefined, "a source that is not GitHub gets no link");
});

test("after a switch, checking for new versions redraws what runs now", async () => {
  // install, refresh and versions all answer with this body in the stub.
  const body = { ...PAYLOAD, release_version: "1.0.0", runtime_verified: true, restarted_drivers: ["p1"] };
  const { api } = load(body);
  catalogEntry = { path: "drivers/ferroamp.lua", source: "managed", installed_version: "1.1.1" };
  const panel = element("div");
  api.render(panel, "ferroamp", body, {
    runningVersion: "1.0.0", runningSource: "bundled", logicalPath: "drivers/ferroamp.lua",
    headlineEl: element("span"), detailEl: element("span"),
  });
  assert.equal(buttonsOf(rowOf(panel, "release")).length, 0, "the release's copy runs at first");

  buttonsOf(rowOf(panel, "v1.1.1"))[0].click();
  await settle();
  buttonsOf(panel).find((b) => b.textContent === "Check for new versions").click();
  await settle();
  await settle();

  const release = rowOf(panel, "release");
  assert.doesNotMatch(textOf(release), /running now/, "1.1.1 runs now, not the release's copy");
  assert.equal(buttonsOf(release).map((b) => b.textContent).join(" "), "Use this",
    "the way back to the release's copy must stay after a redraw");
});
