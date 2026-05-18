// <ftw-savings-card> — historical vs flat-rate savings card.
//
// Fetches /api/savings/daily and renders:
//   - a headline "+247 SEK (+23%)" sized for scanning, color by sign
//   - a diverging per-day sparkline (positive days green, negative red)
//     anchored on a zero baseline — same +/- shape the plan-view's
//     "vs flat avg price" badge uses, extended along the time axis.
//   - a Week/Month toggle matching ftw-history-card's pill style
//
// Attributes:
//   default-range — "week" (default, 7 days) | "month" (so far)
//   range         — when set externally, drives the toggle state
//   poll-ms       — refresh interval (default 300000 = 5 min; 0 disables)
//
// Empty-state handling: when /api/savings/daily returns no days (most
// commonly because cfg.Price.Zone is unset), the card collapses to a
// muted line and hides the sparkline. The card still mounts so the
// presence in the DOM is intentional — operators see it and know the
// feature exists, they just need to configure prices.

import { FtwElement, ftwDebugDelay } from "./ftw-element.js";

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
    .toggle button.active { color: #0a0a0a; }
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
      grid-template-rows: auto auto;
      gap: 6px;
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

    @media (max-width: 900px) {
      .card-inner { padding: var(--card-pad-tight, 12px 14px); }
      .total { font-size: 1.2rem; }
    }
  `;

  static get observedAttributes() {
    return ["default-range", "range", "poll-ms"];
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
    return `
      <div class="card-inner">
        <div class="head">
          <div class="label">Saved vs flat-rate</div>
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
          <svg class="spark" data-role="spark" viewBox="0 0 100 72" preserveAspectRatio="none"></svg>
          <div class="day-labels" data-role="labels"></div>
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
    this._paint();
  }

  _refresh() {
    const days = this._daysFor(this._range);
    if (this._abort) this._abort.abort();
    this._abort = new AbortController();
    const seq = ++this._reqSeq;
    const signal = this._abort.signal;

    fetch("/api/savings/daily?days=" + days, { signal })
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
  _paint() {
    const root = this.shadowRoot;
    if (!root) return;
    const totalEl  = root.querySelector('[data-role="total"]');
    const pctEl    = root.querySelector('[data-role="pct"]');
    const subEl    = root.querySelector('[data-role="sub"]');
    const sparkWrap = root.querySelector('[data-role="spark-wrap"]');
    const sparkEl  = root.querySelector('[data-role="spark"]');
    const labelsEl = root.querySelector('[data-role="labels"]');
    if (!totalEl || !pctEl || !subEl || !sparkEl || !labelsEl || !sparkWrap) return;

    if (this._state === "loading") {
      totalEl.textContent = "—";
      pctEl.textContent = "";
      subEl.textContent = "";
      sparkWrap.style.display = "none";
      return;
    }
    if (this._state === "error") {
      totalEl.textContent = "failed to load";
      pctEl.textContent = "";
      subEl.textContent = "";
      sparkWrap.style.display = "none";
      this.style.setProperty('--ftw-savings-color', 'var(--fg-muted)');
      return;
    }
    if (this._state === "empty" || !this._payload) {
      totalEl.textContent = "—";
      pctEl.textContent = "";
      subEl.innerHTML = 'No price provider configured — set <b>price.zone</b> in config to enable historical savings.';
      sparkWrap.style.display = "none";
      this.style.setProperty('--ftw-savings-color', 'var(--fg-muted)');
      return;
    }
    if (this._state === "awaiting_prices") {
      totalEl.textContent = "—";
      pctEl.textContent = "";
      subEl.innerHTML = 'Awaiting price data for the selected range — savings will appear once the price provider catches up.';
      sparkWrap.style.display = "none";
      this.style.setProperty('--ftw-savings-color', 'var(--fg-muted)');
      return;
    }

    sparkWrap.style.display = "";
    const { days, totals } = this._payload;
    const savedOre = Number(totals && totals.saved_ore) || 0;
    const flatOre  = Number(totals && totals.flat_cost_ore) || 0;
    const actualOre = Number(totals && totals.actual_cost_ore) || 0;
    const savedSek = savedOre / 100;
    // Percentage anchors on absolute flat cost so a $0 flat baseline
    // doesn't divide-by-zero. When flat is tiny, we suppress the pct
    // entirely — the absolute SEK number tells the story alone.
    const denomOre = Math.abs(flatOre);
    const pct = denomOre > 1 ? (savedOre / denomOre) * 100 : 0;

    const sign = (v) => (v >= 0 ? "+" : "−");
    const fmtSek = (sek) => {
      const v = Math.abs(sek);
      if (v >= 100) return v.toFixed(0);
      if (v >= 10)  return v.toFixed(1);
      return v.toFixed(2);
    };

    totalEl.textContent = `${sign(savedSek)}${fmtSek(savedSek)} SEK`;
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

    // Sub-line: actual + flat cost in SEK, dim. Gives the operator the
    // two numbers the saving is derived from.
    const actualSek = actualOre / 100;
    const flatSek = flatOre / 100;
    subEl.innerHTML =
      `Actual <b>${fmtSek(actualSek)} SEK</b>, flat-rate <b>${fmtSek(flatSek)} SEK</b>`;

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
    for (const d of days) {
      const v = Math.abs(Number(d.saved_ore) || 0);
      if (v > maxAbs) maxAbs = v;
    }
    if (maxAbs === 0) maxAbs = 1;
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
function escapeHtml(s) {
  return String(s).replace(/[<>&"']/g, (c) =>
    ({ "<": "&lt;", ">": "&gt;", "&": "&amp;", '"': "&quot;", "'": "&#39;" }[c]));
}

customElements.define("ftw-savings-card", FtwSavingsCard);
