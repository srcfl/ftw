// <ftw-notif-test-button> — single-click dispatcher for a test
// notification. POSTs to the configured endpoint (default
// /api/notifications/test) and surfaces the result in an adjacent
// status span. Drop in anywhere the operator should be able to
// verify the outbound transport without saving the full settings form.
//
// Attributes:
//   endpoint — POST URL (default "/api/notifications/test").
//   label    — button label text (default "Send test notification").
//
// Emits `ftw-notif-test` CustomEvent with { ok, error? } after each try.

import { FtwElement } from "./ftw-element.js";
import { apiFetch } from "./api-fetch.js";

export class FtwNotifTestButton extends FtwElement {
  static styles = `
    :host { display: inline-flex; align-items: center; gap: 8px; }
    button { background: var(--accent, #375a8f); color: var(--fg, #fff);
             border: 0; padding: 6px 12px; border-radius: 4px;
             cursor: pointer; font: inherit; }
    button[disabled] { opacity: 0.6; cursor: default; }
    .status { font-size: 0.85rem; color: var(--text-dim, #888); }
    .ok { color: var(--ok-fg, #7dd17d); }
    .err { color: var(--warn-fg, #e0b353); }
  `;

  constructor() {
    super();
    this._state = { busy: false, msg: "" };
  }

  async _send() {
    const url = this.getAttribute("endpoint") || "/api/notifications/test";
    this._state = { busy: true, msg: "sending…", cls: "" };
    this.update();
    try {
      const r = await apiFetch(url, { method: "POST" });
      const d = await r.json().catch(() => ({}));
      if (r.ok) {
        this._state = { busy: false, msg: "✓ Sent", cls: "ok" };
        this.dispatchEvent(new CustomEvent("ftw-notif-test", { detail: { ok: true } }));
      } else {
        const err = d.error || ("HTTP " + r.status);
        this._state = { busy: false, msg: "✗ " + err, cls: "err" };
        this.dispatchEvent(new CustomEvent("ftw-notif-test", { detail: { ok: false, error: err } }));
      }
    } catch (e) {
      this._state = { busy: false, msg: "✗ " + e, cls: "err" };
      this.dispatchEvent(new CustomEvent("ftw-notif-test", { detail: { ok: false, error: String(e) } }));
    }
    this.update();
  }

  afterRender() {
    const btn = this.shadowRoot.querySelector("button");
    if (btn) btn.addEventListener("click", () => this._send());
  }

  render() {
    const label = this.getAttribute("label") || "Send test notification";
    const cls = this._state.cls || "";
    const msg = (this._state.msg || "").replace(/[&<>]/g, c =>
      c === "&" ? "&amp;" : c === "<" ? "&lt;" : "&gt;");
    const disabled = this._state.busy ? " disabled" : "";
    return `<button${disabled}>${label}</button><span class="status ${cls}">${msg}</span>`;
  }
}

customElements.define("ftw-notif-test-button", FtwNotifTestButton);
