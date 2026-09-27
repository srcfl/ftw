// <ftw-update-check> — pre-setup notice that a newer release is published.
//
// Usage:
//
//   <ftw-update-check></ftw-update-check>
//
// Behavior:
//   1. On connect, silently calls GET /api/version/check.
//   2. If the backend returns 503 (no self-update on this install) or a
//      network error fires, the component stays invisible — setup is never
//      blocked by the check.
//   3. The banner only renders when the response has
//      update_available && !skipped && native. It names `ftw update`; the
//      owner runs it on the machine after setup (ADR 0007, decision 12).
//   4. Continue hides the card for this page load only. We do NOT POST
//      /api/version/skip — that would silence the dashboard's
//      <ftw-update-badge> too, which is a separate decision the operator
//      should make from the dashboard itself.
//
// Shared tokens declared on :root in /components/theme.css keep the
// component consistent in setup and the dashboard.

import { FtwElement } from "./ftw-element.js";
import { apiFetch } from "./api-fetch.js";

class FtwUpdateCheck extends FtwElement {
  static styles = `
    :host {
      display: block;
      width: 100%;
    }
    :host(.hidden) { display: none; }

    /* Tokens resolved against /components/theme.css — amber single-
       accent palette, ink canvas and hairline borders. */
    .banner {
      display: flex;
      flex-direction: column;
      gap: 10px;
      padding: 14px 16px;
      background: var(--ink-raised);
      border: 1px solid color-mix(in srgb, var(--accent-e) 40%, var(--line));
      border-radius: 10px;
      text-align: left;
    }
    .banner-title {
      font-family: var(--mono, ui-monospace, monospace);
      font-weight: 500;
      color: var(--accent-e);
      font-size: 0.72rem;
      text-transform: uppercase;
      letter-spacing: 0.18em;
    }
    .banner-detail {
      font-size: 0.85rem;
      font-family: var(--mono, ui-monospace, monospace);
      color: var(--fg);
    }
    .banner-hint { font-size: 0.8rem; color: var(--fg-dim); }
    .banner-hint code { font-family: var(--mono, ui-monospace, monospace); color: var(--fg); }
    .banner-notes {
      font-size: 0.78rem;
      color: var(--accent-e);
      text-decoration: none;
      align-self: flex-start;
    }
    .banner-notes:hover { text-decoration: underline; }
    .banner-actions {
      display: flex;
      gap: 10px;
      align-items: center;
      flex-wrap: wrap;
    }

    button {
      font-family: var(--sans, system-ui, sans-serif);
      cursor: pointer;
    }
    .btn-skip {
      background: none;
      border: none;
      color: var(--fg-muted);
      font-family: var(--mono, ui-monospace, monospace);
      font-size: 0.75rem;
      letter-spacing: 0.08em;
      padding: 6px 10px;
      transition: color 0.15s;
    }
    .btn-skip:hover { color: var(--fg); }
  `;

  constructor() {
    super();
    this._info = null;       // last /api/version/check payload
    this.classList.add("hidden");
  }

  connectedCallback() {
    super.connectedCallback();
    this._check();
  }

  update() {
    const action = this.shadowRoot.activeElement?.dataset?.action;
    super.update();
    if (action) {
      const button = [...this.shadowRoot.querySelectorAll("[data-action]")].find(el => el.dataset.action === action);
      if (button && !button.disabled) button.focus({ preventScroll:true });
    }
  }

  // ---- data ----
  _check() {
    apiFetch("/api/version/check")
      .then((r) => {
        // 503 = no self-update on this install. Stay invisible — this
        // is config, not an error.
        if (r.status === 503) return null;
        return r.json().catch(() => null);
      })
      .then((info) => {
        if (!info || typeof info !== "object") return;
        this._info = info;
        this.update();
      })
      .catch(() => { /* silent — never a setup blocker */ });
  }

  // ---- actions ----
  _dismiss() {
    // Session-only: wipe the local flag so the banner hides but don't
    // persist via /api/version/skip — the dashboard badge should still
    // nudge afterwards.
    if (this._info) this._info.update_available = false;
    this.update();
  }

  // ---- render ----
  render() {
    const info = this._info;
    // The banner only informs: the owner runs ftw update on the box.
    const showBanner =
      !!info &&
      info.update_available &&
      !info.skipped &&
      info.native === true;

    // Toggle :host visibility so the element collapses when it has
    // nothing to say — the wizard layout shouldn't reserve space.
    this.classList.toggle("hidden", !showBanner);
    return showBanner ? this._bannerHTML(info) : "";
  }

  afterRender() {
    const dis = this.shadowRoot.querySelector('[data-action="dismiss"]');
    if (dis) dis.addEventListener("click", () => this._dismiss());
  }

  _bannerHTML(info) {
    const href = safeHref(info.release_notes_url);
    const notes = href
      ? `<a class="banner-notes" href="${escapeHTML(href)}" target="_blank" rel="noopener">Release notes ↗</a>`
      : "";
    return `
      <div class="banner" part="banner">
        <div class="banner-title">A newer release is published</div>
        <div class="banner-detail">${escapeHTML(info.current || "?")}  →  ${escapeHTML(info.latest || "?")}</div>
        ${notes}
        <div class="banner-hint">After setup, install it on the machine that runs FTW with <code>ftw update</code>.</div>
        <div class="banner-actions">
          <button class="btn-skip" data-action="dismiss">Continue</button>
        </div>
      </div>
    `;
  }
}

// safeHref rejects anything that isn't http:/https:. release_notes_url
// comes from the GitHub Releases API; belt-and-brace against a stray
// javascript:/data: URL ending up in the payload.
function safeHref(u) {
  if (!u) return "";
  try {
    const p = new URL(String(u), window.location.href);
    if (p.protocol === "http:" || p.protocol === "https:") return p.toString();
  } catch (_) { /* fall through */ }
  return "";
}

function escapeHTML(s) {
  return String(s == null ? "" : s)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

customElements.define("ftw-update-check", FtwUpdateCheck);
