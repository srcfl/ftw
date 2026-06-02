// <ftw-notif-history> — toolbar bell icon with a "recent failure" dot,
// click opens a modal showing the last N dispatched notifications
// (fetched from /api/notifications/history).
//
// Attributes:
//   poll-ms    — poll interval for the badge count (default 30000).
//   endpoint   — history URL (default "/api/notifications/history").
//   limit      — how many rows to fetch when the modal opens (default 100).
//   fail-hours — how many hours back to count failures for the red dot
//                (default 24).
//
// Drop anywhere in the header/toolbar; keeps its own state and is safe
// to mount alongside other persistent header bits.
import { FtwElement } from "./ftw-element.js";

export class FtwNotifHistory extends FtwElement {
  static styles = `
    :host { display: inline-flex; align-items: center; }
    button.bell {
      appearance: none; background: transparent; border: 0;
      cursor: pointer; color: var(--fg, #e5e5e5);
      padding: 4px 6px; position: relative;
      display: inline-flex; align-items: center; justify-content: center;
      border-radius: 4px;
    }
    button.bell:hover { background: var(--ink-raised, #222); }
    button.bell svg { width: 20px; height: 20px; display: block; }
    .dot {
      position: absolute; top: 2px; right: 2px;
      min-width: 14px; height: 14px; padding: 0 3px;
      border-radius: 10px; background: var(--red-e, #e57373); color: #0a0a0a;
      font-size: 10px; font-weight: 700; line-height: 14px;
      text-align: center;
    }
    .dot.hidden { display: none; }

    ftw-modal table {
      width: 100%; border-collapse: collapse; font-size: 0.8rem;
    }
    ftw-modal th, ftw-modal td {
      text-align: left; padding: 6px 8px; border-bottom: 1px solid var(--line, #333);
      vertical-align: top;
    }
    ftw-modal th {
      position: sticky; top: 0;
      background: var(--ink-raised, #1a1a1a); font-weight: 600;
    }
    ftw-modal tr.sent    td .status-cell { color: var(--green-e, #7dd17d); }
    ftw-modal tr.failed  td .status-cell { color: var(--red-e, #e57373); }
    ftw-modal td.body    { color: var(--text-dim, #aaa); }
    ftw-modal .empty { padding: 24px; text-align: center; color: var(--text-dim, #888); }
    ftw-modal .scroll { max-height: 60vh; overflow: auto; }
    ftw-modal .error-text { color: var(--red-e, #e57373); font-family: monospace; }
    ftw-modal .refresh-btn {
      background: var(--ink-raised, #222); color: var(--fg, #eee);
      border: 1px solid var(--line, #333); border-radius: 4px;
      padding: 4px 10px; cursor: pointer; font: inherit;
    }
    ftw-modal .refresh-btn:hover { background: var(--ink-raised-hover, #2a2a2a); }
  `;

  constructor() {
    super();
    this._failCount = 0;
    this._rows = [];
    this._loading = false;
    this._pollTimer = null;
  }

  connectedCallback() {
    super.connectedCallback();
    this._refreshBadge();
    this._startPolling();
  }

  disconnectedCallback() {
    if (this._pollTimer) { clearInterval(this._pollTimer); this._pollTimer = null; }
  }

  _startPolling() {
    if (this._pollTimer) { clearInterval(this._pollTimer); this._pollTimer = null; }
    var n = parseInt(this.getAttribute("poll-ms") || "30000", 10);
    if (n > 0) this._pollTimer = setInterval(() => this._refreshBadge(), n);
  }

  _endpoint() { return this.getAttribute("endpoint") || "/api/notifications/history"; }
  _limit()    { return parseInt(this.getAttribute("limit") || "100", 10); }
  _failHours(){ return parseInt(this.getAttribute("fail-hours") || "24", 10); }

  async _fetchRows(limit) {
    try {
      var r = await fetch(this._endpoint() + "?limit=" + encodeURIComponent(limit || this._limit()));
      if (!r.ok) return [];
      var d = await r.json();
      return Array.isArray(d) ? d : [];
    } catch (e) { return []; }
  }

  async _refreshBadge() {
    // Only need the recent window to count failures — hit a small limit
    // so the polling call is cheap.
    var rows = await this._fetchRows(50);
    var windowMs = this._failHours() * 3600 * 1000;
    var cutoff = Date.now() - windowMs;
    var fails = rows.filter(r => r.status === "failed" && (r.ts_ms || 0) >= cutoff).length;
    this._failCount = fails;
    this._updateDot();
  }

  _updateDot() {
    var dot = this.shadowRoot.querySelector(".dot");
    if (!dot) {
      this.update();
      return;
    }
    if (this._failCount > 0) {
      dot.textContent = this._failCount > 99 ? "99+" : String(this._failCount);
      dot.classList.remove("hidden");
    } else {
      dot.textContent = "";
      dot.classList.add("hidden");
    }
  }

  async _open() {
    this._loading = true;
    this.update();
    this._rows = await this._fetchRows();
    this._loading = false;
    this.update();
    var m = this.shadowRoot.querySelector("ftw-modal");
    if (m && typeof m.open === "function") m.open();
    else if (m) m.setAttribute("open", "");
  }

  afterRender() {
    var btn = this.shadowRoot.querySelector("button.bell");
    if (btn) btn.addEventListener("click", () => this._open());
    var refresh = this.shadowRoot.querySelector(".refresh-btn");
    if (refresh) refresh.addEventListener("click", async () => {
      this._loading = true;
      this.update();
      this._rows = await this._fetchRows();
      this._loading = false;
      this._refreshBadge();
      this.update();
      var m = this.shadowRoot.querySelector("ftw-modal");
      if (m) m.setAttribute("open", ""); // keep open after re-render
    });
  }

  _row(r) {
    var ts = new Date(r.ts_ms || 0);
    var tsText = isNaN(ts.getTime()) ? "?" : ts.toLocaleString();
    var esc = s => (s == null ? "" : String(s)).replace(/[&<>]/g, c =>
      c === "&" ? "&amp;" : c === "<" ? "&lt;" : "&gt;");
    var statusCell = r.status === "failed"
      ? '<span class="status-cell">✗ failed</span><div class="error-text">' + esc(r.error) + '</div>'
      : '<span class="status-cell">✓ sent</span>';
    return '<tr class="' + esc(r.status || "") + '">' +
      '<td>' + esc(tsText) + '</td>' +
      '<td>' + esc(r.event_type) + (r.driver ? ' · ' + esc(r.driver) : '') + '</td>' +
      '<td><strong>' + esc(r.title) + '</strong></td>' +
      '<td class="body">' + esc(r.body) + '</td>' +
      '<td>' + statusCell + '</td>' +
      '</tr>';
  }

  render() {
    var bell = `
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
           stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
        <path d="M6 8a6 6 0 0 1 12 0c0 7 3 9 3 9H3s3-2 3-9"/>
        <path d="M10.3 21a1.94 1.94 0 0 0 3.4 0"/>
      </svg>
    `;
    var dotHtml = this._failCount > 0
      ? '<span class="dot">' + (this._failCount > 99 ? "99+" : this._failCount) + '</span>'
      : '<span class="dot hidden"></span>';

    var tableBody;
    if (this._loading) {
      tableBody = '<div class="empty">Loading…</div>';
    } else if (!this._rows || this._rows.length === 0) {
      tableBody = '<div class="empty">No notifications yet. Configure + enable rules in Settings → Notifications.</div>';
    } else {
      tableBody =
        '<div class="scroll"><table>' +
        '<thead><tr>' +
          '<th>When</th><th>Event</th><th>Title</th><th>Body</th><th>Status</th>' +
        '</tr></thead>' +
        '<tbody>' + this._rows.map(r => this._row(r)).join("") + '</tbody>' +
        '</table></div>';
    }

    return `
      <button class="bell" type="button" title="Notification history" aria-label="Notification history">
        ${bell}${dotHtml}
      </button>
      <ftw-modal style="--ftw-modal-max-width:880px">
        <span slot="title">Notification history</span>
        ${tableBody}
        <div slot="footer">
          <button type="button" class="refresh-btn">Refresh</button>
        </div>
      </ftw-modal>
    `;
  }
}

customElements.define("ftw-notif-history", FtwNotifHistory);
