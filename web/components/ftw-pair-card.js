// <ftw-pair-card> — surfaces an active pair session on the dashboard, and
// lets the owner start a new session without touching the CLI.
//
// When no session is active (GET /api/pair/status → 404) the card renders a
// start form (intent textarea + TTL select + Start button). POST /api/pair/start
// spawns the sidecar; three fast 1 s polls flip the card to active-mode as
// soon as the sidecar registers itself via POST /api/pair/status.
//
// When a session is active the card renders the subetha pair code (with a
// Copy button), intent, TTL countdown, tool counter, and an Abort button.

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
    .hint {
      margin: 6px 0 0;
      font-size: 0.7rem;
      color: var(--ink-raised);
      font-family: var(--sans, var(--mono));
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
    const aiPrompt = this._aiPrompt();

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
        <section class="friend-message">
          <span class="eyebrow">Claude Code prompt</span>
          <pre class="message" id="ai-prompt-pre">${escapeHTML(aiPrompt)}</pre>
          <button class="copy-msg" id="copy-ai-prompt-btn">Copy</button>
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

    const copyAiBtn = this.shadowRoot.getElementById("copy-ai-prompt-btn");
    if (copyAiBtn) {
      copyAiBtn.addEventListener("click", () => this._copyAiPrompt(copyAiBtn));
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

  async _copyAiPrompt(btn) {
    const ok = await copyToClipboard(this._aiPrompt());
    if (ok) {
      const original = btn.textContent;
      btn.textContent = "Copied!";
      setTimeout(() => { btn.textContent = original; }, 1500);
    }
  }

  // _aiPrompt MUST stay in sync with buildPrompt() in
  // go/cmd/ftw-connect/main.go — that's what ftw-connect auto-copies to the
  // friend's clipboard. The dashboard renders the same text so the owner can
  // paste it manually if clipboard sync fails on the friend side.
  _aiPrompt() {
    return `You are connected to a live forty-two-watts (42W) instance over the MCP server \`ftw-remote\`.

You're helping the owner remotely. The owner is *not* expected to know git or GitHub — **you** open the PR at the end, from your own machine, not theirs. The owner's role here is to share their site with you and accept your help; you handle the development.

## First, orient yourself

Run these in order on your first turn:

1. \`ftw_api\` with \`method: GET, path: /api/pair/status\` — reads the owner's stated intent for this session and the time remaining.
2. \`ftw_api\` with \`method: GET, path: /api/status\` — shows the running state of the instance (drivers, mode, grid/PV/battery readings).
3. \`read_file\` at \`/app/docs/api.md\` (or wherever the repo is mounted — try \`list_directory\` from \`/app\` first) if you need a catalog of HTTP endpoints.

Tell the owner in chat what you found so they can confirm the plan before you start making changes.

## Available MCP tools (17 — these run *on the owner's machine* through the tunnel)

- \`ftw_api(method, path, body?)\` — proxy to the running 42W HTTP API (see docs/api.md)
- \`read_file\` / \`write_file\` / \`list_directory\` — scoped to the owner's repo, state dir, and /tmp
- \`run_command(cmd, workdir)\` — shell on the owner's machine, same scope, 30s default timeout
- \`restart_main_service\` / \`tail_service_logs\` — restart the owner's service, read recent logs
- \`network_scan\` / \`http_probe\` / \`modbus_probe\` / \`modbus_write\` / \`mqtt_observe\` / \`pcap_capture\` — LAN-level introspection from the owner's machine
- \`deploy_driver(name, lua_source, config)\` — write a Lua driver, update config.yaml, wait for reload, verify it ticks against the owner's hardware
- \`session_log\` / \`session_remaining\` / \`session_end\` — session controls

You also have your *own* local tools (Read/Write/Edit/Bash on your local filesystem) — those are how you'll prepare and submit the PR.

## When the work is done — opening the PR

The driver source lives on the owner's machine after you \`write_file\` it there. To turn that into a PR from your own machine:

1. **Snapshot the final state.** Call \`read_file\` on every file you modified on the owner's machine, so you have the canonical text in this conversation. Also call \`session_log\` once to get the audit-log markdown.
2. **Clone the repo locally** if you haven't already: \`git clone https://github.com/frahlg/forty-two-watts.git /tmp/ftw-work\` (use your local Bash tool, not \`run_command\`).
3. **Apply the changes** to that local clone using your local Write tool — drop the driver file into \`drivers/\`, edit \`config.yaml\` to add the driver entry, etc. Match what's on the owner's machine.
4. **Open the PR** with \`gh pr create\` against \`master\`, picking the \`pair-session.md\` template and pasting the session-log markdown into the *Pair-session report* section. Use a \`feat(driver): ...\` style title.
5. **Tell the owner** in chat: link to the PR, what was changed, what you'd like them to test, anything unexpected they should know.
6. **Call \`session_end\`** to close the tunnel. The owner's sidecar exits.

## Boundaries

- Trust level is "ssh-equivalent for the duration of this session". Be respectful of the owner's site.
- Modbus writes and \`deploy_driver\` calls touch real hardware. Confirm with the owner in chat before doing anything that could move energy.
- Everything you do is recorded; the owner sees the audit log in the PR you open.
`;
  }

  _friendMessage(remaining) {
    const code = this._state ? this._state.code : "";
    return `I need help with my home energy system. I've started a pair
session — please join with this code:

  ${code}

One-time setup (Mac/Linux):
  curl -fsSL https://raw.githubusercontent.com/frahlg/forty-two-watts/master/scripts/install-ftw-connect.sh | bash

Then run:
  ftw-connect ${code}

It opens an end-to-end-encrypted tunnel and prints a local URL.
It also copies a ready-to-paste agent prompt to your clipboard —
paste it into Claude Code, Codex, or whatever agent you use, and
we're connected. No config files touched on your side.

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
