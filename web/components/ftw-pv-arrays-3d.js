// <ftw-pv-arrays-3d> — tiny 3D preview of a site's PV-array config.
//
// Purpose: the settings Weather tab lets the operator list each PV
// plane (name + kWp + tilt_deg + azimuth_deg). Those four numbers
// fully specify a panel's orientation but they are hard to cross-
// check by eye on a phone in the shed. This component turns the
// list into a simple 3D scene — ground plane, compass, and one
// scaled / tilted / rotated rectangle per array, pivoted around a
// shared centre (the metaphorical middle of the roof). The operator
// drags to rotate, and the camera auto-fits so the whole set stays
// in frame at any array count.
//
// The component is self-contained: Three.js loads via the importmap
// declared in index.html ("three" + "three/addons/"); this file is
// lazy-imported from settings.js the first time the Weather tab
// opens, so the dashboard's main thread never pays for three.js on
// pages that don't touch the settings modal.
//
// Public API
// ----------
//   el.setArrays([{ name, kwp, tilt_deg, azimuth_deg }, ...])
//     Replaces the scene contents. Call whenever the list changes
//     — pass an empty array to clear.
//
// Azimuth convention matches the rest of the stack + the config
// help text in settings.js: 0 = N, 90 = E, 180 = S, 270 = W.
// Tilt: 0 = flat roof (panel horizontal), 90 = wall (panel
// vertical), typical pitched roof ≈ 35.

import { FtwElement } from "./ftw-element.js";
import * as THREE from "three";
import { OrbitControls } from "three/addons/controls/OrbitControls.js";

// Panel sizing — kWp → square edge in world units (metres, approximately).
// Real mono-Si panels land around 2 m² per kWp (3 panels of ~1.7 m² per
// kWp of string), so a 5 kWp array is ~10 m² ≈ 3.16 m × 3.16 m. We keep
// the visual aspect-ratio square (roof-layout independent) and only
// scale the edge by sqrt(kWp * area-per-kWp) so a 10 kWp array reads
// as ~√2 × the 5 kWp one, not 2×.
const AREA_PER_KWP = 2;             // m²/kWp, conservative average
const PANEL_EDGE_MIN = 1.4;         // floor so a 0.5 kWp array is still visible
const PANEL_EDGE_MAX = 7;           // cap so a 30 kWp array doesn't swallow the scene
const PANEL_ELEV    = 0.4;          // height above ground plane for the panel base

// Gap between adjacent panels along the orbit (world units). Small —
// the squares are the dominant visual, gap just keeps them from
// kissing at the closest angular separation.
const PANEL_GAP = 0.3;
// Minimum orbit radius even when a single small array is present —
// keeps the panel visibly distinct from the centre pivot sphere.
const MIN_ORBIT_R = 1.8;

// Panels whose azimuths differ by less than this are treated as the
// "same roof line" and laid out side-by-side (perpendicular to the
// cluster's mean azimuth) instead of spaced radially around the
// orbit. Spacing nearly-parallel arrays radially drives the required
// orbit radius to +Infinity as Δθ → 0, and visually they should
// read as neighbours on one roof anyway.
const PANEL_CLUSTER_DEG = 15;

// Ground plane half-size relative to the placement radius, so the
// ground always extends past the outer edge of the arrays by a
// healthy margin regardless of kWp totals.
const GROUND_MARGIN = 4;

class FtwPvArrays3d extends FtwElement {
  static styles = `
    :host {
      display: block;
      position: relative;
      width: 100%;
      /* Responsive height — phone portrait gets ~220 px, tablet gets
         ~300 px, desktop caps around 360 px. Uses clamp over viewport
         height so rotating the phone doesn't blow the viz to full-
         screen. */
      height: clamp(220px, 38vh, 360px);
      background: var(--ink-sunken);
      border: 1px solid var(--line);
      border-radius: var(--radius-md);
      overflow: hidden;
      cursor: grab;
    }
    :host(:active) { cursor: grabbing; }
    canvas { display: block; width: 100% !important; height: 100% !important; }
    .hint {
      position: absolute;
      left: 10px;
      bottom: 8px;
      font-family: var(--mono);
      font-size: 10px;
      letter-spacing: 0.08em;
      color: var(--fg-muted);
      pointer-events: none;
      text-transform: uppercase;
      opacity: 0.75;
    }
    .empty {
      position: absolute;
      inset: 0;
      display: grid;
      place-items: center;
      font-family: var(--mono);
      font-size: 11px;
      color: var(--fg-muted);
      pointer-events: none;
    }
  `;

  constructor() {
    super();
    this._arrays = [];
    this._three = null;
    this._raf = 0;
    this._ro = null;
    // Camera auto-fits on the first successful rebuild and
    // whenever the array count changes. Scrubbing kWp / tilt /
    // azimuth on a stable count keeps the operator's rotation/pan.
    this._cameraFitted = false;
    // Number of arrays at the last _rebuild(). A change in this
    // count is the signal we use to refit the camera even after
    // _cameraFitted — adding or removing arrays is a structural
    // change the operator expects the viewport to follow.
    this._prevCount = 0;
  }

  disconnectedCallback() {
    if (this._raf) cancelAnimationFrame(this._raf);
    this._raf = 0;
    if (this._ro) { this._ro.disconnect(); this._ro = null; }
    const t = this._three;
    if (t) {
      t.controls.dispose();
      t.renderer.dispose();
      t.scene.traverse((o) => {
        if (o.geometry) o.geometry.dispose();
        if (o.material) {
          const mats = Array.isArray(o.material) ? o.material : [o.material];
          mats.forEach((m) => { if (m.map) m.map.dispose(); m.dispose(); });
        }
      });
    }
    this._three = null;
  }

  // Public entry point. Arrays is the same shape as
  // config.weather.pv_arrays: [{ name, kwp, tilt_deg, azimuth_deg }, ...]
  setArrays(arrays) {
    this._arrays = Array.isArray(arrays) ? arrays.slice() : [];
    if (this._three) {
      this._rebuild();
    }
    this._updateEmptyHint();
  }

  render() {
    return `<div class="empty">configure at least one PV array to preview</div>
            <div class="hint">drag to rotate · scroll to zoom</div>`;
  }

  afterRender() {
    if (this._three) return;
    this._initThree();
    this._rebuild();
    this._startLoop();
    this._updateEmptyHint();
  }

  _updateEmptyHint() {
    const empty = this.shadowRoot.querySelector(".empty");
    if (empty) empty.style.display = this._arrays.length ? "none" : "grid";
  }

  _initThree() {
    const host = this;
    const rect = host.getBoundingClientRect();
    const w = Math.max(1, Math.round(rect.width));
    const h = Math.max(1, Math.round(rect.height));

    const scene = new THREE.Scene();
    scene.background = null; // stage's CSS bg shows through

    const camera = new THREE.PerspectiveCamera(45, w / h, 0.1, 500);
    camera.position.set(12, 10, 12);
    camera.lookAt(0, 0, 0);

    const renderer = new THREE.WebGLRenderer({ antialias: true, alpha: true });
    renderer.setPixelRatio(Math.min(window.devicePixelRatio, 2));
    renderer.setSize(w, h);
    host.shadowRoot.appendChild(renderer.domElement);

    // Orbit controls — left-drag rotates, middle-click (scroll-wheel
     // button) pans, right-drag dollies. Pan is enabled so a zoomed-
     // in operator can nudge the viewport when the auto-fit picked a
     // pose that hides a corner of their layout; clamps below stop
     // the camera from going below the ground plane.
    const controls = new OrbitControls(camera, renderer.domElement);
    controls.enableDamping = true;
    controls.dampingFactor = 0.08;
    controls.enablePan = true;
    controls.screenSpacePanning = true;
    controls.mouseButtons = {
      LEFT:   THREE.MOUSE.ROTATE,
      MIDDLE: THREE.MOUSE.PAN,
      RIGHT:  THREE.MOUSE.DOLLY,
    };
    // Middle-click on the canvas would otherwise trigger the
    // browser's auto-scroll cursor (the diamond pan icon that
    // scrolls the whole page) which fights OrbitControls' own pan.
    // preventDefault on mousedown blocks the auto-scroll without
    // interfering with the OrbitControls pan handler that runs
    // afterwards via its own capture listener.
    renderer.domElement.addEventListener("mousedown", (e) => {
      if (e.button === 1) e.preventDefault();
    });
    // Belt-and-braces: auxclick is the post-middle-click event
    // some browsers fire; blocking it kills any residual
    // click-to-toggle-autoscroll that slipped past mousedown.
    renderer.domElement.addEventListener("auxclick", (e) => {
      if (e.button === 1) e.preventDefault();
    });
    controls.minPolarAngle = 0.15;            // nearly top-down allowed
    controls.maxPolarAngle = Math.PI * 0.48;  // stop just above the ground
    controls.minDistance = 3;
    controls.maxDistance = 60;

    // Lighting — soft ambient + a warm sun from the south-east at ~45°
    // elevation. The sun direction is cosmetic; it just gives the
    // panels a side to catch that contrasts with the shaded side so
    // the tilt angle reads at a glance.
    scene.add(new THREE.AmbientLight(0xc7cfdd, 0.65));
    const sun = new THREE.DirectionalLight(0xffe8bf, 0.85);
    sun.position.set(12, 15, 6);
    scene.add(sun);

    // Ground plane. Size is refitted every _rebuild() based on the
    // outermost panel position, so a solo-array setup gets a tight
    // mat and a 6-array setup gets a correspondingly wider one.
    const groundGeo = new THREE.PlaneGeometry(20, 20);
    const groundMat = new THREE.MeshStandardMaterial({
      color: 0x24282f,
      roughness: 0.95,
      metalness: 0.0,
      transparent: true,
      opacity: 0.9,
    });
    const ground = new THREE.Mesh(groundGeo, groundMat);
    ground.rotation.x = -Math.PI / 2;
    ground.receiveShadow = false;
    scene.add(ground);

    // Light grid on the ground for scale cues — 2 m spacing.
    const grid = new THREE.GridHelper(20, 10, 0x4a5060, 0x353a44);
    grid.position.y = 0.01;
    scene.add(grid);

    // Compass rose — four labels pinned to N / E / S / W on the
    // ground plane edge. Labels are built as CanvasTextures so no
    // external font loading is needed. Each label is billboarded
    // (always faces the camera) via a per-frame update in the render
    // loop so rotating the scene doesn't bury a label in its own
    // backface.
    const compass = this._makeCompass();
    scene.add(compass);

    // Pivot marker — small ring at the roof centre. Kept tiny so it
    // doesn't fight the array rectangles for attention.
    const pivotGeo = new THREE.RingGeometry(0.35, 0.55, 32);
    const pivotMat = new THREE.MeshBasicMaterial({
      color: 0xffb85c, side: THREE.DoubleSide, transparent: true, opacity: 0.8,
    });
    const pivot = new THREE.Mesh(pivotGeo, pivotMat);
    pivot.rotation.x = -Math.PI / 2;
    pivot.position.y = 0.05;
    scene.add(pivot);

    const arrayRoot = new THREE.Group();
    scene.add(arrayRoot);

    const ro = new ResizeObserver(() => {
      const r = host.getBoundingClientRect();
      const w2 = Math.max(1, Math.round(r.width));
      const h2 = Math.max(1, Math.round(r.height));
      camera.aspect = w2 / h2;
      camera.updateProjectionMatrix();
      renderer.setSize(w2, h2);
    });
    ro.observe(host);
    this._ro = ro;

    this._three = { scene, camera, renderer, controls, ground, grid, compass, arrayRoot };
  }

  // Build a compass: four text sprites at the cardinal directions,
  // plus a thin colored stripe pointing North so the operator can
  // immediately see which way is up even before the labels resolve.
  _makeCompass() {
    const group = new THREE.Group();
    const dirs = [
      { label: "N", angle: 0,         color: "#ff7a7a" },
      { label: "E", angle: Math.PI/2, color: "#d7e1f0" },
      { label: "S", angle: Math.PI,   color: "#d7e1f0" },
      { label: "W", angle: Math.PI*1.5, color: "#d7e1f0" },
    ];
    // Radius is set relative to a default ground size; _rebuild
    // repositions these when the actual array spread is known.
    const r = 10;
    for (const d of dirs) {
      const sprite = makeLabelSprite(d.label, { color: d.color, canvasSize: 96 });
      // North = -Z, East = +X, South = +Z, West = -X.
      sprite.position.set(Math.sin(d.angle) * r, 0.2, -Math.cos(d.angle) * r);
      group.add(sprite);
      group.userData[d.label] = sprite;
    }
    // North stripe: a thin red line from the origin to the N edge,
    // sunk slightly so it hides under a PV panel's shadow if one
    // happens to sit at azimuth 0.
    const north = new THREE.Mesh(
      new THREE.PlaneGeometry(0.14, r),
      new THREE.MeshBasicMaterial({ color: 0xff7a7a, transparent: true, opacity: 0.7 }),
    );
    north.rotation.x = -Math.PI / 2;
    north.position.set(0, 0.015, -r / 2);
    group.add(north);
    group.userData.north = north;
    return group;
  }

  // Rebuild the array meshes + refit the ground plane + camera. Called
  // every setArrays() and once on first attach. Cheap enough to run
  // full-rebuild rather than diff — a typical site has < 6 arrays.
  _rebuild() {
    const t = this._three;
    if (!t) return;
    // Dispose old meshes. Detach via the public remove() API so the
    // child's .parent bookkeeping stays consistent (popping .children
    // directly leaves stale parent pointers — a Three.js footgun).
    // Also dispose any texture maps attached to sprite materials
    // (CanvasTexture from makeLabelSprite) — repeated setArrays()
    // calls would otherwise leak GPU memory per rebuild.
    const children = t.arrayRoot.children.slice();
    for (const c of children) {
      t.arrayRoot.remove(c);
      c.traverse((o) => {
        if (o.geometry) o.geometry.dispose();
        if (o.material) {
          const mats = Array.isArray(o.material) ? o.material : [o.material];
          mats.forEach((m) => {
            if (m.map) m.map.dispose();
            m.dispose();
          });
        }
      });
    }
    if (!this._arrays.length) {
      this._resizeGroundAndCompass(MIN_ORBIT_R + 2);
      if (!this._cameraFitted) {
        this._fitCamera(MIN_ORBIT_R + 2);
      }
      this._prevCount = 0;
      return;
    }

    // Compute each array's edge length (metres) from its kWp, with
    // soft min/max so pathologically small or large ratings don't
    // blow the scene out of scale.
    const panels = this._arrays.map((a) => {
      const kwp = Math.max(0.1, Number(a.kwp) || 0.1);
      const edge = Math.max(PANEL_EDGE_MIN,
        Math.min(PANEL_EDGE_MAX, Math.sqrt(kwp * AREA_PER_KWP)));
      // Normalize into [0, 360). Otherwise a user-entered 360 (or
      // a negative) would survive into the neighbour-collision math
      // below as Δθ = 360°, collapsing sin(Δθ/2) to 0 and blowing
      // needR to +Infinity — same failure mode as the single-panel
      // wraparound bug, via a different input path.
      const rawAz = Number.isFinite(a.azimuth_deg) ? a.azimuth_deg : 180;
      const azimuth = ((rawAz % 360) + 360) % 360;
      const tilt = Number.isFinite(a.tilt_deg) ? a.tilt_deg : 35;
      return { edge, azimuth, tilt, name: a.name || "" };
    });

    // Cluster panels by azimuth proximity. Panels within
    // PANEL_CLUSTER_DEG of their left-hand neighbour in the sorted
    // order belong to the same cluster and get laid out side-by-
    // side (perpendicular to the cluster's mean azimuth) rather
    // than spaced radially around the orbit. A long south-facing
    // house with three arrays at ~170°/180°/190° then reads as a
    // row along the ridge, instead of three panels sitting on top
    // of each other because Δθ is too small to separate radially.
    const sorted = panels.slice().sort((a, b) => a.azimuth - b.azimuth);
    const clusters = [];
    for (const p of sorted) {
      const cur = clusters[clusters.length - 1];
      const last = cur && cur.panels[cur.panels.length - 1];
      if (cur && (p.azimuth - last.azimuth) < PANEL_CLUSTER_DEG) {
        cur.panels.push(p);
      } else {
        clusters.push({ panels: [p] });
      }
    }
    // Wrap-around: if the first and last clusters are close across
    // the 360°/0° seam (e.g. panels at 355° and 5°), merge them.
    // Prepend the high-azimuth cluster's panels so left-to-right
    // order within the merged cluster follows increasing azimuth
    // modulo 360 (east-of-north on one side, west-of-north on the
    // other).
    if (clusters.length >= 2) {
      const first = clusters[0];
      const last = clusters[clusters.length - 1];
      const wrapGap = (first.panels[0].azimuth + 360) -
                      last.panels[last.panels.length - 1].azimuth;
      if (wrapGap < PANEL_CLUSTER_DEG) {
        first.panels = last.panels.concat(first.panels);
        clusters.pop();
      }
    }

    // Compute each cluster's aggregate geometry:
    //   - meanAz:   circular mean of its panels' azimuths.
    //   - totalW:   side-by-side row width along the tangent.
    //   - maxEdge:  depth along the radial (tilt footprint).
    //   - boundR:   bounding-circle radius used for cluster↔cluster
    //               collision math (sqrt((W/2)^2 + (e/2)^2)).
    for (const c of clusters) {
      const ps = c.panels;
      let sx = 0, sy = 0;
      for (const p of ps) {
        const rad = THREE.MathUtils.degToRad(p.azimuth);
        sx += Math.cos(rad); sy += Math.sin(rad);
      }
      c.meanAz = (THREE.MathUtils.radToDeg(Math.atan2(sy, sx)) + 360) % 360;
      c.totalW = ps.reduce((acc, p) => acc + p.edge, 0) +
                 (ps.length - 1) * PANEL_GAP;
      c.maxEdge = ps.reduce((m, p) => Math.max(m, p.edge), 0);
      const halfW = c.totalW / 2;
      const halfD = c.maxEdge / 2;
      c.boundR = Math.sqrt(halfW * halfW + halfD * halfD);
    }
    clusters.sort((a, b) => a.meanAz - b.meanAz);

    // Cluster↔cluster collision math. The chord between two cluster
    // centres on the orbit circle must exceed the sum of their
    // bounding-circle radii + gap:
    //   2·r·sin(Δθ/2) >= boundR_i + boundR_j + PANEL_GAP
    let needR = MIN_ORBIT_R;
    if (clusters.length === 1) {
      // No neighbour to clear — the cluster's own bounding radius
      // (which subsumes the single-panel case) determines the floor.
      needR = Math.max(MIN_ORBIT_R, clusters[0].boundR + PANEL_GAP);
    } else {
      for (let i = 0; i < clusters.length; i++) {
        const a = clusters[i];
        const b = clusters[(i + 1) % clusters.length];
        let dth = b.meanAz - a.meanAz;
        if (i === clusters.length - 1) dth += 360;
        const dRad = THREE.MathUtils.degToRad(Math.max(1, dth));
        const span = a.boundR + b.boundR + PANEL_GAP;
        const req = span / (2 * Math.sin(dRad / 2));
        if (req > needR) needR = req;
      }
    }
    const r = Math.max(needR, MIN_ORBIT_R);

    for (const c of clusters) {
      const mesh = this._buildCluster(c, r);
      t.arrayRoot.add(mesh);
    }

    this._resizeGroundAndCompass(r);
    // Auto-fit on the first successful rebuild, and again whenever
    // the array count changes. Scrubbing tilt / kWp / azimuth on a
    // stable count keeps the operator's camera pose; adding or
    // removing arrays is a structural change where they expect the
    // new layout to be centred (e.g. trimming 3 → 1 used to leave
    // the single remaining panel outside the fitted frustum).
    const countChanged = this._prevCount !== panels.length;
    if (!this._cameraFitted || countChanged) {
      this._fitCamera(r);
      this._cameraFitted = true;
    }
    this._prevCount = panels.length;
  }

  // Build a cluster group — a row of panels sharing a mean azimuth,
  // laid out side-by-side along the cluster's tangent to the orbit
  // circle. The outer Y-rotation (previously per-panel in
  // _buildPanel) lives here so the cluster orients as a unit; each
  // inner panel only needs a tangent-offset + tilt.
  _buildCluster(cluster, radius) {
    const meanRad = THREE.MathUtils.degToRad(cluster.meanAz);
    const group = new THREE.Group();
    // Rotate local +X to point toward the cluster's mean azimuth.
    // Three.js Y-rotation is CCW viewed from +Y looking down; our
    // N→E→S→W = CW-from-above, hence the -az + π/2.
    group.rotation.y = -meanRad + Math.PI / 2;

    // Distribute panels along local +Z (perpendicular to the radial
    // direction). zCursor runs from -totalW/2 to +totalW/2 so the
    // row is centred on the cluster's mean-azimuth spoke. Panels
    // are already sorted by azimuth, so lower-azimuth panels land
    // at -Z — which for a south cluster is world +X (east) — i.e.
    // east-to-west visual order matches east-to-west azimuth order.
    let zCursor = -cluster.totalW / 2;
    for (const p of cluster.panels) {
      zCursor += p.edge / 2;
      const panel = this._buildPanel(p, radius, zCursor);
      group.add(panel);
      zCursor += p.edge / 2 + PANEL_GAP;
    }
    return group;
  }

  _buildPanel(p, radius, zOffset) {
    const tRad = THREE.MathUtils.degToRad(p.tilt);
    const offset = zOffset || 0;

    // The panel is a single inner group positioned at
    //   (radius, PANEL_ELEV + tiltLift, zOffset)
    // inside the cluster group's already-azimuth-rotated frame.
    // Tilt is around local Z; that lifts the +X-local edge up (the
    // edge pointing "away from the pivot" in cluster-local space),
    // which after the cluster's Y rotation leans the normal in the
    // cluster's mean-azimuth direction. Panels whose azimuths differ
    // from the mean but stay within PANEL_CLUSTER_DEG are drawn
    // tilted along the mean — a small visual fib that is
    // imperceptible within that window and keeps the code from
    // having to unwind per-panel azimuth deltas inside the cluster.
    //
    // Inner group positions + tilts the panel. The Y offset grows
    // with tilt so the panel's lower edge never clips through the
    // ground. At tilt=0 the panel sits at PANEL_ELEV; at tilt=90
    // (wall), its bottom corner would otherwise be at
    //   PANEL_ELEV − edge/2 ≈ 0.4 − 1.2 = −0.8
    // which buries it under the ground mat. Bumping Y by
    // edge/2 · sin(tilt) keeps the bottom at PANEL_ELEV regardless
    // of how steep the tilt gets, and the panel appears to "lift
    // off" for a wall-mount — visually correct + never clips.
    const tiltLift = (p.edge / 2) * Math.sin(tRad);
    const tiltGroup = new THREE.Group();
    tiltGroup.position.set(radius, PANEL_ELEV + tiltLift, offset);
    // Tilt: rotate around local Z so the panel's TOP surface (its
    // original +Y normal) tips to face local +X — which, after the
    // outer Y-rotation, is the azimuth direction. This matches the
    // physics: a south-facing panel at 35° tilt has its normal
    // leaning south, its *far* edge (south end at the eave) LOW
    // and its *near* edge (north end at the ridge) HIGH.
    //
    // Positive rotation around +Z takes +X→+Y, which would lift the
    // far edge UP and tilt the normal toward -X (away from the
    // azimuth) — inverted from what we want. Negating flips both
    // signs at once: far edge drops, near edge rises, normal tilts
    // toward +X = azimuth. Reported as "tilt appears inverted" in
    // the earlier draft.
    tiltGroup.rotation.z = -tRad;

    // Panel: a rectangle in the XZ plane (lay flat first, then the
    // tilt above rotates it). The panel's "top edge" (the one
    // leaning away from the pivot) is its +X-local edge.
    const color = panelColor(p.azimuth, p.tilt);
    const geo = new THREE.PlaneGeometry(p.edge, p.edge);
    const mat = new THREE.MeshStandardMaterial({
      color,
      roughness: 0.45,
      metalness: 0.2,
      side: THREE.DoubleSide,
      emissive: color,
      emissiveIntensity: 0.05,
    });
    const panel = new THREE.Mesh(geo, mat);
    panel.rotation.x = -Math.PI / 2; // lay flat on the tiltGroup's local plane
    tiltGroup.add(panel);

    // Outline — a slightly larger wireframe square around the panel
    // so tilt transitions read cleanly even when the material's
    // shading is subtle (e.g. panel facing the camera edge-on).
    const edges = new THREE.EdgesGeometry(geo);
    const line = new THREE.LineSegments(edges,
      new THREE.LineBasicMaterial({ color: 0xf2e5cf, transparent: true, opacity: 0.55 }));
    line.rotation.x = -Math.PI / 2;
    line.position.y = 0.002; // anti-z-fight
    tiltGroup.add(line);

    // Name label if present. Amber pill (--accent-e style) with
    // near-black text — matches the FTW eyebrow palette in
    // DESIGN.md and reads from any rotation against either the
    // ground or the sky. Offset is kept tight to the panel's top
    // so the label lands right above the square instead of
    // floating far above where it loses its association.
    if (p.name) {
      const sprite = makeLabelSprite(p.name, {
        color: "#0a0a0a",
        bgColor: "#f5c45a",
        canvasSize: 72,
      });
      sprite.position.set(0, 0.15 + p.edge * 0.2, 0);
      tiltGroup.add(sprite);
    }

    tiltGroup.userData = { azimuth: p.azimuth, tilt: p.tilt,
                           edge: p.edge, name: p.name };
    return tiltGroup;
  }

  _resizeGroundAndCompass(radius) {
    const t = this._three;
    const outer = radius + GROUND_MARGIN;
    // Ground + grid scale as a ratio of their original size (both
    // built at size 20). Rescaling keeps the grid's 2 m cell size
    // constant per world unit rather than per screen unit, which
    // preserves the "scale cue" role of the grid lines.
    const s = (outer * 2) / 20;
    t.ground.scale.set(s, s, s);
    t.grid.scale.set(s, 1, s);
    // Reposition compass labels to the edge + orient their north stripe.
    ["N", "E", "S", "W"].forEach((k, i) => {
      const sprite = t.compass.userData[k];
      const angle = i * Math.PI / 2;
      sprite.position.set(Math.sin(angle) * outer, 0.2, -Math.cos(angle) * outer);
    });
    const north = t.compass.userData.north;
    if (north) {
      north.geometry.dispose();
      north.geometry = new THREE.PlaneGeometry(0.14, outer);
      north.position.set(0, 0.015, -outer / 2);
    }
  }

  // Fit the camera so the outer orbit radius (+ a margin) is fully
  // visible at the current aspect. Uses perspective FOV math rather
  // than controls.fitSphere to keep compatibility with OrbitControls
  // (which doesn't ship a native fit method in the lean addons).
  _fitCamera(radius) {
    const t = this._three;
    const target = radius + GROUND_MARGIN * 0.4;
    const fov = THREE.MathUtils.degToRad(t.camera.fov);
    // Account for the aspect — at narrow portrait we need more room
    // vertically, so use the smaller FOV across the two dims.
    const vFov = fov;
    const hFov = 2 * Math.atan(Math.tan(fov / 2) * t.camera.aspect);
    const effectiveFov = Math.min(vFov, hFov);
    const dist = target / Math.sin(effectiveFov / 2);
    // Default pose: camera at southeast (azimuth 135°), looking
    // northwest toward the origin, at ~26° above the horizon. This
    // puts north at the far side of the frame and gives south-
    // facing panels their natural "tilted away from the viewer"
    // read — the standard orientation for PV layout review. The
    // operator can drag to their preferred pose afterwards.
    //
    // Azimuth convention (same as panels): N = 0 → -Z, E = +X,
    // S = +Z, W = -X. Camera at SE = (+X, +Z). The orbit angle θ
    // is measured from +Z (south); θ = π/4 yields equal sin and
    // cos → equal east and south components → exact southeast.
    const elev = 0.45;
    const theta = Math.PI / 4;
    t.camera.position.set(
      dist * Math.sin(theta) * Math.cos(elev),
      dist * Math.sin(elev),
      dist * Math.cos(theta) * Math.cos(elev),
    );
    t.camera.lookAt(0, 0, 0);
    t.controls.target.set(0, 0, 0);
    t.controls.update();
  }

  _startLoop() {
    const t = this._three;
    const step = () => {
      this._raf = requestAnimationFrame(step);
      // Billboard the compass sprites so they always face the camera;
      // otherwise a back-side rotation would show them mirrored.
      ["N", "E", "S", "W"].forEach((k) => {
        const s = t.compass.userData[k];
        if (s) s.lookAt(t.camera.position);
      });
      t.controls.update();
      t.renderer.render(t.scene, t.camera);
    };
    this._raf = requestAnimationFrame(step);
  }
}

// Panel colour — hue from azimuth so south-facing arrays read warm
// and north-facing (rare, typically verification cases) read cool.
// Lightness slightly higher for near-flat panels (receive more light)
// and a touch darker for steeper tilts, so the visual "temperature"
// of the array pile quickly separates low-yield roofs from high-yield.
function panelColor(azimuth, tilt) {
  // Map azimuth → hue: 180° (south) → warm amber (40°), 0° (north)
  // → cool blue (220°). Use a cosine lerp so E and W (90° / 270°)
  // land mid-way.
  const a = ((azimuth % 360) + 360) % 360;
  const southness = 0.5 * (1 - Math.cos(THREE.MathUtils.degToRad(a)));
  // southness: 0 at N, 1 at S, 0.5 at E/W.
  const hue = 220 - southness * 180; // 220° (blue) → 40° (amber)
  const light = 0.55 - Math.min(0.15, tilt / 600); // 0.55 → 0.40 as tilt rises
  return new THREE.Color().setHSL(hue / 360, 0.55, light);
}

// Build a Sprite with a text label. When `bgColor` is provided the
// label renders as a filled rounded pill (amber FTW eyebrow look
// per DESIGN.md, near-black on-accent text) — used for the per-
// panel name chip so it reads from any rotation without blending
// into the panel colour. When `bgColor` is null, falls back to a
// transparent-background sprite (used by the compass cardinals).
function makeLabelSprite(text, opts = {}) {
  const color = opts.color || "#e8ecf4";
  const bgColor = opts.bgColor || null;
  const canvasSize = opts.canvasSize || 72;
  const fontPx = Math.round(canvasSize * 0.6);

  // Measure text first so the canvas (and therefore the sprite's
  // aspect ratio) matches the label content instead of hard-wrapping
  // at a fixed 3× width.
  const measureCtx = document.createElement("canvas").getContext("2d");
  measureCtx.font = `600 ${fontPx}px 'JetBrains Mono', ui-monospace, Menlo, monospace`;
  const textW = Math.max(canvasSize, Math.ceil(measureCtx.measureText(text).width) + canvasSize * 0.8);

  const canvas = document.createElement("canvas");
  canvas.width = Math.max(128, textW);
  canvas.height = canvasSize;
  const ctx = canvas.getContext("2d");
  ctx.clearRect(0, 0, canvas.width, canvas.height);

  if (bgColor) {
    // Rounded pill background — roundRect is Chromium/Safari/Firefox
    // all modern. If an older browser lands here it throws; fall
    // back to a plain rect in that case.
    ctx.fillStyle = bgColor;
    const pad = canvasSize * 0.12;
    const r = (canvas.height - pad * 2) / 2;
    if (ctx.roundRect) {
      ctx.beginPath();
      ctx.roundRect(pad, pad, canvas.width - pad * 2, canvas.height - pad * 2, r);
      ctx.fill();
    } else {
      ctx.fillRect(pad, pad, canvas.width - pad * 2, canvas.height - pad * 2);
    }
  }

  ctx.font = `600 ${fontPx}px 'JetBrains Mono', ui-monospace, Menlo, monospace`;
  ctx.textAlign = "center";
  ctx.textBaseline = "middle";
  ctx.fillStyle = color;
  ctx.fillText(text, canvas.width / 2, canvas.height / 2);

  const tex = new THREE.CanvasTexture(canvas);
  tex.colorSpace = THREE.SRGBColorSpace;
  // depthTest:false + high renderOrder keeps the label on top of
  // every panel, compass line, and ground element regardless of
  // camera angle. Without this, sprites were z-occluded by their
  // own panel when the camera looked up from ground level, and by
  // the opposite panel when looking across the scene from the
  // west. depthWrite stays false so the label doesn't write a
  // depth value other objects could depth-test against.
  const mat = new THREE.SpriteMaterial({
    map: tex,
    transparent: true,
    depthWrite: false,
    depthTest: false,
  });
  const s = new THREE.Sprite(mat);
  s.renderOrder = 999;
  // Sprite aspect = canvas aspect so text doesn't squash when names
  // differ in length. World scale picked so a 72px-tall canvas
  // renders at ~0.55 world-units tall at default camera distance.
  const worldH = 0.55;
  const worldW = worldH * (canvas.width / canvas.height);
  s.scale.set(worldW, worldH, 1);
  return s;
}

customElements.define("ftw-pv-arrays-3d", FtwPvArrays3d);
