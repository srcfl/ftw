// <ftw-pair-card> — surfaces an active pair session on the dashboard, and
// lets the owner start a new session without touching the CLI.
//
// When no session is active (GET /api/pair/status → 404) the card renders a
// start form (intent textarea + TTL select + Start button). POST /api/pair/start
// spawns the sidecar; three fast 1 s polls flip the card to active-mode as
// soon as the sidecar registers itself via POST /api/pair/status.
//
// When a session is active the card renders the wormhole code (with a Copy
// button), intent, TTL countdown, tool counter, and an Abort button.

import { FtwElement } from "./ftw-element.js";

const POLL_MS = 5000;
const FAST_POLL_MS = 1000;
const FAST_POLL_ROUNDS = 3;

class FtwPairCard extends FtwElement {
  static styles = `
    :host { display: block; }

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
    .code-row {
      display: flex;
      align-items: baseline;
      gap: 10px;
      margin-bottom: 8px;
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
    button.copy {
      background: transparent;
      color: var(--fg);
      border: 1px solid var(--line);
      padding: 2px 8px;
      font-family: var(--mono);
      cursor: pointer;
      font-size: 10px;
      letter-spacing: 0.1em;
      text-transform: uppercase;
    }
    button.copy:hover {
      border-color: var(--accent-e);
      color: var(--accent-e);
    }
    /* Start form */
    .start-form {
      display: flex;
      flex-direction: column;
      gap: 10px;
    }
    .field label {
      display: block;
      font-family: var(--mono);
      font-size: 10px;
      letter-spacing: 0.18em;
      text-transform: uppercase;
      color: var(--ink-raised2, var(--fg-dim, var(--fg)));
      opacity: 0.6;
      margin-bottom: 4px;
    }
    .field textarea,
    .field select {
      width: 100%;
      box-sizing: border-box;
      background: var(--bg, #111);
      color: var(--fg);
      border: 1px solid var(--line);
      font-family: var(--sans);
      font-size: 0.85rem;
      padding: 6px 8px;
    }
    .field textarea {
      resize: vertical;
      min-height: 52px;
    }
    .field select {
      appearance: none;
      -webkit-appearance: none;
      cursor: pointer;
    }
    button.start {
      align-self: flex-start;
      background: var(--accent-e);
      color: #0a0a0a;
      border: 0;
      padding: 6px 14px;
      font-family: var(--mono);
      cursor: pointer;
      font-size: 11px;
      letter-spacing: 0.1em;
      text-transform: uppercase;
    }
    button.start:hover {
      opacity: 0.85;
    }
    button.start:disabled {
      opacity: 0.4;
      cursor: default;
    }
    /* Friend-message block (active session only) */
    .friend-message {
      margin: 12px 0;
    }
    .friend-message .eyebrow {
      display: block;
      margin-bottom: 6px;
    }
    .message {
      font-family: var(--mono);
      font-size: 0.72rem;
      line-height: 1.55;
      background: var(--surface, var(--bg, #111));
      border: 1px solid var(--line);
      padding: 10px 12px;
      margin: 0 0 8px;
      white-space: pre;
      overflow-x: auto;
      color: var(--fg);
    }
    button.copy-msg {
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
    button.copy-msg:hover {
      opacity: 0.85;
    }
  `;

  constructor() {
    super();
    this._state = null;
    this._tick = null;
    this._fastRounds = 0;
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

  // _startFastPolls switches to 1 s polling for FAST_POLL_ROUNDS iterations,
  // then reverts to the normal cadence. Call immediately after POST /api/pair/start
  // so the card flips to active-mode as soon as the sidecar registers itself.
  _startFastPolls() {
    this._fastRounds = FAST_POLL_ROUNDS;
    clearInterval(this._tick);
    const fast = setInterval(() => {
      this._refresh();
      this._fastRounds--;
      if (this._fastRounds <= 0) {
        clearInterval(fast);
        this._tick = setInterval(() => this._refresh(), POLL_MS);
      }
    }, FAST_POLL_MS);
  }

  async _start() {
    const root = this.shadowRoot;
    const intentEl = root.getElementById("intent-input");
    const ttlEl = root.getElementById("ttl-select");
    const btn = root.getElementById("start-btn");
    if (!intentEl || !ttlEl || !btn) return;

    btn.disabled = true;
    try {
      const resp = await fetch("/api/pair/start", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ intent: intentEl.value.trim(), ttl: ttlEl.value }),
      });
      if (!resp.ok) {
        const txt = await resp.text();
        alert("Failed to start pair session: " + txt);
        btn.disabled = false;
        return;
      }
    } catch (e) {
      alert("Failed to start pair session: " + e.message);
      btn.disabled = false;
      return;
    }
    this._startFastPolls();
  }

  async _abort() {
    if (!confirm("End the pair session now?")) return;
    await fetch("/api/pair/abort", { method: "POST" });
    this._state = null;
    this.update();
  }

  async _copyCode() {
    if (!this._state) return;
    const btn = this.shadowRoot.getElementById("copy-btn");
    const ok = await copyToClipboard(this._state.code);
    if (ok && btn) {
      const original = btn.textContent;
      btn.textContent = "Copied!";
      setTimeout(() => { btn.textContent = original; }, 1500);
    }
  }

  render() {
    if (!this._state) {
      // No active session — show the start form.
      return `
        <div class="pair-card">
          <header>
            <span class="eyebrow">Pair session</span>
          </header>
          <div class="start-form">
            <div class="field">
              <label for="intent-input">Intent (optional)</label>
              <textarea id="intent-input" rows="2" placeholder="e.g. help me write a GoodWe XS driver"></textarea>
            </div>
            <div class="field">
              <label for="ttl-select">Session length</label>
              <select id="ttl-select">
                <option value="1h">1 hour</option>
                <option value="4h" selected>4 hours</option>
                <option value="12h">12 hours</option>
              </select>
            </div>
            <button class="start" id="start-btn">Start pair session</button>
          </div>
        </div>
      `;
    }

    // Active session — show code, intent, countdown, tools, abort + copy.
    const remaining = this._computeRemaining();
    const lastTools = (this._state.last_tools || [])
      .map((t) => escapeHTML(t))
      .join(", ") || "—";

    const friendMsg = this._friendMessage(remaining);

    return `
      <div class="pair-card">
        <header>
          <span class="eyebrow">Pair session active</span>
          <button class="abort" id="abort-btn">Abort</button>
        </header>
        <div class="code-row">
          <p class="code">${escapeHTML(this._state.code)}</p>
          <button class="copy" id="copy-btn">Copy</button>
        </div>
        <p class="intent">${escapeHTML(this._state.intent || "(no intent set)")}</p>
        <section class="friend-message">
          <span class="eyebrow">SHARE WITH YOUR FRIEND</span>
          <pre class="message" id="friend-msg-pre">${escapeHTML(friendMsg)}</pre>
          <button class="copy-msg" id="copy-msg-btn">Copy this message</button>
        </section>
        <dl>
          <dt>TTL</dt><dd>${escapeHTML(remaining)}</dd>
          <dt>Tool calls</dt><dd>${this._state.tool_count ?? 0}</dd>
          <dt>Last tools</dt><dd>${lastTools}</dd>
        </dl>
      </div>
    `;
  }

  afterRender() {
    const abortBtn = this.shadowRoot.getElementById("abort-btn");
    if (abortBtn) abortBtn.addEventListener("click", () => this._abort());

    const copyBtn = this.shadowRoot.getElementById("copy-btn");
    if (copyBtn) copyBtn.addEventListener("click", () => this._copyCode());

    const startBtn = this.shadowRoot.getElementById("start-btn");
    if (startBtn) startBtn.addEventListener("click", () => this._start());

    const copyMsgBtn = this.shadowRoot.getElementById("copy-msg-btn");
    if (copyMsgBtn) {
      copyMsgBtn.addEventListener("click", () => this._copyFriendMessage(copyMsgBtn));
    }
  }

  async _copyFriendMessage(btn) {
    if (!this._state) return;
    const remaining = this._computeRemaining();
    const msg = this._friendMessage(remaining);
    const ok = await copyToClipboard(msg);
    if (ok) {
      const original = btn.textContent;
      btn.textContent = "Copied!";
      setTimeout(() => { btn.textContent = original; }, 1500);
    }
  }

  _friendMessage(remaining) {
    const code = this._state ? this._state.code : "";
    return `Send this in Signal/SMS/Slack to your friend:

I need help with my home energy system. I've started a pair
session — please join with this code:

  ${code}

One-time setup on your Mac/Linux:
  brew install uv     (skip if already installed)
  uv tool install fowl
  go install github.com/frahlg/forty-two-watts/go/cmd/ftw-connect@latest

Then run:
  ftw-connect ${code}

It'll open a tunnel, register an MCP server with your Claude Code,
and copy a context prompt to your clipboard. Paste it into Claude
Code and we're connected.

Session expires in ${remaining} or when I click Abort.`;
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

// copyToClipboard — works in both secure (HTTPS/localhost) and insecure
// (plain HTTP LAN) contexts. Tries the modern Clipboard API first; falls
// back to the legacy textarea+execCommand path when the secure-context
// gate blocks the primary path.
async function copyToClipboard(text) {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch (_) {
    // fall through to legacy path
  }
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.style.position = "fixed";
  ta.style.top = "-9999px";
  document.body.appendChild(ta);
  ta.focus();
  ta.select();
  try {
    return document.execCommand("copy");
  } finally {
    document.body.removeChild(ta);
  }
}

customElements.define("ftw-pair-card", FtwPairCard);
