// <ftw-pair-card> — surfaces an active pair session on the dashboard.
//
// Polls /api/pair/status every 5 s. Renders the wormhole code, the
// owner-supplied intent, a TTL countdown, the running tool counter,
// and an Abort button that POSTs /api/pair/abort. When the endpoint
// returns 404 (no active session) the card hides itself entirely via
// :host(.hidden) { display:none } — identical pattern to ftw-update-check.

import { FtwElement } from "./ftw-element.js";

const POLL_MS = 5000;

class FtwPairCard extends FtwElement {
  static styles = `
    :host { display: block; }
    :host(.hidden) { display: none; }

    .pair-card {
      border: 1px solid var(--line);
      padding: 16px;
      background: var(--ink-raised);
      margin-bottom: 8px;
    }
    .pair-card header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      margin-bottom: 8px;
    }
    .eyebrow {
      font-family: var(--mono);
      letter-spacing: 0.18em;
      font-size: 11px;
      text-transform: uppercase;
      color: var(--accent-e);
    }
    .code {
      font-family: var(--mono);
      font-size: 20px;
      font-weight: 600;
      margin: 0 0 8px;
      color: var(--fg);
      letter-spacing: 0.06em;
    }
    .intent {
      font-family: var(--sans);
      color: var(--fg);
      margin: 0 0 12px;
      font-size: 0.9rem;
    }
    dl {
      display: grid;
      grid-template-columns: max-content 1fr;
      gap: 4px 16px;
      font-family: var(--mono);
      font-size: 12px;
      margin: 0;
    }
    dt {
      color: var(--ink-raised2, var(--fg-dim, var(--fg)));
      opacity: 0.6;
    }
    dd {
      margin: 0;
      color: var(--fg);
    }
    button.abort {
      background: var(--accent-e);
      color: #0a0a0a;
      border: 0;
      padding: 4px 10px;
      font-family: var(--mono);
      cursor: pointer;
      font-size: 11px;
      letter-spacing: 0.1em;
      text-transform: uppercase;
    }
    button.abort:hover {
      opacity: 0.85;
    }
  `;

  constructor() {
    super();
    this._state = null;
    this._tick = null;
  }

  connectedCallback() {
    super.connectedCallback();
    this._refresh();
    this._tick = setInterval(() => this._refresh(), POLL_MS);
  }

  disconnectedCallback() {
    if (this._tick) clearInterval(this._tick);
  }

  async _refresh() {
    try {
      const r = await fetch("/api/pair/status");
      if (r.status === 404) {
        this._state = null;
      } else if (r.ok) {
        this._state = await r.json();
      }
    } catch (_) {}
    this.update();
  }

  async _abort() {
    if (!confirm("End the pair session now?")) return;
    await fetch("/api/pair/abort", { method: "POST" });
    this._state = null;
    this.update();
  }

  render() {
    if (!this._state) {
      this.classList.add("hidden");
      return "";
    }
    this.classList.remove("hidden");

    const remaining = this._computeRemaining();
    const lastTools = (this._state.last_tools || [])
      .map((t) => escapeHTML(t))
      .join(", ") || "—";

    return `
      <div class="pair-card">
        <header>
          <span class="eyebrow">Pair session active</span>
          <button class="abort" id="abort-btn">Abort</button>
        </header>
        <p class="code">${escapeHTML(this._state.code)}</p>
        <p class="intent">${escapeHTML(this._state.intent || "(no intent set)")}</p>
        <dl>
          <dt>TTL</dt><dd>${escapeHTML(remaining)}</dd>
          <dt>Tool calls</dt><dd>${this._state.tool_count ?? 0}</dd>
          <dt>Last tools</dt><dd>${lastTools}</dd>
        </dl>
      </div>
    `;
  }

  afterRender() {
    const btn = this.shadowRoot.getElementById("abort-btn");
    if (btn) btn.addEventListener("click", () => this._abort());
  }

  _computeRemaining() {
    if (!this._state.started_at || !this._state.ttl_s) return "—";
    const startedMs = Date.parse(this._state.started_at);
    const expiry = startedMs + this._state.ttl_s * 1000;
    const left = Math.max(0, Math.floor((expiry - Date.now()) / 1000));
    const h = Math.floor(left / 3600);
    const m = Math.floor((left % 3600) / 60);
    return `${h}h ${m}m`;
  }
}

function escapeHTML(s) {
  return String(s == null ? "" : s)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

customElements.define("ftw-pair-card", FtwPairCard);
