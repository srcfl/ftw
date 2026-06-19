// <ftw-history-card> — daily kWh card with Week / Month toggle.
//
// Smart wrapper around <ftw-bar-chart>. Owns the range state, fetches
// /api/energy/daily, picks the right field per metric, and renders the
// card chrome (label, total, toggle).
//
// Attributes:
//   metric   — "import" | "export" | "load" | "pv"
//              | "bat_charged" | "bat_discharged"
//              picks which *_wh field to plot from each day bucket
//   label    — heading text (e.g. "Imported")
//   accent   — bar + total color (default depends on metric; falls back
//              to var(--cyan))
//   default-range — "week" (default, last 7 days) | "month" (so far)
//   range    — when set (e.g. by an outer wrapper card with a shared
//              Week/Month toggle), this attribute is the authoritative
//              source for the range. Changes on it re-fetch. Use this
//              to drive multiple sibling cards from one toggle.
//   hide-toggle — presence hides the per-card Week/Month pill (the
//              caller renders a shared one in the wrapper) and flips
//              the card chrome to the "sub-card" surface so nested
//              tiles read as recessed against the wrapper.
//   poll-ms  — refresh interval in ms (default 300000 = 5 min);
//              0 disables polling
//
// The card auto-fetches on connect and on range change. No external
// JS wiring needed beyond placing the element in HTML.

import { FtwElement, ftwDebugDelay } from "./ftw-element.js";
import { ownerFetch } from "./owner-fetch.js";
import "./ftw-bar-chart.js";

const FIELD_BY_METRIC = {
  import:        "import_wh",
  export:        "export_wh",
  load:          "load_wh",
  pv:            "pv_wh",
  bat_charged:   "bat_charged_wh",
  bat_discharged:"bat_discharged_wh",
};
const DEFAULT_ACCENT = {
  import:        "var(--red-e)",
  export:        "var(--green-e)",
  load:          "var(--fg)",
  pv:            "var(--amber)",
  bat_charged:   "var(--cyan)",
  bat_discharged:"var(--cyan-dim, var(--cyan))",
};

// Several history cards often flip Week/Month together. Share the
// expensive daily-energy request across siblings so Month does not fan
// out into four identical 30-day queries and leave tiles painting at
// different times.
const DAILY_CACHE_TTL_MS = 15000;
const dailyFetchCache = new Map(); // days -> { at, data?, promise? }

function fetchDailyEnergy(days) {
  const now = Date.now();
  const cached = dailyFetchCache.get(days);
  if (cached && cached.data && now - cached.at < DAILY_CACHE_TTL_MS) {
    return Promise.resolve(cached.data);
  }
  if (cached && cached.promise && now - cached.at < DAILY_CACHE_TTL_MS) {
    return cached.promise;
  }

  const promise = ownerFetch("/api/energy/daily?days=" + days)
    .then((r) => {
      if (!r.ok) throw new Error("HTTP " + r.status);
      return r.json();
    })
    .then((resp) => {
      dailyFetchCache.set(days, { at: Date.now(), data: resp });
      return resp;
    })
    .catch((err) => {
      const cur = dailyFetchCache.get(days);
      if (cur && cur.promise === promise) dailyFetchCache.delete(days);
      throw err;
    });
  dailyFetchCache.set(days, { at: now, promise });
  return promise;
}

class FtwHistoryCard extends FtwElement {
  static styles = `
    :host { display: block; }
    /* All card chrome lives on .card-inner, NOT :host. The global
       reset "*, *::before, *::after { padding:0; margin:0 }" from
       style.css beats the shadow :host rule in Chromium for the
       host element — the universal selector reaches in from the
       document tree and wins. A class selector inside the shadow
       DOM has specificity (0,0,1,0), which always beats * (0,0,0,0).
       Use this wrapper pattern for any web component that needs
       visible padding/margin on its outermost surface. */
    .card-inner {
      display: flex;
      flex-direction: column;
      gap: 8px;
      background: var(--ink-raised);
      border: 1px solid var(--line);
      border-radius: var(--radius-md, 10px);
      padding: var(--card-pad, 14px 16px);
    }
    /* Nested mode — when the host has the hide-toggle attribute, the card is sitting
       inside an outer wrapper (see .history-group in next.css) that owns
       the label/toggle chrome. Flip the surface to --ink-sunken so the
       nested tile reads as recessed, matching the fuse-card → phase-box
       pattern. */
    :host([hide-toggle]) .card-inner {
      background: var(--ink-sunken);
    }
    .head {
      display: flex;
      align-items: center;
      justify-content: space-between;
    }
    /* Card heading (Imported / Consumed / Exported). Reads --fg-label
       from theme.css so every label across the dashboard — inside
       shadow DOMs or out — picks up the same brightness from one
       themeable token. The shadow boundary doesn't block custom
       properties, so nothing else is needed to keep it in sync. */
    .label {
      font-family: var(--mono);
      font-size: 10px;
      color: var(--fg-label);
      letter-spacing: 0.1em;
      text-transform: uppercase;
    }
    /* Week / Month toggle — a segmented pill following DESIGN.md:
       eyebrow type (mono 0.18em, UPPERCASE, 500 weight), one accent
       (--accent-e amber — never the legacy --accent purple), pill
       radius 999px, near-black #0a0a0a on-accent text. The active
       selection is a single ::before element that slides between the
       two buttons via transform — the actual buttons carry only text,
       which keeps the flip smooth with no background flash. */
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
      top: 2px;
      bottom: 2px;
      left: 2px;
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
      /* Inactive Week/Month text matches the card label brightness
         (--fg-label) so the whole row reads at one tier. The active
         button still flips to near-black on the amber pill below. */
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
    .total {
      font-family: var(--mono);
      font-size: 1.05rem;
      font-weight: 700;
      font-variant-numeric: tabular-nums;
      letter-spacing: -0.01em;
      color: var(--ftw-history-accent, var(--fg));
    }
    /* The "(X avg)" annotation next to the total. Rendered as a
       trailing span so we can dial it back visually — one step
       smaller than the total and tinted with --fg-muted so the
       primary number (total for the range) reads first and the avg
       is available as secondary context without competing for
       attention. */
    .total .avg-mini {
      font-size: 0.82rem;
      font-weight: 500;
      color: var(--fg-label);
      margin-left: 6px;
      letter-spacing: 0;
    }
    @media (max-width: 900px) {
      .card-inner { padding: var(--card-pad-tight, 12px 14px); }
    }
  `;

  static get observedAttributes() {
    return ["metric", "label", "accent", "default-range", "poll-ms", "range", "hide-toggle"];
  }

  constructor() {
    super();
    this._range = null;
    this._timer = null;
    this._chart = null;
    this._totalEl = null;
    this._toggleEl = null;
    this._reqSeq = 0;
    this._p2pStateListener = null;
    this._waitingForDirect = false;
    this._authListener = null;
  }

  connectedCallback() {
    super.connectedCallback();
    this._refresh();
    this._restartPolling();
    if (typeof window !== "undefined" &&
        window.ftwP2P &&
        typeof window.ftwP2P.onState === "function") {
      const listener = (s) => {
        if (this._p2pStateListener !== listener) return;
        if (s !== "direct") {
          this._waitingForDirect = true;
          return;
        }
        if (!this._waitingForDirect || !this.isConnected) return;
        this._waitingForDirect = false;
        dailyFetchCache.clear();
        this._refresh();
      };
      this._p2pStateListener = listener;
      window.ftwP2P.onState(listener);
    }
    if (typeof window !== "undefined") {
      this._authListener = () => {
        if (!this.isConnected) return;
        dailyFetchCache.clear();
        this._refresh();
      };
      window.addEventListener("ftw-owner-authenticated", this._authListener);
    }
  }
  disconnectedCallback() {
    if (this._timer) { clearInterval(this._timer); this._timer = null; }
    this._p2pStateListener = null;
    if (this._authListener && typeof window !== "undefined") {
      window.removeEventListener("ftw-owner-authenticated", this._authListener);
      this._authListener = null;
    }
  }

  attributeChangedCallback(name) {
    // `range` syncs BEFORE update() because render() also reads the
    // attribute into _range — if we called update() first, the check
    // below would see _range already equal to the new attribute and
    // skip the refresh, leaving the chart stuck on the old range.
    let rangeChanged = false;
    if (name === "range") {
      const next = this.getAttribute("range");
      if (next && next !== this._range) {
        this._range = next;
        rangeChanged = true;
      }
    }
    this.update();
    if (name === "metric" || name === "poll-ms") {
      this._refresh();
      this._restartPolling();
    }
    if (rangeChanged) this._refresh();
  }

  _restartPolling() {
    if (this._timer) { clearInterval(this._timer); this._timer = null; }
    // `??` not `||`: poll-ms="0" must disable polling, but "0" is truthy
    // in the ||-fallback so that path silently reverts to 300000.
    const raw = this.getAttribute("poll-ms");
    const ms = Number(raw ?? 300000);
    if (ms > 0 && this.isConnected) {
      this._timer = setInterval(() => this._refresh(), ms);
    }
  }

  _accent() {
    const explicit = this.getAttribute("accent");
    if (explicit) return explicit;
    const metric = this.getAttribute("metric") || "import";
    return DEFAULT_ACCENT[metric] || "var(--cyan)";
  }

  render() {
    // `range` attribute (pushed in by a wrapper) wins over internal
    // state. Falls back to `default-range`, then "week". Reading it on
    // every render keeps the component in sync whether the source is
    // an outer toggle or this card's own.
    const rangeAttr = this.getAttribute("range");
    if (rangeAttr && rangeAttr !== this._range) this._range = rangeAttr;
    if (this._range == null) {
      this._range = this.getAttribute("default-range") || "week";
    }
    const label = this.getAttribute("label") || "";
    const accent = this._accent();
    const hideToggle = this.hasAttribute("hide-toggle");
    this.style.setProperty("--ftw-history-accent", accent);
    // accent is applied to the bar-chart in afterRender() via
    // setAttribute, not interpolated here, so a future caller passing a
    // CSS value containing quotes can't escape the attribute context.
    const wk = this._range === "week";
    const toggleHtml = hideToggle ? "" : `
          <div class="toggle" role="tablist" data-active="${wk ? "week" : "month"}">
            <button type="button" role="tab" data-range="week"  aria-selected="${wk ? "true" : "false"}"${wk ? ' class="active"' : ""}>Week</button>
            <button type="button" role="tab" data-range="month" aria-selected="${!wk ? "true" : "false"}"${!wk ? ' class="active"' : ""}>Month</button>
          </div>`;
    return `
      <div class="card-inner">
        <div class="head">
          <div class="label">${escapeHtml(label)}</div>${toggleHtml}
        </div>
        <div class="total" data-role="total">— kWh</div>
        <ftw-bar-chart data-role="chart" loading="true"></ftw-bar-chart>
      </div>
    `;
  }

  afterRender() {
    this._chart   = this.shadowRoot.querySelector('[data-role="chart"]');
    this._totalEl = this.shadowRoot.querySelector('[data-role="total"]');
    this._toggleEl = this.shadowRoot.querySelector('.toggle');
    if (this._chart) this._chart.setAttribute("accent", this._accent());
    if (this._toggleEl) {
      this._toggleEl.addEventListener('click', (e) => {
        const btn = e.target.closest('button[data-range]');
        if (!btn) return;
        const next = btn.getAttribute('data-range');
        if (!next || next === this._range) return;
        this._range = next;
        this.update();
        this._refresh();
        this.dispatchEvent(new CustomEvent('ftw-history-range', {
          detail: { range: next, metric: this.getAttribute('metric') || '' },
          bubbles: true, composed: true,
        }));
      });
    }
  }

  _daysFor(range) {
    if (range === "month") {
      const now = new Date();
      return now.getDate();
    }
    return 7;
  }

  _refresh() {
    if (!this._chart || !this._totalEl) return;
    const metric = this.getAttribute("metric") || "import";
    const field  = FIELD_BY_METRIC[metric] || "import_wh";
    const days   = this._daysFor(this._range);

    // Bump the sequence so stale responses arriving after a Week/Month
    // toggle don't overwrite the newer chart. The shared request cache
    // intentionally keeps the underlying fetch alive for sibling cards;
    // the per-card seq guard decides whether a response still applies.
    const seq = ++this._reqSeq;

    this._chart.setAttribute("loading", "true");
    fetchDailyEnergy(days)
      .then((resp) => {
        if (seq !== this._reqSeq) return;
        const buckets = (resp && resp.days) || [];
        let sum = 0;
        // Pass kWh (not Wh) as `value` so the bar-chart's avg label
        // matches the per-bar displayValue units — otherwise the chart
        // would show bars as "10.5" and the avg line as "10500".
        const data = buckets.map((b) => {
          const wh = Number(b[field]) || 0;
          sum += wh;
          const kwh = wh / 1000;
          return {
            label: fmtDayShort(b.day),
            value: kwh,
            displayValue: kwh >= 100 ? kwh.toFixed(0) : kwh.toFixed(1),
            title: fmtDayShort(b.day) + ": " + fmtKwh(wh),
          };
        });
        const apply = () => {
          if (seq !== this._reqSeq) return;
          // "41.7 kWh total (5.9 kWh avg)" — the avg used to live as
          // a tag next to the dashed line inside the chart, but the
          // number is more scannable up here next to the total. The
          // line stays in the chart as a visual reference. Styled
          // via a trailing <span class="avg-mini"> so the avg reads
          // as secondary text (smaller, muted) instead of a second
          // prominent number.
          if (data.length) {
            const avgWh = sum / data.length;
            this._totalEl.innerHTML =
              escapeHtml(fmtKwh(sum) + " total") +
              ' <span class="avg-mini">(' +
              escapeHtml(fmtKwh(avgWh) + " avg") + ")</span>";
          } else {
            this._totalEl.textContent = "— kWh";
          }
          this._chart.data = data;
        };
        // `?delay=N` — hold in the skeleton state for N ms after the
        // fetch resolves, for inspecting the loading→loaded transition.
        const delay = ftwDebugDelay();
        if (delay > 0) setTimeout(apply, delay);
        else apply();
      })
      .catch((err) => {
        if (err && err.name === "AbortError") return;
        if (seq !== this._reqSeq) return;
        this._chart.removeAttribute("loading");
        this._chart.data = [];
        this._totalEl.textContent = "failed to load";
      });
  }
}

function fmtKwh(wh) {
  const kwh = (wh || 0) / 1000;
  if (kwh >= 100) return kwh.toFixed(0) + " kWh";
  if (kwh >= 10)  return kwh.toFixed(1) + " kWh";
  return kwh.toFixed(2) + " kWh";
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

customElements.define("ftw-history-card", FtwHistoryCard);
