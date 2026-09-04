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

describe("roof buildings on the map", () => {
  const { whenSelected, buildingsBounds } = tab._pure;

  function footprint(distanceM, ring) {
    return {
      type: "Feature",
      geometry: { type: "Polygon", coordinates: [ring] },
      properties: { distance_m: distanceM },
    };
  }

  it("types the selected flag as a boolean, or MapLibre rejects the layer", () => {
    // A bare ["get", ...] case condition is value-typed; MapLibre then drops
    // the whole layer through its error event without throwing, and the map
    // silently stays empty.
    assert.deepEqual(whenSelected("#picked", "#other"), [
      "case", ["boolean", ["get", "selected"], false], "#picked", "#other",
    ]);
  });

  it("zooms to the near buildings plus the site, not the whole radius", () => {
    const bounds = buildingsBounds([
      footprint(40, [[18.06, 59.32], [18.07, 59.33], [18.06, 59.33], [18.06, 59.32]]),
      footprint(90, [[18.065, 59.325], [18.066, 59.326], [18.065, 59.326], [18.065, 59.325]]),
      footprint(120, [[18.068, 59.328], [18.069, 59.329], [18.068, 59.329], [18.068, 59.328]]),
      footprint(800, [[18.20, 59.40], [18.21, 59.41], [18.20, 59.41], [18.20, 59.40]]),
    ], 59.335, 18.05);
    assert.deepEqual(bounds, [[18.05, 59.32], [18.07, 59.335]]);
  });

  it("falls back to every footprint when almost nothing is near", () => {
    const bounds = buildingsBounds([
      footprint(800, [[18.20, 59.40], [18.21, 59.41], [18.20, 59.41], [18.20, 59.40]]),
      footprint(900, [[18.30, 59.42], [18.31, 59.43], [18.30, 59.43], [18.30, 59.42]]),
    ], 59.33, 18.07);
    assert.deepEqual(bounds, [[18.07, 59.33], [18.31, 59.43]]);
  });

  it("survives features with no usable geometry", () => {
    assert.equal(buildingsBounds([], undefined, undefined), null);
    assert.deepEqual(
      buildingsBounds([{ type: "Feature", properties: { distance_m: 5 } },
                       { type: "Feature", properties: { distance_m: 6 } },
                       { type: "Feature", properties: { distance_m: 7 } }], 59.33, 18.07),
      [[18.07, 59.33], [18.07, 59.33]],
    );
  });

  it("bakes theme colours to sRGB — MapLibre cannot parse oklch()", () => {
    assert.match(source, /getImageData\(0, 0, 1, 1\)/);
    assert.match(source, /"rgb\(" \+ px\[0\]/);
  });

  it("keeps a building click from dragging the site pin", () => {
    assert.match(
      source,
      /queryRenderedFeatures\(e\.point, \{ layers: \["roof-buildings-fill"\] \}\)/,
    );
  });

  it("retries the draw once the style has loaded", () => {
    assert.match(source, /map\.once\("load", drawBuildings\)/);
  });

  it("offers footprint drawing as the fallback for catalogs without buildings", () => {
    const html = tab.render(stubCtx());
    assert.ok(html.includes('id="roof-draw-footprint"'));
    assert.ok(html.includes("Drawing the footprint yourself is optional"));
    // The drawn ring must reach the derive, and beat a stale building pick.
    assert.match(source, /payload\.footprint = roofState\.drawnFootprint/);
    assert.match(source, /TerraDrawPolygonMode/);
    assert.match(source, /roofState\.drawnFootprint = null/);
  });

  it("derives at the same coordinates the picker searched", () => {
    // Both requests read the live form state; deriving against the stored
    // site while the pin has moved makes the picked building "not found
    // near this site".
    assert.match(source, /payload\.latitude = lat/);
    assert.match(source, /payload\.longitude = lon/);
    assert.match(source, /body: JSON\.stringify\(payload\)/);
  });
});
