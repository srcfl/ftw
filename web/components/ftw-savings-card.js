// <ftw-savings-card> — historical savings vs no-PV/no-battery baseline.
//
// Fetches /api/savings/daily and renders:
//   - a headline "+247 SEK saved (+23%)" sized for scanning, color by sign
//   - a diverging per-day sparkline (positive days green, negative red)
//     anchored on a zero baseline.
//   - a Week/Month toggle matching ftw-history-card's pill style
//
// Attributes:
//   default-range — "week" (default, 7 days) | "month" (so far)
//   range         — when set externally, drives the toggle state
//   poll-ms       — refresh interval (default 300000 = 5 min; 0 disables)
//
// Empty-state handling: when /api/savings/daily returns no days, the card
// collapses to a muted line and hides the sparkline.

import { FtwElement, ftwDebugDelay } from "./ftw-element.js";
import { apiFetch } from "./api-fetch.js";

class FtwSavingsCard extends FtwElement {
  static styles = `
    :host { display: block; }
    .card-inner {
      display: flex;
      flex-direction: column;
      gap: 10px;
      background: var(--ink-raised);
      border: 1px solid var(--line);
      border-radius: var(--radius-md, 10px);
      padding: var(--card-pad, 14px 16px);
    }
    .head {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 12px;
    }
    .label {
      font-family: var(--mono);
      font-size: 10px;
      color: var(--fg-label);
      letter-spacing: 0.1em;
      text-transform: uppercase;
    }
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
      top: 2px; bottom: 2px; left: 2px;
      width: calc(50% - 2px);
      background: var(--accent-e);
      border-radius: 999px;
      transform: translateX(0);
      transition: transform 260ms cubic-bezier(0.4, 0, 0.2, 1);
      z-index: 0;
    }
    .toggle[data-active="month"]::before { transform: translateX(100%); }
    .toggle button {
      position: relative;
      z-index: 1;
      background: transparent;
      border: 0;
      color: var(--fg-label);
      font-family: var(--mono);
      font-size: 10px;
      font-weight: 500;
      letter-spacing: 0.18em;
      text-transform: uppercase;
      padding: 4px 12px;
      cursor: pointer;
      transition: color 220ms ease;
    }
    .toggle button.active { color: var(--on-accent, #0a0a0a); }
    .toggle button:not(.active):hover { color: var(--fg); }
    .toggle button:focus-visible {
      outline: 1px solid var(--accent-e);
      outline-offset: 2px;
      border-radius: 999px;
    }

    /* Headline — mono tabular, sized to compete with the kWh totals on
       sibling cards. Color is set per-render via --ftw-savings-color so
       sign flips don't require a re-render of the whole tree. */
    .headline {
      display: flex;
      align-items: baseline;
      gap: 10px;
      flex-wrap: wrap;
    }
    .total {
      font-family: var(--mono);
      font-size: 1.4rem;
      font-weight: 700;
      font-variant-numeric: tabular-nums;
      letter-spacing: -0.01em;
      color: var(--ftw-savings-color, var(--fg));
    }
    .pct {
      font-family: var(--mono);
      font-size: 0.9rem;
      font-weight: 500;
      font-variant-numeric: tabular-nums;
      color: var(--ftw-savings-color, var(--fg-dim));
      opacity: 0.85;
    }
    .sub {
      font-family: var(--sans);
      font-size: 0.78rem;
      color: var(--fg-muted);
    }
    .sub b {
      font-family: var(--mono);
      font-weight: 500;
      color: var(--fg-dim);
    }

    /* Sparkline — pure SVG. Bars above the zero line are green, below
       are red, both pulled from theme tokens so light-mode flips
       cleanly. The baseline is a 1 px var(--line) hairline. */
    .spark-wrap {
      display: grid;
      gap: 6px;
      position: relative;
    }
    .spark-grid,
    .labels-grid {
      display: grid;
      grid-template-columns: 44px 1fr;
      gap: 8px;
      align-items: stretch;
    }
    .spark-stage {
      position: relative;
      min-width: 0;
    }
    .spark-scale {
      height: 72px;
      display: grid;
      grid-template-rows: 1fr auto 1fr;
      align-items: center;
      justify-items: end;
      font-family: var(--mono);
      font-size: 9px;
      color: var(--fg-muted);
      font-variant-numeric: tabular-nums;
    }
    svg.spark {
      width: 100%;
      height: 72px;
      display: block;
      overflow: visible;
    }
    svg.spark .bar-pos { fill: var(--green-e); }
    svg.spark .bar-neg { fill: var(--red-e); }
    svg.spark .baseline {
      stroke: var(--line);
      stroke-width: 1;
    }
    .day-labels {
      display: grid;
      grid-auto-flow: column;
      grid-auto-columns: 1fr;
      font-family: var(--mono);
      font-size: 9px;
      letter-spacing: 0.08em;
      color: var(--fg-muted);
      text-transform: uppercase;
      text-align: center;
    }
    .spark-tip {
      position: absolute;
      display: none;
      min-width: 170px;
      max-width: 220px;
      z-index: 5;
      pointer-events: none;
      background: var(--ink-elevated);
      border: 1px solid var(--line);
      border-radius: 6px;
      padding: 7px 9px;
      color: var(--fg);
      font-family: var(--sans);
      font-size: 11px;
      line-height: 1.35;
      box-shadow: 0 8px 24px rgba(0,0,0,0.35);
    }
    .spark-tip b {
      color: var(--fg);
      font-family: var(--mono);
      font-weight: 600;
    }
    .spark-tip .muted {
      color: var(--fg-muted, #858585);
      margin-top: 2px;
    }
    .day-labels span {
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    /* Empty / loading / error states — collapse the chart area entirely
       so the card doesn't reserve dead pixels when there's nothing to
       show. */
    .empty {
      font-family: var(--sans);
      font-size: 0.85rem;
      color: var(--fg-muted);
      padding: 8px 0;
    }

    :host([compact]) .card-inner {
      height: 100%;
      box-sizing: border-box;
      justify-content: center;
      gap: 5px;
      background: var(--ink-sunken);
      border-radius: var(--radius-md, 10px);
      padding: 12px 14px;
    }
    :host([compact]) .toggle,
    :host([compact]) .pct,
    :host([compact]) .sub,
    :host([compact]) .spark-wrap {
      display: none !important;
    }
    :host([compact]) .total {
      font-size: 1.05rem;
    }

    @media (max-width: 900px) {
      .card-inner { padding: var(--card-pad-tight, 12px 14px); }
      .total { font-size: 1.2rem; }
    }
  `;

  static get observedAttributes() {
    return ["default-range", "range", "poll-ms", "compact"];
  }

  constructor() {
    super();
    this._range = null;
    this._timer = null;
    this._reqSeq = 0;
    this._abort = null;
    this._state = "loading"; // "loading" | "ready" | "empty" | "error"
    this._payload = null;
  }

  connectedCallback() {
    super.connectedCallback();
    this._refresh();
    this._restartPolling();
  }
  disconnectedCallback() {
    if (this._timer) { clearInterval(this._timer); this._timer = null; }
    if (this._abort) { this._abort.abort(); this._abort = null; }
  }

  attributeChangedCallback(name) {
    let rangeChanged = false;
    if (name === "range") {
      const next = this.getAttribute("range");
      if (next && next !== this._range) {
        this._range = next;
        rangeChanged = true;
      }
    }
    this.update();
    if (name === "poll-ms") {
      this._refresh();
      this._restartPolling();
    }
    if (rangeChanged) this._refresh();
  }

  _restartPolling() {
    if (this._timer) { clearInterval(this._timer); this._timer = null; }
    const raw = this.getAttribute("poll-ms");
    const ms = Number(raw ?? 300000);
    if (ms > 0 && this.isConnected) {
      this._timer = setInterval(() => this._refresh(), ms);
    }
  }

  _daysFor(range) {
    if (this.hasAttribute("compact")) return 1;
    if (range === "month") {
      const now = new Date();
      return now.getDate();
    }
    return 7;
  }

  render() {
    const rangeAttr = this.getAttribute("range");
    if (rangeAttr && rangeAttr !== this._range) this._range = rangeAttr;
    if (this._range == null) {
      this._range = this.getAttribute("default-range") || "week";
    }
    const wk = this._range === "week";
    const compact = this.hasAttribute("compact");
    return `
      <div class="card-inner">
        <div class="head">
          <div class="label" title="Actual historical net grid cost compared with buying the recorded house load from the grid with no PV and no battery.">${compact ? "Saved today" : "Saved vs no PV/battery"}</div>
          <div class="toggle" role="tablist" data-active="${wk ? "week" : "month"}">
            <button type="button" role="tab" data-range="week"  aria-selected="${wk ? "true" : "false"}"${wk ? ' class="active"' : ""}>Week</button>
            <button type="button" role="tab" data-range="month" aria-selected="${!wk ? "true" : "false"}"${!wk ? ' class="active"' : ""}>Month</button>
          </div>
        </div>
        <div class="headline">
          <span class="total" data-role="total">—</span>
          <span class="pct"   data-role="pct"></span>
        </div>
        <div class="sub" data-role="sub"></div>
        <div class="spark-wrap" data-role="spark-wrap">
          <div class="spark-grid">
            <div class="spark-scale" aria-hidden="true">
              <span data-role="axis-max"></span>
              <span>0</span>
              <span data-role="axis-min"></span>
            </div>
            <div class="spark-stage">
              <svg class="spark" data-role="spark" viewBox="0 0 100 72" preserveAspectRatio="none"></svg>
              <div class="spark-tip" data-role="spark-tip"></div>
            </div>
          </div>
          <div class="labels-grid">
            <span aria-hidden="true"></span>
            <div class="day-labels" data-role="labels"></div>
          </div>
        </div>
      </div>
    `;
  }

  afterRender() {
    const toggle = this.shadowRoot.querySelector('.toggle');
    if (toggle) {
      toggle.addEventListener('click', (e) => {
        const btn = e.target.closest('button[data-range]');
        if (!btn) return;
        const next = btn.getAttribute('data-range');
        if (!next || next === this._range) return;
        this._range = next;
        this.update();
        this._refresh();
        this.dispatchEvent(new CustomEvent('ftw-savings-range', {
          detail: { range: next },
          bubbles: true, composed: true,
        }));
      });
    }
    const spark = this.shadowRoot.querySelector('[data-role="spark"]');
    if (spark) {
      spark.addEventListener('mousemove', (e) => this._showSparkTip(e));
      spark.addEventListener('mouseleave', () => this._hideSparkTip());
    }
    this._paint();
  }

  _refresh() {
    const days = this._daysFor(this._range);
    if (this._abort) this._abort.abort();
    this._abort = new AbortController();
    const seq = ++this._reqSeq;
    const signal = this._abort.signal;

    apiFetch("/api/savings/daily?days=" + days, { signal })
      .then((r) => r.json())
      .then((resp) => {
        if (seq !== this._reqSeq) return;
        const buckets = (resp && resp.days) || [];
        const totals = (resp && resp.totals) || null;
        // Three terminal states from the API:
        //   - days array empty       → no zone configured
        //   - every day no_prices    → zone configured but the prices table
        //                              has no slot covering the window yet
        //                              (fresh boot, awaiting first fetch)
        //   - some day has prices    → render normally
        if (!buckets.length) {
          this._state = "empty";
        } else if (buckets.every((d) => d.resolution === "no_prices")) {
          this._state = "awaiting_prices";
        } else {
          this._state = "ready";
          this._payload = { days: buckets, totals };
        }
        const apply = () => { if (seq === this._reqSeq) this._paint(); };
        const delay = ftwDebugDelay();
        if (delay > 0) setTimeout(apply, delay);
        else apply();
      })
      .catch((err) => {
        if (err && err.name === "AbortError") return;
        if (seq !== this._reqSeq) return;
        this._state = "error";
        this._paint();
      });
  }

  // _paint redraws the dynamic parts of the card from this._payload
  // without re-rendering the whole shadow DOM. Cheap on every fetch tick
  // — no toggle blip, no event listener re-bind.
  _showSparkTip(e) {
    if (this._state !== "ready" || !this._payload || !Array.isArray(this._payload.days)) return;
    const days = this._payload.days;
    if (!days.length) return;
    const root = this.shadowRoot;
    const tip = root.querySelector('[data-role="spark-tip"]');
    const stage = root.querySelector('.spark-stage');
    if (!tip || !stage) return;

    const rect = e.currentTarget.getBoundingClientRect();
    if (rect.width <= 0) return;
    const idx = Math.max(0, Math.min(days.length - 1,
      Math.floor(((e.clientX - rect.left) / rect.width) * days.length)));
    const d = days[idx];
    const savedOre = Number(d.saved_ore) || 0;
    const actualOre = Number(d.actual_cost_ore) || 0;
    const baselineOre = Number(d.baseline_cost_ore ?? d.flat_cost_ore) || 0;
    tip.innerHTML =
      `<div><b>${escapeHtml(fmtDayShort(d.day))}</b> ${escapeHtml(fmtSavedSekOre(savedOre))}</div>` +
      `<div class="muted">Actual ${escapeHtml(fmtSekOre(actualOre))} · baseline ${escapeHtml(fmtSekOre(baselineOre))}</div>`;
    tip.style.display = 'block';

    const stageRect = stage.getBoundingClientRect();
    const tipW = tip.offsetWidth || 190;
    const tipH = tip.offsetHeight || 44;
    const maxLeft = Math.max(4, stageRect.width - tipW - 4);
    const maxTop = Math.max(4, stageRect.height - tipH - 4);
    const left = Math.min(maxLeft, Math.max(4, e.clientX - stageRect.left + 10));
    const top = Math.min(maxTop, Math.max(4, e.clientY - stageRect.top + 10));
    tip.style.left = left + 'px';
    tip.style.top = top + 'px';
  }

  _hideSparkTip() {
    const tip = this.shadowRoot && this.shadowRoot.querySelector('[data-role="spark-tip"]');
    if (tip) tip.style.display = 'none';
  }

  _paint() {
    const root = this.shadowRoot;
    if (!root) return;
    const totalEl  = root.querySelector('[data-role="total"]');
    const pctEl    = root.querySelector('[data-role="pct"]');
    const subEl    = root.querySelector('[data-role="sub"]');
    const sparkWrap = root.querySelector('[data-role="spark-wrap"]');
    const sparkEl  = root.querySelector('[data-role="spark"]');
    const labelsEl = root.querySelector('[data-role="labels"]');
    const axisMaxEl = root.querySelector('[data-role="axis-max"]');
    const axisMinEl = root.querySelector('[data-role="axis-min"]');
    if (!totalEl || !pctEl || !subEl || !sparkEl || !labelsEl || !sparkWrap) return;

    if (this._state === "loading") {
      totalEl.textContent = "—";
      pctEl.textContent = "";
      subEl.textContent = "";
      sparkWrap.style.display = "none";
      if (axisMaxEl) axisMaxEl.textContent = "";
      if (axisMinEl) axisMinEl.textContent = "";
      return;
    }
    if (this._state === "error") {
      totalEl.textContent = "failed to load";
      pctEl.textContent = "";
      subEl.textContent = "";
      sparkWrap.style.display = "none";
      if (axisMaxEl) axisMaxEl.textContent = "";
      if (axisMinEl) axisMinEl.textContent = "";
      this.style.setProperty('--ftw-savings-color', 'var(--fg-muted)');
      return;
    }
    if (this._state === "empty" || !this._payload) {
      totalEl.textContent = "—";
      pctEl.textContent = "";
      subEl.innerHTML = 'No price provider configured — set <b>price.zone</b> to calculate historical savings.';
      sparkWrap.style.display = "none";
      if (axisMaxEl) axisMaxEl.textContent = "";
      if (axisMinEl) axisMinEl.textContent = "";
      this.style.setProperty('--ftw-savings-color', 'var(--fg-muted)');
      return;
    }
    if (this._state === "awaiting_prices") {
      totalEl.textContent = "—";
      pctEl.textContent = "";
      subEl.innerHTML = 'Awaiting price data for the selected range.';
      sparkWrap.style.display = "none";
      if (axisMaxEl) axisMaxEl.textContent = "";
      if (axisMinEl) axisMinEl.textContent = "";
      this.style.setProperty('--ftw-savings-color', 'var(--fg-muted)');
      return;
    }

    sparkWrap.style.display = "";
    const { days, totals } = this._payload;
    const savedOre = Number(totals && totals.saved_ore) || 0;
    const baselineOre  = Number(totals && (totals.baseline_cost_ore ?? totals.flat_cost_ore)) || 0;
    const actualOre = Number(totals && totals.actual_cost_ore) || 0;
    const savedSek = savedOre / 100;
    // Percentage anchors on absolute baseline cost so a zero baseline
    // doesn't divide-by-zero. When the baseline is tiny, we suppress the pct
    // entirely — the absolute SEK number tells the story alone.
    const denomOre = Math.abs(baselineOre);
    const pct = denomOre > 1 ? (savedOre / denomOre) * 100 : 0;

    const sign = (v) => (v >= 0 ? "+" : "−");
    const fmtSek = (sek) => {
      const v = Math.abs(sek);
      if (v >= 100) return v.toFixed(0);
      if (v >= 10)  return v.toFixed(1);
      return v.toFixed(2);
    };

    totalEl.textContent = `${sign(savedSek)}${fmtSek(savedSek)} SEK ${savedSek >= 0 ? "saved" : "lost"}`;
    if (Math.abs(pct) >= 0.5) {
      pctEl.textContent = `(${sign(pct)}${Math.abs(pct).toFixed(0)}%)`;
    } else {
      pctEl.textContent = "";
    }

    // Color: green when saved>0, red when saved<0, neutral within tiny
    // dead zone (avoids flicker when the running total bobs across 0).
    const deadZoneSek = 0.5;
    const color = Math.abs(savedSek) < deadZoneSek
      ? "var(--fg-dim)"
      : (savedSek > 0 ? "var(--green-e)" : "var(--red-e)");
    this.style.setProperty('--ftw-savings-color', color);

    // Sub-line: actual + no-PV/no-battery baseline in SEK. Gives the
    // operator the two numbers the delta is derived from.
    const actualSek = actualOre / 100;
    const baselineSek = baselineOre / 100;
    subEl.innerHTML =
      `Actual <b>${fmtSek(actualSek)} SEK</b>, no PV/battery <b>${fmtSek(baselineSek)} SEK</b>`;

    // ---- Sparkline -----------------------------------------------------
    // Bars on a zero baseline, full height split 50/50 above/below.
    // Width is filled (viewBox 0..100); each bar is allocated 100/N
    // worth, with a small gutter. Heights are normalized to the
    // largest absolute saved_ore in the window so even a near-zero
    // total still produces a readable shape.
    const n = days.length;
    if (!n) { sparkEl.innerHTML = ""; labelsEl.innerHTML = ""; return; }

    const viewW = 100;
    const viewH = 72;
    const baselineY = viewH / 2;
    const slot = viewW / n;
    const gutter = Math.min(2.5, slot * 0.18);
    const barW = Math.max(1, slot - gutter);

    let maxAbs = 0;
    let hasNegative = false;
    for (const d of days) {
      const v = Math.abs(Number(d.saved_ore) || 0);
      if (v > maxAbs) maxAbs = v;
      if ((Number(d.saved_ore) || 0) < 0) hasNegative = true;
    }
    const axisAbs = maxAbs;
    if (maxAbs === 0) maxAbs = 1;
    if (axisMaxEl) axisMaxEl.textContent = axisAbs > 0 ? '+' + fmtSekOre(axisAbs) : '0 SEK';
    if (axisMinEl) axisMinEl.textContent = hasNegative ? '−' + fmtSekOre(axisAbs) : '';
    const maxBarH = baselineY - 4; // 4 px headroom

    const parts = [];
    parts.push(`<line class="baseline" x1="0" x2="${viewW}" y1="${baselineY}" y2="${baselineY}"/>`);
    days.forEach((d, i) => {
      const v = Number(d.saved_ore) || 0;
      const h = (Math.abs(v) / maxAbs) * maxBarH;
      const x = i * slot + gutter / 2;
      let y, hh, cls;
      if (v >= 0) {
        y = baselineY - h;
        hh = h;
        cls = "bar-pos";
      } else {
        y = baselineY;
        hh = h;
        cls = "bar-neg";
      }
      const title = `${d.day}: ${sign(v / 100)}${fmtSek(Math.abs(v) / 100)} SEK`;
      parts.push(
        `<rect class="${cls}" x="${x.toFixed(2)}" y="${y.toFixed(2)}" ` +
        `width="${barW.toFixed(2)}" height="${Math.max(0.5, hh).toFixed(2)}">` +
        `<title>${escapeHtml(title)}</title></rect>`
      );
    });
    sparkEl.innerHTML = parts.join("");

    // Labels: show day-of-week shorthand under each bar; if the range
    // exceeds 14 days, thin them out to every other one so the row
    // doesn't wrap or collide.
    const step = n > 14 ? 2 : 1;
    const labels = days.map((d, i) =>
      `<span>${(i % step === 0) ? escapeHtml(fmtDayShort(d.day)) : ""}</span>`
    ).join("");
    labelsEl.innerHTML = labels;
  }
}

function fmtDayShort(iso) {
  const parts = String(iso || "").split("-");
  if (parts.length !== 3) return iso || "";
  const d = new Date(+parts[0], +parts[1] - 1, +parts[2]);
  return d.toLocaleDateString(undefined, { weekday: "short", day: "numeric" });
}
function fmtSekOre(ore) {
  const sek = Math.abs(Number(ore) || 0) / 100;
  if (sek >= 100) return sek.toFixed(0) + " SEK";
  if (sek >= 10) return sek.toFixed(1) + " SEK";
  return sek.toFixed(2) + " SEK";
}
function fmtSavedSekOre(ore) {
  const v = Number(ore) || 0;
  return (v >= 0 ? "+" : "−") + fmtSekOre(v) + (v >= 0 ? " saved" : " lost");
}
function escapeHtml(s) {
  return String(s).replace(/[<>&"']/g, (c) =>
    ({ "<": "&lt;", ">": "&gt;", "&": "&amp;", '"': "&quot;", "'": "&#39;" }[c]));
}

customElements.define("ftw-savings-card", FtwSavingsCard);
