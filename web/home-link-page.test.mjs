import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

const page = readFileSync(new URL("./home-link.html", import.meta.url), "utf8");

test("Home Link loads its design tokens and renders a dashboard", () => {
  assert.match(page, /href="\/components\/theme\.css"/);
  assert.match(page, /href="\/style\.css/);
  assert.match(page, /href="\/app\.css/);
  assert.match(page, /src="\/components\/ftw-energy-flow\.js/);
  assert.match(page, /background:[\s\S]*var\(--ink\)/);
  assert.match(page, /color:\s*var\(--fg\)/);
  assert.match(page, /<ftw-energy-flow\b/);
  assert.match(page, /data-view="overview"/);
  assert.match(page, /data-view="energy"/);
  assert.match(page, /data-view="plan"/);
  assert.match(page, /data-view="system"/);
  assert.match(page, /id="remote-grid"/);
  assert.match(page, /id="remote-pv"/);
  assert.match(page, /id="remote-battery"/);
  assert.match(page, /id="remote-load"/);
  assert.match(page, /id="remote-soc"/);
});

test("Home Link renders typed values instead of raw JSON", () => {
  assert.doesNotMatch(page, /JSON\.stringify\(response/);
  assert.doesNotMatch(page, /<pre\b/);
  assert.match(page, /function renderOverview/);
  assert.match(page, /function renderPlan/);
  assert.match(page, /function renderAssets/);
  assert.match(page, /function renderHistory/);
});

test("Home Link puts remote values into text nodes", () => {
  assert.doesNotMatch(page, /\.innerHTML\s*=/);
  assert.match(page, /data\.textContent = value/);
  assert.match(page, /asset\.label \|\| asset\.asset_id/);
});

test("Home Link unlocks once and refreshes without another passkey", () => {
  assert.match(page, /id="home-link-unlock-button"/);
  assert.match(page, /await session\.authorize\(\)/);
  assert.match(page, /return session\.read\(scope, history\)/);
  assert.match(page, /setInterval\(function \(\)/);
  assert.match(page, /session\.isAuthorized\(\)/);
  assert.doesNotMatch(page, /navigator\.credentials\.get/);
});

test("Home Link remembers the home but exposes a clear forget action", () => {
  assert.match(page, /FTWHomeLinkSession\.resolveInvite\(sourceURL, storage\)/);
  assert.match(page, /id="home-link-forget"/);
  assert.match(page, /FTWHomeLinkSession\.forgetInvite\(storage\)/);
  assert.match(page, /history\.replaceState\(null, "", window\.location\.pathname\)/);
});

test("Home Link shows why a session or read failed", () => {
  // A swallowed reason left a relay-side rejection indistinguishable from an
  // offline gateway, so failures must carry their cause into the UI.
  assert.match(page, /function reason\(error\)/);
  assert.doesNotMatch(page, /\.catch\(function \(\) \{/);
  assert.match(page, /"Could not reach this home" \+ reason\(error\)/);
});
