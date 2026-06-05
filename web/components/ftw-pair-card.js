// <ftw-pair-card> — surfaces an active pair session on the dashboard.
//
// v2 of the friend onboarding (Phase 1+2 of relay-as-tunnel):
//   - shows the public URL the operator copies + sends to the friend
//   - shows the 4-digit voice-channel approval code prominently
//   - exposes an Allow form that POSTs the matching code to the relay
//     once the friend reads it back on voice
//   - replaces the misleading "0 clients connected" with a real
//     last-activity presence indicator backed by the relay's
//     /tunnel/sessions/<token>/info endpoint
//
// All pure render decisions live in ./ftw-pair-card-render.js so the
// state-machine + golden message can be unit-tested in Node without
// a DOM or Web Components polyfill — see the .test.mjs file next to
// the helpers.

import { FtwElement } from "./ftw-element.js";
import { ownerFetch } from "./owner-fetch.js";
import {
  POLL_MS,
  FAST_POLL_MS,
  FAST_POLL_ROUNDS,
  escapeHTML,
  computeRemaining,
  derivePresence,
  friendMessage,
} from "./ftw-pair-card-render.js";

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
    .hint {
      margin: 6px 0 0;
      font-size: 0.7rem;
      color: var(--ink-raised);
      font-family: var(--sans, var(--mono));
    }
    .muted {
      color: var(--ink-raised2, var(--fg-dim, var(--fg)));
      opacity: 0.6;
    }
    .live {
      color: var(--accent-e);
    }

    /* ---- Approval form + presence indicator (v2) ---- */
    .approval {
      margin: 12px 0;
      padding: 10px 12px;
      border: 1px solid var(--accent-e);
      background: var(--surface, var(--bg, #111));
    }
    .approval .big-code {
      font-family: var(--mono);
      font-size: 2.4rem;
      letter-spacing: 0.4em;
      text-align: center;
      color: var(--accent-e);
      margin: 10px 0;
      font-weight: 700;
    }
    .approval p {
      margin: 4px 0;
      font-size: 0.85rem;
    }
    .approval-row {
      display: flex;
      gap: 8px;
      align-items: center;
      margin-top: 8px;
    }
    .approval-row input {
      font-family: var(--mono);
      font-size: 1.2rem;
      letter-spacing: 0.2em;
      text-align: center;
      width: 5.5em;
      padding: 4px 6px;
      background: var(--bg, #111);
      color: var(--fg);
      border: 1px solid var(--line);
    }
    .approval-row button {
      background: var(--accent-e);
      color: #0a0a0a;
      border: 0;
      padding: 6px 14px;
      font-family: var(--mono);
      font-size: 11px;
      letter-spacing: 0.1em;
      text-transform: uppercase;
      cursor: pointer;
    }
    .approval-row button:disabled {
      opacity: 0.4;
      cursor: default;
    }
    .approval-msg {
      font-size: 0.78rem;
      margin-top: 6px;
      font-family: var(--mono);
    }
    .approval-msg.err { color: #c66; }
    .approval-msg.ok  { color: var(--accent-e); }

    /* presence dot + label */
    .presence {
      display: inline-flex;
      gap: 6px;
      align-items: center;
    }
    .dot {
      width: 8px;
      height: 8px;
      border-radius: 50%;
      display: inline-block;
      background: var(--fg);
    }
    .dot.fresh  { background: var(--accent-e); box-shadow: 0 0 6px var(--accent-e); }
    .dot.recent { background: var(--accent-e); opacity: 0.7; }
    .dot.idle   { background: var(--ink-raised2, #888); opacity: 0.6; }
    .dot.pending{ background: var(--accent-e); opacity: 0.5; }
    .dot.dead   { background: #c66; }

    /* URL block — long URL with copy button */
    .url-block {
      margin: 10px 0;
      padding: 8px 10px;
      background: var(--surface, var(--bg, #111));
      border: 1px solid var(--line);
      display: flex;
      align-items: center;
      gap: 8px;
    }
    .url-block .url {
      font-family: var(--mono);
      font-size: 0.78rem;
      flex: 1;
      overflow-x: auto;
      white-space: nowrap;
      color: var(--fg);
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
      const resp = await ownerFetch("/api/pair/start", {
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
    await ownerFetch("/api/pair/abort", { method: "POST" });
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

    // Active session — show URL, approval form (if pending), token,
    // intent, countdown, presence, tools, abort + copy.
    const remaining = computeRemaining(this._state);
    const lastTools = (this._state.last_tools || [])
      .map((t) => escapeHTML(t))
      .join(", ") || "—";

    const friendMsg = friendMessage(this._state);
    const presence = derivePresence(this._state);
    const url = this._state.pair_url || "";
    const approvalCode = this._state.approval_code || "";

    return `
      <div class="pair-card">
        <header>
          <span class="eyebrow">Pair session active</span>
          <button class="abort" id="abort-btn">Abort</button>
        </header>

        ${url ? `
        <div class="url-block">
          <span class="url" id="url-span">${escapeHTML(url)}</span>
          <button class="copy" id="copy-url-btn">Copy URL</button>
        </div>
        ${approvalCode ? `
        <div class="approval">
          <p>Share <strong>both</strong> with your friend — URL via Signal/SMS, code on the same message. They'll open the URL and type the code to activate.</p>
          <div class="big-code">${escapeHTML(approvalCode)}</div>
          <div class="approval-row">
            <button id="copy-code-btn">Copy code</button>
            <button id="copy-bundle-btn">Copy URL + code</button>
          </div>
        </div>
        ` : ""}
        ` : ""}

        <div class="code-row">
          <p class="code">${escapeHTML(this._state.code)}</p>
          <button class="copy" id="copy-btn">Copy token</button>
        </div>
        <p class="intent">${escapeHTML(this._state.intent || "(no intent set)")}</p>

        <section class="friend-message">
          <span class="eyebrow">SHARE WITH YOUR FRIEND</span>
          <pre class="message" id="friend-msg-pre">${escapeHTML(friendMsg)}</pre>
          <button class="copy-msg" id="copy-msg-btn">Copy this message</button>
        </section>

        <dl>
          <dt>TTL</dt><dd>${escapeHTML(remaining)}</dd>
          <dt>Friend</dt><dd>
            <span class="presence">
              <span class="dot ${escapeHTML(presence.class)}"></span>
              <span>${escapeHTML(presence.label)}</span>
            </span>
          </dd>
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

    const copyUrlBtn = this.shadowRoot.getElementById("copy-url-btn");
    if (copyUrlBtn) {
      copyUrlBtn.addEventListener("click", () => this._copyUrl(copyUrlBtn));
    }

    const copyCodeBtn = this.shadowRoot.getElementById("copy-code-btn");
    if (copyCodeBtn) {
      copyCodeBtn.addEventListener("click", () => this._copyText(copyCodeBtn, this._state?.approval_code || ""));
    }
    const copyBundleBtn = this.shadowRoot.getElementById("copy-bundle-btn");
    if (copyBundleBtn) {
      copyBundleBtn.addEventListener("click", () => {
        const bundle = (this._state?.pair_url || "") + "\nCode: " + (this._state?.approval_code || "");
        this._copyText(copyBundleBtn, bundle);
      });
    }
  }

  async _copyText(btn, text) {
    if (!text) return;
    const ok = await copyToClipboard(text);
    if (ok) {
      const original = btn.textContent;
      btn.textContent = "Copied!";
      setTimeout(() => { btn.textContent = original; }, 1500);
    }
  }

  async _copyFriendMessage(btn) {
    if (!this._state) return;
    const msg = friendMessage(this._state);
    const ok = await copyToClipboard(msg);
    if (ok) {
      const original = btn.textContent;
      btn.textContent = "Copied!";
      setTimeout(() => { btn.textContent = original; }, 1500);
    }
  }

  async _copyUrl(btn) {
    if (!this._state || !this._state.pair_url) return;
    const ok = await copyToClipboard(this._state.pair_url);
    if (ok) {
      const original = btn.textContent;
      btn.textContent = "Copied!";
      setTimeout(() => { btn.textContent = original; }, 1500);
    }
  }

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
