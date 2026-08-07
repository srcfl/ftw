// <ftw-bar-chart> — vertical bar chart for small daily/weekly series.
//
// Pure presentational: takes a data array via the .data setter, draws
// one column per entry. Knows nothing about kWh, drivers, or the
// dashboard — accent color and bar layout are configurable. Use it
// anywhere a compact "N values, one per bucket" chart is wanted
// (history cards, MPC slots, twin diagnostics).
//
// Properties:
//   .data = [{ label, value, displayValue?, title? }, ...]
//     label        — short string under the bar (e.g. "Mon 15")
//     value        — numeric, drives bar height (relative to max in set)
//     displayValue — optional pre-formatted string above the bar; falls
//                    back to a 1-decimal short form of value
//     title        — optional tooltip on the column
//
// Attributes:
//   accent       — CSS color for the bars (default var(--cyan))
//   chart-height — px height of the BAR area (not counting value + label
//                  rows above/below). Default 96.
//   bar-max      — px max-width per bar (default 28)
//   loading      — "true" shows a placeholder; data setter clears it
//   empty-text   — text shown when data is empty (default "no data")
//   avg          — "off" to hide the horizontal average line (default on)
//
// Dispatches: nothing (pure dumb).

import { FtwElement } from "./ftw-element.js";

class FtwBarChart extends FtwElement {
  static styles = `
    :host {
      display: block;
      --ftw-bar-color: var(--cyan, #38bdf8);
    }
    .chart {
      display: grid;
      /* Two rows: bar area (which reserves some padding above for value
         tags sitting on top of 100% bars) and the day-label row. */
      grid-template-rows: auto auto;
      gap: 2px;
    }
    .bars-slot {
      /* Extra padding-top is the safe-area where a .val sitting on top
         of a 100%-tall bar can overflow without clipping. Without this
         a bar that happens to be the max would hide its own label.
         box-sizing border-box so the inner grid still gets the full
         --ftw-chart-height for bar math. */
      padding-top: 14px;
      box-sizing: content-box;
    }
    .bar-area {
      position: relative;
      display: grid;
      grid-auto-flow: column;
      grid-auto-columns: minmax(0, 1fr);
      gap: 4px;
      height: var(--ftw-chart-height, 96px);
      align-items: end;
    }
    .lbls {
      display: grid;
      grid-auto-flow: column;
      grid-auto-columns: minmax(0, 1fr);
      gap: 4px;
      align-items: center;
    }
    /* Empty-state filler (no data vs loading placeholder — the latter
       uses the skeleton chart further down, which keeps the full
       layout height so nothing reflows when data arrives). Height
       matches the real chart (bar area + padding + label row) so an
       empty bucket doesn't visually shrink the card either. */
    .placeholder {
      display: flex;
      align-items: center;
      justify-content: center;
      height: calc(var(--ftw-chart-height, 96px) + 14px + 14px);
      color: var(--fg-muted);
      font-family: var(--mono);
      font-size: 11px;
    }
    /* Skeleton loading. Same DOM shape and same outer size as a real
       chart so the card doesn't reflow when data lands. Individual
       bars pulse via a slow opacity shimmer so the user knows the
       component is waiting on I/O rather than blank. The skeleton
       label row relies on a real &nbsp; child for its intrinsic line
       height (matches the loaded state's font-metric-driven row
       height), and the visible "bar" is a ::before overlay — this
       avoids the old 6 px reflow that an explicit \`height: 7px\` on
       the label caused when data arrived. */
    .chart.loading .bar {
      background: var(--fg-muted);
      border-radius: 2px;
      animation: ftw-bar-shimmer 1.5s ease-in-out infinite;
    }
    .chart.loading .lbl {
      color: transparent;
      position: relative;
    }
    .chart.loading .lbl::before {
      content: '';
      position: absolute;
      left: 20%;
      right: 20%;
      top: 50%;
      height: 7px;
      margin-top: -3.5px;
      background: var(--fg-muted);
      border-radius: 2px;
      animation: ftw-bar-shimmer 1.5s ease-in-out infinite;
    }
    .chart.loading .val { visibility: hidden; }
    @keyframes ftw-bar-shimmer {
      0%, 100% { opacity: 0.10; }
      50%      { opacity: 0.22; }
    }
    /* Fade-in for the real chart when loading completes. Triggers on
       EVERY render of the loaded state — a tab switch (Week↔Month)
       that re-renders the chart gets the same soft cross-fade from
       the preceding skeleton, so the visual transition is smooth
       even though the underlying DOM is torn down + rebuilt. */
    .chart.loaded { animation: ftw-bar-fade-in 260ms ease-out; }
    @keyframes ftw-bar-fade-in {
      from { opacity: 0; }
      to   { opacity: 1; }
    }
    .col {
      position: relative;
      display: flex;
      align-items: flex-end;
      justify-content: center;
      height: 100%;
      min-width: 0;
      /* Let .val overflow upward into the bars-slot padding when its
         bar is at the top of the chart — otherwise the tallest bar's
         value would get clipped. */
      overflow: visible;
    }
    .bar {
      width: 100%;
      max-width: var(--ftw-bar-max, 28px);
      background: var(--ftw-bar-color);
      border-radius: 2px 2px 0 0;
      min-height: 1px;
      transition: height 0.3s ease;
    }
    /* Value tag sticks to the TOP of its own bar.
       \`bottom: N%\` pins its bottom edge to the top of the bar (both
       percentages resolve against the same bar-area height), so the
       label follows the tip up and down. */
    .val {
      position: absolute;
      left: 0;
      right: 0;
      text-align: center;
      font-family: var(--mono);
      font-size: 9px;
      color: var(--fg-muted);
      line-height: 1;
      padding-bottom: 2px;
      white-space: nowrap;
      transition: bottom 0.3s ease;
      pointer-events: none;
    }
    .lbl {
      font-family: var(--mono);
      font-size: 9px;
      color: var(--fg-muted);
      text-align: center;
      white-space: nowrap;
    }
    /* Horizontal dashed line marking the arithmetic mean of the
       series. Positioned from the bottom of .bar-area so
       \`bottom: N%\` tracks exactly the same reference as every bar's
       \`height: N%\`. Color sits at --fg-dim (one step brighter than
       --fg-muted) with opacity 0.85, which gives enough contrast to
       read against both an empty baseline and a tall active bar
       without becoming a second visual focal point next to the bars.
       The numeric value lives next to the card total (e.g.
       "41.7 kWh total (5.9 kWh avg)") so there's no separate tag in
       the plot area anymore. */
    .avg-line {
      position: absolute;
      left: 0;
      right: 0;
      height: 0;
      border-top: 1px dashed var(--fg-dim);
      opacity: 0.85;
      pointer-events: none;
    }
    /* Optional dashed overlay line (e.g. STRÅNG-expected PV vs the
       produced bars). An SVG stretched to fill .bar-area, plotted in a
       0..100 viewBox so it needs no pixel math against the CSS grid;
       non-scaling-stroke keeps the dash crisp despite the stretch. */
    .overlay-line {
      position: absolute;
      inset: 0;
      width: 100%;
      height: 100%;
      pointer-events: none;
      overflow: visible;
    }
    .overlay-line polyline {
      fill: none;
      stroke: var(--ftw-overlay-color, var(--amber, #f59e0b));
      stroke-width: 1.5;
      stroke-dasharray: 4 3;
      vector-effect: non-scaling-stroke;
      opacity: 0.9;
    }
  `;

  static get observedAttributes() {
    return ["accent", "chart-height", "bar-max", "loading", "empty-text", "avg"];
  }

  constructor() {
    super();
    this._data = [];
    this._overlay = null;
  }

  attributeChangedCallback() { this.update(); }

  // Bulk setter — preferred update path. Replaces the whole series.
  set data(arr) {
    this._data = Array.isArray(arr) ? arr : [];
    this.removeAttribute("loading");
    this.update();
  }
  get data() { return this._data; }

  // Optional dashed overlay line, index-aligned with .data. Shape:
  //   { values: (number|null)[], color?: string }
  // values[i] is plotted above column i on the SAME axis as the bars
  // (nulls / non-finite entries break the line). Set null to remove.
  set overlay(o) {
    this._overlay = o && Array.isArray(o.values) ? o : null;
    this.update();
  }
  get overlay() { return this._overlay; }

  render() {
    const accent = this.getAttribute("accent");
    const height = this.getAttribute("chart-height");
    const barMax = this.getAttribute("bar-max");
    if (accent) this.style.setProperty("--ftw-bar-color", accent);
    if (height) this.style.setProperty("--ftw-chart-height", height + "px");
    if (barMax) this.style.setProperty("--ftw-bar-max", barMax + "px");

    const loading = this.getAttribute("loading") === "true";
    if (loading) {
      // Skeleton bars: 7 fake columns at varying heights. Same outer
      // structure (.chart > .bars-slot > .bar-area + .lbls) as the
      // loaded state so the card doesn't shift a single pixel when the
      // fetch resolves. Heights are deterministic (indices, not
      // Math.random) so the skeleton looks identical across repaints
      // — avoids a twitchy "dice-rolling" vibe while the tab sits in
      // a loading state.
      const phantom = [35, 80, 55, 95, 40, 70, 25];
      const phantomCols = phantom.map((h) =>
        `<div class="col">` +
        `<div class="bar" style="height:${h}%"></div>` +
        `</div>`
      ).join("");
      const phantomLbls = phantom.map(() =>
        // &nbsp; gives the skeleton label the same intrinsic line
        // height as a real label with "Mon 15" text — without it
        // the .lbls row collapses ~6 px shorter than the loaded
        // state and the card jumps up when data arrives.
        `<span class="lbl">&nbsp;</span>`
      ).join("");
      return `
        <div class="chart loading" aria-busy="true">
          <div class="bars-slot">
            <div class="bar-area">${phantomCols}</div>
          </div>
          <div class="lbls">${phantomLbls}</div>
        </div>
      `;
    }
    if (!this._data.length) {
      const empty = this.getAttribute("empty-text") || "no data";
      return `<div class="placeholder">${escapeHtml(empty)}</div>`;
    }

    let max = 0, sum = 0, count = 0;
    for (const d of this._data) {
      const v = Number(d.value) || 0;
      if (v > max) max = v;
      sum += v;
      count++;
    }
    const avg = count > 0 ? sum / count : 0;

    // Fold overlay values into the axis max so the expected line and the
    // produced bars share one scale (an expected level above the tallest
    // bar must still fit in-frame).
    const overlayVals = this._overlay ? this._overlay.values : null;
    if (overlayVals) {
      for (const ov of overlayVals) {
        const n = Number(ov);
        if (isFinite(n) && n > max) max = n;
      }
    }

    const colsSvg = this._data.map((d) => {
      const v = Number(d.value) || 0;
      // 2% floor keeps tiny-but-nonzero values visible; gate on v>0 so
      // truly-zero buckets render as empty columns, not 2% slivers.
      const pct = max > 0 && v > 0 ? Math.max(2, (v / max) * 100) : 0;
      const display = d.displayValue != null ? String(d.displayValue) : shortNum(v);
      const title = d.title != null ? String(d.title) : `${d.label || ""}: ${display}`;
      // The .val's \`bottom\` matches the bar's \`height\` so the tag
      // floats exactly on top of each bar. For zero buckets the tag
      // sits at the baseline.
      return `<div class="col" title="${escapeHtml(title)}">` +
             `<span class="val" style="bottom:${pct}%">${escapeHtml(display)}</span>` +
             `<div class="bar" style="height:${pct}%"></div>` +
             `</div>`;
    }).join("");

    const lblsSvg = this._data.map((d) =>
      `<span class="lbl">${escapeHtml(d.label || "")}</span>`
    ).join("");

    // Average overlay. Suppressed when avg="off" or when the average
    // rounds to zero (placing a line at the baseline adds noise).
    // Suppressed with only one sample too — "mean of one value" just
    // draws a line clipped to the top of the tallest bar.
    const avgOn = this.getAttribute("avg") !== "off";
    let avgOverlay = "";
    if (avgOn && max > 0 && avg > 0 && count > 1) {
      const avgPct = Math.min(100, (avg / max) * 100);
      // Numeric value is shown in the card header (next to the
      // total), not in the plot area — so all we need here is the
      // line itself. A title attribute on the line gives a cursor
      // tooltip for anyone who wants to confirm the exact number.
      const display = shortNum(avg);
      avgOverlay =
        `<div class="avg-line" style="bottom:${avgPct}%" ` +
        `title="average ${display}"></div>`;
    }

    // Dashed overlay line (expected series). Plotted in a 0..100 viewBox
    // stretched to fill .bar-area: x centers each column, y is the value
    // as a percentage of the shared max (inverted — SVG y grows downward).
    let overlayLine = "";
    if (overlayVals && max > 0) {
      const n = this._data.length;
      const pts = [];
      for (let i = 0; i < n; i++) {
        const val = Number(overlayVals[i]);
        if (!isFinite(val)) continue;
        const x = n > 1 ? (i + 0.5) / n * 100 : 50;
        const y = Math.max(0, Math.min(100, 100 - (val / max) * 100));
        pts.push(`${x.toFixed(2)},${y.toFixed(2)}`);
      }
      if (pts.length >= 2) {
        const color = this._overlay.color;
        if (color) this.style.setProperty("--ftw-overlay-color", color);
        overlayLine =
          `<svg class="overlay-line" viewBox="0 0 100 100" ` +
          `preserveAspectRatio="none" aria-hidden="true">` +
          `<polyline points="${pts.join(" ")}"></polyline></svg>`;
      }
    }

    return `
      <div class="chart loaded">
        <div class="bars-slot">
          <div class="bar-area">${colsSvg}${avgOverlay}${overlayLine}</div>
        </div>
        <div class="lbls">${lblsSvg}</div>
      </div>
    `;
  }
}

function shortNum(v) {
  const a = Math.abs(v);
  if (a >= 100) return v.toFixed(0);
  if (a >= 10)  return v.toFixed(1);
  return v.toFixed(2);
}
function escapeHtml(s) {
  return String(s).replace(/[<>&"']/g, (c) =>
    ({ "<": "&lt;", ">": "&gt;", "&": "&amp;", '"': "&quot;", "'": "&#39;" }[c]));
}

customElements.define("ftw-bar-chart", FtwBarChart);
