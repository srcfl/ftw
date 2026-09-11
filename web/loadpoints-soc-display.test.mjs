import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import vm from "node:vm";

// Runs loadpoints.js against a stub document and a canned
// /api/loadpoints answer, then reads the HTML it put in the grid.
// The SoC row is what these tests are about: one number, its source
// in words, no editor.
const source = readFileSync(new URL("./loadpoints.js", import.meta.url), "utf8");

function inertElement() {
  return {
    innerHTML: "",
    textContent: "",
    scrollTop: 0,
    scrollLeft: 0,
    classList: { contains: () => true },
    addEventListener() {},
    querySelector() { return null; },
    querySelectorAll() { return []; },
  };
}

async function renderCard(lp) {
  const grid = inertElement();
  const document = {
    readyState: "complete",
    body: inertElement(),
    scrollingElement: inertElement(),
    getElementById(id) { return id === "loadpoints-grid" ? grid : null; },
    addEventListener() {},
  };
  const fetch = (path) => {
    const body = path === "/api/loadpoints" ? { loadpoints: [lp] } : null;
    return Promise.resolve({ ok: true, json: () => Promise.resolve(body) });
  };
  const context = vm.createContext({
    document, fetch,
    setInterval() { return 1; }, clearInterval() {},
  });
  vm.runInContext(source, context, { filename: "loadpoints.js" });
  // fetchAll awaits two fetches and two json() calls; a macrotask
  // boundary is enough for all of them to settle.
  await new Promise((resolve) => setImmediate(resolve));
  return grid.innerHTML;
}

function socRow(html) {
  const m = html.match(/<span class="lp-cfg-key">SoC<\/span><span class="lp-cfg-val">(.*?)<\/span><\/div>/s);
  assert.ok(m, "SoC row must render");
  return m[1];
}

const base = { id: "garage", driver_name: "easee", plugged_in: true, current_soc: 0.72, vehicle_soc: 0.65 };

test("each soc_source is worded for an operator; the raw token never shows", async () => {
  const words = {
    inferred: "estimated",
    vehicle: "from the car",
    completed: "pinned after the car stopped asking",
  };
  for (const [token, phrase] of Object.entries(words)) {
    const row = socRow(await renderCard({ ...base, soc_source: token }));
    assert.match(row, new RegExp("^72\\.0% · " + phrase), token);
    assert.doesNotMatch(row, new RegExp("\\b" + token + "\\b"), token + " must not print raw");
  }
});

test("an unknown soc_source falls back to 'estimated' and is not echoed", async () => {
  const row = socRow(await renderCard({ ...base, soc_source: "bms_guess" }));
  assert.match(row, /^72\.0% · estimated/);
  assert.doesNotMatch(row, /bms_guess/);
});

test("the card shows current_soc alone, never vehicle_soc beside it", async () => {
  const row = socRow(await renderCard({ ...base, soc_source: "vehicle" }));
  assert.match(row, /72\.0%/);
  assert.doesNotMatch(row, /65\.0%/);
  assert.equal((row.match(/%/g) || []).length, 1, "exactly one percentage");
});

test("the inline editor is gone; a plugged-in car gets a hint to the EV card instead", async () => {
  const plugged = await renderCard({ ...base, soc_source: "inferred" });
  assert.doesNotMatch(plugged, /lp-soc-|Set SoC manually|✎/);
  assert.match(socRow(plugged), /<span class="lp-cfg-hint">Set in the EV card on the dashboard\.<\/span>/);

  const unplugged = await renderCard({ ...base, plugged_in: false, soc_source: "inferred" });
  assert.doesNotMatch(unplugged, /lp-soc-|✎|lp-cfg-hint/);
  assert.equal(socRow(unplugged), "72.0% · estimated");
});

test("no SoC at all renders a dash", async () => {
  const row = socRow(await renderCard({ ...base, current_soc: null, vehicle_soc: null, soc_source: "" }));
  assert.equal(row, "—");
});

test("driver, vehicle and loadpoint names stay escaped in the rendered card", async () => {
  const html = await renderCard({
    ...base,
    id: "<b>lp</b>",
    driver_name: "<img src=x onerror=alert(1)>",
    vehicle_driver: "<script>1</script>",
    vehicle_charging_state: "\"quoted\"",
    soc_source: "inferred",
  });
  assert.doesNotMatch(html, /<img |<script>|<b>lp<\/b>/);
  assert.match(html, /&lt;img src=x onerror=alert\(1\)&gt;/);
  assert.match(html, /&lt;script&gt;1&lt;\/script&gt;/);
  assert.match(html, /&quot;quoted&quot;/);
  assert.match(html, /data-lp-id="&lt;b&gt;lp&lt;\/b&gt;"/);
});
