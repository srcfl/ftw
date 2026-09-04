// Turning a rectangle drawn on the map into a PV array.
//
// Drawing supplies two of the three numbers an array needs. Area comes from
// the shape; azimuth from how the shape is turned. Tilt cannot be seen from
// directly above at all, so it stays typed — and it is also what converts the
// drawn outline into real panel area, because what you trace on a map is the
// *horizontal projection* of a sloped rectangle, not the rectangle itself.
//
// Everything here is pure so it can be tested without a browser or a map.

export const DEFAULT_PACKING_FACTOR = 0.7;
export const DEFAULT_MODULE_W_PER_M2 = 200;
export const DEFAULT_TILT_DEG = 35;

// IUGG mean Earth radius. At the scale of one roof the radius matters far
// less than the flat-Earth approximation below, which is exact enough for a
// 20 m rectangle and meaningless across a county.
const EARTH_RADIUS_M = 6371008.8;
const DEG = Math.PI / 180;

// Beyond this a roof is a wall: cos(tilt) approaches zero and the plan-area
// division runs away. A wall has no horizontal projection to trace anyway, so
// clamping here bounds the arithmetic instead of returning Infinity.
const MAX_TILT_FOR_PROJECTION_DEG = 89;

function stripClosingVertex(ring) {
  if (ring.length > 1) {
    const first = ring[0];
    const last = ring[ring.length - 1];
    if (first[0] === last[0] && first[1] === last[1]) return ring.slice(0, -1);
  }
  return ring.slice();
}

/**
 * Project a WGS84 ring ([[lon, lat], …]) to metres about its own centroid.
 *
 * A local tangent plane, not a real projection: over one building the error
 * is well under the precision anyone draws with, and it avoids carrying a
 * projection library into the settings page.
 */
export function toLocalMetres(ring) {
  const pts = stripClosingVertex(ring || []);
  if (pts.length === 0) return [];
  let lon0 = 0;
  let lat0 = 0;
  for (const [lon, lat] of pts) {
    lon0 += lon;
    lat0 += lat;
  }
  lon0 /= pts.length;
  lat0 /= pts.length;
  const mPerDegLat = EARTH_RADIUS_M * DEG;
  const mPerDegLon = mPerDegLat * Math.cos(lat0 * DEG);
  return pts.map(([lon, lat]) => [(lon - lon0) * mPerDegLon, (lat - lat0) * mPerDegLat]);
}

/** Area of the drawn outline in m², as seen from above. */
export function planAreaM2(ring) {
  const p = toLocalMetres(ring);
  if (p.length < 3) return 0;
  let twiceArea = 0;
  for (let i = 0; i < p.length; i++) {
    const [x1, y1] = p[i];
    const [x2, y2] = p[(i + 1) % p.length];
    twiceArea += x1 * y2 - x2 * y1;
  }
  return Math.abs(twiceArea) / 2;
}

/** Compass bearing of a local vector, 0 = north, 90 = east. */
function bearingDeg(dx, dy) {
  return (((Math.atan2(dx, dy) / DEG) % 360) + 360) % 360;
}

/**
 * Direction of the ring's longest edge, as a line in [0, 180).
 *
 * For a panel rectangle that edge runs along the ridge, which is a line and
 * not an arrow — calling it "north" rather than "south" would be a
 * distinction the drawing does not contain.
 */
export function ridgeAzimuthDeg(ring) {
  const p = toLocalMetres(ring);
  if (p.length < 2) return null;
  let longest = 0;
  let bx = 0;
  let by = 0;
  for (let i = 0; i < p.length; i++) {
    const [x1, y1] = p[i];
    const [x2, y2] = p[(i + 1) % p.length];
    const dx = x2 - x1;
    const dy = y2 - y1;
    const len = Math.hypot(dx, dy);
    if (len > longest) {
      longest = len;
      bx = dx;
      by = dy;
    }
  }
  if (longest <= 0) return null;
  return bearingDeg(bx, by) % 180;
}

/** Shortest angle between two compass bearings, in degrees. */
export function angularDistanceDeg(a, b) {
  const d = Math.abs((((a - b) % 360) + 360) % 360);
  return d > 180 ? 360 - d : d;
}

/** Turn an azimuth to face the opposite way. */
export function flipAzimuthDeg(azimuthDeg) {
  return ((((azimuthDeg + 180) % 360) + 360) % 360);
}

/**
 * The two directions the face could point: perpendicular to the ridge, either
 * side of it. A flat outline genuinely does not say which.
 */
export function faceAzimuthCandidates(ring) {
  const ridge = ridgeAzimuthDeg(ring);
  if (ridge === null) return [];
  return [(ridge + 90) % 360, (ridge + 270) % 360];
}

/**
 * The candidate a panel is more likely to use: the equatorward one.
 *
 * This is a default, not a measurement. Both perpendiculars fit the drawing
 * equally well, so the UI offers a flip rather than pretending to know.
 */
export function preferredAzimuthDeg(ring, latitudeDeg) {
  const candidates = faceAzimuthCandidates(ring);
  if (candidates.length === 0) return null;
  const target = (latitudeDeg || 0) >= 0 ? 180 : 0;
  const [a, b] = candidates;
  return angularDistanceDeg(a, target) <= angularDistanceDeg(b, target) ? a : b;
}

/**
 * Real panel area from the traced outline.
 *
 * A sloped rectangle of area A casts a shadow of A·cos(tilt) on the map, so
 * recovering it divides that back out. A 35° roof carries about 22 % more
 * panel than its outline suggests, which is the difference between a
 * believable rating and a quietly low one.
 */
export function slopeAreaM2(planArea, tiltDeg) {
  const tilt = Math.min(Math.max(tiltDeg || 0, 0), MAX_TILT_FOR_PROJECTION_DEG);
  return planArea / Math.cos(tilt * DEG);
}

/** Installable DC capacity in watts for a roof area, matching the roof model's basis. */
export function ratedWFromSlopeArea(areaM2, packingFactor, moduleWPerM2) {
  const packing = packingFactor == null ? DEFAULT_PACKING_FACTOR : packingFactor;
  const wPerM2 = moduleWPerM2 == null ? DEFAULT_MODULE_W_PER_M2 : moduleWPerM2;
  return areaM2 * packing * wPerM2;
}

/** Human-readable face name, mirroring the roof model's naming. */
export function compassName(azimuthDeg, tiltDeg) {
  if (tiltDeg < 5) return "Roof flat";
  const points = [
    [0, "north"], [45, "north-east"], [90, "east"], [135, "south-east"],
    [180, "south"], [225, "south-west"], [270, "west"], [315, "north-west"],
    [360, "north"],
  ];
  let best = points[0];
  for (const p of points) {
    if (Math.abs(p[0] - azimuthDeg) < Math.abs(best[0] - azimuthDeg)) best = p;
  }
  return `Roof ${best[1]}`;
}

function round(value, places) {
  const factor = 10 ** places;
  return Math.round(value * factor) / factor;
}

/**
 * Everything a drawn rectangle says about one array.
 *
 * Returns the config-shaped entry separately from the measurements, so only
 * the four fields weather.pv_arrays actually defines are ever written back.
 */
export function arrayFromRing(ring, options) {
  const opts = options || {};
  const plan = planAreaM2(ring);
  if (!(plan > 0)) return null;
  const tiltDeg = opts.tiltDeg == null ? DEFAULT_TILT_DEG : opts.tiltDeg;
  const candidates = faceAzimuthCandidates(ring);
  const azimuth = opts.azimuthDeg == null
    ? preferredAzimuthDeg(ring, opts.latitude)
    : opts.azimuthDeg;
  const azimuthDeg = azimuth == null ? 180 : Math.round(azimuth);
  const slope = slopeAreaM2(plan, tiltDeg);
  return {
    array: {
      name: opts.name || compassName(azimuthDeg, tiltDeg),
      rated_w: Math.round(ratedWFromSlopeArea(slope, opts.packingFactor, opts.moduleWPerM2)),
      tilt_deg: tiltDeg,
      azimuth_deg: azimuthDeg,
    },
    planAreaM2: round(plan, 1),
    slopeAreaM2: round(slope, 1),
    azimuthCandidates: candidates.map((c) => Math.round(c)),
  };
}
