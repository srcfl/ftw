// <ftw-energy-flow> — hero diagram for /next.
//
// Planet/sun layout. The HOUSE sits at the center (the sun), and every
// other device — PV inverters, batteries, grid, EV chargers, whatever
// future category — is a "planet" that orbits it. A planet declares
// which CORNER it belongs to:
//
//       top-left  (225°)       top-right (315°)
//                    ╲       ╱
//                     ╲     ╱
//                   ┌──HOUSE──┐
//                     ╱     ╲
//                    ╱       ╲
//     bottom-left (135°)       bottom-right (45°)
//
// Corners are hard-wired at 45° diagonals so the overall X is uniform
// no matter how many planets each corner holds. Adding a second (or
// third) planet at the same corner makes the earlier ones scoot aside:
// they cluster along an arc centered on that corner's anchor angle,
// same orbit radius, so every power beam stays the same length.
// When no planets report at a corner, a "no data" placeholder fills
// it so the X reads as complete even on first paint.
//
// Flow edges carry two simultaneous animations:
//   1. A dashed, blurred stroke whose dash-offset animates via CSS —
//      gives a "current flowing" feel without redrawing any DOM.
//   2. Particle circles riding the edge via a single rAF loop, with
//      damped-oscillator perpendicular motion so the spray looks
//      turbulent rather than a rotating screw. Speed scales with |kW|.
// Both effects are skipped when |kW| < 50 W so idle edges read as still.
//
// Update pattern — app.js calls `setReadings(...)` each status poll
// with a fully-resolved planet list. The component never introspects
// /api/status itself and has no knowledge of driver roles — all
// role→corner/color/sub-text logic lives in the caller.
//
//   flow.setReadings({
//     load: 1.2,
//     planets: [
//       { id: "grid",     corner: "bottom-left",  title: "GRID",
//         kw:  0.5, toHub: true,  color: "var(--red-e)", sub: "importing" },
//       { id: "pv-east",  corner: "top-left",     title: "SOLAR", name: "east",
//         kw:  3.1, toHub: true,  color: "var(--amber)", sub: "generating" },
//       { id: "bat-main", corner: "top-right",    title: "BATTERY",
//         kw:  2.2, toHub: false, color: "var(--cyan)",  sub: "charging", soc: 78 },
//       { id: "ev",       corner: "bottom-right", title: "EV CHARGER",
//         kw:  3.7, toHub: false, color: "var(--green-e)", sub: "charging" },
//     ],
//   });

import { FtwElement, ftwDebugDelay } from "./ftw-element.js";

// World width is fixed at 1000; CX is always at the center of whatever
// crop we render. The viewBox HEIGHT (and CY = H/2) are computed per
// render — they grow with the largest cluster size so the arc never
// pushes a planet outside the box. Keeping H dynamic is what lets the
// hero card grow when the user adds a second/third PV/battery/etc.
// FLOW_IDLE_W — single source of truth for the "small enough to call
// idle/balanced" threshold (in watts, magnitude). Used by:
//   - this component (beam activation, sub-label "idle / charging /
//     generating", aggregated-bubble greyscale, self-powered %)
//   - web/app.js per-planet object construction (mirrors via
//     window.FTW_FLOW_IDLE_W set below — non-module script, can't
//     import; falls back to the same literal if this module hasn't
//     loaded yet)
//
// Inclusive comparison everywhere: |kW| <= threshold ⇒ idle, strictly
// > threshold ⇒ active. So at exactly 42 W the planet is idle AND the
// beam is inactive — no mixed state at the boundary.
const FLOW_IDLE_W = 42;
const FLOW_IDLE_KW = FLOW_IDLE_W / 1000;
function isIdleKw(kw) { return Math.abs(kw) <= FLOW_IDLE_KW; }
if (typeof window !== "undefined") window.FTW_FLOW_IDLE_W = FLOW_IDLE_W;
export { FLOW_IDLE_W, FLOW_IDLE_KW, isIdleKw };

const W = 1000;
const CX = W / 2;
// Baseline height used when no cluster has more than one planet — also
// the floor for the dynamic computation so the diagram never shrinks
// below the single-device size.
const H_BASE = 580;

// localStorage key for the last-seen max cluster size. Used during the
// loading phase to reserve the final SVG height up front so the page
// doesn't shift ~150 px when the first /api/status response arrives
// on a multi-inverter setup.
const EF_SIZING_CACHE_KEY = "ftw-ef-sizing-n";
// localStorage key for the last-seen populated-corner map. Used by the
// loading skeleton so the placeholder silhouette matches the user's
// actual config — a Y-shape for no-EV setups, three corners for no-
// battery, full X for the all-corners case — instead of always
// painting four placeholder bubbles that the first /api/status
// response immediately tears down.
//
// Value shape: { "top-left": 2, "bottom-left": 1, … } — corner name
// to planet count. The count lets individual-mode skeletons pre-draw
// the right number of bubbles per corner (dual PV inverters render as
// two at top-left) so the skeleton lines up with the post-load layout.
// Legacy array form ["top-left","bottom-left"] is read-compatible
// (each entry treated as count = 1).
const EF_LAYOUT_CACHE_KEY = "ftw-ef-corners";
// localStorage key for the combined/individual aggregation choice.
// Persisted so reloads / new sessions restore the user's last pick
// instead of always defaulting to "combined".
const EF_AGGREGATED_CACHE_KEY = "ftw-ef-aggregated";

// Corner → anchor angle in screen coordinates (0°=east, 90°=south).
// Fixed at exactly 45° so the whole diagram reads as a uniform X
// regardless of how many planets live at any corner.
const CORNER_ANGLE = {
  "top-left":     -3 * Math.PI / 4, // 225°
  "top-right":    -Math.PI / 4,     // 315°
  "bottom-right":  Math.PI / 4,     //  45°
  "bottom-left":   3 * Math.PI / 4, // 135°
};
// Default title shown when a corner has no planets reporting yet.
// Keeps the first-paint X intact; updated as soon as drivers push.
const CORNER_PLACEHOLDER_TITLE = {
  "top-left":     "SOLAR",
  "top-right":    "BATTERY",
  "bottom-right": "EV CHARGER",
  "bottom-left":  "GRID",
};

// Each corner's partner across the vertical centerline. When a corner
// is populated but its partner is empty we collapse the populated
// anchor to the shared top/bottom axis (top-center for the upper pair,
// bottom-center for the lower pair) — turns a "PV + Grid only" setup
// into a clean vertical line rather than a diagonal, and a "PV +
// Battery + Grid" setup into a Y with Grid centered below the house.
const PARTNER_CORNER = {
  "top-left":     "top-right",
  "top-right":    "top-left",
  "bottom-left":  "bottom-right",
  "bottom-right": "bottom-left",
};
// Axis angles the populated side collapses to when its partner is empty.
const ANGLE_TOP_CENTER    = -Math.PI / 2;
const ANGLE_BOTTOM_CENTER =  Math.PI / 2;

// Role-native color used on placeholder bubbles during the loading
// phase so the dashed rings + role icons read as colored from the
// first paint instead of a gray "unknown" blob. Once real telemetry
// lands, the per-driver color (set by app.js) overrides this.
const CORNER_PLACEHOLDER_COLOR = {
  "top-left":     "var(--amber)",
  "top-right":    "var(--cyan)",
  "bottom-right": "var(--green-e)",
  "bottom-left":  "var(--red-e)",
};
const CORNER_PLACEHOLDER_ROLE = {
  "top-left":     "pv",
  "top-right":    "battery",
  "bottom-right": "ev",
  "bottom-left":  "grid",
};

// SVG icon per role, drawn at (0,0) in a ±10 unit box. The loading
// state renders this instead of the kW/title/sub text block — stroke
// and fill both use currentColor so the icon picks up the planet's
// accent (we set `color: <hex>` on the parent group). Only visible
// while the SVG carries the ef-loading / ef-fade-in class; the
// steady-state render hides .ef-icon via CSS.
const ROLE_ICON = {
  pv: `<circle r="2.4" fill="currentColor"/>
       <path d="M0 -10 V-6 M7 -7 L4.5 -4.5 M10 0 H6 M7 7 L4.5 4.5 M0 10 V6 M-7 7 L-4.5 4.5 M-10 0 H-6 M-7 -7 L-4.5 -4.5"
             stroke="currentColor" stroke-width="2" fill="none" stroke-linecap="round"/>`,
  battery: `<rect x="-7" y="-4.5" width="12" height="9" rx="1.5"
                  stroke="currentColor" stroke-width="2" fill="none"/>
            <rect x="5.5" y="-2" width="2.5" height="4" fill="currentColor" rx="0.6"/>
            <rect x="-5" y="-2.5" width="5" height="5" fill="currentColor" opacity="0.7"/>`,
  grid: `<path d="M-2 -9 L4 -1 L0.5 -1 L4 9 L-4 1 L-0.5 1 Z"
               fill="currentColor" stroke="none"/>`,
  ev: `<path d="M-3 -7 V-3 M3 -7 V-3 M-6 -3 H6 V1 A6 6 0 0 1 -6 1 Z"
             stroke="currentColor" stroke-width="2" fill="none"
             stroke-linejoin="round" stroke-linecap="round"/>
       <path d="M0 2 V8" stroke="currentColor" stroke-width="2"
             stroke-linecap="round"/>`,
  unknown: `<circle r="6" fill="none" stroke="currentColor" stroke-width="2"/>`,
};

class FtwEnergyFlow extends FtwElement {
  static styles = `
    :host {
      display: block;
      background: linear-gradient(180deg,
        var(--hero-bg-top) 0%,
        var(--hero-bg-bot) 100%);
      border: 1px solid var(--line);
      border-radius: var(--radius-lg);
      padding: 20px 28px;
      position: relative;
      overflow: hidden;
    }
    :host::before {
      content: '';
      position: absolute;
      inset: 0;
      background: radial-gradient(circle at 50% 46%,
        var(--hero-glow-a), transparent 60%);
      pointer-events: none;
    }
    .title {
      font-family: var(--mono);
      font-size: 16px;
      font-weight: 600;
      letter-spacing: 0.22em;
      text-transform: uppercase;
      color: var(--fg);
      text-align: center;
      padding: 2px 0;
      margin-top: 10px;
      margin-bottom: 48px;
      position: relative;
    }
    svg {
      width: 100%;
      height: calc(var(--efl-h-factor, 1) * 535px);
      display: block;
    }
    /* SVG text classes — font-size values are in viewBox units (the SVG
       scales with container width via preserveAspectRatio), so at narrow
       viewports the default sizes render too small. Media queries below
       bump them back into legible range on small screens. */
    .sv-node-title { font-family: var(--mono); font-size: 10px; font-weight: 500; letter-spacing: 0.08em; }
    .sv-node-value { font-family: var(--mono); font-size: 20px; font-weight: 700; font-variant-numeric: tabular-nums; letter-spacing: -0.01em; }
    .sv-node-sub   { font-family: var(--mono); font-size: 10px; letter-spacing: 0.04em; }
    .sv-hub-value  { font-family: var(--mono); font-size: 18px; font-weight: 700; font-variant-numeric: tabular-nums; }
    .sv-hub-label  { font-family: var(--mono); font-size: 9px; letter-spacing: 0.1em; }
    .sv-hub-sub    { font-family: var(--mono); font-size: 9px; letter-spacing: 0.06em; font-variant-numeric: tabular-nums; }
    .ef-clickable { cursor: pointer; outline: none; }
    .ef-clickable:focus-visible > circle { stroke-width: 3; filter: drop-shadow(0 0 4px var(--accent, #6cf)); }
    /* One dash cycle advances by exactly (dash + gap). The fwd/rev pair
       keeps direction declarative — we flip the animation-name, not the
       path, so swapping a source→sink edge (grid export, battery
       discharge) is a one-token change at render time. */
    @keyframes ef-dash-fwd { to { stroke-dashoffset: -48; } }
    @keyframes ef-spin { to { transform: rotate(360deg); } }
    .ring {
      transform-box: fill-box;
      transform-origin: center;
      animation: ef-spin 24s linear infinite;
    }
    /* Loading sequence — three phases gated by SVG classes that
       render() sets:
         1. ef-loading:  planets + sun scaled to 0.7 and rotating in
                         place like the hub's dashed ring. Text hidden;
                         a role icon shows instead.
         2. ef-fade-in:  (first render after the first setReadings).
                         Planets ease-in-out back up to 1.0 scale over
                         500 ms; icons cross-fade to text in 150 ms,
                         delayed until the grow finishes (500 ms →
                         650 ms). animation-fill-mode on both keeps
                         the final state sticky through the render.
         3. (no class):  steady state — default static visuals, no
                         animations, so the 2 s /api/status poll
                         re-renders don't re-trigger any of this. */
    .ef-icon { display: none; }
    svg.ef-loading .ef-icon,
    svg.ef-fade-in .ef-icon { display: block; }

    svg.ef-loading .ef-node text,
    svg.ef-loading .ef-hub text { display: none; }

    /* Planet/hub shrink to 0.7 × while loading but do NOT spin
       themselves — if the whole node spun, the role icon would tumble
       with it. Only the dashed loading ring inside each node rotates,
       which matches the existing hub .ring pattern. */
    svg.ef-loading .ef-node,
    svg.ef-loading .ef-hub {
      transform-box: fill-box;
      transform-origin: center;
      transform: scale(0.7);
    }

    /* Per-planet dashed loading ring. Always in the DOM so the fade-in
       render can target it; visibility gated to the loading state. Uses
       the same ef-spin keyframe as the permanent .ring, but 8× faster
       so it reads as an active spinner rather than the hub's ambient
       decoration. During loading the hub's own .ring also accelerates
       via the rule further down, keeping every spinning border in the
       scene in sync. */
    .ef-loading-ring { display: none; }
    svg.ef-loading .ef-loading-ring {
      display: block;
      transform-box: fill-box;
      transform-origin: center;
      animation: ef-spin 3s linear infinite;
    }
    svg.ef-loading .ring { animation-duration: 3s; }

    /* Aggregation toggle — sits centred at the bottom of the hero.
       Bottom-centre rather than upper-right so the position is stable
       across desktop and mobile (narrow viewports would have crowded
       the top-right cluster against the hero's title). Only rendered
       when the current readings have >1 planet in any corner, so
       single-inverter setups never see the control. Amber track
       (--accent-e) when ON per the shared design system: one accent, near-black
       on-accent fill, 999 px radius, mono eyebrow label at 0.18 em. */
    .ef-toggle {
      position: absolute;
      bottom: 14px;
      left: 50%;
      transform: translateX(-50%);
      display: inline-flex;
      align-items: center;
      gap: 8px;
      background: none;
      border: 0;
      cursor: pointer;
      z-index: 3;
      font-family: var(--mono);
      font-size: 10px;
      letter-spacing: 0.18em;
      text-transform: uppercase;
      font-weight: 500;
      color: var(--fg-muted);
      padding: 4px 6px;
      border-radius: 999px;
      transition: color 200ms ease;
    }
    .ef-toggle:hover { color: var(--fg-dim); }
    .ef-toggle:focus-visible {
      outline: 1px solid var(--accent-e);
      outline-offset: 2px;
    }
    .ef-toggle-track {
      position: relative;
      /* Sized so the 12 px thumb has exactly 2 px of track showing on
         each side (2 + 12 + 12-travel + 2 = 28 H-total minus the 1 px
         border per side). box-sizing: border-box so border math stays
         inside the declared dimensions — otherwise the track ends up
         18 px tall and the thumb floats visibly off-center. */
      box-sizing: border-box;
      width: 30px;
      height: 18px;
      background: var(--ink-sunken);
      border: 1px solid var(--line);
      border-radius: 999px;
      transition: background 220ms ease, border-color 220ms ease;
      flex-shrink: 0;
    }
    .ef-toggle-track::before {
      content: '';
      position: absolute;
      /* Positioned with explicit 2 px gaps on every side of the
         padding box (= inside the 1 px border). Track is 30×18 with
         border-box, so padding box is 28×16, and a 12 px thumb at
         top:2 + height:12 leaves exactly 2 px gap top + 2 px gap
         bottom. Uses explicit offsets instead of the translateY/50%
         centering trick because the percentage resolution against a
         border-box padding area introduced a 1 px sub-pixel drift
         the user could see. */
      top: 2px;
      left: 2px;
      width: 12px;
      height: 12px;
      border-radius: 999px;
      background: var(--fg-muted);
      transition: transform 220ms ease, background 220ms ease;
    }
    .ef-toggle[aria-checked="true"] { color: var(--fg); }
    .ef-toggle[aria-checked="true"] .ef-toggle-track {
      background: var(--accent-e);
      border-color: var(--accent-e);
    }
    .ef-toggle[aria-checked="true"] .ef-toggle-track::before {
      /* Travel = padding width (28) − left gap (2) − thumb (12) −
         right gap (2) = 12 px, landing the thumb with the same 2 px
         margin on the right that it had on the left in the OFF state. */
      transform: translateX(12px);
      background: #0a0a0a;
    }

    /* Layer fade — both aggregation modes live in the SVG at the same
       time; data-agg on the SVG toggles which one is visible. 300 ms
       ease gives a soft cross-fade; pointer-events follows opacity so
       clicks don't hit the invisible layer. transition on opacity is
       cheap and the invisible layer's particles keep running (RAF)
       but at opacity 0 they're not painted. */
    .ef-layer { transition: opacity 300ms ease; }
    svg[data-agg="on"]  .ef-layer-agg { opacity: 1; }
    svg[data-agg="on"]  .ef-layer-ind { opacity: 0; pointer-events: none; }
    svg[data-agg="off"] .ef-layer-agg { opacity: 0; pointer-events: none; }
    svg[data-agg="off"] .ef-layer-ind { opacity: 1; }

    @keyframes ef-node-grow {
      from { transform: scale(0.7); }
      to   { transform: scale(1); }
    }
    @keyframes ef-fade-in-opacity {
      from { opacity: 0; }
      to   { opacity: 1; }
    }
    @keyframes ef-fade-out-opacity {
      from { opacity: 1; }
      to   { opacity: 0; }
    }
    svg.ef-fade-in .ef-node,
    svg.ef-fade-in .ef-hub {
      transform-box: fill-box;
      transform-origin: center;
      animation: ef-node-grow 500ms ease-in-out both;
    }
    svg.ef-fade-in .ef-node text,
    svg.ef-fade-in .ef-hub text {
      animation: ef-fade-in-opacity 150ms ease-out 500ms both;
    }
    svg.ef-fade-in .ef-icon {
      animation: ef-fade-out-opacity 150ms ease-in 500ms both;
    }
    /* Desktop-only (>900px). No-EV "Y" layout centers Grid at the
       bottom; in combined mode the merged bubble sits right on the
       card edge and its lower arc clips. Extra 20px bottom padding
       fixes that. :has() reads the SVG's live data-agg (flipped by
       the toggle click handler without re-render), so this tracks
       combined↔individual without re-rendering. */
    @media (min-width: 901px) {
      :host([data-no-ev]:has(svg[data-agg="on"])) { padding-bottom: 40px; }
    }
    @media (max-width: 900px) {
      :host { padding: 20px 12px; }
      .title { margin-bottom: 8px; }
      svg { height: calc(var(--efl-h-factor, 1) * 510px); }
      .sv-node-title { font-size: 13px; }
      .sv-node-value { font-size: 24px; }
      .sv-node-sub   { font-size: 13px; }
      .sv-hub-value  { font-size: 22px; }
      .sv-hub-label  { font-size: 11px; }
      .sv-hub-sub    { font-size: 10px; }
    }
    @media (max-width: 600px) {
      svg { height: calc(var(--efl-h-factor, 1) * 460px); }
      .sv-node-title { font-size: 18px; }
      .sv-node-value { font-size: 30px; }
      .sv-node-sub   { font-size: 16px; }
      .sv-hub-value  { font-size: 28px; }
      .sv-hub-label  { font-size: 14px; }
      /* Hub sub-lines (% SELF-POWERED NOW / TODAY) need to stay
         readable but not crowd the 28px power value. Earlier spec
         was 14px; that competed with the headline number. 11px keeps
         the labels sharp without dominating. */
      .sv-hub-sub    { font-size: 11px; }
    }
  `;

  constructor() {
    super();
    // Start with empty clusters; render shows placeholder slots until the
    // first setReadings() push arrives from app.js.
    this._readings = { load: 0, planets: [] };
    // Flipped true on the first setReadings(); the SVG carries an
    // `ef-loading` class until then, which a CSS keyframe uses to pulse
    // every node + number while /api/status is still in flight. Matches
    // the skeleton shimmer used by the history cards so the whole hero
    // + card region reads as "waiting" on initial paint.
    // `_everFaded` caps the loading→loaded fade-in to exactly ONE
    // render. After that, the SVG renders without the fade class so
    // the 2 s /api/status poll doesn't blink the hero each cycle.
    this._loaded = false;
    this._everFaded = false;
    // Aggregation toggle — when true (default), corners that host
    // multiple planets (dual PV inverters, dual batteries, …) render
    // as a single summed bubble; when false, each planet draws in its
    // own slot. The button is only shown when at least one corner
    // actually has >1 planet. Persisted across renders AND across
    // reloads via localStorage; the fade is done in CSS by stacking
    // both layers in the SVG and toggling their opacity via data-agg
    // on the svg element.
    this._aggregated = true;
    try {
      const stored = localStorage.getItem(EF_AGGREGATED_CACHE_KEY);
      if (stored === "0") this._aggregated = false;
      else if (stored === "1") this._aggregated = true;
    } catch (e) {}
    // JS-driven particle system — one rAF loop animates every "electron"
    // independently. Each particle has its own amp/phase/freq/speed plus
    // a low-frequency 2D noise term, so even at high particle counts
    // the stream looks like a turbulent spray, not a threaded screw.
    this._rafId = null;
    this._particles = [];
    this._bound = [];
    this._snapshot = null;
    // Anchored once at construction so `t = now - tickStart` is on the
    // same timeline for the entire component lifetime. Resetting it
    // each afterRender would make restored bornAt values (from the
    // snapshot Map) refer to the old timeline — particles would jump.
    this._tickStart = performance.now();
    // Compact layout kicks in on narrow viewports — shortens beams so
    // the node boxes cluster closer to the hub, leaving more room for
    // the enlarged text. Kept in sync with the (max-width: 600px) CSS
    // breakpoint via matchMedia so fonts and geometry flip together.
    this._mq = typeof window !== "undefined" && window.matchMedia
      ? window.matchMedia("(max-width: 600px)")
      : null;
    this._compact = !!(this._mq && this._mq.matches);
    this._onMqChange = (e) => {
      this._compact = e.matches;
      this.update();
    };
    if (this._mq) {
      this._mq.addEventListener("change", this._onMqChange);
    }
    // Generic viewport listener — covers desktop window resizes and
    // device rotations that don't cross the 600px matchMedia threshold
    // (which `_mq` already handles). Throttled via rAF so a continuous
    // resize-drag triggers at most one re-render per frame.
    this._onResize = () => {
      if (this._resizeRaf) return;
      this._resizeRaf = requestAnimationFrame(() => {
        this._resizeRaf = 0;
        this.update();
      });
    };
    if (typeof window !== "undefined") {
      window.addEventListener("resize", this._onResize, { passive: true });
      window.addEventListener("orientationchange", this._onResize, { passive: true });
    }
  }

  disconnectedCallback() {
    if (this._rafId) cancelAnimationFrame(this._rafId);
    this._rafId = null;
    this._particles = [];
    if (this._resizeRaf) {
      cancelAnimationFrame(this._resizeRaf);
      this._resizeRaf = 0;
    }
    if (this._mq) {
      this._mq.removeEventListener("change", this._onMqChange);
    }
    if (typeof window !== "undefined" && this._onResize) {
      window.removeEventListener("resize", this._onResize);
      window.removeEventListener("orientationchange", this._onResize);
    }
  }

  // Bulk setter — preferred update path. `load` merges; `planets`
  // replaces the whole list when provided. Passing `undefined` for
  // `planets` leaves the previous cluster intact (useful during
  // transient /api/status errors so the diagram doesn't blank out).
  setReadings(r) {
    if (r.load != null)         this._readings.load    = r.load;
    if (Array.isArray(r.planets)) this._readings.planets = r.planets;
    // Optional today's-totals payload pushed through to the central
    // hub render. selfPoweredPctToday is the share of consumption
    // sourced from PV/battery (i.e. NOT the grid) over the whole
    // day — parallel to the live `selfPoweredPct` the component
    // computes from current planet power. Nullable; if missing, the
    // hub just hides that line.
    if (r.selfPoweredPctToday !== undefined) this._readings.selfPoweredPctToday = r.selfPoweredPctToday;
    // `?delay=N` (dev hook) holds the loading state for N ms after the
    // first setReadings call so the shimmer + fade-in can be inspected.
    // Subsequent calls (once loaded) apply immediately.
    const delay = ftwDebugDelay();
    if (!this._loaded && delay > 0) {
      if (!this._loadTimer) {
        this._loadTimer = setTimeout(() => {
          this._loadTimer = null;
          this._loaded = true;
          this.update();
        }, delay);
      }
      // Still re-render so the loading-state layout reflects the new
      // planet list (e.g. cluster arcs shift if count changes).
      this.update();
      return;
    }
    this._loaded = true;
    this.update();
  }

  // SVG class for the three loading phases. Called exactly once per
  // render(); the `_everFaded` flag flips the first time we exit
  // loading so the fade-in animation plays once and never re-triggers
  // on subsequent /api/status polls (every 2 s). After that, the SVG
  // renders with no class and shows its steady state directly.
  _svgClass() {
    if (!this._loaded) return "ef-loading";
    if (!this._everFaded) { this._everFaded = true; return "ef-fade-in"; }
    return "";
  }

  // Build one aggregation layer — either the individual planets or the
  // folded "combined" version. Both layouts use the SAME orbit + hub
  // geometry (P), so a planet at corner X always sits on the same
  // ring regardless of which layer is visible; only the cluster arc
  // density differs. Pushes particle params into `this._particles`
  // so afterRender() binds the <circle> DOM nodes back; each layer's
  // particles live under a unique key so both layouts can animate
  // independently when the toggle flips.
  _buildLayer(layerGroups, P, layerClass, anchors) {
    const placed = [];
    for (const c of Object.keys(layerGroups)) {
      const g = layerGroups[c];
      if (g.length === 0) continue;
      const pl = clusterArc(g.length, CX, P.cy, P.orbitR, anchors[c], P.baseR);
      g.forEach((planet, i) => {
        placed.push({ ...planet,
          _pos: pl.positions[i], _r: pl.r, _groupSize: g.length });
      });
    }
    const edges = placed.map(p => {
      const kwAbs = Math.abs(p.kw);
      return {
        id: `${layerClass}:${p.id}`,
        ...radialEndpoints(p._pos, p._r, P.hubR, p.toHub, CX, P.cy),
        kw: kwAbs,
        color: p.color,
        active: !p.placeholder && !isIdleKw(p.kw),
      };
    });
    const maxKw = Math.max(0.5, ...edges.map(e => e.kw));
    const edgesSvg = edges.map(e => renderEdge(e, maxKw, this._particles)).join("");
    const nodesSvg = placed.map(p => {
      const aggregateMemberCount = Number(p.dailyAggregateMembers || 0);
      const suppressAggregateDaily =
        p.dailyScope === "aggregate" &&
        !p.aggregated &&
        (layerClass === "ind" || aggregateMemberCount > 1) &&
        Math.max(p._groupSize || 1, aggregateMemberCount || 1) > 1;
      return renderCircleNode({
        pos: p._pos,
        value: p.placeholder ? "—" : fmtKw(p.kw),
        title: p.title,
        nameLabel: p._groupSize > 1 && p.name ? p.name.toUpperCase() : null,
        sub: p.sub,
        color: p.color,
        soc: p.placeholder ? null : p.soc,
        chargeLimit: p.placeholder ? null : p.chargeLimit,
        socStale: !p.placeholder && !!p.socStale,
        socSource: p.placeholder ? null : p.socSource,
        radius: p._r,
        clickable: !p.placeholder && !!p.role,
        role: p.role || "",
        name: p.name || "",
        id: p.id,
        aggregated: !!p.aggregated,
        dailyKwh: p.placeholder || suppressAggregateDaily ? null : (p.dailyKwh || null),
        dailyKwhParts: p.placeholder || suppressAggregateDaily ? null : (p.dailyKwhParts || null),
        compact: !!this._compact,
      });
    }).join("");
    return `<g class="ef-layer ef-layer-${layerClass}">` +
           `<g class="ef-edges">${edgesSvg}</g>${nodesSvg}</g>`;
  }

  // Override FtwElement.update so we can snapshot particle motion
  // state BEFORE the base class wipes the shadow DOM. afterRender()
  // restores the state onto the freshly-bound particles keyed by
  // `_key`. Particles that survive across renders never stutter; new
  // particles (added because kW grew) warm up normally; dropped ones
  // just vanish. No per-2s reset.
  update() {
    if (this._bound && this._bound.length) {
      const snap = new Map();
      for (const b of this._bound) {
        if (b.p._key) {
          snap.set(b.p._key, {
            bornAt: b.p.bornAt,
            sx: b.p.sx, sy: b.p.sy,
            vx: b.p.vx, vy: b.p.vy,
            life: b.p.life,
            phase: b.p.phase,
            omega: b.p.omega,
            damp:  b.p.damp,
            amp:   b.p.amp,
          });
        }
      }
      this._snapshot = snap;
    }
    super.update();
  }

  // Called by FtwElement after each render() replaces the shadow DOM.
  // We cancel any in-flight rAF, bind the freshly-rendered <circle>
  // elements to the particle-param list `render()` just built, and
  // start a new animation loop. The loop is a single rAF that iterates
  // every particle — cheaper than SMIL when you have hundreds of them,
  // and gives us per-frame noise terms SMIL can't express.
  afterRender() {
    if (this._rafId) {
      cancelAnimationFrame(this._rafId);
      this._rafId = null;
    }
    // Aggregation toggle — flipping the aria-checked attribute and
    // the svg's data-agg triggers the CSS opacity transition between
    // layers. Intentionally NOT calling this.update() here: a full
    // re-render would wipe the shadow DOM mid-fade and the transition
    // would freeze. The two layers already both exist; we just flip
    // which is visible.
    const toggleBtn = this.shadowRoot.querySelector(".ef-toggle");
    if (toggleBtn) {
      toggleBtn.addEventListener("click", () => {
        this._aggregated = !this._aggregated;
        const svgEl = this.shadowRoot.querySelector("svg");
        if (svgEl) svgEl.dataset.agg = this._aggregated ? "on" : "off";
        toggleBtn.setAttribute("aria-checked", this._aggregated ? "true" : "false");
        toggleBtn.setAttribute("title", this._aggregated
          ? "Split multi-device corners into individual bubbles"
          : "Combine multi-device corners into one bubble");
        try { localStorage.setItem(EF_AGGREGATED_CACHE_KEY, this._aggregated ? "1" : "0"); } catch (e) {}
      });
    }
    // Delegated click on the SVG — one listener per render covers every
    // planet group that opted in via data-role. The handler dispatches
    // `ftw-planet-click` so callers (app.js) can route per-role
    // (e.g. ev → open EV modal scoped to this driver).
    const svg = this.shadowRoot.querySelector('svg');
    if (svg) {
      const fire = (g) => {
        const role = g.getAttribute('data-role') || '';
        const name = g.getAttribute('data-name') || '';
        const id   = g.getAttribute('data-id')   || '';
        this.dispatchEvent(new CustomEvent('ftw-planet-click', {
          detail: { role, name, id }, bubbles: true, composed: true,
        }));
      };
      svg.addEventListener('click', (e) => {
        const g = e.target.closest && e.target.closest('.ef-clickable');
        if (g) fire(g);
      });
      svg.addEventListener('keydown', (e) => {
        if (e.key !== 'Enter' && e.key !== ' ') return;
        const g = e.target.closest && e.target.closest('.ef-clickable');
        if (g) { e.preventDefault(); fire(g); }
      });
    }
    const nodes = this.shadowRoot.querySelectorAll('.ef-p');
    if (!nodes.length || !this._particles.length) return;
    // Wire each DOM node to its param slot. `render()` assigned indices
    // via `data-i`; we trust those rather than node order in case the
    // browser reorders subtree attribute-only nodes in the future.
    const bound = [];
    nodes.forEach((n) => {
      const i = +n.dataset.i;
      const p = this._particles[i];
      if (p) bound.push({ el: n, p });
    });
    if (!bound.length) {
      this._bound = [];
      return;
    }
    // Carry per-particle motion state across re-renders so the fountain
    // doesn't visibly reset every 2 s. update() snapshots prior state
    // keyed on `_key`; here we copy it back onto the new param list
    // and skip warm-up for any particle that already existed.
    if (this._snapshot && this._snapshot.size) {
      for (const b of bound) {
        const prev = this._snapshot.get(b.p._key);
        if (prev) {
          b.p.bornAt = prev.bornAt;
          b.p.sx = prev.sx; b.p.sy = prev.sy;
          b.p.vx = prev.vx; b.p.vy = prev.vy;
          b.p.life = prev.life;
          b.p.phase = prev.phase;
          b.p.omega = prev.omega;
          b.p.damp  = prev.damp;
          b.p.amp   = prev.amp;
          b.p._warmUp = false;
        }
      }
      this._snapshot = null;
    }
    this._bound = bound;
    const tick = (now) => {
      const t = (now - this._tickStart) / 1000;
      for (let k = 0; k < bound.length; k++) {
        const b = bound[k];
        const p = b.p;
        let age = t - p.bornAt;
        if (age >= p.life || p.life === 0) {
          rollLife(p, t);
          // First-ever spawn: backdate bornAt uniformly across the
          // pool's lifetime so particles are spread evenly instead of
          // bursting together. p._warmUpIdx is in (0, 1), so this
          // seeds the fountain with a steady state.
          if (p._warmUp) {
            p.bornAt = t - p._warmUpIdx * p.life;
            p._warmUp = false;
          }
          age = t - p.bornAt;
        }
        // Along-path progress: linear travel from spawn toward target.
        // No easing — real electrons don't decelerate.
        const along = p.vx * age;              // along-vector component
        const alongY = p.vy * age;
        // Perpendicular offset: damped harmonic oscillator. This is
        // the "gravity circling the beam" effect — a spring pulls the
        // particle toward the beam centerline with angular frequency
        // omega, while γ damps amplitude over time so particles
        // spiral IN as they approach the target.
        //   perp(t) = A * e^(−γt) * cos(ωt + φ)
        const envelope = Math.exp(-p.damp * age);
        const wave = Math.cos(p.omega * age + p.phase);
        const perp = p.amp * envelope * wave;
        const x = p.sx + along + p.perpX * perp;
        const y = p.sy + alongY + p.perpY * perp;
        // Opacity is fixed — set at render time, never touched here.
        // Size variance (per-particle `radius`) replaces the old
        // opacity pulse as the "texture" cue.
        b.el.setAttribute('cx', x.toFixed(1));
        b.el.setAttribute('cy', y.toFixed(1));
      }
      this._rafId = requestAnimationFrame(tick);
    };
    this._rafId = requestAnimationFrame(tick);
  }

  render() {
    const { load } = this._readings;

    // Self-powered % for the visible site demand — house load plus any
    // active EV charger. When EV is excluded, a 9 kW car charge can make a
    // PV+battery-covered house display 0 % simply because grid import exceeds
    // the house-only load. The energy-flow diagram shows the EV as part of the
    // live balance, so the denominator should match what is on screen.
    let selfPoweredPct = null;
    {
      let gridImport = 0;
      for (const p of (this._readings.planets || [])) {
        if (p.role === "grid" && p.toHub) gridImport += Math.max(0, p.kw || 0);
      }
      let evDemandKw = 0;
      for (const p of (this._readings.planets || [])) {
        if (p.role === "ev") evDemandKw += Math.max(0, p.kw || 0);
      }
      const consumptionKw = (Math.abs(load) || 0) + evDemandKw;
      if (!isIdleKw(consumptionKw)) {
        const fromGrid = Math.min(gridImport, consumptionKw);
        selfPoweredPct = Math.max(0, Math.min(100, (1 - fromGrid / consumptionKw) * 100));
      }
    }

    // Two tiers of base parameters. Desktop keeps the full 0..1000 viewBox
    // with larger circles + hub; compact crops to 180..820 and shrinks the
    // base orbit so phone widths render legibly. Both tiers are FLOORS:
    // the dynamic-sizing block below grows orbitR + viewBox H/W when any
    // corner holds 2+ planets so the arc never pushes a node off-canvas.
    const tier = this._compact
      ? { vbX: 180, vbW: 640, orbitR: 268, baseR: 86, hubR: 95 }
      : { vbX: 0,   vbW: W,   orbitR: 288, baseR: 84, hubR: 99 };

    // Group planets by corner. Missing corners get a placeholder so
    // the X silhouette is complete on first paint even before any
    // drivers report.
    const groups = {
      "top-left": [], "top-right": [],
      "bottom-right": [], "bottom-left": [],
    };
    for (const p of this._readings.planets) {
      if (groups[p.corner]) groups[p.corner].push(p);
    }

    // Persist (loaded) / restore (loading) the populated-corner map so
    // the loading skeleton on the next visit matches THIS user's
    // hardware — Y-shape if no EV, 3-corner if no battery, full X
    // otherwise — AND the per-corner planet count, so individual-mode
    // skeletons render the right number of bubbles per corner (dual
    // PV inverters → two placeholders at top-left). Writes only when
    // the map changes (adding/removing a driver, not every 2 s poll).
    if (this._loaded) {
      try {
        const counts = {};
        for (const c of Object.keys(groups)) {
          if (groups[c].length > 0) counts[c] = groups[c].length;
        }
        const serialized = JSON.stringify(counts);
        if (localStorage.getItem(EF_LAYOUT_CACHE_KEY) !== serialized) {
          localStorage.setItem(EF_LAYOUT_CACHE_KEY, serialized);
        }
      } catch (e) {}
    }
    let expectedCounts = null;
    if (!this._loaded) {
      try {
        const raw = localStorage.getItem(EF_LAYOUT_CACHE_KEY);
        if (raw) {
          const parsed = JSON.parse(raw);
          if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
            expectedCounts = parsed;
          } else if (Array.isArray(parsed) && parsed.length > 0) {
            // Legacy shape: array of corner names → treat each as count=1.
            expectedCounts = {};
            for (const c of parsed) expectedCounts[c] = 1;
          }
        }
      } catch (e) {}
    }

    for (const c of Object.keys(groups)) {
      if (groups[c].length === 0) {
        // Placeholders fill empty corners during the initial loading
        // phase so the X silhouette reads complete before /api/status
        // returns (role-native color + spinning ring). Once loaded,
        // an empty corner is a genuine absence — but the rules
        // differ by role:
        //   • Grid (bottom-left) — ALWAYS rendered even when absent.
        //     A site without a reporting grid meter is a critical
        //     telemetry gap, and a visible "no data" bubble is the
        //     right signal to the operator.
        //   • PV / Battery / EV — hardware is optional. Drawing a
        //     "— / no data" bubble for a category the user didn't
        //     configure would misrepresent absent hardware as
        //     present-but-idle, so those corners render nothing.
        if (this._loaded && c !== "bottom-left") continue;
        // Loading + cached layout: only the corners that were
        // populated last session get a placeholder. First-ever visit
        // (no cache) keeps the full X silhouette as before.
        if (!this._loaded && expectedCounts && !expectedCounts[c]) continue;
        // How many placeholder bubbles to draw at this corner.
        // Loading + individual mode + cache → use cached count so a
        // 2-PV user sees 2 placeholders at top-left (matches the
        // post-load layout). Loaded-empty-grid, combined mode, or
        // no-cache visits all stay at 1.
        const n = (!this._loaded && !this._aggregated && expectedCounts && expectedCounts[c])
          ? expectedCounts[c]
          : 1;
        for (let i = 0; i < n; i++) {
          groups[c].push({
            id: n > 1 ? `_placeholder-${c}-${i}` : `_placeholder-${c}`, corner: c,
            title: CORNER_PLACEHOLDER_TITLE[c], name: null,
            role: CORNER_PLACEHOLDER_ROLE[c],
            kw: 0, toHub: true,
            // Loaded-but-empty grid reads as muted ("broken") rather
            // than role-colored; unloaded corners keep the lively
            // role-native color so the skeleton doesn't look dead.
            color: this._loaded ? "var(--fg-muted)" : CORNER_PLACEHOLDER_COLOR[c],
            sub: "no data", soc: null,
            placeholder: true,
          });
        }
      }
    }
    // Y layout (no EV): bottom-right is empty, so Grid collapses to
    // bottom-center. CSS uses this (combined with svg[data-agg="on"]
    // via :has()) to add desktop-only bottom padding in combined mode,
    // where the merged Grid bubble would otherwise clip on the card edge.
    this.toggleAttribute("data-no-ev", groups["bottom-right"].length === 0);

    const maxN = Math.max(1, ...Object.values(groups).map(g => g.length));

    // Sizing reservation. During the loading phase we only have
    // placeholder clusters (n=1 each), but the SVG that follows once
    // /api/status returns often has n=2 (dual-inverter setups) — and
    // the factor delta is ~150 px, which shifts the whole page when
    // the first reading lands. Reserve space for a "typical" loaded
    // layout up front: prefer the last-known cluster size from
    // localStorage (so repeat sessions paint at exactly the right
    // height), fall back to 2-per-corner. Once loaded, the true maxN
    // wins and we cache it for next time.
    let sizingN = maxN;
    if (!this._loaded) {
      let cached = 0;
      try { cached = +localStorage.getItem(EF_SIZING_CACHE_KEY) || 0; } catch (e) {}
      sizingN = Math.max(2, cached);
    } else {
      try { localStorage.setItem(EF_SIZING_CACHE_KEY, String(maxN)); } catch (e) {}
    }

    // -- Dynamic orbit + container sizing --------------------------------
    // For N>=3 the arc would either spill past 60° (clusterArc's maxSpan)
    // or shrink baseR — so we grow orbitR instead, keeping every node
    // full-sized. For N=1 or 2 the natural step already fits and orbitR
    // stays at the tier floor.
    const gap = 16;
    const maxSpan = Math.PI / 3;
    let orbitR = tier.orbitR;
    if (sizingN >= 3) {
      const step = maxSpan / (sizingN - 1);
      const required = (2 * tier.baseR + gap) / (2 * Math.sin(step / 2));
      orbitR = Math.max(orbitR, Math.ceil(required));
    }

    // Recompute the actual step + half-span at this orbitR (mirrors the
    // formula clusterArc uses), so we know how far the outermost arc
    // position lies from the corner anchor.
    const naturalStep = 2 * Math.asin(Math.min(1, (2 * tier.baseR + gap) / (2 * orbitR)));
    const stepActual = sizingN <= 1 ? 0 : Math.min(naturalStep, maxSpan / (sizingN - 1));
    const halfSpan = ((sizingN - 1) * stepActual) / 2;

    // Effective anchor per corner. A corner collapses to the shared
    // top/bottom axis when it's populated and its partner is empty —
    // turns "PV only" into PV-at-top-center and "Grid only" into
    // Grid-at-bottom-center so the silhouette stays symmetric.
    const effAnchor = {};
    const populated = [];
    for (const c of Object.keys(CORNER_ANGLE)) {
      const partner = PARTNER_CORNER[c];
      const mine = groups[c] && groups[c].length > 0;
      const partnerEmpty = !groups[partner] || groups[partner].length === 0;
      if (mine && partnerEmpty) {
        effAnchor[c] = (c === "top-left" || c === "top-right")
          ? ANGLE_TOP_CENTER : ANGLE_BOTTOM_CENTER;
      } else {
        effAnchor[c] = CORNER_ANGLE[c];
      }
      if (mine) populated.push(c);
    }

    // Worst-case x/y offsets from CX/CY, measured only across the
    // corners we actually render. Empty corners contribute nothing —
    // a PV-only setup doesn't need to reserve battery/EV space.
    const margin = 12;
    let maxYOff = 0, maxXOff = 0;
    for (const c of populated) {
      const a0 = effAnchor[c];
      for (const da of [-halfSpan, +halfSpan]) {
        const ay = Math.abs(orbitR * Math.sin(a0 + da));
        const ax = Math.abs(orbitR * Math.cos(a0 + da));
        if (ay > maxYOff) maxYOff = ay;
        if (ax > maxXOff) maxXOff = ax;
      }
    }
    // Y pattern (no EV): Grid collapses to bottom-center, the 12 px
    // margin isn't quite enough once the combined bubble's visuals
    // (ring + connectors) extend past baseR, so the lower arc clips.
    // Bias extra room to the bottom only by growing Hdyn AND shifting
    // CY up by the same amount — top side stays tight.
    const bottomExtra = groups["bottom-right"].length === 0 ? 30 : 0;
    const Hdyn = Math.max(H_BASE, Math.ceil(2 * (maxYOff + tier.baseR + margin)) + bottomExtra);
    const Wneeded = Math.ceil(2 * (maxXOff + tier.baseR + margin));
    let vbW = tier.vbW, vbX = tier.vbX;
    if (Wneeded > vbW) {
      // Hub stays at world x=500; recenter the crop around it.
      vbW = Wneeded;
      vbX = Math.round(W / 2 - vbW / 2); // negative is fine — the viewBox can extend past world bounds
    }
    // Scale the rendered CSS height so the SVG keeps its on-screen
    // visual proportions as the viewBox grows. CSS picks the per-tier
    // base (535/510/460) via media queries; we just multiply.
    this.style.setProperty("--efl-h-factor", (Hdyn / H_BASE).toFixed(3));

    // Per-render layout struct. CY now varies — every helper that used
    // to read module-level CY now takes (cx, cy) explicitly. In the Y
    // pattern CY is shifted up by half of bottomExtra so the added
    // Hdyn becomes empty space beneath the bottom-center cluster,
    // not centered letter-boxing.
    const cy = (Hdyn - bottomExtra) / 2;
    // Hub vertical layout — four rows centred around `cy` now that
    // the trailing CONSUMING label is gone. Stack span ≈ 90px (icon
    // → bottom-of-self-today); offsets chosen so the visual mass
    // sits in the middle of the disc instead of biasing upward.
    //
    //   1. icon                 (above the text block)
    //   2. realtime power       (the big number)
    //   3. % SELF-POWERED NOW
    //   4. % SELF-POWERED TODAY
    const compact = this._compact;
    const P = {
      vbX, vbW, H: Hdyn, cy,
      orbitR, baseR: tier.baseR, hubR: tier.hubR,
      // Hub icon Y — backed off ~14 px from the previous lift after a
      // visual check showed the icon sitting on top of the dashed
      // ring (which lives at hubR − 8). Now icon at cy − 60 desktop
      // / cy − 76 compact stays comfortably inside the ring while
      // still clearing the power value below.
      hubIconY:      cy - (compact ? 76 : 60),
      // Desktop power value pushed down 8 px (cy − 10 → cy − 2);
      // compact stays at cy − 8 since the small-screen layout was
      // already balanced. Sub-lines on desktop follow the same
      // 8 px shift so the relative spacing is preserved.
      hubValueY:     cy - (compact ? 8  : 2),
      hubSelfNowY:   cy + (compact ? 14 : 24),
      hubSelfTodayY: cy + (compact ? 32 : 42),
    };
    // -- /Dynamic sizing -------------------------------------------------

    // Two layouts live in the SVG simultaneously so the
    // aggregated↔individual toggle is a pure CSS cross-fade instead
    // of a shadow-DOM re-render (which would wipe the transition).
    // `groups` above is the individual layout (one entry per planet);
    // `aggGroups` folds multi-planet corners into a single synthesized
    // bubble. The aggregated bubble's id is stable (`agg-<corner>`) so
    // particles keep state across /api/status polls.
    this._particles = [];
    const hasMultipleType = hasMultipleOfAnyType(this._readings.planets);
    const aggGroups = aggregateGroups(groups);
    const indLayerSvg = this._buildLayer(groups, P, "ind", effAnchor);
    const aggLayerSvg = this._buildLayer(aggGroups, P, "agg", effAnchor);

    const aggAttr = this._aggregated ? "on" : "off";

    return `
      <div class="title">Energy balance</div>
      ${hasMultipleType ? `
        <button class="ef-toggle" type="button" role="switch"
                aria-checked="${this._aggregated ? "true" : "false"}"
                aria-label="Combine multiple inverters into one bubble"
                title="${this._aggregated ? "Split multi-device corners into individual bubbles" : "Combine multi-device corners into one bubble"}">
          <span class="ef-toggle-label">Combined</span>
          <span class="ef-toggle-track"></span>
        </button>
      ` : ""}
      <svg class="${this._svgClass()}" data-agg="${aggAttr}" viewBox="${P.vbX} 0 ${P.vbW} ${P.H}" preserveAspectRatio="xMidYMid meet" aria-hidden="true">
        <defs>
          <radialGradient id="ef-hub" cx="50%" cy="50%" r="50%">
            <stop offset="0%" stop-color="oklch(0.85 0.18 var(--accent-hue))" stop-opacity="0.55"/>
            <stop offset="70%" stop-color="oklch(0.5 0.12 var(--accent-hue))" stop-opacity="0.04"/>
            <stop offset="100%" stop-color="transparent"/>
          </radialGradient>
          <filter id="ef-soft">
            <feGaussianBlur stdDeviation="2.5" result="b"/>
            <feMerge><feMergeNode in="b"/><feMergeNode in="SourceGraphic"/></feMerge>
          </filter>
          <!-- Wide bloom for the outer railgun aura. stdDeviation=5 gives
               a 10 px halo which is enough to read as a glow without
               washing adjacent nodes. The filter region is 200% of the
               bbox so the bloom isn't clipped at edge endpoints. -->
          <filter id="ef-bloom" x="-50%" y="-50%" width="200%" height="200%">
            <feGaussianBlur stdDeviation="5" result="b"/>
            <feMerge><feMergeNode in="b"/><feMergeNode in="SourceGraphic"/></feMerge>
          </filter>
        </defs>

        <circle cx="${CX}" cy="${P.cy}" r="200" fill="url(#ef-hub)"/>

        ${aggLayerSvg}
        ${indLayerSvg}

        <!-- HOUSE / hub: load reading lives here. Kept outside both
             aggregation layers because the hub value (instantaneous
             house load) is identical in both views, so there's no
             reason to duplicate it. -->
        <g class="ef-hub">
          <circle cx="${CX}" cy="${P.cy}" r="${P.hubR}"
                  fill="var(--hero-house-fill)"
                  stroke="var(--hero-house-stroke)" stroke-width="1.5"/>
          <circle class="ring" cx="${CX}" cy="${P.cy}" r="${P.hubR - 8}"
                  fill="none"
                  stroke="var(--hero-house-ring)" stroke-width="1"
                  stroke-dasharray="2 4"/>
          <g transform="translate(${CX - 16}, ${P.hubIconY})"
             stroke="var(--hero-house-stroke)" stroke-width="1.6"
             fill="none" stroke-linecap="round" stroke-linejoin="round">
            <path d="M2 16 L16 3 L30 16 L30 26 L2 26 Z"/>
            <path d="M12 26 V18 H20 V26"/>
          </g>
          <text x="${CX}" y="${P.hubValueY}" text-anchor="middle"
                fill="var(--hero-load-text)" class="sv-hub-value">
            ${fmtKw(load)}
          </text>
          ${selfPoweredPct !== null ? `
          <text x="${CX}" y="${P.hubSelfNowY}" text-anchor="middle"
                fill="var(--fg)" class="sv-hub-sub">
            ${Math.round(selfPoweredPct)}% SELF-POWERED NOW
          </text>` : ""}
          ${this._readings.selfPoweredPctToday != null ? `
          <text x="${CX}" y="${P.hubSelfTodayY}" text-anchor="middle"
                fill="var(--fg)" class="sv-hub-sub">
            ${Math.round(this._readings.selfPoweredPctToday)}% SELF-POWERED TODAY
          </text>` : ""}
        </g>
      </svg>
    `;
  }
}

// ---------- geometry + edge helpers ----------

// Place N nodes on a hub-centered circle of radius `orbitR` along an
// arc around `anchorAngle` (the polar angle that points to the
// quadrant's corner). Every node sits on the same circle, so its
// radial beam to the hub is the same length as every other satellite's
// beam — the "circle around the house" layout.
//
// Sizing:
//   • n === 1 → node sits exactly on the anchor with full baseR.
//   • n  >= 2 → angular step is whatever keeps adjacent circles gap
//               apart on the orbit (chord = 2*baseR + gap). If that
//               would spread the cluster past `maxSpan`, we cap the
//               span and shrink nodeR so non-overlap still holds.
//
// `maxSpan` (≈60°) keeps the arc comfortably inside its quadrant — PV
// stops short of the grid anchor, battery stops short of EV — so the
// four regions stay visually distinct even with many devices.
function clusterArc(n, cx, cy, orbitR, anchorAngle, baseR) {
  if (n <= 1) {
    return { positions: [polar(cx, cy, orbitR, anchorAngle)], r: baseR };
  }
  const gap = 16;
  const maxSpan = Math.PI / 3; // 60°
  const idealChord = 2 * baseR + gap;
  let step = 2 * Math.asin(Math.min(1, idealChord / (2 * orbitR)));
  let r = baseR;
  if (step * (n - 1) > maxSpan) {
    step = maxSpan / (n - 1);
    const chord = 2 * orbitR * Math.sin(step / 2);
    r = Math.max(32, Math.floor((chord - gap) / 2));
  }
  const half = ((n - 1) * step) / 2;
  const positions = Array.from({ length: n }, (_, i) =>
    polar(cx, cy, orbitR, anchorAngle - half + i * step),
  );
  return { positions, r };
}

function polar(cx, cy, r, a) {
  return { x: cx + r * Math.cos(a), y: cy + r * Math.sin(a) };
}

// Radial beam endpoints for a planet sitting on the hub's orbit. Both
// endpoints lie on the line between the planet center and the hub
// center; the planet endpoint lands on its circle perimeter, the hub
// endpoint on the hub perimeter. `toHub` picks the particle direction
// (animateMotion always walks from → to, so swapping them flips the
// flow — cheaper than maintaining two edge variants).
function radialEndpoints(pos, nodeR, hubR, toHub, cx, cy) {
  const dx = cx - pos.x;
  const dy = cy - pos.y;
  const len = Math.hypot(dx, dy) || 1;
  const ux = dx / len;
  const uy = dy / len;
  const planetEdge = { x: pos.x + ux * nodeR, y: pos.y + uy * nodeR };
  const hubEdge    = { x: cx    - ux * hubR,   y: cy    - uy * hubR  };
  return toHub
    ? { from: planetEdge, to: hubEdge }
    : { from: hubEdge,    to: planetEdge };
}

// Render a single edge as beam paths + plain particle circles. Each
// particle is POSITIONED by the rAF loop in afterRender, not by SMIL —
// so every electron has its own independent amp/phase/freq plus a 2D
// noise term, and at high kW the stream looks genuinely chaotic
// instead of resolving into visible screw threads.
function renderEdge(e, _maxKw, collect) {
  const width = clamp(1.5 + e.kw * 1.8, 1.5, 16);
  const dx = e.to.x - e.from.x;
  const dy = e.to.y - e.from.y;
  const len = Math.hypot(dx, dy);
  const straightD = `M ${e.from.x} ${e.from.y} L ${e.to.x} ${e.to.y}`;
  if (!e.active || len < 1) {
    return `<path d="${straightD}" stroke="var(--hero-line-base)" stroke-width="${width.toFixed(1)}" fill="none" stroke-linecap="round"/>`;
  }
  // Railgun beam — bloom + body + white core. Opacities nudged up so
  // the beam reads as a hot wire behind the particle spray, not a
  // ghost. Still balanced so particles stay the primary signal.
  const beam =
    `<path d="${straightD}" stroke="${e.color}" stroke-width="${(width * 2.6).toFixed(1)}" ` +
      `fill="none" stroke-linecap="round" opacity="0.22" filter="url(#ef-bloom)"/>` +
    `<path d="${straightD}" stroke="${e.color}" stroke-width="${width.toFixed(1)}" ` +
      `fill="none" stroke-linecap="round" opacity="0.45"/>` +
    `<path d="${straightD}" stroke="var(--white-s)" stroke-width="${(width * 0.35).toFixed(1)}" ` +
      `fill="none" stroke-linecap="round" opacity="0.55"/>`;

  // Fountain emitter: each particle has its own spawn jitter, velocity,
  // lifetime, lateral wobble, and opacity envelope — reset on respawn.
  // No shared path; no modulo-of-time-loop. When a particle's life
  // expires the rAF loop re-rolls its parameters and sends it again
  // from the source box, so the visual is a continuous spray with no
  // pattern that the eye can latch onto.
  const dirX = dx / len;
  const dirY = dy / len;
  const perpX = -dirY;
  const perpY =  dirX;
  // Base speed in px/s. A 250 px edge at 80 px/s takes ~3 s end-to-end —
  // calm enough to be readable but not sluggish.
  const baseSpeed = 80;
  // Per-kW particle count. Pool is continuously alive — particles
  // respawn the instant life ends. 75 is the ceiling per beam at
  // ~5 kW and above; min 10 so trickle flows still read as a stream.
  const count = clamp(Math.round(e.kw * 15), 10, 75);

  let particleSvg = "";
  for (let i = 0; i < count; i++) {
    // Static per-particle geometry. The dynamic bits (jitter, speed,
    // life, wobble, born-at) are rolled on every respawn in the rAF
    // loop — see rollLife() there.
    const params = {
      // Static geometry (frozen at edge-render time).
      fx: e.from.x, fy: e.from.y,
      dirX, dirY, perpX, perpY,
      len, baseSpeed,
      // Emission area half-width along the source box face — spawn
      // points are randomised within this interval each respawn.
      // Kept tight so the fountain reads as a focused stream; the
      // mid-flight swirl (rollLife's omega/damp) does the visual
      // work of making the beam feel alive.
      spread: 6,
      // Cone half-angle (radians) — particles deviate this much from
      // a straight line to the target. Narrow enough that the overall
      // flow direction is still obvious but wide enough that no two
      // particles trace identical arcs.
      cone: 0.12,
      // Dynamic fields — initialised below with random starting phases
      // so the fountain is already mid-flight on first paint instead
      // of bursting from zero.
      bornAt: 0,
      sx: 0, sy: 0,           // spawn point (after jitter)
      vx: 0, vy: 0,            // velocity vector (after cone + speed)
      life: 0,                 // seconds until respawn
      wobbleAmp: 0,
      wobbleFreq: 0,
      wobblePhase: 0,
      // Per-particle constants (not reset on respawn). Opacity is
      // baked into the circle's initial attribute and never touched
      // again — no per-frame fade. Size variance stands in for the
      // old pulse, giving the spray visual texture without animation.
      radius: 0.8 + Math.random() * 1.3,
      fixedOpacity: (0.75 + Math.random() * 0.25).toFixed(2),
    };
    // First tick triggers rollLife (life===0). `_warmUp` + uniform
    // warm-up index backdates the first bornAt so particles are
    // spread evenly across the pool's lifetime on initial paint —
    // not bunched into a burst. After the first spawn, each particle
    // respawns independently whenever its own life expires.
    params._warmUp = true;
    params._warmUpIdx = (i + 0.5) / count;
    params.bornAt = 0;
    const idx = collect.length;
    collect.push(params);
    // Stable key across re-renders so update() can carry particle
    // motion state forward — otherwise every /api/status poll (every
    // ~2s) wipes innerHTML and every electron resets to its spawn
    // point, which reads as a visible "jam + restart" tick.
    params._key = `${e.id}|${i}`;
    particleSvg +=
      `<circle class="ef-p" data-i="${idx}" cx="${e.from.x.toFixed(1)}" cy="${e.from.y.toFixed(1)}" ` +
      `r="${params.radius.toFixed(2)}" fill="${e.color}" opacity="${params.fixedOpacity}"/>`;
  }
  return beam + particleSvg;
}

// Respawn a particle — called when age >= life or on first tick.
// Re-rolls every dynamic parameter with Math.random() so each flight
// is unique. Gravity model: spawn with an angular-velocity spring
// around the beam line; damping γ is tuned so amplitude decays to
// ~10% by end-of-life, giving the "spirals into the target" feel.
function rollLife(p, now) {
  p.bornAt = now;
  // Spawn jitter along the source box face — spread perpendicular to
  // the beam direction so the fountain emits from a line segment, not
  // a point.
  const jitter = (Math.random() - 0.5) * 2 * p.spread;
  p.sx = p.fx + p.perpX * jitter;
  p.sy = p.fy + p.perpY * jitter;
  // Cone emission: small angular deviation from the straight-line
  // direction (±cone radians). Keeps the flow heading target-ward
  // while giving each particle its own trajectory.
  const coneOff = (Math.random() - 0.5) * 2 * p.cone;
  const c = Math.cos(coneOff), s = Math.sin(coneOff);
  // Rotate (dirX,dirY) by coneOff into this life's velocity vector.
  const speed = p.baseSpeed * (0.75 + Math.random() * 0.5);
  p.vx = (p.dirX * c - p.dirY * s) * speed;
  p.vy = (p.dirX * s + p.dirY * c) * speed;
  // Lifetime sized to roughly reach target (len / speed). Slight extra
  // randomness so particles don't all respawn in lockstep.
  p.life = (p.len / speed) * (0.9 + Math.random() * 0.25);
  // Spring/damping parameters. Strong pull toward the beam (damp)
  // combined with high oscillation frequency (omega) packs several
  // tight cycles near the source before the envelope collapses, so
  // the eye reads it as swirling-into-the-beam rather than drifting.
  // damp ≈ 5.5/life drops amplitude to e^-5.5 (~0.4%) by end-of-life
  // and to e^-2.75 (~6%) at the midpoint; at omega in [4.5, 9] that's
  // 2-3 visible cycles before gravity dominates.
  p.omega = 4.5 + Math.random() * 4.5;
  p.damp  = 5.5 / p.life + Math.random() * 0.6;
  p.phase = Math.random() * Math.PI * 2;
  // Smaller initial radius to match — otherwise the early orbit flies
  // too far off the wire before gravity grabs it.
  p.amp = 2.5 + Math.random() * 3.5;
}

// Tiny xorshift — deterministic per-particle jitter so re-renders don't
// shuffle the stream. Returns a value in [0, 1).
function seedRand(seed) {
  let x = (seed + 0x9E3779B9) | 0;
  x ^= x << 13; x ^= x >>> 17; x ^= x << 5;
  return ((x >>> 0) / 4294967295);
}
function hashStr(s) {
  let h = 2166136261;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i); h = Math.imul(h, 16777619);
  }
  return h;
}

// ---------- nodes ----------

// Circular node for the × layout. Text is centered and respread for a
// disk: title near the top, value at the middle, sub below the middle,
// and — for batteries — a SoC reading as a fourth line below sub.
// Text baselines scale with radius so a desktop-size 105 px circle and
// a multi-device 55 px circle both read proportionally. Stroke is the
// accent color so each node carries its identity on the edge of the
// circle — no separate stripe needed the way rectangular boxes have.
function renderCircleNode({ pos, title, nameLabel, value, sub, color, soc,
                            chargeLimit = null, socStale = false, socSource = null,
                            radius = 86,
                            clickable = false, role = "", name = "", id = "",
                            aggregated = false,
                            dailyKwh = null, dailyKwhParts = null,
                            compact = false }) {
  const r = radius;
  const { x, y } = pos;
  // Daily totals line — empty string when no payload was passed (back-
  // compat with callers that haven't been wired up yet). Drops out
  // entirely on small planets (r < 50, e.g. 4+ planets clustered at
  // one corner) so the disc text doesn't overflow.
  const hasParts = Array.isArray(dailyKwhParts) && dailyKwhParts.length > 0;
  const showDaily = (dailyKwh || hasParts) && r >= 50;
  // Clickable planets must advertise themselves to assistive tech:
  // role=button so screen readers announce "button", and aria-label
  // derived from the visible title/name so the announcement names
  // what activating this node will open.
  const nodeLabel = [title, nameLabel].filter(Boolean).join(" ");
  const ariaLabel = nodeLabel ? `Open ${nodeLabel}` : "Open node";
  const groupAttrs = clickable
    ? ` class="ef-node ef-clickable" data-role="${escapeXml(role)}" data-name="${escapeXml(name)}" data-id="${escapeXml(id)}" tabindex="0" role="button" aria-label="${escapeXml(ariaLabel)}"`
    : ` class="ef-node"`;
  // When a per-device name suffix is present, the title becomes two
  // stacked lines ("SOLAR" / "SUNGROW"). That preserves more horizontal
  // room inside the disk than a single "SOLAR · SUNGROW" line.
  const twoLine = !!nameLabel;
  // Layout branches by visible-row count:
  //
  //   3 rows — title · value · daily               (e.g. solar)
  //   4 rows — title · value · daily · soc         (e.g. battery, no sub)
  //   5 rows — title · value · daily · sub · soc   (full)
  //   4 rows — title · value · sub · soc           (legacy, no daily)
  //
  // Three-row layout centres the power value on the disc midline.
  // Four/five-row layouts keep the original generous title→value gap
  // mirrored on value→daily; sub/soc trail on a tighter step.
  const showSub = !!sub;
  const showSoc = soc != null;
  const simple3 = showDaily && !showSub && !showSoc;
  let titleY, valueY, dailyY, subY, socY;
  if (simple3) {
    // Three-row stack with the title pinned at the legacy level
    // (so SOLAR aligns horizontally with titles on other corners)
    // and the power value biased a few pixels BELOW the disc
    // midline — visual centre on the big number sits near the
    // disc midline instead of riding above it. Per operator note:
    // "solar big power should go down a couple of pixels, not same
    // spacing between SOLAR and the kWh line".
    titleY = Math.round((twoLine ? -0.50 : -0.42) * r);
    valueY = Math.round(0.04 * r);
    dailyY = Math.round(0.42 * r);
    subY = 0;
    socY = 0;
  } else {
    titleY = Math.round((twoLine ? -0.50 : -0.42) * r);
    // Two "missing-row" branches that both compress the value→daily
    // gap from the wide 0.46 r used in the full 5-row case to ~0.26 r,
    // so daily and the next row don't crowd each other at the disc
    // bottom:
    //   - no-sub-but-soc  (battery, post colour-swap)
    //   - no-soc-but-sub  (grid)
    // The full 5-row case (EV with daily, sub, AND soc) keeps the
    // wide gap because there's enough vertical room to spread.
    const noSubButSoc = !showSub && showSoc;
    const noSocButSub = showSub && !showSoc;
    const compressed  = noSubButSoc || noSocButSub;
    // Compact (≤600px) uses a wider value→daily gap than first
    // tried (0.22 r → 0.32 r) — the larger CSS font sizes on small
    // screens make 0.22 r feel cramped. Trailing rows shift down
    // proportionally so the bubble bottom still sits inside the disc.
    const dailyR = compressed
      ? (compact ? 0.32 : 0.30)
      : (compact ? 0.32 : 0.50);
    // Compact bidirectional planets bumped down a touch (0.55 → 0.62)
    // so the importing/exporting label and battery SoC stop sitting
    // right under the kWh row on small screens.
    const subR = noSocButSub
      ? (compact ? 0.62 : 0.60)
      : (showDaily ? (compact ? 0.55 : 0.66) : 0.42);
    const socR = noSubButSoc
      ? (compact ? 0.62 : 0.52)
      : (showDaily ? (compact ? 0.74 : 0.80) : 0.70);
    valueY = Math.round((showDaily ? 0.04 : 0.09) * r);
    dailyY = Math.round(dailyR * r);
    subY   = Math.round(subR * r);
    socY   = Math.round(socR * r);
  }
  const titleSvg = twoLine
    ? `<text x="${x}" y="${y + titleY}" text-anchor="middle"
             fill="var(--hero-label-text)" class="sv-node-title">
         <tspan x="${x}" dy="0">${escapeXml(title)}</tspan>
         <tspan x="${x}" dy="1.2em">${escapeXml(nameLabel)}</tspan>
       </text>`
    : `<text x="${x}" y="${y + titleY}" text-anchor="middle"
             fill="var(--hero-label-text)" class="sv-node-title">
         ${escapeXml(title)}
       </text>`;
  // Label reads "Avg SoC" when the value is a cross-battery mean from
  // the aggregation layer, "SoC" for a single-battery reading.
  // Honest about provenance — a 72 % on the aggregated bubble is not
  // the same fact as a 72 % on one inverter.
  // The "%" makes "SoC" redundant — drop the label, keep only the
  // value. Aggregated batteries still get an "Avg" prefix to flag the
  // value is a cross-battery mean. EV planets gain two honesty markers:
  //  - "~" prefix when the value came from the inferred path (pluginSoC
  //    + deliveredWh) instead of the vehicle's own BMS.
  //  - "⚠ " prefix when the vehicle reading is stale (driver lost
  //    contact with the car for more than its stale_after_s window).
  //    Used to be "★ " — but "★" universally reads as "favourite",
  //    the opposite of "degraded". A warning glyph + amber color
  //    matches the rest of the app's stale signalling.
  // When a charge-limit is also known, render as "24/50%" — no spaces
  // around the slash, the format reads as "current of limit".
  // At small radii (r<50, e.g. 4+ planets clustered in one corner) the
  // chargeLimit suffix overflows the disc — drop it and show only the
  // current SoC so the text always fits.
  let socText = "";
  if (soc != null) {
    const socPrefix = socSource === "inferred" ? "~" : "";
    const aggPrefix = aggregated ? "Avg " : "";
    const stalePrefix = socStale ? "⚠ " : "";
    const showLimit = chargeLimit != null && chargeLimit > 0 && r >= 50;
    const socBody = showLimit
      ? `${socPrefix}${Math.round(soc)}/${Math.round(chargeLimit)}%`
      : `${socPrefix}${Math.round(soc)}%`;
    const fillColor = socStale ? "var(--stat-warn, #f59e0b)" : "var(--cyan)";
    const titleAttr = socStale ? ` data-tooltip="Reading is stale (no recent contact with vehicle)"` : "";
    socText = `<text x="${x}" y="${y + socY}" text-anchor="middle"
             fill="${fillColor}" font-weight="600" class="sv-node-sub"${titleAttr}>${aggPrefix}${stalePrefix}${socBody}</text>`;
  }
  // Icon swapped in during the loading + fade-in phases. Scale chosen
  // so a full-size planet (r ≈ 86) hosts a ~30 px icon — readable at
  // the overview zoom without competing with the text once it fades
  // in. `color: ${color}` on the group feeds currentColor into every
  // stroke/fill in the icon SVG.
  const iconSvg = ROLE_ICON[role] || ROLE_ICON.unknown;
  const iconScale = r * 0.45 / 10;
  return `
    <g${groupAttrs} style="color:${color}">
      <circle cx="${x}" cy="${y}" r="${r}"
              fill="var(--hero-box-fill)" stroke="${color}" stroke-width="2"/>
      <circle class="ef-loading-ring" cx="${x}" cy="${y}" r="${r - 6}"
              fill="none" stroke="${color}" stroke-width="1"
              stroke-dasharray="2 4"/>
      <g class="ef-icon" transform="translate(${x} ${y}) scale(${iconScale})">${iconSvg}</g>
      ${titleSvg}
      <text x="${x}" y="${y + valueY}" text-anchor="middle" fill="${color}" class="sv-node-value">
        ${value}
      </text>
      ${showDaily ? `<text x="${x}" y="${y + dailyY}" text-anchor="middle"
            fill="var(--hero-sub-text)" class="sv-node-sub">
        ${hasParts ? renderDailyParts(dailyKwhParts) : escapeXml(dailyKwh)}
      </text>` : ""}
      <text x="${x}" y="${y + subY}" text-anchor="middle"
            fill="var(--hero-sub-text)" class="sv-node-sub">
        ${escapeXml(sub)}
      </text>
      ${socText}
    </g>`;
}

// ---------- primitives ----------

// renderDailyParts — emit a sequence of styled <tspan>s inside the
// daily-totals <text>. Each part has { text, color?, bold?, gap? }.
// `gap` inserts a small space-tspan before the part for separation.
// Used today by the grid bubble to colour-code import (red) vs
// export (green) and bold the magnitudes; other roles still pass a
// plain string via dailyKwh.
function renderDailyParts(parts) {
  return parts.map((p, i) => {
    const fill = p.color ? `fill="${p.color}"` : "";
    const weight = p.bold ? `font-weight="600"` : "";
    const lead = i > 0 ? ` ` : "";
    return `<tspan ${fill} ${weight}>${escapeXml(lead + (p.text || ""))}</tspan>`;
  }).join("");
}

function fmtKw(kw) {
  // Input is kilowatts. Sub-kW values render as plain integer watts
  // (no decimals) so a "30 W" import or "−12 W" battery trickle stays
  // visible as the real number. The visual "idle / balanced"
  // affordances (beam dimming, sub-label text) live on a separate
  // ±FLOW_IDLE_W threshold; this formatter only handles display.
  //
  // Sub-half-watt magnitudes round to 0 W — that's noise, not signal.
  // `+ 0` normalises Math.round's −0 result to plain 0 so a tiny
  // negative reading doesn't print as "-0 W".
  if (Math.abs(kw) < 1) return `${Math.round(kw * 1000) + 0} W`;
  return `${kw.toFixed(2)} kW`;
}

// Collapse multi-planet corners into a single synthesized "combined"
// bubble. Single-planet corners (typical for grid + ev) pass through
// unchanged so toggling between aggregated and individual only moves
// bubbles that actually have siblings to fold. The folded bubble gets
// a stable id (`agg-<corner>`) so particle state can persist across
// /api/status polls the same way it does for individual planets.
function aggregateGroups(groups) {
  const out = {};
  for (const [corner, group] of Object.entries(groups)) {
    if (group.length <= 1) { out[corner] = group.slice(); continue; }
    // First planet's identity drives color + role + title; the kW is
    // summed so the combined beam reflects the quadrant's total flow,
    // and toHub is only true if every underlying planet agrees (mixed
    // directions keep the beam pointing outward, which matches the
    // "net discharge" case for batteries split between charge and
    // discharge — rare, but correct).
    const first = group[0];
    const totalKw = group.reduce((s, p) => s + (p.kw || 0), 0);
    const absKw = Math.abs(totalKw);
    const toHub = group.every(p => p.toHub) ||
                  (!group.some(p => p.toHub) ? false : (totalKw >= 0));
    let sub = first.sub || "";
    const idle = isIdleKw(totalKw);
    if (first.role === "battery") {
      sub = idle ? "idle" : (totalKw >= 0 ? "charging" : "discharging");
    } else if (first.role === "pv") {
      sub = idle ? "idle" : "generating";
    } else if (first.role === "ev") {
      sub = idle ? "idle" : "charging";
    }
    // SoC: simple mean across reporters. Real weighting needs per-
    // battery capacity, which the component doesn't have here — the
    // unweighted mean is close enough for the ambient display; the
    // detailed per-battery SoC lives in the individual view.
    const socs = group.map(p => p.soc).filter(s => s != null);
    const soc = socs.length ? socs.reduce((a, b) => a + b, 0) / socs.length : null;
    // EV-only metadata: chargeLimit / socStale / socSource come from
    // the vehicle driver and only ever appear on EV planets. Aggregating
    // these across multiple EVs would mix unrelated cars, so we only
    // forward them when the group contains exactly one EV reporter
    // (e.g. one wallbox + one paired Tesla). Otherwise drop them and
    // the aggregated bubble renders just the averaged SoC, no badge.
    let chargeLimit = null;
    let socStale = false;
    let socSource = null;
    if (first.role === "ev") {
      const limits = group.map(p => p.chargeLimit).filter(v => v != null && v > 0);
      if (limits.length === 1) chargeLimit = limits[0];
      // socStale is true if ANY underlying reading is stale — better
      // to over-warn than under-warn here.
      socStale = group.some(p => !!p.socStale);
      // socSource: prefer "vehicle" when at least one reporter has BMS
      // truth; otherwise inherit "inferred" from the first.
      const sources = group.map(p => p.socSource).filter(Boolean);
      socSource = sources.includes("vehicle") ? "vehicle"
                : sources.find(s => s === "inferred") || first.socSource || null;
    }
    // Daily-kWh line on the aggregated bubble: prefer the first
    // planet's value (callers push the aggregate-role daily total —
    // e.g. household import_wh, fleet bat_charged_wh — onto every
    // member of the group, so first wins). Falls back to nil silently.
    const dailyKwh = first.dailyKwh || null;
    const dailyKwhParts = first.dailyKwhParts || null;
    out[corner] = [{
      id: `agg-${corner}`,
      corner,
      title: first.title,
      role: first.role,
      kw: totalKw,
      toHub,
      color: idle ? "var(--fg-muted)" : first.color,
      sub,
      soc,
      chargeLimit,
      socStale,
      socSource,
      name: `${group.length}×`,
      aggregated: true,
      dailyKwh,
      dailyKwhParts,
    }];
  }
  return out;
}

// The aggregation toggle is only shown when at least one role has
// more than one planet — single-inverter setups never see the
// control. Counts per role (pv/battery/grid/ev); returns true the
// first time any count clears 2.
function hasMultipleOfAnyType(planets) {
  const counts = {};
  for (const p of planets) {
    if (!p || p.placeholder) continue;
    const r = p.role || "";
    counts[r] = (counts[r] || 0) + 1;
    if (counts[r] > 1) return true;
  }
  return false;
}
function clamp(v, a, b) { return Math.max(a, Math.min(b, v)); }
function escapeXml(s) {
  return String(s).replace(/[<>&"']/g, c =>
    ({ "<": "&lt;", ">": "&gt;", "&": "&amp;", '"': "&quot;", "'": "&apos;" }[c]));
}

customElements.define("ftw-energy-flow", FtwEnergyFlow);
