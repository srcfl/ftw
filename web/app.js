// FTW dashboard — plain JS, no framework

(function () {
  "use strict";

  const POLL_INTERVAL = 2000;        // status poll cadence — snappier cards
  const historyMigrationUI = import("/history-migration.js").catch(function () { return null; });
  function updateHistoryMigration(health) {
    historyMigrationUI.then(function (ui) { if (ui) ui.updateMigrationBanner(health); }).catch(function () {});
    if (updateBadge && typeof updateBadge.setBootHealth === "function") updateBadge.setBootHealth(health);
  }

  // Prices arrive as minor units per kWh; what to call them depends on the
  // configured currency. window.FTWUnits is set when
  // components/price-units.js loads — same read-at-use-time pattern as
  // the energy-flow idle threshold, with öre as the no-modules fallback.
  function fmtPricePerKwh(minorPerKwh) {
    const u = typeof window !== "undefined" && window.FTWUnits;
    if (!u) return minorPerKwh.toFixed(0) + " öre/kWh";
    return u.formatPricePerKwh(minorPerKwh, u.activeCurrency());
  }
  const CHART_POINTS = 360;          // up to 30 min of points (server pushes every ~5s)
  const CHART_RANGE_MS = {           // visible time window per range option
    "5m": 5 * 60 * 1000,
    "15m": 15 * 60 * 1000,
    "1h": 60 * 60 * 1000,
    "6h": 6 * 60 * 60 * 1000,
    "24h": 24 * 60 * 60 * 1000,
    "3d": 3 * 24 * 60 * 60 * 1000,
  };
  const CHART_SMOOTH_MS = {
    "5m": 10 * 1000,
    "15m": 20 * 1000,
    "1h": 60 * 1000,
    "6h": 5 * 60 * 1000,
    "24h": 15 * 60 * 1000,
    "3d": 30 * 60 * 1000,
  };
  const STATUS_DISPLAY_TAU_MS = 8 * 1000;
  let chartRange = "5m";             // current selected range
  let currentMode = null;
  let lastRevealedMode = null;       // last mode the manual drawer auto-opened for
  let pendingMode = null;            // mode tapped here, not yet confirmed by the server
  let pendingModeUntil = 0;          // browser-clock deadline for that optimistic paint
  let animating = !document.hidden;  // 30fps redraw loop flag
  let lastDataTs = 0;                // browser-clock timestamp of newest pushed point
  let lastPushAt = 0;                // browser-clock timestamp of last push attempt — for dedupe (NEVER mix with server ts)
  let lastFlashAt = 0;               // browser-clock timestamp of last "new data" flash

  // Kept as a named helper because components share the same local API surface.
  function apiFetch(path, opts) {
    return fetch(path, opts || {});
  }

  // A single-flight read must always settle. Without a deadline, one half-open
  // request would keep its lock or cache entry forever and every later timer
  // would reuse work that can no longer finish. Keep the deadline through JSON
  // decoding too: receiving headers does not mean the response body arrived.
  const API_READ_TIMEOUT_MS = 15_000;
  function boundedApiRead(path, decode) {
    var controller = new AbortController();
    var timer = setTimeout(function () { controller.abort(); }, API_READ_TIMEOUT_MS);
    var request;
    try {
      request = apiFetch(path, { signal: controller.signal });
    } catch (err) {
      clearTimeout(timer);
      return Promise.reject(err);
    }
    return request.then(decode).finally(function () { clearTimeout(timer); });
  }

  function evWrite(path, options) {
    var controller = new AbortController();
    var timer = setTimeout(function () { controller.abort(); }, 30000);
    return apiFetch(path, Object.assign({}, options, { signal: controller.signal }))
      .then(function (response) {
        // Keep the deadline until the body arrives, including an error body.
        return response.arrayBuffer().then(function (body) {
          return new Response(response.status === 204 ? null : body, { status: response.status, statusText: response.statusText, headers: response.headers });
        });
      })
      .catch(function (e) {
        if (e && e.name === "AbortError") throw new Error("FTW has not confirmed the request. Check its current state before trying again");
        throw e;
      }).finally(function () { clearTimeout(timer); });
  }

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
  var statusDisplayState = {};

  // Resolve a CSS custom property off :root at call time. Lazy on
  // purpose: a runtime theme toggle rewrites the oklch values, so we
  // re-read every paint rather than caching at module load.
  function cssVar(name) {
    try {
      var v = getComputedStyle(document.documentElement)
        .getPropertyValue(name).trim();
      return v || null;
    } catch (_) {
      return null;
    }
  }

  // Deterministic color palette for battery series — each driver gets a
  // stable color based on name hash so reload is consistent.
  var BATTERY_PALETTE = [
    "#f59e0b", "#8b5cf6", "#ec4899", "#06b6d4",
    "#eab308", "#14b8a6", "#f43f5e", "#a855f7",
  ];
  function batteryColor(name) {
    // Route the "pixii" driver through the theme's --violet token so
    // it visibly separates from "laddning bil" (which hashes to the
    // same purple slot in EV_PALETTE). Other names fall through.
    if (name === "pixii") return cssVar('--violet') || BATTERY_PALETTE[7];
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
    // Route "laddning bil" through the theme's --cyan token so it
    // doesn't collide with the Pixii battery purple (both hash to
    // the same purple slot in their respective palettes).
    if (name === "laddning bil") return cssVar('--cyan') || EV_PALETTE[6];
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

  // Latest MPC plan for the live chart's forward-looking dashed PV +
  // Load forecast. plan.js owns the sole plan poll and publishes each
  // result in-page so the chart and both briefing surfaces stay aligned.
  var chartPlan = null;
  window.addEventListener("ftw-plan-data", function (event) {
    chartPlan = event && event.detail ? event.detail.plan : null;
  });
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
  // The header's status light lives inside <ftw-update-badge> so one slot
  // decides what the corner says — connection, updates and optimizer
  // health used to be separate elements hiding each other with CSS.
  const updateBadge = document.querySelector("ftw-update-badge");
  const overviewHealth = $("overview-health");
  const overviewHealthLabel = $("overview-health-label");
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
  const fuseUse = $("fuse-use");
  const fuseFill = $("fuse-fill");
  const fusePhases = $("fuse-phases");
  const eImport = $("e-import");
  const eExport = $("e-export");
  const ePv = $("e-pv");
  const eCharged = $("e-charged");
  const eDischarged = $("e-discharged");
  const eLoad = $("e-load");
  const overviewEImport = $("overview-e-import");
  const overviewEExport = $("overview-e-export");
  const overviewEPv = $("overview-e-pv");
  const lastUpdate = $("last-update");
  const versionEl = $("version");
  // ---- Formatting ----
  function formatW(w) {
    if (w == null || !isFinite(w)) return "—";
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
  function formatSignedWh(wh) {
    var v = Number(wh) || 0;
    var abs = Math.abs(v);
    var sign = v > 0.5 ? "+" : v < -0.5 ? "-" : "±";
    if (abs >= 1000) return sign + (abs / 1000).toFixed(2) + " kWh";
    return sign + Math.round(abs) + " Wh";
  }
  function formatWhMagnitude(wh) {
    var abs = Math.abs(Number(wh) || 0);
    if (abs >= 1000) return (abs / 1000).toFixed(2) + " kWh";
    return Math.round(abs) + " Wh";
  }

  // Live-stats strip helpers. signClass picks a colour class based on
  // direction (import/export for grid, charging/discharging for
  // battery). Used by the stats strip just above the live power
  // chart. Threshold matches the live chart's idle band (±10 W) so
  // tiny noise doesn't flip the colour every poll.
  function signClass(kind, w) {
    var v = Number(w) || 0;
    if (Math.abs(v) <= 10) return "is-neutral";
    if (kind === "grid") return v > 0 ? "is-import" : "is-export";
    if (kind === "bat") return v > 0 ? "is-charging" : "is-discharging";
    return "is-neutral";
  }
  function updateLiveStat(key, w, cls) {
    var el = document.getElementById("live-stat-" + key);
    if (!el) return;
    el.textContent = formatW(w);
    el.className = "live-stat-value " + (cls || "is-neutral");
  }
  function updateLiveSocStat(soc) {
    var el = document.getElementById("live-stat-soc");
    if (!el) return;
    if (soc == null || !isFinite(soc)) { el.textContent = "—"; return; }
    el.textContent = (soc * 100).toFixed(1) + " %";
    el.className = "live-stat-value is-neutral";
  }
  function batteryTargetLine(targetW) {
    if (targetW == null || !isFinite(targetW)) return "";
    // Just the target — the "· charging/discharging" suffix overflowed the
    // node circle; the live W value + SoC% already convey direction.
    return "target " + formatW(targetW);
  }

  function hasControllableBatteryDrivers(data) {
    var drvs = (data && data.drivers) || {};
    return Object.keys(drvs).some(function (name) {
      var d = drvs[name] || {};
      return d.bat_w != null && !d.observe_only;
    });
  }

  function smoothDisplayNumber(key, value, now) {
    if (value == null || !isFinite(value)) return value;
    var prev = statusDisplayState[key];
    if (!prev || !isFinite(prev.value)) {
      statusDisplayState[key] = { value: value, ts: now };
      return value;
    }
    var dt = Math.max(0, now - prev.ts);
    var alpha = 1 - Math.exp(-dt / STATUS_DISPLAY_TAU_MS);
    var next = prev.value + (value - prev.value) * alpha;
    statusDisplayState[key] = { value: next, ts: now };
    return next;
  }

  function smoothStatusForDisplay(data) {
    var now = Date.now();
    var out = Object.assign({}, data);
    ["grid_w", "pv_w", "bat_w", "ev_w", "ev_charging_w"].forEach(function (field) {
      if (out[field] != null) out[field] = smoothDisplayNumber("site:" + field, out[field], now);
    });
    var drivers = {};
    var sums = { pv_w: 0, bat_w: 0, ev_w: 0 };
    var have = { pv_w: false, bat_w: false, ev_w: false };
    Object.keys(data.drivers || {}).forEach(function (name) {
      var d = Object.assign({}, data.drivers[name] || {});
      ["meter_w", "pv_w", "bat_w", "ev_w"].forEach(function (field) {
        if (d[field] != null) d[field] = smoothDisplayNumber("driver:" + name + ":" + field, d[field], now);
      });
      var online = d.status !== "offline" && d.status !== "disabled" && !d.not_running;
      if (online) {
        ["pv_w", "bat_w", "ev_w"].forEach(function (field) {
          if (d[field] != null) {
            sums[field] += d[field] || 0;
            have[field] = true;
          }
        });
      }
      drivers[name] = d;
    });
    out.drivers = drivers;
    if (have.pv_w) out.pv_w = sums.pv_w;
    if (have.bat_w) out.bat_w = sums.bat_w;
    if (have.ev_w) {
      out.ev_w = sums.ev_w;
      out.ev_charging_w = sums.ev_w;
    }
    if (out.grid_w != null) {
      out.load_w = Math.max(0, (out.grid_w || 0) - (out.bat_w || 0) - (out.pv_w || 0) - (out.ev_w || 0));
    } else {
      // No live meter: do not keep a previous load or treat JSON null as 0 W.
      out.load_w = null;
    }
    return out;
  }

  function smoothSeriesForChart(values, timestamps, windowMs) {
    if (!windowMs || windowMs <= 0 || values.length < 3) return values;
    var out = new Array(values.length);
    for (var i = 0; i < values.length; i++) {
      var t = timestamps[i];
      var sum = 0;
      var n = 0;
      for (var j = i; j >= 0; j--) {
        if (t - timestamps[j] > windowMs) break;
        var v = values[j];
        if (v == null || !isFinite(v)) continue;
        sum += v;
        n++;
      }
      out[i] = n > 0 ? sum / n : values[i];
    }
    return out;
  }

  function statusClass(status) {
    if (!status) return "status-offline";
    const s = status.toLowerCase();
    if (s === "ok") return "status-ok";
    if (s === "degraded") return "status-degraded";
    return "status-offline";
  }

  // The component registry loads as a deferred module while this classic
  // dashboard script starts immediately at the end of the document. A fast
  // first /api/status response can therefore arrive before <ftw-energy-flow>
  // has upgraded, and before energy-flow-readings.js has assigned the
  // mapper. Cache the raw payload (not only the derived readings) and
  // replay when either the mapper or the element is ready, so the hero
  // does not sit in its loading skeleton until the next two-second poll.
  var lastFlowStatus = null;
  var lastFlowReadings = null;
  var flowUpgradeReplayQueued = false;

  window.ftwOnFlowMapperReady = function () {
    if (lastFlowStatus) paintEnergyFlow(lastFlowStatus);
  };

  function energyFlowOpts(data) {
    var byDriver = {};
    (data.dispatch || []).forEach(function (d) {
      if (d && d.driver) byDriver[d.driver] = Number(d.target_w) || 0;
    });
    return {
      idleW: (typeof window.FTW_FLOW_IDLE_W === "number" && window.FTW_FLOW_IDLE_W) || 42,
      batterySub: function (name, d) {
        if (d.observe_only) return "observe only";
        return batteryTargetLine(byDriver[name]);
      },
    };
  }

  function queueFlowReplay() {
    if (flowUpgradeReplayQueued) return;
    flowUpgradeReplayQueued = true;
    if (window.customElements && typeof window.customElements.whenDefined === "function") {
      window.customElements.whenDefined("ftw-energy-flow").then(function () {
        if (lastFlowStatus) paintEnergyFlow(lastFlowStatus);
      });
    }
  }

  function paintEnergyFlow(data) {
    lastFlowStatus = data;
    var flowEl = document.getElementById("energy-flow");
    if (!flowEl) return;
    if (typeof window.ftwFlowReadingsFromStatus !== "function") {
      queueFlowReplay();
      return;
    }
    lastFlowReadings = window.ftwFlowReadingsFromStatus(data, energyFlowOpts(data));
    (lastFlowReadings.planets || []).forEach(function (p) {
      if (p.role !== "ev" || p.placeholder || !p.name) return;
      var lpEv = loadpointsByDriver && loadpointsByDriver[p.name];
      if (!lpEv) return;
      if (lpEv.vehicle_soc > 0) {
        p.soc = lpEv.vehicle_soc * 100;
        p.socSource = "vehicle";
      } else if (lpEv.current_soc > 0 && lpEv.soc_source !== "assumed") {
        p.soc = lpEv.current_soc * 100;
        p.socSource = lpEv.soc_source || "inferred";
      }
      if (lpEv.vehicle_charge_limit > 0) {
        p.chargeLimit = lpEv.vehicle_charge_limit * 100;
      }
      p.socStale = !!lpEv.vehicle_stale;
    });
    if (typeof flowEl.setReadings === "function") {
      flowEl.setReadings(lastFlowReadings);
    } else {
      queueFlowReplay();
    }
  }

  // ---- Render ----
  function render(data) {
    var batteryTargetsByDriver = {};
    var totalBatteryTargetW = 0;
    var hasBatteryTarget = false;
    (data.dispatch || []).forEach(function (d) {
      if (!d || !d.driver) return;
      var target = Number(d.target_w) || 0;
      batteryTargetsByDriver[d.driver] = target;
      totalBatteryTargetW += target;
      hasBatteryTarget = true;
    });

    // Battery presence → body.no-battery toggle. Drives visibility of
    // the "Bat charged" / "Bat discharged" tiles and the Plan chart's
    // Charge / Discharge / SoC legend + drawing layers. Any driver
    // exposing bat_w counts; if the current tick has zero such
    // drivers the class goes on and the CSS in app.css hides
    // everything tagged .bat-only. Re-evaluated every /api/status
    // poll so plugging in a battery lights everything back up
    // without a reload.
    var hasBattery = false;
    var hasControllableBattery = false;
    var drvMap = data.drivers || {};
    for (var drvName in drvMap) {
      if (drvMap[drvName] && drvMap[drvName].bat_w != null) {
        hasBattery = true;
        if (!drvMap[drvName].observe_only) hasControllableBattery = true;
      }
    }
    document.body.classList.toggle("no-battery", !hasBattery);
    var cardBatEl = document.getElementById("card-bat");
    if (cardBatEl) {
      cardBatEl.classList.toggle("clickable", hasControllableBattery);
      if (hasControllableBattery) {
        cardBatEl.setAttribute("role", "button");
        cardBatEl.setAttribute("tabindex", "0");
      } else {
        cardBatEl.removeAttribute("role");
        cardBatEl.removeAttribute("tabindex");
      }
    }

    // PUSH CHART DATA FIRST — never let a DOM render error somewhere below
    // silently kill the chart-update path. (Prior bug: missing #dispatch-list
    // threw inside renderDispatch, which is between renderDrivers and
    // pushChartData, so the chart starved while ticks kept incrementing.)
    try {
      // Build a {driver → target_w} index from the dispatch array so the
      // per-battery push below doesn't need an inner loop.
      pushChartData(data, batteryTargetsByDriver);
    } catch (e) { console.error("pushChartData error:", e); }

    data = smoothStatusForDisplay(data);

    // Version (live from API — survives stale browser cache of index.html)
    if (versionEl && data.version) {
      versionEl.textContent = data.version;
    }
    // Grid + target indicator. A stale meter arrives as JSON null, not 0 —
    // 0 W is a real balanced house and must not be used as a missing-data
    // stand-in.
    if (data.grid_w == null || !isFinite(data.grid_w)) {
      gridW.textContent = "—";
      gridDir.textContent = "no data";
      gridW.className = "card-value val-neutral";
    } else {
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
    }
    var targetDisp = document.getElementById("grid-target-display");
    if (targetDisp) {
      var t = data.grid_target_w || 0;
      targetDisp.textContent = t === 0 ? "target 0" : "target " + formatW(t);
    }
    var slotBadge = document.getElementById("grid-slot-badge");
    if (slotBadge) {
      var slot = data.energy && data.energy.current_slot;
      if (slot) {
        // 15-min settlement: import_wh and export_wh accumulate
        // SEPARATELY within the slot — the bill is import × import_price
        // plus export × export_price, never their net. Render both
        // directions so the operator sees the slot's true exposure.
        var impWh = Number(slot.import_wh) || 0;
        var expWh = Number(slot.export_wh) || 0;
        slotBadge.textContent = "15m ↑" + formatWhMagnitude(impWh) + " ↓" + formatWhMagnitude(expWh);
        slotBadge.className = "grid-slot-badge" +
          (impWh > expWh + 5 ? " slot-import" : expWh > impWh + 5 ? " slot-export" : "");
      } else {
        slotBadge.textContent = "";
        slotBadge.className = "grid-slot-badge";
      }
    }

    // PV — stored as negative (site convention) but displayed positive
    // so "SOLAR 5.3 kW" reads as generation magnitude without the minus.
    // Internal data (chart history, hero setReadings, plan math) stays
    // on site convention — flip is for this tile only. Configured-but-
    // offline solar is "—", not a pretend 0 W of generation.
    var pvLive = Object.keys(data.drivers || {}).some(function (name) {
      var d = data.drivers[name] || {};
      var online = window.ftwDriverOnline
        ? window.ftwDriverOnline(d)
        : d.status !== "offline" && d.status !== "disabled" && !d.not_running;
      return online && d.pv_w != null;
    });
    var pvConfigured = Object.keys(data.drivers || {}).some(function (name) {
      return (data.drivers[name] || {}).pv_w != null;
    });
    var pvDir = document.getElementById("pv-dir");
    if (pvConfigured && !pvLive) {
      pvW.textContent = "—";
      pvW.className = "card-value val-neutral";
      if (pvDir) pvDir.textContent = "no data";
    } else {
      pvW.textContent = formatW(-(data.pv_w || 0));
      pvW.className = "card-value val-generation";
      if (pvDir) pvDir.textContent = "generating";
    }

    // Load
    loadW.textContent = formatW(data.load_w);
    var loadDir = document.getElementById("load-dir");
    if (loadDir) {
      loadDir.textContent = data.load_w == null || !isFinite(data.load_w) ? "no data" : "using now";
    }

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

    // Battery — positive=charge, negative=discharge. Same honesty as solar:
    // a configured pack that is not reporting is "—", not idle at 0 W.
    var batLive = Object.keys(data.drivers || {}).some(function (name) {
      var d = data.drivers[name] || {};
      var online = window.ftwDriverOnline
        ? window.ftwDriverOnline(d)
        : d.status !== "offline" && d.status !== "disabled" && !d.not_running;
      return online && d.bat_w != null;
    });
    var batConfigured = Object.keys(data.drivers || {}).some(function (name) {
      return (data.drivers[name] || {}).bat_w != null;
    });
    if (batConfigured && !batLive) {
      batW.textContent = "—";
      batDir.textContent = "no data";
      batW.className = "card-value val-neutral";
    } else {
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
    }
    if (batSoc) {
      batSoc.textContent = (batConfigured && !batLive) || !Number.isFinite(data.bat_soc)
        ? "—"
        : Math.round(data.bat_soc * 100) + "% SoC";
    }
    var batTargetDisp = document.getElementById("bat-target-display");
    if (batTargetDisp) {
      batTargetDisp.textContent = hasBatteryTarget ? batteryTargetLine(totalBatteryTargetW) : "";
    }

    // Live stats strip — mirrors the per-tile values up top but in one
    // mono-typed line above the live chart, so the Live card matches
    // the Plan card's information density. Each cell colours by sign
    // / direction so an operator scans the row and immediately sees
    // who's importing, exporting, charging, or idle.
    // Same honesty as the Values tiles: site-level pv_w / bat_w / bat_soc
    // become 0 when every reporter of that role is offline. 0 W here would
    // undo the "—" those tiles just drew.
    var pvStat = pvConfigured && !pvLive ? null : -(data.pv_w || 0);
    var batStat = batConfigured && !batLive ? null : data.bat_w;
    var socStat = batConfigured && !batLive ? null : data.bat_soc;
    updateLiveStat("grid", data.grid_w, signClass("grid", data.grid_w));
    updateLiveStat("pv", pvStat, pvStat == null ? "is-neutral" : "is-export");
    updateLiveStat("load", data.load_w, "is-neutral");
    updateLiveStat("bat", batStat, batStat == null ? "is-neutral" : signClass("bat", batStat));
    updateLiveSocStat(socStat);

    // Hero energy-flow diagram. Mapping lives in energy-flow-readings.js
    // so a stale meter cannot be drawn as 0 W "balanced" and a quiet
    // hybrid inverter cannot vanish from the X. EV SoC is overlayed in
    // paintEnergyFlow because it comes from the loadpoint table, not
    // /api/status.
    paintEnergyFlow(data);

    // Mode buttons — primary (strategy) + advanced (manual)
    currentMode = data.mode;
    // Buttons come from GET /api/modes; if that hasn't landed yet (offline at
    // first paint), this confirmed-live poll is the retry trigger.
    if (!modeCatalogRendered) renderModeCatalog();
    // A tap paints its button before the POST returns. A status read already
    // in flight still answers with the old mode, so prefer the tapped one
    // until the server confirms it or the short wait runs out.
    if (pendingMode && (data.mode === pendingMode || Date.now() >= pendingModeUntil)) pendingMode = null;
    var activeMode = pendingMode || data.mode;
    var allModeButtons = document.querySelectorAll("#mode-buttons-primary button, #mode-buttons button");
    allModeButtons.forEach(function (btn) {
      if (btn.dataset.mode === activeMode) btn.classList.add("active");
      else btn.classList.remove("active");
    });
    revealManualModes(activeMode);
    // When planner is driving, grey out the grid-target slider and show a hint.
    var plannerActive = (data.mode || "").indexOf("planner_") === 0;
    // "Use the plan" is the way out of a manual mode. It has nothing to
    // offer while the planner already drives, so it only shows when it does.
    var planUseRow = document.getElementById("plan-use-row");
    var planUseBtn = document.getElementById("plan-use-btn");
    if (planUseBtn) planUseBtn.hidden = plannerActive;
    if (planUseRow) planUseRow.hidden = plannerActive;
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
      // signal is the kind of redundancy the shared design system warns about.
      peakLimitValue.textContent = formatW(display);
      if (enabled) writeLastPeakLimitW(peakSrcW);
      if (peakLimitSend) peakLimitSend.disabled = true; // pristine
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
      if (overviewEImport) overviewEImport.textContent = formatKwh(t.import_wh);
      if (overviewEExport) overviewEExport.textContent = formatKwh(t.export_wh);
      if (overviewEPv) overviewEPv.textContent = formatKwh(t.pv_wh);
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
        return '<span class="' + cls + '">' + dot + " " + escHtml(n) + "</span>";
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
    chartHistory.load.push(data.load_w);
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
    var chartSmoothMs = CHART_SMOOTH_MS[chartRange] || 0;
    var smoothedPVSeries = null;
    var smoothedLoadSeries = null;
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
      var smoothedGridSeries = smoothSeriesForChart(chartHistory.grid, chartHistory.timestamps, chartSmoothMs);
      smoothedPVSeries = smoothSeriesForChart(chartHistory.pv, chartHistory.timestamps, chartSmoothMs);
      smoothedLoadSeries = smoothSeriesForChart(chartHistory.load, chartHistory.timestamps, chartSmoothMs);
      series = [
        { data: smoothedGridSeries, color: "#ef4444", width: 2,   dash: [], name: "Grid", fill: true,  toggle: "grid" },
        { data: smoothedPVSeries,   color: "#22c55e", width: 2,   dash: [], name: "PV",   fill: true,  toggle: "pv" },
        { data: smoothedLoadSeries, color: C.load, width: 1.5, dash: [], name: "Load", fill: false, toggle: "load" },
      ];
      // Append one actual/target pair per discovered battery driver.
      // Stable order so chart colors don't jump as the driver set grows.
      Object.keys(chartBatteries).sort().forEach(function (name) {
        var slot = chartBatteries[name];
        var color = batteryColor(name);
        var toggle = "bat:" + name;
        var label = batteryLabel(name);
        series.push({ data: smoothSeriesForChart(slot.bat, chartHistory.timestamps, chartSmoothMs), color: color, width: 2, dash: [], name: label, fill: false, toggle: toggle });
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
        series.push({ data: smoothSeriesForChart(slot.ev, chartHistory.timestamps, chartSmoothMs), color: color, width: 2, dash: [], name: label, fill: false, toggle: toggle });
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
    ctx.strokeStyle = C.grid;
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
      var lastActualPV = lastIdx >= 0 && smoothedPVSeries ? smoothedPVSeries[lastIdx] : null;
      var lastActualLoad = lastIdx >= 0 && smoothedLoadSeries ? smoothedLoadSeries[lastIdx] : null;

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
    ctx.strokeStyle = C.grid;
    ctx.lineWidth = chartNowStrokeW;
    ctx.beginPath();
    ctx.moveTo(nowX, pad.top);
    ctx.lineTo(nowX, pad.top + plotH);
    ctx.stroke();

    // Y-axis labels (outside clip so they're fully visible)
    ctx.fillStyle = C.dim;
    ctx.font = chartFontAxis;
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
      if (ageMs < 5000) ageStr = "live";
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
      pointCount: chartHistory.timestamps.length,
      fontTooltip: chartFontTooltip,
      fontTooltipS: chartFontTooltipS
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
      surface: cssColor("--ink-raised", "#161616"),
      accent:  cssColor("--accent-e", "#fbbf24"),
      load:    cssColor("--white-s", "#e8e8e8"),
    };
    return _chartColors;
  }

  function drawHoverOverlay(ctx) {
    if (!chartLayout) return;
    var C = chartColors();
    var l = chartLayout;
    var i = hoverIndex;
    var fontTooltip = l.fontTooltip || "10px monospace";
    var fontTooltipS = l.fontTooltipS || "9px monospace";
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

    ctx.fillStyle = C.surface;
    ctx.strokeStyle = C.grid;
    ctx.lineWidth = 1;
    ctx.fillRect(boxX, boxY, boxW, boxH);
    ctx.strokeRect(boxX, boxY, boxW, boxH);

    ctx.font = fontTooltip;
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
          ctx.font = fontTooltipS;
          ctx.fillText("→ " + formatW(lab.target[i]), boxX + boxW - 10 - actualW, y);
          ctx.font = fontTooltip;
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
    var fontTooltip = l.fontTooltip || "10px monospace";
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
      { name: "SoC",       val: a.soc != null ? Math.round(a.soc * 100) + "%" : "—", color: "#60a5fa", literal: true },
      { name: "Price",     val: fmtPricePerKwh(a.price_ore), color: "#fbbf24", literal: true },
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

    ctx.font = fontTooltip;
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
    // CONTROL write — strict (FIX-B): driver enable/disable/restart/diagnose.
    return apiFetch("/api/drivers/" + encodeURIComponent(name) + "/" + action, { method: "POST" })
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
    return apiFetch("/api/v2x/command", {
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

  function formatOptionalW(w) {
    return w == null ? "—" : formatW(w);
  }

  // ---------------------------------------------------------------------
  // V2X manual-command surface. Dashboard driver cards are rendered here
  // rather than in a framework so the static UI keeps working without a
  // build step.
  // ---------------------------------------------------------------------
  function renderV2XControls(name, d) {
    var isLive = d.status === "ok";
    var chargeMax = d.v2x_charge_power_max_w || d.v2x_rated_power_w || 50000;
    var dischargeMax = d.v2x_discharge_power_max_w || d.v2x_rated_power_w || 50000;
    var maxW = Math.max(1, Math.min(50000, Math.max(chargeMax, dischargeMax)));
    var suggested = Math.min(3000, maxW);
    var disabled = isLive ? "" : " disabled";
    // Mono eyebrow + caption flags that this manual surface bypasses the
    // dispatch policy envelope.
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
      // Diagnose is a UI-only action: no API mutation, just open the
      // modal and let it poll /api/drivers/{name} on its own cadence.
      if (action === "diagnose") {
        if (window.FTWDiagnostics) window.FTWDiagnostics.open(name);
        return;
      }
      if (action === "restart" && !window.confirm("Restart driver \"" + name + "\"? It will briefly stop reporting while it reconnects.")) return;
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
      var isV2X = !isVehicle && (d.v2x_w != null || d.v2x_connected != null || d.v2x_vehicle_soc != null);

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
      } else if (isV2X) {
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
        if (disp && d.bat_w != null && !d.observe_only) {
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
          (d.observe_only
            ? '  <span class="stat-label">Control</span><span class="stat-value stat-dim">observe only</span>'
            : "") +
          '  <span class="stat-label">SoC</span><span class="stat-value">' + formatSoc(batSocVal) + "</span>" +
          '  <span class="stat-label">Ticks</span><span class="stat-value">' + ticks + "</span>" +
          '  <span class="stat-label">Errors</span><span class="stat-value">' + errors + "</span>" +
          "</div>" +
          '<div class="driver-soc-bar"><div class="driver-soc-fill" style="width:' + Math.round(batSocVal * 100) + '%"></div></div>';
      } else {
        // Metrics-only driver (e.g. MyUplink heat-pump telemetry): emits
        // scalar metrics via emit_metric, no meter/pv/battery DER reading.
        // Don't render phantom 0 W / 0 % PV+battery+SoC rows — show liveness
        // and point at the per-driver metrics view (Diagnose) instead.
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
  // Loadpoint cache — keyed by driver_name so the EV-planet builder
  // in render() can look up vehicle SoC + charge-limit without a
  // second round of fetches per status tick. Refreshed in parallel
  // with /api/status. `null` until the first fetch lands; missing
  // entries mean "no loadpoint for this driver" and the planet
  // falls back to legacy kW-only rendering.
  var loadpointsByDriver = null;
  var chargingNoticePoints = [];
  var chargingNoticeRows = new Map();
  var chargingNoticeTimer = null;
  function updateChargingNotice(payload) {
    var fresh = !!(payload && Array.isArray(payload.loadpoints));
    if (fresh) {
      chargingNoticePoints = payload.loadpoints.map(function (lp) {
        var previous = chargingNoticePoints.find(function (old) { return old.id === lp.id; });
        // Offline telemetry cannot establish that the cable was removed.
        return previous && previous.plugged_in && lp.charger && !lp.charger.available
          ? Object.assign({}, lp, { plugged_in: true }) : lp;
      });
      if (chargingNoticeTimer) clearTimeout(chargingNoticeTimer);
      chargingNoticeTimer = setTimeout(function () { updateChargingNotice(null); }, 15000);
    }
    fresh = fresh && !document.hidden;
    var host = document.getElementById("charging-notices");
    var anchor = document.getElementById("power-now");
    if (!anchor) return;
    if (!host) {
      host = document.createElement("div");
      host.id = "charging-notices";
      anchor.parentNode.insertBefore(host, anchor);
    }
    var keep = new Set();
    chargingNoticePoints.filter(function (lp) { return lp.plugged_in; }).forEach(function (lp) {
      keep.add(lp.id);
      var row = chargingNoticeRows.get(lp.id);
      if (!row) {
        var section = document.createElement("section");
        section.className = "overview-card";
        section.setAttribute("aria-label", "Car connection");
        var title = document.createElement("strong");
        var status = document.createElement("p");
        status.setAttribute("role", "status");
        var button = document.createElement("button");
        button.type = "button";
        section.append(title, status, button);
        host.appendChild(section);
        row = { section: section, title: title, status: status, button: button };
        chargingNoticeRows.set(lp.id, row);
      }
      var available = fresh && (!lp.charger || lp.charger.available);
      row.title.textContent = available ? "Car connected" : "Car status is out of date";
      var message = available ? renderEvPlanStatus(lp, null) : null;
      row.status.textContent = message ? message.textContent : "Waiting for current charger status. The last reading cannot confirm charging.";
      row.button.textContent = "Check charging" + (lp.soc_source !== "vehicle" ? " and battery level" : "");
      row.button.onclick = function () {
        if (energyFlowEl) energyFlowEl.dispatchEvent(new CustomEvent("ftw-planet-click", { detail: { role: "ev", name: lp.driver_name } }));
      };
    });
    chargingNoticeRows.forEach(function (row, id) {
      if (!keep.has(id)) { row.section.remove(); chargingNoticeRows.delete(id); }
    });
    host.hidden = keep.size === 0;
  }
  document.addEventListener("visibilitychange", function () {
    if (document.hidden) updateChargingNotice(null);
  });
  // Last successful /api/status payload — surfaced so secondary
  // consumers (e.g. the EV modal's 5 s refresh) can read derived
  // facts like siteHasPV() without re-fetching. `null` until the
  // first fetch lands; consumers MUST handle null.
  var lastStatusPayload = null;
  var statusPollTimer = null;
  var statusPollInFlight = false;
  var liveHistoryPollTimer = null;
  function fetchStatus() {
    return Promise.all([
      boundedApiRead("/api/status", function (r) {
        if (!r.ok) return r.json().catch(function () { return {}; }).then(function (body) {
          var error = new Error("HTTP " + r.status);
          error.starting = body.error === "starting";
          throw error;
        });
        return r.json();
      }),
      boundedApiRead("/api/loadpoints", function (r) { return r.ok ? r.json() : null; })
        .catch(function () { return null; }),
      boundedApiRead("/api/health", function (r) { return r.ok ? r.json() : null; })
        .catch(function () { return null; })
        .then(function (health) {
          updateHistoryMigration(health);
          return health;
        }),
    ])
      .then(function (results) {
        var data = results[0];
        var lp = results[1];
        var health = results[2];
        lastStatusPayload = data;
        // Surface DB corruption-recovery events (boot-time, immutable per
        // process) as a top banner. Tolerant: health may be null.
        try { updateStorageBanner(health && health.storage); }
        catch (eSb) { /* silent */ }
        if (lp && Array.isArray(lp.loadpoints)) {
          var idx = {};
          lp.loadpoints.forEach(function (l) {
            if (l && l.driver_name) idx[l.driver_name] = l;
          });
          loadpointsByDriver = idx;
        }
        updateChargingNotice(lp);
        setConnected(true);
        if (firstLoad) { firstLoad = false; }
        if (setupBannerShown) { hideSetupBanner(); }
        // Always refresh timestamp on successful fetch
        lastUpdate.textContent = "Last update: " + new Date().toLocaleTimeString();
        // Isolate render errors from connection state / timestamp
        try { render(data); }
        catch (e) { console.error("render error:", e); }
        try { window.dispatchEvent(new CustomEvent("ftw-status", { detail: data })); }
        catch (e3) { /* assistant chip is optional */ }
        // Show a subtle prompt when no drivers are configured
        try { updateNoDevicesPrompt(data.drivers); }
        catch (e2) { /* silent */ }
      })
      .catch(function (e) {
        console.warn("status fetch failed:", e);
        updateChargingNotice(null);
        setConnected(false);
        if (firstLoad && !e.starting) { showSetupBanner(); }
      });
  }

  // Only the visibility-owned poll is coalesced. Command handlers still call
  // fetchStatus directly so an operator action can refresh without waiting for
  // the next timer. Settling a hidden poll only releases the lock; returning to
  // the tab or a later timer tick decides when the next poll starts.
  function pollStatus() {
    if (document.hidden || statusPollInFlight) return;
    statusPollInFlight = true;
    fetchStatus().finally(function () {
      statusPollInFlight = false;
    });
  }

  function syncStatusPolling() {
    if (document.hidden) {
      if (statusPollTimer !== null) {
        clearInterval(statusPollTimer);
        statusPollTimer = null;
      }
      if (liveHistoryPollTimer !== null) {
        clearInterval(liveHistoryPollTimer);
        liveHistoryPollTimer = null;
      }
      return;
    }

    // Refresh both live surfaces at once after returning to the tab, then keep
    // exactly one foreground timer for each. These reads are useless while
    // hidden; history's minute poll used to remain after status polling paused.
    pollStatus();
    loadHistory(chartRange, true);
    fetchLiveHistory(true);
    if (statusPollTimer === null) {
      statusPollTimer = setInterval(pollStatus, POLL_INTERVAL);
    }
    if (liveHistoryPollTimer === null) {
      liveHistoryPollTimer = setInterval(function () { fetchLiveHistory(true); }, 60_000);
    }
  }

  // ---- Storage-health banner (DB corruption auto-recovered) ----
  // storage = { state, cache, last_event_ms, detail } from /api/health.
  // Heal events are set once at boot and never change, so a session-scoped
  // dismissal (keyed on the event) hides it without losing a fresh alert
  // after a later restart.
  function updateStorageBanner(storage) {
    var existing = document.getElementById("storage-banner");
    var bad = !!storage && (
      (storage.state && storage.state !== "ok") ||
      (storage.cache && storage.cache !== "ok")
    );
    if (!bad) { if (existing) existing.remove(); return; }

    var key = String(storage.last_event_ms || storage.detail || "1");
    try {
      if (sessionStorage.getItem("ftw-storage-banner-dismissed") === key) {
        if (existing) existing.remove();
        return;
      }
    } catch (e) { /* sessionStorage unavailable — show anyway */ }

    var detail = storage.detail || "Database recovered from a problem.";
    if (existing) {
      var t = existing.querySelector(".storage-banner-text");
      if (t) t.textContent = detail;
      existing.setAttribute("data-key", key);
      return;
    }

    var banner = document.createElement("div");
    banner.id = "storage-banner";
    banner.className = "storage-banner";
    banner.setAttribute("data-key", key);

    var tag = document.createElement("span");
    tag.className = "storage-banner-tag";
    tag.textContent = "Storage";

    var text = document.createElement("span");
    text.className = "storage-banner-text";
    text.textContent = detail;

    var dismiss = document.createElement("button");
    dismiss.className = "storage-banner-dismiss";
    dismiss.type = "button";
    dismiss.setAttribute("aria-label", "Dismiss");
    dismiss.innerHTML = "&times;";
    dismiss.addEventListener("click", function () {
      try { sessionStorage.setItem("ftw-storage-banner-dismissed", banner.getAttribute("data-key") || "1"); }
      catch (e) { /* ignore */ }
      banner.remove();
    });

    banner.appendChild(tag);
    banner.appendChild(text);
    banner.appendChild(dismiss);
    var main = document.querySelector("main");
    if (main) main.parentNode.insertBefore(banner, main);
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
    // The wizard's Save replaces config (it doesn't merge yet), so the copy
    // says "Run setup wizard" rather than "Add a device". ?step=3 is honored
    // by setup.js init (deep-link → scan step).
    prompt.innerHTML = 'No devices connected. <a href="/setup?step=3">Run setup wizard &rarr;</a>';
    var cards = document.querySelector(".summary-cards");
    if (cards) cards.parentNode.insertBefore(prompt, cards.nextSibling);
  }

  function setMode(mode) {
    markModeActive(mode);
    revealManualModes(mode);
    pendingMode = mode;
    pendingModeUntil = Date.now() + 4000;
    apiFetch("/api/mode", {
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
        pendingMode = null; // the write failed — show server truth again
        setConnected(false);
      });
  }

  function markModeActive(mode) {
    document.querySelectorAll("#mode-buttons-primary button, #mode-buttons button").forEach(function (btn) {
      if (btn.dataset.mode === mode) btn.classList.add("active");
      else btn.classList.remove("active");
    });
  }

  // Open the manual drawer when the live mode lives there, so a reload
  // (or a change made from the phone app / HA) never leaves the current
  // setting with no button on screen.
  //
  // Only on a transition. The status poll repeats the same mode every couple
  // of seconds; re-opening on every repeat would undo an explicit "Hide
  // manual" a second after the user pressed it. A move to a *different*
  // manual mode still opens the drawer — that button has to be on screen.
  function revealManualModes(mode) {
    if (!mode || mode === lastRevealedMode) return;
    var panel = document.getElementById("mode-buttons");
    if (!panel) return;
    var match = panel.querySelector('button[data-mode="' + mode + '"]');
    // Before the catalog paints, a missing button means "not rendered yet",
    // not "not a manual mode" — don't record it, or the catalog's own call
    // would come back as a repeat and never open the drawer.
    if (!match && !modeCatalogRendered) return;
    lastRevealedMode = mode;
    if (!match) return;
    panel.style.display = "flex";
    var advBtn = document.getElementById("mode-advanced-btn");
    if (advBtn) advBtn.textContent = "Hide manual";
  }

  // ---- Mode buttons, built from the server's canonical catalog ----
  // The dashboard no longer hard-codes which modes exist or how they're
  // labelled. GET /api/modes returns every selectable mode with a label,
  // tooltip, and tier; we render `primary` into the strategy row and
  // `advanced` behind the "Manual…" toggle (hidden modes are valid but not
  // shown). This is the UI-side counterpart to the HA discovery fix — both
  // surfaces derive from control.AllModes, so they can't drift.
  //
  // /api/modes is static, non-sensitive metadata, so it rides a plain fetch
  // (not the frequently-polled status path). If it fails — offline
  // at first paint — modeCatalogRendered stays false and fetchStatus retries
  // it on the next successful poll, so the buttons appear as soon as the host
  // is reachable, with no hard-coded fallback list.
  var modeCatalogRendered = false;
  function renderModeCatalog() {
    if (modeCatalogRendered) return Promise.resolve(true);
    var primary = document.getElementById("mode-buttons-primary");
    var advanced = document.getElementById("mode-buttons");
    if (!primary || !advanced) return Promise.resolve(false);
    return fetch("/api/modes", { headers: { Accept: "application/json" } })
      .then(function (r) {
        if (!r.ok) throw new Error("HTTP " + r.status);
        return r.json();
      })
      .then(function (data) {
        var modes = (data && data.modes) || [];
        if (!modes.length) return false;
        var frags = { primary: document.createDocumentFragment(), advanced: document.createDocumentFragment() };
        modes.forEach(function (m) {
          if (m.tier !== "primary" && m.tier !== "advanced") return; // skip hidden
          // Household prefs on the Plan card replaced Passive/Active as
          // the primary knobs. Catalog keys stay primary for HA/app.
          if (String(m.key || "").indexOf("planner_") === 0) return;
          var btn = document.createElement("button");
          btn.dataset.mode = m.key;
          btn.textContent = m.label;
          if (m.tooltip) btn.title = m.tooltip;
          if (currentMode && m.key === currentMode) btn.classList.add("active");
          frags[m.tier].appendChild(btn);
        });
        primary.replaceChildren(frags.primary);
        advanced.replaceChildren(frags.advanced);
        primary.hidden = !primary.childElementCount;
        modeCatalogRendered = true;
        revealManualModes(currentMode);
        return true;
      })
      .catch(function () {
        // Leave modeCatalogRendered false so a later poll retries.
        return false;
      });
  }

  // Fire-and-forget wrappers around postJson. postJson itself rethrows
  // so callers that chain .then/.finally behave correctly;
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

  function setBatteryCoversEV(enabled) {
    postJson("/api/battery_covers_ev", { enabled: !!enabled }).catch(function () {});
  }

  function postJson(url, body) {
    // CONTROL write — strict (FIX-B). Covers /api/target, /api/peak_limit,
    // /api/peak_import_ceiling, /api/battery_covers_ev,
    // /api/ev/command, … (every state-changing dashboard knob routes here).
    return apiFetch(url, {
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
    // update-badge.js is deferred, so on the first tick or two the element
    // may not have upgraded yet and the method is absent. Polling re-asserts
    // this every cycle and the component defaults to connected, which is
    // what the old #conn-status markup shipped as.
    if (updateBadge && typeof updateBadge.setConnected === "function") {
      updateBadge.setConnected(ok);
    }
    if (ok) {
      if (overviewHealth) overviewHealth.classList.add("is-connected");
      if (overviewHealthLabel) overviewHealthLabel.textContent = "Live";
      // render() will update lastUpdate with timestamp
    } else {
      if (overviewHealth) overviewHealth.classList.remove("is-connected");
      if (overviewHealthLabel) overviewHealthLabel.textContent = "Connection lost";
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
  // Permission to sell from the battery is a deliberate household answer, so
  // a prefs read that fails or answers with nothing usable lands on the mode
  // that never exports.
  var PLANNER_FALLBACK_MODE = "planner_passive_arbitrage";
  // "Use the plan" — the one control that hands a manually-driven house back
  // to the planner. Which planner mode that is follows from the household's
  // own prefs, and the server already maps them (mapped_mode), so the two
  // surfaces cannot drift. setMode() from here on, so the optimistic paint,
  // the pending-mode hold and the drawer all behave as they do for a tap.
  var planUseBtn = document.getElementById("plan-use-btn");
  if (planUseBtn) {
    planUseBtn.addEventListener("click", function () {
      apiFetch("/api/planner/prefs", { headers: { Accept: "application/json" } })
        .then(function (r) {
          if (!r.ok) throw new Error("HTTP " + r.status);
          return r.json();
        })
        .then(function (prefs) {
          var mapped = prefs && prefs.mapped_mode;
          setMode(typeof mapped === "string" && mapped.indexOf("planner_") === 0
            ? mapped
            : PLANNER_FALLBACK_MODE);
        })
        .catch(function () {
          setMode(PLANNER_FALLBACK_MODE);
        });
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

  function evFmtClock(ms) {
    // 24-hour clock, matching plan-brief.js's formatClock — the rest of
    // the plan UI speaks 24 h regardless of browser locale.
    var d = new Date(ms);
    return String(d.getHours()).padStart(2, "0") + ":" + String(d.getMinutes()).padStart(2, "0");
  }

  function evFmtElapsed(ms) {
    var s = Math.max(0, Math.round(ms / 1000));
    if (s < 90) return s + " s";
    var m = Math.round(s / 60);
    if (m < 90) return m + " min";
    return Math.round(m / 60) + " h";
  }

  // manualStatusText is the one sentence the Manual tab and the plan strip
  // show while an operator hold runs. It follows the charger, not the
  // request: sent, taken by the charger, charging, not drawing, stalled or
  // limited by the fuse, each with the time elapsed. The box derives the
  // state (loadpoint.manual); this only puts words on it. Returns null
  // when no hold is active.
  function manualStatusText(lp, d) {
    if (lp && lp.manual_restore_unconfirmed) return "Confirm how to continue charging. FTW could not confirm the charger or connection.";
    var m = lp && lp.manual;
    if (!lp || !lp.manual_active || !m || !m.active) return null;
    var reqA = m.requested_a > 0 ? Math.round(m.requested_a) + " A" : formatW(m.requested_w || lp.manual_charge_w || 0);
    var cmdA = m.commanded_a > 0 ? Math.round(m.commanded_a) + " A" : formatW(m.commanded_w || 0);
    var since = m.since_ms > 0 ? evFmtElapsed(Date.now() - m.since_ms) : "";
    var sinceP = since ? " (" + since + ")" : "";
    var reason = m.charger_reason ? " Charger reports: " + m.charger_reason + "." : "";
    var end = lp.manual_release_soc > 0
      ? " Returns to the plan at the estimated " + Math.round(lp.manual_release_soc * 100) + " % target."
      : " Continues until the car stops drawing, you return to the plan, or unplug.";
    switch (m.state) {
      case "pausing":
        return "Pause requested. " + ((lp.current_power_w || 0) >= 100 ? formatW(lp.current_power_w) + " is still flowing. " : "") + "Waiting for the charger to stop.";
      case "paused":
        return "Paused by you. Charging stays off until you resume the plan, choose Charge now, or unplug.";
      case "unavailable":
        return "Charger status is out of date. FTW cannot confirm whether the car is charging.";
      case "charging":
        return ((lp.current_power_w || 0) >= 100
          ? "Charging at " + formatW(lp.current_power_w) + " — " + reqA + " requested."
          : "The charger reports charging. Waiting for a power reading.") + end;
      case "sent":
        return "FTW received " + reqA + ". Waiting for the charger to confirm the new limit." + sinceP + ((lp.current_power_w || 0) >= 100 ? " Still charging at " + formatW(lp.current_power_w) + "." : "") + end;
      case "accepted":
        return "Charger reports a " + cmdA + " limit. Waiting for the car to start drawing…" + sinceP + reason + end;
      case "not_drawing":
        return "Charger offers " + cmdA + " but the car is not drawing" + sinceP + "." +
          (reason || " It may be full, or held by its own charge limit or schedule.");
      case "stalled":
        if (evIsPaused(lp)) return "The charger has not stopped after your pause request. Check the charger’s app.";
        return "The charger has not acted on " + reqA + (since ? " after " + since : "") + "." +
          ((lp.current_power_w || 0) >= 100 ? " Still charging at " + formatW(lp.current_power_w) + "." : "") +
          (reason || " Check the car's own charge limit or schedule, then the charger's app.");
      case "limited":
        if (m.limit_reason === "charger_limit") return "The charger limits this request to " + cmdA + " (" + reqA + " requested).";
        if (m.limit_reason === "fuse_cooldown") {
          return "Paused: main-fuse protection — " + reqA + " requested; charging resumes on its own." + sinceP;
        }
        if (m.limit_reason === "site_meter_stale") {
          return "Paused for safety: site-meter data is stale — " + reqA + " requested; charging resumes when telemetry recovers.";
        }
        return "Main fuse limits this charge to " + cmdA + " right now (" + reqA + " requested)." + sinceP;
    }
    return "FTW received " + reqA + ". Waiting for charger status." + end;
  }

  // renderEvPlanStatus answers the question the status table can't:
  // "why isn't it charging right now, and when will it?" Field
  // experience: a car plugged in against a schedule sits at 0 W until
  // the cheap slots arrive, the modal looks dead, and the operator
  // presses Start — which overrides the plan for the whole session.
  // One honest sentence here is what prevents that. Returns null when
  // there is nothing worth saying (no loadpoint, or unplugged — the
  // schedule note covers that case).
  function renderEvPlanStatus(lp, d) {
    if (!lp || (!lp.plugged_in && !lp.manual_restore_unconfirmed)) return null;
    var text = null;
    var tone = "var(--text-dim)";
    var kwPlanned = lp.plan_total_wh > 0 ? " ~" + (lp.plan_total_wh / 1000).toFixed(1) + " kWh planned." : "";
    var winActive = lp.plan_next_start_ms > 0 && lp.plan_next_start_ms <= Date.now() && Date.now() < lp.plan_next_end_ms;
    var charging = (lp.current_power_w || 0) >= 100;
    var hasSchedule = lp.schedule && lp.schedule.soc > 0;
    if (lp.manual_restore_unconfirmed) {
      text = manualStatusText(lp, d);
    } else if (lp.charger && !lp.charger.available) {
      text = lp.charger.known
        ? "Charger status is out of date. FTW cannot confirm whether the car is charging."
        : "Waiting for the charger's first status report.";
      tone = "var(--text)";
    } else if (lp.manual_active) {
      // The same sentence as the charge controls, so the charger's own reason is
      // never hidden behind "manual charge is running".
      text = manualStatusText(lp, d) || "Manual charge requested. Waiting for charger status.";
      if (lp.manual && (lp.manual.state === "not_drawing" || lp.manual.state === "stalled")) {
        tone = "var(--text)";
      }
    } else if (charging) {
      text = winActive
        ? "Charging on plan until " + evFmtClock(lp.plan_next_end_ms) + "." + kwPlanned
        : "Charging.";
      if (lp.commanded_reason === "fuse_limit") {
        text += " Rate is limited by the main fuse right now.";
      }
    } else if (lp.charging_declined) {
      text = "The car stopped asking for charge. Check its charge limit or schedule. This does not confirm the battery is full.";
    } else if (lp.commanded_known && lp.commanded_w > 0) {
      text = "FTW requests " + formatW(lp.commanded_w) + ". Waiting for the car to draw power.";
      var chargerReason = lp.charger && lp.charger.reason || d && d.reason_no_current_label;
      if (chargerReason) text += " Charger reports: " + chargerReason + ".";
      tone = "var(--text)";
    } else if (lp.commanded_known && !lp.commanded_w &&
        (lp.commanded_reason === "fuse_cooldown" || lp.commanded_reason === "fuse_limit")) {
      // The specific pauses beat the generic branches — this is the
      // line that stops an operator debugging cable and charger while
      // the box is protecting the main fuse (#1009).
      text = "Paused: main-fuse protection — the house is using the headroom; charging resumes on its own." + kwPlanned;
      tone = "var(--text)";
    } else if (lp.commanded_known && !lp.commanded_w && lp.commanded_reason === "site_meter_stale") {
      text = "Paused for safety: site-meter data is stale — charging resumes when telemetry recovers.";
      tone = "var(--text)";
    } else if (lp.plan_pending) {
      text = "Updating the charging plan…";
    } else if (lp.plan_outdated) {
      text = "Charging times are unavailable. Your settings are saved.";
    } else if (lp.grid_deferred && hasSchedule) {
      // Richer than the pv_surplus_pause reason it usually co-occurs
      // with: it also says when normal planning resumes.
      text = "Waiting for tomorrow's electricity prices — until they arrive (~13:00) the car charges from PV surplus only.";
    } else if (lp.commanded_known && !lp.commanded_w && lp.commanded_reason === "pv_surplus_pause") {
      text = "Paused: waiting for PV surplus — solar is below the charger's minimum step right now." + kwPlanned;
    } else if (winActive) {
      text = "Paused by the box — charging resumes on its own." + kwPlanned;
    } else if (lp.plan_next_start_ms > Date.now()) {
      text = "Charging planned " + evFmtClock(lp.plan_next_start_ms) + "–" +
        evFmtClock(lp.plan_next_end_ms) + "." + kwPlanned +
        " The planner picks the cheapest hours before your target.";
    } else if (lp.surplus_only) {
      text = "PV surplus only — charges when solar exceeds house load.";
    } else if (!hasSchedule) {
      text = "No charging plan yet. Set a ready time, or choose Charge now.";
      tone = "var(--text)";
    } else {
      text = "No charge window yet for this goal. Choose Charge now if you need to charge immediately.";
    }
    if (lp.plan_pending && !lp.manual_active && text && text.indexOf("Updating the charging plan") < 0) text += " Updating the charging plan…";
    if (lp.manual_save_error) text = (text ? text + " " : "") + "This choice is active now, but could not be saved for restart. FTW is retrying.";
    if (!text) return null;
    var p = document.createElement("p");
    p.style.color = tone;
    p.style.margin = "0 0 0.6rem 0";
    p.style.padding = "0.35rem 0.5rem";
    p.style.borderLeft = "3px solid var(--accent, #888)";
    p.style.background = "color-mix(in srgb, var(--accent, #888) 8%, transparent)";
    p.textContent = text;
    return p;
  }

  // Keep controls mounted while polling so editing never loses focus.
  var statusTableEl = null;
  var evPlanEl = null;   // { el, update } from buildEvPlanView
  var evPlanLpId = null;
  var evControlsEl = null;
  var evControlsLpId = null;
  var evBoostEl = null;  // { el, update } from buildEvBoostView
  var evBoostLpId = null;
  var evBoostDetails = null;
  var schedNeedsRebuild = false;
  var manualNeedsRebuild = false;

  // Detect whether the site has any PV driver configured. Used to hide
  // the "surplus charge from PV" option on PV-less sites where the
  // bat-SoC unlock wouldn't have anything to grab anyway.
  function siteHasPV(status) {
    if (!status || !status.drivers) return false;
    for (var k in status.drivers) {
      if (Object.prototype.hasOwnProperty.call(status.drivers, k)) {
        var dr = status.drivers[k];
        if (dr && typeof dr.pv_w === "number") return true;
      }
    }
    return false;
  }

  var evReading = false;
  var evReadPromise = null;
  var evLastLp = null;
  function refreshEvModal() {
    if (evReading) return evReadPromise;
    evReading = true;
    var requestedDriver = evModalDriver;
    // Pass driver query if known so the backend can scope the response
    // to the clicked planet (multi-EV setups). Falls back to whatever
    // the backend returns when no driver filter is honored.
    var url = "/api/ev/status" + (evModalDriver ? "?driver=" + encodeURIComponent(evModalDriver) : "");
    // siteHasPV is a static-per-config check, so we read the most
    // recent payload cached by fetchStatus() instead of issuing a
    // duplicate /api/status fetch on every 5 s modal tick. Falls back
    // to "no PV" until the dashboard's own fetchStatus lands once.
    evReadPromise = Promise.all([
      // Local API reads.
      boundedApiRead(url, function (r) { if (!r.ok) throw new Error("Charger status unavailable"); return r.json(); }),
      boundedApiRead("/api/loadpoints", function (r) { if (!r.ok) throw new Error("Charging settings unavailable"); return r.json(); }),
    ]).then(function (results) {
      if (requestedDriver !== evModalDriver || evModal.classList.contains("hidden")) return;
      var problem = evModalBody.querySelector('.ev-read-problem');
      if (problem) problem.remove();
      if (statusTableEl) statusTableEl.hidden = false;
      var d = results[0];
      var lps = results[1];
      var status = lastStatusPayload;
      var hasLoadpoints = lps && Array.isArray(lps.loadpoints) && lps.loadpoints.length > 0;
      var carConnected = d && d.connected !== false;
      // Hard short-circuit only when there's NOTHING to show: no
      // active EV AND no configured loadpoint. Otherwise we fall
      // through so the schedule editor stays reachable — operators
      // routinely want to set tomorrow morning's target before
      // plugging in tonight, and the schedule is LP state (persisted
      // across restarts), not driver state.
      if (!carConnected && !hasLoadpoints) {
        setEvModalMessage("No EV charger connected");
        statusTableEl = null;
        evPlanEl = null;
        evPlanLpId = null;
        evBoostEl = null;
        evBoostLpId = null;
        evControlsEl = null;
        evControlsLpId = null;
        return;
      }
      // Status table: replace in place so the rest of the modal body
      // (including any mounted schedule section) is untouched. On the
      // very first call evModalBody may still contain the placeholder
      // from setEvModalMessage; wipe only when we have no anchor yet.
      // When no car is connected but loadpoints exist, render a
      // dim placeholder note in place of the live status table so
      // the modal still has a header before the schedule editor.
      var freshStatus;
      if (carConnected) {
        freshStatus = document.createElement("details");
        freshStatus.open = !!(statusTableEl && statusTableEl.open);
        var detailsLabel = document.createElement("summary");
        detailsLabel.textContent = "Charger details";
        freshStatus.appendChild(detailsLabel);
        freshStatus.appendChild(renderEvStatusTable(d));
      } else {
        freshStatus = document.createElement("p");
        freshStatus.style.color = "var(--text-dim)";
        freshStatus.style.fontStyle = "italic";
        freshStatus.style.margin = "0 0 0.6rem 0";
        freshStatus.textContent = "No car connected. Set a ready time, or plug in and choose Charge now.";
      }
      if (statusTableEl && statusTableEl.parentNode === evModalBody) {
        evModalBody.replaceChild(freshStatus, statusTableEl);
      } else {
        evModalBody.textContent = "";
        evModalBody.appendChild(freshStatus);
      }
      statusTableEl = freshStatus;

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
        // Prefer a plugged-in loadpoint when no driver filter is active —
        // that's the one the operator is most likely thinking about. Fall
        // back to the first configured loadpoint so the schedule editor
        // is still reachable when no car is currently connected (the
        // schedule is persistent loadpoint state, not driver state, and
        // operators routinely want to set tomorrow morning's target
        // before plugging in tonight).
        if (!matched && !evModalDriver) {
          for (var j = 0; j < lps.loadpoints.length; j++) {
            if (lps.loadpoints[j].plugged_in) { matched = lps.loadpoints[j]; break; }
          }
        }
        if (!matched && !evModalDriver) {
          matched = lps.loadpoints[0];
        }
      }
      if (!carConnected && matched && matched.schedule && matched.schedule.soc > 0) {
        freshStatus.textContent = "No car connected. This goal is saved and applies when you plug in." + (matched.plan_pending ? " Updating the plan…" : matched.plan_outdated ? " Charging times are unavailable." : "");
      }
      evLastLp = matched;
      if (matched && matched.charger && !matched.charger.available) {
        var oldReport = document.createElement("p");
        oldReport.textContent = matched.charger.updated_at_ms
          ? "Last charger report: " + evFmtClock(matched.charger.updated_at_ms) + " (out of date)."
          : "Waiting for the charger's first status report.";
        evModalBody.replaceChild(oldReport, statusTableEl);
        statusTableEl = oldReport;
      }
      var setupNote = evModalBody.querySelector(".ev-setup-note");
      if (!matched && lps) {
        if (!setupNote) {
          setupNote = document.createElement("p");
          setupNote.className = "ev-setup-note";
          setupNote.setAttribute("role", "status");
          evModalBody.insertBefore(setupNote, statusTableEl.nextSibling);
        }
        setupNote.textContent = "FTW can read this charger, but charging control is not set up. Open Settings → Chargers and add the charger.";
      } else if (setupNote) {
        setupNote.remove();
      }
      // Plan view: what the box will do with this car (one sentence +
      // the planned windows on a 24 h track) and the car's charge level,
      // which the plan is built from. Mounted once per loadpoint so the
      // slider survives polls; update() redraws the rest every tick.
      if (matched) {
        if (evPlanEl == null || evPlanLpId !== matched.id) {
          if (evPlanEl && evPlanEl.el.parentNode === evModalBody) {
            evModalBody.removeChild(evPlanEl.el);
          }
          evPlanEl = buildEvPlanView(matched, d);
          evPlanLpId = matched.id;
          evModalBody.insertBefore(evPlanEl.el, statusTableEl);
        } else {
          evPlanEl.update(matched, d);
          if (evPlanEl.el.parentNode !== evModalBody) {
            evModalBody.insertBefore(evPlanEl.el, statusTableEl);
          }
        }
      } else if (evPlanEl) {
        if (evPlanEl.el.parentNode === evModalBody) {
          evModalBody.removeChild(evPlanEl.el);
        }
        evPlanEl = null;
        evPlanLpId = null;
      }
      if (matched) {
        // Rebuild only for a changed charger or a removed goal.
        var lpChanged = evControlsEl == null || evControlsLpId !== matched.id;
        if (lpChanged || schedNeedsRebuild || manualNeedsRebuild) {
          if (evControlsEl && evControlsEl.parentNode === evModalBody) {
            evModalBody.removeChild(evControlsEl);
          }
          evControlsEl = buildEvControls(matched, siteHasPV(status));
          evControlsLpId = matched.id;
          schedNeedsRebuild = false;
          manualNeedsRebuild = false;
          evModalBody.appendChild(evControlsEl);
        } else if (evControlsEl.parentNode !== evModalBody) {
          // Modal was previously closed: body got wiped but our cached
          // section is still valid — re-attach.
          evModalBody.appendChild(evControlsEl);
        }
      } else {
        if (evControlsEl && evControlsEl.parentNode === evModalBody) {
          evModalBody.removeChild(evControlsEl);
        }
        evControlsEl = null;
        evControlsLpId = null;
      }
      if (matched && evControlsEl && statusTableEl) evModalBody.appendChild(statusTableEl);
      if (matched && evControlsEl && typeof evControlsEl.update === "function") {
        evControlsEl.update(matched, d);
      }
      // Boost from the home battery: one control, below the goal, for
      // whichever mode the loadpoint is in. Mounted once per loadpoint
      // (the reserve slider keeps its value); update() redraws state
      // every poll. Always kept last so a controls rebuild cannot land
      // underneath it.
      if (matched) {
        if (evBoostEl == null || evBoostLpId !== matched.id) {
          if (evBoostEl && evBoostEl.el.parentNode === evModalBody) {
            evModalBody.removeChild(evBoostEl.el);
          }
          if (evBoostDetails && evBoostDetails.parentNode === evModalBody) evBoostDetails.remove();
          evBoostEl = buildEvBoostView(matched, status);
          evBoostLpId = matched.id;
          evBoostDetails = document.createElement("details");
          var boostSummary = document.createElement("summary");
          boostSummary.textContent = "Use the home battery";
          evBoostDetails.appendChild(boostSummary);
          evBoostDetails.appendChild(evBoostEl.el);
        } else {
          evBoostEl.update(matched, status);
        }
        if (evModalBody.lastChild !== evBoostDetails) {
          evModalBody.appendChild(evBoostDetails);
        }
      } else if (evBoostEl) {
        if (evBoostDetails && evBoostDetails.parentNode === evModalBody) evBoostDetails.remove();
        evBoostEl = null;
        evBoostLpId = null;
      }
    }).catch(function () {
      if (requestedDriver !== evModalDriver || evModal.classList.contains("hidden")) return;
      var problem = evModalBody.querySelector('.ev-read-problem');
      if (!problem) {
        problem = document.createElement("p");
        problem.className = "ev-read-problem";
        problem.setAttribute("role", "alert");
        evModalBody.prepend(problem);
      }
      problem.textContent = "Waiting for current charger status. FTW cannot confirm whether the car is charging. Trying again…";
      if (statusTableEl) statusTableEl.hidden = true;
      if (evLastLp) {
        var stale = Object.assign({}, evLastLp, {
          charger: { known: true, available: false },
          manual: Object.assign({}, evLastLp.manual, { state: "unavailable" }),
        });
        if (evControlsEl && evControlsEl.update) evControlsEl.update(stale, null);
        if (evPlanEl) evPlanEl.update(stale, null);
      }
    }).finally(function () { evReading = false; });
    return evReadPromise;
  }

  function refreshEvModalAfterWrite() {
    if (evReading) return evReadPromise.then(function () { return refreshEvModal(); });
    return refreshEvModal();
  }

  // Stop reasons the controller reports for a battery boost lease, in
  // the operator's words. Unknown reasons fall back to the raw token.
  var EV_BOOST_STOP_LABELS = {
    cancelled: "stopped by you",
    expired: "time limit reached",
    vehicle_unplugged: "car unplugged",
    ev_target_reached: "car target reached",
    departure_reached: "departure time reached",
    operator_hold: "a manual charge took priority",
    surplus_only: "PV only took priority",
    site_safety_block: "site-meter safety stopped it",
    loadpoint_driver_unavailable: "charger driver unavailable",
    battery_unavailable: "home battery unavailable",
    battery_reserve_reached: "home battery reached its reserve",
    battery_hold: "a battery hold took priority",
    core_mode: "the site mode does not allow it",
    fuse_safety_block: "fuse protection stopped it",
    restart_lease_invalid: "the saved lease was no longer safe after a restart",
  };

  function evFmtRemaining(ms) {
    var leftS = Math.max(0, Math.ceil((ms - Date.now()) / 1000));
    var h = Math.floor(leftS / 3600);
    var m = Math.ceil((leftS % 3600) / 60);
    return h > 0 ? h + " h " + m + " min" : m + " min";
  }

  // buildEvBoostView — the one way to let the home battery help the car.
  // A per-loadpoint, time-boxed lease with a reserve the house keeps
  // (POST/DELETE /api/loadpoints/{id}/battery_boost); the controller ends
  // it on its own with a reason the view names. Replaces the site-wide
  // battery_covers_ev toggle that used to sit here: that setting had no
  // time limit and no reserve. A site that still has it on sees one
  // line about it here with a way to turn it off, so the state is never
  // hidden. Mounted once per loadpoint; update() runs on every poll.
  function buildEvBoostView(lp, status) {
    var box = document.createElement("div");
    box.style.marginTop = "0.9rem";
    box.style.paddingTop = "0.6rem";
    box.style.borderTop = "1px solid var(--line)";

    var eyebrow = document.createElement("div");
    eyebrow.textContent = "Boost from home battery";
    eyebrow.style.fontFamily = "var(--mono)";
    eyebrow.style.fontSize = "0.7rem";
    eyebrow.style.letterSpacing = "0.18em";
    eyebrow.style.textTransform = "uppercase";
    eyebrow.style.color = "var(--text-dim)";
    eyebrow.style.marginBottom = "0.45rem";
    box.appendChild(eyebrow);

    // Active state.
    var activeRow = document.createElement("div");
    activeRow.style.display = "flex";
    activeRow.style.alignItems = "center";
    activeRow.style.gap = "0.6rem";
    var activeText = document.createElement("div");
    activeText.style.flex = "1";
    activeText.style.fontSize = "0.9rem";
    var stopBtn = document.createElement("button");
    stopBtn.type = "button";
    stopBtn.textContent = "Stop boost";
    stopBtn.style.padding = "0.35rem 0.7rem";
    stopBtn.style.border = "1px solid var(--line)";
    stopBtn.style.borderRadius = "4px";
    stopBtn.style.cursor = "pointer";
    stopBtn.style.background = "transparent";
    stopBtn.style.color = "var(--fg)";
    activeRow.appendChild(activeText);
    activeRow.appendChild(stopBtn);
    box.appendChild(activeRow);

    // Idle state: reserve + duration + start.
    var form = document.createElement("div");
    var copy = document.createElement("small");
    copy.style.display = "block";
    copy.style.color = "var(--text-dim)";
    copy.style.marginBottom = "0.5rem";
    copy.textContent = "Let the home battery feed the car for a while. Fuse, reserve and battery-health limits still apply.";
    form.appendChild(copy);
    var hdr = sliderHeader("Keep home battery above", "30%");
    form.appendChild(hdr.row);
    var reserve = fullWidthSlider(30, hdr.value);
    reserve.min = "5";
    reserve.setAttribute("aria-label", "Home battery reserve, percent");
    form.appendChild(reserve);
    var durRow = document.createElement("div");
    durRow.style.display = "flex";
    durRow.style.alignItems = "center";
    durRow.style.gap = "0.6rem";
    durRow.style.margin = "0.55rem 0";
    var durLbl = document.createElement("span");
    durLbl.textContent = "For";
    durLbl.style.fontFamily = "var(--mono)";
    durLbl.style.fontSize = "0.68rem";
    durLbl.style.letterSpacing = "0.14em";
    durLbl.style.textTransform = "uppercase";
    durLbl.style.color = "var(--text-dim)";
    var dur = document.createElement("select");
    [["1800", "30 min"], ["3600", "1 h"], ["7200", "2 h"], ["14400", "4 h"]].forEach(function (o) {
      var opt = document.createElement("option");
      opt.value = o[0];
      opt.textContent = o[1];
      if (o[0] === "3600") opt.selected = true;
      dur.appendChild(opt);
    });
    dur.style.flex = "1";
    var startBtn = document.createElement("button");
    startBtn.type = "button";
    startBtn.style.padding = "0.4rem 0.8rem";
    startBtn.style.border = "none";
    startBtn.style.borderRadius = "4px";
    startBtn.style.cursor = "pointer";
    startBtn.style.fontWeight = "600";
    startBtn.style.background = "var(--accent-e)";
    startBtn.style.color = "#0a0a0a";
    function renderStartLabel() { startBtn.textContent = "Boost for " + dur.options[dur.selectedIndex].textContent; }
    renderStartLabel();
    dur.addEventListener("change", renderStartLabel);
    durRow.appendChild(durLbl);
    durRow.appendChild(dur);
    durRow.appendChild(startBtn);
    form.appendChild(durRow);
    var blocked = document.createElement("small");
    blocked.style.display = "block";
    blocked.style.color = "var(--text-dim)";
    form.appendChild(blocked);
    box.appendChild(form);

    var msg = document.createElement("small");
    msg.style.display = "block";
    msg.style.color = "var(--text-dim)";
    msg.style.minHeight = "1em";
    msg.style.marginTop = "0.3rem";
    msg.setAttribute("aria-live", "polite");
    box.appendChild(msg);

    // Older site-wide setting, shown only while it is on.
    var legacy = document.createElement("div");
    legacy.style.display = "flex";
    legacy.style.alignItems = "center";
    legacy.style.gap = "0.6rem";
    legacy.style.marginTop = "0.6rem";
    legacy.style.padding = "0.4rem 0.5rem";
    legacy.style.borderLeft = "3px solid var(--accent, #888)";
    legacy.style.background = "color-mix(in srgb, var(--accent, #888) 8%, transparent)";
    var legacyText = document.createElement("small");
    legacyText.style.flex = "1";
    legacyText.textContent = "Site-wide battery cover is on (older setting): the home battery feeds the car with no time limit and no reserve.";
    var legacyOff = document.createElement("button");
    legacyOff.type = "button";
    legacyOff.textContent = "Turn off";
    legacyOff.style.padding = "0.3rem 0.6rem";
    legacyOff.style.border = "1px solid var(--line)";
    legacyOff.style.borderRadius = "4px";
    legacyOff.style.cursor = "pointer";
    legacyOff.style.background = "transparent";
    legacyOff.style.color = "var(--fg)";
    legacy.appendChild(legacyText);
    legacy.appendChild(legacyOff);
    box.appendChild(legacy);

    startBtn.addEventListener("click", function () {
      var r = parseInt(reserve.value, 10);
      if (!isFinite(r) || r < 5 || r > 100) { msg.textContent = "Reserve must be 5–100 %."; return; }
      startBtn.disabled = true;
      msg.textContent = "Starting boost…";
      // CONTROL write — strict (FIX-B): time-boxed battery boost lease.
      evWrite("/api/loadpoints/" + encodeURIComponent(lp.id) + "/battery_boost", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          duration_s: parseInt(dur.value, 10),
          min_battery_soc: r / 100,
        }),
      }).then(function (res) { return res.json().then(function (j) { return { ok: res.ok, body: j }; }); })
        .then(function (res) {
          startBtn.disabled = false;
          if (res.ok) { msg.textContent = ""; refreshEvModalAfterWrite(); }
          else { msg.textContent = (res.body && res.body.error) || "Boost could not start."; }
        }).catch(function (e) { startBtn.disabled = false; msg.textContent = "Boost could not start: " + e.message; });
    });

    stopBtn.addEventListener("click", function () {
      stopBtn.disabled = true;
      msg.textContent = "Stopping boost…";
      evWrite("/api/loadpoints/" + encodeURIComponent(lp.id) + "/battery_boost", { method: "DELETE" })
        .then(function (res) {
          stopBtn.disabled = false;
          if (res.ok) { msg.textContent = ""; refreshEvModalAfterWrite(); }
          else { msg.textContent = "Boost could not be stopped."; }
        }).catch(function (e) { stopBtn.disabled = false; msg.textContent = "Boost could not be stopped: " + e.message; });
    });

    legacyOff.addEventListener("click", function () {
      legacyOff.disabled = true;
      setBatteryCoversEV(false);
      msg.textContent = "Site-wide battery cover turned off.";
    });

    function update(lpNow, statusNow) {
      var boost = (lpNow && lpNow.battery_boost) || { state: "inactive", active: false };
      var active = !!boost.active;
      activeRow.style.display = active ? "flex" : "none";
      form.hidden = active;
      if (active) {
        activeText.textContent = "Active · " + evFmtRemaining(boost.expires_at_ms) + " left · home battery kept above " +
          Math.round((boost.min_battery_soc || 0) * 100) + " %.";
      } else {
        var why = "";
        if (!lpNow.plugged_in) why = "Plug in the car first.";
        else if (lpNow.manual_active) why = "Stop the manual charge first — it already takes what it needs.";
        else if (lpNow.surplus_only) why = "Turn off PV only first.";
        startBtn.disabled = !!why;
        startBtn.style.opacity = why ? "0.5" : "1";
        var last = boost.stop_reason
          ? "Last boost ended: " + (EV_BOOST_STOP_LABELS[boost.stop_reason] || boost.stop_reason) + "."
          : "";
        blocked.textContent = why || last;
      }
      var coverOn = !!(statusNow && statusNow.battery_covers_ev);
      legacy.style.display = coverOn ? "flex" : "none";
      if (!coverOn) legacyOff.disabled = false;
    }

    update(lp, status);
    return { el: box, update: update };
  }

  // buildManualChargeSection renders the Tesla-style manual override: an
  // amp slider (range = the charger's min/max charge current) plus Start /
  // Stop. Start pins a persistent manual hold at the slider's amps,
  // which overrides surplus_only and the plan (the fuse clamp still
  // applies); Stop clears the hold and drops back to whatever mode the
  // loadpoint is in (PV-surplus-only if that toggle is on). The amperage
  // is sent as watts (power_w = A × phases × voltage); the driver
  // converts back to amps given the wallbox it's talking to.
  function evIsPaused(lp) {
    return !!(lp && !lp.manual_restore_unconfirmed && lp.manual_active && (lp.manual_charge_w === 0 ||
      lp.manual && (lp.manual.requested_w === 0 || lp.manual.state === "paused" || lp.manual.state === "pausing")));
  }

  function buildManualChargeSection(lp) {
    var phases = (lp && lp.phases) || 3;
    var voltage = (lp && lp.voltage_v) || 230;
    var perA = phases * voltage; // watts per amp
    function wToA(w) { return perA > 0 ? w / perA : 0; }
    function aToW(a) { return Math.round(a * perA); }

    var minA = Math.max(1, Math.round(wToA((lp && lp.min_charge_w) || 0)) || 6);
    var maxA = Math.round(wToA((lp && lp.max_charge_w) || 0)) || 16;
    if (maxA <= minA) { maxA = minA + 1; }

    var active = !!(lp && lp.manual_active);
    var curA = active ? Math.round(wToA((lp && lp.manual_charge_w) || 0)) : maxA;
    if (curA < minA) { curA = minA; }
    if (curA > maxA) { curA = maxA; }

    // Manual charge has no SoC gate. The estimate is a guess when the
    // charger cannot read the car, and gating Start on it left the
    // operator with a button that released itself on the next tick
    // whenever the guess already sat at the target. Start runs until the
    // car stops asking for current, Stop, or unplug; the plan takes over
    // again after that.

    var box = document.createElement("div");
    box.style.marginTop = "0.75rem";
    box.style.paddingTop = "0.6rem";
    box.style.borderTop = "1px solid var(--line)";

    var eyebrow = document.createElement("div");
    eyebrow.textContent = "Charge now is active";
    eyebrow.hidden = !active;
    eyebrow.style.fontFamily = "var(--mono)";
    eyebrow.style.fontSize = "0.7rem";
    eyebrow.style.letterSpacing = "0.18em";
    eyebrow.style.textTransform = "uppercase";
    eyebrow.style.color = "var(--text-dim)";
    eyebrow.style.marginBottom = "0.45rem";
    box.appendChild(eyebrow);

    // Slider + live readout.
    var row = document.createElement("div");
    row.style.display = "flex";
    row.style.alignItems = "center";
    row.style.gap = "0.6rem";

    var slider = document.createElement("input");
    slider.type = "range";
    slider.min = String(minA);
    slider.max = String(maxA);
    slider.step = "1";
    slider.value = String(curA);
    slider.setAttribute("aria-label", "Charging current");
    slider.style.flex = "1";
    slider.style.accentColor = "var(--accent-e)";

    var readout = document.createElement("div");
    readout.style.fontFamily = "var(--mono)";
    readout.style.fontSize = "0.9rem";
    readout.style.minWidth = "6.5em";
    readout.style.textAlign = "right";
    readout.style.color = "var(--fg)";
    function renderReadout() {
      var a = parseInt(slider.value, 10) || minA;
      readout.textContent = a + " A · " + (aToW(a) / 1000).toFixed(1) + " kW";
    }
    renderReadout();
    var currentEditingUntil = 0;
    slider.addEventListener("input", function () { currentEditingUntil = Date.now() + 1500; renderReadout(); });

    row.appendChild(slider);
    row.appendChild(readout);
    row.hidden = !active;
    row.style.display = active ? "flex" : "none";
    box.appendChild(row);

    // Status line.
    var status = document.createElement("small");
    status.style.display = "block";
    status.style.color = "var(--text-dim)";
    status.style.marginTop = "0.35rem";
    status.style.minHeight = "1em";
    status.setAttribute("role", "status");
    status.setAttribute("aria-live", "polite");
    var idleText = "Starts at up to " + maxA + " A · " + (aToW(maxA) / 1000).toFixed(1) + " kW. Ignores the goal and solar rule until you return to the plan or unplug.";
    status.textContent = active ? "Changes apply when you release the slider." : idleText;
    box.appendChild(status);

    // Start / Stop buttons.
    var btnRow = document.createElement("div");
    btnRow.style.display = "flex";
    btnRow.style.gap = "0.5rem";
    btnRow.style.marginTop = "0.5rem";

    var startBtn = document.createElement("button");
    startBtn.type = "button";
    startBtn.textContent = "Charge now";
    startBtn.hidden = active;
    startBtn.style.flex = "1";
    startBtn.style.padding = "0.4rem 0.6rem";
    startBtn.style.border = "none";
    startBtn.style.borderRadius = "4px";
    startBtn.style.cursor = "pointer";
    startBtn.style.fontWeight = "600";
    startBtn.style.background = "var(--accent-e)";
    startBtn.style.color = "#0a0a0a";

    var stopBtn = document.createElement("button");
    stopBtn.type = "button";
    stopBtn.textContent = "Return to plan";
    stopBtn.style.flex = "1";
    stopBtn.style.padding = "0.4rem 0.6rem";
    stopBtn.style.border = "1px solid var(--line)";
    stopBtn.style.borderRadius = "4px";
    stopBtn.style.cursor = "pointer";
    stopBtn.style.background = "transparent";
    stopBtn.style.color = "var(--fg)";
    stopBtn.hidden = !active;
    stopBtn.disabled = !active;
    stopBtn.style.opacity = active ? "1" : "0.5";

    var pauseBtn = stopBtn.cloneNode(false);
    pauseBtn.textContent = "Pause charging";
    pauseBtn.hidden = false;
    pauseBtn.disabled = false;
    pauseBtn.style.opacity = "1";
    btnRow.appendChild(startBtn);
    btnRow.appendChild(pauseBtn);
    btnRow.appendChild(stopBtn);
    box.appendChild(btnRow);

    // Live state. The section is mounted once per loadpoint; update() runs
    // on every poll and rewrites the status line from lp.manual, so what the
    // operator reads follows the charger instead of the click. A message
    // written by a click holds the line until the box shows the new state.
    var lastLp = lp;
    var lastD = null;
    var busy = false;
    var holdLineUntil = 0;
    function renderStatus() {
      var on = !!(lastLp && lastLp.manual_active);
      var paused = evIsPaused(lastLp);
      var restore = !!lastLp.manual_restore_unconfirmed;
      if (!busy && paused && lastLp.manual && lastLp.manual.state === "paused") holdLineUntil = 0;
      if (!busy) {
        stopBtn.hidden = !on && !restore;
        stopBtn.disabled = !on && !restore;
        eyebrow.hidden = !on || paused || restore;
        row.hidden = !on || paused || restore;
        row.style.display = on && !paused && !restore ? "flex" : "none";
        pauseBtn.hidden = paused;
        pauseBtn.disabled = false;
        stopBtn.textContent = paused || restore ? "Resume plan" : "Return to plan";
        stopBtn.style.opacity = on || restore ? "1" : "0.5";
        startBtn.hidden = on && !paused && !restore;
        if (!on || paused || restore) { slider.value = String(maxA); renderReadout(); }
        startBtn.disabled = false;
      }
      if (busy || Date.now() < holdLineUntil) return;
      status.textContent = restore ? "Choose Charge now to request charging, Resume plan to use your goal, or Pause charging to request a stop." : paused ? "The goal and solar rule wait until you resume the plan. Charge now starts immediately." : on ? "Changes apply when you release the slider." : idleText;
    }
    function update(nextLp, d) {
      if (nextLp) lastLp = nextLp;
      if (d) lastD = d;
      if (!busy && Date.now() >= currentEditingUntil && lastLp.manual_active) {
        slider.value = String(Math.min(maxA, Math.max(minA, Math.round(wToA(lastLp.manual_charge_w || 0)))));
        renderReadout();
      }
      renderStatus();
    }

    // failOn turns an HTTP error into a rejection carrying the server's
    // reason, so a refused Start (403, 404, 409) never reads as success.
    function failOn(r) {
      if (r.ok) return r;
      return r.json().catch(function () { return {}; }).then(function (j) {
        throw new Error((j && j.error) || ("HTTP " + r.status));
      });
    }

    function requestCharge(pause) {
      pause = pause === true;
      if (busy) return;
      busy = true;
      slider.disabled = true;
      stopBtn.disabled = true;
      startBtn.disabled = true;
      var a = parseInt(slider.value, 10) || minA;
      pauseBtn.disabled = true;
      status.textContent = pause ? "Sending pause request…" : "Asking FTW for " + a + " A…";
      // CONTROL write — strict (FIX-B): persistent manual hold (hold_s:0)
      // with no SoC release — see the note above the slider.
      evWrite("/api/loadpoints/" + encodeURIComponent(lp.id) + "/manual_hold", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          power_w: pause ? 0 : aToW(a),
          hold_s: 0,
          phase_mode: phases === 1 ? "1p" : "3p",
        }),
      }).then(failOn).then(function () {
        busy = false;
        slider.disabled = false;
        status.textContent = pause ? "Pause requested. Waiting for the charger to stop…" : "FTW received " + a + " A. Waiting for the charger…";
        holdLineUntil = Date.now() + 6000;
        refreshEvModalAfterWrite(); // read the hold and charger response without rebuilding focused inputs
      }).catch(function (e) {
        busy = false;
        slider.disabled = false;
        startBtn.disabled = false;
        stopBtn.disabled = !lastLp.manual_active;
        pauseBtn.disabled = false;
        status.textContent = "Request not confirmed: " + ((e && e.message) || "try again") + ".";
        holdLineUntil = Infinity;
      });
    }
    startBtn.addEventListener("click", function () { requestCharge(false); });
    pauseBtn.addEventListener("click", function () { requestCharge(true); });
    slider.addEventListener("change", function () {
      if (lastLp.manual_active) requestCharge();
    });

    stopBtn.addEventListener("click", function () {
      busy = true;
      stopBtn.disabled = true;
      slider.disabled = true;
      startBtn.disabled = true;
      pauseBtn.disabled = true;
      status.textContent = "Returning to the plan…";
      evWrite("/api/loadpoints/" + encodeURIComponent(lp.id) + "/manual_hold", {
        method: "DELETE",
      }).then(failOn).then(function () {
        busy = false;
        slider.disabled = false;
        startBtn.disabled = false;
        pauseBtn.disabled = false;
        status.textContent = "The plan decides when to charge.";
        holdLineUntil = Date.now() + 6000;
        refreshEvModalAfterWrite();
      }).catch(function (e) {
        busy = false;
        slider.disabled = false;
        startBtn.disabled = false;
        stopBtn.disabled = false;
        pauseBtn.disabled = false;
        status.textContent = "Return to plan not confirmed: " + ((e && e.message) || "try again") + ".";
        holdLineUntil = Infinity;
      });
    });

    return { el: box, update: update };
  }

  // sliderHeader builds a "LABEL ............ value" row (mono uppercase
  // label on the left, mono accent value on the right) that sits above a
  // full-width slider, so the Current-charge and Target sliders read as a
  // consistent pair. Returns the row plus the value span — the caller
  // wires the slider's input event to update value.textContent.
  function sliderHeader(labelText, valueText) {
    var row = document.createElement("div");
    row.style.display = "flex";
    row.style.justifyContent = "space-between";
    row.style.alignItems = "baseline";
    row.style.marginBottom = "0.3rem";
    var lbl = document.createElement("span");
    lbl.textContent = labelText;
    lbl.style.fontFamily = "var(--mono)";
    lbl.style.fontSize = "0.68rem";
    lbl.style.letterSpacing = "0.14em";
    lbl.style.textTransform = "uppercase";
    lbl.style.color = "var(--text-dim)";
    var val = document.createElement("span");
    val.textContent = valueText;
    val.style.fontFamily = "var(--mono)";
    val.style.fontSize = "0.95rem";
    val.style.color = "var(--accent-e)";
    row.appendChild(lbl);
    row.appendChild(val);
    return { row: row, value: val };
  }

  // fullWidthSlider returns a 0–100 whole-percent range input spanning the
  // modal width, wired to update a value span on drag.
  function fullWidthSlider(initVal, valueSpan) {
    var s = document.createElement("input");
    s.type = "range";
    s.min = "0"; s.max = "100"; s.step = "1";
    s.value = String(initVal);
    s.style.width = "100%";
    s.style.margin = "0";
    s.style.accentColor = "var(--accent-e)";
    s.style.cursor = "pointer";
    s.addEventListener("input", function () { valueSpan.textContent = s.value + "%"; });
    return s;
  }

  // buildEvPlanView — the plug-in moment. Above the controls, the operator
  // sees what the box will do with this car: the plan-status sentence,
  // the planned charge windows drawn on a 24 h track, and right under
  // it the car's CURRENT charge, which is the input the plan is built
  // from. Chargers like Easee cannot read the car, so that value is an
  // estimate; correcting it should be the easiest thing in the modal.
  // The slider writes on release (POST /api/loadpoints/{id}/soc, which
  // replans before it answers) and the view refetches so the plan on
  // screen moves with the value. No button. Mounted once per loadpoint;
  // update() runs on every poll and leaves the slider alone while the
  // operator is dragging it.
  var EV_PLAN_HORIZON_MS = 24 * 3600 * 1000;

  function buildEvPlanView(lp, d) {
    var box = document.createElement("div");
    box.style.margin = "0 0 0.6rem 0";

    var headline = null; // <p> from renderEvPlanStatus, or null

    // 24 h track: windows from lp.plan_windows placed by wall clock.
    var track = document.createElement("div");
    track.className = "ev-plan-track";
    track.style.position = "relative";
    track.style.height = "12px";
    track.style.borderRadius = "3px";
    track.style.background = "color-mix(in srgb, var(--line) 60%, transparent)";
    track.style.margin = "0.5rem 0 0.25rem";
    track.style.overflow = "hidden";

    var ticks = document.createElement("div");
    ticks.style.display = "flex";
    ticks.style.justifyContent = "space-between";
    ticks.style.fontFamily = "var(--mono)";
    ticks.style.fontSize = "0.65rem";
    ticks.style.color = "var(--text-dim)";

    var caption = document.createElement("small");
    caption.style.display = "block";
    caption.style.color = "var(--text-dim)";
    caption.style.marginTop = "0.3rem";

    var planWrap = document.createElement("div");
    planWrap.appendChild(track);
    planWrap.appendChild(ticks);
    planWrap.appendChild(caption);

    // Car's current charge.
    var socWrap = document.createElement("div");
    socWrap.style.marginTop = "0.75rem";
    var hdr = sliderHeader("Battery now", "—");
    socWrap.appendChild(hdr.row);
    var slider = fullWidthSlider(50, hdr.value);
    slider.setAttribute("aria-label", "Car's current charge, percent");
    socWrap.appendChild(slider);
    var note = document.createElement("small");
    note.style.display = "block";
    note.style.color = "var(--text-dim)";
    note.style.marginTop = "0.3rem";
    note.style.minHeight = "1em";
    socWrap.appendChild(note);

    box.appendChild(planWrap);
    box.appendChild(socWrap);
    var capacityView = buildEvCapacityView(lp);
    box.appendChild(capacityView.el);

    // While the operator holds the slider (pointer down, or keyboard
    // input in the last moment), polls must not snap it back to the
    // server's value. Focus alone does not count: a range input keeps
    // focus after a drag, and the slider must still follow the box.
    var dragging = false;
    var lastInputAt = 0;
    var noteTimer = null;
    var socFailed = false;
    slider.addEventListener("pointerdown", function () { dragging = true; });
    ["pointerup", "pointercancel"].forEach(function (ev) {
      slider.addEventListener(ev, function () { dragging = false; });
    });
    slider.addEventListener("input", function () { lastInputAt = Date.now(); });
    function operatorHolds() { return dragging || socSaving || socPending !== null || (Date.now() - lastInputAt) < 1500; }

    function sourceNote(lpNow) {
      var src = (lpNow && lpNow.soc_source) || "";
      var retention = lpNow.soc_retention === "session"
        ? " FTW keeps this level for the same charging session, including after a box restart."
        : lpNow.soc_retention === "error"
          ? " This level could not be saved for a box restart. Enter it again before relying on the plan after restarting."
          : " This level must be entered again after a box restart.";
      if (src === "assumed") return "Battery level needs confirmation. The plan currently assumes " + Math.round(lpNow.current_soc * 100) + " %. Drag to match the car." + retention;
      if (src === "vehicle") return "Reported by the car. Drag only to correct drift.";
      if (src === "completed") return "The car stopped asking for charge. Its actual battery level is not confirmed. Drag to match the car.";
      return "Estimated from energy delivered. Drag to the real value and the plan follows." + retention;
    }

    var socPending = null;
    var socSaving = false;
    var socRevision = 0;
    function sendSoc() {
      if (socSaving || socPending === null) return;
      var v = socPending;
      var revision = socRevision;
      socPending = null;
      socSaving = true;
      evWrite("/api/loadpoints/" + encodeURIComponent(lp.id) + "/soc", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ soc: v / 100 }),
      }).then(function (r) { return r.json().then(function (j) { return { ok: r.ok, body: j }; }); })
        .then(function (res) {
          if (!(res.ok && res.body && res.body.ok)) throw new Error((res.body && res.body.error) || "FTW refused the change.");
          if (revision !== socRevision) return;
          note.textContent = "Charge level saved: " + v + " %." +
            (!(lastLp.schedule && lastLp.schedule.soc > 0) && !lastLp.manual_active && !lastLp.surplus_only
              ? " Set a ready time, or choose Charge now." : " Reading the updated plan…");
          noteTimer = setTimeout(function () { noteTimer = null; if (!socFailed) note.textContent = sourceNote(lastLp); }, 6000);
          refreshEvModalAfterWrite().then(function () {
            if (revision === socRevision && lastLp.soc_retention === "error") {
              if (noteTimer) { clearTimeout(noteTimer); noteTimer = null; }
              socFailed = true;
              note.textContent = "Charge level updated: " + v + " %. It could not be saved for a box restart.";
            }
          });
        }).catch(function (e) {
          if (revision === socRevision) { socFailed = true; note.textContent = "Charge level not confirmed: " + e.message; }
        }).finally(function () {
          socSaving = false;
          if (socPending !== null) sendSoc();
        });
    }
    slider.addEventListener("change", function () {
      var v = parseInt(slider.value, 10);
      if (!isFinite(v) || v < 0 || v > 100) return;
      if (noteTimer) { clearTimeout(noteTimer); noteTimer = null; }
      socRevision++;
      socFailed = false;
      socPending = v;
      note.textContent = "Sending charge level: " + v + " %…";
      sendSoc();
    });

    var lastLp = lp;

    function drawTrack(lpNow) {
      track.textContent = "";
      ticks.textContent = "";
      var now = Date.now();
      var windows = (lpNow && Array.isArray(lpNow.plan_windows)) ? lpNow.plan_windows : [];
      planWrap.hidden = windows.length === 0 || !!lpNow.manual_active;
      var shown = 0;
      var shownWh = 0;
      windows.forEach(function (w) {
        var start = Math.max(w.start_ms, now);
        var end = Math.min(w.end_ms, now + EV_PLAN_HORIZON_MS);
        if (!(end > start)) return;
        var seg = document.createElement("div");
        seg.className = "ev-plan-window";
        seg.style.position = "absolute";
        seg.style.top = "0";
        seg.style.bottom = "0";
        seg.style.left = ((start - now) / EV_PLAN_HORIZON_MS * 100) + "%";
        seg.style.width = Math.max(0.8, (end - start) / EV_PLAN_HORIZON_MS * 100) + "%";
        seg.style.background = "var(--accent-e)";
        seg.style.borderRadius = "2px";
        seg.title = evFmtClock(w.start_ms) + "–" + evFmtClock(w.end_ms) + " · " + (w.wh / 1000).toFixed(1) + " kWh";
        track.appendChild(seg);
        shown++;
        shownWh += w.wh;
      });
      for (var h = 0; h <= 24; h += 6) {
        var t = document.createElement("span");
        t.textContent = h === 0 ? "now" : evFmtClock(now + h * 3600 * 1000);
        ticks.appendChild(t);
      }
      if (lpNow && lpNow.manual_active) {
        caption.textContent = shown > 0
          ? "Manual charge is selected. The plan below resumes when you return to it."
          : "Manual charge is selected. Nothing else is planned in the next 24 h.";
      } else if (shown > 0) {
        var first = windows[0];
        caption.textContent = "Charges " + evFmtClock(first.start_ms) + "–" + evFmtClock(first.end_ms) +
          (shown > 1 ? " and " + (shown - 1) + " more window" + (shown > 2 ? "s" : "") : "") +
          " · " + (shownWh / 1000).toFixed(1) + " kWh in the next 24 h.";
      } else if (lpNow && lpNow.plugged_in) {
        caption.textContent = "No charge window in the next 24 h.";
      } else {
        caption.textContent = "Plug in to see the plan for this car.";
      }
    }

    function update(lpNow, dNow) {
      lastLp = lpNow;
      capacityView.update(lpNow);
      var fresh = renderEvPlanStatus(lpNow, dNow);
      if (headline && headline.parentNode === box) {
        if (fresh) { box.replaceChild(fresh, headline); } else { box.removeChild(headline); }
      } else if (fresh) {
        box.insertBefore(fresh, planWrap);
      }
      headline = fresh;
      drawTrack(lpNow);
      var plugged = !!(lpNow && lpNow.plugged_in);
      socWrap.hidden = !plugged;
      if (!plugged) return;
      var cur = (lpNow.current_soc != null) ? Math.max(0, Math.min(100, Math.round(lpNow.current_soc * 100))) : null;
      if (!operatorHolds() && cur != null) {
        slider.value = String(cur);
        hdr.value.textContent = lpNow.soc_source === "assumed" ? "Not confirmed" : cur + "%";
      }
      if (!noteTimer && !socFailed && !operatorHolds()) note.textContent = sourceNote(lpNow);
    }

    update(lp, d);
    return { el: box, update: update, slider: slider };
  }

  function buildEvCapacityView(lp) {
    var wrap = document.createElement("details");
    wrap.style.cssText = "margin-top:0.6rem;font-size:0.85rem";
    var summary = document.createElement("summary");
    summary.style.cssText = "cursor:pointer;color:var(--text-dim)";
    wrap.appendChild(summary);
    var hint = document.createElement("p");
    hint.style.cssText = "color:var(--text-dim);margin:0.4rem 0";
    wrap.appendChild(hint);
    var label = document.createElement("label");
    label.textContent = "Usable battery size (kWh) ";
    var input = document.createElement("input");
    input.type = "number"; input.min = "1"; input.max = "300"; input.step = "0.1";
    input.setAttribute("inputmode", "decimal");
    input.setAttribute("aria-label", "Usable battery size, kWh");
    input.style.cssText = "width:7em;padding:0.3rem;border:1px solid var(--line);border-radius:4px;background:var(--bg);color:var(--fg)";
    label.appendChild(input); wrap.appendChild(label);
    var help = document.createElement("p");
    help.style.cssText = "color:var(--text-dim);margin:0.4rem 0";
    help.textContent = "Applies when you leave the field. Find the usable size in your car’s specifications.";
    wrap.appendChild(help);
    var note = document.createElement("p");
    note.setAttribute("role", "status");
    note.style.cssText = "color:var(--text-dim);margin:0.4rem 0";
    wrap.appendChild(note);
    var retry = document.createElement("button");
    retry.type = "button"; retry.textContent = "Try battery size again"; retry.hidden = true;
    wrap.appendChild(retry);
    var busy = false, dirty = false, currentCapacity = null, available = true;
    input.addEventListener("input", function () { dirty = true; });
    function save() {
      if (busy) return;
      var value = Number(input.value);
      if (!isFinite(value) || value < 1 || value > 300) { note.textContent = "Enter the usable battery size from 1 to 300 kWh."; note.setAttribute("role", "alert"); return; }
      busy = true; input.disabled = true; retry.hidden = true;
      note.setAttribute("role", "status"); note.textContent = "Sending battery size…";
      evWrite("/api/loadpoints/" + encodeURIComponent(lp.id) + "/vehicle", {
        method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ capacity_wh: Math.round(value * 1000) }),
      }).then(function (r) { return r.json().then(function (j) { if (!r.ok || !j.ok) throw new Error(j.error || "FTW refused the change."); return j; }); })
        .then(function () {
          dirty = false;
          note.textContent = "Battery size saved. Reading charging status…";
          return refreshEvModalAfterWrite().then(function () {
            note.textContent = !available ? "Battery size saved. Current charging status is unavailable."
              : currentCapacity !== null && currentCapacity !== Math.round(value * 1000)
                ? "Saved as the usual battery size. This session uses " + (currentCapacity / 1000) + " kWh."
                : "Battery size saved. The plan uses this size for its estimates.";
          });
        })
        .catch(function (err) { note.textContent = "Battery size not confirmed: " + err.message; note.setAttribute("role", "alert"); retry.hidden = false; })
        .finally(function () { busy = false; input.disabled = false; });
    }
    input.addEventListener("change", save); retry.addEventListener("click", save);
    function update(next) {
      var capacity = Number(next.vehicle_capacity_wh);
      currentCapacity = isFinite(capacity) && capacity > 0 ? capacity : null;
      available = !next.charger || next.charger.available !== false;
      wrap.hidden = !next.plugged_in || !(capacity > 0);
      summary.textContent = "Car battery · " + (capacity / 1000) + " kWh";
      hint.textContent = next.capacity_source === "default" ? "FTW is using a default size. Check it against your car." : "Used for estimates. Check this size if you use another car.";
      if (!busy && !dirty && document.activeElement !== input) input.value = String(capacity / 1000);
    }
    update(lp);
    return { el: wrap, update: update };
  }

  // Solar is a rule of the plan. A manual hold overrides it.
  function buildPVModeSection(lp) {
    var soBox = document.createElement("div");
    soBox.style.marginTop = "0.7rem";
    var soWrap = document.createElement("label");
    soWrap.style.cssText = "display:flex;align-items:center;gap:0.5rem;font-size:0.85rem;cursor:pointer";
    var soCb = document.createElement("input");
    soCb.type = "checkbox";
    soCb.setAttribute("role", "switch");
    soCb.style.accentColor = "var(--accent-e)";
    var soText = document.createElement("span");
    soText.textContent = "Only spare solar";
    soWrap.appendChild(soCb);
    soWrap.appendChild(soText);
    var soStatus = document.createElement("small");
    soStatus.style.cssText = "display:block;color:var(--text-dim);margin:0.25rem 0 0 1.5rem";
    soStatus.setAttribute("role", "status");
    var busy = false, failure = "", latest = lp;
    function update(next) {
      latest = next;
      if (busy) return;
      soCb.checked = !!latest.surplus_only;
      soCb.disabled = !!latest.manual_active || !!latest.manual_restore_unconfirmed;
      soStatus.textContent = failure || (evIsPaused(latest) || latest.manual_restore_unconfirmed
        ? "This rule resumes with the plan."
        : latest.manual_active
        ? "Charge now overrides this rule. It resumes when you return to the plan."
        : latest.surplus_only
          ? "No grid or home battery. Your target may not be reached in time."
          : "The plan may use grid power to reach your target.");
    }
    soCb.addEventListener("change", function () {
      var on = soCb.checked;
      busy = true;
      failure = "";
      soCb.disabled = true;
      soStatus.textContent = "Applying solar rule…";
      evWrite("/api/loadpoints/" + encodeURIComponent(lp.id) + "/target", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ surplus_only: on }),
      }).then(function (r) {
        if (!r.ok) return r.json().then(function (j) { throw new Error(j.error || ("HTTP " + r.status)); });
        soStatus.textContent = "Solar rule saved. Reading the plan…";
        // Keep it pending until the read finishes; a poll cannot undo the choice.
        return refreshEvModalAfterWrite();
      }).catch(function (e) {
        failure = "Solar rule not confirmed: " + e.message;
      }).finally(function () { busy = false; update(latest); });
    });
    soBox.appendChild(soWrap);
    soBox.appendChild(soStatus);
    soBox.update = update;
    update(lp);
    return soBox;
  }

  // buildScheduleSection — target SoC by a deadline + recurring + the
  // home battery threshold for spare solar. Persisted across
  // restarts; the backend rolls the deadline forward daily when Recurring
  // is set and arms the surplus-grab when the home battery is at/above the
  // threshold (5 pp release hysteresis).
  function buildScheduleSection(lp, hasPV) {
    var sched = (lp && lp.schedule) || {};
    // Convert "minutes-of-day-UTC" to a "HH:MM" string in the browser's
    // local zone. The UI shows local time everywhere; we marshal back to
    // UTC minutes on save.
    var hasSched = !!(sched.soc || sched.recurring || sched.surplus_unlock_bat_soc);
    var initLocalHHMM = hasSched ? utcMinsToLocalHHMM(typeof sched.time_of_day_min_utc === "number" ? sched.time_of_day_min_utc : 360) : "07:00";
    var initSoC = typeof sched.soc === "number" && sched.soc > 0 ? sched.soc * 100 : 80;
    var initRec = !!sched.recurring;
    var savedUnlock = typeof sched.surplus_unlock_bat_soc === "number" ? sched.surplus_unlock_bat_soc * 100 : 0;
    // Surplus on/off is derived from the saved threshold: > 0 ⇒ enabled.
    // The threshold input retains the last-used value (or defaults to 50)
    // so unchecking + re-checking doesn't wipe the user's pick.
    var initSurplus = savedUnlock > 0;
    var initUnlock = savedUnlock > 0 ? savedUnlock : 50;

    // The goal editor opens below the saved goal summary.
    var box = document.createElement("div");
    box.style.marginTop = "0.9rem";
    box.style.paddingTop = "0.9rem";
    box.style.borderTop = "1px solid var(--line)";

    // Schedule is persistent loadpoint state — operators can configure
    // tomorrow morning's target tonight before plugging in. Show a
    // small hint when the loadpoint isn't currently connected so saved
    // edits don't feel inert: they'll apply at the next plug-in.
    if (lp && !lp.plugged_in) {
      var unpluggedHint = document.createElement("div");
      unpluggedHint.textContent = "Car not plugged in. Edits are saved and apply at next plug-in.";
      unpluggedHint.style.fontSize = "0.72rem";
      unpluggedHint.style.color = "var(--text-dim)";
      unpluggedHint.style.marginBottom = "0.5rem";
      unpluggedHint.style.fontStyle = "italic";
      box.appendChild(unpluggedHint);
    }

    // Surplus-only ↔ schedule interaction. The schedule's explainer
    // promises "the planner uses cheap grid hours to fill the gap PV
    // can't cover" — but that promise is conditional on grid charging
    // being allowed in the first place. Surplus only is a hard
    // constraint in the MPC (mpc.go:474): EV actions that would import
    // grid are rejected outright, regardless of how close the deadline
    // is. So if both are on, the deadline becomes best-effort against
    // whatever PV happens to land. Flag that clearly to the operator
    // so they don't set a 05:00 deadline + surplus-only and expect
    // overnight grid charging to make it happen.
    //
    // The hint's visibility is wired live to the surplus-only checkbox
    // above, so toggling it gives instant feedback without waiting on
    // the network save round-trip.
    var surplusBestEffortHint = document.createElement("div");
    surplusBestEffortHint.textContent = "Only spare solar is on. Your target may not be reached in time.";
    surplusBestEffortHint.style.fontSize = "0.72rem";
    surplusBestEffortHint.style.color = "var(--fg)";
    surplusBestEffortHint.style.fontStyle = "italic";
    surplusBestEffortHint.style.marginBottom = "0.5rem";
    surplusBestEffortHint.style.padding = "0.4rem 0.55rem";
    surplusBestEffortHint.style.borderLeft = "2px solid var(--accent-e)";
    surplusBestEffortHint.style.background = "var(--ink-raised)";
    surplusBestEffortHint.style.display = (lp && lp.surplus_only) ? "" : "none";
    box.appendChild(surplusBestEffortHint);

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

    var recWrap = checkbox(initRec, "Repeat on chosen days");
    var surWrap = checkbox(initSurplus && !!hasPV, "Also use spare solar before the planned hours");
    var recCb = recWrap.input;
    var surCb = surWrap.input;

    // Weekdays. The wire's zero means every day (older rows and clients);
    // the chips show that as all seven lit, and send zero back for it,
    // the same spelling the webapp uses. Bit 0 = Monday (ISO).
    var initDays = (typeof sched.days === "number" && (sched.days & 0x7f) > 0) ? (sched.days & 0x7f) : 0x7f;
    var dayChips = [];
    var daysRow = document.createElement("div");
    daysRow.setAttribute("role", "group");
    daysRow.setAttribute("aria-label", "Days");
    daysRow.style.display = "flex";
    daysRow.style.gap = "0.3rem";
    daysRow.style.margin = "0.1rem 0 0.55rem";
    ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"].forEach(function (name, bit) {
      var chip = document.createElement("button");
      chip.type = "button";
      chip.textContent = name;
      chip.dataset.bit = String(bit);
      chip.style.flex = "1";
      chip.style.padding = "0.25rem 0";
      chip.style.fontFamily = "var(--mono)";
      chip.style.fontSize = "0.68rem";
      chip.style.letterSpacing = "0.06em";
      chip.style.borderRadius = "3px";
      chip.style.cursor = "pointer";
      chip.style.border = "1px solid var(--line)";
      chip.on = !!(initDays & (1 << bit));
      dayChips.push(chip);
      daysRow.appendChild(chip);
    });
    function paintChips() {
      dayChips.forEach(function (chip) {
        chip.setAttribute("aria-pressed", chip.on ? "true" : "false");
        chip.style.background = chip.on ? "var(--accent-e)" : "transparent";
        chip.style.color = chip.on ? "#0a0a0a" : "var(--text-dim)";
        chip.style.borderColor = chip.on ? "var(--accent-e)" : "var(--line)";
      });
    }
    function selectedDays() {
      var mask = 0;
      dayChips.forEach(function (chip) { if (chip.on) mask |= 1 << Number(chip.dataset.bit); });
      return mask;
    }
    paintChips();

    // Target: same header + full-width slider treatment as the car's
    // current charge above the goal.
    var targetHdr = sliderHeader("Charge to", Math.max(0, Math.min(100, Math.round(initSoC))) + "%");
    box.appendChild(targetHdr.row);
    var targetSlider = fullWidthSlider(Math.max(0, Math.min(100, Math.round(initSoC))), targetHdr.value);
    targetSlider.min = "10";
    targetSlider.step = "5";
    targetSlider.setAttribute("aria-label", "Target charge, percent");
    box.appendChild(targetSlider);
    box.appendChild(row("Ready by", timeInp));

    var checkRow = document.createElement("div");
    checkRow.style.display = "flex";
    checkRow.style.flexDirection = "column";
    checkRow.style.gap = "0.35rem";
    checkRow.style.marginBottom = "0.55rem";
    checkRow.appendChild(recWrap);
    box.appendChild(checkRow);
    box.appendChild(daysRow);
    var solarDetails = document.createElement("details");
    var solarSummary = document.createElement("summary");
    solarSummary.textContent = "Solar timing";
    solarSummary.style.cssText = "cursor:pointer;font-size:0.8rem;color:var(--text-dim)";
    solarDetails.appendChild(solarSummary);
    if (hasPV) box.appendChild(solarDetails);
    // Surplus-from-PV only makes sense on sites with a PV driver — the
    // bat-SoC unlock would have no surplus to grab otherwise. Omit the
    // checkbox + threshold entirely on PV-less sites.
    if (hasPV) {
      solarDetails.appendChild(surWrap);
    }

    var unlockHint = document.createElement("small");
    unlockHint.style.display = "block";
    unlockHint.style.color = "var(--text-dim)";
    unlockHint.style.marginTop = "0.2rem";
    unlockHint.style.marginBottom = "0.3rem";
    unlockHint.textContent = "Only once the home battery is at or above this level.";

    var thresholdRow = row("Home battery ≥", unlockWrap);

    if (hasPV) {
      solarDetails.appendChild(unlockHint);
      solarDetails.appendChild(thresholdRow);
    }

    function applySurplusGate() {
      var on = surCb.checked;
      unlockWrap.input.disabled = !on;
      thresholdRow.style.opacity = on ? "1" : "0.4";
      thresholdRow.style.pointerEvents = on ? "auto" : "none";
      unlockHint.style.opacity = on ? "1" : "0.55";
    }
    function applyDaysGate() {
      // Inline display:flex beats the [hidden] attribute; toggle display.
      daysRow.style.display = recCb.checked ? "flex" : "none";
    }
    if (hasPV) {
      applySurplusGate();
    }
    applyDaysGate();

    // One explicit action remains: removing the schedule. Everything
    // else saves as it changes (see scheduleSave below).
    var btnRow = document.createElement("div");
    btnRow.style.display = "flex";
    btnRow.style.gap = "0.5rem";
    btnRow.style.marginTop = "0.55rem";
    btnRow.style.justifyContent = "flex-end";

    function mkBtn(label) {
      var b = document.createElement("button");
      b.type = "button";
      b.textContent = label;
      b.style.padding = "0.3rem 0.8rem";
      b.style.fontSize = "0.8rem";
      b.style.fontFamily = "var(--mono)";
      b.style.letterSpacing = "0.06em";
      b.style.textTransform = "uppercase";
      b.style.borderRadius = "3px";
      b.style.cursor = "pointer";
      b.style.border = "1px solid var(--line)";
      b.style.background = "transparent";
      b.style.color = "var(--fg)";
      return b;
    }
    var chooseBtn = mkBtn("Use " + Math.round(initSoC) + " % by " + initLocalHHMM);
    chooseBtn.style.textTransform = "none";
    chooseBtn.style.letterSpacing = "normal";
    chooseBtn.addEventListener("click", scheduleSave);
    btnRow.appendChild(chooseBtn);
    var clearBtn = mkBtn("Remove schedule");
    function paintClear() {
      chooseBtn.hidden = hasSched;
      clearBtn.hidden = !hasSched;
      clearBtn.disabled = !hasSched;
      clearBtn.style.opacity = hasSched ? "1" : "0.4";
    }
    paintClear();
    btnRow.appendChild(clearBtn);
    box.appendChild(btnRow);

    var status = document.createElement("small");
    status.style.display = "block";
    status.style.color = "var(--text-dim)";
    status.style.marginTop = "0.4rem";
    status.style.minHeight = "1em";
    status.textContent = hasSched
      ? "Changes save as you make them; the plan above follows."
      : "No goal set yet. Choose this goal, or change the level or time.";
    box.appendChild(status);

    // Every control writes when it changes: the slider on release, the
    // time and checkboxes on change, a chip on click. Debounced so a
    // burst of edits is one request; a stale response never overwrites
    // a newer status. The box replans before it answers the schedule
    // write, so the refetch right after redraws the plan view above
    // from the new schedule. No Save button.
    var saveTimer = null;
    var saveSeq = 0;
    var writeQueue = Promise.resolve();
    var statusTimer = null;
    function scheduleSave() {
      if (saveTimer) clearTimeout(saveTimer);
      if (statusTimer) { clearTimeout(statusTimer); statusTimer = null; }
      saveSeq++;
      chooseBtn.disabled = true;
      status.textContent = "Applying schedule…";
      saveTimer = setTimeout(function () { saveTimer = null; doSave(); }, 400);
    }
    function doSave() {
      var seq = saveSeq;
      var localHHMM = timeInp.value || initLocalHHMM;
      var minUTC = localHHMMToUtcMins(localHHMM);
      // Surplus checkbox gates the threshold: when off (or hidden on
      // PV-less sites), the threshold is sent as 0 — the backend
      // interprets 0 as "feature disabled".
      var unlockVal = (hasPV && surCb.checked) ? Number(unlockWrap.input.value) : 0;
      var days = recCb.checked ? selectedDays() : 0;
      if (recCb.checked && days === 0) {
        status.textContent = "Pick at least one day.";
        return;
      }
      var body = {
        schedule: {
          soc: Number(targetSlider.value) / 100,
          time_of_day_min_utc: minUTC,
          recurring: !!recCb.checked,
          // All seven days is the wire's zero.
          days: days === 0x7f ? 0 : days,
          surplus_unlock_bat_soc: unlockVal > 0 ? unlockVal / 100 : 0,
        },
      };
      // Complete writes in order, even when the charger or planner is slow.
      writeQueue = writeQueue.catch(function () {}).then(function () {
        if (seq !== saveSeq) return;
        return evWrite("/api/loadpoints/" + encodeURIComponent(lp.id) + "/target", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(body),
        }).then(function (r) {
          if (!r.ok) return r.json().then(function (j) { throw new Error(j.error || ("HTTP " + r.status)); });
          if (seq !== saveSeq) return;
          hasSched = true;
          paintClear();
          status.textContent = "Schedule saved.";
          refreshEvModalAfterWrite();
        }).catch(function (e) {
          if (seq === saveSeq) {
            chooseBtn.disabled = false;
            chooseBtn.textContent = "Use " + targetSlider.value + " % by " + timeInp.value;
            status.textContent = "Schedule not confirmed: " + e.message;
          }
        });
      });
    }

    targetSlider.addEventListener("change", scheduleSave);
    timeInp.addEventListener("change", scheduleSave);
    recCb.addEventListener("change", function () { applyDaysGate(); scheduleSave(); });
    dayChips.forEach(function (chip) {
      chip.addEventListener("click", function () { chip.on = !chip.on; paintChips(); scheduleSave(); });
    });
    if (hasPV) {
      surCb.addEventListener("change", function () { applySurplusGate(); scheduleSave(); });
      unlockWrap.input.addEventListener("change", scheduleSave);
    }

    clearBtn.addEventListener("click", function () {
      clearBtn.disabled = true;
      if (saveTimer) { clearTimeout(saveTimer); saveTimer = null; }
      saveSeq++;
      status.textContent = "Removing…";
      writeQueue = writeQueue.catch(function () {}).then(function () {
        return evWrite("/api/loadpoints/" + encodeURIComponent(lp.id) + "/target", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ schedule: null }),
        }).then(function (r) {
          if (!r.ok) return r.json().then(function (j) { throw new Error(j.error || ("HTTP " + r.status)); });
          status.textContent = "Schedule removed.";
          schedNeedsRebuild = true;
          refreshEvModalAfterWrite();
        }).catch(function (e) {
          status.textContent = "Removal not confirmed: " + e.message;
          clearBtn.disabled = false;
        });
      });
    });

    box.update = function (nextLp) {
      surplusBestEffortHint.style.display = nextLp.surplus_only && !nextLp.manual_active ? "" : "none";
    };
    return box;
  }

  // One session view. Opening settings never changes the charging mode.
  function buildEvControls(lp, hasPV) {
    var container = document.createElement("div");
    container.className = "ev-controls";
    var manual = buildManualChargeSection(lp);
    container.appendChild(manual.el);

    var goal = document.createElement("section");
    goal.className = "ev-goal";
    goal.style.cssText = "margin-top:1rem;padding:0.8rem;border:1px solid var(--line);border-radius:8px";
    var heading = document.createElement("div");
    heading.textContent = "Your goal";
    heading.style.cssText = "font-weight:600;font-size:0.95rem;margin-bottom:0.3rem";
    goal.appendChild(heading);
    var summary = document.createElement("p");
    summary.style.cssText = "margin:0;font-size:0.9rem";
    goal.appendChild(summary);
    var suspended = document.createElement("small");
    suspended.style.cssText = "display:block;color:var(--text-dim);margin-top:0.3rem";
    goal.appendChild(suspended);

    var editor = document.createElement("details");
    var editLabel = document.createElement("summary");
    editLabel.style.cssText = "cursor:pointer;color:var(--accent-e);font-size:0.85rem;margin-top:0.65rem";
    editor.appendChild(editLabel);
    var schedule = buildScheduleSection(lp, hasPV);
    editor.appendChild(schedule);
    goal.appendChild(editor);
    var solar = hasPV || lp.surplus_only ? buildPVModeSection(lp) : null;
    if (solar) goal.appendChild(solar);
    container.appendChild(goal);

    container.update = function (nextLp, d) {
      manual.el.hidden = !nextLp.plugged_in;
      manual.update(nextLp, d);
      var s = nextLp.schedule;
      var hasGoal = !!(s && s.soc > 0);
      summary.textContent = hasGoal
        ? Math.round(s.soc * 100) + " % by " + utcMinsToLocalHHMM(s.time_of_day_min_utc) +
          (s.recurring ? " · repeats" : " · once")
        : "No ready time set.";
      editLabel.textContent = hasGoal ? "Change goal" : "Set a ready time";
      suspended.textContent = evIsPaused(nextLp) || nextLp.manual_restore_unconfirmed
        ? "Resume the plan to use this goal. Edits apply then."
        : nextLp.manual_active
        ? "Charge now overrides this goal. Edits apply when you return to the plan."
        : "Changes apply as you make them.";
      schedule.update(nextLp);
      if (solar) solar.update(nextLp);
    };
    container.update(lp, null);
    return container;
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

    // Planet click routing. EV → EV modal scoped to driver. A battery
    // driver opens its own short hold; the merged battery opens pool
    // control. Grid → grid modal hosting the peak-import ceiling and
    // the legacy grid target setpoint.
    var gridModal = document.getElementById("grid-modal");
    if (energyFlowEl) {
      energyFlowEl.addEventListener("ftw-planet-click", function (e) {
        var d = (e && e.detail) || {};
        if (d.role === "ev") openEvModal(d.name || null);
        if (d.role === "battery") {
          var drv = (lastStatusPayload && lastStatusPayload.drivers) || {};
          var clicked = drv[d.name || ""] || {};
          if (clicked.observe_only) return;
          var bc = document.getElementById("battery-control");
          if (bc && typeof bc.open === "function") bc.open(d.name || d.id || "");
        }
        if (d.role === "pv") {
          var pc = document.getElementById("pv-control");
          if (pc && typeof pc.open === "function") {
            // d.id is the driver id when the user clicked an expanded
            // per-driver bubble; "" / undefined opens at the aggregate
            // scope from the merged bubble.
            pc.open(d.id || "");
          }
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
      if (hasControllableBatteryDrivers(lastStatusPayload)) {
        cardBat.classList.add("clickable");
        cardBat.setAttribute("role", "button");
        cardBat.setAttribute("tabindex", "0");
        cardBat.setAttribute("aria-label", "Open battery controls");
      }
      var openBat = function () {
        if (!hasControllableBatteryDrivers(lastStatusPayload)) return;
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

    // Legacy Start/Pause/Resume footer buttons were removed in favour of
    // the in-body Manual Charge control (buildManualControl): an amp slider
    // + Start/Stop that pins a persistent manual hold at a chosen current
    // (Start, overrides surplus) or clears it (Stop, back to automatic).
    // The POST /api/ev/command endpoint stays for HA / scripts.
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
        loadHistory(chartRange, true);
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
      ["#3b82f6", "Charged"], ["#f59e0b", "Discharged"], ["var(--white-s)", "Load"],
    ] : [
      ["#ef4444", "Grid"], ["#22c55e", "PV"], ["var(--white-s)", "Load"],
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
  function loadHistory(range, force) {
    var points = CHART_POINTS;
    return fetchHistory(range || "5m", points, force)
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
    syncStatusPolling();
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
  var historyCakeWaitingForUpgrade = false;
  var historyCakeReqSeq = 0;

  // historyState mirrors both toggles so the Bars/Cakes view and
  // the Week/Month range stay coordinated. Only the cake re-fetches
  // /api/energy/daily on a range change; the bar tiles each
  // re-fetch themselves when their range= attribute is updated.
  var historyState = { range: "week", view: "bars" };

  function fetchHistoryCake() {
    var seq = ++historyCakeReqSeq;
    historyCakeEl = $("history-cake-el");
    if (!historyCakeEl || typeof historyCakeEl.setTotals !== "function") {
      if (!historyCakeWaitingForUpgrade && window.customElements && customElements.whenDefined) {
        historyCakeWaitingForUpgrade = true;
        customElements.whenDefined("ftw-energy-cake").then(function () {
          historyCakeWaitingForUpgrade = false;
          if (historyState.view === "cakes") fetchHistoryCake();
        });
      }
      return;
    }
    if (historyCakeWrap) historyCakeWrap.classList.add("loading");
    var days = historyState.range === "month" ? 30 : 7;
    var clearLoading = function () {
      if (seq !== historyCakeReqSeq) return;
      if (historyCakeWrap) historyCakeWrap.classList.remove("loading");
    };
    // Local API read.
    apiFetch("/api/energy/daily?days=" + days)
      .then(function (r) { return r.json(); })
      .then(function (j) {
        if (seq !== historyCakeReqSeq) return;
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

  // ---- Live 24h history (battery + SoC) ----
  // Self-contained: fetches /api/history once a minute, draws a
  // compact stacked canvas of (battery action bars, SoC line) over
  // the last 24 h. Mirrors the Plan card's lower charts so the two
  // cards have matching visual weight. Read-only — no interaction.
  function renderLiveHistory(items) {
    var canvas = document.getElementById("live-history-chart");
    if (!canvas || !items || !items.length) return;
    // Normalise the /api/history payload: `ts` for the timestamp and
    // `bat_soc` (0–1 fraction) for the SoC field, plus `bat_w` from
    // the row blob. Skip rows with no battery sample so the chart
    // doesn't draw spurious zero bars.
    var points = items
      .filter(function (it) { return it.bat_w != null || it.bat_soc != null; })
      .map(function (it) {
        return {
          ts_ms: it.ts || it.ts_ms,
          bat_w: it.bat_w || 0,
          soc_pct: (it.bat_soc != null) ? it.bat_soc * 100 : null,
        };
      });
    if (!points.length) return;
    var dpr = window.devicePixelRatio || 1;
    var cssW = canvas.parentElement.clientWidth || canvas.width;
    var cssH = 140;
    if (canvas.width !== Math.round(cssW * dpr) || canvas.height !== Math.round(cssH * dpr)) {
      canvas.width = Math.round(cssW * dpr);
      canvas.height = Math.round(cssH * dpr);
    }
    canvas.style.width = cssW + "px";
    canvas.style.height = cssH + "px";
    var ctx = canvas.getContext("2d");
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, cssW, cssH);

    var C = chartColors();
    var pad = { left: 36, right: 36, top: 8, bottom: 18 };
    var plotW = cssW - pad.left - pad.right;
    var plotH = cssH - pad.top - pad.bottom;
    if (plotW <= 0 || plotH <= 0) return;

    // x: time. Use first/last point timestamps for the window.
    var t0 = points[0].ts_ms;
    var t1 = points[points.length - 1].ts_ms;
    var span = Math.max(1, t1 - t0);
    var xOf = function (ts) { return pad.left + (ts - t0) / span * plotW; };

    // Battery action axis: symmetric around 0. Find absmax for bat_w.
    var batMax = 0;
    for (var i = 0; i < points.length; i++) {
      var b = Math.abs(points[i].bat_w || 0);
      if (b > batMax) batMax = b;
    }
    if (batMax < 1000) batMax = 1000; // sane minimum
    var batMid = pad.top + plotH * 0.55;
    var batH = plotH * 0.55;
    var yOfBat = function (w) {
      var frac = (w || 0) / batMax;
      return batMid - frac * (batH / 2);
    };

    // Zero baseline for battery
    ctx.strokeStyle = C.grid;
    ctx.lineWidth = 1;
    ctx.beginPath();
    ctx.moveTo(pad.left, batMid);
    ctx.lineTo(pad.left + plotW, batMid);
    ctx.stroke();

    // Battery action bars. Charge (positive) above zero in amber-ish
    // (matches Plan's "charge" colour), discharge in violet-ish.
    var barW = Math.max(1, plotW / points.length - 0.5);
    for (var j = 0; j < points.length; j++) {
      var p = points[j];
      var bw = p.bat_w || 0;
      if (Math.abs(bw) < 50) continue; // suppress noise
      var x = xOf(p.ts_ms);
      var y = yOfBat(bw);
      ctx.fillStyle = bw > 0 ? "rgba(251,191,36,0.85)" : "rgba(167,139,250,0.85)";
      var h = Math.abs(y - batMid);
      var top = bw > 0 ? y : batMid;
      ctx.fillRect(x - barW / 2, top, barW, h);
    }

    // SoC line. Plotted in a dedicated bottom strip with its own
    // 0-100% scale and right-axis labels.
    var socTop = pad.top + plotH * 0.62;
    var socH = plotH * 0.38;
    var socOf = function (pct) {
      var v = Math.max(0, Math.min(100, pct || 0));
      return socTop + socH - (v / 100) * socH;
    };
    ctx.strokeStyle = "rgba(34,211,238,0.95)";
    ctx.lineWidth = 1.5;
    ctx.beginPath();
    var started = false;
    for (var k = 0; k < points.length; k++) {
      var sp = points[k];
      if (sp.soc_pct == null) continue;
      var sx = xOf(sp.ts_ms);
      var sy = socOf(sp.soc_pct);
      if (!started) { ctx.moveTo(sx, sy); started = true; }
      else { ctx.lineTo(sx, sy); }
    }
    ctx.stroke();

    // Axis labels — small, mono, dim.
    ctx.fillStyle = C.dim;
    ctx.font = "10px ui-monospace, SFMono-Regular, Menlo, Monaco, monospace";
    ctx.textAlign = "right";
    ctx.textBaseline = "middle";
    // battery: max charge / max discharge labels at the edges
    ctx.fillText((batMax / 1000).toFixed(1) + "kW", pad.left - 4, pad.top + 6);
    ctx.fillText("-" + (batMax / 1000).toFixed(1) + "kW", pad.left - 4, batMid + (batH / 2) - 6);
    // SoC: 100% / 0% on the right
    ctx.textAlign = "left";
    ctx.fillText("100%", pad.left + plotW + 4, socTop + 6);
    ctx.fillText("0%", pad.left + plotW + 4, socTop + socH - 6);
    // time axis: 24h ago / now
    ctx.fillStyle = C.muted;
    ctx.textAlign = "left";
    ctx.fillText("24h ago", pad.left, cssH - 4);
    ctx.textAlign = "right";
    ctx.fillText("now", pad.left + plotW, cssH - 4);
  }

  // Shared, deduped /api/history fetch. Several triggers ask for the SAME
  // range+points payload — boot, the 1-min poll, and (undebounced) every
  // window resize — so on first load a layout-driven resize storm would
  // otherwise fan out into many identical requests. Coalesce in-flight calls
  // and reuse a fresh result for a short TTL; pass force=true to bypass the
  // cache when a genuinely fresh sample is wanted (the periodic poll).
  // Mirrors ftw-history-card.js's dailyFetchCache.
  var HISTORY_CACHE_TTL_MS = 15000;
  var historyFetchCache = Object.create(null); // "range|points" -> { at, data?, promise? }
  function fetchHistory(range, points, force) {
    var key = range + "|" + points;
    var now = Date.now();
    var c = historyFetchCache[key];
    // Never overlap the same history read. A forced timer or visibility
    // refresh may bypass completed cache data, but not work already in flight.
    if (c && c.promise) return c.promise;
    if (!force && c && c.data && (now - c.at) < HISTORY_CACHE_TTL_MS) {
      return Promise.resolve(c.data);
    }
    var promise = boundedApiRead(
      "/api/history?range=" + range + "&points=" + points,
      function (r) { return r.ok ? r.json() : null; }
    )
      .then(function (data) { historyFetchCache[key] = { at: Date.now(), data: data }; return data; })
      .catch(function (err) {
        var cur = historyFetchCache[key];
        if (cur && cur.promise === promise) delete historyFetchCache[key];
        throw err;
      });
    historyFetchCache[key] = { at: now, promise: promise };
    return promise;
  }

  var lastLiveHistFetch = 0;
  function fetchLiveHistory(force) {
    if (document.hidden) return Promise.resolve();
    lastLiveHistFetch = Date.now();
    return fetchHistory("24h", 288, force) // 5-min cadence
      .then(function (d) {
        if (!d || !d.items) return;
        renderLiveHistory(d.items);
      })
      .catch(function () { /* silent — chart just shows last state */ });
  }

  // ---- Init ----
  renderModeCatalog(); // build mode buttons from the server's canonical catalog
  syncStatusPolling();
  window.addEventListener("resize", function () {
    // Redraw the 24h chart at the new width. Reuses the cached payload
    // (fetchHistory's short TTL) so a first-load resize storm doesn't
    // fan out into identical /api/history requests — only the data is
    // shared; renderLiveHistory still re-measures the canvas each call.
    fetchLiveHistory();
  });
  requestAnimationFrame(animationFrame);
})();
