// <ftw-price-chart> — full-width bar chart of known electricity spot
// prices (next 48 h or so), with a toggle to include VAT (default on)
// and hover tooltip per slot. Peaks + lows are marked. Self-fetching:
// hits /api/prices and /api/config on connect, polls /api/prices
// every 5 min after that.
//
// Inputs (none — autonomous). The component renders its own header
// (label + VAT toggle) and the SVG chart underneath.
//
// Data shape from /api/prices:
//   { zone: "SE4", enabled: true, items: [
//       { slot_ts_ms, slot_len_min, spot_ore_kwh, total_ore_kwh, ... }
//     ] }
//
// Sweden VAT rate (25 %) is read from /api/config price.vat_percent
// when available; falls back to 25 for the toggle math when the
// config endpoint is missing.

import { FtwElement } from "./ftw-element.js";
import { apiFetch } from "./api-fetch.js";
import {
  buildCompactPriceView,
  formatPriceSlotLabel,
} from "./price-summary.js";

class FtwPriceChart extends FtwElement {
  static styles = `
    :host {
      display: block;
      font-family: var(--sans);
      color: var(--fg);
    }
    .head {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 10px;
      gap: 12px;
      flex-wrap: wrap;
    }
    .label {
      font-family: var(--mono);
      font-size: 0.7rem;
      font-weight: 500;
      letter-spacing: 0.18em;
      text-transform: uppercase;
      color: var(--fg-muted);
    }
    .meta {
      font-family: var(--mono);
      font-size: 11px;
      color: var(--fg-dim);
    }
    .meta-stats {
      display: flex;
      gap: 0.9rem;
      row-gap: 0.35rem;
      flex-wrap: wrap;
      margin-top: 0.15rem;
      /* line-height 1 keeps each "now / low / high / avg" item tight
         vertically so when the row wraps onto two lines on a phone the
         line-spacing comes from row-gap, not from per-item baseline
         leading (which otherwise stacked the wrapped row too far down). */
      line-height: 1;
    }
    .meta-stats .meta-label {
      text-transform: uppercase;
      letter-spacing: 0.06em;
      color: var(--fg-muted);
      margin-right: 0.18em;
    }
    .meta-stats span { white-space: nowrap; }
    .toggle {
      position: relative;
      display: inline-grid;
      grid-auto-flow: column;
      grid-auto-columns: minmax(0, 1fr);
      border: 1px solid var(--line);
      border-radius: 999px;
      background: var(--ink-sunken);
      padding: 2px;
      isolation: isolate;
    }
    .toggle::before {
      content: '';
      position: absolute;
      top: 2px; bottom: 2px;
      left: 2px;
      width: calc(50% - 2px);
      background: var(--accent-e);
      border-radius: 999px;
      transform: translateX(0);
      transition: transform 240ms cubic-bezier(0.4, 0, 0.2, 1);
      z-index: 0;
    }
    .toggle[data-vat="off"]::before {
      transform: translateX(100%);
    }
    /* Horizon pill is a 3-position selector (Today / +Tomorrow / Tomorrow);
       slider width is 1/3 of the inner area instead of the default 1/2. */
    .toggle[data-horizon]::before {
      width: calc((100% - 4px) / 3);
    }
    .toggle[data-horizon="all"]::before      { transform: translateX(100%); }
    .toggle[data-horizon="tomorrow"]::before { transform: translateX(200%); }
    .toggles {
      display: flex;
      gap: 8px;
      flex-wrap: wrap;
      align-items: center;
    }
    .toggle button {
      position: relative;
      z-index: 1;
      background: transparent;
      border: 0;
      color: var(--fg-dim);
      font-family: var(--mono);
      font-size: 10px;
      font-weight: 500;
      letter-spacing: 0.18em;
      text-transform: uppercase;
      padding: 4px 14px;
      cursor: pointer;
      transition: color 220ms ease;
    }
    .toggle button.active { color: #0a0a0a; }
    .toggle button:not(.active):hover { color: var(--fg); }

    .chart-wrap {
      position: relative;
    }
    svg.chart {
      width: 100%;
      display: block;
      user-select: none;
      -webkit-user-select: none;
      -webkit-touch-callout: none;
      /* Allow normal vertical page scroll when the touch starts on the
         chart. Horizontal gestures are reserved for scrubbing — we win
         them by calling preventDefault on touchmove once long-press
         has fired. */
      touch-action: pan-y;
    }
    .scrub-cursor {
      pointer-events: none;
      transition: opacity 80ms ease;
    }
    .empty {
      color: var(--fg-muted);
      font-size: 0.85rem;
      padding: 24px 8px;
      text-align: center;
    }
    /* Tooltip — absolutely positioned, follows the cursor's slot. */
    .tip {
      position: absolute;
      pointer-events: none;
      background: var(--ink-raised);
      border: 1px solid var(--line);
      border-radius: 6px;
      padding: 8px 10px;
      font-family: var(--mono);
      font-size: 12px;
      color: var(--fg);
      transform: translate(-50%, -110%);
      white-space: nowrap;
      opacity: 0;
      transition: opacity 80ms;
      z-index: 5;
    }
    .tip.visible { opacity: 1; }
    .tip-time {
      color: var(--fg-dim);
      margin-bottom: 2px;
    }
    .tip-price {
      font-size: 14px;
      font-weight: 600;
    }
    .tip-price.peak  { color: var(--red-e); }
    .tip-price.low   { color: var(--green-e); }

    .compact-head {
      display: flex;
      align-items: start;
      justify-content: space-between;
      gap: 14px;
      margin-bottom: 14px;
    }
    .compact-kicker {
      margin-bottom: 3px;
      color: var(--accent-e);
      font-family: var(--mono);
      font-size: 9px;
      font-weight: 700;
      letter-spacing: 0.18em;
      text-transform: uppercase;
    }
    .compact-title {
      color: var(--fg);
      font-family: var(--sans);
      font-size: 16px;
      font-weight: 700;
      letter-spacing: -0.015em;
    }
    .compact-link,
    .compact-setup {
      color: var(--accent-e);
      font-family: var(--mono);
      font-size: 10px;
      font-weight: 650;
      letter-spacing: 0.08em;
      text-decoration: none;
      text-transform: uppercase;
    }
    .compact-link {
      padding-top: 3px;
      white-space: nowrap;
    }
    .compact-link:hover,
    .compact-setup:hover {
      color: var(--fg);
    }
    .compact-link:focus-visible,
    .compact-setup:focus-visible {
      outline: 2px solid var(--accent-e);
      outline-offset: 4px;
      border-radius: 2px;
    }
    .compact-summary {
      display: grid;
      grid-template-columns: minmax(0, 1.2fr) minmax(130px, 0.8fr);
      align-items: end;
      gap: 18px;
    }
    .compact-current {
      display: grid;
      grid-template-columns: auto 1fr;
      align-items: baseline;
      column-gap: 7px;
    }
    .compact-value {
      color: var(--fg);
      font-family: var(--mono);
      font-size: clamp(2rem, 5vw, 3.15rem);
      font-weight: 750;
      font-variant-numeric: tabular-nums;
      letter-spacing: -0.065em;
      line-height: 0.95;
    }
    .compact-unit {
      color: var(--fg-dim);
      font-family: var(--mono);
      font-size: 11px;
      white-space: nowrap;
    }
    .compact-meta {
      grid-column: 1 / -1;
      margin-top: 6px;
      color: var(--fg-muted);
      font-family: var(--mono);
      font-size: 10px;
      letter-spacing: 0.04em;
    }
    .compact-low {
      display: flex;
      min-width: 0;
      flex-direction: column;
      padding-left: 16px;
      border-left: 1px solid var(--line);
    }
    .compact-low > span {
      color: var(--fg-label);
      font-family: var(--mono);
      font-size: 9px;
      letter-spacing: 0.1em;
      text-transform: uppercase;
    }
    .compact-low strong {
      margin-top: 3px;
      color: var(--green-e);
      font-family: var(--mono);
      font-size: 1rem;
      font-variant-numeric: tabular-nums;
    }
    .compact-low small {
      overflow: hidden;
      margin-top: 1px;
      color: var(--fg-muted);
      font-family: var(--mono);
      font-size: 10px;
      text-overflow: ellipsis;
      white-space: nowrap;
    }
    .compact-profile {
      display: block;
      width: 100%;
      height: 58px;
      margin-top: 16px;
      overflow: visible;
    }
    .compact-profile rect {
      fill: var(--fg-muted);
      opacity: 0.38;
    }
    .compact-profile rect.is-current {
      fill: var(--accent-e);
      opacity: 1;
    }
    .compact-profile rect.is-low {
      fill: var(--green-e);
      opacity: 0.9;
    }
    .compact-profile line {
      stroke: var(--line);
      stroke-width: 1;
    }
    .compact-stale,
    .compact-profile-empty {
      display: block;
      margin-top: 8px;
      color: var(--fg-muted);
      font-family: var(--mono);
      font-size: 10px;
    }
    .compact-stale {
      color: var(--amber);
    }
    .compact-empty {
      display: flex;
      min-height: 126px;
      flex-direction: column;
      align-items: flex-start;
      justify-content: center;
      gap: 9px;
      color: var(--fg-muted);
    }
    .compact-empty strong {
      color: var(--fg-dim);
      font-family: var(--mono);
      font-size: 15px;
    }

    /* Phone layout — the JS picks a taller viewBox H on small screens
       so bars get more vertical room WITHOUT vertically stretching
       text (which made labels like "NOW" look horizontally squeezed
       relative to their height). The SVG's intrinsic ratio handles
       sizing — no CSS aspect-ratio needed. The tooltip is pinned
       above the bars so it never covers the data being read. */
    @media (max-width: 600px) {
      .chart-wrap { padding-top: 40px; }
      .tip {
        transform: translate(-50%, 0);
        transition: opacity 80ms, left 120ms cubic-bezier(.4, 0, .2, 1);
      }
      .compact-summary {
        grid-template-columns: minmax(0, 1fr) minmax(112px, 0.75fr);
        gap: 12px;
      }
      .compact-value {
        font-size: 2.15rem;
      }
      .compact-low {
        padding-left: 12px;
      }
    }
  `;

  static get observedAttributes() {
    return ["compact"];
  }

  constructor() {
    super();
    this._data = null;        // { zone, items: [{tsMs, ore}], min, max, vatPct }
    this._priceState = "loading";
    this._vatOn = readVatPref(); // persisted across reloads, defaults to true
    this._horizon = readHorizonPref(); // "today" or "all", persisted
    this._refreshTimer = null;
    this._hover = null;       // { idx, x, y } during hover
    this._vatPct = 25;        // fallback; overwritten from /api/config
    this._geom = null;        // { padL, plotW, n, W } — set in _renderChart
    this._isTouching = false; // suppresses synthesized mouse events after touch
  }

  attributeChangedCallback() {
    this.update();
  }

  connectedCallback() {
    super.connectedCallback();
    this._loadConfig();
    this._loadPrices();
    this._refreshTimer = setInterval(() => this._loadPrices(), 5 * 60 * 1000);
    // Re-render when the viewport crosses the small-screen breakpoint
    // — render() picks a different viewBox H per side, so a rotation
    // or window-resize over the 600 px line needs a redraw.
    if (typeof window !== "undefined" && window.matchMedia) {
      this._mql = window.matchMedia("(max-width: 600px)");
      this._mqlListener = () => this.update();
      if (this._mql.addEventListener) this._mql.addEventListener("change", this._mqlListener);
      else if (this._mql.addListener) this._mql.addListener(this._mqlListener);
    }
    this._vatSyncListener = (event) => {
      const next = event && event.detail && event.detail.vatOn;
      if (typeof next !== "boolean" || next === this._vatOn) return;
      this._vatOn = next;
      this.update();
    };
    window.addEventListener("ftw-price-vat-change", this._vatSyncListener);
  }

  disconnectedCallback() {
    if (this._refreshTimer) {
      clearInterval(this._refreshTimer);
      this._refreshTimer = null;
    }
    if (this._mql && this._mqlListener) {
      if (this._mql.removeEventListener) this._mql.removeEventListener("change", this._mqlListener);
      else if (this._mql.removeListener) this._mql.removeListener(this._mqlListener);
      this._mql = null;
      this._mqlListener = null;
    }
    if (this._vatSyncListener) {
      window.removeEventListener("ftw-price-vat-change", this._vatSyncListener);
      this._vatSyncListener = null;
    }
  }

  async _loadConfig() {
    try {
      const r = await apiFetch("/api/config");
      const j = await r.json();
      const v = j && j.price && j.price.vat_percent;
      if (typeof v === "number" && v > 0) {
        this._vatPct = v;
        this.update();
      }
    } catch (e) { /* ignore — fallback 25 % is fine */ }
  }

  async _loadPrices() {
    try {
      // since_ms = local midnight today so past slots stay visible (the
      // chart should read like a calendar, not a sliding window). The
      // API's default lookback is only 1 h, which dropped the morning
      // off as the day progressed.
      const midnight = new Date();
      midnight.setHours(0, 0, 0, 0);
      const since = midnight.getTime();
      const until = Date.now() + 48 * 3600_000;
      const r = await apiFetch(`/api/prices?since_ms=${since}&until_ms=${until}`);
      if (r.ok === false) throw new Error(`Price request failed: ${r.status}`);
      const j = await r.json();
      if (j && j.enabled === false) {
        this._data = null;
        this._priceState = "unconfigured";
      } else if (!j || !Array.isArray(j.items)) {
        throw new Error("Price response did not include items");
      } else {
        // Items already carry both spot_ore_kwh and total_ore_kwh, but
        // the operator's mental model is "spot price ± VAT" — so we
        // base on spot and let the toggle add VAT. Keeps the toggle
        // semantics honest (it's NOT a tariff/grid-fee toggle, just
        // VAT) and matches the API spec the user asked for.
        const items = j.items.map((it) => ({
          tsMs:  Number(it.slot_ts_ms) || 0,
          lenMin: Number(it.slot_len_min) || 60,
          spot:  Number(it.spot_ore_kwh) || 0,
        })).sort((a, b) => a.tsMs - b.tsMs);
        this._data = { zone: j.zone || "", items };
        this._priceState = "ready";
      }
      this.update();
    } catch (e) {
      this._priceState = this._data ? "stale" : "error";
      this.update();
    }
  }

  // Resolved öre/kWh per slot for the active toggle.
  _priceFor(item) {
    return this._vatOn
      ? item.spot * (1 + this._vatPct / 100)
      : item.spot;
  }

  render() {
    if (this.hasAttribute("compact")) return this._renderCompact();
    const data = this._data;
    const hasTomorrow = data ? itemsIncludeTomorrow(data.items) : false;
    // No tomorrow data → no choice to make, so the toggle is hidden
    // and the effective horizon is forced to "today" regardless of
    // the stored preference. The stored value is kept untouched so the
    // user's choice re-applies once tomorrow's prices publish.
    const effectiveHorizon = hasTomorrow ? this._horizon : "today";
    const vatLabel = this._vatOn ? "incl. VAT" : "spot only";
    const horizonLabel =
      effectiveHorizon === "today"    ? "today" :
      effectiveHorizon === "tomorrow" ? "tomorrow" :
                                        "today + tomorrow";
    const horizonToggleHtml = hasTomorrow ? `
          <div class="toggle" role="tablist" data-horizon="${effectiveHorizon}">
            <button type="button" data-horizon="today"    class="${effectiveHorizon === "today" ? "active" : ""}"    aria-selected="${effectiveHorizon === "today"}">Today</button>
            <button type="button" data-horizon="all"      class="${effectiveHorizon === "all"   ? "active" : ""}"    aria-selected="${effectiveHorizon === "all"}">+ Tomorrow</button>
            <button type="button" data-horizon="tomorrow" class="${effectiveHorizon === "tomorrow" ? "active" : ""}" aria-selected="${effectiveHorizon === "tomorrow"}">Tomorrow</button>
          </div>` : "";
    // Filter first so the stats row reflects the active horizon.
    let visible = [];
    if (data) {
      if (effectiveHorizon === "today")         visible = filterToday(data.items);
      else if (effectiveHorizon === "tomorrow") visible = filterTomorrow(data.items);
      else                                      visible = data.items;
    }
    // Compute stats over the visible window using the consumer-resolved
    // öre/kWh (spot + grid + VAT, matching whichever toggle is active),
    // so the numbers in the subtitle line up exactly with what the
    // chart bars are showing. `current` is the slot covering wall-clock
    // now if it's in the window, else the nearest. Empty horizon → no
    // stats row, falls through to the existing "no data" message.
    let statsHtml = "";
    if (visible.length > 0) {
      const prices = visible.map(it => this._priceFor(it));
      const now = Date.now();
      let curIdx = visible.findIndex(it => {
        const start = (it.tsMs || 0);
        const end   = start + 60 * 60 * 1000;
        return now >= start && now < end;
      });
      if (curIdx < 0) {
        // Nearest by absolute time delta (used when "Tomorrow" tab is
        // active and the wall clock is still in today, etc.).
        let best = -1, bestD = Infinity;
        for (let i = 0; i < visible.length; i++) {
          const start = new Date(visible[i].starts_at || visible[i].ts || 0).getTime();
          const d = Math.abs(start - now);
          if (d < bestD) { bestD = d; best = i; }
        }
        curIdx = best;
      }
      const cur = curIdx >= 0 ? prices[curIdx] : null;
      const lo = Math.min(...prices);
      const hi = Math.max(...prices);
      const avg = prices.reduce((a, b) => a + b, 0) / prices.length;
      const fmt = v => (v == null ? "—" : v.toFixed(1) + " öre");
      statsHtml = `
          <div class="meta meta-stats">
            <span><span class="meta-label">now</span> ${fmt(cur)}</span>
            <span><span class="meta-label">low</span> ${fmt(lo)}</span>
            <span><span class="meta-label">high</span> ${fmt(hi)}</span>
            <span><span class="meta-label">avg</span> ${fmt(avg)}</span>
          </div>
      `;
    }
    const head = `
      <div class="head">
        <div>
          <div class="label">Electricity prices</div>
          <div class="meta">${data ? `${escapeXml(data.zone)} · ${vatLabel} · ${horizonLabel}` : "—"}</div>
          ${statsHtml}
        </div>
        <div class="toggles">
          <div class="toggle" role="tablist" data-vat="${this._vatOn ? "on" : "off"}">
            <button type="button" data-vat="on"  class="${this._vatOn ? "active" : ""}" aria-selected="${this._vatOn}">Incl. VAT</button>
            <button type="button" data-vat="off" class="${!this._vatOn ? "active" : ""}" aria-selected="${!this._vatOn}">Spot</button>
          </div>${horizonToggleHtml}
        </div>
      </div>
    `;
    if (!data || !data.items.length) {
      return head + `<div class="empty">No price data available.</div>`;
    }
    if (!visible.length) {
      const which = effectiveHorizon === "tomorrow" ? "tomorrow" : "today";
      return head + `<div class="empty">No price data for ${which}.</div>`;
    }
    return head + this._renderChart({ ...data, items: visible });
  }

  _renderCompact() {
    const data = this._data;
    const view = buildCompactPriceView({
      state: this._priceState,
      items: data && data.items,
      now: Date.now(),
      vatOn: this._vatOn,
      vatPercent: this._vatPct,
    });
    const head = `
      <div class="compact-head">
        <div>
          <div class="compact-kicker">Market now</div>
          <div class="compact-title">Electricity price</div>
        </div>
        <a class="compact-link" href="#energy">Full view <span aria-hidden="true">→</span></a>
      </div>
    `;
    if (view.kind !== "ready") {
      const settings = view.kind === "unconfigured"
        ? `<a class="compact-setup" href="#more">Open price settings</a>`
        : "";
      return `${head}
        <div class="compact-empty" role="status">
          <strong>${escapeXml(view.message)}</strong>
          ${settings}
        </div>
      `;
    }

    const summary = view.summary;
    const formatOre = (value) => {
      if (!Number.isFinite(value)) return "—";
      const digits = Math.abs(value) >= 100 ? 0 : 1;
      return Number(value).toFixed(digits);
    };
    const currentValue = summary.current ? formatOre(summary.current.ore) : "—";
    const lowValue = summary.nextLow ? formatOre(summary.nextLow.ore) : "—";
    const lowTime = summary.nextLow
      ? formatPriceSlotLabel(summary.nextLow.tsMs)
      : "No later slot published";
    const vatLabel = this._vatOn ? "incl. VAT" : "spot only";
    const stale = view.stale
      ? `<span class="compact-stale">Last update failed</span>`
      : "";
    const accessible = summary.current
      ? `Current electricity price ${currentValue} öre per kilowatt-hour. Next low ${lowValue} öre at ${lowTime}.`
      : `No current electricity price slot. Next low ${lowValue} öre at ${lowTime}.`;

    return `${head}
      <div class="compact-summary" aria-label="${escapeXml(accessible)}">
        <div class="compact-current">
          <span class="compact-value">${currentValue}</span>
          <span class="compact-unit">öre/kWh</span>
          <span class="compact-meta">${escapeXml(data.zone || "—")} · ${vatLabel}</span>
        </div>
        <div class="compact-low">
          <span>Next low</span>
          <strong>${lowValue} öre</strong>
          <small>${escapeXml(lowTime)}</small>
        </div>
      </div>
      ${this._renderCompactProfile(summary)}
      ${stale}
    `;
  }

  _renderCompactProfile(summary) {
    const items = summary.today;
    if (!items.length) {
      return `<div class="compact-profile-empty">Today’s profile is not published yet.</div>`;
    }
    const W = 360;
    const H = 58;
    const pad = 2;
    const values = items.map((item) => item.ore);
    const min = Math.min(0, ...values);
    const max = Math.max(0, ...values);
    const range = Math.max(1, max - min);
    const y = (value) => pad + ((max - value) / range) * (H - pad * 2);
    const zeroY = y(0);
    const slotW = W / items.length;
    const bars = items.map((item, index) => {
      const valueY = y(item.ore);
      const top = Math.min(zeroY, valueY);
      const height = Math.max(1, Math.abs(zeroY - valueY));
      const current = summary.current && summary.current.tsMs === item.tsMs;
      const low = summary.nextLow && summary.nextLow.tsMs === item.tsMs;
      const className = current ? "is-current" : low ? "is-low" : "";
      return `<rect class="${className}" x="${(index * slotW + 1).toFixed(2)}"
                    y="${top.toFixed(2)}" width="${Math.max(1, slotW - 2).toFixed(2)}"
                    height="${height.toFixed(2)}" />`;
    }).join("");
    return `
      <svg class="compact-profile" viewBox="0 0 ${W} ${H}" preserveAspectRatio="none"
           role="img" aria-label="Today’s electricity price profile">
        <line x1="0" x2="${W}" y1="${zeroY.toFixed(2)}" y2="${zeroY.toFixed(2)}" />
        ${bars}
      </svg>
    `;
  }

  _renderChart(data) {
    // Compute prices, min/max, and the indices of the lowest +
    // highest slots for the marker overlays.
    const items = data.items;
    const n = items.length;
    const prices = items.map((it) => this._priceFor(it));
    let lo = 0, hi = 0;
    for (let i = 1; i < n; i++) {
      if (prices[i] < prices[lo]) lo = i;
      if (prices[i] > prices[hi]) hi = i;
    }
    const minP = prices[lo];
    const maxP = prices[hi];
    const meanP = prices.reduce((a, p) => a + p, 0) / n;

    // SVG geometry. Width = 100 % via viewBox. Height of the viewBox
    // is doubled on phones so bars get more vertical room — bumping
    // the box AT THE VIEWBOX level (not via a mismatched CSS
    // aspect-ratio + preserveAspectRatio="none") keeps text scaled
    // uniformly. preserveAspectRatio="none" is harmless when the box
    // and viewBox match.
    const W = 1000;
    const small = typeof window !== "undefined" && window.matchMedia &&
      window.matchMedia("(max-width: 600px)").matches;
    const H = small ? 720 : 240;
    // Wider left padding so the y-axis öre labels have breathing
    // room between the SVG edge and the plot's first bar (was 36 →
    // labels rendered too close to the card's left border).
    // Phones get bigger fonts AND more padding so the larger labels
    // stay inside the SVG box and below the NOW pill clears its top.
    // +4 px left padding so 3-digit öre prices (e.g. "234 ö") clear the
    // SVG edge — the label is text-anchored "end" at `pad.l - 4` and
    // extends left from there, so a tighter pad.l clipped large prices.
    const pad = small
      ? { t: 26, r: 16, b: 40, l: 84 }
      : { t: 16, r: 16, b: 28, l: 60 };
    // Phone sizes bumped per operator request (2026-05): axis labels
    // were readable but the NOW marker felt thin and crowded against
    // the bars. +50 % on axes, +33 % on NOW + thicker stroke so the
    // current hour reads at-a-glance from across a room.
    const fsAxis = small ? 27 : 10;  // y-axis öre + x-axis time
    const fsNow  = small ? 24 : 10;  // NOW label
    const fsMark = small ? 26 : 11;  // peak/low ▼▲ glyphs
    const nowStrokeW = small ? 3 : 1.5;
    // Tick density drops from every 3 h to every 6 h on phones so the
    // bigger labels don't overlap each other across a 48 h chart.
    const tickStepMs = (small ? 6 : 3) * 3600_000;
    const tickLabelDy = small ? 26 : 16;
    const plotW = W - pad.l - pad.r;
    const plotH = H - pad.t - pad.b;
    const barW = plotW / n;
    // Geometry stash for hit-testing from raw clientX during touch
    // scrubbing — touchmove targets stay anchored to the touchstart
    // element, so we can't lean on data-idx like the mouse path does.
    this._geom = { padL: pad.l, plotW, n, W };
    // Y scale: include 0 so a negative-spot day still renders, and
    // pad the top so the peak's marker doesn't kiss the edge.
    const yMin = Math.min(0, minP);
    const yMax = Math.max(maxP * 1.08, 1);
    const yToPx = (v) => pad.t + plotH - ((v - yMin) / (yMax - yMin)) * plotH;
    const zeroY = yToPx(0);
    const meanY = yToPx(meanP);

    // "Now" vertical line — falls inside one of the slots if any.
    const now = Date.now();
    let nowIdx = -1;
    for (let i = 0; i < n; i++) {
      const start = items[i].tsMs;
      const end   = start + items[i].lenMin * 60_000;
      if (now >= start && now < end) { nowIdx = i; break; }
    }

    // Bars — colour by relative price (cheaper = green, expensive =
    // red, mid = neutral). Using the per-slot deviation from the mean
    // keeps the colour discipline meaningful even on flat-price days.
    const bars = items.map((it, i) => {
      const x = pad.l + i * barW;
      const p = prices[i];
      const y = yToPx(p);
      // Negative-price slots draw downward from zero; the rect's top
      // is the zero line and its height extends to the price's y.
      // Positive slots draw the conventional way (top = price y, down
      // to zero). Either way, height is the absolute distance.
      const top = p < 0 ? zeroY : y;
      const h = Math.max(1, Math.abs(zeroY - y));
      const dev = (p - meanP) / Math.max(1, maxP - minP);
      // Negative slots are flagged yellow regardless of where they
      // land in the lo/hi ranking — "they pay you to take it" reads
      // as a different category, not just "the cheapest hour today".
      const fill = p < 0 ? "var(--yellow)"
                  : i === lo ? "var(--green-e)"
                  : i === hi ? "var(--red-e)"
                  : (p < meanP ? `color-mix(in srgb, var(--green-e) ${Math.round(40 - dev * 40)}%, transparent)`
                              : `color-mix(in srgb, var(--red-e) ${Math.round(40 + dev * 40)}%, transparent)`);
      const stroke = (i === lo || i === hi) ? "currentColor" : "none";
      return `<rect x="${x + 0.5}" y="${top}" width="${Math.max(0.1, barW - 1)}" height="${h}"
                    fill="${fill}" data-idx="${i}"
                    style="${i === lo ? 'color: var(--green-e)' : i === hi ? 'color: var(--red-e)' : ''}"
                    stroke="${stroke}" stroke-width="${stroke === 'none' ? 0 : 1}" />`;
    }).join("");

    // Mean reference line — true dotted (round caps + zero-length
    // dashes spaced by 6 px) so it reads as "average over the known
    // price period" rather than a regular dashed grid line. Sits
    // above the bars but below the markers and tooltip.
    const meanLine = `<line x1="${pad.l}" x2="${pad.l + plotW}" y1="${meanY}" y2="${meanY}"
                          stroke="var(--fg-muted)" stroke-width="1.5"
                          stroke-linecap="round" stroke-dasharray="0.01 6" />`;

    // X-axis time ticks — every 3 hours so 48 h reads as ~16 evenly
    // spaced labels, not the 8-label sparse grid we had before.
    // Operators kept asking "what hour is this?" mid-chart.
    const xTicks = [];
    if (n > 0) {
      const startT = items[0].tsMs;
      const endT = items[n - 1].tsMs + items[n - 1].lenMin * 60_000;
      for (let t = ceilTo(startT, tickStepMs); t < endT; t += tickStepMs) {
        const frac = (t - startT) / (endT - startT);
        const x = pad.l + frac * plotW;
        xTicks.push(`<line x1="${x}" x2="${x}" y1="${pad.t + plotH}" y2="${pad.t + plotH + 4}"
                          stroke="var(--line)" />
                     <text x="${x}" y="${pad.t + plotH + tickLabelDy}" text-anchor="middle"
                           fill="var(--fg-label)" font-family="var(--mono)" font-size="${fsAxis}">
                       ${fmtClock(t)}
                     </text>`);
      }
    }
    // Y-axis labels — min / mean / max.
    const yLabels = [
      { y: yToPx(yMax), text: roundOre(yMax) + " ö" },
      { y: meanY,       text: roundOre(meanP) + " ö" },
      { y: yToPx(yMin), text: roundOre(yMin) + " ö" },
    ].map((l) => `<text x="${pad.l - 4}" y="${l.y + 3}" text-anchor="end"
                       fill="var(--fg-label)" font-family="var(--mono)" font-size="${fsAxis}">${l.text}</text>`).join("");

    // "Now" marker — vertical line plus a "now" pill.
    let nowMarker = "";
    if (nowIdx >= 0) {
      const x = pad.l + (nowIdx + 0.5) * barW;
      nowMarker = `
        <line x1="${x}" x2="${x}" y1="${pad.t}" y2="${pad.t + plotH}"
              stroke="var(--accent-e)" stroke-width="${nowStrokeW}" stroke-dasharray="2 3"
              opacity="0.7" />
        <text x="${x}" y="${pad.t - 6}" text-anchor="middle"
              fill="var(--accent-e)" font-family="var(--mono)" font-size="${fsNow}"
              font-weight="700"
              stroke="var(--accent-e)" stroke-width="${small ? 0.6 : 0}"
              paint-order="stroke fill">NOW</text>
      `;
    }

    // Tomorrow boundary — yellow vertical line at midnight when the
    // chart spans across the day boundary, with a rotated "TOMORROW"
    // label hugging it. Only renders when the data actually crosses
    // 00:00 (so single-day "Today only" views don't get a stray line
    // at the right edge).
    let dayBoundary = "";
    if (n > 0) {
      const tomorrow = new Date();
      tomorrow.setHours(0, 0, 0, 0);
      tomorrow.setDate(tomorrow.getDate() + 1);
      const midnightMs = tomorrow.getTime();
      const startT = items[0].tsMs;
      const endT = items[n - 1].tsMs + items[n - 1].lenMin * 60_000;
      if (midnightMs > startT && midnightMs < endT) {
        const frac = (midnightMs - startT) / (endT - startT);
        const x = pad.l + frac * plotW;
        const tx = x + 4;
        const ty = pad.t + 6;
        dayBoundary = `
          <line x1="${x}" x2="${x}" y1="${pad.t}" y2="${pad.t + plotH}"
                stroke="var(--yellow)" stroke-width="1.5" opacity="0.8" />
          <text x="${tx}" y="${ty}" text-anchor="start"
                fill="var(--yellow)" font-family="var(--mono)" font-size="10"
                font-weight="600" letter-spacing="0.18em"
                transform="rotate(90 ${tx} ${ty})">TOMORROW</text>
        `;
      }
    }

    // Peak / low markers — small triangles above their bars.
    const markBar = (idx, color, glyph) => {
      const x = pad.l + (idx + 0.5) * barW;
      const p = prices[idx];
      // For negative-priced slots the bar extends downward from zero,
      // so anchoring the marker at the price's y would bury it inside
      // the bar. Pin to just above the zero line instead so the
      // glyph still reads as a pointer at the column.
      const baseY = p < 0 ? zeroY : yToPx(p);
      const y = baseY - 6;
      return `<text x="${x}" y="${y}" text-anchor="middle"
                    fill="${color}" font-family="var(--mono)" font-size="${fsMark}"
                    font-weight="700">${glyph}</text>`;
    };

    // Hit-target overlay — invisible rects sized to bar width that
    // cover the FULL plot height so hover is forgiving even when a
    // bar is short (cheap slots).
    const hits = items.map((_, i) => {
      const x = pad.l + i * barW;
      return `<rect x="${x}" y="${pad.t}" width="${barW}" height="${plotH}"
                    fill="transparent" data-idx="${i}" class="hit" />`;
    }).join("");

    return `
      <div class="chart-wrap">
        <svg class="chart" viewBox="0 0 ${W} ${H}" preserveAspectRatio="none"
             role="img" aria-label="Electricity price chart">
          ${meanLine}
          ${bars}
          ${dayBoundary}
          ${nowMarker}
          ${markBar(lo, "var(--green-e)", "▼")}
          ${markBar(hi, "var(--red-e)",   "▲")}
          ${xTicks.join("")}
          ${yLabels}
          <line class="scrub-cursor" x1="0" x2="0" y1="${pad.t}" y2="${pad.t + plotH}"
                stroke="var(--fg)" stroke-width="1" opacity="0" />
          ${hits}
        </svg>
        <div class="tip" data-tip>
          <div class="tip-time" data-tip-time>—</div>
          <div class="tip-price" data-tip-price>—</div>
        </div>
      </div>
    `;
  }

  afterRender() {
    const root = this.shadowRoot;
    const vatToggle = root.querySelector(".toggle[data-vat]");
    if (vatToggle) {
      vatToggle.querySelectorAll("button[data-vat]").forEach((b) => {
        b.addEventListener("click", () => {
          const next = b.dataset.vat === "on";
          if (next === this._vatOn) return;
          this._vatOn = next;
          writeVatPref(next);
          window.dispatchEvent(new CustomEvent("ftw-price-vat-change", {
            detail: { vatOn: next },
          }));
          this.update();
        });
      });
    }
    const horizonToggle = root.querySelector(".toggle[data-horizon]");
    if (horizonToggle) {
      horizonToggle.querySelectorAll("button[data-horizon]").forEach((b) => {
        b.addEventListener("click", () => {
          const next = b.dataset.horizon;
          if (next === this._horizon) return;
          this._horizon = next;
          writeHorizonPref(next);
          this.update();
        });
      });
    }
    // Tooltip wiring — listen on the SVG and route by data-idx.
    const svg = root.querySelector("svg.chart");
    const tip = root.querySelector("[data-tip]");
    if (!svg || !tip || !this._data) return;
    const onMouseMove = (e) => {
      if (this._isTouching) return; // touch path owns the tooltip
      const target = e.target.closest("[data-idx]");
      if (!target) { this._hideTip(); return; }
      const i = Number(target.dataset.idx);
      if (!Number.isFinite(i)) { this._hideTip(); return; }
      const rect = svg.getBoundingClientRect();
      this._showTipAt(i, e.clientX - rect.left, e.clientY - rect.top);
    };
    svg.addEventListener("mousemove", onMouseMove);
    svg.addEventListener("mouseleave", () => { if (!this._isTouching) this._hideTip(); });

    // Touch — long-press to enter scrub mode, then drag horizontally
    // to walk the tooltip across slots. The 250 ms threshold lets a
    // regular vertical swipe-to-scroll pass through unmolested; if
    // the finger moves >10 px before the timer fires, we cancel
    // (gesture is a scroll, not a press).
    let pressTimer = null;
    let scrubbing = false;
    let startX = 0, startY = 0;
    const SCRUB_DELAY_MS = 250;
    const SCRUB_TOLERANCE_PX = 10;

    const cancelPress = () => {
      if (pressTimer) { clearTimeout(pressTimer); pressTimer = null; }
    };
    const enterScrub = () => {
      pressTimer = null;
      scrubbing = true;
      if (navigator.vibrate) { try { navigator.vibrate(8); } catch (_) {} }
      const idx = this._idxFromClientX(startX);
      if (idx >= 0) {
        const rect = svg.getBoundingClientRect();
        this._showTipAt(idx, startX - rect.left, startY - rect.top);
      }
    };
    const endTouch = () => {
      cancelPress();
      if (scrubbing) {
        scrubbing = false;
        this._hideTip();
      }
      // Defer clearing _isTouching past the synthesized mouse events
      // that fire after touchend on iOS/Android — without this the
      // tooltip flashes back open as the page settles.
      setTimeout(() => { this._isTouching = false; }, 400);
    };

    svg.addEventListener("touchstart", (e) => {
      if (e.touches.length !== 1) { cancelPress(); return; }
      const t = e.touches[0];
      startX = t.clientX;
      startY = t.clientY;
      this._isTouching = true;
      cancelPress();
      pressTimer = setTimeout(enterScrub, SCRUB_DELAY_MS);
    }, { passive: true });

    svg.addEventListener("touchmove", (e) => {
      if (e.touches.length !== 1) return;
      const t = e.touches[0];
      if (!scrubbing) {
        if (Math.hypot(t.clientX - startX, t.clientY - startY) > SCRUB_TOLERANCE_PX) {
          cancelPress();
        }
        return;
      }
      // In scrub mode — block page scroll and walk the tooltip.
      e.preventDefault();
      const idx = this._idxFromClientX(t.clientX);
      if (idx < 0) return;
      const rect = svg.getBoundingClientRect();
      this._showTipAt(idx, t.clientX - rect.left, t.clientY - rect.top);
    }, { passive: false });

    svg.addEventListener("touchend", endTouch);
    svg.addEventListener("touchcancel", endTouch);
  }

  _idxFromClientX(clientX) {
    const svg = this.shadowRoot.querySelector("svg.chart");
    if (!svg || !this._geom) return -1;
    const rect = svg.getBoundingClientRect();
    if (rect.width === 0) return -1;
    const vbX = ((clientX - rect.left) / rect.width) * this._geom.W;
    const barW = this._geom.plotW / this._geom.n;
    const i = Math.floor((vbX - this._geom.padL) / barW);
    if (i < 0 || i >= this._geom.n) return -1;
    return i;
  }

  _showTipAt(idx, localX, localY) {
    const tip = this.shadowRoot.querySelector("[data-tip]");
    const item = this._data && this._data.items[idx];
    if (!tip || !item) return;
    const price = this._priceFor(item);
    const tEnd = item.tsMs + item.lenMin * 60_000;
    tip.querySelector("[data-tip-time]").textContent =
      `${fmtClock(item.tsMs)}–${fmtClock(tEnd)}`;
    const priceEl = tip.querySelector("[data-tip-price]");
    priceEl.textContent = `${roundOre(price)} öre`;
    // Annotate peak/low per the same indices used in render.
    const items = this._data.items;
    const prices = items.map((it) => this._priceFor(it));
    let lo = 0, hi = 0;
    for (let i = 1; i < items.length; i++) {
      if (prices[i] < prices[lo]) lo = i;
      if (prices[i] > prices[hi]) hi = i;
    }
    priceEl.classList.toggle("peak", idx === hi);
    priceEl.classList.toggle("low",  idx === lo);
    // On small screens the tooltip is pinned above the bars and tracks
    // slot centre, not finger Y — keeps the readout clear of the data
    // it's reading. Desktop keeps the cursor-follow behaviour.
    const smallScreen = typeof window !== "undefined" &&
      window.matchMedia && window.matchMedia("(max-width: 600px)").matches;
    if (smallScreen && this._geom) {
      const svg = this.shadowRoot.querySelector("svg.chart");
      const svgRect = svg ? svg.getBoundingClientRect() : null;
      const slotVbX = this._geom.padL + (idx + 0.5) * (this._geom.plotW / this._geom.n);
      const slotPxX = svgRect && svgRect.width
        ? (slotVbX / this._geom.W) * svgRect.width
        : localX;
      // Clamp so the tooltip never runs off the chart's left/right edge.
      const halfW = ((tip.getBoundingClientRect().width) || 120) / 2;
      const wrapW = svgRect ? svgRect.width : 1000;
      const clampedX = Math.max(halfW + 4, Math.min(wrapW - halfW - 4, slotPxX));
      tip.style.left = clampedX + "px";
      tip.style.top  = "0px";
    } else {
      tip.style.left = localX + "px";
      tip.style.top  = localY + "px";
    }
    tip.classList.add("visible");
    // Vertical scrub cursor — pin it to the slot centre so the eye
    // can confirm which column the tooltip is reading from.
    const cursor = this.shadowRoot.querySelector("svg .scrub-cursor");
    if (cursor && this._geom) {
      const slotX = this._geom.padL + (idx + 0.5) * (this._geom.plotW / this._geom.n);
      cursor.setAttribute("x1", slotX);
      cursor.setAttribute("x2", slotX);
      cursor.setAttribute("opacity", "0.5");
    }
  }

  _hideTip() {
    const tip = this.shadowRoot.querySelector("[data-tip]");
    if (tip) tip.classList.remove("visible");
    const cursor = this.shadowRoot.querySelector("svg .scrub-cursor");
    if (cursor) cursor.setAttribute("opacity", "0");
  }
}

const VAT_PREF_KEY = "ftw.priceChart.vatOn";
function readVatPref() {
  try {
    const v = localStorage.getItem(VAT_PREF_KEY);
    if (v === "0" || v === "false") return false;
    if (v === "1" || v === "true")  return true;
  } catch (_) { /* private mode / disabled storage — fall through */ }
  return true;
}
function writeVatPref(on) {
  try { localStorage.setItem(VAT_PREF_KEY, on ? "1" : "0"); } catch (_) {}
}

const HORIZON_PREF_KEY = "ftw.priceChart.horizon";
function readHorizonPref() {
  try {
    const v = localStorage.getItem(HORIZON_PREF_KEY);
    if (v === "today" || v === "all" || v === "tomorrow") return v;
  } catch (_) {}
  return "all";
}
function writeHorizonPref(h) {
  try { localStorage.setItem(HORIZON_PREF_KEY, h); } catch (_) {}
}

// Filter to slots whose start time falls inside today's calendar day
// (local timezone). Used for the "Today only" toggle position.
function filterToday(items) {
  const start = new Date();
  start.setHours(0, 0, 0, 0);
  const end = new Date(start);
  end.setDate(end.getDate() + 1);
  const t0 = start.getTime(), t1 = end.getTime();
  return items.filter((it) => it.tsMs >= t0 && it.tsMs < t1);
}

// Filter to slots whose start time falls inside tomorrow's calendar
// day (local timezone). Used for the "Tomorrow only" toggle position.
function filterTomorrow(items) {
  const start = new Date();
  start.setHours(0, 0, 0, 0);
  start.setDate(start.getDate() + 1);
  const end = new Date(start);
  end.setDate(end.getDate() + 1);
  const t0 = start.getTime(), t1 = end.getTime();
  return items.filter((it) => it.tsMs >= t0 && it.tsMs < t1);
}

// True if any slot starts on tomorrow's calendar day (local timezone) —
// drives whether the today/tomorrow toggle is meaningful at all.
function itemsIncludeTomorrow(items) {
  if (!items || !items.length) return false;
  const start = new Date();
  start.setHours(0, 0, 0, 0);
  start.setDate(start.getDate() + 1);
  const t0 = start.getTime();
  return items.some((it) => it.tsMs >= t0);
}

function fmtClock(tsMs) {
  const d = new Date(tsMs);
  return d.getHours().toString().padStart(2, "0") + ":" +
         d.getMinutes().toString().padStart(2, "0");
}

function roundOre(v) {
  if (Math.abs(v) >= 100) return v.toFixed(0);
  if (Math.abs(v) >= 10)  return v.toFixed(1);
  return v.toFixed(2);
}

function ceilTo(t, step) {
  return Math.ceil(t / step) * step;
}

function escapeXml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", "\"": "&quot;", "'": "&#39;"
  }[c]));
}

customElements.define("ftw-price-chart", FtwPriceChart);
