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
//
// Modal lifecycle: the <ftw-modal> element is created ONCE in
// connectedCallback and appended to the shadow root directly. Poll
// cycles only update the label text — they never touch or recreate the
// modal element, so an open modal is not destroyed by a background poll.

import { FtwElement } from "./ftw-element.js";
import { ownerFetch } from "./owner-fetch.js";
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
    // Stable modal reference — created once, never destroyed by a poll cycle.
    this._modal = null;
  }

  connectedCallback() {
    // Perform the initial render (lays down the launcher-row button).
    super.connectedCallback();

    // Create the modal element once and attach it to the shadow root.
    // We do this here rather than in render() so that polling never
    // destroys and recreates it. The modal stays alive for the lifetime
    // of the launcher element.
    if (!this._modal) {
      this._modal = document.createElement("ftw-modal");
      this._modal.id = "pair-modal";
      this._modal.style.setProperty("--ftw-modal-max-width", "520px");

      const titleSlot = document.createElement("span");
      titleSlot.slot = "title";
      titleSlot.textContent = "Pair session";
      this._modal.appendChild(titleSlot);

      const card = document.createElement("ftw-pair-card");
      this._modal.appendChild(card);

      this._modal.addEventListener("ftw-modal-close", () => {
        // Re-poll after the modal closes so the label reflects any state
        // change that happened while the card was open (e.g. session started
        // or aborted). Short delay lets the card's abort/start call settle.
        setTimeout(() => this._poll(), 500);
      });

      this.shadowRoot.appendChild(this._modal);
    }

    this._poll();
    this._tick = setInterval(() => this._poll(), POLL_MS);
  }

  disconnectedCallback() {
    if (this._tick) clearInterval(this._tick);
    this._modal = null;
  }

  async _poll() {
    try {
      const r = await ownerFetch("/api/pair/status");
      this._session = r.ok ? await r.json() : null;
    } catch (_) {
      this._session = null;
    }
    // Update only the label button — do NOT call this.update() here as that
    // would wipe the shadow root and destroy the stable modal element.
    this._updateLabel();
  }

  // _updateLabel refreshes only the launcher button text and active class.
  // This is the surgical alternative to a full this.update() call so that
  // the modal DOM is never touched by a background poll cycle.
  _updateLabel() {
    const btn = this.shadowRoot.getElementById("launcher-btn");
    if (!btn) return;
    const active = !!this._session;
    btn.textContent = this._label();
    if (active) {
      btn.classList.add("active");
    } else {
      btn.classList.remove("active");
    }
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
    if (this._modal) this._modal.setAttribute("open", "");
  }

  render() {
    const active = !!this._session;
    // render() only produces the launcher button row. The modal is managed
    // separately as a stable element in connectedCallback — it is not
    // included here so that update() calls never affect it.
    return `
      <div class="launcher-row">
        <button class="launcher-btn${active ? " active" : ""}" id="launcher-btn">
          ${escapeHTML(this._label())}
        </button>
      </div>
    `;
  }

  afterRender() {
    const btn = this.shadowRoot.getElementById("launcher-btn");
    if (btn) btn.addEventListener("click", () => this._openModal());
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
