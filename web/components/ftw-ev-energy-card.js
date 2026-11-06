// <ftw-ev-energy-card> — EV charging energy and cost for the last 7 and 30
// local days. Fetches GET /api/ev/energy. Compact mode is the week/month
// comparison; the full card adds the daily chart.

import { FtwElement } from "./ftw-element.js";
import { apiFetch } from "./api-fetch.js";
import "./ftw-bar-chart.js";
import {
  chargerName,
  costFootnote,
  formatEVCost,
  formatEnergyKWh,
  windowById,
} from "./ev-energy-format.js";

const CACHE_TTL_MS = 15000;
const cache = { at: 0, data: null, promise: null };

function fetchEVEnergy() {
  const now = Date.now();
  if (cache.data && now - cache.at < CACHE_TTL_MS) return Promise.resolve(cache.data);
  if (cache.promise && now - cache.at < CACHE_TTL_MS) return cache.promise;
  const promise = apiFetch("/api/ev/energy")
    .then((r) => {
      if (!r.ok) throw new Error("HTTP " + r.status);
      return r.json();
    })
    .then((body) => {
      cache.at = Date.now();
      cache.data = body;
      cache.promise = null;
      return body;
    })
    .catch((err) => {
      if (cache.promise === promise) cache.promise = null;
      throw err;
    });
  cache.at = now;
  cache.promise = promise;
  return promise;
}

function escapeHtml(s) {
  return String(s).replace(/[<>&"']/g, (c) =>
    ({ "<": "&lt;", ">": "&gt;", "&": "&amp;", '"': "&quot;", "'": "&#39;" }[c]));
}

function fmtDayShort(iso) {
  const parts = String(iso || "").split("-");
  if (parts.length !== 3) return iso || "";
  const d = new Date(+parts[0], +parts[1] - 1, +parts[2]);
  return d.toLocaleDateString(undefined, { weekday: "short", day: "numeric" });
}

function periodCells(week, month, currency) {
  return [week, month].map((row) => {
    const label = row && row.id === "7d" ? "Last 7 days" : "Last 30 days";
    return `<div class="period"><span>${label}</span><strong>${escapeHtml(formatEnergyKWh(row && row.energy_wh))}</strong><em>${escapeHtml(formatEVCost(row && row.cost_ore, row && row.unpriced_wh, currency))}</em></div>`;
  }).join("");
}

class FtwEvEnergyCard extends FtwElement {
  static styles = `
    :host { display: block; margin: 0 0 12px; }
    :host([hidden]) { display: none; }
    .card-inner {
      display: flex;
      flex-direction: column;
      gap: 10px;
      background: var(--ink-raised);
      border: 1px solid var(--line);
      border-radius: var(--radius-md, 10px);
      padding: var(--card-pad, 14px 16px);
    }
    .head, .chart-head {
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
    .periods {
      display: grid;
      grid-template-columns: 1fr 1fr;
      gap: 12px;
    }
    .period span, .chargers th {
      font-family: var(--mono);
      font-size: 10px;
      color: var(--fg-label);
      letter-spacing: 0.08em;
      text-transform: uppercase;
    }
    .period strong {
      display: block;
      margin-top: 4px;
      font-family: var(--mono);
      font-size: 1.25rem;
      font-weight: 700;
      font-variant-numeric: tabular-nums;
      color: var(--fg);
    }
    .period em, .note, .empty, .chargers td {
      font-style: normal;
      font-family: var(--sans);
      font-size: 0.78rem;
      color: var(--fg-muted);
    }
    .chargers { width: 100%; border-collapse: collapse; }
    .chargers th, .chargers td { text-align: left; padding: 4px 8px 4px 0; }
    .chargers td:first-child { color: var(--fg); }
    .note { line-height: 1.4; }
    .toggle {
      display: inline-grid;
      grid-auto-flow: column;
      border: 1px solid var(--line);
      border-radius: 999px;
      background: var(--ink-sunken);
      padding: 2px;
    }
    .toggle button {
      background: transparent;
      border: 0;
      color: var(--fg-label);
      font-family: var(--mono);
      font-size: 10px;
      letter-spacing: 0.12em;
      text-transform: uppercase;
      padding: 4px 10px;
      cursor: pointer;
      border-radius: 999px;
    }
    .toggle button.active { background: var(--accent-e); color: var(--on-accent, #0a0a0a); }
    @media (max-width: 700px) {
      .periods { grid-template-columns: 1fr; }
    }
  `;

  constructor() {
    super();
    this._payload = null;
    this._error = "";
    this._chartRange = "7d";
    this._timer = null;
    this._seq = 0;
  }

  connectedCallback() {
    super.connectedCallback();
    if (!this._onVisibility) {
      this._onVisibility = () => this._syncPolling();
      document.addEventListener("visibilitychange", this._onVisibility);
    }
    this._syncPolling();
  }

  disconnectedCallback() {
    if (this._timer) clearInterval(this._timer);
    this._timer = null;
    if (this._onVisibility) {
      document.removeEventListener("visibilitychange", this._onVisibility);
      this._onVisibility = null;
    }
  }

  _syncPolling() {
    if (this._timer) clearInterval(this._timer);
    this._timer = null;
    if (!this.isConnected || document.hidden) return;
    this._refresh();
    const ms = Number(this.getAttribute("poll-ms") ?? 300000);
    if (ms > 0) this._timer = setInterval(() => this._refresh(), ms);
  }

  _refresh() {
    const seq = ++this._seq;
    fetchEVEnergy()
      .then((body) => {
        if (seq !== this._seq) return;
        this._payload = body;
        this._error = "";
        this._applyVisibility();
        this.update();
      })
      .catch((err) => {
        if (err && err.name === "AbortError") return;
        if (seq !== this._seq) return;
        this._error = "Could not load EV charging.";
        this.update();
      });
  }

  _applyVisibility() {
    if (!this.hasAttribute("compact")) return;
    const month = windowById(this._payload && this._payload.windows, "30d");
    const energy = month && Number(month.energy_wh) > 0;
    if (energy) this.removeAttribute("hidden");
    else this.setAttribute("hidden", "");
  }

  render() {
    if (this._error) {
      return `<div class="card-inner"><div class="label">EV charging</div><p class="empty">${escapeHtml(this._error)}</p></div>`;
    }
    if (!this._payload) {
      return `<div class="card-inner"><div class="label">EV charging</div><p class="empty">Loading…</p></div>`;
    }
    const currency = this._payload.currency || "SEK";
    const week = windowById(this._payload.windows, "7d");
    const month = windowById(this._payload.windows, "30d");
    const energy = (month && Number(month.energy_wh)) || 0;
    if (!energy) {
      return `<div class="card-inner"><div class="label">EV charging</div><p class="empty">No EV charging in the last 30 days.</p></div>`;
    }
    const compact = this.hasAttribute("compact");
    const chargers = compact ? "" : chargerLines(week, month, currency);
    const chart = compact ? "" : chartBlock(this._chartRange);
    const note = compact
      ? ((month && month.cost_partial) ? "Cost leaves out hours with no electricity price." : "")
      : costFootnote(month || week);
    return `<div class="card-inner">
      <div class="head"><div class="label">EV charging</div></div>
      <div class="periods">${periodCells(week, month, currency)}</div>
      ${chargers}
      ${chart}
      ${note ? `<p class="note">${escapeHtml(note)}</p>` : ""}
    </div>`;
  }

  afterRender() {
    const chart = this.shadowRoot.querySelector('[data-role="chart"]');
    if (chart && this._payload) {
      const row = windowById(this._payload.windows, this._chartRange) || windowById(this._payload.windows, "30d");
      const currency = this._payload.currency || "SEK";
      chart.setAttribute("accent", "var(--cyan)");
      chart.data = ((row && row.daily) || []).map((day) => {
        const kwh = (Number(day.energy_wh) || 0) / 1000;
        const cost = formatEVCost(day.cost_ore, day.unpriced_wh, currency);
        return {
          label: fmtDayShort(day.day),
          value: kwh,
          displayValue: kwh >= 100 ? kwh.toFixed(0) : kwh.toFixed(1),
          title: fmtDayShort(day.day) + ": " + formatEnergyKWh(day.energy_wh) + " · " + cost,
        };
      });
    }
    const toggle = this.shadowRoot.querySelector(".toggle");
    if (toggle) {
      toggle.addEventListener("click", (event) => {
        const button = event.target.closest("button[data-range]");
        if (!button) return;
        const next = button.getAttribute("data-range");
        if (!next || next === this._chartRange) return;
        this._chartRange = next;
        this.update();
      });
    }
  }
}

function chartBlock(range) {
  const week = range !== "30d";
  return `<div class="chart-head">
      <div class="label">Daily energy</div>
      <div class="toggle" role="tablist" aria-label="Chart range">
        <button type="button" data-range="7d"${week ? ' class="active" aria-selected="true"' : ' aria-selected="false"'}>7 days</button>
        <button type="button" data-range="30d"${week ? ' aria-selected="false"' : ' class="active" aria-selected="true"'}>30 days</button>
      </div>
    </div>
    <ftw-bar-chart data-role="chart" chart-height="96"></ftw-bar-chart>`;
}

function chargerLines(week, month, currency) {
  const seen = new Map();
  for (const row of [week, month]) {
    for (const charger of (row && row.chargers) || []) {
      if (charger && charger.asset_id && !seen.has(charger.asset_id)) seen.set(charger.asset_id, charger);
    }
  }
  if (seen.size < 2) return "";
  const ids = [...seen.keys()].sort();
  const find = (row, id) => ((row && row.chargers) || []).find((c) => c.asset_id === id);
  const body = ids.map((id) => {
    const name = chargerName(seen.get(id));
    const w = find(week, id);
    const m = find(month, id);
    return `<tr><td>${escapeHtml(name)}</td><td>${escapeHtml(formatEnergyKWh(w && w.energy_wh))} · ${escapeHtml(formatEVCost(w && w.cost_ore, w && w.unpriced_wh, currency))}</td><td>${escapeHtml(formatEnergyKWh(m && m.energy_wh))} · ${escapeHtml(formatEVCost(m && m.cost_ore, m && m.unpriced_wh, currency))}</td></tr>`;
  }).join("");
  return `<table class="chargers"><thead><tr><th>Charger</th><th>7 days</th><th>30 days</th></tr></thead><tbody>${body}</tbody></table>`;
}

customElements.define("ftw-ev-energy-card", FtwEvEnergyCard);
