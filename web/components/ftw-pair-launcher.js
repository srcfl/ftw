// <ftw-pair-launcher> — footer-mounted launcher for the pair-session feature.
//
// Stays out of the way (rare feature) but discoverable. Polls
// /api/pair/status every 30 s to know whether a session is currently
// active and reflects that in its label.
//
// When idle:   "Need help? Start a pair session →"  (--ink-raised color)
// When active: "Pair session active · {h}h {m}m left" (--accent-e color)
//
// Click → opens <ftw-modal> wrapping <ftw-pair-card>. The card handles
// start / active / abort internally; the launcher only owns the entry
// point and the active-state indicator.
//
// The launcher polls independently of the modal. Closing the modal while
// a session is still active keeps the "active" label in the footer.

import { FtwElement } from "./ftw-element.js";
import "./ftw-pair-card.js";
import "./ftw-modal.js";

const POLL_MS = 30_000;

class FtwPairLauncher extends FtwElement {
  static styles = `
    :host {
      display: block;
    }

    .launcher-row {
      display: flex;
      justify-content: flex-end;
      padding: 8px 0 4px;
    }

    .launcher-btn {
      appearance: none;
      background: transparent;
      border: none;
      cursor: pointer;
      font-family: var(--sans);
      font-size: 0.78rem;
      color: var(--ink-raised2, var(--fg-dim, var(--fg)));
      opacity: 0.65;
      padding: 4px 0;
      transition: opacity 0.15s;
      text-decoration: underline;
      text-decoration-style: dotted;
      text-underline-offset: 2px;
    }

    .launcher-btn:hover {
      opacity: 1;
    }

    .launcher-btn.active {
      color: var(--accent-e);
      opacity: 1;
      text-decoration: none;
    }
  `;

  constructor() {
    super();
    this._session = null;
    this._tick = null;
  }

  connectedCallback() {
    super.connectedCallback();
    this._poll();
    this._tick = setInterval(() => this._poll(), POLL_MS);
  }

  disconnectedCallback() {
    if (this._tick) clearInterval(this._tick);
  }

  async _poll() {
    try {
      const r = await fetch("/api/pair/status");
      this._session = r.ok ? await r.json() : null;
    } catch (_) {
      this._session = null;
    }
    this.update();
  }

  _label() {
    if (!this._session) {
      return "Need help? Start a pair session →";
    }
    const remaining = this._remaining();
    return `Pair session active · ${remaining} left`;
  }

  _remaining() {
    if (!this._session || !this._session.started_at || !this._session.ttl_s) {
      return "—";
    }
    const startedMs = Date.parse(this._session.started_at);
    const expiry = startedMs + this._session.ttl_s * 1000;
    const left = Math.max(0, Math.floor((expiry - Date.now()) / 1000));
    const h = Math.floor(left / 3600);
    const m = Math.floor((left % 3600) / 60);
    return `${h}h ${m}m`;
  }

  _openModal() {
    const modal = this.shadowRoot.getElementById("pair-modal");
    if (modal) modal.setAttribute("open", "");
  }

  render() {
    const active = !!this._session;
    return `
      <div class="launcher-row">
        <button class="launcher-btn${active ? " active" : ""}" id="launcher-btn">
          ${escapeHTML(this._label())}
        </button>
      </div>

      <ftw-modal id="pair-modal" style="--ftw-modal-max-width:520px">
        <span slot="title">Pair session</span>
        <ftw-pair-card></ftw-pair-card>
      </ftw-modal>
    `;
  }

  afterRender() {
    const btn = this.shadowRoot.getElementById("launcher-btn");
    if (btn) btn.addEventListener("click", () => this._openModal());

    // When the modal closes, re-poll so the label reflects any state
    // change that happened while the card was open (e.g. session started
    // or aborted). Debounce: poll once with a short delay to let the
    // card's own abort/start call settle on the server.
    const modal = this.shadowRoot.getElementById("pair-modal");
    if (modal) {
      modal.addEventListener("ftw-modal-close", () => {
        setTimeout(() => this._poll(), 500);
      });
    }
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

customElements.define("ftw-pair-launcher", FtwPairLauncher);
