// An operator picking a driver must be able to see how well tested it is.
//
// The signed channel now carries 80 drivers: 37 that have run on customer
// sites for months, and 43 that nobody has ever put on hardware. Both appear
// in the same dropdown. Without the distinction on screen, choosing between
// them is guesswork, and the untested ones look exactly as trustworthy as the
// proven ones.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const source = readFileSync(join(here, "setup.js"), "utf8");

test("verification status is turned into words, not shown as a raw enum", () => {
  assert.match(source, /function verificationLabel\(status\)/,
    "the vocabulary belongs in one function so the dropdown and the " +
    "description cannot disagree about the same driver");

  for (const [status, wording] of [
    ["production", "verified on hardware"],
    ["beta", "in testing"],
    ["experimental", "untested"],
  ]) {
    assert.ok(source.includes(`'${status}'`),
      `verificationLabel must handle ${status}`);
    assert.ok(source.includes(wording),
      `${status} should read as "${wording}" rather than as the enum value`);
  }
});

test("the dropdown label carries the verification verdict", () => {
  const populate = source.slice(
    source.indexOf("function populateDriverDropdown"),
    source.indexOf("window.onDriverSelected"));
  assert.match(populate, /verificationLabel\(entry\.verification_status\)/,
    "each option must say how well tested that driver is");
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
  assert.match(selected, /verification_notes/,
    "a driver that explains its own testing should have that shown");
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
