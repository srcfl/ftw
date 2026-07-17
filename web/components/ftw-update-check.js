// <ftw-update-check> — pre-setup update banner.
//
// Usage:
//
//   <ftw-update-check></ftw-update-check>
//
// Behavior:
//   1. On connect, silently calls GET /api/version/check.
//   2. If the backend returns 503 (self-update gated off) or a network
//      error fires, the component stays invisible — setup is never
//      blocked by the check.
//   3. The banner only renders when the response has
//      update_available && !skipped && sidecar_ready. sidecar_ready
//      is true exclusively in docker-compose deploys where the
//      ftw-updater sidecar's Unix socket is reachable; native installs
//      and dev runs keep the banner hidden so we don't offer an Update
//      button that can only fail.
//   4. Update-now posts /api/version/update, opens an <ftw-modal>-based
//      progress overlay, polls /api/version/update/status, and
//      cache-busts reloads on `done`. Failure and ~3-minute timeout
//      swap the spinner for a Reload / Continue-setup escape hatch so
//      the operator can bail out.
//   5. Continue-anyway hides the card for this page load only. We do
//      NOT POST /api/version/skip — that would silence the dashboard's
//      <ftw-update-badge> too, which is a separate decision the operator
//      should make from the dashboard itself.
//
// Reuse:
//   - <ftw-modal> supplies the overlay
//     chrome, ESC/backdrop-close handling, and theming tokens.
//   - Shared tokens declared on :root in /components/theme.css keep the
//     component consistent in setup and the dashboard.

import { FtwElement } from "./ftw-element.js";
import { apiFetch } from "./api-fetch.js";
import "./ftw-modal.js";

const STATUS_POLL_MS = 2000;
const UPDATE_SOFT_TIMEOUT_MS = 180 * 1000;

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
    .btn-primary {
      padding: 11px 18px;
      border: none;
      border-radius: 8px;
      background: var(--accent-e);
      color: #0a0a0a;
      font-size: 14px;
      font-weight: 500;
      transition: transform 0.12s;
    }
    .btn-primary:hover { transform: translateY(-1px); }
    .btn-secondary {
      padding: 10px 18px;
      border: 1px solid var(--line);
      border-radius: 8px;
      background: transparent;
      color: var(--fg);
      font-size: 14px;
      transition: border-color 0.15s;
    }
    .btn-secondary:hover { border-color: var(--fg-dim); }
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

    /* Progress overlay content — lives inside <ftw-modal>. The modal
       owns the backdrop, positioning, and ESC/click-close. We drive
       the content and, while actively updating, block close by
       cancelling the ftw-modal-close event in afterRender(). */
    .progress { text-align: center; padding: 0.5rem 0; }
    .progress .spinner {
      display: inline-block;
      width: 28px;
      height: 28px;
      border: 3px solid var(--line);
      border-top-color: var(--accent-e);
      border-radius: 50%;
      animation: spin 0.9s linear infinite;
      margin-bottom: 0.75rem;
    }
    .progress h3 {
      margin: 0 0 0.4rem;
      font-size: 1rem;
      color: var(--fg);
    }
    .progress .msg {
      font-size: 0.88rem;
      color: var(--fg);
      margin: 0 0 0.3rem;
    }
    .progress .hint {
      font-size: 0.78rem;
      color: var(--fg-dim);
      margin: 0;
    }
    @keyframes spin { to { transform: rotate(360deg); } }

    /* Hide the modal's X during an active update. The operator should
       wait for the reload (or the timeout escape hatch) rather than
       silently dismissing a container mid-recreate. */
    ftw-modal.busy::part(close) { display: none; }

    .overlay-actions {
      display: flex;
      gap: 10px;
      justify-content: flex-end;
      flex-wrap: wrap;
    }
  `;

  constructor() {
    super();
    this._info = null;       // last /api/version/check payload
    this._phase = "idle";    // idle | updating | timedOut | failed
    this._status = null;     // last /api/version/update/status payload
    this._statusTimer = null;
    this._pollAbort = null;  // AbortController for in-flight status fetches
    this._updateStartedAt = 0;
    this.classList.add("hidden");
  }

  connectedCallback() {
    super.connectedCallback();
    this._check();
  }

  disconnectedCallback() {
    this._stopPolling();
  }

  // ---- data ----
  _check() {
    apiFetch("/api/version/check")
      .then((r) => {
        // 503 = self-update disabled by deploy. Stay invisible — this
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
  _beginUpdate() {
    this._phase = "updating";
    this._status = { state: "starting" };
    this._updateStartedAt = Date.now();
    this.update();

    apiFetch("/api/version/update", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
    })
      .then((r) => r.json().then((b) => ({ ok: r.ok, body: b })))
      .then((res) => {
        if (!res.ok) {
          this._fail((res.body && res.body.error) || "failed to start");
          return;
        }
        this._startPolling();
      })
      .catch((e) => this._fail(String(e)));
  }

  _dismiss() {
    // Session-only: wipe the local flag so the banner hides but don't
    // persist via /api/version/skip — the dashboard badge should still
    // nudge afterwards.
    if (this._info) this._info.update_available = false;
    this.update();
  }

  _startPolling() {
    this._stopPolling();
    this._pollAbort = new AbortController();
    this._statusTimer = setInterval(() => this._tick(), STATUS_POLL_MS);
    this._tick();
  }

  // Stops the poll interval AND aborts any in-flight status fetch. Called
  // from every transition out of "updating" (fail, timeout, cancel, modal
  // close, disconnect) so a late `state === "done"` response can't hijack
  // the setup flow with a surprise _reload() after the operator bailed.
  _stopPolling() {
    clearInterval(this._statusTimer);
    this._statusTimer = null;
    if (this._pollAbort) {
      this._pollAbort.abort();
      this._pollAbort = null;
    }
  }

  _tick() {
    const signal = this._pollAbort ? this._pollAbort.signal : undefined;
    apiFetch("/api/version/update/status", { signal })
      .then((r) => (r.ok ? r.json() : null))
      .then((st) => {
        // Belt-and-braces: if the phase was reset while the fetch was in
        // flight (cancel button / modal close / timeout), AbortController
        // should have rejected the promise above — but guard anyway so a
        // late resolution can never trigger _reload() after bailout.
        if (!st || this._phase !== "updating") return;
        this._status = st;
        if (st.state === "failed") {
          this._fail(st.message || "Update failed");
          return;
        }
        if (st.state === "done") {
          this._stopPolling();
          // Give the new container a moment to open its listener,
          // then cache-bust reload so stale JS is replaced.
          setTimeout(() => {
            if (this._phase === "updating") this._reload();
          }, 800);
        }
        this.update();
      })
      .catch(() => { /* aborted, or main container mid-restart — both fine */ });

    if (Date.now() - this._updateStartedAt > UPDATE_SOFT_TIMEOUT_MS) {
      if (this._phase === "updating") {
        this._stopPolling();
        this._phase = "timedOut";
        this.update();
      }
    }
  }

  _fail(msg) {
    this._stopPolling();
    this._phase = "failed";
    this._status = { state: "failed", message: msg };
    this.update();
  }

  _reload() {
    const u = new URL(window.location.href);
    u.searchParams.set("_u", String(Date.now()));
    window.location.replace(u.toString());
  }

  // ---- render ----
  render() {
    const info = this._info;
    // Banner is only useful when the full pull+restart flow is actionable.
    // sidecar_ready is true in docker-compose deploys where the ftw-updater
    // sidecar exposes its Unix socket at the configured SocketPath; native
    // installs and dev runs leave the socket absent, so we stay invisible
    // instead of offering an Update button that can only fail.
    const showBanner =
      !!info &&
      info.update_available &&
      !info.skipped &&
      info.sidecar_ready === true &&
      this._phase === "idle";

    // Toggle :host visibility so the element collapses when it has
    // nothing to say — the wizard layout shouldn't reserve space.
    if (showBanner || this._phase !== "idle") {
      this.classList.remove("hidden");
    } else {
      this.classList.add("hidden");
    }

    return `
      ${showBanner ? this._bannerHTML(info) : ""}
      ${this._phase !== "idle" ? this._overlayHTML() : ""}
    `;
  }

  afterRender() {
    const upd = this.shadowRoot.querySelector('[data-action="update"]');
    if (upd) upd.addEventListener("click", () => this._beginUpdate());
    const dis = this.shadowRoot.querySelector('[data-action="dismiss"]');
    if (dis) dis.addEventListener("click", () => this._dismiss());
    const rel = this.shadowRoot.querySelector('[data-action="reload"]');
    if (rel) rel.addEventListener("click", () => this._reload());
    const cancel = this.shadowRoot.querySelector('[data-action="cancel"]');
    if (cancel) {
      cancel.addEventListener("click", () => {
        this._stopPolling();
        this._phase = "idle";
        this.update();
      });
    }

    // Block ftw-modal's self-close while we're mid-update. The operator
    // uses our explicit Reload / Continue-setup buttons on fail/timeout;
    // they shouldn't silently dismiss a container being recreated.
    const modal = this.shadowRoot.querySelector("ftw-modal");
    if (modal) {
      modal.addEventListener("ftw-modal-close", (e) => {
        if (this._phase === "updating") {
          e.preventDefault();
          return;
        }
        // In failed / timedOut, treat close as "Continue setup".
        this._stopPolling();
        this._phase = "idle";
        this.update();
      });
    }
  }

  _bannerHTML(info) {
    const href = safeHref(info.release_notes_url);
    const notes = href
      ? `<a class="banner-notes" href="${escapeHTML(href)}" target="_blank" rel="noopener">Release notes ↗</a>`
      : "";

    return `
      <div class="banner" part="banner">
        <div class="banner-title">Update available</div>
        <div class="banner-detail">${escapeHTML(info.current || "?")}  →  ${escapeHTML(info.latest || "?")}</div>
        ${notes}
        <div class="banner-actions">
          <button class="btn-primary" data-action="update">Update now</button>
          <button class="btn-skip" data-action="dismiss">Continue anyway</button>
        </div>
      </div>
    `;
  }

  _overlayHTML() {
    const st = this._status || { state: "starting" };
    const busy = this._phase === "updating";
    const failed = this._phase === "failed";
    const timedOut = this._phase === "timedOut";

    let title = "Updating";
    let msg = stateLabel(st.state) + "…";
    let hint = "The page will reload automatically.";
    let actions = "";
    if (failed) {
      title = "Update failed";
      msg = st.message || "Update failed";
      hint = "";
      actions = `
        <div class="overlay-actions" slot="footer">
          <button class="btn-secondary" data-action="cancel">Continue setup</button>
          <button class="btn-primary" data-action="reload">Reload page</button>
        </div>
      `;
    } else if (timedOut) {
      const elapsed = Math.round((Date.now() - this._updateStartedAt) / 1000);
      title = "Taking longer than expected";
      msg = `Still working after ${elapsed}s. You can reload to check, or continue setup and let the update finish in the background.`;
      hint = "";
      actions = `
        <div class="overlay-actions" slot="footer">
          <button class="btn-secondary" data-action="cancel">Continue setup</button>
          <button class="btn-primary" data-action="reload">Reload page</button>
        </div>
      `;
    }

    return `
      <ftw-modal open class="${busy ? "busy" : ""}">
        <span slot="title">${escapeHTML(title)}</span>
        <div class="progress">
          ${busy ? `<span class="spinner" aria-hidden="true"></span>` : ""}
          <p class="msg">${escapeHTML(msg)}</p>
          ${hint ? `<p class="hint">${escapeHTML(hint)}</p>` : ""}
        </div>
        ${actions}
      </ftw-modal>
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

function stateLabel(state) {
  switch (state) {
    case "pulling":    return "Pulling new image";
    case "restarting": return "Applying update";
    case "done":       return "Reloading";
    case "failed":     return "Failed";
    default:           return "Starting update";
  }
}

customElements.define("ftw-update-check", FtwUpdateCheck);
