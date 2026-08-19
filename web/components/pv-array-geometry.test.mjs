// node --test web/components/pv-array-geometry.test.mjs

import assert from "node:assert/strict";
import { describe, it } from "node:test";

import {
  DEFAULT_MODULE_W_PER_M2,
  DEFAULT_PACKING_FACTOR,
  angularDistanceDeg,
  arrayFromRing,
  compassName,
  faceAzimuthCandidates,
  flipAzimuthDeg,
  ratedWFromSlopeArea,
  planAreaM2,
  preferredAzimuthDeg,
  ridgeAzimuthDeg,
  slopeAreaM2,
} from "./pv-array-geometry.js";

// Fixtures are built with the standard ellipsoidal metres-per-degree series,
// deliberately *not* with the module's own spherical projection — a shape
// round-tripped through the code under test would agree with itself and prove
// nothing. The two disagree by roughly half a percent in area at this
// latitude, which is the known bias of a spherical Earth against WGS84 and is
// far below the precision anyone draws a roof with.
const STOCKHOLM = { lat: 59.3293, lon: 18.0686 };

function metresPerDegree(latDeg) {
  const p = (latDeg * Math.PI) / 180;
  return {
    lat: 111132.92 - 559.82 * Math.cos(2 * p) + 1.175 * Math.cos(4 * p)
      - 0.0023 * Math.cos(6 * p),
    lon: 111412.84 * Math.cos(p) - 93.5 * Math.cos(3 * p) + 0.118 * Math.cos(5 * p),
  };
}

/** Build a WGS84 ring from local east/north offsets in metres. */
function ringFromMetres(offsets, origin) {
  const m = metresPerDegree(origin.lat);
  return offsets.map(([x, y]) => [origin.lon + x / m.lon, origin.lat + y / m.lat]);
}

/** Rotate local offsets counter-clockwise in the east/north plane. */
function rotate(offsets, degrees) {
  const r = (degrees * Math.PI) / 180;
  const c = Math.cos(r);
  const s = Math.sin(r);
  return offsets.map(([x, y]) => [x * c - y * s, x * s + y * c]);
}

// 10 m along the ridge (east-west) by 6 m down the slope.
const RECT_10x6 = [[-5, -3], [5, -3], [5, 3], [-5, 3]];

describe("plan area", () => {
  it("recovers the drawn size in square metres", () => {
    const ring = ringFromMetres(RECT_10x6, STOCKHOLM);
    const area = planAreaM2(ring);
    assert.ok(Math.abs(area - 60) < 0.6, `area ${area} should be ~60 m²`);
  });

  it("does not care whether the ring repeats its first point", () => {
    const ring = ringFromMetres(RECT_10x6, STOCKHOLM);
    const closed = [...ring, ring[0]];
    assert.ok(Math.abs(planAreaM2(ring) - planAreaM2(closed)) < 1e-9);
  });

  it("is unsigned, so winding order cannot produce a negative roof", () => {
    const ring = ringFromMetres(RECT_10x6, STOCKHOLM);
    assert.ok(Math.abs(planAreaM2(ring) - planAreaM2([...ring].reverse())) < 1e-9);
  });

  it("treats a shape with no area as no array", () => {
    assert.equal(planAreaM2([]), 0);
    assert.equal(planAreaM2([[18, 59], [18.001, 59]]), 0);
    assert.equal(arrayFromRing([[18, 59], [18.001, 59]], {}), null);
  });
});

describe("orientation", () => {
  it("reads the ridge from the longest edge", () => {
    const ring = ringFromMetres(RECT_10x6, STOCKHOLM);
    // The 10 m edges run east-west: a ridge bearing of 90°.
    assert.ok(Math.abs(ridgeAzimuthDeg(ring) - 90) < 0.5);
  });

  it("offers both faces the outline permits, and no others", () => {
    const ring = ringFromMetres(RECT_10x6, STOCKHOLM);
    const [a, b] = faceAzimuthCandidates(ring).map(Math.round);
    assert.deepEqual([a, b].sort((x, y) => x - y), [0, 180]);
  });

  it("defaults to the equatorward face, per hemisphere", () => {
    const north = ringFromMetres(RECT_10x6, STOCKHOLM);
    assert.ok(Math.abs(preferredAzimuthDeg(north, STOCKHOLM.lat) - 180) < 0.5);

    const south = ringFromMetres(RECT_10x6, { lat: -33.87, lon: 151.21 });
    const picked = preferredAzimuthDeg(south, -33.87);
    assert.ok(angularDistanceDeg(picked, 0) < 0.5, `expected ~0°, got ${picked}`);
  });

  it("follows the rectangle round as it turns", () => {
    // Turning the shape 30° counter-clockwise swings the ridge from 90° to
    // 60°, so the faces move with it: 150° and 330°, and south-ish wins.
    const ring = ringFromMetres(rotate(RECT_10x6, 30), STOCKHOLM);
    assert.ok(Math.abs(ridgeAzimuthDeg(ring) - 60) < 0.5);
    assert.ok(Math.abs(preferredAzimuthDeg(ring, STOCKHOLM.lat) - 150) < 0.5);
  });

  it("keeps the ridge a line rather than an arrow", () => {
    // Drawing the same rectangle the other way round is the same roof.
    const ring = ringFromMetres(RECT_10x6, STOCKHOLM);
    const reversed = ringFromMetres([...RECT_10x6].reverse(), STOCKHOLM);
    assert.ok(Math.abs(ridgeAzimuthDeg(ring) - ridgeAzimuthDeg(reversed)) < 0.5);
  });

  it("flips to the opposite face", () => {
    assert.equal(flipAzimuthDeg(180), 0);
    assert.equal(flipAzimuthDeg(0), 180);
    assert.equal(flipAzimuthDeg(270), 90);
    assert.equal(flipAzimuthDeg(350), 170);
  });

  it("measures the shorter way round the compass", () => {
    assert.equal(angularDistanceDeg(350, 10), 20);
    assert.equal(angularDistanceDeg(10, 350), 20);
    assert.equal(angularDistanceDeg(0, 180), 180);
  });
});

describe("tilt turns an outline into panel area", () => {
  it("leaves a flat roof alone", () => {
    assert.ok(Math.abs(slopeAreaM2(60, 0) - 60) < 1e-9);
  });

  it("recovers the area hidden by the slope", () => {
    // cos 60° = 0.5, so a 60 m² shadow is cast by 120 m² of roof.
    assert.ok(Math.abs(slopeAreaM2(60, 60) - 120) < 1e-9);
    // A 35° roof carries ~22 % more panel than its outline suggests.
    assert.ok(Math.abs(slopeAreaM2(60, 35) - 73.24) < 0.05);
  });

  it("stays finite at a wall, where there is no outline to trace", () => {
    assert.ok(Number.isFinite(slopeAreaM2(60, 90)));
  });

  it("means a steeper roof is a bigger array for the same drawing", () => {
    const ring = ringFromMetres(RECT_10x6, STOCKHOLM);
    const flat = arrayFromRing(ring, { latitude: STOCKHOLM.lat, tiltDeg: 0 });
    const steep = arrayFromRing(ring, { latitude: STOCKHOLM.lat, tiltDeg: 45 });
    assert.ok(steep.array.rated_w > flat.array.rated_w,
      `${steep.array.rated_w} should exceed ${flat.array.rated_w}`);
  });
});

describe("capacity", () => {
  it("uses the same basis as the roof model", () => {
    // 60 m² × 0.70 packing × 200 W/m² = 8400 W.
    assert.ok(Math.abs(ratedWFromSlopeArea(60) - 8400) < 1e-9);
    assert.equal(DEFAULT_PACKING_FACTOR, 0.7);
    assert.equal(DEFAULT_MODULE_W_PER_M2, 200);
  });

  it("honours an overridden packing factor", () => {
    assert.ok(Math.abs(ratedWFromSlopeArea(60, 0.5, 200) - 6000) < 1e-9);
  });
});

describe("the entry written back to config", () => {
  it("carries only the four fields weather.pv_arrays defines", () => {
    const ring = ringFromMetres(RECT_10x6, STOCKHOLM);
    const out = arrayFromRing(ring, { latitude: STOCKHOLM.lat });
    assert.deepEqual(
      Object.keys(out.array).sort(),
      ["azimuth_deg", "name", "rated_w", "tilt_deg"],
    );
  });

  it("describes a south-facing 35° roof from the drawing alone", () => {
    const ring = ringFromMetres(RECT_10x6, STOCKHOLM);
    const out = arrayFromRing(ring, { latitude: STOCKHOLM.lat });
    assert.equal(out.array.azimuth_deg, 180);
    assert.equal(out.array.tilt_deg, 35);
    assert.equal(out.array.name, "Roof south");
    assert.ok(Math.abs(out.planAreaM2 - 60) < 0.6);
    assert.ok(Math.abs(out.slopeAreaM2 - 73.2) < 0.6);
    assert.ok(Math.abs(out.array.rated_w - 10250) < 100);
    assert.deepEqual(out.azimuthCandidates.slice().sort((a, b) => a - b), [0, 180]);
  });

  it("lets an explicit azimuth override the guess", () => {
    const ring = ringFromMetres(RECT_10x6, STOCKHOLM);
    const out = arrayFromRing(ring, { latitude: STOCKHOLM.lat, azimuthDeg: 0 });
    assert.equal(out.array.azimuth_deg, 0);
    assert.equal(out.array.name, "Roof north");
  });

  it("names a flat roof for what it is", () => {
    assert.equal(compassName(180, 0), "Roof flat");
    assert.equal(compassName(90, 35), "Roof east");
    assert.equal(compassName(225, 35), "Roof south-west");
  });
});
