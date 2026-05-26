// <ftw-battery-control> — operator-pinned battery setpoint modal.
//
// Wraps a <ftw-modal> with a direction segmented control (charge /
// discharge / idle), power input, duration chips (5 / 15 / 30 min),
// active-hold banner, and Stop / Install buttons. Talks to the
// /api/battery/manual_hold endpoints; safety clamps (SoC, per-driver
// caps, slew, fuse guard) are enforced server-side regardless of what
// is requested here.
//
// Usage:
//
//   <ftw-battery-control id="battery-control"></ftw-battery-control>
//
//   const el = document.getElementById("battery-control");
//   el.open();   // opens the modal and starts polling /api/battery/manual_hold
//
// Opening from another component (e.g. battery-planet click) is the
// only entry point — there's no auto-open behavior.

import { FtwElement } from "./ftw-element.js";

class FtwBatteryControl extends FtwElement {
  static styles = `
    :host { display: contents; font-family: var(--sans); color: var(--fg); }

    .row {
      display: flex;
      flex-direction: column;
      gap: 8px;
      margin-bottom: 16px;
    }
    .row.hidden { display: none; }

    .label {
      font-family: var(--mono);
      font-size: 0.7rem;
      font-weight: 500;
      text-transform: uppercase;
      letter-spacing: 0.18em;
      color: var(--fg-muted);
    }

    .segmented {
      display: flex;
      background: var(--ink-sunken);
      border: 1px solid var(--line);
      border-radius: 8px;
      padding: 2px;
      gap: 2px;
    }
    .seg-btn {
      flex: 1;
      border: 0;
      background: transparent;
      color: var(--fg-dim);
      padding: 9px 10px;
      border-radius: 6px;
      font-family: var(--sans);
      font-size: 14px;
      font-weight: 500;
      cursor: pointer;
      transition: background 120ms, color 120ms;
    }
    .seg-btn:hover { color: var(--fg); }
    .seg-btn.active {
      background: var(--accent-e);
      color: #0a0a0a;
    }

    .power-wrap {
      display: flex;
      align-items: center;
      gap: 10px;
    }
    .power-wrap input {
      flex: 1;
      background: var(--ink-raised);
      border: 1px solid var(--line);
      border-radius: 8px;
      padding: 10px 12px;
      color: var(--fg);
      font-family: var(--mono);
      font-size: 0.95rem;
      font-variant-numeric: tabular-nums;
    }
    .power-wrap input:focus {
      outline: none;
      border-color: var(--accent-e);
    }
    .unit {
      font-family: var(--mono);
      color: var(--fg-muted);
      font-size: 0.78rem;
      text-transform: uppercase;
      letter-spacing: 0.18em;
    }

    .chips {
      display: flex;
      gap: 6px;
      flex-wrap: wrap;
    }
    .chip {
      border: 1px solid var(--line);
      background: transparent;
      color: var(--fg-dim);
      padding: 6px 14px;
      border-radius: 999px;
      cursor: pointer;
      font-family: var(--sans);
      font-size: 13px;
      font-weight: 500;
      transition: border-color 120ms, color 120ms;
    }
    .chip:hover {
      color: var(--fg);
      border-color: var(--fg-dim);
    }
    .chip.active {
      background: var(--accent-e);
      color: #0a0a0a;
      border-color: var(--accent-e);
    }

    .active-banner {
      background: color-mix(in srgb, var(--accent-e) 10%, transparent);
      border: 1px solid var(--accent-e);
      border-radius: 8px;
      padding: 12px 14px;
      margin-bottom: 16px;
    }
    .active-banner.hidden { display: none; }
    .active-headline {
      font-family: var(--sans);
      font-weight: 600;
      color: var(--accent-e);
      margin-bottom: 2px;
    }
    .active-detail {
      font-family: var(--mono);
      font-size: 12px;
      color: var(--fg-dim);
      font-variant-numeric: tabular-nums;
    }

    .error {
      background: color-mix(in srgb, var(--red-e) 12%, transparent);
      border: 1px solid var(--red-e);
      color: var(--red-e);
      padding: 8px 12px;
      border-radius: 8px;
      font-size: 13px;
      margin-top: 8px;
    }
    .error.hidden { display: none; }

    /* Footer buttons — primary CTA is the accent, on-accent text is
       near-black per DESIGN.md. The Stop button is a ghost variant
       until a hold is active, then it switches to the red status hue
       to signal a destructive action. */
    .footer-btn {
      flex: 1;
      padding: 11px 18px;
      border-radius: 8px;
      cursor: pointer;
      font-family: var(--sans);
      font-weight: 500;
      font-size: 14px;
      transition: transform 80ms, border-color 120ms, color 120ms, background 120ms;
    }
    .footer-btn:disabled {
      opacity: 0.45;
      cursor: not-allowed;
    }
    .footer-btn[data-variant="install"] {
      flex: 2;
      background: var(--accent-e);
      color: #0a0a0a;
      border: 1px solid var(--accent-e);
    }
    .footer-btn[data-variant="install"]:hover:not(:disabled) {
      transform: translateY(-1px);
    }
    .footer-btn[data-stop] {
      background: transparent;
      color: var(--fg);
      border: 1px solid var(--line);
    }
    .footer-btn[data-stop]:not(:disabled) {
      color: var(--red-e);
      border-color: var(--red-e);
    }
    .footer-btn[data-stop]:hover:not(:disabled) {
      background: color-mix(in srgb, var(--red-e) 12%, transparent);
    }
  `;

  constructor() {
    super();
    this._refreshTimer = null;
    this._formState = { direction: "charge", holdS: 900 };
  }

  connectedCallback() {
    super.connectedCallback();
  }

  disconnectedCallback() {
    if (this._refreshTimer) {
      clearInterval(this._refreshTimer);
      this._refreshTimer = null;
    }
  }

  open() {
    const modal = this.shadowRoot.querySelector("ftw-modal");
    if (!modal) return;
    this._showError("");
    this._selectDirection(this._formState.direction);
    this._selectDuration(this._formState.holdS);
    modal.open();
    this._refresh();
    if (this._refreshTimer) clearInterval(this._refreshTimer);
    this._refreshTimer = setInterval(() => this._refresh(), 3000);
  }

  render() {
    return `
      <ftw-modal style="--ftw-modal-max-width:420px">
        <span slot="title">Battery control</span>

        <div class="active-banner hidden" data-active>
          <div class="active-headline"></div>
          <div class="active-detail"></div>
        </div>

        <div class="row">
          <label class="label">Direction</label>
          <div class="segmented" role="radiogroup" aria-label="Direction">
            <button type="button" class="seg-btn active" data-direction="charge" role="radio" aria-checked="true">Charge</button>
            <button type="button" class="seg-btn" data-direction="discharge" role="radio" aria-checked="false">Discharge</button>
            <button type="button" class="seg-btn" data-direction="idle" role="radio" aria-checked="false">Idle</button>
          </div>
        </div>

        <div class="row" data-power-row>
          <label class="label" for="power-input">Power</label>
          <div class="power-wrap">
            <input data-power-input type="number" min="0" step="100" value="3000" inputmode="numeric"/>
            <span class="unit">W</span>
          </div>
        </div>

        <div class="row">
          <label class="label">Duration</label>
          <div class="chips">
            <button type="button" class="chip" data-hold="300">5 min</button>
            <button type="button" class="chip active" data-hold="900">15 min</button>
            <button type="button" class="chip" data-hold="1800">30 min</button>
          </div>
        </div>

        <div class="error hidden" data-error></div>

        <div slot="footer" style="display:flex;gap:0.5rem;width:100%">
          <button type="button" class="footer-btn" data-stop disabled>Stop</button>
          <button type="button" class="footer-btn" data-install data-variant="install">Install hold</button>
        </div>
      </ftw-modal>
    `;
  }

  afterRender() {
    const root = this.shadowRoot;
    const modal = root.querySelector("ftw-modal");
    const segBtns = root.querySelectorAll(".seg-btn");
    const chips = root.querySelectorAll(".chip");
    const installBtn = root.querySelector("[data-install]");
    const stopBtn = root.querySelector("[data-stop]");

    segBtns.forEach((b) => {
      b.addEventListener("click", () => this._selectDirection(b.dataset.direction));
    });
    chips.forEach((c) => {
      c.addEventListener("click", () => this._selectDuration(Number(c.dataset.hold)));
    });

    installBtn.addEventListener("click", () => this._install());
    stopBtn.addEventListener("click", () => this._stop());

    modal.addEventListener("ftw-modal-close", () => {
      if (this._refreshTimer) {
        clearInterval(this._refreshTimer);
        this._refreshTimer = null;
      }
    });
  }

  _selectDirection(dir) {
    this._formState.direction = dir;
    const root = this.shadowRoot;
    root.querySelectorAll(".seg-btn").forEach((b) => {
      const on = b.dataset.direction === dir;
      b.classList.toggle("active", on);
      b.setAttribute("aria-checked", on ? "true" : "false");
    });
    // Idle has no power magnitude — hide the row to keep the form
    // honest. Reappears the moment the user picks charge or discharge.
    const powerRow = root.querySelector("[data-power-row]");
    if (powerRow) powerRow.classList.toggle("hidden", dir === "idle");
  }

  _selectDuration(holdS) {
    this._formState.holdS = holdS;
    this.shadowRoot.querySelectorAll(".chip").forEach((c) => {
      c.classList.toggle("active", Number(c.dataset.hold) === holdS);
    });
  }

  _showError(msg) {
    const errBox = this.shadowRoot.querySelector("[data-error]");
    if (!errBox) return;
    if (!msg) {
      errBox.classList.add("hidden");
      errBox.textContent = "";
      return;
    }
    errBox.textContent = msg;
    errBox.classList.remove("hidden");
  }

  _renderActive(d) {
    const root = this.shadowRoot;
    const banner = root.querySelector("[data-active]");
    const stopBtn = root.querySelector("[data-stop]");
    if (!banner || !stopBtn) return;
    if (!d || !d.active) {
      banner.classList.add("hidden");
      stopBtn.disabled = true;
      return;
    }
    const dir = d.direction || "idle";
    const headline =
      dir === "charge"    ? "Charging at " + (d.power_w || 0) + " W" :
      dir === "discharge" ? "Discharging at " + (d.power_w || 0) + " W" :
                            "Holding idle";
    let remaining = "";
    if (d.expires_at_ms) {
      const ms = d.expires_at_ms - Date.now();
      if (ms > 0) {
        const totalSec = Math.round(ms / 1000);
        const min = Math.floor(totalSec / 60);
        const sec = totalSec % 60;
        remaining = "Expires in " + (min > 0 ? min + " min " : "") + sec + " s";
      } else {
        remaining = "Expired";
      }
    }
    banner.querySelector(".active-headline").textContent = headline;
    banner.querySelector(".active-detail").textContent = remaining;
    banner.classList.remove("hidden");
    stopBtn.disabled = false;
  }

  _refresh() {
    fetch("/api/battery/manual_hold")
      .then((r) => r.json())
      .then((d) => this._renderActive(d))
      .catch(() => { /* network blip — leave previous state */ });
  }

  _install() {
    this._showError("");
    const dir = this._formState.direction;
    const holdS = this._formState.holdS;
    const installBtn = this.shadowRoot.querySelector("[data-install]");
    let powerW = 0;
    if (dir !== "idle") {
      const input = this.shadowRoot.querySelector("[data-power-input]");
      powerW = Number(input.value);
      if (!isFinite(powerW) || powerW < 0) {
        this._showError("Power must be a non-negative number");
        return;
      }
    }
    installBtn.disabled = true;
    fetch("/api/battery/manual_hold", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ direction: dir, power_w: powerW, hold_s: holdS }),
    })
      .then((r) => {
        if (!r.ok) {
          return r.json().then((j) => {
            throw new Error((j && j.error) || ("HTTP " + r.status));
          });
        }
        return r.json();
      })
      .then((d) => this._renderActive(d))
      .catch((err) => this._showError(err.message || "Failed to install hold"))
      .finally(() => { installBtn.disabled = false; });
  }

  _stop() {
    this._showError("");
    const stopBtn = this.shadowRoot.querySelector("[data-stop]");
    stopBtn.disabled = true;
    fetch("/api/battery/manual_hold", { method: "DELETE" })
      .then((r) => {
        if (!r.ok) throw new Error("HTTP " + r.status);
        return r.json();
      })
      .then((d) => this._renderActive(d))
      .catch((err) => {
        this._showError(err.message || "Failed to clear hold");
        stopBtn.disabled = false;
      });
  }
}

customElements.define("ftw-battery-control", FtwBatteryControl);
