// forty-two-watts dashboard — plain JS, no framework

(function () {
  "use strict";

  const POLL_INTERVAL = 2000;        // status poll cadence — snappier cards

  // FLOW_IDLE_KW — magnitude below which a planet is treated as
  // "idle / balanced" for label + colour purposes. Mirror of
  // ftw-energy-flow.js's FLOW_IDLE_W (which sets window.FTW_FLOW_IDLE_W
  // when its module loads). Read at use-time so the module-set value
  // wins; literal `42` is the no-modules fallback. Inclusive
  // comparison everywhere: |kW| <= threshold ⇒ idle.
  function flowIdleKw() {
    const w = (typeof window !== "undefined" && window.FTW_FLOW_IDLE_W) || 42;
    return w / 1000;
  }
  function isFlowIdle(kw) { return Math.abs(kw) <= flowIdleKw(); }
  const CHART_POINTS = 360;          // up to 30 min of points (server pushes every ~5s)
  const CHART_RANGE_MS = {           // visible time window per range option
    "5m": 5 * 60 * 1000,
    "15m": 15 * 60 * 1000,
    "1h": 60 * 60 * 1000,
    "6h": 6 * 60 * 60 * 1000,
    "24h": 24 * 60 * 60 * 1000,
    "3d": 3 * 24 * 60 * 60 * 1000,
  };
  let chartRange = "5m";             // current selected range
  let currentMode = null;
  let animating = true;              // 30fps redraw loop flag
  let lastDataTs = 0;                // browser-clock timestamp of newest pushed point
  let lastPushAt = 0;                // browser-clock timestamp of last push attempt — for dedupe (NEVER mix with server ts)
  let lastFlashAt = 0;               // browser-clock timestamp of last "new data" flash

  // ---- Chart data ----
  var chartHistory = {
    grid: [],
    pv: [],
    load: [],
    timestamps: [],
    // Energy counters (cumulative Wh, today-scoped)
    e_import: [],
    e_export: [],
    e_pv: [],
    e_charged: [],
    e_discharged: [],
    e_load: [],
  };

  // Per-battery-driver chart series. Discovered dynamically from the
  // /api/status drivers map (any driver exposing bat_w is treated as a
  // battery source). Shape: { [driverName]: { bat: [...], target: [...] } }.
  // Kept separate from chartHistory so the Object.keys(...).shift() loops
  // below don't stumble over a nested object.
  var chartBatteries = {};

  // Per-EV-charger chart series. Same discovery pattern as batteries,
  // triggered by any driver exposing `ev_w` (EV charge power in W,
  // positive = charging). One series per charger so multi-charger
  // homes see each car separately. Shape: { [driverName]: { ev: [...] } }.
  var chartEVs = {};

  // Deterministic color palette for battery series — each driver gets a
  // stable color based on name hash so reload is consistent.
  var BATTERY_PALETTE = [
    "#f59e0b", "#8b5cf6", "#ec4899", "#06b6d4",
    "#eab308", "#14b8a6", "#f43f5e", "#a855f7",
  ];
  function batteryColor(name) {
    var h = 0;
    for (var i = 0; i < name.length; i++) {
      h = ((h << 5) - h + name.charCodeAt(i)) | 0;
    }
    return BATTERY_PALETTE[Math.abs(h) % BATTERY_PALETTE.length];
  }
  // A driver name → presentation label. Capitalize first letter so
  // "pixii" → "Pixii"; everything else passes through as-is.
  function batteryLabel(name) {
    if (!name) return name;
    return name.charAt(0).toUpperCase() + name.slice(1);
  }

  // Ensure a battery-driver slot exists. Backfills bat/target arrays
  // with zeros up to current timestamps length so row indices align
  // with the other chartHistory series.
  function ensureBatteryDriver(name) {
    if (chartBatteries[name]) return chartBatteries[name];
    var pad = chartHistory.timestamps.length;
    var slot = { bat: new Array(pad).fill(0), target: new Array(pad).fill(0) };
    chartBatteries[name] = slot;
    syncBatteryLegend();
    return slot;
  }

  // Append a legend item for any newly-discovered battery driver. Uses
  // the same markup as the static legend entries so the click handler
  // (delegated on #chart-legend) picks them up automatically.
  function syncBatteryLegend() {
    var host = document.getElementById("chart-legend");
    if (!host) return;
    Object.keys(chartBatteries).forEach(function (name) {
      var key = "bat:" + name;
      if (host.querySelector('[data-toggle="' + cssEscape(key) + '"]')) return;
      var span = document.createElement("span");
      span.className = "legend-item";
      span.dataset.toggle = key;
      if (legendHidden[key]) span.classList.add("legend-off");
      var swatch = document.createElement("span");
      swatch.className = "legend-color";
      swatch.style.background = batteryColor(name);
      span.appendChild(swatch);
      span.appendChild(document.createTextNode(" " + batteryLabel(name)));
      host.appendChild(span);
    });
  }

  // EV chargers use a distinct palette (magenta/fuchsia family) so they
  // can't be confused with batteries (amber/purple) or PV (green) in
  // the live chart. Deterministic hash → stable color across reloads.
  var EV_PALETTE = [
    "#ec4899", "#d946ef", "#f97316", "#0ea5e9",
    "#84cc16", "#f43f5e", "#a855f7", "#14b8a6",
  ];
  function evColor(name) {
    var h = 0;
    for (var i = 0; i < name.length; i++) {
      h = ((h << 5) - h + name.charCodeAt(i)) | 0;
    }
    return EV_PALETTE[Math.abs(h) % EV_PALETTE.length];
  }
  function evLabel(name) {
    if (!name) return name;
    return name.charAt(0).toUpperCase() + name.slice(1) + " EV";
  }
  function ensureEVDriver(name) {
    if (chartEVs[name]) return chartEVs[name];
    var pad = chartHistory.timestamps.length;
    var slot = { ev: new Array(pad).fill(0) };
    chartEVs[name] = slot;
    syncEVLegend();
    return slot;
  }
  function syncEVLegend() {
    var host = document.getElementById("chart-legend");
    if (!host) return;
    Object.keys(chartEVs).forEach(function (name) {
      var key = "ev:" + name;
      if (host.querySelector('[data-toggle="' + cssEscape(key) + '"]')) return;
      var span = document.createElement("span");
      span.className = "legend-item";
      span.dataset.toggle = key;
      if (legendHidden[key]) span.classList.add("legend-off");
      var swatch = document.createElement("span");
      swatch.className = "legend-color";
      swatch.style.background = evColor(name);
      span.appendChild(swatch);
      span.appendChild(document.createTextNode(" " + evLabel(name)));
      host.appendChild(span);
    });
  }
  // Minimal CSS.escape polyfill (legend keys contain ':').
  function cssEscape(s) { return String(s).replace(/[^a-zA-Z0-9_-]/g, function(c) { return "\\" + c; }); }

  // Latest MPC plan — refreshed every 30s. Drives the forward-looking
  // dashed PV + Load forecast on the live chart (right-hand segment
  // extending past "now").
  var chartPlan = null;
  function refreshChartPlan() {
    fetch("/api/mpc/plan")
      .then(function (r) { return r.json(); })
      .then(function (j) { if (j && j.plan) chartPlan = j.plan; })
      .catch(function () {});
  }
  refreshChartPlan();
  setInterval(refreshChartPlan, 30000);
  var chartLayout = null;
  var hoverIndex = -1;
  var hoverForecast = null; // { ts, action } when hovering in future region
  // Per-series visibility toggled by clicking legend items. Persisted
  // to localStorage so reload keeps the operator's view.
  var legendHidden = {};
  try { legendHidden = JSON.parse(localStorage.getItem("legend-hidden") || "{}") || {}; } catch (e) { legendHidden = {}; }
  var chartView = "power"; // "power" or "energy"

  // ---- DOM refs ----
  const $ = (id) => document.getElementById(id);
  const gridW = $("grid-w");
  const gridDir = $("grid-dir");
  const loadW = $("load-w");
  const cardGrid = $("card-grid");
  const pvW = $("pv-w");
  const batW = $("bat-w");
  const batDir = $("bat-dir");
  const connStatus = $("conn-status");
  const driversGrid = $("drivers-grid");
  const dispatchList = $("dispatch-list");
  const modeButtons = $("mode-buttons");
  const gridTargetSlider = $("grid-target-slider");
  const gridTargetValue = $("grid-target-value");
  const gridTargetSend = $("grid-target-send");
  const peakLimitSlider = $("peak-limit-slider");
  const peakLimitValue = $("peak-limit-value");
  const peakLimitSend = $("peak-limit-send");
  const peakLimitEnableToggle = $("peak-limit-enabled-toggle");
  const peakLimitEnableLabel = $("peak-limit-enabled-label");
  // Dirty-tracking for sliders that POST on click. The poll handler
  // skips overwrite while dirty so the user's pending edit isn't
  // silently reverted; the Save button mirrors `dirty` so users get a
  // visual cue that there's work to commit. Cleared on successful POST.
  let peakLimitDirty = false;
  let gridTargetDirty = false;
  // Last non-zero peak ceiling, remembered so unchecking + rechecking
  // the enable toggle restores the previous value instead of resetting
  // to the default. Keyed by localStorage so it survives reloads.
  function readLastPeakLimitW() {
    try {
      const v = localStorage.getItem("ftw-peak-import-ceiling-w");
      const n = v == null ? null : Number(v);
      return Number.isFinite(n) && n > 0 ? n : 5000;
    } catch (e) { return 5000; }
  }
  function writeLastPeakLimitW(w) {
    try { localStorage.setItem("ftw-peak-import-ceiling-w", String(w)); } catch (e) {}
  }
  const evSlider = $("ev-slider");
  const evValue = $("ev-value");
  const evSend = $("ev-send");
  const bceToggle = $("battery-covers-ev-toggle");
  const bceLabel = $("battery-covers-ev-label");
  const fuseUse = $("fuse-use");
  const fuseFill = $("fuse-fill");
  const fusePhases = $("fuse-phases");
  const eImport = $("e-import");
  const eExport = $("e-export");
  const ePv = $("e-pv");
  const eCharged = $("e-charged");
  const eDischarged = $("e-discharged");
  const eLoad = $("e-load");
  const lastUpdate = $("last-update");
  const versionEl = $("version");
  // ---- Formatting ----
  function formatW(w) {
    const abs = Math.abs(w);
    if (abs >= 1000) {
      return (w / 1000).toFixed(1) + " kW";
    }
    return Math.round(w) + " W";
  }

  // Fuse bar color — smooth green→yellow→orange→red gradient across
  // 0 %→100 %. Anchors land at the named color at each zone
  // boundary: 0 %=green, 50 %=yellow, 75 %=orange, 90 %+=red. Between
  // them the hue interpolates linearly so a bar at 45 % already reads
  // yellow-leaning (h ≈ 100, close to yellow's 95), not solid green —
  // matches the "45 % is more towards yellow than green" spec. Oklch
  // holds lightness + chroma constant so the transitions don't muddy.
  function fuseFillColor(pct) {
    var c = Math.max(0, Math.min(100, pct));
    var h;
    if (c < 50) {
      // 0 → 50 : 150 (green) → 95 (yellow)
      h = 150 - (c / 50) * (150 - 95);
    } else if (c < 75) {
      // 50 → 75 : 95 (yellow) → 50 (orange)
      h = 95 - ((c - 50) / 25) * (95 - 50);
    } else if (c < 90) {
      // 75 → 90 : 50 (orange) → 22 (red)
      h = 50 - ((c - 75) / 15) * (50 - 22);
    } else {
      // 90 → 100: solid red. Saturate rather than continue shifting
      // hue — operators just need to see "DANGER", not a hue delta.
      h = 22;
    }
    return "oklch(0.78 0.18 " + h.toFixed(1) + ")";
  }

  // Snap an axis range to "nice" round numbers. Returns { min, max, step }
  // where step is a 1/2/5 × 10^k value chosen so the axis spans `count`
  // ticks across roughly the original range. Guarantees that 0 lands on
  // a gridline when the input range crosses zero.
  function niceAxis(min, max, count) {
    if (!(max > min)) { max = min + 1; }
    var rough = (max - min) / count;
    var mag = Math.pow(10, Math.floor(Math.log10(rough)));
    var norm = rough / mag;
    var step = (norm < 1.5 ? 1 : norm < 3 ? 2 : norm < 7 ? 5 : 10) * mag;
    return {
      min: Math.floor(min / step) * step,
      max: Math.ceil(max / step) * step,
      step: step,
    };
  }

  function formatSoc(soc) {
    return Math.round(soc * 100) + "%";
  }

  function formatKwh(wh) {
    var kwh = (wh || 0) / 1000;
    if (kwh >= 100) return kwh.toFixed(0) + " kWh";
    if (kwh >= 10) return kwh.toFixed(1) + " kWh";
    return kwh.toFixed(2) + " kWh";
  }
  // Compact kWh — bubble lines that pack two arrows ("↓ 5.2 ↑ 12") need
  // tighter formatting than the standalone tile reading. Drops the "kWh"
  // unit (already implied by the bubble label "kWh today" elsewhere).
  function fmtKwhShort(kwh) {
    if (kwh == null || !isFinite(kwh)) return "—";
    var v = Math.abs(kwh);
    if (v >= 100) return kwh.toFixed(0);
    if (v >= 10)  return kwh.toFixed(1);
    return kwh.toFixed(2);
  }

  function statusClass(status) {
    if (!status) return "status-offline";
    const s = status.toLowerCase();
    if (s === "ok") return "status-ok";
    if (s === "degraded") return "status-degraded";
    return "status-offline";
  }

  // ---- Render ----
  function render(data) {
    // Battery presence → body.no-battery toggle. Drives visibility of
    // the "Bat charged" / "Bat discharged" tiles and the Plan chart's
    // Charge / Discharge / SoC legend + drawing layers. Any driver
    // exposing bat_w counts; if the current tick has zero such
    // drivers the class goes on and the CSS in next.css hides
    // everything tagged .bat-only. Re-evaluated every /api/status
    // poll so plugging in a battery lights everything back up
    // without a reload.
    var hasBattery = false;
    var drvMap = data.drivers || {};
    for (var drvName in drvMap) {
      if (drvMap[drvName] && drvMap[drvName].bat_w != null) {
        hasBattery = true; break;
      }
    }
    document.body.classList.toggle("no-battery", !hasBattery);

    // PUSH CHART DATA FIRST — never let a DOM render error somewhere below
    // silently kill the chart-update path. (Prior bug: missing #dispatch-list
    // threw inside renderDispatch, which is between renderDrivers and
    // pushChartData, so the chart starved while ticks kept incrementing.)
    try {
      // Build a {driver → target_w} index from the dispatch array so the
      // per-battery push below doesn't need an inner loop.
      var targetsByDriver = {};
      (data.dispatch || []).forEach(function (d) {
        if (d && d.driver) targetsByDriver[d.driver] = d.target_w || 0;
      });
      pushChartData(data, targetsByDriver);
    } catch (e) { console.error("pushChartData error:", e); }

    // Version (live from API — survives stale browser cache of index.html)
    if (versionEl && data.version) {
      versionEl.textContent = data.version;
    }
    // Grid + target indicator
    gridW.textContent = formatW(data.grid_w);
    if (data.grid_w > 10) {
      gridDir.textContent = "importing";
      gridW.className = "card-value val-import";
    } else if (data.grid_w < -10) {
      gridDir.textContent = "exporting";
      gridW.className = "card-value val-export";
    } else {
      gridDir.textContent = "balanced";
      gridW.className = "card-value val-neutral";
    }
    var targetDisp = document.getElementById("grid-target-display");
    if (targetDisp) {
      var t = data.grid_target_w || 0;
      targetDisp.textContent = t === 0 ? "target 0" : "target " + formatW(t);
    }

    // PV — stored as negative (site convention) but displayed positive
    // so "SOLAR 5.3 kW" reads as generation magnitude without the minus.
    // Internal data (chart history, hero setReadings, plan math) stays
    // on site convention — flip is for this tile only.
    pvW.textContent = formatW(-data.pv_w);
    pvW.className = "card-value val-generation";

    // Load
    loadW.textContent = formatW(data.load_w || 0);

    // EV tile (tile-mode parity with the energy-flow's EV planet).
    // Reads ev_charging_w (post sub-watt floor in /api/status); the
    // sub-card label flips to "charging" when active so the tile reads
    // the same way the planet does.
    var cardEvWEl   = document.getElementById("card-ev-w");
    var cardEvSubEl = document.getElementById("card-ev-sub");
    if (cardEvWEl) {
      var evWNow = data.ev_charging_w || 0;
      cardEvWEl.textContent = formatW(evWNow);
      cardEvWEl.className = evWNow > 1 ? "card-value val-load" : "card-value val-neutral";
      if (cardEvSubEl) cardEvSubEl.textContent = evWNow > 1 ? "charging" : "charger";
    }

    // Battery — positive=charge, negative=discharge
    batW.textContent = formatW(data.bat_w);
    if (data.bat_w > 10) {
      batDir.textContent = "charging";
      batW.className = "card-value val-charging";
    } else if (data.bat_w < -10) {
      batDir.textContent = "discharging";
      batW.className = "card-value val-discharging";
    } else {
      batDir.textContent = "idle";
      batW.className = "card-value val-neutral";
    }

    // Hero energy-flow diagram — build a flat "planets" list where each
    // entry declares which corner it orbits (top-left=PV, top-right=
    // battery, bottom-left=grid, bottom-right=EV). The component knows
    // nothing about driver roles — all role→color/sub-text/direction
    // mapping lives here so the four corners stay a caller concern.
    // setReadings() replaces `planets` atomically, so a transient
    // /api/status error preserves the last good layout if we skip it.
    var flowEl = document.getElementById("energy-flow");
    if (flowEl && typeof flowEl.setReadings === "function") {
      var planets = [];

      // Today's totals (aggregate across drivers; per-driver split
      // isn't in the API). The same string lands on every same-role
      // planet so the aggregation layer can pick it up cleanly.
      var todayE = (data.energy && data.energy.today) || {};
      var importKwh   = (todayE.import_wh || 0) / 1000;
      var exportKwh   = (todayE.export_wh || 0) / 1000;
      var pvKwhTotal  = (todayE.pv_wh || 0) / 1000;
      var loadKwhTotal = (todayE.load_wh || 0) / 1000;
      var batChargedKwh    = (todayE.bat_charged_wh || 0) / 1000;
      var batDischargedKwh = (todayE.bat_discharged_wh || 0) / 1000;
      // Solar only flows one direction (production); the arrow would
      // be redundant. Use the kWh unit instead so the line reads as
      // a standalone total.
      var pvDailyStr   = fmtKwhShort(pvKwhTotal) + " kWh";
      // Grid daily totals are colour-coded: import red, export green,
      // both bold so the polarity reads at a glance against the dark
      // bubble. Other planets stay on the plain dimmed text style.
      var gridDailyParts = [
        { text: "↓ " + fmtKwhShort(importKwh), color: "var(--red-e)",   bold: true },
        { text: "↑ " + fmtKwhShort(exportKwh), color: "var(--green-e)", bold: true },
      ];
      // Battery daily totals share the grid's colour discipline:
      // charge (energy stored) green, discharge (energy spent) red.
      // Reads at a glance whether the day was a net-fill or net-drain.
      var batDailyParts = [
        { text: "↑ " + fmtKwhShort(batChargedKwh),    color: "var(--green-e)", bold: true },
        { text: "↓ " + fmtKwhShort(batDischargedKwh), color: "var(--red-e)",   bold: true },
      ];

      // Grid — single utility, bottom-left corner. Import = toward house.
      var gkw = (data.grid_w || 0) / 1000;
      var gIdle = isFlowIdle(gkw);
      planets.push({
        id: "grid", corner: "bottom-left", title: "GRID", role: "grid",
        kw: gkw, toHub: gkw >= 0,
        color: gIdle ? "var(--fg-muted)" :
               (gkw >= 0 ? "var(--red-e)" : "var(--green-e)"),
        sub: gIdle ? "balanced" :
             (gkw >= 0 ? "importing" : "exporting"),
        dailyKwhParts: gridDailyParts,
      });

      var drvs = data.drivers || {};
      Object.keys(drvs).forEach(function (name) {
        var d = drvs[name] || {};
        // Solar — display positive kW when generating (site convention
        // has pv_w negative for export into the house). All internal
        // state (chart history, math) stays on site convention; the
        // sign flip is display-only and lives in this function.
        if (d.pv_w != null) {
          var pvKw = -d.pv_w / 1000;
          var pvGen = !isFlowIdle(pvKw);
          planets.push({
            id: "pv-" + name, corner: "top-left", title: "SOLAR", name: name, role: "pv",
            kw: pvKw, toHub: true,
            color: pvGen ? "var(--amber)" : "var(--fg-muted)",
            // Solar is one-directional: the power value alone already
            // shows whether it's generating or idle. The sub-label
            // would just repeat the same fact in words.
            sub: "",
            dailyKwh: pvDailyStr,
          });
        }
        // Battery — sign shows charge/discharge. Discharge flows toward
        // the house; charge flows away from it.
        if (d.bat_w != null) {
          var bKw = d.bat_w / 1000;
          var bIdle = isFlowIdle(bKw);
          // Direction conveyed by colour of the power value: charge
          // green (filling), discharge red (draining), idle stays
          // neutral cyan (the battery's identity hue). Drops the
          // wordy charging/discharging sub-label.
          var bColor = bIdle ? "var(--cyan)" :
                       (bKw >= 0 ? "var(--green-e)" : "var(--red-e)");
          planets.push({
            id: "bat-" + name, corner: "top-right", title: "BATTERY", name: name, role: "battery",
            kw: bKw, toHub: bKw < 0,
            color: bColor,
            sub: "",
            soc: d.bat_soc != null ? Math.round(d.bat_soc * 100) : null,
            dailyKwhParts: batDailyParts,
          });
        }
        // EV — always consumes from the house side. When a loadpoint
        // maps to this driver AND a vehicle telemetry source is
        // reporting (DerVehicle), inject the vehicle's own SoC +
        // charge-limit so the bubble renders "24 / 50 %" — measured
        // truth instead of session-Wh estimate.
        if (d.ev_w != null) {
          var eKw = d.ev_w / 1000;
          var eActive = !isFlowIdle(eKw);
          var lpEv = loadpointsByDriver && loadpointsByDriver[name];
          var evSoc = null;
          var evLimit = null;
          var evSocStale = false;
          var evSocSource = null;
          if (lpEv) {
            // Prefer vehicle-reported when present; fall back to
            // the inferred SoC the manager computed from session_wh.
            if (lpEv.vehicle_soc_pct > 0) {
              evSoc = lpEv.vehicle_soc_pct;
              evSocSource = "vehicle";
            } else if (lpEv.current_soc_pct > 0) {
              evSoc = lpEv.current_soc_pct;
              evSocSource = lpEv.soc_source || "inferred";
            }
            if (lpEv.vehicle_charge_limit_pct > 0) {
              evLimit = lpEv.vehicle_charge_limit_pct;
            }
            evSocStale = !!lpEv.vehicle_stale;
          }
          planets.push({
            id: "ev-" + name, corner: "bottom-right", title: "EV CHARGER", name: name, role: "ev",
            kw: eKw, toHub: false,
            color: eActive ? "var(--green-e)" : "var(--white-s)",
            sub: eActive ? "charging" : "idle",
            soc: evSoc,
            chargeLimit: evLimit,
            socStale: evSocStale,
            socSource: evSocSource,
          });
        }
      });

      // Self-powered today: share of consumption sourced from PV /
      // battery over the whole day. Mirrors the realtime
      // selfPoweredPct the energy-flow component computes from
      // current planet power, so the two hub lines are directly
      // comparable. Clamped 0..100 because metering glitches can
      // briefly report import > load.
      var selfPoweredPctToday = null;
      if (loadKwhTotal > 0.001) {
        selfPoweredPctToday = Math.max(0, Math.min(100,
          (1 - importKwh / loadKwhTotal) * 100));
      }
      flowEl.setReadings({
        load:    (data.load_w || 0) / 1000,
        planets: planets,
        selfPoweredPctToday: selfPoweredPctToday,
      });
    }

    // Mode buttons — primary (strategy) + advanced (manual)
    currentMode = data.mode;
    var allModeButtons = document.querySelectorAll("#mode-buttons-primary button, #mode-buttons button");
    allModeButtons.forEach(function (btn) {
      if (btn.dataset.mode === data.mode) btn.classList.add("active");
      else btn.classList.remove("active");
    });
    // When planner is driving, grey out the grid-target slider and show a hint.
    var plannerActive = (data.mode || "").indexOf("planner_") === 0;
    var gridSlider = document.getElementById("grid-target-slider");
    var gridSend = document.getElementById("grid-target-send");
    var gridHint = document.getElementById("grid-target-hint");
    if (gridSlider) gridSlider.disabled = plannerActive;
    if (gridSend) gridSend.disabled = plannerActive;
    if (gridHint) {
      // Always show the hint inside the modal — when planner is
      // driving, the hint *is* the explanation for the disabled
      // control, so it takes a brighter style (card-hint-active);
      // when the operator can edit the slider, it stays as quiet
      // small print.
      gridHint.style.display = "block";
      gridHint.classList.toggle("card-hint-active", plannerActive);
    }
    // Plan-stale banner
    if (data.plan_stale && plannerActive && gridHint) {
      gridHint.textContent = "⚠ Plan stale — falling back to self_consumption.";
      gridHint.classList.add("card-hint-warn");
    } else if (gridHint) {
      gridHint.textContent = plannerActive
        ? "Planner is driving — set strategy to Manual to edit grid target."
        : "Planner controls this when a strategy is active.";
      gridHint.classList.remove("card-hint-warn");
    }

    // Grid target — only update slider if user has no pending edit. We
    // check `dirty` rather than activeElement so a value that was
    // dragged then mouse-released doesn't revert before Save is clicked.
    if (gridTargetSlider && !gridTargetDirty) {
      gridTargetSlider.value = data.grid_target_w;
      gridTargetValue.textContent = formatW(data.grid_target_w);
    }
    // Peak import ceiling. Backed by the new peak_import_ceiling_w
    // (hard rule across modes); 0 = disabled. Falls back to the legacy
    // peak_limit_w field for older backends so the UI doesn't go blank
    // during a partial-deploy window.
    const peakSrcW = data.peak_import_ceiling_w != null
      ? data.peak_import_ceiling_w
      : data.peak_limit_w;
    if (peakLimitSlider && !peakLimitDirty && peakSrcW != null) {
      const enabled = peakSrcW > 0;
      if (peakLimitEnableToggle) {
        peakLimitEnableToggle.checked = enabled;
        if (peakLimitEnableLabel) peakLimitEnableLabel.textContent = enabled ? "On" : "Off";
      }
      peakLimitSlider.disabled = !enabled;
      const display = enabled ? peakSrcW : readLastPeakLimitW();
      peakLimitSlider.value = display;
      // Don't decorate the value with " (off)" — the toggle is the
      // single source of truth for enabled/disabled, doubling that
      // signal is the kind of redundancy DESIGN.md warns about.
      peakLimitValue.textContent = formatW(display);
      if (enabled) writeLastPeakLimitW(peakSrcW);
      if (peakLimitSend) peakLimitSend.disabled = true; // pristine
    }
    if (evSlider && document.activeElement !== evSlider && data.ev_charging_w != null) {
      evSlider.value = data.ev_charging_w;
      evValue.textContent = formatW(data.ev_charging_w);
    }
    // Battery-covers-EV toggle — only update DOM when not mid-click,
    // otherwise the user's change would get overwritten by the next
    // status poll before the POST round-trip settles.
    if (bceToggle && document.activeElement !== bceToggle && data.battery_covers_ev != null) {
      bceToggle.checked = !!data.battery_covers_ev;
      if (bceLabel) bceLabel.textContent = data.battery_covers_ev ? "On" : "Off";
    }

    // Energy today
    if (data.energy && data.energy.today) {
      var t = data.energy.today;
      if (eImport) eImport.textContent = formatKwh(t.import_wh);
      if (eExport) eExport.textContent = formatKwh(t.export_wh);
      if (ePv) ePv.textContent = formatKwh(t.pv_wh);
      if (eCharged) eCharged.textContent = formatKwh(t.bat_charged_wh);
      if (eDischarged) eDischarged.textContent = formatKwh(t.bat_discharged_wh);
      if (eLoad) eLoad.textContent = formatKwh(t.load_wh);
    }

    // Fuse gauge — per-phase bars if the server reports phase amperage,
    // otherwise a single aggregate bar + number (fallback).
    if (fuseUse && fuseFill) {
      var fuseCfg = data.fuse || {};
      var maxAmps = fuseCfg.max_amps || 16;
      var phases  = fuseCfg.phases   || 3;
      var voltage = fuseCfg.voltage  || 230;

      var phaseI = Array.isArray(data.phase_amps) ? data.phase_amps : [];
      var phaseW = Array.isArray(data.phase_powers) ? data.phase_powers : [];
      var hasPhaseData = phaseI.length > 0;

      // Hide the per-phase box row entirely when no phase data, otherwise
      // we'd render `phases` boxes populated with 0 A alongside the
      // aggregate fallback and the two cards would double up.
      var fallbackBar = $("fuse-bar-fallback");
      if (fallbackBar) fallbackBar.style.display = hasPhaseData ? "none" : "block";
      fuseUse.style.display = hasPhaseData ? "none" : "block";
      if (fusePhases) fusePhases.style.display = hasPhaseData ? "" : "none";

      if (!hasPhaseData) {
        var totalDischarge = 0;
        if (data.bat_w < 0) totalDischarge = Math.abs(data.bat_w);
        var pvGen = Math.abs(data.pv_w);
        var throughput = Math.max(Math.abs(data.grid_w), pvGen + totalDischarge);
        var peakA = throughput / voltage / phases;
        fuseUse.textContent = peakA.toFixed(1) + " A";
        var totalFusePct = Math.min(100, (peakA / maxAmps) * 100);
        fuseFill.style.width = totalFusePct + "%";
        fuseFill.style.backgroundColor = fuseFillColor(totalFusePct);
        fuseFill.className = "fuse-fill";
      } else if (fusePhases) {
        // Per-phase boxes: one tile per configured phase, side-by-side.
        if (fusePhases.childElementCount !== phases) {
          fusePhases.innerHTML = "";
          for (var p = 0; p < phases; p++) {
            // Each grid column is a wrapper: the boxed fuse tile on
            // top, the signed-W readout BELOW the box (outside it),
            // smaller font, dimmed. Keeps the box clean and lets the
            // W label sit right under the bar without padding/border
            // around it.
            var col = document.createElement("div");
            col.className = "fuse-phase-col";

            var box = document.createElement("div");
            box.className = "fuse-phase-box";
            var lab = document.createElement("div");
            lab.className = "fuse-phase-label";
            lab.textContent = "L" + (p + 1);
            var v = document.createElement("div");
            v.className = "fuse-phase-val";
            v.textContent = "-- A";
            var bar = document.createElement("div");
            bar.className = "fuse-phase-bar";
            var fill = document.createElement("div");
            fill.className = "fuse-phase-fill";
            bar.appendChild(fill);
            box.appendChild(lab);
            box.appendChild(v);
            box.appendChild(bar);

            var vw = document.createElement("div");
            vw.className = "fuse-phase-w";
            vw.textContent = "-- W";

            col.appendChild(box);
            col.appendChild(vw);
            fusePhases.appendChild(col);
          }
        }
        var cols = fusePhases.querySelectorAll(".fuse-phase-col");
        for (var rb = 0; rb < cols.length; rb++) {
          var rawA = rb < phaseI.length ? phaseI[rb] : 0;
          var rawW = rb < phaseW.length ? phaseW[rb] : 0;
          var magA = Math.abs(rawA);
          var pct = Math.min(100, (magA / maxAmps) * 100);
          var bf = cols[rb].querySelector(".fuse-phase-fill");
          var bv = cols[rb].querySelector(".fuse-phase-val");
          var bvw = cols[rb].querySelector(".fuse-phase-w");
          bf.style.setProperty("--fill-pct", pct + "%");
          bf.style.backgroundColor = (rawA < -0.1) ? "" : fuseFillColor(pct);
          bf.className = "fuse-phase-fill" + (rawA < -0.1 ? " export" : "");
          // Display SIGNED current (operator-requested): the bar still
          // sizes by magnitude so the fuse-fraction is visually honest,
          // but the number shows direction so an exporting phase reads
          // "-7.3 A" rather than the same "7.3 A" as an importing one.
          var signedA = rawA;
          if (Math.abs(signedA) < 0.05) signedA = 0;
          bv.textContent = signedA.toFixed(1) + " A";
          if (bvw) {
            var sw = rawW;
            if (Math.abs(sw) < 1) sw = 0;
            bvw.textContent = Math.round(sw) + " W";
            bvw.classList.toggle("export", rawW < -1);
          }
        }
      }
    }

    // Status bar — driver health summary
    var sbDrivers = document.getElementById("sb-drivers");
    var sbVersion = document.getElementById("sb-version");
    if (sbDrivers && data.drivers) {
      var names = Object.keys(data.drivers);
      var parts = names.map(function (n) {
        var d = data.drivers[n];
        var dot = d.status === "ok" ? "\u25cf" : d.status === "degraded" ? "\u25cb" : "\u2715";
        var cls = d.status === "ok" ? "sb-ok" : d.status === "degraded" ? "sb-warn" : "sb-err";
        return '<span class="' + cls + '">' + dot + " " + n + "</span>";
      });
      sbDrivers.innerHTML = parts.join("  ");
    }
    if (sbVersion && data.version) {
      sbVersion.textContent = data.version;
    }

    // Dispatch targets — keyed by driver name so the driver card can show
    // its commanded target inline alongside the actual battery power.
    var dispatchByDriver = {};
    (data.dispatch || []).forEach(function (d) { dispatchByDriver[d.driver] = d; });

    // Drivers
    renderDrivers(data.drivers || {}, dispatchByDriver);

    // Dispatch
    renderDispatch(data.dispatch || []);

    // Chart push happens at the top of render() for resilience to DOM errors.
    // The rAF loop redraws ~30fps for the smooth flowing feel.
    // Timestamp is updated in fetchStatus (before render, so it's robust to render errors)
  }

  function pushChartData(data, targetsByDriver) {
    var t = (data.energy && data.energy.today) || {};
    var now = Date.now();
    // Dedupe via JS-side push timer ONLY — never compare against server timestamps
    // from chartHistory.timestamps because clock skew between RPi and browser
    // would silently block all pushes if server is even slightly ahead.
    if (lastPushAt > 0 && now - lastPushAt < 800) return;
    lastPushAt = now;

    chartHistory.grid.push(data.grid_w);
    chartHistory.pv.push(data.pv_w);
    chartHistory.load.push(data.load_w || 0);
    chartHistory.timestamps.push(now);
    chartHistory.e_import.push(t.import_wh || 0);
    chartHistory.e_export.push(t.export_wh || 0);
    chartHistory.e_pv.push(t.pv_wh || 0);
    chartHistory.e_charged.push(t.bat_charged_wh || 0);
    chartHistory.e_discharged.push(t.bat_discharged_wh || 0);
    chartHistory.e_load.push(t.load_wh || 0);

    // Per-battery-driver push. Any driver in data.drivers that exposes
    // bat_w is considered battery-capable and gets its own chart series.
    var drivers = data.drivers || {};
    var seenBatteries = {};
    var seenEVs = {};
    Object.keys(drivers).forEach(function (name) {
      var d = drivers[name] || {};
      if (d.bat_w != null) {
        seenBatteries[name] = true;
        var bslot = ensureBatteryDriver(name);
        bslot.bat.push(d.bat_w || 0);
        bslot.target.push((targetsByDriver && targetsByDriver[name]) || 0);
      }
      if (d.ev_w != null) {
        seenEVs[name] = true;
        var eslot = ensureEVDriver(name);
        eslot.ev.push(d.ev_w || 0);
      }
    });
    // Drivers that have history but didn't report this cycle: push a
    // 0 so index alignment with chartHistory.timestamps stays intact.
    // (Keeps the line continuous; the driver offline/gap is already
    // visible through the driver card's status indicator.)
    Object.keys(chartBatteries).forEach(function (name) {
      if (seenBatteries[name]) return;
      var slot = chartBatteries[name];
      slot.bat.push(0);
      slot.target.push(0);
    });
    Object.keys(chartEVs).forEach(function (name) {
      if (seenEVs[name]) return;
      chartEVs[name].ev.push(0);
    });

    if (chartHistory.grid.length > CHART_POINTS) {
      Object.keys(chartHistory).forEach(function(k) { chartHistory[k].shift(); });
      Object.keys(chartBatteries).forEach(function (name) {
        var slot = chartBatteries[name];
        slot.bat.shift();
        slot.target.shift();
      });
      Object.keys(chartEVs).forEach(function (name) {
        chartEVs[name].ev.shift();
      });
    }
    lastDataTs = now;
    // Fire a discrete pulse for this new data point — heartbeat feel
    lastFlashAt = now;
  }

  function renderChart() {
    var canvas = document.getElementById("power-chart");
    if (!canvas) return;
    var ctx = canvas.getContext("2d");
    var dpr = window.devicePixelRatio || 1;
    var w = canvas.parentElement.clientWidth - 32;
    var h = 300;
    // Small-screen sizing for canvas-rendered axes + the NOW marker.
    // Canvas text doesn't honour CSS @media queries, so we branch in
    // JS — same threshold as ftw-price-chart (600 px) for consistency.
    // Fonts scale ~50 % up; the now-line stroke doubles. The values
    // below are read by every ctx.font / ctx.lineWidth assignment in
    // this function via the chartFont* / chartLine* variables.
    var smallScreen = window.matchMedia &&
      window.matchMedia("(max-width: 600px)").matches;
    var chartFontAxis     = smallScreen ? "16px monospace" : "11px monospace";
    var chartFontWaiting  = smallScreen ? "18px monospace" : "12px monospace";
    var chartFontTooltip  = smallScreen ? "14px monospace" : "10px monospace";
    var chartFontTooltipS = smallScreen ? "13px monospace" : "9px monospace";
    var chartNowStrokeW   = smallScreen ? 2 : 1;
    if (canvas.width !== w * dpr || canvas.height !== h * dpr) {
      canvas.width = w * dpr;
      canvas.height = h * dpr;
      canvas.style.width = w + "px";
      canvas.style.height = h + "px";
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    }

    var pad = { top: 20, right: 10, bottom: 25, left: 55 };
    var plotW = w - pad.left - pad.right;
    var plotH = h - pad.top - pad.bottom;

    var windowMs = CHART_RANGE_MS[chartRange] || CHART_RANGE_MS["5m"];
    var now = Date.now();
    var windowStart = now - windowMs;
    // Forward-looking forecast: at least 15 minutes so the transition
    // from actual→predicted is smooth. For longer ranges, use half the
    // past window. Keeps the live trace dominant on wide views while
    // giving short views enough runway to show the spline curve.
    var futureMs = Math.max(15 * 60 * 1000, windowMs / 2);
    var windowEnd = now + futureMs;
    var totalMs = windowEnd - windowStart;

    // Build series based on view
    var series;
    if (chartView === "energy") {
      function toKwh(arr) { return arr.map(function(x){ return x / 1000; }); }
      series = [
        { data: toKwh(chartHistory.e_import),     color: "#ef4444", width: 2, dash: [], name: "Import",     fill: true },
        { data: toKwh(chartHistory.e_export),     color: "#22c55e", width: 2, dash: [], name: "Export",     fill: true },
        { data: toKwh(chartHistory.e_pv),         color: "#10b981", width: 2, dash: [], name: "PV",         fill: true },
        { data: toKwh(chartHistory.e_charged),    color: "#3b82f6", width: 2, dash: [], name: "Charged",    fill: false },
        { data: toKwh(chartHistory.e_discharged), color: "#f59e0b", width: 2, dash: [], name: "Discharged", fill: false },
        { data: toKwh(chartHistory.e_load),       color: "#e2e8f0", width: 2, dash: [], name: "Load",       fill: false },
      ];
    } else {
      series = [
        { data: chartHistory.grid, color: "#ef4444", width: 2,   dash: [], name: "Grid", fill: true,  toggle: "grid" },
        { data: chartHistory.pv,   color: "#22c55e", width: 2,   dash: [], name: "PV",   fill: true,  toggle: "pv" },
        { data: chartHistory.load, color: "#e2e8f0", width: 1.5, dash: [], name: "Load", fill: false, toggle: "load" },
      ];
      // Append one actual/target pair per discovered battery driver.
      // Stable order so chart colors don't jump as the driver set grows.
      Object.keys(chartBatteries).sort().forEach(function (name) {
        var slot = chartBatteries[name];
        var color = batteryColor(name);
        var toggle = "bat:" + name;
        var label = batteryLabel(name);
        series.push({ data: slot.bat,    color: color, width: 2,   dash: [],     name: label,         fill: false, toggle: toggle });
        series.push({ data: slot.target, color: color, width: 1.5, dash: [6, 4], name: label + " tgt", fill: false, toggle: toggle });
      });
      // Append one line per EV charger. EV power is always ≥ 0 (pure
      // consumer — never exports), so a single actual-value line is
      // enough. No target dashed line like batteries have, because the
      // charger's "command" is just a current setpoint the driver sends
      // to the wallbox, not a separate power target worth charting.
      Object.keys(chartEVs).sort().forEach(function (name) {
        var slot = chartEVs[name];
        var color = evColor(name);
        var toggle = "ev:" + name;
        var label = evLabel(name);
        series.push({ data: slot.ev, color: color, width: 2, dash: [], name: label, fill: false, toggle: toggle });
      });
      // Respect click-to-hide from legend.
      series = series.filter(function (s) { return !legendHidden[s.toggle]; });
    }

    // Y range only across points within the visible time window
    var visibleVals = [];
    for (var k = 0; k < chartHistory.timestamps.length; k++) {
      if (chartHistory.timestamps[k] >= windowStart) {
        for (var s = 0; s < series.length; s++) {
          if (series[s].data[k] != null) visibleVals.push(series[s].data[k]);
        }
      }
    }
    // Forecast values are intentionally NOT included in the y-range.
    // Including them made the live segment feel cramped whenever a
    // future slot predicted extreme power. Instead, forecasts are
    // clipped to the actual-data plot rect (see ctx.clip above).
    if (visibleVals.length === 0) {
      // Empty state — draw axes + "waiting for data" hint
      ctx.clearRect(0, 0, w, h);
      ctx.fillStyle = "#666";
      ctx.font = chartFontWaiting;
      ctx.textAlign = "center";
      ctx.fillText("waiting for data...", w / 2, h / 2);
      ctx.textAlign = "left";
      return;
    }
    var yMin = Math.min(0, Math.min.apply(null, visibleVals));
    var yMax = Math.max(100, Math.max.apply(null, visibleVals));
    var yRange = yMax - yMin || 1;
    yMin -= yRange * 0.1;
    yMax += yRange * 0.1;
    yRange = yMax - yMin;

    // Smooth y-range transitions to avoid jarring re-scales
    if (chartLayout && chartLayout.yMin != null) {
      var lerp = 0.18; // tighter = snappier; looser = smoother
      yMin = chartLayout.yMin + (yMin - chartLayout.yMin) * lerp;
      yMax = chartLayout.yMax + (yMax - chartLayout.yMax) * lerp;
      yRange = yMax - yMin;
    }

    // Snap axis to "nice" round numbers so gridlines carry readable
    // labels ("0 W", "1.0 kW") instead of the raw fractional tick value
    // that the lerp produces mid-animation ("6 W", "1.04 kW").
    var nice = niceAxis(yMin, yMax, 5);
    yMin = nice.min; yMax = nice.max; yRange = yMax - yMin;
    var yStep = nice.step;

    ctx.clearRect(0, 0, w, h);

    // Clip to plot area so flowing lines don't draw over the y-axis labels
    ctx.save();
    ctx.beginPath();
    ctx.rect(pad.left, pad.top, plotW, plotH);
    ctx.clip();

    // Shaded band behind the forecast (future) portion of the chart so
    // it's immediately obvious what's measured vs predicted.
    var xNowShade = pad.left + plotW * (now - windowStart) / totalMs;
    if (xNowShade < pad.left + plotW) {
      ctx.fillStyle = "rgba(251,191,36,0.06)"; // warm amber
      ctx.fillRect(xNowShade, pad.top, pad.left + plotW - xNowShade, plotH);
    }

    // Grid lines (drawn inside clip so they only appear in the plot area).
    // Walk yMin..yMax in yStep increments so every line lands on a round
    // number — that's what lets the y-axis labels stay readable.
    ctx.strokeStyle = "#2a2a2a";
    ctx.lineWidth = 0.5;
    ctx.font = chartFontAxis;
    var steps = Math.round(yRange / yStep);
    for (var i = 0; i <= steps; i++) {
      var y = pad.top + plotH - (plotH * i / steps);
      ctx.beginPath();
      ctx.moveTo(pad.left, y);
      ctx.lineTo(w - pad.right, y);
      ctx.stroke();
    }

    // Zero line
    if (yMin < 0 && yMax > 0) {
      var zeroY = pad.top + plotH * (1 - (0 - yMin) / yRange);
      ctx.strokeStyle = "#444";
      ctx.lineWidth = 1;
      ctx.setLineDash([4, 4]);
      ctx.beginPath();
      ctx.moveTo(pad.left, zeroY);
      ctx.lineTo(w - pad.right, zeroY);
      ctx.stroke();
      ctx.setLineDash([]);
    }

    // Map ts → x. Spans the whole chart including the future segment.
    function tsToX(ts) {
      return pad.left + plotW * (ts - windowStart) / totalMs;
    }
    function valToY(v) {
      return pad.top + plotH * (1 - (v - yMin) / yRange);
    }

    // Collect latest-point coordinates per series so we can draw the live
    // pulses OUTSIDE the clip rect (otherwise they get cut off at the right edge)
    var liveTips = [];

    // Draw each series. Fill area under prominent ones with subtle gradient.
    series.forEach(function (sr) {
      if (sr.data.length < 2 || chartHistory.timestamps.length < 2) return;

      var pts = [];
      for (var j = 0; j < sr.data.length; j++) {
        var ts = chartHistory.timestamps[j];
        if (sr.data[j] == null) continue;
        pts.push({ x: tsToX(ts), y: valToY(sr.data[j]) });
      }
      if (pts.length < 2) return;

      if (sr.fill) {
        var grad = ctx.createLinearGradient(0, pad.top, 0, pad.top + plotH);
        grad.addColorStop(0, hexAlpha(sr.color, 0.22));
        grad.addColorStop(1, hexAlpha(sr.color, 0.0));
        ctx.fillStyle = grad;
        ctx.beginPath();
        ctx.moveTo(pts[0].x, pad.top + plotH);
        for (var p = 0; p < pts.length; p++) ctx.lineTo(pts[p].x, pts[p].y);
        ctx.lineTo(pts[pts.length - 1].x, pad.top + plotH);
        ctx.closePath();
        ctx.fill();
      }

      ctx.strokeStyle = sr.color;
      ctx.lineWidth = sr.width;
      ctx.lineJoin = "round";
      // butt caps on dashed lines so each dash renders crisp; round caps
      // fill the gap between dashes and look like a solid line.
      ctx.lineCap = (sr.dash && sr.dash.length) ? "butt" : "round";
      ctx.setLineDash(sr.dash || []);
      ctx.beginPath();
      ctx.moveTo(pts[0].x, pts[0].y);
      for (var p2 = 1; p2 < pts.length; p2++) ctx.lineTo(pts[p2].x, pts[p2].y);
      ctx.stroke();
      ctx.setLineDash([]);

      // Cache the right-most point for un-clipped pulse drawing
      if (sr.width >= 2) {
        liveTips.push({ x: pts[pts.length - 1].x, y: pts[pts.length - 1].y, color: sr.color });
      }
    });

    // ---- Forward-looking forecast (dashed lines from plan) ----
    // Simple linear segments through slot midpoints. Anchored at the
    // current actual value so the transition is continuous.
    if (chartView === "power" && chartPlan && chartPlan.actions) {
      var lastIdx = chartHistory.timestamps.length - 1;
      var lastActualPV = lastIdx >= 0 ? chartHistory.pv[lastIdx] : null;
      var lastActualLoad = lastIdx >= 0 ? chartHistory.load[lastIdx] : null;

      var drawForecast = function (field, color, lastActual) {
        var pts = [];
        if (lastActual != null) {
          pts.push({ x: tsToX(now), y: valToY(lastActual) });
        }
        for (var i = 0; i < chartPlan.actions.length; i++) {
          var a = chartPlan.actions[i];
          var aEnd = a.slot_start_ms + a.slot_len_min * 60000;
          if (aEnd < now) continue;
          if (a.slot_start_ms > windowEnd) break;
          var midMs = (a.slot_start_ms + aEnd) / 2;
          if (midMs < now) midMs = (now + aEnd) / 2;
          pts.push({ x: tsToX(midMs), y: valToY(a[field]) });
        }
        if (pts.length < 2) return;
        ctx.strokeStyle = color;
        ctx.lineWidth = 2;
        ctx.setLineDash([6, 4]);
        ctx.beginPath();
        ctx.moveTo(pts[0].x, pts[0].y);
        for (var j = 1; j < pts.length; j++) ctx.lineTo(pts[j].x, pts[j].y);
        ctx.stroke();
        ctx.setLineDash([]);
      };
      if (!legendHidden.pv_fc)   drawForecast("pv_w",   "#86efac", lastActualPV);
      if (!legendHidden.load_fc) drawForecast("load_w", "#fde68a", lastActualLoad);
    }

    // ---- Now-line separator (between past actuals and future forecast) ----
    var xNow = tsToX(now);
    ctx.strokeStyle = "rgba(251,191,36,0.75)";
    ctx.lineWidth = 1.2;
    ctx.setLineDash([4, 4]);
    ctx.beginPath();
    ctx.moveTo(xNow, pad.top);
    ctx.lineTo(xNow, pad.top + plotH);
    ctx.stroke();
    ctx.setLineDash([]);
    // No in-canvas "now" / "predicted →" labels — the amber shaded
    // band + dashed vertical divider already communicate the boundary,
    // and the "now" text on the x-axis keeps the anchor obvious.

    ctx.restore();

    // ---- Live pulses (drawn outside clip so they're never cut off) ----
    // ONE discrete ripple per new data point — no continuous breathing.
    // Each new push (lastFlashAt) triggers an 800ms expanding ring + flash.
    var FLASH_MS = 800;
    var sinceFlash = lastFlashAt > 0 ? (now - lastFlashAt) : Infinity;
    var flashActive = sinceFlash < FLASH_MS;
    var flashProgress = flashActive ? (sinceFlash / FLASH_MS) : 1; // 0..1
    var rippleR = 5 + flashProgress * 32;
    var rippleAlpha = flashActive ? (1 - flashProgress) * 0.85 : 0;
    // Brief brightness boost on the dot itself for the first 200ms
    var dotBoost = sinceFlash < 200 ? (1 - sinceFlash / 200) : 0;

    liveTips.forEach(function (tip) {
      // Static halo — soft, always visible (NOT breathing)
      ctx.fillStyle = tip.color;
      ctx.globalAlpha = 0.18;
      ctx.beginPath();
      ctx.arc(tip.x, tip.y, 7, 0, Math.PI * 2);
      ctx.fill();

      // Solid core dot
      ctx.globalAlpha = 1;
      ctx.beginPath();
      ctx.arc(tip.x, tip.y, 3 + dotBoost * 1.5, 0, Math.PI * 2);
      ctx.fill();

      // White center for crispness
      ctx.fillStyle = "#fff";
      ctx.globalAlpha = 0.7 + dotBoost * 0.3;
      ctx.beginPath();
      ctx.arc(tip.x, tip.y, 1.2 + dotBoost, 0, Math.PI * 2);
      ctx.fill();

      // Ripple — fires once per new data point, expands and fades
      if (flashActive) {
        ctx.strokeStyle = tip.color;
        ctx.globalAlpha = rippleAlpha;
        ctx.lineWidth = 2;
        ctx.beginPath();
        ctx.arc(tip.x, tip.y, rippleR, 0, Math.PI * 2);
        ctx.stroke();
      }
    });
    ctx.globalAlpha = 1;

    // Subtle vertical "now" line — anchor point so the eye knows
    // where present is. Stroke width bumps on small screens so the
    // marker stays visible alongside the larger axis labels.
    var nowX = pad.left + plotW;
    ctx.strokeStyle = smallScreen ? "rgba(255,255,255,0.30)" : "rgba(255,255,255,0.12)";
    ctx.lineWidth = chartNowStrokeW;
    ctx.beginPath();
    ctx.moveTo(nowX, pad.top);
    ctx.lineTo(nowX, pad.top + plotH);
    ctx.stroke();

    // Y-axis labels (outside clip so they're fully visible)
    ctx.fillStyle = "#888";
    ctx.font = chartFontAxis;
    for (var i2 = 0; i2 <= steps; i2++) {
      var yVal = yMin + (yRange * i2 / steps);
      var ly = pad.top + plotH - (plotH * i2 / steps);
      ctx.fillText(chartView === "energy" ? yVal.toFixed(1) + " kWh" : formatW(yVal), 2, ly + 4);
    }

    // Time labels
    ctx.fillStyle = "#666";
    ctx.fillText(chartRange + " ago", pad.left, h - 5);
    ctx.textAlign = "right";
    ctx.fillText("now", w - pad.right, h - 5);
    ctx.textAlign = "left";

    // Live freshness indicator — top-right corner of plot area
    if (lastDataTs > 0) {
      var ageMs = now - lastDataTs;
      var ageStr;
      if (ageMs < 1500) ageStr = "live";
      else if (ageMs < 60_000) ageStr = Math.round(ageMs / 1000) + "s ago";
      else ageStr = Math.round(ageMs / 60_000) + "m ago";
      var fresh = ageMs < 5000;
      // Dot flashes briefly when fresh data lands (discrete, not breathing)
      var dotFlash = sinceFlash < 400 ? (1 - sinceFlash / 400) : 0;
      ctx.fillStyle = fresh ? "#22c55e" : "#f59e0b";
      ctx.globalAlpha = 0.35 + dotFlash * 0.5;
      ctx.beginPath();
      ctx.arc(w - pad.right - 78, pad.top + 4, 3.5 + dotFlash * 3, 0, Math.PI * 2);
      ctx.fill();
      ctx.globalAlpha = 1;
      ctx.beginPath();
      ctx.arc(w - pad.right - 78, pad.top + 4, 2.5, 0, Math.PI * 2);
      ctx.fill();
      ctx.font = chartFontTooltip;
      ctx.fillStyle = fresh ? "#aaa" : "#f59e0b";
      ctx.fillText(ageStr, w - pad.right - 70, pad.top + 8);
    }

    // Store layout for hover tooltip + animation loop
    chartLayout = {
      pad: pad, plotW: plotW, plotH: plotH, w: w, h: h,
      yMin: yMin, yMax: yMax, yRange: yRange,
      windowStart: windowStart, windowMs: windowMs,
      windowEnd: windowEnd, totalMs: totalMs, now: now,
      plan: chartPlan,
      series: series,
      pointCount: chartHistory.timestamps.length
    };

    if (hoverIndex >= 0 && hoverIndex < chartLayout.pointCount) {
      drawHoverOverlay(ctx);
    } else if (hoverForecast) {
      drawForecastHoverOverlay(ctx);
    }
  }

  // hex like "#ef4444" → "rgba(239,68,68,a)"
  function hexAlpha(hex, alpha) {
    var h = hex.replace("#", "");
    if (h.length === 3) h = h[0]+h[0]+h[1]+h[1]+h[2]+h[2];
    var r = parseInt(h.substr(0,2), 16);
    var g = parseInt(h.substr(2,2), 16);
    var b = parseInt(h.substr(4,2), 16);
    return "rgba(" + r + "," + g + "," + b + "," + alpha + ")";
  }

  function drawHoverOverlay(ctx) {
    if (!chartLayout) return;
    var l = chartLayout;
    var i = hoverIndex;
    // Map by timestamp (matches the time-anchored line drawing)
    var ts = chartHistory.timestamps[i];
    if (ts == null) return;
    var x = l.pad.left + l.plotW * (ts - l.windowStart) / l.totalMs;

    // Vertical line
    ctx.strokeStyle = "rgba(255,255,255,0.3)";
    ctx.lineWidth = 1;
    ctx.setLineDash([2, 2]);
    ctx.beginPath();
    ctx.moveTo(x, l.pad.top);
    ctx.lineTo(x, l.pad.top + l.plotH);
    ctx.stroke();
    ctx.setLineDash([]);

    // Dots on each series at this x
    l.series.forEach(function (s) {
      if (i >= s.data.length) return;
      var y = l.pad.top + l.plotH * (1 - (s.data[i] - l.yMin) / l.yRange);
      ctx.fillStyle = s.color;
      ctx.beginPath();
      ctx.arc(x, y, 3, 0, Math.PI * 2);
      ctx.fill();
    });

    // Tooltip box — labels match current view
    var labels = chartView === "energy" ? [
      { name: "Import",     data: chartHistory.e_import,     color: "#ef4444" },
      { name: "Export",     data: chartHistory.e_export,     color: "#22c55e" },
      { name: "PV",         data: chartHistory.e_pv,         color: "#10b981" },
      { name: "Charged",    data: chartHistory.e_charged,    color: "#3b82f6" },
      { name: "Discharged", data: chartHistory.e_discharged, color: "#f59e0b" },
      { name: "Load",       data: chartHistory.e_load,       color: "#e2e8f0" },
    ] : (function () {
      var rows = [
        { name: "Grid", data: chartHistory.grid, color: "#ef4444" },
        { name: "PV",   data: chartHistory.pv,   color: "#22c55e" },
        { name: "Load", data: chartHistory.load, color: "#e2e8f0" },
      ];
      // Battery rows render their target inline as "actual W (→ target W)"
      // so it's visually obvious the two numbers are the same metric — one
      // measured, one commanded. See value formatter below.
      Object.keys(chartBatteries).sort().forEach(function (name) {
        var slot = chartBatteries[name];
        rows.push({ name: batteryLabel(name), data: slot.bat, color: batteryColor(name), target: slot.target });
      });
      Object.keys(chartEVs).sort().forEach(function (name) {
        var slot = chartEVs[name];
        rows.push({ name: evLabel(name), data: slot.ev, color: evColor(name) });
      });
      return rows;
    })();

    var ts = chartHistory.timestamps[i] || 0;
    var timeStr = ts > 0 ? new Date(ts).toLocaleTimeString() : "";
    var lineHeight = 16;
    var boxW = 200;
    var boxH = (labels.length + 1) * lineHeight + 10;

    // Position tooltip (avoid going off-screen)
    var boxX = x + 10;
    if (boxX + boxW > l.w - 5) boxX = x - boxW - 10;
    var boxY = l.pad.top + 5;

    ctx.fillStyle = "rgba(20,20,35,0.95)";
    ctx.strokeStyle = "#444";
    ctx.lineWidth = 1;
    ctx.fillRect(boxX, boxY, boxW, boxH);
    ctx.strokeRect(boxX, boxY, boxW, boxH);

    ctx.font = chartFontTooltip;
    ctx.fillStyle = "#888";
    ctx.fillText(timeStr, boxX + 6, boxY + lineHeight - 2);

    labels.forEach(function (lab, idx) {
      if (i >= lab.data.length) return;
      var y = boxY + (idx + 2) * lineHeight - 4;
      ctx.fillStyle = lab.color;
      ctx.fillRect(boxX + 6, y - 8, 8, 8);
      ctx.fillStyle = lab.dim ? "#888" : "#ddd";
      ctx.fillText(lab.name, boxX + 18, y);
      ctx.textAlign = "right";
      if (chartView === "energy") {
        ctx.fillStyle = "#fff";
        ctx.fillText(lab.data[i].toFixed(2) + " kWh", boxX + boxW - 6, y);
      } else {
        var actual = formatW(lab.data[i]);
        ctx.fillStyle = "#fff";
        ctx.fillText(actual, boxX + boxW - 6, y);
        // Inline target as dim "(→ -674 W)" so user sees commanded vs actual
        // in one glance. Skip when target is 0 to reduce visual noise.
        if (lab.target && i < lab.target.length && Math.abs(lab.target[i]) > 1) {
          var actualW = ctx.measureText(actual).width;
          ctx.fillStyle = "#888";
          ctx.font = chartFontTooltipS;
          ctx.fillText("→ " + formatW(lab.target[i]), boxX + boxW - 10 - actualW, y);
          ctx.font = chartFontTooltip;
        }
      }
      ctx.textAlign = "left";
    });
  }

  function drawForecastHoverOverlay(ctx) {
    if (!chartLayout || !hoverForecast) return;
    var l = chartLayout;
    var a = hoverForecast.action;
    var ts = hoverForecast.ts;
    var x = l.pad.left + l.plotW * (ts - l.windowStart) / l.totalMs;

    // Vertical line
    ctx.strokeStyle = "rgba(251,191,36,0.4)";
    ctx.lineWidth = 1;
    ctx.setLineDash([2, 2]);
    ctx.beginPath();
    ctx.moveTo(x, l.pad.top);
    ctx.lineTo(x, l.pad.top + l.plotH);
    ctx.stroke();
    ctx.setLineDash([]);

    // Tooltip box for forecast values
    var labels = [
      { name: "PV pred",   val: -a.pv_w,  color: "#86efac" },
      { name: "Load pred", val: a.load_w, color: "#fde68a" },
      { name: "Battery",   val: a.battery_w, color: "#f59e0b", showSign: true },
      { name: "Grid",      val: a.grid_w,    color: "#ef4444", showSign: true },
      { name: "SoC",       val: a.soc_pct + "%", color: "#60a5fa", literal: true },
      { name: "Price",     val: a.price_ore.toFixed(0) + " öre/kWh", color: "#fbbf24", literal: true },
    ];

    var lineHeight = 16;
    var boxW = 200;
    var boxH = (labels.length + 2) * lineHeight + 14;
    var boxX = x + 10;
    if (boxX + boxW > l.w - 5) boxX = x - boxW - 10;
    var boxY = l.pad.top + 5;

    ctx.fillStyle = "rgba(20,20,35,0.96)";
    ctx.strokeStyle = "rgba(251,191,36,0.6)";
    ctx.lineWidth = 1;
    ctx.fillRect(boxX, boxY, boxW, boxH);
    ctx.strokeRect(boxX, boxY, boxW, boxH);

    ctx.font = chartFontTooltip;
    var d = new Date(ts);
    var hh = d.getHours().toString().padStart(2, "0") + ":" + d.getMinutes().toString().padStart(2, "0");
    ctx.fillStyle = "#fbbf24";
    ctx.fillText(hh + "  predicted", boxX + 6, boxY + lineHeight - 2);

    labels.forEach(function (lab, idx) {
      var y = boxY + (idx + 2) * lineHeight - 4;
      ctx.fillStyle = lab.color;
      ctx.fillRect(boxX + 6, y - 8, 8, 8);
      ctx.fillStyle = "#ddd";
      ctx.fillText(lab.name, boxX + 18, y);
      ctx.fillStyle = "#fff";
      ctx.textAlign = "right";
      var val = lab.literal ? lab.val : formatW(lab.val);
      ctx.fillText(val, boxX + boxW - 6, y);
      ctx.textAlign = "left";
    });

    if (a.reason) {
      var ry = boxY + (labels.length + 2) * lineHeight + 2;
      ctx.fillStyle = "#86efac";
      ctx.font = "italic 10px monospace";
      // Truncate if too long for box
      var reason = a.reason.length > 28 ? a.reason.substring(0, 27) + "…" : a.reason;
      ctx.fillText(reason, boxX + 6, ry);
    }
  }

  function driverLifecycleCall(name, action) {
    return fetch("/api/drivers/" + encodeURIComponent(name) + "/" + action, { method: "POST" })
      .then(function (res) {
        if (!res.ok) return res.text().then(function (t) { throw new Error(t || ("HTTP " + res.status)); });
        return res.json();
      });
  }

  function renderDriverActions(name, d) {
    // Buttons per driver: Restart (if running), Disable (if running),
    // Enable (if disabled). Small, unobtrusive; rely on the existing
    // .btn-send style from index.html.
    var isDisabled = d.disabled === true || d.status === "disabled";
    var actions = '<div class="driver-actions" style="margin-top:6px;display:flex;gap:6px;flex-wrap:wrap">';
    actions += '<button class="btn-send" data-drv-action="diagnose" data-drv="' + escHtml(name) + '">Diagnose</button>';
    if (isDisabled) {
      actions += '<button class="btn-send" data-drv-action="enable" data-drv="' + escHtml(name) + '">Enable</button>';
    } else {
      actions += '<button class="btn-send" data-drv-action="restart" data-drv="' + escHtml(name) + '">Restart</button>';
      actions += '<button class="btn-send" data-drv-action="disable" data-drv="' + escHtml(name) + '" style="opacity:0.75">Disable</button>';
    }
    actions += "</div>";
    return actions;
  }

  // Event delegation — one listener for all driver-action buttons. Saves
  // re-binding on every re-render.
  if (driversGrid) {
    driversGrid.addEventListener("click", function (ev) {
      var btn = ev.target.closest("[data-drv-action]");
      if (!btn) return;
      var name = btn.getAttribute("data-drv");
      var action = btn.getAttribute("data-drv-action");
      if (!name || !action) return;
      // Diagnose is a UI-only action: no API mutation, just open the
      // modal and let it poll /api/drivers/{name} on its own cadence.
      if (action === "diagnose") {
        if (window.FTWDiagnostics) window.FTWDiagnostics.open(name);
        return;
      }
      if (action === "disable" && !window.confirm("Disable driver \"" + name + "\"? It will be stopped and won't auto-start until re-enabled.")) return;
      btn.disabled = true;
      btn.textContent = action + "ing…";
      driverLifecycleCall(name, action)
        .then(function () { fetchStatus(); })
        .catch(function (err) { alert("Driver " + action + " failed: " + err.message); })
        .finally(function () { btn.disabled = false; });
    });
  }

  function renderDrivers(drivers, dispatchByDriver) {
    driversGrid.innerHTML = "";
    var names = Object.keys(drivers).sort();
    names.forEach(function (name) {
      var d = drivers[name];
      var card = document.createElement("div");
      card.className = "driver-card";
      if (d.disabled === true || d.status === "disabled") card.className += " driver-card-disabled";
      if (d.not_running === true) card.className += " driver-card-warn";

      var ticks = d.tick_count != null ? d.tick_count : 0;
      var errors = d.consecutive_errors != null ? d.consecutive_errors : 0;

      // Detect driver kind from telemetry shape. Vehicle drivers
      // (e.g. tesla_vehicle) emit DerVehicle which carries SoC +
      // charge_limit + charging_state but no power; render a vehicle-
      // specific body. EV chargers emit DerEV with power. Anything
      // else falls through to the legacy meter/pv/battery layout.
      var isVehicle = (d.vehicle_soc != null || d.vehicle_charge_limit_pct != null);
      var isEV = !isVehicle && (d.ev_w != null || d.ev_connected != null || d.ev_charging != null);

      var body;
      if (isVehicle) {
        var vSoc = d.vehicle_soc != null ? Math.round(d.vehicle_soc) : null;
        var vLimit = d.vehicle_charge_limit_pct != null ? Math.round(d.vehicle_charge_limit_pct) : null;
        var vState = d.vehicle_charging_state || "—";
        var vTtf = d.vehicle_time_to_full_min;
        var vStale = !!d.vehicle_stale;
        var vAmps = d.vehicle_charge_amps;             // car's in-app current limit
        var vActual = d.vehicle_charger_actual_current; // current actually flowing
        var socDisplay = (vSoc != null && vLimit != null)
          ? vSoc + " / " + vLimit + " %"
          : (vSoc != null ? vSoc + " %" : "—");
        if (vStale) socDisplay = "⚠ " + socDisplay;
        var stateClassV = (vState === "Charging") ? "stat-ok" : (vState === "Disconnected" ? "stat-dim" : "stat-warn");
        var ttfStr = (vTtf != null && vTtf > 0)
          ? (vTtf >= 60 ? Math.floor(vTtf / 60) + "h " + (vTtf % 60) + "m" : vTtf + " min")
          : "—";
        // Amps row reads "5 / 16 A" (actual / in-app limit) when both
        // present; flags as warn when actual lags the limit (vehicle
        // throttled itself or wallbox limit is lower than what the
        // car would accept). Skipped entirely when neither field is
        // reported (older proxies).
        var ampsRow = "";
        if (vAmps != null || vActual != null) {
          var ampsText = (vActual != null ? Math.round(vActual) : "?") +
                         " / " +
                         (vAmps != null ? Math.round(vAmps) : "?") +
                         " A";
          var ampsCls = (vActual != null && vAmps != null && vActual + 0.5 < vAmps)
            ? "stat-warn" : "stat-value";
          ampsRow =
            '  <span class="stat-label">Amps (actual/limit)</span>' +
            '<span class="stat-value ' + ampsCls + '">' + ampsText + '</span>';
        }
        body =
          '<div class="driver-stats">' +
          '  <span class="stat-label">SoC</span><span class="stat-value">' + socDisplay + '</span>' +
          '  <span class="stat-label">State</span><span class="stat-value ' + stateClassV + '">' + escHtml(vState) + '</span>' +
          ampsRow +
          '  <span class="stat-label">Time to full</span><span class="stat-value">' + ttfStr + '</span>' +
          (vStale ? '  <span class="stat-label">Note</span><span class="stat-value stat-warn">data stale</span>' : '') +
          '  <span class="stat-label">Ticks</span><span class="stat-value">' + ticks + '</span>' +
          '  <span class="stat-label">Errors</span><span class="stat-value">' + errors + '</span>' +
          '</div>' +
          (vSoc != null
            ? '<div class="driver-soc-bar"><div class="driver-soc-fill" style="width:' + vSoc + '%"></div></div>'
            : '');
      } else if (isEV) {
        var evWVal = d.ev_w != null ? d.ev_w : 0;
        // state_label + reason_no_current_label come from the driver —
        // UI renders them verbatim. Protocol knowledge stays in Lua.
        var opLabel = d.ev_state_label
          || (d.ev_charging ? "charging" : (d.ev_connected ? "connected" : "idle"));
        var stateClass =
          (d.ev_charging ? "stat-ok"
          : (d.ev_connected ? "stat-warn" : "stat-dim"));
        var sessionKwh = d.ev_session_wh != null ? (d.ev_session_wh / 1000).toFixed(2) + " kWh" : "—";
        var maxA = d.ev_max_a != null ? d.ev_max_a.toFixed(0) + " A" : "—";
        var reason = d.ev_reason_no_current_label || null;

        body =
          '<div class="driver-stats">' +
          '  <span class="stat-label">State</span><span class="stat-value ' + stateClass + '">' + escHtml(opLabel) + '</span>' +
          '  <span class="stat-label">Power</span><span class="stat-value">' + formatW(evWVal) + '</span>' +
          '  <span class="stat-label">Session</span><span class="stat-value">' + sessionKwh + '</span>' +
          '  <span class="stat-label">Max current</span><span class="stat-value">' + maxA + '</span>' +
          (reason
            ? '  <span class="stat-label">Reason</span><span class="stat-value stat-warn">' + escHtml(reason) + '</span>'
            : '') +
          (d.ev_cable_locked === false && d.ev_connected
            ? '  <span class="stat-label">Cable</span><span class="stat-value stat-warn">unlocked</span>'
            : '') +
          (d.ev_is_online === false
            ? '  <span class="stat-label">Cloud</span><span class="stat-value stat-warn">offline</span>'
            : '') +
          '  <span class="stat-label">Ticks</span><span class="stat-value">' + ticks + '</span>' +
          '  <span class="stat-label">Errors</span><span class="stat-value">' + errors + '</span>' +
          '</div>';
      } else {
        var meterW = d.meter_w != null ? d.meter_w : 0;
        var pvWVal = d.pv_w != null ? d.pv_w : 0;
        var batWVal = d.bat_w != null ? d.bat_w : 0;
        var batSocVal = d.bat_soc != null ? d.bat_soc : 0;

        // Battery target + tracking deviation. Skip if no dispatch (planner
        // hasn't run) OR this driver has no battery (target meaningless).
        var batteryRow =
          '  <span class="stat-label">Battery</span><span class="stat-value">' + formatW(batWVal) + "</span>";
        var disp = (dispatchByDriver || {})[name];
        if (disp && d.bat_w != null) {
          var dev = batWVal - disp.target_w;
          var devClass = Math.abs(dev) > 200 ? "stat-warn" : "stat-dim";
          batteryRow =
            '  <span class="stat-label">Battery</span><span class="stat-value">' + formatW(batWVal) +
            '    <span class="stat-target">→ ' + formatW(disp.target_w) + '</span>' +
            '    <span class="' + devClass + '">Δ ' + formatW(dev) + '</span>' +
            "</span>";
        }

        body =
          '<div class="driver-stats">' +
          '  <span class="stat-label">Meter</span><span class="stat-value">' + formatW(meterW) + "</span>" +
          '  <span class="stat-label">PV</span><span class="stat-value">' + formatW(-pvWVal) + "</span>" +
          batteryRow +
          '  <span class="stat-label">SoC</span><span class="stat-value">' + formatSoc(batSocVal) + "</span>" +
          '  <span class="stat-label">Ticks</span><span class="stat-value">' + ticks + "</span>" +
          '  <span class="stat-label">Errors</span><span class="stat-value">' + errors + "</span>" +
          "</div>" +
          '<div class="driver-soc-bar"><div class="driver-soc-fill" style="width:' + Math.round(batSocVal * 100) + '%"></div></div>';
      }

      // For disabled drivers the body is minimal — just show the label.
      if (d.disabled === true || d.status === "disabled") {
        body =
          '<div class="driver-stats">' +
          '  <span class="stat-label">State</span><span class="stat-value stat-dim">disabled</span>' +
          '</div>';
      } else if (d.not_running === true || (d.status === "offline" && d.tick_count == null)) {
        // Configured in yaml but never successfully spawned — most often
        // a cloud auth failure. Offer Restart to retry with fresh creds.
        body =
          '<div class="driver-stats">' +
          '  <span class="stat-label">State</span><span class="stat-value stat-warn">not running (spawn failed)</span>' +
          '  <span class="stat-label">Hint</span><span class="stat-value stat-dim">check credentials, then restart</span>' +
          '</div>';
      }

      card.innerHTML =
        '<div class="driver-header">' +
        '  <span class="driver-name">' + escHtml(name) + "</span>" +
        '  <span class="status-dot ' + statusClass(d.status) + '" title="' + escHtml(d.status || "unknown") + '"></span>' +
        "</div>" +
        body +
        renderDriverActions(name, d) +
        // Inline battery model — rendered from models.js's cached payload.
        // Drawing it here in the same pass as the driver card avoids the
        // earlier race where two independent polls fought over the slot.
        (!isEV && !isVehicle && !(d.disabled === true || d.status === "disabled") && window.renderInlineBatteryModel ? window.renderInlineBatteryModel(name) : "");

      driversGrid.appendChild(card);
    });
  }

  function renderDispatch(dispatch) {
    // index.html no longer has #dispatch-list — graceful no-op if missing
    if (!dispatchList) return;
    dispatchList.innerHTML = "";
    dispatch.forEach(function (d) {
      var item = document.createElement("div");
      item.className = "dispatch-item";
      item.innerHTML =
        '<span class="dispatch-driver">' + escHtml(d.driver) + "</span>" +
        "<span>" +
        '<span class="dispatch-target">' + formatW(d.target_w) + "</span>" +
        (d.clamped ? '<span class="dispatch-clamped">CLAMPED</span>' : "") +
        "</span>";
      dispatchList.appendChild(item);
    });
    if (dispatch.length === 0) {
      dispatchList.innerHTML = '<div class="dispatch-item" style="color:var(--text-dim)">No dispatch targets</div>';
    }
  }

  function escHtml(str) {
    var div = document.createElement("div");
    div.textContent = str;
    return div.innerHTML;
  }

  // ---- API ----
  var firstLoad = true;
  var setupBannerShown = false;
  // Loadpoint cache — keyed by driver_name so the EV-planet builder
  // in render() can look up vehicle SoC + charge-limit without a
  // second round of fetches per status tick. Refreshed in parallel
  // with /api/status. `null` until the first fetch lands; missing
  // entries mean "no loadpoint for this driver" and the planet
  // falls back to legacy kW-only rendering.
  var loadpointsByDriver = null;
  function fetchStatus() {
    Promise.all([
      fetch("/api/status").then(function (r) { if (!r.ok) throw new Error("HTTP " + r.status); return r.json(); }),
      fetch("/api/loadpoints").then(function (r) { return r.ok ? r.json() : null; }).catch(function () { return null; }),
    ])
      .then(function (results) {
        var data = results[0];
        var lp = results[1];
        if (lp && Array.isArray(lp.loadpoints)) {
          var idx = {};
          lp.loadpoints.forEach(function (l) {
            if (l && l.driver_name) idx[l.driver_name] = l;
          });
          loadpointsByDriver = idx;
        }
        setConnected(true);
        if (firstLoad) { firstLoad = false; }
        if (setupBannerShown) { hideSetupBanner(); }
        // Always refresh timestamp on successful fetch
        lastUpdate.textContent = "Last update: " + new Date().toLocaleTimeString();
        // Isolate render errors from connection state / timestamp
        try { render(data); }
        catch (e) { console.error("render error:", e); }
        // Show a subtle prompt when no drivers are configured
        try { updateNoDevicesPrompt(data.drivers); }
        catch (e2) { /* silent */ }
      })
      .catch(function (e) {
        console.warn("status fetch failed:", e);
        setConnected(false);
        if (firstLoad) { showSetupBanner(); }
      });
  }

  // ---- Setup banner (bootstrap mode — no config yet) ----
  function showSetupBanner() {
    if (setupBannerShown) return;
    var banner = document.createElement("div");
    banner.id = "setup-banner";
    banner.className = "setup-banner";
    banner.innerHTML = 'No devices configured yet. <a href="/setup">Run the setup wizard &rarr;</a>';
    var main = document.querySelector("main");
    if (main) main.parentNode.insertBefore(banner, main);
    setupBannerShown = true;
  }
  function hideSetupBanner() {
    var el = document.getElementById("setup-banner");
    if (el) el.remove();
    setupBannerShown = false;
  }

  // ---- "Add a device" prompt when drivers object is empty ----
  function updateNoDevicesPrompt(drivers) {
    var existing = document.getElementById("no-devices-prompt");
    var hasDrivers = drivers && typeof drivers === "object" && Object.keys(drivers).length > 0;
    if (hasDrivers) {
      if (existing) existing.remove();
      return;
    }
    if (existing) return; // already showing
    var prompt = document.createElement("div");
    prompt.id = "no-devices-prompt";
    prompt.className = "no-devices-prompt";
    prompt.innerHTML = 'No devices connected. <a href="/setup?step=3">Add a device</a>';
    var cards = document.querySelector(".summary-cards");
    if (cards) cards.parentNode.insertBefore(prompt, cards.nextSibling);
  }

  function setMode(mode) {
    fetch("/api/mode", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ mode: mode }),
    })
      .then(function (res) {
        if (!res.ok) throw new Error("HTTP " + res.status);
        // Immediately poll to reflect change
        fetchStatus();
      })
      .catch(function () {
        setConnected(false);
      });
  }

  // Fire-and-forget wrappers around postJson. postJson itself rethrows
  // so callers that chain .then/.finally (evCommand) behave correctly;
  // here we explicitly mark the rejection handled so the browser
  // doesn't log "Uncaught (in promise)" on every network hiccup.
  // postJson has already console.warn'd the failure.
  function setTarget(w) {
    postJson("/api/target", { grid_target_w: w }).catch(function () {});
  }

  function setPeakLimit(w) {
    postJson("/api/peak_limit", { peak_limit_w: w }).catch(function () {});
  }

  // POST the new hard-rule peak ceiling. 0 = disabled (operator opted
  // out of tariff protection — only the physical fuse applies).
  // Returns the postJson promise so callers can clear dirty + show
  // success on resolution.
  function setPeakImportCeiling(w) {
    return postJson("/api/peak_import_ceiling", { peak_import_ceiling_w: w });
  }

  function setEvCharging(w) {
    postJson("/api/ev_charging", { power_w: w, active: w > 0 }).catch(function () {});
  }

  function setBatteryCoversEV(enabled) {
    postJson("/api/battery_covers_ev", { enabled: !!enabled }).catch(function () {});
  }

  function postJson(url, body) {
    return fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    })
      .then(function (res) {
        if (!res.ok) throw new Error("HTTP " + res.status);
        fetchStatus();
        return res;
      })
      .catch(function (e) {
        console.warn("POST failed:", url, e);
        // Don't flip connection state on POST failures —
        // connection state reflects read polling, not write commands
        throw e;
      });
  }

  function setConnected(ok) {
    if (ok) {
      connStatus.className = "conn-status connected";
      connStatus.title = "Connected";
      // render() will update lastUpdate with timestamp
    } else {
      connStatus.className = "conn-status disconnected";
      connStatus.title = "Disconnected";
      lastUpdate.textContent = "Connection lost";
    }
  }

  // ---- Events ----
  modeButtons.addEventListener("click", function (e) {
    if (e.target.tagName === "BUTTON" && e.target.dataset.mode) {
      setMode(e.target.dataset.mode);
    }
  });
  var primaryButtons = document.getElementById("mode-buttons-primary");
  if (primaryButtons) {
    primaryButtons.addEventListener("click", function (e) {
      if (e.target.tagName === "BUTTON" && e.target.dataset.mode) {
        setMode(e.target.dataset.mode);
      }
    });
  }
  var advBtn = document.getElementById("mode-advanced-btn");
  if (advBtn) {
    advBtn.addEventListener("click", function () {
      var panel = document.getElementById("mode-buttons");
      if (!panel) return;
      var shown = panel.style.display !== "none";
      panel.style.display = shown ? "none" : "flex";
      advBtn.textContent = shown ? "Manual…" : "Hide manual";
    });
  }

  // Grid target slider: dirty on input, Save enabled while dirty,
  // poll skips overwrite while dirty. Mirrors the peak slider pattern.
  if (gridTargetSlider) {
    gridTargetSlider.addEventListener("input", function () {
      gridTargetValue.textContent = formatW(Number(gridTargetSlider.value));
      gridTargetDirty = true;
      if (gridTargetSend) gridTargetSend.disabled = false;
    });
  }
  if (gridTargetSend) {
    gridTargetSend.addEventListener("click", function () {
      const w = Number(gridTargetSlider.value);
      gridTargetSend.disabled = true;
      postJson("/api/target", { grid_target_w: w })
        .then(function () { gridTargetDirty = false; })
        .catch(function () { gridTargetSend.disabled = false; /* keep dirty so user can retry */ });
    });
  }

  // Peak slider: same dirty pattern, plus an enable/disable checkbox.
  // Off → POST 0 (backend reads 0 = disabled). On → POST slider value.
  // The slider is disabled when the toggle is off so the operator can't
  // accidentally drag a dead control.
  if (peakLimitSlider) {
    peakLimitSlider.addEventListener("input", function () {
      const w = Number(peakLimitSlider.value);
      peakLimitValue.textContent = formatW(w);
      peakLimitDirty = true;
      if (peakLimitSend) peakLimitSend.disabled = false;
    });
  }
  if (peakLimitEnableToggle) {
    peakLimitEnableToggle.addEventListener("change", function () {
      const enabled = peakLimitEnableToggle.checked;
      if (peakLimitEnableLabel) peakLimitEnableLabel.textContent = enabled ? "On" : "Off";
      if (peakLimitSlider) peakLimitSlider.disabled = !enabled;
      // Show the value the toggle is about to enable (last known)
      // without committing — the operator still has to press Save
      // unless the toggle is being switched OFF, which is a destructive
      // change worth one extra click confirmation. Wait — the spec
      // calls for the toggle itself to be the on/off control, so flip
      // straight through: post immediately on toggle change.
      const w = enabled ? Number(peakLimitSlider.value) || readLastPeakLimitW() : 0;
      if (enabled && peakLimitSlider) peakLimitSlider.value = w;
      if (peakLimitValue) peakLimitValue.textContent = formatW(w);
      peakLimitEnableToggle.disabled = true;
      setPeakImportCeiling(w)
        .then(function () { peakLimitDirty = false; if (peakLimitSend) peakLimitSend.disabled = true; })
        .catch(function () { /* leave dirty so user can retry */ })
        .finally(function () { peakLimitEnableToggle.disabled = false; });
    });
  }
  if (peakLimitSend) {
    peakLimitSend.addEventListener("click", function () {
      const w = Number(peakLimitSlider.value);
      writeLastPeakLimitW(w);
      peakLimitSend.disabled = true;
      setPeakImportCeiling(w)
        .then(function () { peakLimitDirty = false; })
        .catch(function () { peakLimitSend.disabled = false; /* keep dirty */ });
    });
  }

  if (bceToggle) {
    bceToggle.addEventListener("change", function () {
      if (bceLabel) bceLabel.textContent = bceToggle.checked ? "On" : "Off";
      setBatteryCoversEV(bceToggle.checked);
    });
  }

  // EV detail modal — <ftw-modal> handles ESC / backdrop / close button;
  // we only drive open()/close() and refresh the body on a timer. Opened
  // by clicking an EV planet in the energy-flow hero (no card-ev tile).
  var evModal = document.getElementById("ev-modal");
  var evModalBody = document.getElementById("ev-modal-body");
  var evModalDriver = null; // captured from the planet click; sent on commands
  var energyFlowEl = document.getElementById("energy-flow");

  // Render the EV modal by building DOM nodes (textContent) rather than
  // concatenating strings into innerHTML — d.driver comes from driver
  // config and would otherwise be an XSS vector if the config is edited
  // by a lower-trust user.
  function renderEvStatusTable(d) {
    var status = d.charging ? "Charging" : (d.connected ? "Connected" : "Idle");
    var rows = [
      ["Status", status],
      ["Power", formatW(d.w || 0)],
    ];
    if (d.session_wh != null) rows.push(["Session", (d.session_wh / 1000).toFixed(1) + " kWh"]);
    if (d.driver) rows.push(["Driver", String(d.driver)]);

    var table = document.createElement("table");
    table.style.width = "100%";
    table.style.borderCollapse = "collapse";
    rows.forEach(function (r) {
      var tr = document.createElement("tr");
      var tdLabel = document.createElement("td");
      tdLabel.style.padding = "0.3rem 0";
      tdLabel.style.color = "var(--text-dim)";
      tdLabel.textContent = r[0];
      var tdVal = document.createElement("td");
      tdVal.style.padding = "0.3rem 0";
      tdVal.style.textAlign = "right";
      tdVal.style.fontWeight = "600";
      tdVal.textContent = r[1];
      tr.appendChild(tdLabel);
      tr.appendChild(tdVal);
      table.appendChild(tr);
    });
    return table;
  }

  function setEvModalMessage(text) {
    evModalBody.textContent = "";
    var p = document.createElement("p");
    p.style.color = "var(--text-dim)";
    p.textContent = text;
    evModalBody.appendChild(p);
  }

  // Schedule-control persistence across the modal's auto-refresh tick.
  // refreshEvModal wipes evModalBody every poll; without these the
  // user's in-progress edits to the schedule inputs would be reset
  // every few seconds. The cached element is re-attached as long as
  // the user has touched any field and hasn't yet hit Save / Clear,
  // and the LP id hasn't changed (different planet clicked).
  var schedCacheEl = null;
  var schedCacheLpId = null;
  var schedDirty = false;

  function refreshEvModal() {
    // Pass driver query if known so the backend can scope the response
    // to the clicked planet (multi-EV setups). Falls back to whatever
    // the backend returns when no driver filter is honored.
    var url = "/api/ev/status" + (evModalDriver ? "?driver=" + encodeURIComponent(evModalDriver) : "");
    Promise.all([
      fetch(url).then(function (r) { return r.ok ? r.json() : null; }).catch(function () { return null; }),
      fetch("/api/loadpoints").then(function (r) { return r.ok ? r.json() : null; }).catch(function () { return null; }),
    ]).then(function (results) {
      var d = results[0];
      var lps = results[1];
      if (!d || d.connected === false) {
        setEvModalMessage("No EV charger connected");
        return;
      }
      evModalBody.textContent = "";
      evModalBody.appendChild(renderEvStatusTable(d));
      // Match the modal's driver to a configured loadpoint; the
      // surplus-only control only makes sense when the driver is
      // wired to one. With no driver filter we pick the first
      // plugged-in loadpoint as a best-effort fallback.
      var matched = null;
      if (lps && Array.isArray(lps.loadpoints) && lps.loadpoints.length > 0) {
        if (evModalDriver) {
          for (var i = 0; i < lps.loadpoints.length; i++) {
            if (lps.loadpoints[i].driver_name === evModalDriver) {
              matched = lps.loadpoints[i];
              break;
            }
          }
        }
        if (!matched) {
          for (var j = 0; j < lps.loadpoints.length; j++) {
            if (lps.loadpoints[j].plugged_in) { matched = lps.loadpoints[j]; break; }
          }
        }
      }
      if (matched) {
        if (schedDirty && schedCacheEl && schedCacheLpId === matched.id) {
          // User is mid-edit: re-attach their dirty form instead of
          // rebuilding it from server state and trampling input.
          evModalBody.appendChild(schedCacheEl);
        } else {
          evModalBody.appendChild(buildScheduleControl(matched));
        }
      }
    }).catch(function () {
      setEvModalMessage("Failed to load EV status");
    });
  }

  // buildScheduleControl renders the persistent charging schedule
  // section: target SoC + time (local; converted to UTC for the wire),
  // recurring checkbox, and the bat-SoC surplus-unlock threshold.
  // The backend persists this across restarts (state.config), rolls the
  // deadline forward each day when Recurring is set, and arms the
  // surplus-grab whenever the home battery sits at or above the
  // threshold (with 5 pp release hysteresis).
  function buildScheduleControl(lp) {
    var sched = (lp && lp.schedule) || {};
    // Convert "minutes-of-day-UTC" to a "HH:MM" string in the
    // browser's local zone. The UI shows local time everywhere;
    // we marshal back to UTC minutes on save.
    var hasSched = !!(sched.soc_pct || sched.recurring || sched.surplus_unlock_bat_soc_pct);
    var initLocalHHMM = utcMinsToLocalHHMM(typeof sched.time_of_day_min_utc === "number" ? sched.time_of_day_min_utc : 360);
    var initSoC = typeof sched.soc_pct === "number" && sched.soc_pct > 0 ? sched.soc_pct : 50;
    var initRec = !!sched.recurring;
    var savedUnlock = typeof sched.surplus_unlock_bat_soc_pct === "number" ? sched.surplus_unlock_bat_soc_pct : 0;
    // Surplus on/off is derived from the saved threshold: > 0 ⇒ enabled.
    // The threshold input retains the last-used value (or defaults to 50)
    // so unchecking + re-checking doesn't wipe the user's pick.
    var initSurplus = savedUnlock > 0;
    var initUnlock = savedUnlock > 0 ? savedUnlock : 50;

    var box = document.createElement("div");
    box.style.marginTop = "0.75rem";
    box.style.paddingTop = "0.6rem";
    box.style.borderTop = "1px solid var(--line)";

    var eyebrow = document.createElement("div");
    eyebrow.textContent = "Schedule";
    eyebrow.style.fontFamily = "var(--mono)";
    eyebrow.style.fontSize = "0.7rem";
    eyebrow.style.letterSpacing = "0.18em";
    eyebrow.style.textTransform = "uppercase";
    eyebrow.style.color = "var(--text-dim)";
    eyebrow.style.marginBottom = "0.55rem";
    box.appendChild(eyebrow);

    function row(labelText, controlEl) {
      var r = document.createElement("div");
      r.style.display = "flex";
      r.style.alignItems = "center";
      r.style.justifyContent = "space-between";
      r.style.gap = "0.5rem";
      r.style.marginBottom = "0.4rem";
      var l = document.createElement("label");
      l.textContent = labelText;
      l.style.fontSize = "0.85rem";
      l.style.color = "var(--fg)";
      r.appendChild(l);
      r.appendChild(controlEl);
      return r;
    }

    function numInput(value, min, max, step, suffix) {
      var wrap = document.createElement("div");
      wrap.style.display = "inline-flex";
      wrap.style.alignItems = "baseline";
      wrap.style.gap = "0.25rem";
      var inp = document.createElement("input");
      inp.type = "number";
      inp.value = String(value);
      inp.min = String(min);
      inp.max = String(max);
      inp.step = String(step);
      inp.style.width = "4.5rem";
      inp.style.padding = "0.25rem 0.4rem";
      inp.style.background = "var(--ink-raised)";
      inp.style.color = "var(--fg)";
      inp.style.border = "1px solid var(--line)";
      inp.style.borderRadius = "3px";
      inp.style.fontFamily = "var(--mono)";
      inp.style.fontSize = "0.85rem";
      inp.style.textAlign = "right";
      wrap.appendChild(inp);
      if (suffix) {
        var s = document.createElement("span");
        s.textContent = suffix;
        s.style.color = "var(--text-dim)";
        s.style.fontSize = "0.8rem";
        wrap.appendChild(s);
      }
      wrap.input = inp;
      return wrap;
    }

    var socWrap = numInput(initSoC, 0, 100, 5, "%");
    var unlockWrap = numInput(initUnlock, 0, 100, 5, "%");

    var timeInp = document.createElement("input");
    timeInp.type = "time";
    timeInp.value = initLocalHHMM;
    timeInp.style.padding = "0.25rem 0.4rem";
    timeInp.style.background = "var(--ink-raised)";
    timeInp.style.color = "var(--fg)";
    timeInp.style.border = "1px solid var(--line)";
    timeInp.style.borderRadius = "3px";
    timeInp.style.fontFamily = "var(--mono)";
    timeInp.style.fontSize = "0.85rem";

    function checkbox(checked, labelText) {
      var cb = document.createElement("input");
      cb.type = "checkbox";
      cb.checked = checked;
      cb.style.accentColor = "var(--accent-e)";
      var wrap = document.createElement("label");
      wrap.style.display = "inline-flex";
      wrap.style.alignItems = "center";
      wrap.style.gap = "0.4rem";
      wrap.style.cursor = "pointer";
      wrap.style.fontSize = "0.85rem";
      wrap.appendChild(cb);
      var txt = document.createElement("span");
      txt.textContent = labelText;
      wrap.appendChild(txt);
      wrap.input = cb;
      return wrap;
    }

    var recWrap = checkbox(initRec, "Recurring (every day)");
    var surWrap = checkbox(initSurplus, "Surplus charge from home battery");
    var recCb = recWrap.input;
    var surCb = surWrap.input;

    box.appendChild(row("Target SoC", socWrap));
    box.appendChild(row("By", timeInp));

    var checkRow = document.createElement("div");
    checkRow.style.display = "flex";
    checkRow.style.flexDirection = "column";
    checkRow.style.gap = "0.35rem";
    checkRow.style.marginBottom = "0.55rem";
    checkRow.appendChild(recWrap);
    checkRow.appendChild(surWrap);
    box.appendChild(checkRow);

    var unlockHint = document.createElement("small");
    unlockHint.style.display = "block";
    unlockHint.style.color = "var(--text-dim)";
    unlockHint.style.marginTop = "0.2rem";
    unlockHint.style.marginBottom = "0.3rem";
    unlockHint.textContent = "Always grab PV surplus when home battery ≥ threshold.";
    box.appendChild(unlockHint);

    var thresholdRow = row("Threshold", unlockWrap);
    box.appendChild(thresholdRow);

    function applySurplusGate() {
      var on = surCb.checked;
      unlockWrap.input.disabled = !on;
      thresholdRow.style.opacity = on ? "1" : "0.4";
      thresholdRow.style.pointerEvents = on ? "auto" : "none";
      unlockHint.style.opacity = on ? "1" : "0.55";
    }
    applySurplusGate();
    surCb.addEventListener("change", applySurplusGate);

    // Actions
    var btnRow = document.createElement("div");
    btnRow.style.display = "flex";
    btnRow.style.gap = "0.5rem";
    btnRow.style.marginTop = "0.55rem";
    btnRow.style.justifyContent = "flex-end";

    function mkBtn(label, primary) {
      var b = document.createElement("button");
      b.textContent = label;
      b.style.padding = "0.3rem 0.8rem";
      b.style.fontSize = "0.8rem";
      b.style.fontFamily = "var(--mono)";
      b.style.letterSpacing = "0.06em";
      b.style.textTransform = "uppercase";
      b.style.borderRadius = "3px";
      b.style.cursor = "pointer";
      b.style.border = "1px solid var(--line)";
      if (primary) {
        b.style.background = "var(--accent-e)";
        b.style.color = "#0a0a0a";
        b.style.borderColor = "var(--accent-e)";
      } else {
        b.style.background = "transparent";
        b.style.color = "var(--fg)";
      }
      return b;
    }
    var clearBtn = mkBtn("Clear", false);
    var saveBtn = mkBtn(hasSched ? "Update" : "Save", true);
    clearBtn.disabled = !hasSched;
    if (!hasSched) clearBtn.style.opacity = "0.4";
    btnRow.appendChild(clearBtn);
    btnRow.appendChild(saveBtn);
    box.appendChild(btnRow);

    var status = document.createElement("small");
    status.style.display = "block";
    status.style.color = "var(--text-dim)";
    status.style.marginTop = "0.4rem";
    status.style.minHeight = "1em";
    box.appendChild(status);

    // Cache + dirty wiring: each user edit flips schedDirty=true so
    // the auto-refresh keeps THIS element on screen instead of
    // rebuilding it from stale server state.
    schedCacheEl = box;
    schedCacheLpId = lp.id;
    schedDirty = false;
    function markDirty() { schedDirty = true; }
    [socWrap.input, timeInp, unlockWrap.input].forEach(function (el) {
      el.addEventListener("input", markDirty);
      el.addEventListener("change", markDirty);
    });
    recCb.addEventListener("change", markDirty);

    saveBtn.addEventListener("click", function () {
      saveBtn.disabled = true;
      clearBtn.disabled = true;
      status.textContent = "Saving…";
      var localHHMM = timeInp.value || initLocalHHMM;
      var minUTC = localHHMMToUtcMins(localHHMM);
      // Surplus checkbox gates the threshold: when off, the threshold
      // is sent as 0 (the backend interprets 0 as "feature disabled").
      var unlockVal = surCb.checked ? Number(unlockWrap.input.value) : 0;
      var body = {
        schedule: {
          soc_pct: Number(socWrap.input.value),
          time_of_day_min_utc: minUTC,
          recurring: !!recCb.checked,
          surplus_unlock_bat_soc_pct: unlockVal,
        },
      };
      fetch("/api/loadpoints/" + encodeURIComponent(lp.id) + "/target", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      }).then(function (r) {
        if (!r.ok) throw new Error("HTTP " + r.status);
        status.textContent = "Saved.";
        schedDirty = false;
        schedCacheEl = null;
        refreshEvModal();
      }).catch(function (e) {
        status.textContent = "Save failed: " + e.message;
        saveBtn.disabled = false;
        clearBtn.disabled = false;
      });
    });

    clearBtn.addEventListener("click", function () {
      saveBtn.disabled = true;
      clearBtn.disabled = true;
      status.textContent = "Clearing…";
      fetch("/api/loadpoints/" + encodeURIComponent(lp.id) + "/target", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ schedule: null }),
      }).then(function (r) {
        if (!r.ok) throw new Error("HTTP " + r.status);
        status.textContent = "Cleared.";
        schedDirty = false;
        schedCacheEl = null;
        refreshEvModal();
      }).catch(function (e) {
        status.textContent = "Clear failed: " + e.message;
        saveBtn.disabled = false;
      });
    });

    return box;
  }

  function utcMinsToLocalHHMM(min) {
    var d = new Date();
    d.setUTCHours(Math.floor(min / 60), min % 60, 0, 0);
    return String(d.getHours()).padStart(2, "0") + ":" + String(d.getMinutes()).padStart(2, "0");
  }
  function localHHMMToUtcMins(hhmm) {
    var parts = String(hhmm).split(":");
    if (parts.length !== 2) return 360;
    var h = parseInt(parts[0], 10), m = parseInt(parts[1], 10);
    if (isNaN(h) || isNaN(m)) return 360;
    var d = new Date();
    d.setHours(h, m, 0, 0);
    return d.getUTCHours() * 60 + d.getUTCMinutes();
  }

  var evRefreshTimer = null;
  if (evModal) {
    var evBtnStart = document.getElementById("ev-btn-start");
    var evBtnPause = document.getElementById("ev-btn-pause");
    var evBtnResume = document.getElementById("ev-btn-resume");
    var evActionBtns = [evBtnStart, evBtnPause, evBtnResume];

    function openEvModal(driver) {
      evModalDriver = driver || null;
      evModal.open();
      refreshEvModal();
      // Guard against stacked timers if the modal opens again while a
      // previous timer is still alive.
      if (evRefreshTimer) { clearInterval(evRefreshTimer); }
      evRefreshTimer = setInterval(refreshEvModal, 5000);
    }
    evModal.addEventListener("ftw-modal-close", function () {
      if (evRefreshTimer) { clearInterval(evRefreshTimer); evRefreshTimer = null; }
      evModalDriver = null;
    });

    // Planet click routing. EV → EV modal scoped to driver. Battery →
    // <ftw-battery-control> manual-hold modal (no driver scoping; the
    // hold applies to the aggregate battery setpoint). Grid → grid
    // modal hosting the peak-import ceiling and the (legacy) grid
    // target setpoint.
    var gridModal = document.getElementById("grid-modal");
    if (energyFlowEl) {
      energyFlowEl.addEventListener("ftw-planet-click", function (e) {
        var d = (e && e.detail) || {};
        if (d.role === "ev") openEvModal(d.name || null);
        if (d.role === "battery") {
          var bc = document.getElementById("battery-control");
          if (bc && typeof bc.open === "function") bc.open();
        }
        if (d.role === "grid" && gridModal) gridModal.open();
      });
    }

    // Tile-mode (numeric cards) parity: when the operator toggles the
    // hero off, the energy-flow planets aren't on screen, so the
    // modal triggers need a second home. Binding click on the matching
    // .summary-card opens the same modal, with a `.clickable` class
    // that gives the card a pointer cursor + hover lift to advertise
    // the affordance. EV has no tile-mode card today (loadpoints are
    // listed separately) — leave that one to the planet for now.
    var cardBat = document.getElementById("card-bat");
    if (cardGrid && gridModal) {
      cardGrid.classList.add("clickable");
      cardGrid.setAttribute("role", "button");
      cardGrid.setAttribute("tabindex", "0");
      cardGrid.setAttribute("aria-label", "Open grid controls");
      cardGrid.addEventListener("click", function () { gridModal.open(); });
      cardGrid.addEventListener("keydown", function (e) {
        if (e.key === "Enter" || e.key === " ") { e.preventDefault(); gridModal.open(); }
      });
    }
    if (cardBat) {
      cardBat.classList.add("clickable");
      cardBat.setAttribute("role", "button");
      cardBat.setAttribute("tabindex", "0");
      cardBat.setAttribute("aria-label", "Open battery controls");
      var openBat = function () {
        var bc = document.getElementById("battery-control");
        if (bc && typeof bc.open === "function") bc.open();
      };
      cardBat.addEventListener("click", openBat);
      cardBat.addEventListener("keydown", function (e) {
        if (e.key === "Enter" || e.key === " ") { e.preventDefault(); openBat(); }
      });
    }
    var cardEv = document.getElementById("card-ev");
    if (cardEv && typeof openEvModal === "function") {
      cardEv.classList.add("clickable");
      cardEv.setAttribute("role", "button");
      cardEv.setAttribute("tabindex", "0");
      cardEv.setAttribute("aria-label", "Open EV charger");
      // Pass null so openEvModal aggregates across all EV drivers —
      // matches the no-driver-scoping fallback the planet click uses
      // when the operator hasn't picked a specific charger.
      var openEv = function () { openEvModal(null); };
      cardEv.addEventListener("click", openEv);
      cardEv.addEventListener("keydown", function (e) {
        if (e.key === "Enter" || e.key === " ") { e.preventDefault(); openEv(); }
      });
    }

    function evCommand(action) {
      evActionBtns.forEach(function (b) { b.disabled = true; });
      var body = { action: action };
      if (evModalDriver) body.driver = evModalDriver;
      postJson("/api/ev/command", body)
        .catch(function () { /* postJson already logs */ })
        .finally(function () {
          refreshEvModal();
          evActionBtns.forEach(function (b) { b.disabled = false; });
        });
    }
    evBtnStart.addEventListener("click", function () { evCommand("ev_start"); });
    evBtnPause.addEventListener("click", function () { evCommand("ev_pause"); });
    evBtnResume.addEventListener("click", function () { evCommand("ev_resume"); });
  }


  // Click-to-toggle legend items. Each item has data-toggle with a
  // key; clicking toggles visibility of the matching series and
  // persists to localStorage.
  var chartLegend = document.getElementById("chart-legend");
  if (chartLegend) {
    // Apply persisted "off" state on initial render.
    chartLegend.querySelectorAll(".legend-item[data-toggle]").forEach(function (el) {
      if (legendHidden[el.dataset.toggle]) el.classList.add("legend-off");
    });
    chartLegend.addEventListener("click", function (e) {
      var item = e.target.closest(".legend-item[data-toggle]");
      if (!item) return;
      var key = item.dataset.toggle;
      legendHidden[key] = !legendHidden[key];
      item.classList.toggle("legend-off", !!legendHidden[key]);
      try { localStorage.setItem("legend-hidden", JSON.stringify(legendHidden)); } catch (e2) {}
      renderChart();
    });
  }

  // Range selector
  var rangeButtons = document.getElementById("range-buttons");
  if (rangeButtons) {
    rangeButtons.addEventListener("click", function (e) {
      if (e.target.tagName === "BUTTON" && e.target.dataset.range) {
        rangeButtons.querySelectorAll("button").forEach(function (b) {
          b.classList.toggle("active", b === e.target);
        });
        chartRange = e.target.dataset.range;
        loadHistory(chartRange);
      }
    });
  }

  // Power / Energy view toggle
  var viewButtons = document.getElementById("view-buttons");
  var chartTitle = document.getElementById("chart-title");
  if (viewButtons) {
    viewButtons.addEventListener("click", function (e) {
      if (e.target.tagName === "BUTTON" && e.target.dataset.view) {
        viewButtons.querySelectorAll("button").forEach(function (b) {
          b.classList.toggle("active", b === e.target);
        });
        chartView = e.target.dataset.view;
        if (chartTitle) chartTitle.textContent = chartView === "energy" ? "Energy (cumulative today)" : "Power";
        // Reset cached layout so the y-axis snaps to the new view's range
        // instead of lerp-ing from the previous view's scale.
        chartLayout = null;
        updateLegend();
        renderChart();
      }
    });
  }

  function updateLegend() {
    var legend = document.getElementById("chart-legend");
    if (!legend) return;
    var items = chartView === "energy" ? [
      ["#ef4444", "Import"], ["#22c55e", "Export"], ["#10b981", "PV"],
      ["#3b82f6", "Charged"], ["#f59e0b", "Discharged"], ["#e2e8f0", "Load"],
    ] : [
      ["#ef4444", "Grid"], ["#22c55e", "PV"], ["#e2e8f0", "Load"],
      ["#f59e0b", "Ferroamp"], ["#8b5cf6", "Sungrow"],
    ];
    legend.innerHTML = items.map(function(it) {
      return '<span class="legend-item"><span class="legend-color" style="background:'+it[0]+'"></span> '+it[1]+'</span>';
    }).join('');
  }

  // ---- Chart hover ----
  var canvas = document.getElementById("power-chart");
  if (canvas) {
    canvas.addEventListener("mousemove", function (e) {
      if (!chartLayout) return;
      var rect = canvas.getBoundingClientRect();
      var x = e.clientX - rect.left;
      var l = chartLayout;
      if (x < l.pad.left || x > l.pad.left + l.plotW) {
        if (hoverIndex !== -1 || hoverForecast) { hoverIndex = -1; hoverForecast = null; }
        return;
      }
      // Map x → timestamp using the FULL plot span (past + future).
      var hoverTs = l.windowStart + (x - l.pad.left) / l.plotW * l.totalMs;
      if (hoverTs <= l.now) {
        // Past: nearest history point
        var bestIdx = -1, bestDelta = Infinity;
        for (var i = 0; i < chartHistory.timestamps.length; i++) {
          var d = Math.abs(chartHistory.timestamps[i] - hoverTs);
          if (d < bestDelta) { bestDelta = d; bestIdx = i; }
        }
        hoverIndex = bestIdx;
        hoverForecast = null;
      } else {
        // Future: find the plan slot covering hoverTs
        hoverIndex = -1;
        hoverForecast = null;
        var plan = l.plan;
        if (plan && plan.actions) {
          for (var j = 0; j < plan.actions.length; j++) {
            var a = plan.actions[j];
            var aEnd = a.slot_start_ms + a.slot_len_min * 60000;
            if (hoverTs >= a.slot_start_ms && hoverTs < aEnd) {
              hoverForecast = { ts: hoverTs, action: a };
              break;
            }
          }
        }
      }
    });
    canvas.addEventListener("mouseleave", function () {
      hoverIndex = -1;
      hoverForecast = null;
    });
  }

  // ---- History loader ----
  function loadHistory(range) {
    var points = CHART_POINTS;
    return fetch("/api/history?range=" + (range || "5m") + "&points=" + points)
      .then(function (res) { return res.ok ? res.json() : null; })
      .then(function (data) {
        if (!data || !data.items) return;
        // Populate chart history from persisted data
        Object.keys(chartHistory).forEach(function(k) { chartHistory[k] = []; });
        // Reset the dynamic battery + EV sets and rediscover from the
        // history items themselves — drivers that existed earlier but no
        // longer appear in /api/status will simply not be recreated.
        chartBatteries = {};
        chartEVs = {};
        data.items.forEach(function (it) {
          var et = it.energy_today || {};
          chartHistory.grid.push(it.grid_w || 0);
          chartHistory.pv.push(it.pv_w || 0);
          chartHistory.load.push(it.load_w || 0);
          chartHistory.timestamps.push(it.ts || 0);
          chartHistory.e_import.push(et.import_wh || 0);
          chartHistory.e_export.push(et.export_wh || 0);
          chartHistory.e_pv.push(et.pv_wh || 0);
          chartHistory.e_charged.push(et.bat_charged_wh || 0);
          chartHistory.e_discharged.push(et.bat_discharged_wh || 0);
          chartHistory.e_load.push(et.load_wh || 0);

          // Per-driver discovery from this item's drivers + targets maps.
          // A driver can expose bat_w, ev_w, or both.
          var itDrivers = it.drivers || {};
          var itTargets = it.targets || {};
          var seenBat = {}, seenEV = {};
          Object.keys(itDrivers).forEach(function (name) {
            var d = itDrivers[name] || {};
            if (d.bat_w != null) {
              seenBat[name] = true;
              var bslot = ensureBatteryDriver(name);
              bslot.bat.push(d.bat_w || 0);
              bslot.target.push(itTargets[name] || 0);
            }
            if (d.ev_w != null) {
              seenEV[name] = true;
              ensureEVDriver(name).ev.push(d.ev_w || 0);
            }
          });
          Object.keys(chartBatteries).forEach(function (name) {
            if (seenBat[name]) return;
            var slot = chartBatteries[name];
            slot.bat.push(0);
            slot.target.push(0);
          });
          Object.keys(chartEVs).forEach(function (name) {
            if (seenEV[name]) return;
            chartEVs[name].ev.push(0);
          });
        });
        syncBatteryLegend();
        syncEVLegend();
        renderChart();
      })
      .catch(function () { /* silent */ });
  }

  // ---- Animation loop ----
  // Drives the chart at ~30fps — points scroll left smoothly as time advances,
  // pulse rings shimmer at the latest data point. Cards still update on poll.
  var lastFrame = 0;
  function animationFrame(ts) {
    if (animating) {
      // Throttle to ~30fps to keep CPU low — the visual feel is the same
      if (ts - lastFrame > 33) {
        renderChart();
        lastFrame = ts;
      }
    }
    requestAnimationFrame(animationFrame);
  }

  // Pause animation when tab is hidden (saves battery on background tabs)
  document.addEventListener("visibilitychange", function () {
    animating = !document.hidden;
  });

  // ---- History wrapper: one Week/Month toggle drives all three tiles ----
  // The wrapper card in index.html hosts #history-toggle and the three
  // <ftw-history-card hide-toggle range="week"> children. Clicking a
  // button flips data-active (moves the sliding pill) and pushes
  // `range=week|month` onto every child — the component observes that
  // attribute and re-fetches, so the three charts stay in lock-step.
  var historyToggle = $("history-toggle");
  var historyViewToggle = $("history-view-toggle");
  var historyTiles = $("history-tiles");
  var historyCakeWrap = $("history-cake");
  var historyCakeEl = $("history-cake-el");

  // historyState mirrors both toggles so the Bars/Cakes view and
  // the Week/Month range stay coordinated. Only the cake re-fetches
  // /api/energy/daily on a range change; the bar tiles each
  // re-fetch themselves when their range= attribute is updated.
  var historyState = { range: "week", view: "bars" };

  function fetchHistoryCake() {
    if (!historyCakeEl || typeof historyCakeEl.setTotals !== "function") return;
    if (historyCakeWrap) historyCakeWrap.classList.add("loading");
    var days = historyState.range === "month" ? 30 : 7;
    var clearLoading = function () {
      if (historyCakeWrap) historyCakeWrap.classList.remove("loading");
    };
    fetch("/api/energy/daily?days=" + days)
      .then(function (r) { return r.json(); })
      .then(function (j) {
        var arr = (j && j.days) || [];
        var totals = { import_wh: 0, load_wh: 0, export_wh: 0, pv_wh: 0 };
        for (var i = 0; i < arr.length; i++) {
          totals.import_wh += arr[i].import_wh || 0;
          totals.load_wh   += arr[i].load_wh   || 0;
          totals.export_wh += arr[i].export_wh || 0;
          totals.pv_wh     += arr[i].pv_wh     || 0;
        }
        historyCakeEl.setTotals(totals);
      })
      .catch(function () { /* network blip — leave the previous render */ })
      .then(clearLoading, clearLoading);
  }

  if (historyToggle) {
    historyToggle.addEventListener("click", function (e) {
      var btn = e.target.closest("button[data-range]");
      if (!btn) return;
      var next = btn.getAttribute("data-range");
      if (!next || next === historyToggle.getAttribute("data-active")) return;
      historyToggle.setAttribute("data-active", next);
      historyState.range = next;
      var buttons = historyToggle.querySelectorAll("button[data-range]");
      for (var i = 0; i < buttons.length; i++) {
        var on = buttons[i].getAttribute("data-range") === next;
        buttons[i].classList.toggle("active", on);
        buttons[i].setAttribute("aria-selected", on ? "true" : "false");
      }
      var tiles = document.querySelectorAll(".history-tiles ftw-history-card");
      for (var j = 0; j < tiles.length; j++) {
        tiles[j].setAttribute("range", next);
      }
      // Cake: refresh only when it's the active view.
      if (historyState.view === "cakes") fetchHistoryCake();
    });
  }

  if (historyViewToggle) {
    historyViewToggle.addEventListener("click", function (e) {
      var btn = e.target.closest("button[data-view]");
      if (!btn) return;
      var next = btn.getAttribute("data-view");
      if (!next || next === historyViewToggle.getAttribute("data-active")) return;
      historyViewToggle.setAttribute("data-active", next);
      historyState.view = next;
      var buttons = historyViewToggle.querySelectorAll("button[data-view]");
      for (var i = 0; i < buttons.length; i++) {
        var on = buttons[i].getAttribute("data-view") === next;
        buttons[i].classList.toggle("active", on);
        buttons[i].setAttribute("aria-selected", on ? "true" : "false");
      }
      if (historyTiles) historyTiles.classList.toggle("hidden", next !== "bars");
      if (historyCakeWrap) historyCakeWrap.classList.toggle("hidden", next !== "cakes");
      if (next === "cakes") fetchHistoryCake();
    });
  }

  // ---- Init ----
  loadHistory(chartRange);
  fetchStatus();
  setInterval(fetchStatus, POLL_INTERVAL);
  requestAnimationFrame(animationFrame);
})();
