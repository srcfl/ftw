// <ftw-energy-cake> — donut chart that splits total household
// consumption into "self-consumption" (PV + battery) vs "import"
// (grid), with the surplus EXPORT shown as a separate big number
// next to it. Used by both the "Energy today" panel and the
// "History in numbers" panel under their respective Numbers/Cakes
// toggle.
//
// Inputs are kWh totals over whatever time window the parent picked
// (today, week, month). The component does the percentage maths
// itself so callers can keep blasting raw Wh without conversion.
//
// API:
//   <ftw-energy-cake></ftw-energy-cake>
//   el.setTotals({ import_wh, load_wh, export_wh });
//
// Sign convention: all inputs are non-negative magnitudes (Wh
// integrated over the window). load_wh is total household
// consumption; self-consumption is derived as load_wh − import_wh.

import { FtwElement } from "./ftw-element.js";

const SIZE = 168;       // donut outer diameter (px in viewBox units)
const RING = 28;        // ring thickness
const CX = SIZE / 2;
const CY = SIZE / 2;
const R_OUTER = SIZE / 2 - 2;
const R_INNER = R_OUTER - RING;

class FtwEnergyCake extends FtwElement {
  static styles = `
    :host {
      display: block;
      font-family: var(--sans);
      color: var(--fg);
    }
    .wrap {
      display: grid;
      grid-template-columns: auto 1fr;
      gap: 24px;
      align-items: center;
    }
    .chart-col {
      display: flex;
      flex-direction: column;
      align-items: center;
      gap: 12px;
    }
    svg.donut {
      width: ${SIZE}px;
      height: ${SIZE}px;
      display: block;
    }
    .legend {
      display: flex;
      flex-direction: column;
      gap: 6px;
      width: 100%;
    }
    .legend-row {
      display: grid;
      grid-template-columns: 12px 1fr auto;
      gap: 8px;
      align-items: center;
      font-size: 12px;
    }
    .swatch {
      width: 12px;
      height: 12px;
      border-radius: 3px;
    }
    .legend-label {
      color: var(--fg-dim);
      font-family: var(--mono);
      font-size: 0.7rem;
      letter-spacing: 0.18em;
      text-transform: uppercase;
    }
    .legend-value {
      font-family: var(--mono);
      font-variant-numeric: tabular-nums;
      color: var(--fg);
    }
    .export-col {
      display: flex;
      flex-direction: column;
      align-items: flex-start;
      gap: 4px;
      padding-left: 8px;
      border-left: 1px solid var(--line);
    }
    .export-eyebrow {
      font-family: var(--mono);
      font-size: 0.7rem;
      letter-spacing: 0.18em;
      text-transform: uppercase;
      color: var(--fg-muted);
    }
    .export-number {
      font-family: var(--mono);
      font-variant-numeric: tabular-nums;
      font-size: clamp(1.8rem, 4vw, 2.6rem);
      font-weight: 600;
      color: var(--green-e);
      line-height: 1;
    }
    .export-unit {
      font-family: var(--mono);
      font-size: 0.85rem;
      color: var(--fg-dim);
    }
    .center-pct {
      font-family: var(--mono);
      font-variant-numeric: tabular-nums;
      font-size: 1.6rem;
      font-weight: 600;
      fill: var(--fg);
      text-anchor: middle;
      dominant-baseline: central;
    }
    .center-label {
      font-family: var(--mono);
      font-size: 0.6rem;
      letter-spacing: 0.18em;
      text-transform: uppercase;
      fill: var(--fg-muted);
      text-anchor: middle;
    }
    .empty {
      color: var(--fg-muted);
      font-size: 0.8rem;
      padding: 8px;
    }
    @media (max-width: 600px) {
      .wrap {
        grid-template-columns: 1fr;
        gap: 14px;
      }
      .export-col {
        padding-left: 0;
        border-left: 0;
        border-top: 1px solid var(--line);
        padding-top: 12px;
        align-items: center;
      }
    }
  `;

  constructor() {
    super();
    this._totals = null;
  }

  // Bulk setter — preferred entry point. Pass raw Wh; the component
  // handles unit conversion + percentage maths.
  setTotals(t) {
    this._totals = (t && typeof t === "object") ? t : null;
    this.update();
  }

  render() {
    if (!this._totals) {
      return `<div class="empty">No data yet.</div>`;
    }
    const importWh = Math.max(0, Number(this._totals.import_wh) || 0);
    const loadWh = Math.max(0, Number(this._totals.load_wh) || 0);
    const exportWh = Math.max(0, Number(this._totals.export_wh) || 0);
    // Self-consumption = total load − whatever was sourced from grid.
    // Clamp at 0 so a sensor glitch (import > load reported by the
    // metering driver, which can happen at sub-watt resolution) can
    // never produce a negative slice.
    const selfWh = Math.max(0, loadWh - importWh);
    const total = selfWh + importWh;

    // Empty-state: no consumption recorded. Render the empty donut +
    // export-only number so the layout doesn't jump on first load.
    if (total <= 0) {
      return this._layout({
        slices: this._renderEmpty(),
        centerTop: "—",
        centerSub: "kWh consumed",
        legend: [
          { color: "var(--accent-e)", label: "Self-consumption", value: "—" },
          { color: "var(--red-e)", label: "Imported", value: "—" },
        ],
        exportKwh: exportWh / 1000,
      });
    }

    const selfPct = (selfWh / total) * 100;
    const importPct = 100 - selfPct;

    return this._layout({
      slices: this._renderSlices(selfPct),
      // Centre of the donut now shows TOTAL CONSUMED — the headline
      // "how much energy did the household actually use over this
      // window" number. Self-consumption % still reads off the legend
      // so we don't lose the per-slice context.
      centerTop: formatKwhNum(loadWh / 1000),
      centerSub: "kWh consumed",
      legend: [
        {
          color: "var(--accent-e)",
          label: "Self-consumption",
          value: `${formatKwh(selfWh)} · ${fmtPct(selfPct)}`,
        },
        {
          color: "var(--red-e)",
          label: "Imported",
          value: `${formatKwh(importWh)} · ${fmtPct(importPct)}`,
        },
      ],
      exportKwh: exportWh / 1000,
    });
  }

  _layout({ slices, centerTop, centerSub, legend, exportKwh }) {
    return `
      <div class="wrap">
        <div class="chart-col">
          <svg class="donut" viewBox="0 0 ${SIZE} ${SIZE}" role="img"
               aria-label="Consumption breakdown">
            ${slices}
            <text class="center-pct" x="${CX}" y="${CY - 6}">${centerTop}</text>
            <text class="center-label" x="${CX}" y="${CY + 18}">${centerSub}</text>
          </svg>
          <div class="legend">
            ${legend.map((row) => `
              <div class="legend-row">
                <span class="swatch" style="background:${row.color}"></span>
                <span class="legend-label">${row.label}</span>
                <span class="legend-value">${row.value}</span>
              </div>
            `).join("")}
          </div>
        </div>
        <div class="export-col">
          <span class="export-eyebrow">Exported</span>
          <span class="export-number">${formatKwhNum(exportKwh)}</span>
          <span class="export-unit">kWh sold to grid</span>
        </div>
      </div>
    `;
  }

  _renderEmpty() {
    // Single 100 % muted ring so the donut shape is obvious.
    return ringPath({
      startPct: 0,
      endPct: 100,
      stroke: "var(--line)",
    });
  }

  _renderSlices(selfPct) {
    // Two-slice donut. Self-consumption is the amber/accent slice
    // because it's "yours"; import is the red/--red-e slice because
    // it's the bit you'd want to shrink.
    const self = ringPath({
      startPct: 0,
      endPct: selfPct,
      stroke: "var(--accent-e)",
    });
    const imp = ringPath({
      startPct: selfPct,
      endPct: 100,
      stroke: "var(--red-e)",
    });
    return self + imp;
  }
}

// ringPath — render a single donut arc as a thick stroke between two
// percentages of the full circle. Returns an SVG <path>; concatenate
// multiple paths to compose the donut. Avoiding <circle stroke-dasharray>
// because it doesn't give clean slice boundaries when slices touch.
function ringPath({ startPct, endPct, stroke }) {
  const a = (startPct / 100) * 2 * Math.PI - Math.PI / 2;
  const b = (endPct / 100) * 2 * Math.PI - Math.PI / 2;
  // Full ring (100 %): SVG can't draw an arc that ends where it starts.
  // Split into two semicircles in that case.
  if (endPct - startPct >= 99.999) {
    const m = Math.PI / 2;
    return ringPath({ startPct: 0, endPct: 50, stroke }) +
           ringPath({ startPct: 50, endPct: 100, stroke });
  }
  const r = (R_OUTER + R_INNER) / 2;
  const x1 = CX + r * Math.cos(a);
  const y1 = CY + r * Math.sin(a);
  const x2 = CX + r * Math.cos(b);
  const y2 = CY + r * Math.sin(b);
  const largeArc = endPct - startPct > 50 ? 1 : 0;
  return `<path d="M ${x1} ${y1} A ${r} ${r} 0 ${largeArc} 1 ${x2} ${y2}"
                 fill="none"
                 stroke="${stroke}"
                 stroke-width="${RING}"
                 stroke-linecap="butt"/>`;
}

function formatKwh(wh) {
  const kwh = wh / 1000;
  return `${kwh.toFixed(kwh < 10 ? 2 : 1)} kWh`;
}

function formatKwhNum(kwh) {
  if (!isFinite(kwh)) return "—";
  if (kwh >= 100) return kwh.toFixed(0);
  if (kwh >= 10) return kwh.toFixed(1);
  return kwh.toFixed(2);
}

function fmtPct(p) {
  if (!isFinite(p)) return "—";
  return `${p.toFixed(1)}%`;
}

customElements.define("ftw-energy-cake", FtwEnergyCake);
