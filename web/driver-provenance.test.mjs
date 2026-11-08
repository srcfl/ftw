// An operator picking a driver sees how many homes run that exact version.
// That count is all FTW says about a driver.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const source = readFileSync(join(here, "setup.js"), "utf8");

test("a count is turned into words", () => {
  assert.match(source, /function usageLabel\(count\)/,
    "the wording belongs in one function so the dropdown and the description cannot disagree");
  assert.ok(source.includes("1 home runs this version"));
  assert.ok(source.includes("homes run this version"));
  assert.ok(!source.includes("verified on hardware"), "no status label is left");
  assert.ok(!source.includes("untested"), "no status label is left");
});

test("the dropdown label carries the count", () => {
  const populate = source.slice(
    source.indexOf("function populateDriverDropdown"),
    source.indexOf("window.onDriverSelected"));
  assert.match(populate, /usageLabel\(entry\.run_by\)/);
});

test("the description says which version and where it came from", () => {
  const selected = source.slice(
    source.indexOf("window.onDriverSelected"),
    source.indexOf("prefillDriverConfig()", source.indexOf("window.onDriverSelected")));

  assert.match(selected, /installed_version \|\| selectedCatalog\.version/,
    "prefer the installed version over the catalog's, since they can differ");

  // Local override > signed channel > bundled is the order FTW resolves in.
  // An operator debugging a device needs to know which one is actually running.
  for (const source_kind of ["local", "managed", "bundled"]) {
    assert.ok(selected.includes(`'${source_kind}'`),
      `the description must distinguish a ${source_kind} driver`);
  }
  assert.match(selected, /usageLabel\(selectedCatalog\.run_by\)/);
});

test("a driver with nothing to say still hides the panel", () => {
  const selected = source.slice(source.indexOf("window.onDriverSelected"));
  assert.match(selected, /if \(lines\.length\) \{/,
    "an empty description must not leave an empty box on screen");
  assert.match(selected, /descEl\.style\.display = 'none'/,
    "the panel is hidden when there is nothing to put in it");
});

test("the description is built as elements, not by string concatenation", () => {
  const selected = source.slice(source.indexOf("window.onDriverSelected"));
  assert.ok(!/descEl\.innerHTML\s*=/.test(selected),
    "verification notes come from a driver file; setting innerHTML would " +
    "make them executable");
  assert.match(selected, /createElement\('div'\)/);
  assert.match(selected, /el\.textContent = line/);
});
