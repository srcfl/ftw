import { readFileSync } from "node:fs";
import assert from "node:assert/strict";
import { describe, it } from "node:test";

const source = readFileSync(new URL("./weather.js", import.meta.url), "utf8");

globalThis.window = {};
await import("./weather.js");
const tab = globalThis.window.FTWSettings.tabs.weather;
const { arraysSummary } = tab._pure;

function stubCtx(weather) {
  return {
    config: { weather: weather || {} },
    field: (label, path) => "[field:" + path + "]",
    selectField: (label, path) => "[select:" + path + "]",
    help: () => "[?]",
    escHtml: (s) => String(s == null ? "" : s),
  };
}

describe("weather PV array nameplate", () => {
  it("binds Rated (W) to rated_w, not kwp", () => {
    assert.match(source, /Rated \(W\)/);
    assert.match(source, /data-field="rated_w"/);
    assert.match(source, /placeholder="12960"/);
    assert.doesNotMatch(source, /data-field="kwp"/);
    assert.match(source, /rated_w: 0/);
    assert.doesNotMatch(source, /kwp: 0/);
  });

  it("converts a leftover kwp into rated_w", () => {
    assert.match(source, /function ratedWattsFromLegacyKwp/);
    assert.match(source, /v >= 1000 \? v : v \* 1000/);
    assert.match(source, /function migrateArrayRatedW/);
    assert.match(source, /delete a\.kwp/);
  });
});

describe("weather household path", () => {
  it("summarizes measured solar vs existing arrays", () => {
    assert.equal(
      arraysSummary(0),
      "The solar production pattern is learned from measured solar.",
    );
    assert.equal(arraysSummary(1), "1 array set in config");
    assert.equal(arraysSummary(3), "3 arrays set in config");
  });

  it("keeps the map and keeps Add + orientation off the normal path", () => {
    const html = tab.render(stubCtx());
    const detailsAt = html.indexOf("<details");
    assert.ok(detailsAt > 0);
    const normal = html.slice(0, detailsAt);
    const rest = html.slice(detailsAt);
    assert.ok(normal.includes('id="weather-map"'));
    assert.ok(normal.includes("[field:weather.pv_rated_w]"));
    assert.ok(normal.includes("The solar production pattern is learned from measured solar."));
    assert.doesNotMatch(normal, /pv-array-add/);
    assert.doesNotMatch(normal, /\+ Add array/);
    assert.doesNotMatch(normal, /orientation/i);
    assert.match(rest, /<details class="engine-details"/);
    assert.doesNotMatch(html, /<details[^>]*\sopen\b/);
    assert.ok(rest.includes('id="pv-array-add"'));
    assert.ok(rest.includes("+ Add array"));
  });

  it("shows a read-only count when arrays already exist", () => {
    const html = tab.render(stubCtx({
      pv_arrays: [
        { name: "south", rated_w: 4000, tilt_deg: 35, azimuth_deg: 180 },
        { name: "east", rated_w: 2000, tilt_deg: 35, azimuth_deg: 90 },
      ],
    }));
    const normal = html.slice(0, html.indexOf("<details"));
    assert.ok(normal.includes("2 arrays set in config"));
    assert.doesNotMatch(normal, /pv-array-add/);
  });

  it("does not write observed peak AC into pv_rated_w", () => {
    assert.doesNotMatch(source, /pv_rated_w\s*=/);
    assert.doesNotMatch(source, /observed.*pv_rated_w|peak.*pv_rated_w/i);
  });
});
