// <ftw-pv-control> — operator-pinned PV curtail modal.
//
// Sibling of <ftw-battery-control>. Pins a PV active-power-limit
// override for a bounded duration, primarily so an operator can verify
// that a fresh driver implementation (e.g. SolarEdge curtail) actually
// reaches the inverter — without waiting for the MPC to organically
// trigger a negative-price slot.
//
// Talks to /api/pv/manual_hold (POST/GET/DELETE). The backend resolves
// limit_pct → watts against the driver's configured nominal_w
// (rated inverter AC output), falling back to live |PV| only when
// driver scopes that advertise pv-curtail in the catalog.
//
// Usage:
//
//   <ftw-pv-control id="pv-control"></ftw-pv-control>
//
//   const el = document.getElementById("pv-control");
//   el.open();              // site-aggregate hold
//   el.open("solaredge");   // scope to one driver

import { FtwElement } from "./ftw-element.js";
import { ownerFetch } from "./owner-fetch.js";

class FtwPvControl extends FtwElement {
  static styles = `
    :host { display: contents; font-family: var(--sans); color: var(--fg); }

    .row { display: flex; flex-direction: column; gap: 8px; margin-bottom: 16px; }
    .row.hidden { display: none; }

    .label {
      font-family: var(--mono);
      font-size: 0.7rem;
      font-weight: 500;
      text-transform: uppercase;
      letter-spacing: 0.18em;
      color: var(--fg-muted);
    }

    select, .power-wrap input {
      background: var(--ink-raised);
      border: 1px solid var(--line);
      border-radius: 8px;
      padding: 10px 12px;
      color: var(--fg);
      font-family: var(--mono);
      font-size: 0.95rem;
      font-variant-numeric: tabular-nums;
    }
    select:focus, .power-wrap input:focus {
      outline: none;
      border-color: var(--accent-e);
    }

    .power-wrap { display: flex; align-items: center; gap: 10px; }
    .power-wrap input { flex: 1; }

    .unit {
      font-family: var(--mono);
      color: var(--fg-muted);
      font-size: 0.78rem;
      text-transform: uppercase;
      letter-spacing: 0.18em;
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
    .seg-btn.active { background: var(--accent-e); color: #0a0a0a; }

    .chips { display: flex; gap: 6px; flex-wrap: wrap; }
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
    }
    .chip:hover { color: var(--fg); border-color: var(--fg-dim); }
    .chip.active { background: var(--accent-e); color: #0a0a0a; border-color: var(--accent-e); }

    .active-banner {
      background: color-mix(in srgb, var(--accent-e) 10%, transparent);
      border: 1px solid var(--accent-e);
      border-radius: 8px;
      padding: 12px 14px;
      margin-bottom: 16px;
    }
    .active-banner.hidden { display: none; }
    .active-headline { font-family: var(--sans); font-weight: 600; color: var(--accent-e); margin-bottom: 2px; }
    .active-detail { font-family: var(--mono); font-size: 12px; color: var(--fg-dim); font-variant-numeric: tabular-nums; }

    .empty-state {
      padding: 16px;
      border: 1px dashed var(--line);
      border-radius: 8px;
      color: var(--fg-muted);
      font-size: 13px;
      line-height: 1.5;
      margin-bottom: 12px;
    }
    .empty-state.hidden { display: none; }

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
    .footer-btn:disabled { opacity: 0.45; cursor: not-allowed; }
    .footer-btn[data-variant="install"] {
      flex: 2;
      background: var(--accent-e);
      color: #0a0a0a;
      border: 1px solid var(--accent-e);
    }
    .footer-btn[data-variant="install"]:hover:not(:disabled) { transform: translateY(-1px); }
    .footer-btn[data-stop] {
      background: transparent;
      color: var(--fg);
      border: 1px solid var(--line);
    }
    .footer-btn[data-stop]:not(:disabled) { color: var(--red-e); border-color: var(--red-e); }
    .footer-btn[data-stop]:hover:not(:disabled) {
      background: color-mix(in srgb, var(--red-e) 12%, transparent);
    }
  `;

  constructor() {
    super();
    this._refreshTimer = null;
    this._formState = { mode: "pct", limitPct: 50, limitW: 1000, holdS: 300, driver: "" };
    this._capableDrivers = [];
  }

  disconnectedCallback() {
    if (this._refreshTimer) {
      clearInterval(this._refreshTimer);
      this._refreshTimer = null;
    }
  }

  // open(driverId?: string) — driverId="" / undefined opens at the
  // site-aggregate scope. A driver id pre-selects that driver in the
  // picker (no-op if the catalog doesn't advertise it).
  open(driverId) {
    const modal = this.shadowRoot.querySelector("ftw-modal");
    if (!modal) return;
    this._showError("");
    if (driverId) this._formState.driver = driverId;
    this._selectMode(this._formState.mode);
    this._selectDuration(this._formState.holdS);
    modal.open();
    this._loadCatalog().then(() => {
      this._renderDriverOptions();
      this._refresh();
    });
    if (this._refreshTimer) clearInterval(this._refreshTimer);
    this._refreshTimer = setInterval(() => this._refresh(), 3000);
  }

  render() {
    return `
      <ftw-modal style="--ftw-modal-max-width:440px">
        <span slot="title">PV control</span>

        <div class="active-banner hidden" data-active>
          <div class="active-headline"></div>
          <div class="active-detail"></div>
        </div>

        <div class="empty-state hidden" data-empty>
          No PV drivers advertise the <code>pv-curtail</code> capability.
          Add <code>supports_pv_curtail: true</code> to a curtail-capable
          driver's YAML and reload the config.
        </div>

        <div class="row" data-controls-row>
          <label class="label">Scope</label>
          <select data-driver></select>
        </div>

        <div class="row" data-controls-row>
          <label class="label">Limit by</label>
          <div class="segmented" role="radiogroup" aria-label="Limit by">
            <button type="button" class="seg-btn active" data-mode="pct" role="radio">Percent</button>
            <button type="button" class="seg-btn" data-mode="w" role="radio">Watts</button>
          </div>
        </div>

        <div class="row" data-controls-row data-pct-row>
          <label class="label" for="pct-input">Limit (% of inverter max)</label>
          <div class="power-wrap">
            <input data-pct-input type="number" min="0" max="100" step="5" value="50" inputmode="numeric"/>
            <span class="unit">%</span>
          </div>
        </div>

        <div class="row hidden" data-controls-row data-w-row>
          <label class="label" for="w-input">Limit (W)</label>
          <div class="power-wrap">
            <input data-w-input type="number" min="0" step="100" value="1000" inputmode="numeric"/>
            <span class="unit">W</span>
          </div>
        </div>

        <div class="row" data-controls-row>
          <label class="label">Duration</label>
          <div class="chips">
            <button type="button" class="chip" data-hold="60">1 min</button>
            <button type="button" class="chip active" data-hold="300">5 min</button>
            <button type="button" class="chip" data-hold="900">15 min</button>
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
    root.querySelectorAll(".seg-btn").forEach((b) =>
      b.addEventListener("click", () => this._selectMode(b.dataset.mode)));
    root.querySelectorAll(".chip").forEach((c) =>
      c.addEventListener("click", () => this._selectDuration(Number(c.dataset.hold))));
    const sel = root.querySelector("[data-driver]");
    if (sel) sel.addEventListener("change", (e) => { this._formState.driver = e.target.value; });
    root.querySelector("[data-install]").addEventListener("click", () => this._install());
    root.querySelector("[data-stop]").addEventListener("click", () => this._stop());
    modal.addEventListener("ftw-modal-close", () => {
      if (this._refreshTimer) {
        clearInterval(this._refreshTimer);
        this._refreshTimer = null;
      }
    });
  }

  _selectMode(mode) {
    this._formState.mode = mode;
    const root = this.shadowRoot;
    root.querySelectorAll(".seg-btn").forEach((b) =>
      b.classList.toggle("active", b.dataset.mode === mode));
    root.querySelector("[data-pct-row]").classList.toggle("hidden", mode !== "pct");
    root.querySelector("[data-w-row]").classList.toggle("hidden", mode !== "w");
  }

  _selectDuration(holdS) {
    this._formState.holdS = holdS;
    this.shadowRoot.querySelectorAll(".chip").forEach((c) =>
      c.classList.toggle("active", Number(c.dataset.hold) === holdS));
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

  _loadCatalog() {
    // Source of truth for the picker is the user's config — only
    // drivers explicitly opted in via supports_pv_curtail: true should
    // appear. The catalog endpoint advertises what the lua *can* do;
    // the YAML says what the operator *wants* dispatched.
    return fetch("/api/config")
      .then((r) => r.json())
      .then((cfg) => {
        const drivers = (cfg && cfg.drivers) || [];
        this._capableDrivers = drivers
          .filter((d) => d && d.supports_pv_curtail === true)
          .map((d) => ({ id: d.name, name: d.name }));
      })
      .catch(() => { this._capableDrivers = []; });
  }

  _renderDriverOptions() {
    const root = this.shadowRoot;
    const sel = root.querySelector("[data-driver]");
    const empty = root.querySelector("[data-empty]");
    const controlRows = root.querySelectorAll("[data-controls-row]");
    if (!sel || !empty) return;
    if (this._capableDrivers.length === 0) {
      empty.classList.remove("hidden");
      controlRows.forEach((r) => r.classList.add("hidden"));
      root.querySelector("[data-install]").disabled = true;
      return;
    }
    empty.classList.add("hidden");
    controlRows.forEach((r) => r.classList.remove("hidden"));
    // The mode-specific row visibility depends on _formState.mode; re-apply it.
    this._selectMode(this._formState.mode);
    root.querySelector("[data-install]").disabled = false;

    const opts = ['<option value="">All curtail-capable drivers</option>'];
    for (const e of this._capableDrivers) {
      const id = e.id || e.ID;
      const name = e.name || e.Name || id;
      opts.push(`<option value="${id}">${name}</option>`);
    }
    sel.innerHTML = opts.join("");
    if (this._formState.driver) sel.value = this._formState.driver;
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
    const scope = d.driver ? `driver: ${d.driver}` : "site-aggregate";
    const headline = `Capping PV at ${Math.round(d.limit_w || 0)} W (${scope})`;
    let remaining = "";
    if (d.expires_at_ms) {
      const ms = d.expires_at_ms - Date.now();
      if (ms > 0) {
        const total = Math.round(ms / 1000);
        const min = Math.floor(total / 60);
        const sec = total % 60;
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
    fetch("/api/pv/manual_hold")
      .then((r) => r.json())
      .then((d) => this._renderActive(d))
      .catch(() => { /* network blip */ });
  }

  _install() {
    this._showError("");
    const body = {
      driver: this._formState.driver || undefined,
      hold_s: this._formState.holdS,
    };
    if (this._formState.mode === "pct") {
      const v = Number(this.shadowRoot.querySelector("[data-pct-input]").value);
      if (!isFinite(v) || v < 0 || v > 100) {
        this._showError("Percent must be in [0, 100]");
        return;
      }
      body.limit_pct = v;
    } else {
      const v = Number(this.shadowRoot.querySelector("[data-w-input]").value);
      if (!isFinite(v) || v < 0) {
        this._showError("Watts must be a non-negative number");
        return;
      }
      body.limit_w = v;
    }
    const installBtn = this.shadowRoot.querySelector("[data-install]");
    installBtn.disabled = true;
    ownerFetch("/api/pv/manual_hold", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    })
      .then((r) => {
        if (!r.ok) {
          return r.json().then((j) => { throw new Error((j && j.error) || ("HTTP " + r.status)); });
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
    ownerFetch("/api/pv/manual_hold", { method: "DELETE" })
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

customElements.define("ftw-pv-control", FtwPvControl);
