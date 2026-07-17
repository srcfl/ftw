// <ftw-notif-status> — live status indicator for the notifications
// subsystem. Polls /api/notifications/status on an interval and renders
// connection + counters. Drop-in anywhere the operator should see the
// state of outbound push notifications.
//
// Attributes:
//   interval-ms  — poll cadence (default 5000). Set to 0 to disable.
//   endpoint     — status URL (default "/api/notifications/status").
//
// Reuses the .ha-status-indicator styling by exposing the same classes
// on :host via light-DOM; callers can restyle by targeting the element.

import { FtwElement } from "./ftw-element.js";
import { apiFetch } from "./api-fetch.js";

export class FtwNotifStatus extends FtwElement {
  static styles = `
    :host { display: block; }
    .ind { display:block; padding:6px 10px; border-radius:6px;
           font-size:0.85rem; font-family:monospace; }
    .ok  { background: var(--ok-bg, #0c2a13); color: var(--ok-fg, #7dd17d); }
    .warn{ background: var(--warn-bg, #2c220d); color: var(--warn-fg, #e0b353); }
    .off { background: var(--ink-raised, #222); color: var(--text-dim, #888); }
  `;

  static get observedAttributes() { return ["interval-ms", "endpoint"]; }

  constructor() {
    super();
    this._timer = null;
    this._state = { kind: "loading", text: "checking…" };
  }

  connectedCallback() {
    super.connectedCallback();
    this._refresh();
    this._startPolling();
  }

  disconnectedCallback() {
    if (this._timer) { clearInterval(this._timer); this._timer = null; }
  }

  attributeChangedCallback() {
    this._startPolling();
  }

  _startPolling() {
    if (this._timer) { clearInterval(this._timer); this._timer = null; }
    const n = parseInt(this.getAttribute("interval-ms") || "5000", 10);
    if (n > 0) this._timer = setInterval(() => this._refresh(), n);
  }

  async _refresh() {
    const url = this.getAttribute("endpoint") || "/api/notifications/status";
    try {
      const r = await apiFetch(url);
      const d = await r.json();
      if (!d.enabled) {
        this._state = { kind: "off", text: "○  disabled in config" };
      } else {
        const parts = [
          (d.provider || "?") + " → " + (d.server || "?"),
          "topic " + (d.topic || "(unset)"),
          "sent " + (d.sent || 0) + " / failed " + (d.failed || 0),
        ];
        this._state = { kind: "ok", text: "● " + parts.join("  ·  ") };
      }
    } catch (e) {
      this._state = { kind: "warn", text: "? status endpoint unreachable" };
    }
    this.update();
  }

  render() {
    const cls = this._state.kind === "ok" ? "ok"
              : this._state.kind === "warn" ? "warn" : "off";
    const txt = (this._state.text || "").replace(/[&<>]/g, c =>
      c === "&" ? "&amp;" : c === "<" ? "&lt;" : "&gt;");
    return `<span class="ind ${cls}">${txt}</span>`;
  }
}

customElements.define("ftw-notif-status", FtwNotifStatus);
