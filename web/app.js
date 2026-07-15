// FTW dashboard — plain JS, no framework

(function () {
  "use strict";

  const POLL_INTERVAL = 2000;        // status poll cadence — snappier cards
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
  const batSoc = $("bat-soc");
  const socFill = $("soc-fill");
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
  const evSlider = $("ev-slider");
  const evValue = $("ev-value");
  const evSend = $("ev-send");
  const fuseUse = $("fuse-use");
  const fuseFill = $("fuse-fill");
  const evW = $("ev-w");
  const evStatus = $("ev-status");
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

  function statusClass(status) {
    if (!status) return "status-offline";
    const s = status.toLowerCase();
    if (s === "ok") return "status-ok";
    if (s === "degraded") return "status-degraded";
    return "status-offline";
  }

  // ---- Render ----
  function render(data) {
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

    // PV — negative = generating
    pvW.textContent = formatW(data.pv_w);
    pvW.className = "card-value val-generation";

    // Load
    loadW.textContent = formatW(data.load_w || 0);

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

    // SoC
    var socPct = Math.round(data.bat_soc * 100);
    batSoc.textContent = socPct + "%";
    socFill.style.width = socPct + "%";

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
    if (gridHint) gridHint.style.display = plannerActive ? "block" : "none";
    // Plan-stale banner
    if (data.plan_stale && plannerActive && gridHint) {
      gridHint.textContent = "⚠ Plan stale — falling back to self_consumption.";
      gridHint.classList.add("card-hint-warn");
    } else if (gridHint) {
      gridHint.textContent = "Planner controls this when a strategy is active.";
      gridHint.classList.remove("card-hint-warn");
    }

    // Grid target — only update slider if user is not actively dragging
    if (gridTargetSlider && document.activeElement !== gridTargetSlider) {
      gridTargetSlider.value = data.grid_target_w;
      gridTargetValue.textContent = formatW(data.grid_target_w);
    }
    if (peakLimitSlider && document.activeElement !== peakLimitSlider && data.peak_limit_w != null) {
      peakLimitSlider.value = data.peak_limit_w;
      peakLimitValue.textContent = formatW(data.peak_limit_w);
    }
    if (evSlider && document.activeElement !== evSlider && data.ev_charging_w != null) {
      evSlider.value = data.ev_charging_w;
      evValue.textContent = formatW(data.ev_charging_w);
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
      var hasPhaseData = phaseI.length > 0;

      // Show fallback (single bar + headline amps) only when no per-phase data.
      var fallbackBar = $("fuse-bar-fallback");
      if (fallbackBar) fallbackBar.style.display = hasPhaseData ? "none" : "block";
      fuseUse.style.display = hasPhaseData ? "none" : "block";

      if (!hasPhaseData) {
        var totalDischarge = 0;
        if (data.bat_w < 0) totalDischarge = Math.abs(data.bat_w);
        var pvGen = Math.abs(data.pv_w);
        var throughput = Math.max(Math.abs(data.grid_w), pvGen + totalDischarge);
        var peakA = throughput / voltage / phases;
        fuseUse.textContent = peakA.toFixed(1) + " A";
        var totalFusePct = Math.min(100, (peakA / maxAmps) * 100);
        fuseFill.style.width = totalFusePct + "%";
        fuseFill.className = "fuse-fill" + (totalFusePct > 85 ? " crit" : totalFusePct > 65 ? " warn" : "");
      }

      // Per-phase bars: create/update one row per configured phase.
      if (fusePhases) {
        // Rebuild if phase count changed (first render, or config reload).
        if (fusePhases.childElementCount !== phases) {
          fusePhases.innerHTML = "";
          for (var p = 0; p < phases; p++) {
            var row = document.createElement("div");
            row.className = "fuse-phase-row";
            var label = document.createElement("span");
            label.className = "fuse-phase-label";
            label.textContent = "L" + (p + 1);
            var bar = document.createElement("div");
            bar.className = "fuse-phase-bar";
            var fill = document.createElement("div");
            fill.className = "fuse-phase-fill";
            bar.appendChild(fill);
            var val = document.createElement("span");
            val.className = "fuse-phase-val";
            val.textContent = "-- A";
            row.appendChild(label);
            row.appendChild(bar);
            row.appendChild(val);
            fusePhases.appendChild(row);
          }
        }
        // Populate current values. If fewer phase_amps than phases
        // configured, any missing entries fall back to 0.
        var rows = fusePhases.querySelectorAll(".fuse-phase-row");
        for (var r = 0; r < rows.length; r++) {
          var rawA = r < phaseI.length ? phaseI[r] : 0;
          var magA = Math.abs(rawA);
          var pct = Math.min(100, (magA / maxAmps) * 100);
          var fill = rows[r].querySelector(".fuse-phase-fill");
          var val  = rows[r].querySelector(".fuse-phase-val");
          fill.style.width = pct + "%";
          fill.className = "fuse-phase-fill"
            + (pct > 85 ? " crit" : pct > 65 ? " warn" : "")
            + (rawA < -0.1 ? " export" : "");
          val.textContent = magA.toFixed(1) + " A";
        }
      }
    }

    // EV status card
    if (evW && evStatus) {
      var evPower = data.ev_charging_w || 0;
      evW.textContent = formatW(evPower);
      if (evPower > 100) {
        evStatus.textContent = "charging";
        evW.className = "card-value val-ev-charging";
      } else if (evPower > 0) {
        evStatus.textContent = "connected";
        evW.className = "card-value val-ev-connected";
      } else {
        evStatus.textContent = "idle";
        evW.className = "card-value val-neutral";
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
    Object.keys(drivers).forEach(function (name) {
      var d = drivers[name] || {};
      if (d.bat_w == null) return;
      seenBatteries[name] = true;
      var slot = ensureBatteryDriver(name);
      slot.bat.push(d.bat_w || 0);
      slot.target.push((targetsByDriver && targetsByDriver[name]) || 0);
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

    if (chartHistory.grid.length > CHART_POINTS) {
      Object.keys(chartHistory).forEach(function(k) { chartHistory[k].shift(); });
      Object.keys(chartBatteries).forEach(function (name) {
        var slot = chartBatteries[name];
        slot.bat.shift();
        slot.target.shift();
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
    if (canvas.width !== w * dpr || canvas.height !== h * dpr) {
      canvas.width = w * dpr;
      canvas.height = h * dpr;
      canvas.style.width = w + "px";
      canvas.style.height = h + "px";
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    }

    var C = chartColors(); // theme-aware chrome colors, re-read each draw

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
        { data: toKwh(chartHistory.e_load),       color: C.load, width: 2, dash: [], name: "Load",       fill: false },
      ];
    } else {
      series = [
        { data: chartHistory.grid, color: "#ef4444", width: 2,   dash: [], name: "Grid", fill: true,  toggle: "grid" },
        { data: chartHistory.pv,   color: "#22c55e", width: 2,   dash: [], name: "PV",   fill: true,  toggle: "pv" },
        { data: chartHistory.load, color: C.load, width: 1.5, dash: [], name: "Load", fill: false, toggle: "load" },
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
      ctx.fillStyle = C.muted;
      ctx.font = "12px monospace";
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
    ctx.strokeStyle = C.grid;
    ctx.lineWidth = 0.5;
    ctx.font = "11px monospace";
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
      ctx.strokeStyle = C.muted;
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

    // Subtle vertical "now" line — anchor point so the eye knows where present is
    var nowX = pad.left + plotW;
    ctx.strokeStyle = C.grid;
    ctx.lineWidth = 1;
    ctx.beginPath();
    ctx.moveTo(nowX, pad.top);
    ctx.lineTo(nowX, pad.top + plotH);
    ctx.stroke();

    // Y-axis labels (outside clip so they're fully visible)
    ctx.fillStyle = C.dim;
    ctx.font = "11px monospace";
    for (var i2 = 0; i2 <= steps; i2++) {
      var yVal = yMin + (yRange * i2 / steps);
      var ly = pad.top + plotH - (plotH * i2 / steps);
      ctx.fillText(chartView === "energy" ? yVal.toFixed(1) + " kWh" : formatW(yVal), 2, ly + 4);
    }

    // Time labels
    ctx.fillStyle = C.muted;
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
      ctx.font = "10px monospace";
      ctx.fillStyle = fresh ? C.dim : "#f59e0b";
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

  // Resolve a CSS custom property to a concrete color string for <canvas>,
  // which can't read var(). Re-read per draw so the charts follow the active
  // theme (html[data-theme]) without needing an explicit redraw on toggle —
  // a hidden probe element inherits :root, and getComputedStyle resolves the
  // var() + oklch token to a value canvas can paint.
  var _colorProbe = null;
  function cssColor(name, fallback) {
    if (!_colorProbe) {
      _colorProbe = document.createElement("span");
      _colorProbe.style.cssText = "position:absolute;visibility:hidden;pointer-events:none";
      document.body.appendChild(_colorProbe);
    }
    _colorProbe.style.color = "var(" + name + ", " + (fallback || "#888") + ")";
    return getComputedStyle(_colorProbe).color || fallback || "#888";
  }
  // Theme-aware chart chrome colors. Series hues stay fixed (they read on
  // both themes); only text / gridlines / tooltip surface / the neutral
  // "load" line flip — those are what go invisible on a light background.
  // Cached so the animation loop doesn't getComputedStyle every frame —
  // only recomputed when the active theme actually changes.
  var _chartColors = null, _chartColorsTheme = null;
  function chartColors() {
    var theme = document.documentElement.getAttribute("data-theme") || "";
    if (_chartColors && _chartColorsTheme === theme) return _chartColors;
    _chartColorsTheme = theme;
    _chartColors = {
      text:    cssColor("--fg", "#e6e6e6"),
      dim:     cssColor("--fg-dim", "#aaaaaa"),
      muted:   cssColor("--fg-muted", "#888888"),
      grid:    cssColor("--line", "#2a2a2a"),
      surface: cssColor("--ink-raised", "#14141f"),
      accent:  cssColor("--accent-e", "#fbbf24"),
      load:    cssColor("--fg", "#e2e8f0"),
    };
    return _chartColors;
  }

  function drawHoverOverlay(ctx) {
    if (!chartLayout) return;
    var C = chartColors();
    var l = chartLayout;
    var i = hoverIndex;
    // Map by timestamp (matches the time-anchored line drawing)
    var ts = chartHistory.timestamps[i];
    if (ts == null) return;
    var x = l.pad.left + l.plotW * (ts - l.windowStart) / l.totalMs;

    // Vertical line
    ctx.strokeStyle = C.muted;
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
      { name: "Load",       data: chartHistory.e_load,       color: C.load },
    ] : (function () {
      var rows = [
        { name: "Grid", data: chartHistory.grid, color: "#ef4444" },
        { name: "PV",   data: chartHistory.pv,   color: "#22c55e" },
        { name: "Load", data: chartHistory.load, color: C.load },
      ];
      // Battery rows render their target inline as "actual W (→ target W)"
      // so it's visually obvious the two numbers are the same metric — one
      // measured, one commanded. See value formatter below.
      Object.keys(chartBatteries).sort().forEach(function (name) {
        var slot = chartBatteries[name];
        rows.push({ name: batteryLabel(name), data: slot.bat, color: batteryColor(name), target: slot.target });
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

    ctx.fillStyle = C.surface;
    ctx.strokeStyle = C.grid;
    ctx.lineWidth = 1;
    ctx.fillRect(boxX, boxY, boxW, boxH);
    ctx.strokeRect(boxX, boxY, boxW, boxH);

    ctx.font = "10px monospace";
    ctx.fillStyle = C.dim;
    ctx.fillText(timeStr, boxX + 6, boxY + lineHeight - 2);

    labels.forEach(function (lab, idx) {
      if (i >= lab.data.length) return;
      var y = boxY + (idx + 2) * lineHeight - 4;
      ctx.fillStyle = lab.color;
      ctx.fillRect(boxX + 6, y - 8, 8, 8);
      ctx.fillStyle = lab.dim ? C.muted : C.text;
      ctx.fillText(lab.name, boxX + 18, y);
      ctx.textAlign = "right";
      if (chartView === "energy") {
        ctx.fillStyle = C.text;
        ctx.fillText(lab.data[i].toFixed(2) + " kWh", boxX + boxW - 6, y);
      } else {
        var actual = formatW(lab.data[i]);
        ctx.fillStyle = C.text;
        ctx.fillText(actual, boxX + boxW - 6, y);
        // Inline target as dim "(→ -674 W)" so user sees commanded vs actual
        // in one glance. Skip when target is 0 to reduce visual noise.
        if (lab.target && i < lab.target.length && Math.abs(lab.target[i]) > 1) {
          var actualW = ctx.measureText(actual).width;
          ctx.fillStyle = C.dim;
          ctx.font = "9px monospace";
          ctx.fillText("→ " + formatW(lab.target[i]), boxX + boxW - 10 - actualW, y);
          ctx.font = "10px monospace";
        }
      }
      ctx.textAlign = "left";
    });
  }

  function drawForecastHoverOverlay(ctx) {
    if (!chartLayout || !hoverForecast) return;
    var C = chartColors();
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
      { name: "PV pred",   val: a.pv_w,   color: "#86efac" },
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

    ctx.fillStyle = C.surface;
    ctx.strokeStyle = C.accent;
    ctx.lineWidth = 1;
    ctx.fillRect(boxX, boxY, boxW, boxH);
    ctx.strokeRect(boxX, boxY, boxW, boxH);

    ctx.font = "10px monospace";
    var d = new Date(ts);
    var hh = d.getHours().toString().padStart(2, "0") + ":" + d.getMinutes().toString().padStart(2, "0");
    ctx.fillStyle = C.accent;
    ctx.fillText(hh + "  predicted", boxX + 6, boxY + lineHeight - 2);

    labels.forEach(function (lab, idx) {
      var y = boxY + (idx + 2) * lineHeight - 4;
      ctx.fillStyle = lab.color;
      ctx.fillRect(boxX + 6, y - 8, 8, 8);
      ctx.fillStyle = C.text;
      ctx.fillText(lab.name, boxX + 18, y);
      ctx.fillStyle = C.text;
      ctx.textAlign = "right";
      var val = lab.literal ? lab.val : formatW(lab.val);
      ctx.fillText(val, boxX + boxW - 6, y);
      ctx.textAlign = "left";
    });

    if (a.reason) {
      var ry = boxY + (labels.length + 2) * lineHeight + 2;
      ctx.fillStyle = C.dim;
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

  function parseResponseError(res) {
    return res.text().then(function (t) {
      if (!t) return "HTTP " + res.status;
      try {
        var j = JSON.parse(t);
        return j && j.error ? j.error : t;
      } catch (e) {
        return t;
      }
    });
  }

  function v2xCommand(driver, powerW) {
    return fetch("/api/v2x/command", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        action: powerW === 0 ? "v2x_stop" : "v2x_set_power",
        driver: driver,
        power_w: powerW,
      }),
    }).then(function (res) {
      if (!res.ok) {
        return parseResponseError(res).then(function (msg) { throw new Error(msg); });
      }
      return res.json();
    });
  }

  function renderDriverActions(name, d) {
    // Buttons per driver: Restart (if running), Disable (if running),
    // Enable (if disabled). Small, unobtrusive; rely on the existing
    // .btn-send style from index.html.
    var isDisabled = d.disabled === true || d.status === "disabled";
    var actions = '<div class="driver-actions" style="margin-top:6px;display:flex;gap:6px;flex-wrap:wrap">';
    if (isDisabled) {
      actions += '<button class="btn-send" data-drv-action="enable" data-drv="' + escHtml(name) + '">Enable</button>';
    } else {
      actions += '<button class="btn-send" data-drv-action="restart" data-drv="' + escHtml(name) + '">Restart</button>';
      actions += '<button class="btn-send" data-drv-action="disable" data-drv="' + escHtml(name) + '" style="opacity:0.75">Disable</button>';
    }
    actions += "</div>";
    return actions;
  }

  function formatOptionalW(w) {
    return w == null ? "—" : formatW(w);
  }

  // ---------------------------------------------------------------------
  // SHARED V2X manual-command surface (parseResponseError, v2xCommand,
  // formatOptionalW, renderV2XControls + the click handler and card body
  // below) is intentionally kept byte-identical with the same block in
  // web/next-app.js. There is no shared module system — each file is a
  // standalone IIFE loaded on a different page (app.js → legacy.html,
  // next-app.js → index.html) — so changes here MUST be mirrored there.
  // ---------------------------------------------------------------------
  function renderV2XControls(name, d) {
    var isLive = d.status === "ok";
    var chargeMax = d.v2x_charge_power_max_w || d.v2x_rated_power_w || 50000;
    var dischargeMax = d.v2x_discharge_power_max_w || d.v2x_rated_power_w || 50000;
    var maxW = Math.max(1, Math.min(50000, Math.max(chargeMax, dischargeMax)));
    var suggested = Math.min(3000, maxW);
    var disabled = isLive ? "" : " disabled";
    // Mono eyebrow + caption styled inline from theme.css tokens so it reads
    // identically on both dashboards (legacy style.css has no .v2x-* rules).
    // Flags that this manual surface bypasses the dispatch policy envelope.
    var note = '' +
      '<div class="v2x-experimental-note" style="margin-top:8px">' +
      '  <span style="font-family:var(--mono);font-size:10px;letter-spacing:0.18em;text-transform:uppercase;color:var(--accent-e)">Experimentell</span>' +
      '  <span style="display:block;font-family:var(--mono);font-size:10px;color:var(--fg-dim);margin-top:2px">manuell styrning utan säkerhetsenvelope</span>' +
      '</div>';
    return '' +
      '<div class="v2x-control-panel" data-v2x-driver="' + escHtml(name) + '">' +
      '  <label class="v2x-power-label" for="v2x-power-' + escHtml(name) + '">Manual W</label>' +
      '  <input class="v2x-power-input" id="v2x-power-' + escHtml(name) + '" type="number" min="0" max="' + maxW + '" step="100" value="' + suggested + '"' + disabled + '>' +
      '  <button class="btn-send v2x-command-btn" data-v2x-action="charge" data-drv="' + escHtml(name) + '"' + disabled + '>Charge</button>' +
      '  <button class="btn-send v2x-command-btn" data-v2x-action="discharge" data-drv="' + escHtml(name) + '"' + disabled + '>Discharge</button>' +
      '  <button class="btn-send v2x-command-btn" data-v2x-action="stop" data-drv="' + escHtml(name) + '"' + disabled + '>Stop</button>' +
      '  <div class="v2x-command-status" role="status" aria-live="polite"></div>' +
      note +
      '</div>';
  }

  // Event delegation — one listener for all driver-action buttons. Saves
  // re-binding on every re-render.
  if (driversGrid) {
    driversGrid.addEventListener("click", function (ev) {
      var v2xBtn = ev.target.closest("[data-v2x-action]");
      if (v2xBtn) {
        var v2xName = v2xBtn.getAttribute("data-drv");
        var v2xAction = v2xBtn.getAttribute("data-v2x-action");
        var panel = v2xBtn.closest(".v2x-control-panel");
        var input = panel ? panel.querySelector(".v2x-power-input") : null;
        var status = panel ? panel.querySelector(".v2x-command-status") : null;
        if (!v2xName || !v2xAction) return;

        var requested = input ? Math.abs(Number(input.value || 0)) : 0;
        var max = input ? Number(input.max || 50000) : 50000;
        if (!Number.isFinite(requested)) requested = 0;
        requested = Math.min(Math.max(requested, 0), max);
        var powerW = v2xAction === "stop" ? 0 : requested;
        if (v2xAction === "discharge") powerW = -powerW;
        if (v2xAction === "discharge" && powerW < 0) {
          if (!window.confirm("Discharge " + v2xName + " at " + formatW(powerW) + "?")) return;
        }

        v2xBtn.disabled = true;
        if (status) {
          status.className = "v2x-command-status";
          status.textContent = "Sending " + formatW(powerW) + "…";
        }
        v2xCommand(v2xName, powerW)
          .then(function () {
            if (status) {
              status.className = "v2x-command-status ok";
              status.textContent = powerW === 0 ? "Stopped" : "Sent " + formatW(powerW);
            }
            setTimeout(fetchStatus, 600);
          })
          .catch(function (err) {
            if (status) {
              status.className = "v2x-command-status error";
              status.textContent = err.message;
            }
            alert("V2X command failed: " + err.message);
          })
          .finally(function () { v2xBtn.disabled = false; });
        return;
      }

      var btn = ev.target.closest("[data-drv-action]");
      if (!btn) return;
      var name = btn.getAttribute("data-drv");
      var action = btn.getAttribute("data-drv-action");
      if (!name || !action) return;
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

      // Detect driver kind from telemetry shape. A pure vehicle driver
      // (DerVehicle: SoC + charge-limit, no power) must NOT be mistaken
      // for an EV charger or a V2X charger, so both checks are guarded by
      // !isVehicle. Kept consistent with next-app.js renderDrivers().
      // The legacy dashboard has no dedicated vehicle body, so a vehicle
      // driver falls through to the meter/pv/battery layout below.
      var isVehicle = (d.vehicle_soc != null || d.vehicle_charge_limit_pct != null);
      var isEV = !isVehicle && (d.ev_w != null || d.ev_connected != null || d.ev_charging != null);
      var isV2X = !isVehicle && (d.v2x_w != null || d.v2x_connected != null || d.v2x_vehicle_soc != null);

      var body;
      if (isV2X) {
        var v2xWVal = d.v2x_w != null ? d.v2x_w : 0;
        var connected = d.v2x_connected === true;
        var statusLabel = d.v2x_status
          || (v2xWVal > 100 ? "charging" : (v2xWVal < -100 ? "discharging" : (connected ? "connected" : "idle")));
        var v2xClass = v2xWVal < -100 ? "stat-ok" : (v2xWVal > 100 ? "stat-warn" : (connected ? "stat-warn" : "stat-dim"));
        var vehicleSoc = d.v2x_vehicle_soc != null ? formatSoc(d.v2x_vehicle_soc) : "—";
        var dcSummary = (d.v2x_dc_w != null || d.v2x_dc_v != null || d.v2x_dc_a != null)
          ? formatOptionalW(d.v2x_dc_w) + " · " +
            (d.v2x_dc_v != null ? d.v2x_dc_v.toFixed(0) + " V" : "—") + " · " +
            (d.v2x_dc_a != null ? d.v2x_dc_a.toFixed(1) + " A" : "—")
          : "—";
        var sessionParts = [];
        if (d.v2x_session_charge_wh != null) sessionParts.push("in " + formatKwh(d.v2x_session_charge_wh));
        if (d.v2x_session_discharge_wh != null) sessionParts.push("out " + formatKwh(d.v2x_session_discharge_wh));
        var session = sessionParts.length ? sessionParts.join(" / ") : "—";
        var limitParts = [];
        if (d.v2x_charge_power_max_w != null) limitParts.push("charge " + formatW(d.v2x_charge_power_max_w));
        if (d.v2x_discharge_power_max_w != null) limitParts.push("discharge " + formatW(d.v2x_discharge_power_max_w));
        if (!limitParts.length && d.v2x_rated_power_w != null) limitParts.push("rated " + formatW(d.v2x_rated_power_w));
        var limits = limitParts.length ? limitParts.join(" / ") : "—";
        var mode = d.v2x_control_mode || d.v2x_protocol || "—";

        body =
          '<div class="driver-stats">' +
          '  <span class="stat-label">State</span><span class="stat-value ' + v2xClass + '">' + escHtml(statusLabel) + '</span>' +
          '  <span class="stat-label">Power</span><span class="stat-value">' + formatW(v2xWVal) + '</span>' +
          '  <span class="stat-label">Vehicle SoC</span><span class="stat-value">' + vehicleSoc + '</span>' +
          '  <span class="stat-label">DC</span><span class="stat-value">' + escHtml(dcSummary) + '</span>' +
          '  <span class="stat-label">Session</span><span class="stat-value">' + escHtml(session) + '</span>' +
          '  <span class="stat-label">Limits</span><span class="stat-value">' + escHtml(limits) + '</span>' +
          '  <span class="stat-label">Mode</span><span class="stat-value">' + escHtml(mode) + '</span>' +
          '  <span class="stat-label">Ticks</span><span class="stat-value">' + ticks + '</span>' +
          '  <span class="stat-label">Errors</span><span class="stat-value">' + errors + '</span>' +
          '</div>' +
          renderV2XControls(name, d);
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
      } else if (d.meter_w != null || d.pv_w != null || d.bat_w != null || d.bat_soc != null) {
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
          '  <span class="stat-label">PV</span><span class="stat-value">' + formatW(pvWVal) + "</span>" +
          batteryRow +
          '  <span class="stat-label">SoC</span><span class="stat-value">' + formatSoc(batSocVal) + "</span>" +
          '  <span class="stat-label">Ticks</span><span class="stat-value">' + ticks + "</span>" +
          '  <span class="stat-label">Errors</span><span class="stat-value">' + errors + "</span>" +
          "</div>" +
          '<div class="driver-soc-bar"><div class="driver-soc-fill" style="width:' + Math.round(batSocVal * 100) + '%"></div></div>';
      } else {
        // Metrics-only driver (e.g. MyUplink heat-pump telemetry): no
        // meter/pv/battery DER reading — don't render phantom 0 PV/SoC rows.
        body =
          '<div class="driver-stats">' +
          '  <span class="stat-label">Type</span><span class="stat-value stat-dim">telemetry only</span>' +
          '  <span class="stat-label">Ticks</span><span class="stat-value">' + ticks + "</span>" +
          '  <span class="stat-label">Errors</span><span class="stat-value">' + errors + "</span>" +
          "</div>";
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
        renderDriverActions(name, d);

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
  function fetchStatus() {
    fetch("/api/status")
      .then(function (res) {
        if (!res.ok) throw new Error("HTTP " + res.status);
        return res.json();
      })
      .then(function (data) {
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

  function setEvCharging(w) {
    postJson("/api/ev_charging", { power_w: w, active: w > 0 }).catch(function () {});
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

  gridTargetSlider.addEventListener("input", function () {
    gridTargetValue.textContent = formatW(Number(gridTargetSlider.value));
  });

  gridTargetSend.addEventListener("click", function () {
    setTarget(Number(gridTargetSlider.value));
  });

  peakLimitSlider.addEventListener("input", function () {
    peakLimitValue.textContent = formatW(Number(peakLimitSlider.value));
  });
  peakLimitSend.addEventListener("click", function () {
    setPeakLimit(Number(peakLimitSlider.value));
  });

  // EV detail modal
  var evModal = document.getElementById("ev-modal");
  var evModalBody = document.getElementById("ev-modal-body");
  var evModalClose = document.getElementById("ev-modal-close");
  var cardEv = document.getElementById("card-ev");

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

  // renderLoadpointSoCSection — operator-facing SoC correction UI +
  // target deadline input. Two inline controls per loadpoint:
  //
  //   (1) SoC correction — the Easee cloud API is blind to the
  //       vehicle's BMS; our current_soc is inferred from plug-in
  //       anchor + delivered_wh. If that drifts the operator re-
  //       anchors via POST /api/loadpoints/{id}/soc.
  //
  //   (2) Target SoC + deadline — the MPC uses the terminal
  //       penalty (mpc.go:445) to force a shortfall commitment
  //       when the EV can't reach target by target_time. Without a
  //       target there's no urgency penalty, so arbitrage mode
  //       rationally defers charging (there's no incentive — PV
  //       surplus can be exported for revenue instead). Posts to
  //       /api/loadpoints/{id}/target. target_time_ms == 0 means
  //       "no deadline — charge opportunistically."
  //
  // Target row is reachable even when unplugged — target is user
  // intent, valid to set before plugging in. Only SoC correction
  // requires an active session (anchor makes no sense otherwise).
  function renderLoadpointSoCSection(loadpoints) {
    var section = document.createElement("div");
    section.style.marginTop = "1rem";
    section.style.paddingTop = "0.75rem";
    section.style.borderTop = "1px solid var(--border)";
    var h = document.createElement("div");
    h.style.fontSize = "0.75rem";
    h.style.color = "var(--text-dim)";
    h.style.textTransform = "uppercase";
    h.style.letterSpacing = "0.04em";
    h.style.marginBottom = "0.5rem";
    h.textContent = "Loadpoints (planner)";
    section.appendChild(h);
    loadpoints.forEach(function (lp) {
      section.appendChild(buildLoadpointBlock(lp));
    });
    return section;
  }

  // buildLoadpointBlock returns a header + two rows (SoC correction,
  // target deadline) for one loadpoint.
  function buildLoadpointBlock(lp) {
    var block = document.createElement("div");
    block.style.marginBottom = "0.8rem";

    // Header: name + live state (power or plug status).
    var header = document.createElement("div");
    header.style.display = "flex";
    header.style.alignItems = "baseline";
    header.style.gap = "0.5rem";
    header.style.marginBottom = "0.3rem";
    var name = document.createElement("span");
    name.style.flex = "1";
    name.style.fontWeight = "600";
    name.textContent = lp.id;
    var state = document.createElement("span");
    state.style.fontSize = "0.75rem";
    state.style.color = "var(--text-dim)";
    state.textContent = lp.plugged_in
      ? (lp.current_power_w > 1
         ? Math.round(lp.current_power_w) + " W"
         : "plugged, idle")
      : "unplugged";
    header.appendChild(name);
    header.appendChild(state);
    block.appendChild(header);

    block.appendChild(buildSoCRow(lp));
    block.appendChild(buildSurplusOnlyRow(lp));
    block.appendChild(buildTargetRow(lp));
    return block;
  }

  // buildSurplusOnlyRow renders a single checkbox: when ticked, the
  // loadpoint is configured for "charge only from PV surplus". The
  // toggle is independent of any schedule — the controller's surplus
  // clamp harvests live export opportunistically with or without a
  // target+deadline. Adding a schedule on top is optional: it tells
  // the MPC the operator wants the car to reach X% by Y, which may
  // grid-import during cheap slots. Without a schedule the MPC simply
  // sits this loadpoint out and the runtime dispatch chases surplus.
  //
  // Patch semantics: both directions send only `{ surplus_only: ... }`
  // so the operator's existing target/deadline (if any) is preserved.
  function buildSurplusOnlyRow(lp) {
    var row = document.createElement("div");
    row.style.display = "flex";
    row.style.flexDirection = "column";
    row.style.gap = "0.25rem";
    row.style.marginBottom = "0.3rem";
    var label = document.createElement("label");
    label.style.display = "flex";
    label.style.alignItems = "center";
    label.style.gap = "0.4rem";
    label.style.fontSize = "0.8rem";
    label.style.cursor = "pointer";
    var cb = document.createElement("input");
    cb.type = "checkbox";
    cb.checked = !!lp.surplus_only;
    cb.title = "Charge only from real PV surplus. No grid import and no home-battery discharge. Add a schedule below to also catch up via grid when prices are cheap.";
    var text = document.createElement("span");
    text.textContent = "Surplus only (PV only — no grid or battery)";
    var hint = document.createElement("div");
    hint.style.fontSize = "0.72rem";
    hint.style.color = "var(--text-dim)";
    hint.style.marginLeft = "1.4rem";
    hint.textContent = "Charges only from real PV surplus. No deadline planning, no grid import, and no home-battery discharge. Optional: add a schedule below to also catch up via grid when cheap.";
    cb.addEventListener("change", function () {
      cb.disabled = true;
      var body = { surplus_only: cb.checked };
      fetch("/api/loadpoints/" + encodeURIComponent(lp.id) + "/target", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      }).then(function () {
        cb.disabled = false;
        refreshEvModal();
      }).catch(function () { cb.disabled = false; });
    });
    label.appendChild(cb);
    label.appendChild(text);
    row.appendChild(label);
    row.appendChild(hint);
    return row;
  }

  function buildSoCRow(lp) {
    var row = document.createElement("div");
    row.style.display = "flex";
    row.style.alignItems = "center";
    row.style.gap = "0.5rem";
    row.style.marginBottom = "0.3rem";
    var socLabel = document.createElement("span");
    socLabel.style.fontSize = "0.75rem";
    socLabel.style.color = "var(--text-dim)";
    socLabel.style.width = "4rem";
    socLabel.textContent = "SoC now:";
    var socInput = document.createElement("input");
    socInput.type = "number";
    socInput.min = "0";
    socInput.max = "100";
    socInput.step = "1";
    styleNumberInput(socInput);
    socInput.disabled = !lp.plugged_in;
    socInput.title = lp.plugged_in
      ? "Correct to what your car actually shows"
      : "Plug in the car to set SoC";
    socInput.value = lp.plugged_in && lp.current_soc_pct != null
      ? Math.round(lp.current_soc_pct) : "";
    var btn = document.createElement("button");
    btn.textContent = "Set";
    btn.className = "btn-send";
    btn.style.padding = "0.2rem 0.55rem";
    btn.style.fontSize = "0.75rem";
    btn.disabled = !lp.plugged_in;
    btn.addEventListener("click", function () {
      var v = Number(socInput.value);
      if (!(v >= 0 && v <= 100)) return;
      btn.disabled = true; btn.textContent = "…";
      fetch("/api/loadpoints/" + encodeURIComponent(lp.id) + "/soc", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ soc_pct: v }),
      }).then(function (r) {
        btn.textContent = r.ok ? "✓" : "×";
        setTimeout(function () {
          btn.textContent = "Set"; btn.disabled = !lp.plugged_in;
          refreshEvModal();
        }, 800);
      }).catch(function () {
        btn.textContent = "×";
        setTimeout(function () { btn.textContent = "Set"; btn.disabled = false; }, 1200);
      });
    });
    row.appendChild(socLabel);
    row.appendChild(socInput);
    row.appendChild(document.createTextNode("%"));
    row.appendChild(btn);
    return row;
  }

  // buildTargetRow — "Target: [SoC] by [datetime] [Save] [Clear]".
  // Sets target_soc_pct + target_time on the loadpoint. Empty time
  // input → no deadline (still sets the SoC target, which does
  // nothing alone — the DP only applies the shortfall penalty
  // when a deadline is present). Clear posts zeroes so the
  // manager drops both fields.
  function buildTargetRow(lp) {
    var row = document.createElement("div");
    row.style.display = "flex";
    row.style.alignItems = "center";
    row.style.gap = "0.5rem";
    row.style.flexWrap = "wrap";
    var label = document.createElement("span");
    label.style.fontSize = "0.75rem";
    label.style.color = "var(--text-dim)";
    label.style.width = "4rem";
    label.textContent = "Target:";
    var socInput = document.createElement("input");
    socInput.type = "number";
    socInput.min = "0";
    socInput.max = "100";
    socInput.step = "1";
    styleNumberInput(socInput);
    socInput.title = "Target SoC % the MPC plans toward";
    if (lp.target_soc_pct > 0) socInput.value = Math.round(lp.target_soc_pct);
    var byLabel = document.createElement("span");
    byLabel.style.fontSize = "0.75rem";
    byLabel.style.color = "var(--text-dim)";
    byLabel.textContent = "% by";
    var timeInput = document.createElement("input");
    timeInput.type = "datetime-local";
    timeInput.style.background = "var(--bg)";
    timeInput.style.color = "var(--text)";
    timeInput.style.border = "1px solid var(--border)";
    timeInput.style.borderRadius = "3px";
    timeInput.style.padding = "0.2rem 0.35rem";
    timeInput.style.fontSize = "0.8rem";
    timeInput.title = "Deadline for reaching the target SoC — empty = no deadline";
    var parsed = parseTargetTime(lp.target_time);
    if (parsed) timeInput.value = toLocalInputValue(parsed);
    var saveBtn = document.createElement("button");
    saveBtn.textContent = "Save";
    saveBtn.className = "btn-send";
    saveBtn.style.padding = "0.2rem 0.55rem";
    saveBtn.style.fontSize = "0.75rem";
    saveBtn.addEventListener("click", function () {
      var soc = Number(socInput.value);
      if (!(soc >= 0 && soc <= 100)) return;
      var timeMs = 0;
      if (timeInput.value) {
        var d = new Date(timeInput.value);
        if (!isNaN(d.getTime())) timeMs = d.getTime();
      }
      postTarget(lp.id, soc, timeMs, saveBtn);
    });
    var clearBtn = document.createElement("button");
    clearBtn.textContent = "Clear";
    clearBtn.className = "btn-send";
    clearBtn.style.padding = "0.2rem 0.55rem";
    clearBtn.style.fontSize = "0.75rem";
    clearBtn.style.background = "var(--bg)";
    clearBtn.title = "Drop the target — charge opportunistically";
    clearBtn.disabled = !(lp.target_soc_pct > 0 || parsed);
    clearBtn.addEventListener("click", function () {
      socInput.value = "";
      timeInput.value = "";
      postTarget(lp.id, 0, 0, clearBtn);
    });
    row.appendChild(label);
    row.appendChild(socInput);
    row.appendChild(byLabel);
    row.appendChild(timeInput);
    row.appendChild(saveBtn);
    row.appendChild(clearBtn);
    return row;
  }

  function postTarget(id, socPct, targetTimeMs, btn) {
    var originalLabel = btn.textContent;
    btn.disabled = true; btn.textContent = "…";
    fetch("/api/loadpoints/" + encodeURIComponent(id) + "/target", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ soc_pct: socPct, target_time_ms: targetTimeMs }),
    }).then(function (r) {
      btn.textContent = r.ok ? "✓" : "×";
      setTimeout(function () {
        btn.textContent = originalLabel;
        btn.disabled = false;
        refreshEvModal();
      }, 800);
    }).catch(function () {
      btn.textContent = "×";
      setTimeout(function () {
        btn.textContent = originalLabel;
        btn.disabled = false;
      }, 1200);
    });
  }

  function styleNumberInput(input) {
    input.style.width = "4.5rem";
    input.style.background = "var(--bg)";
    input.style.color = "var(--text)";
    input.style.border = "1px solid var(--border)";
    input.style.borderRadius = "3px";
    input.style.padding = "0.2rem 0.35rem";
  }

  // parseTargetTime accepts the loadpoint's target_time (RFC3339
  // string or undefined) and returns a Date — or null when the
  // value is absent/zero. Go's json.Marshal doesn't omit zero
  // time.Time even with omitempty, so we treat anything before
  // year 2000 as "unset" to handle both the missing-field and
  // 0001-01-01 cases without a format sniff.
  function parseTargetTime(s) {
    if (!s) return null;
    var d = new Date(s);
    if (isNaN(d.getTime())) return null;
    if (d.getFullYear() < 2000) return null;
    return d;
  }

  // toLocalInputValue turns a Date into the "YYYY-MM-DDTHH:MM"
  // string a datetime-local input expects (local time, no TZ).
  function toLocalInputValue(date) {
    var tz = date.getTimezoneOffset() * 60000;
    return new Date(date - tz).toISOString().slice(0, 16);
  }

  function refreshEvModal() {
    // Fetch both the legacy EV status AND the loadpoint manager state
    // — the latter is what the MPC actually optimizes against. Shown
    // as a separate section so operators can correct the inferred
    // vehicle SoC (our infer is blind without a vehicle API).
    Promise.all([
      fetch("/api/ev/status").then(function (r) { return r.json(); }).catch(function () { return null; }),
      fetch("/api/loadpoints").then(function (r) { return r.json(); }).catch(function () { return null; }),
    ]).then(function (pair) {
      var ev = pair[0];
      var lps = pair[1];
      evModalBody.textContent = "";
      if (ev && ev.connected !== false) {
        evModalBody.appendChild(renderEvStatusTable(ev));
      } else if (!lps || !lps.loadpoints || lps.loadpoints.length === 0) {
        setEvModalMessage("No EV charger connected");
        return;
      }
      if (lps && lps.enabled && lps.loadpoints && lps.loadpoints.length > 0) {
        evModalBody.appendChild(renderLoadpointSoCSection(lps.loadpoints));
      }
    });
  }

  var evRefreshTimer = null;
  if (cardEv && evModal) {
    var evBtnStart = document.getElementById("ev-btn-start");
    var evBtnPause = document.getElementById("ev-btn-pause");
    var evBtnResume = document.getElementById("ev-btn-resume");
    var evActionBtns = [evBtnStart, evBtnPause, evBtnResume];
    // Focus-trap bounds — first and last focusable controls in the modal.
    // Tab from the last wraps to the first and vice versa.
    var evFocusable = [evModalClose, evBtnStart, evBtnPause, evBtnResume];
    var evLastFocused = null;

    function openEvModal() {
      evLastFocused = document.activeElement;
      evModal.classList.remove("hidden");
      evModal.setAttribute("aria-hidden", "false");
      refreshEvModal();
      // Guard against stacked timers if the card is clicked while the
      // modal is still open (e.g. background click that didn't close).
      if (evRefreshTimer) { clearInterval(evRefreshTimer); }
      evRefreshTimer = setInterval(refreshEvModal, 5000);
      // Focus lands on the close button so ESC/Enter work immediately.
      setTimeout(function () { evModalClose.focus(); }, 0);
    }
    function closeEvModal() {
      evModal.classList.add("hidden");
      evModal.setAttribute("aria-hidden", "true");
      if (evRefreshTimer) { clearInterval(evRefreshTimer); evRefreshTimer = null; }
      if (evLastFocused && typeof evLastFocused.focus === "function") {
        evLastFocused.focus();
      }
    }
    function isEvModalOpen() { return !evModal.classList.contains("hidden"); }

    cardEv.addEventListener("click", openEvModal);
    // The card has role="button" + tabindex="0" so it's keyboard-focusable;
    // WAI-ARIA requires Enter + Space to activate a role="button" element.
    cardEv.addEventListener("keydown", function (e) {
      if (e.key === "Enter" || e.key === " ") {
        e.preventDefault();
        openEvModal();
      }
    });
    evModalClose.addEventListener("click", closeEvModal);
    evModal.addEventListener("click", function (e) {
      if (e.target === evModal) closeEvModal();
    });
    // ESC-to-close + Tab focus trap. Attached to the modal itself so the
    // listener is only live while one of its descendants has focus.
    evModal.addEventListener("keydown", function (e) {
      if (!isEvModalOpen()) return;
      if (e.key === "Escape") {
        e.preventDefault();
        closeEvModal();
        return;
      }
      if (e.key !== "Tab") return;
      var enabled = evFocusable.filter(function (b) { return b && !b.disabled; });
      if (enabled.length === 0) return;
      var first = enabled[0];
      var last = enabled[enabled.length - 1];
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault();
        first.focus();
      }
    });

    function evCommand(action) {
      // Disable all three action buttons while any command is inflight so
      // a Pause→Resume double-click can't send both. The close button
      // stays enabled so the user can always dismiss the modal.
      evActionBtns.forEach(function (b) { b.disabled = true; });
      postJson("/api/ev/command", { action: action })
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
      ["#3b82f6", "Charged"], ["#f59e0b", "Discharged"], ["var(--fg)", "Load"],
    ] : [
      ["#ef4444", "Grid"], ["#22c55e", "PV"], ["var(--fg)", "Load"],
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
        // Reset the dynamic battery set and rediscover from the history
        // items themselves — drivers that existed earlier but no longer
        // appear in /api/status will simply not be recreated.
        chartBatteries = {};
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

          // Per-battery discovery from this item's drivers + targets maps.
          var itDrivers = it.drivers || {};
          var itTargets = it.targets || {};
          var seen = {};
          Object.keys(itDrivers).forEach(function (name) {
            var d = itDrivers[name] || {};
            if (d.bat_w == null) return;
            seen[name] = true;
            var slot = ensureBatteryDriver(name);
            slot.bat.push(d.bat_w || 0);
            slot.target.push(itTargets[name] || 0);
          });
          Object.keys(chartBatteries).forEach(function (name) {
            if (seen[name]) return;
            var slot = chartBatteries[name];
            slot.bat.push(0);
            slot.target.push(0);
          });
        });
        syncBatteryLegend();
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

  // ---- Init ----
  loadHistory(chartRange);
  fetchStatus();
  setInterval(fetchStatus, POLL_INTERVAL);
  requestAnimationFrame(animationFrame);
})();
