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
  let animating = true;              // 30fps redraw loop flag
  let lastDataTs = 0;                // browser-clock timestamp of newest pushed point
  let lastPushAt = 0;                // browser-clock timestamp of last push attempt — for dedupe (NEVER mix with server ts)
  let lastFlashAt = 0;               // browser-clock timestamp of last "new data" flash

  // ---- Owner / CONTROL fetch (FIX-B) ----------------------------------------
  // Every owner + state-changing API call (mode, target, peak limit, EV command,
  // loadpoint schedule, driver lifecycle, sign-out, …) must ride the STRICT P2P
  // transport so its body + the owner session cookie never traverse the untrusted
  // relay on the public home route. p2pFetchStrict fails closed (synthetic 503)
  // when the channel is down on a public origin; we additionally fail closed when
  // p2p.js never LOADED at all on a non-LAN origin, instead of raw-fetching the
  // owner/control body to the relay. Read-only GETs of non-secret data may still
  // use plain fetch (no body, cookie stripped) — this is for OWNER + CONTROL calls
  // only.
  function isLanFallbackOrigin() {
    // Genuine-LAN origin = the Pi serves this page directly (relay not in path),
    // so a raw fetch is safe. Prefer p2p.js's own isLanOrigin (single source of
    // truth); only when p2p.js never loaded do we conservatively treat a dotted
    // public host as NOT-LAN and fail closed.
    if (window.ftwP2P && typeof window.ftwP2P.isLanOrigin === "function") {
      try { return window.ftwP2P.isLanOrigin(); } catch (e) { /* fall through */ }
    }
    if (/^\/me\/[^/]+\//.test(location.pathname)) return false; // relay tunnel prefix
    var h = (location.hostname || "").toLowerCase();
    if (h === "localhost" || h === "::1" || h === "[::1]") return true;
    if (h.slice(-6) === ".local" || h.indexOf(".") === -1) return true; // *.local / single-label
    if (/^10\./.test(h) || /^127\./.test(h) || /^192\.168\./.test(h) ||
        /^169\.254\./.test(h) || /^172\.(1[6-9]|2\d|3[01])\./.test(h) ||
        /^100\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\./.test(h)) return true; // 100.64/10 CGNAT (Tailscale)
    var hv6 = h.replace(/^\[|\]$/g, "");
    if (/^f[cd][0-9a-f]{2}:/.test(hv6) || /^fe[89ab][0-9a-f]:/.test(hv6)) return true;
    return false; // dotted public host → NOT LAN → fail closed
  }
  // ownerWriteFailClosed mimics enough of a fetch Response that callers handle it
  // uniformly; the owner/control body NEVER leaves the browser.
  function ownerWriteFailClosed(path) {
    var msg = "Secure channel unavailable — reconnecting. This control request was NOT sent to the relay.";
    return Promise.resolve({
      ok: false, status: 503, url: path, headers: new Headers(),
      json: function () { return Promise.resolve({ error: msg, retry: true }); },
      text: function () { return Promise.resolve(msg); }
    });
  }
  // ownerFetch is the single owner/CONTROL fetch entry point. Strict when the P2P
  // transport is present; fail-closed on a public origin when it isn't; raw fetch
  // only on a genuine LAN.
  function ownerFetch(path, opts) {
    opts = opts || {};
    if (typeof window.p2pFetchStrict === "function") return window.p2pFetchStrict(path, opts);
    if (window.p2pFetch) return window.p2pFetch(path, Object.assign({ strict: true }, opts));
    if (!isLanFallbackOrigin()) return ownerWriteFailClosed(path);
    return fetch(path, opts);
  }

  function waitForOwnerTransport(timeoutMs) {
    if (!window.ftwP2P || isLanFallbackOrigin()) return Promise.resolve(true);
    if (typeof window.ftwP2P.state === "function" && window.ftwP2P.state() === "direct") {
      return Promise.resolve(true);
    }
    var connectP = typeof window.ftwP2P.connect === "function"
      ? window.ftwP2P.connect().catch(function () { return false; })
      : Promise.resolve(false);
    return new Promise(function (resolve) {
      var done = false;
      var timer = setTimeout(function () {
        if (done) return;
        done = true;
        resolve(false);
      }, timeoutMs || 9000);
      function finish(ok) {
        if (done) return;
        if (!ok) return;
        done = true;
        clearTimeout(timer);
        resolve(true);
      }
      if (typeof window.ftwP2P.onState === "function") {
        window.ftwP2P.onState(function (s) { finish(s === "direct"); });
      }
      connectP.then(finish);
    });
  }

  function ownerTransportReady() {
    if (isLanFallbackOrigin()) return true;
    return !!(window.ftwP2P &&
      typeof window.ftwP2P.state === "function" &&
      window.ftwP2P.state() === "direct");
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

  // Latest MPC plan — refreshed every 30s. Drives the forward-looking
  // dashed PV + Load forecast on the live chart (right-hand segment
  // extending past "now").
  var chartPlan = null;
  function refreshChartPlan() {
    // Owner read (carries the session cookie) — strict (FIX-B).
    ownerFetch("/api/mpc/plan")
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
  const bceInfo = $("battery-covers-ev-info");
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
    } else if (data.load_w != null) {
      out.load_w = smoothDisplayNumber("site:load_w", data.load_w, now);
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
      pushChartData(data, batteryTargetsByDriver);
    } catch (e) { console.error("pushChartData error:", e); }

    data = smoothStatusForDisplay(data);

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
    var batTargetDisp = document.getElementById("bat-target-display");
    if (batTargetDisp) {
      batTargetDisp.textContent = hasBatteryTarget ? batteryTargetLine(totalBatteryTargetW) : "";
    }

    // Live stats strip — mirrors the per-tile values up top but in one
    // mono-typed line above the live chart, so the Live card matches
    // the Plan card's information density. Each cell colours by sign
    // / direction so an operator scans the row and immediately sees
    // who's importing, exporting, charging, or idle.
    updateLiveStat("grid", data.grid_w, signClass("grid", data.grid_w));
    updateLiveStat("pv", -data.pv_w, "is-export"); // PV is site-signed negative; show as positive generation
    updateLiveStat("load", data.load_w, "is-neutral");
    updateLiveStat("bat", data.bat_w, signClass("bat", data.bat_w));
    updateLiveSocStat(data.bat_soc);

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

      // Today's totals are aggregate across all drivers; per-driver kWh split
      // is not in the API. Mark them as aggregate-only so the energy-flow
      // component can show them on folded bubbles without duplicating the
      // same total on every individual inverter.
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
      var pvDailyMembers = 0;
      var batDailyMembers = 0;
      Object.keys(drvs).forEach(function (name) {
        var d = drvs[name] || {};
        if (d.pv_w != null) pvDailyMembers++;
        if (d.bat_w != null) batDailyMembers++;
      });
      Object.keys(drvs).forEach(function (name) {
        var d = drvs[name] || {};
        var online = d.status !== "offline" && d.status !== "disabled" && !d.not_running;
        if (!online) return;
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
            dailyScope: "aggregate",
            dailyAggregateMembers: pvDailyMembers,
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
          var bTargetLine = batteryTargetLine(batteryTargetsByDriver[name]);
          planets.push({
            id: "bat-" + name, corner: "top-right", title: "BATTERY", name: name, role: "battery",
            kw: bKw, toHub: bKw < 0,
            color: bColor,
            sub: bTargetLine,
            soc: d.bat_soc != null ? Math.round(d.bat_soc * 100) : null,
            dailyKwhParts: batDailyParts,
            dailyScope: "aggregate",
            dailyAggregateMembers: batDailyMembers,
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

      // Self-powered today: share of recorded house consumption sourced
      // from PV/battery over the whole day. Daily EV energy is not split
      // into this aggregate yet, while the realtime component includes
      // active EV load because it is visible in the live balance.
      // Clamped 0..100 because metering glitches can briefly report
      // import > load.
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
      if (bceInfo) bceInfo.hidden = !data.battery_covers_ev;
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
    return ownerFetch("/api/drivers/" + encodeURIComponent(name) + "/" + action, { method: "POST" })
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
    return ownerFetch("/api/v2x/command", {
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
  // SHARED V2X manual-command surface (parseResponseError, v2xCommand,
  // formatOptionalW, renderV2XControls + the click handler and card body
  // below) is intentionally kept byte-identical with the same block in
  // web/app.js. There is no shared module system — each file is a
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
  var ownerDataPrimed = false;
  // Loadpoint cache — keyed by driver_name so the EV-planet builder
  // in render() can look up vehicle SoC + charge-limit without a
  // second round of fetches per status tick. Refreshed in parallel
  // with /api/status. `null` until the first fetch lands; missing
  // entries mean "no loadpoint for this driver" and the planet
  // falls back to legacy kW-only rendering.
  var loadpointsByDriver = null;
  // Last successful /api/status payload — surfaced so secondary
  // consumers (e.g. the EV modal's 5 s refresh) can read derived
  // facts like siteHasPV() without re-fetching. `null` until the
  // first fetch lands; consumers MUST handle null.
  var lastStatusPayload = null;
  function ownerDataAllowed() {
    return isLanFallbackOrigin() || (!authGateActive && !ownerNotAuthed);
  }
  function fetchStatus() {
    if (!ownerDataAllowed()) return Promise.resolve(false);
    // Route the hot poll over the direct P2P DataChannel when it's up. STRICT
    // mode (FIX-2): the owner API (/api/status etc.) must never ride the cleartext
    // relay on the public home route — strict fails closed (synthetic 503) if the
    // channel is down, while still allowing the relay fallback on a genuine-LAN
    // origin where the Pi serves the page directly. A 503 here just shows
    // "reconnecting" until the channel recovers (p2p.js auto-retries).
    // Owner reads carry the owner session cookie, so route them STRICT too (FIX-B):
    // ownerFetch fails closed on a public origin when no channel/transport is
    // available rather than sending the cookie to the relay. A 503 here just shows
    // "reconnecting" until the channel recovers (p2p.js auto-retries).
    var xfetch = ownerFetch;
    return Promise.all([
      xfetch("/api/status").then(function (r) { if (!r.ok) throw new Error("HTTP " + r.status); return r.json(); }),
      xfetch("/api/loadpoints").then(function (r) { return r.ok ? r.json() : null; }).catch(function () { return null; }),
      xfetch("/api/health").then(function (r) { return r.ok ? r.json() : null; }).catch(function () { return null; }),
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
    if (authGateActive || ownerNotAuthed) return; // auth-pending/logged-out empty status is not config state
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
    if (authGateActive || ownerNotAuthed) { if (existing) existing.remove(); return; } // not our config to judge before auth
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
    // CONTROL write — strict (FIX-B): never send the mode change to the relay.
    ownerFetch("/api/mode", {
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

  function setEvCharging(w) {
    postJson("/api/ev_charging", { power_w: w, active: w > 0 }).catch(function () {});
  }

  function setBatteryCoversEV(enabled) {
    postJson("/api/battery_covers_ev", { enabled: !!enabled }).catch(function () {});
  }

  function postJson(url, body) {
    // CONTROL write — strict (FIX-B). Covers /api/target, /api/peak_limit,
    // /api/peak_import_ceiling, /api/ev_charging, /api/battery_covers_ev,
    // /api/ev/command, … (every state-changing dashboard knob routes here).
    return ownerFetch(url, {
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
      if (bceInfo) bceInfo.hidden = !bceToggle.checked;
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

  // EV modal sub-elements held across refreshes. The status table is
  // updated in place on every poll. The schedule section is mounted
  // exactly once per (modal-open × LP) and is NEVER detached on a poll
  // — detaching+reattaching a focused <input> blurs it mid-keystroke
  // and resets caret position. After Save/Clear we set
  // schedNeedsRebuild=true so the next poll picks up the new
  // authoritative server state.
  var statusTableEl = null;
  var schedSectionEl = null;
  var schedLpId = null;
  var schedNeedsRebuild = false;
  // Manual amp-slider override section — same once-per-LP lifecycle as
  // the schedule section so the slider keeps focus/value between polls.
  var manualSectionEl = null;
  var manualLpId = null;
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

  function refreshEvModal() {
    // Pass driver query if known so the backend can scope the response
    // to the clicked planet (multi-EV setups). Falls back to whatever
    // the backend returns when no driver filter is honored.
    var url = "/api/ev/status" + (evModalDriver ? "?driver=" + encodeURIComponent(evModalDriver) : "");
    // siteHasPV is a static-per-config check, so we read the most
    // recent payload cached by fetchStatus() instead of issuing a
    // duplicate /api/status fetch on every 5 s modal tick. Falls back
    // to "no PV" until the dashboard's own fetchStatus lands once.
    Promise.all([
      // Owner reads (carry the session cookie) — strict (FIX-B).
      ownerFetch(url).then(function (r) { return r.ok ? r.json() : null; }).catch(function () { return null; }),
      ownerFetch("/api/loadpoints").then(function (r) { return r.ok ? r.json() : null; }).catch(function () { return null; }),
    ]).then(function (results) {
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
        schedSectionEl = null;
        schedLpId = null;
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
        freshStatus = renderEvStatusTable(d);
      } else {
        freshStatus = document.createElement("p");
        freshStatus.style.color = "var(--text-dim)";
        freshStatus.style.fontStyle = "italic";
        freshStatus.style.margin = "0 0 0.6rem 0";
        freshStatus.textContent = "No car connected — schedule below is saved to the loadpoint and applies on next plug-in.";
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
        if (!matched) {
          for (var j = 0; j < lps.loadpoints.length; j++) {
            if (lps.loadpoints[j].plugged_in) { matched = lps.loadpoints[j]; break; }
          }
        }
        if (!matched) {
          matched = lps.loadpoints[0];
        }
      }
      if (matched) {
        // Build schedule exactly once per LP. Polling never rebuilds
        // it — inputs keep their focus, value and caret position. Only
        // a Save / Clear (which sets schedNeedsRebuild) or switching
        // to a different LP (planet) triggers a fresh build.
        var lpChanged = schedSectionEl == null || schedLpId !== matched.id;
        if (lpChanged || schedNeedsRebuild) {
          if (schedSectionEl && schedSectionEl.parentNode === evModalBody) {
            evModalBody.removeChild(schedSectionEl);
          }
          schedSectionEl = buildScheduleControl(matched, siteHasPV(status));
          schedLpId = matched.id;
          schedNeedsRebuild = false;
          evModalBody.appendChild(schedSectionEl);
        } else if (schedSectionEl.parentNode !== evModalBody) {
          // Modal was previously closed: body got wiped but our
          // cached section is still valid — re-attach.
          evModalBody.appendChild(schedSectionEl);
        }
        // Manual amp-slider override — built once per LP (or after a
        // Start/Stop). Mounted above the schedule so "charge now"
        // controls sit closest to the live status.
        var manualChanged = manualSectionEl == null || manualLpId !== matched.id;
        if (manualChanged || manualNeedsRebuild) {
          if (manualSectionEl && manualSectionEl.parentNode === evModalBody) {
            evModalBody.removeChild(manualSectionEl);
          }
          manualSectionEl = buildManualControl(matched);
          manualLpId = matched.id;
          manualNeedsRebuild = false;
          evModalBody.insertBefore(manualSectionEl, schedSectionEl);
        } else if (manualSectionEl.parentNode !== evModalBody) {
          evModalBody.insertBefore(manualSectionEl, schedSectionEl);
        }
      } else {
        if (schedSectionEl && schedSectionEl.parentNode === evModalBody) {
          evModalBody.removeChild(schedSectionEl);
        }
        if (manualSectionEl && manualSectionEl.parentNode === evModalBody) {
          evModalBody.removeChild(manualSectionEl);
        }
        schedSectionEl = null;
        schedLpId = null;
        manualSectionEl = null;
        manualLpId = null;
      }
    }).catch(function () {
      setEvModalMessage("Failed to load EV status");
    });
  }

  // buildManualControl renders the Tesla-style manual override: an amp
  // slider (range = the charger's min/max charge current) plus Start /
  // Stop. Start pins a persistent manual hold at the slider's amps,
  // which overrides surplus_only and the plan (the fuse clamp still
  // applies); Stop clears the hold and drops back to whatever mode the
  // loadpoint is in (PV-surplus-only if that toggle is on). The amperage
  // is sent as watts (power_w = A × phases × voltage); the driver
  // converts back to amps given the wallbox it's talking to.
  function buildManualControl(lp) {
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

    var box = document.createElement("div");
    box.style.marginTop = "0.75rem";
    box.style.paddingTop = "0.6rem";
    box.style.borderTop = "1px solid var(--line)";

    var eyebrow = document.createElement("div");
    eyebrow.textContent = "Manual Charge";
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
    slider.addEventListener("input", renderReadout);

    row.appendChild(slider);
    row.appendChild(readout);
    box.appendChild(row);

    // Status line.
    var status = document.createElement("small");
    status.style.display = "block";
    status.style.color = "var(--text-dim)";
    status.style.marginTop = "0.35rem";
    status.style.minHeight = "1em";
    status.textContent = active
      ? "Manual override active — overriding PV surplus (fuse still limits)."
      : "Stopped = automatic (PV-surplus-only if enabled below). Start overrides it.";
    box.appendChild(status);

    // Start / Stop buttons.
    var btnRow = document.createElement("div");
    btnRow.style.display = "flex";
    btnRow.style.gap = "0.5rem";
    btnRow.style.marginTop = "0.5rem";

    var startBtn = document.createElement("button");
    startBtn.type = "button";
    startBtn.textContent = active ? "Update" : "Start";
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
    stopBtn.textContent = "Stop";
    stopBtn.style.flex = "1";
    stopBtn.style.padding = "0.4rem 0.6rem";
    stopBtn.style.border = "1px solid var(--line)";
    stopBtn.style.borderRadius = "4px";
    stopBtn.style.cursor = "pointer";
    stopBtn.style.background = "transparent";
    stopBtn.style.color = "var(--fg)";
    stopBtn.disabled = !active;
    stopBtn.style.opacity = active ? "1" : "0.5";

    btnRow.appendChild(startBtn);
    btnRow.appendChild(stopBtn);
    box.appendChild(btnRow);

    startBtn.addEventListener("click", function () {
      startBtn.disabled = true;
      status.textContent = "Starting…";
      var a = parseInt(slider.value, 10) || minA;
      // CONTROL write — strict (FIX-B): persistent manual hold (hold_s:0).
      ownerFetch("/api/loadpoints/" + encodeURIComponent(lp.id) + "/manual_hold", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          power_w: aToW(a),
          hold_s: 0,
          phase_mode: phases === 1 ? "1p" : "3p",
        }),
      }).then(function () {
        status.textContent = "Charging at " + a + " A — overriding PV surplus.";
        manualNeedsRebuild = true; // reflect active state on next poll
      }).catch(function () {
        startBtn.disabled = false;
        status.textContent = "Start failed — try again.";
      });
    });

    stopBtn.addEventListener("click", function () {
      stopBtn.disabled = true;
      status.textContent = "Stopping…";
      ownerFetch("/api/loadpoints/" + encodeURIComponent(lp.id) + "/manual_hold", {
        method: "DELETE",
      }).then(function () {
        status.textContent = "Released — back to automatic charging.";
        manualNeedsRebuild = true;
      }).catch(function () {
        stopBtn.disabled = false;
        status.textContent = "Stop failed — try again.";
      });
    });

    // ---- State of charge (manual correction) ----
    // The SoC is inferred from delivered energy when there's no vehicle BMS
    // reading, so it can drift. Let the operator correct it via the existing
    // POST /api/loadpoints/{id}/soc (re-anchors + replans). Only meaningful
    // during an active session, so gate the editor on plugged_in.
    var socBox = document.createElement("div");
    socBox.style.marginTop = "0.75rem";
    socBox.style.paddingTop = "0.6rem";
    socBox.style.borderTop = "1px solid var(--line)";

    var socEyebrow = document.createElement("div");
    socEyebrow.textContent = "State of charge";
    socEyebrow.style.fontFamily = "var(--mono)";
    socEyebrow.style.fontSize = "0.7rem";
    socEyebrow.style.letterSpacing = "0.18em";
    socEyebrow.style.textTransform = "uppercase";
    socEyebrow.style.color = "var(--text-dim)";
    socEyebrow.style.marginBottom = "0.45rem";
    socBox.appendChild(socEyebrow);

    var curSoc = (lp && lp.current_soc_pct != null) ? lp.current_soc_pct : null;
    var socSource = (lp && lp.soc_source) ? lp.soc_source : "";

    if (lp && lp.plugged_in) {
      var socRow = document.createElement("div");
      socRow.style.display = "flex";
      socRow.style.alignItems = "center";
      socRow.style.gap = "0.5rem";

      var socInput = document.createElement("input");
      socInput.type = "number";
      socInput.min = "0"; socInput.max = "100"; socInput.step = "0.1";
      socInput.value = (curSoc != null) ? curSoc.toFixed(1) : "";
      socInput.style.width = "5.5em";
      socInput.style.fontFamily = "var(--mono)";

      var pctLabel = document.createElement("span");
      pctLabel.textContent = "%";
      pctLabel.style.color = "var(--text-dim)";

      var socSetBtn = document.createElement("button");
      socSetBtn.type = "button";
      socSetBtn.textContent = "Set SoC";
      socSetBtn.style.padding = "0.35rem 0.7rem";
      socSetBtn.style.border = "1px solid var(--line)";
      socSetBtn.style.borderRadius = "4px";
      socSetBtn.style.cursor = "pointer";
      socSetBtn.style.background = "transparent";
      socSetBtn.style.color = "var(--fg)";

      socRow.appendChild(socInput);
      socRow.appendChild(pctLabel);
      socRow.appendChild(socSetBtn);
      socBox.appendChild(socRow);

      var socStatus = document.createElement("small");
      socStatus.style.display = "block";
      socStatus.style.color = "var(--text-dim)";
      socStatus.style.marginTop = "0.35rem";
      socStatus.style.minHeight = "1em";
      socStatus.textContent = "Current: " + (curSoc != null ? curSoc.toFixed(1) + "%" : "—") +
        (socSource ? " (" + socSource + ")" : "") + ". Correct it if the car's real SoC differs.";
      socBox.appendChild(socStatus);

      socSetBtn.addEventListener("click", function () {
        var v = parseFloat(socInput.value);
        if (!isFinite(v) || v < 0 || v > 100) { socStatus.textContent = "Enter 0–100%."; return; }
        socSetBtn.disabled = true;
        socStatus.textContent = "Saving…";
        ownerFetch("/api/loadpoints/" + encodeURIComponent(lp.id) + "/soc", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ soc_pct: v }),
        }).then(function (r) { return r.json().then(function (j) { return { ok: r.ok, body: j }; }); })
          .then(function (res) {
            socSetBtn.disabled = false;
            if (res.ok && res.body && res.body.ok) {
              socStatus.textContent = "Saved — SoC set to " + v.toFixed(1) + "%. Replanning.";
              manualNeedsRebuild = true;
            } else {
              socStatus.textContent = (res.body && res.body.error) || "Set failed.";
            }
          }).catch(function (e) { socSetBtn.disabled = false; socStatus.textContent = "Set failed: " + e.message; });
      });
    } else {
      var socMuted = document.createElement("small");
      socMuted.style.display = "block";
      socMuted.style.color = "var(--text-dim)";
      socMuted.textContent = "Current: " + (curSoc != null ? curSoc.toFixed(1) + "%" : "—") +
        (socSource ? " (" + socSource + ")" : "") + " · plug in to set manually.";
      socBox.appendChild(socMuted);
    }

    box.appendChild(socBox);

    return box;
  }

  // buildScheduleControl renders the persistent charging schedule
  // section: target SoC + time (local; converted to UTC for the wire),
  // recurring checkbox, and the bat-SoC surplus-unlock threshold.
  // The backend persists this across restarts (state.config), rolls the
  // deadline forward each day when Recurring is set, and arms the
  // surplus-grab whenever the home battery sits at or above the
  // threshold (with 5 pp release hysteresis).
  function buildScheduleControl(lp, hasPV) {
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

    // Outer wrapper holds two distinct sections:
    //  1. PV mode (surplus-only toggle) — saves immediately on click
    //  2. Schedule (target SoC + deadline + bat-SoC unlock) — Save button
    // They're separated so it's obvious which controls the Save button
    // owns. Earlier we kept the surplus toggle inside the Schedule
    // section and operators couldn't tell whether Save covered it.
    var wrap = document.createElement("div");
    wrap.style.marginTop = "0.75rem";
    wrap.style.paddingTop = "0.6rem";
    wrap.style.borderTop = "1px solid var(--line)";

    // ---- Section 1: PV mode (surplus-only) ----
    // Per-loadpoint hard flag, *independent* of any schedule. When on,
    // dispatch refuses to import grid for this loadpoint regardless of
    // what the MPC plans. Operators can run with this alone (no target,
    // no deadline — just "harvest PV when there's enough") or layer a
    // schedule on top below.
    var soBox = document.createElement("div");
    soBox.style.marginBottom = "0.8rem";
    soBox.style.paddingBottom = "0.7rem";
    soBox.style.borderBottom = "1px solid var(--line)";

    var soEyebrow = document.createElement("div");
    soEyebrow.textContent = "PV Mode";
    soEyebrow.style.fontFamily = "var(--mono)";
    soEyebrow.style.fontSize = "0.7rem";
    soEyebrow.style.letterSpacing = "0.18em";
    soEyebrow.style.textTransform = "uppercase";
    soEyebrow.style.color = "var(--text-dim)";
    soEyebrow.style.marginBottom = "0.45rem";
    soBox.appendChild(soEyebrow);

    var soWrap = document.createElement("label");
    soWrap.style.display = "flex";
    soWrap.style.alignItems = "center";
    soWrap.style.gap = "0.4rem";
    soWrap.style.fontSize = "0.85rem";
    soWrap.style.cursor = "pointer";
    var soCb = document.createElement("input");
    soCb.type = "checkbox";
    soCb.checked = !!(lp && lp.surplus_only);
    soCb.style.accentColor = "var(--accent-e)";
    var soText = document.createElement("span");
    soText.textContent = "Surplus only (PV only — no grid or battery)";
    soWrap.appendChild(soCb);
    soWrap.appendChild(soText);

    var soStatus = document.createElement("small");
    soStatus.style.display = "block";
    soStatus.style.color = "var(--text-dim)";
    soStatus.style.marginTop = "0.25rem";
    soStatus.style.marginLeft = "1.4rem";
    soStatus.style.minHeight = "1em";
    soStatus.textContent = "Saves automatically on click. Independent of the schedule below.";

    soBox.appendChild(soWrap);
    soBox.appendChild(soStatus);
    wrap.appendChild(soBox);

    soCb.addEventListener("change", function () {
      // Surface the surplus-only ↔ schedule interaction immediately on
      // toggle, before the network save returns — operators get instant
      // feedback that flipping surplus on turns the deadline soft.
      if (typeof surplusBestEffortHint !== "undefined" && surplusBestEffortHint) {
        surplusBestEffortHint.style.display = soCb.checked ? "" : "none";
      }
      soCb.disabled = true;
      soStatus.textContent = "Saving…";
      // CONTROL write — strict (FIX-B): loadpoint surplus-only toggle.
      ownerFetch("/api/loadpoints/" + encodeURIComponent(lp.id) + "/target", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ surplus_only: soCb.checked }),
      }).then(function () {
        soCb.disabled = false;
        soStatus.textContent = "Saved. Independent of the schedule below.";
        // Rebuild on next poll so the schedule section reflects any
        // server-side side effects (e.g. soc_source recompute).
        schedNeedsRebuild = true;
      }).catch(function () {
        soCb.disabled = false;
        soStatus.textContent = "Save failed — try again.";
      });
    });

    // ---- Section 2: Schedule ----
    var box = document.createElement("div");

    var eyebrow = document.createElement("div");
    eyebrow.textContent = "Schedule (grid charging)";
    eyebrow.style.fontFamily = "var(--mono)";
    eyebrow.style.fontSize = "0.7rem";
    eyebrow.style.letterSpacing = "0.18em";
    eyebrow.style.textTransform = "uppercase";
    eyebrow.style.color = "var(--text-dim)";
    eyebrow.style.marginBottom = "0.55rem";
    box.appendChild(eyebrow);

    var schedExplainer = document.createElement("div");
    schedExplainer.textContent = "Target SoC by a deadline. The planner uses cheap grid hours to fill the gap PV can't cover.";
    schedExplainer.style.fontSize = "0.72rem";
    schedExplainer.style.color = "var(--text-dim)";
    schedExplainer.style.marginBottom = "0.5rem";
    box.appendChild(schedExplainer);

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
    surplusBestEffortHint.textContent = "Surplus only is on — the deadline becomes best-effort from real PV surplus only. Turn it off to let the planner grid-charge if PV can't cover.";
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
    var surWrap = checkbox(initSurplus && !!hasPV, "Surplus charge from PV");
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
    // Surplus-from-PV only makes sense on sites with a PV driver — the
    // bat-SoC unlock would have no surplus to grab otherwise. Omit the
    // checkbox + threshold entirely on PV-less sites.
    if (hasPV) {
      checkRow.appendChild(surWrap);
    }
    box.appendChild(checkRow);

    var unlockHint = document.createElement("small");
    unlockHint.style.display = "block";
    unlockHint.style.color = "var(--text-dim)";
    unlockHint.style.marginTop = "0.2rem";
    unlockHint.style.marginBottom = "0.3rem";
    unlockHint.textContent = "Always grab PV surplus when home battery ≥ threshold.";

    var thresholdRow = row("Threshold", unlockWrap);

    if (hasPV) {
      box.appendChild(unlockHint);
      box.appendChild(thresholdRow);
    }

    function applySurplusGate() {
      var on = surCb.checked;
      unlockWrap.input.disabled = !on;
      thresholdRow.style.opacity = on ? "1" : "0.4";
      thresholdRow.style.pointerEvents = on ? "auto" : "none";
      unlockHint.style.opacity = on ? "1" : "0.55";
    }
    if (hasPV) {
      applySurplusGate();
      surCb.addEventListener("change", applySurplusGate);
    }

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
    var saveBtn = mkBtn(hasSched ? "Update schedule" : "Set schedule", true);
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

    // Save and Clear both flag schedNeedsRebuild so the next poll
    // rebuilds the section from the new authoritative server state
    // (e.g. so the button label flips from "Save" → "Update" once a
    // schedule exists). Polling never rebuilds otherwise — the cached
    // schedSectionEl stays mounted and inputs keep focus.
    saveBtn.addEventListener("click", function () {
      saveBtn.disabled = true;
      clearBtn.disabled = true;
      status.textContent = "Saving…";
      var localHHMM = timeInp.value || initLocalHHMM;
      var minUTC = localHHMMToUtcMins(localHHMM);
      // Surplus checkbox gates the threshold: when off (or hidden on
      // PV-less sites), the threshold is sent as 0 — the backend
      // interprets 0 as "feature disabled".
      var unlockVal = (hasPV && surCb.checked) ? Number(unlockWrap.input.value) : 0;
      var body = {
        schedule: {
          soc_pct: Number(socWrap.input.value),
          time_of_day_min_utc: minUTC,
          recurring: !!recCb.checked,
          surplus_unlock_bat_soc_pct: unlockVal,
        },
      };
      // CONTROL write — strict (FIX-B): loadpoint schedule save.
      ownerFetch("/api/loadpoints/" + encodeURIComponent(lp.id) + "/target", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      }).then(function (r) {
        if (!r.ok) throw new Error("HTTP " + r.status);
        status.textContent = "Saved.";
        schedNeedsRebuild = true;
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
      // CONTROL write — strict (FIX-B): loadpoint schedule clear.
      ownerFetch("/api/loadpoints/" + encodeURIComponent(lp.id) + "/target", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ schedule: null }),
      }).then(function (r) {
        if (!r.ok) throw new Error("HTTP " + r.status);
        status.textContent = "Cleared.";
        schedNeedsRebuild = true;
        refreshEvModal();
      }).catch(function (e) {
        status.textContent = "Clear failed: " + e.message;
        saveBtn.disabled = false;
      });
    });

    wrap.appendChild(box);
    return wrap;
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
    if (!ownerDataAllowed()) return Promise.resolve(null);
    var points = CHART_POINTS;
    // Owner read (carries the session cookie) — strict (FIX-B).
    return ownerFetch("/api/history?range=" + (range || "5m") + "&points=" + points)
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
    // Owner read (carries the session cookie) — strict (FIX-B).
    ownerFetch("/api/energy/daily?days=" + days)
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
  // Mirrors ftw-history-card.js's dailyFetchCache. Routed through ownerFetch
  // (strict / FIX-B): /api/history is an owner read carrying the session cookie,
  // so it must never traverse the relay in cleartext on the public home route
  // (master used a plain fetch here; the P2P-only route keeps it strict).
  var HISTORY_CACHE_TTL_MS = 15000;
  var historyFetchCache = Object.create(null); // "range|points" -> { at, data?, promise? }
  function fetchHistory(range, points, force) {
    var key = range + "|" + points;
    var now = Date.now();
    var c = historyFetchCache[key];
    if (!force && c && (now - c.at) < HISTORY_CACHE_TTL_MS) {
      if (c.data) return Promise.resolve(c.data);
      if (c.promise) return c.promise;
    }
    var promise = ownerFetch("/api/history?range=" + range + "&points=" + points)
      .then(function (r) { return r.ok ? r.json() : null; })
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
    if (!ownerDataAllowed()) return Promise.resolve();
    lastLiveHistFetch = Date.now();
    return fetchHistory("24h", 288, force) // 5-min cadence
      .then(function (d) {
        if (!d || !d.items) return;
        renderLiveHistory(d.items);
      })
      .catch(function () { /* silent — chart just shows last state */ });
  }

  // ---- P2P transport indicator ----
  // Reflects window.ftwP2P state (direct / relay / connecting). PURELY
  // INFORMATIONAL — it explains how your browser is talking to your Pi; it is
  // NOT a toggle. (The old click-to-toggle "disable direct P2P" made no sense on
  // the P2P-only home route — there's no cleartext relay fallback for owner data,
  // so disabling it just broke the channel. Tap/hover now only reveals the
  // explanation.)
  function setupP2PIndicator() {
    var el = document.getElementById("p2p-status");
    if (!window.ftwP2P) return;
    var label = el && el.querySelector(".p2p-label");
    var titles = {
      direct: "Direct & end-to-end encrypted between your browser and your Pi — the relay sees nothing.",
      relay: "Relayed via a blind TURN server — still end-to-end encrypted; the relay only forwards ciphertext.",
      connecting: "Opening a direct, encrypted channel to your Pi…",
      off: "Direct channel unavailable.",
    };
    var text = { direct: "Direct", relay: "Relayed", connecting: "Connecting…", off: "" };
    if (el) el.style.cursor = "default";

    // The sign-in gate's trust line mirrors the same transport, so a visitor sees
    // the security story BEFORE they're signed in. Direct = end-to-end to the Pi;
    // Relayed = still E2E, relay forwards ciphertext only.
    var trust = document.getElementById("signin-gate-trust");
    var trustText = document.getElementById("signin-gate-trust-text");
    var trustCopy = {
      direct: "Direct & end-to-end encrypted to your Pi. The relay never sees your home.",
      relay: "End-to-end encrypted. The relay only forwards ciphertext — it never sees your home.",
      connecting: "Opening an encrypted channel to your Pi…",
      off: "End-to-end encrypted to your Pi. The relay never sees your home.",
    };

    window.ftwP2P.onState(function (s) {
      if (el) {
        if (s === "off") { el.hidden = true; }
        else {
          el.hidden = false;
          el.classList.remove("p2p-direct", "p2p-relay", "p2p-connecting");
          el.classList.add("p2p-" + s);
          if (label) label.textContent = text[s] || "";
          el.title = titles[s] || "Transport";
        }
      }
      if (trust) {
        trust.classList.remove("is-relay", "is-connecting");
        if (s === "relay") trust.classList.add("is-relay");
        else if (s === "connecting") trust.classList.add("is-connecting");
        if (trustText && trustCopy[s]) trustText.textContent = trustCopy[s];
      }
    });
  }

  // ---- Owner auth: inline sign-in + sign-out (the dashboard IS the door) ----
  // whoami reports whether the viewer is signed in (and whether there's a
  // session to revoke). On the LAN (bypass) there's nothing to sign in/out of.
  // Remotely, when NOT signed in, we reveal an inline passkey sign-in (a discreet
  // banner + a header key) and run the ceremony over the SAME strict P2P channel —
  // no redirect to /owner-access/login.html, which would spawn a fresh channel
  // with no session. All three calls (whoami, login/*, logout) ride ownerFetch
  // (strict / FIX-B) so they never traverse the relay in cleartext.

  // Minimal WebAuthn codec — mirrors owner-access/webauthn.js. next-app.js is a
  // classic script, so it can't `import` the module; these few helpers are tiny.
  function b64urlToBuf(s) {
    if (typeof s !== "string") return s;
    var pad = "=".repeat((4 - (s.length % 4)) % 4);
    var b64 = (s + pad).replace(/-/g, "+").replace(/_/g, "/");
    var bin = atob(b64), buf = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) buf[i] = bin.charCodeAt(i);
    return buf.buffer;
  }
  function bufToB64url(buf) {
    var bytes = new Uint8Array(buf), bin = "";
    for (var i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
    return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  }
  function decodeAssertionOptions(opts) {
    opts = JSON.parse(JSON.stringify(opts));
    if (opts.publicKey) opts = opts.publicKey;
    opts.challenge = b64urlToBuf(opts.challenge);
    if (Array.isArray(opts.allowCredentials)) {
      opts.allowCredentials = opts.allowCredentials.map(function (c) {
        return Object.assign({}, c, { id: b64urlToBuf(c.id) });
      });
    }
    return opts;
  }
  function encodeAssertionResult(cred) {
    return {
      id: cred.id, rawId: bufToB64url(cred.rawId), type: cred.type,
      response: {
        clientDataJSON: bufToB64url(cred.response.clientDataJSON),
        authenticatorData: bufToB64url(cred.response.authenticatorData),
        signature: bufToB64url(cred.response.signature),
        userHandle: cred.response.userHandle ? bufToB64url(cred.response.userHandle) : null,
      },
      clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {},
    };
  }

  // hasDecryptableDirectory reports whether instance-sync has a usable directory
  // cached (the user already authenticated + PRF-decrypted this session, or a
  // migrated single-home record seeded the browser-carried copy). When false the
  // visitor is anonymous and gets the PUBLIC landing — never any instance data.
  function hasDecryptableDirectory() {
    try {
      var sync = window.ftwInstanceSync;
      if (!sync || typeof sync.getCachedInstances !== "function") return false;
      var list = sync.getCachedInstances() || [];
      return list.length >= 1;
    } catch (e) { return false; }
  }

  // The sign-in GATE replaces the dashboard when the viewer isn't signed in on a
  // remote origin — so a logged-out visitor sees a clean "sign in to reach your
  // home", never the empty dashboard chrome (which falsely reads as an
  // unconfigured instance). On the LAN (bypass) the gate never shows. ownerNotAuthed
  // also suppresses the "no devices configured" prompt for logged-out viewers.
  var ownerNotAuthed = false;
  var authGateActive = false;
  function showGate(mode) {
    document.documentElement.classList.add("ftw-gated"); // CSS shows the gate, hides nothing else needed (opaque overlay)
    var g = document.getElementById("signin-gate");
    if (g) g.setAttribute("data-mode", mode || "signin");
    authGateActive = true;
    ownerNotAuthed = (mode !== "connecting");
    try { hideSetupBanner(); } catch (e) {}
    try {
      var noDevices = document.getElementById("no-devices-prompt");
      if (noDevices) noDevices.remove();
    } catch (e) {}
  }
  function hideGate() {
    document.documentElement.classList.remove("ftw-gated");
    authGateActive = false;
    ownerNotAuthed = false;
  }

  // ---- C3: silent device-key sign-in (no passkey) ---------------------------
  // A device that was set up on the LAN (C4) holds a NON-EXTRACTABLE key the Pi
  // pinned. Once the channel is open we can prove that key to the Pi to mint the
  // owner session SILENTLY — no Face ID. Flow:
  //   GET  /api/owner-access/device-challenge -> {challenge, exp_ms}
  //   sign "ftw-device-pop:v1:<site>:<challenge>" with the device key
  //   POST /api/owner-access/device-pop {device_pubkey, challenge, sig}
  // All over ownerFetch (strict P2P), so the proof never crosses the relay in
  // cleartext. Resolves true iff the session was minted. Resolves false (never
  // throws) for "no device key", "no site", or any PoP failure — the caller then
  // falls back to the passkey ceremony.
  var devicePoPBusy = false;
  function waitForDeviceKeyStore(timeoutMs) {
    if (window.ftwDeviceKey && typeof window.ftwDeviceKey.hasDeviceKey === "function") {
      return Promise.resolve(window.ftwDeviceKey);
    }
    return new Promise(function (resolve) {
      var deadline = Date.now() + (timeoutMs || 3000);
      function tick() {
        if (window.ftwDeviceKey && typeof window.ftwDeviceKey.hasDeviceKey === "function") {
          resolve(window.ftwDeviceKey);
          return;
        }
        if (Date.now() >= deadline) { resolve(null); return; }
        setTimeout(tick, 50);
      }
      tick();
    });
  }

  function runDevicePoP() {
    if (devicePoPBusy) return Promise.resolve(false);
    // Need both the device-key store and the pinned site (for the signing string).
    // device-key.js is an ES module loaded before this classic script, but modules
    // are deferred and can still finish after setupAuth's first direct-channel
    // tick. Wait briefly so a reload does not burn the one silent-auth attempt
    // before window.ftwDeviceKey exists.
    if (!window.ftwP2P || typeof window.ftwP2P.site !== "function") {
      return Promise.resolve(false);
    }
    devicePoPBusy = true;
    return waitForOwnerTransport(10000)
      .then(function (transportOk) {
        if (!transportOk) return false;
        return waitForDeviceKeyStore(3000);
      })
      .then(function (store) {
        if (!store) return false;
        return store.hasDeviceKey()
          .then(function (has) {
            if (!has) return false; // never enrolled on this device → passkey path
            return Promise.all([store.getOrCreate(), window.ftwP2P.site()])
              .then(function (pair) {
                var key = pair[0], site = pair[1];
                if (!site) return false;
                return ownerFetch("/api/owner-access/device-challenge", { credentials: "same-origin" })
                  .then(function (r) { return r.ok ? r.json() : null; })
                  .then(function (ch) {
                    if (!ch || !ch.challenge) return false;
                    var msg = "ftw-device-pop:v1:" + site + ":" + ch.challenge;
                    return key.sign(msg).then(function (sig) {
                      return ownerFetch("/api/owner-access/device-pop", {
                        method: "POST",
                        headers: { "Content-Type": "application/json" },
                        body: JSON.stringify({ device_pubkey: key.pubHex, challenge: ch.challenge, sig: sig }),
                        credentials: "same-origin",
                      }).then(function (pop) { return !!(pop && pop.ok); });
                    });
                  });
              });
          });
      })
      .catch(function () { return false; })
      .then(function (ok) { devicePoPBusy = false; return ok; });
  }

  // applySignedIn refreshes the dashboard in place once a session is minted
  // (passkey OR silent device-PoP). Shared so both paths converge.
  function refreshOwnerData(forceHistory) {
    ownerDataPrimed = true;
    fetchStatus();
    fetchLiveHistory(!!forceHistory);
    loadHistory(chartRange);
  }
  function primeOwnerData() {
    if (ownerDataPrimed) return;
    refreshOwnerData(true);
  }
  function applySignedIn() {
    hideGate();
    var signoutBtn = document.getElementById("signout-btn");
    if (signoutBtn && !isLanFallbackOrigin()) signoutBtn.hidden = false;
    refreshOwnerData(true);
    announceOwnerAuthenticated();
  }

  function announceOwnerAuthenticated() {
    window.dispatchEvent(new CustomEvent("ftw-owner-authenticated"));
  }

  // Explicit logout is a local browser intent, not just a server session revoke:
  // if we let C3 run immediately afterwards, the remembered device-key silently
  // mints a fresh session and "logout" appears to do nothing. Keep a local guard
  // until the next successful passkey ceremony; normal reloads still auto-remember
  // as long as the user did not press Sign out.
  var MANUAL_SIGNOUT_KEY = "ftw.owner.manual_signout.v1";
  function manualSignoutActive() {
    try { return localStorage.getItem(MANUAL_SIGNOUT_KEY) === "1"; }
    catch (e) { return false; }
  }
  function markManualSignout() {
    try { localStorage.setItem(MANUAL_SIGNOUT_KEY, "1"); } catch (e) {}
    silentAuthTried = true;
  }
  function clearManualSignout() {
    try { localStorage.removeItem(MANUAL_SIGNOUT_KEY); } catch (e) {}
  }

  // runSignIn: explicit button-driven sign-in over the dashboard's strict P2P
  // transport. The button says "passkey", so it must run the passkey ceremony
  // directly; the SILENT device-key path is only for automatic remembered-device
  // checks in setupAuth(), and is disabled after an explicit logout until passkey
  // auth succeeds.
  var signInBusy = false;
  function runSignIn(opts) {
    opts = opts || {};
    var allowSilent = opts.allowSilent === true && !manualSignoutActive();
    if (signInBusy) return Promise.resolve(false);
    signInBusy = true;
    // say updates whichever message line is on screen. The same ceremony runs
    // from both the returning-visitor gate (#signin-banner-msg) and the public
    // landing (#signin-landing-msg); writing both keeps feedback visible without
    // forking the flow.
    var msgEls = ["signin-banner-msg", "signin-landing-msg"]
      .map(function (id) { return document.getElementById(id); })
      .filter(function (el) { return !!el; });
    function say(t, cls) {
      for (var i = 0; i < msgEls.length; i++) {
        msgEls[i].textContent = t || "";
        msgEls[i].className = "signin-gate-msg" + (cls ? " " + cls : "");
      }
    }
    if (!allowSilent) return runPasskeySignIn(say);

    say("Reaching your home…");
    return runDevicePoP().then(function (silentOk) {
      if (silentOk) {
        signInBusy = false;
        say("Signed in — this device is remembered.", "ok");
        applySignedIn();
        return true;
      }
      return runPasskeySignIn(say);
    });
  }

  // openDirectoryAfterAssertion derives the directory ENCRYPTION key from THIS
  // login assertion's PRF output (prf.js: outputFrom → deriveEncKey), loads +
  // decrypts the relay directory blob (instance-sync.js: loadDirectory), and
  // routes. v1 contract: the directory is a LIST. Exactly 1 entry → auto-open
  // (no picker). >1 → auto-open the FIRST and leave a clearly-marked picker TODO.
  // 0 → "finish setup on your home network" guidance (no error). When the PRF
  // output is absent (Firefox, or a passkey enrolled without prf), we fall back to
  // loadDirectory(W, null, origin) — the browser-carried copy — and surface that
  // encrypted home sync is unavailable here. A PRF/decrypt failure is NEVER fatal:
  // the dashboard still opens; instance-sync's local cache is the source of truth.
  function openDirectoryAfterAssertion(cred, say) {
    var prf = window.ftwPrf, sync = window.ftwInstanceSync;
    if (!prf || typeof prf.outputFrom !== "function" || typeof prf.deriveEncKey !== "function" ||
        !sync || typeof sync.loadDirectory !== "function") {
      return Promise.resolve(); // crypto modules absent → carry-local only
    }
    // W = base64url(assertion.response.userHandle) — the opaque wallet handle the
    // relay keys the encrypted blob on.
    var W = null;
    try {
      var uh = cred && cred.response ? cred.response.userHandle : null;
      if (uh) W = bufToB64url(uh);
    } catch (e) {}
    if (!W) return Promise.resolve(); // no wallet handle → nothing to fetch
    var origin = location.origin;
    function route(dir) {
      var list = (dir && dir.instances) || [];
      if (list.length === 1) {
        // Exactly one home — auto-open. The pin re-resolves from the freshly
        // cached directory entry (p2p.js::pinnedIdentity), so the next owner
        // fetch connects to the right site with no relay round-trip.
        return;
      }
      if (list.length > 1) {
        // TODO(multi-instance picker): v1 has no picker, so auto-open the FIRST
        // entry. instance-sync's getCachedInstances()[0] is what p2p.js pins, so
        // "the first" is already the chosen one. Replace this with a picker UI
        // when multi-home support lands.
        try { console.log("ftw: " + list.length + " homes in directory; auto-opening the first (picker TODO)"); } catch (e) {}
        return;
      }
      // 0 entries — the wallet has no home registered yet. This isn't an error;
      // the user just hasn't finished setup on their home network.
      say("Finish setting up 42W on your home network, then return here.", "ok");
    }
    var prfOut = null;
    try { prfOut = prf.outputFrom(cred); } catch (e) { prfOut = null; }
    if (!prfOut) {
      // No PRF on this browser/passkey — fall back to the browser-carried copy.
      say("Signed in. (Encrypted home sync isn’t available on this browser.)", "ok");
      return sync.loadDirectory(W, null, origin).then(route).catch(function () {});
    }
    return prf.deriveEncKey(prfOut)
      .then(function (encKey) { return sync.loadDirectory(W, encKey, origin); })
      .then(route)
      .catch(function () { /* PRF/decrypt failure → carry-local; never blocks login */ });
  }

  // runPasskeySignIn is the explicit passkey ceremony — the fallback when the
  // silent device path isn't available. Kept as its own function so runSignIn can
  // try silent first without duplicating the WebAuthn flow. The assertion requests
  // the PRF extension (prf.extensionInput): the same passkey tap yields the
  // directory-decryption key, so after finish we load + route the directory.
  function runPasskeySignIn(say) {
    say("Opening secure channel…");
    return waitForOwnerTransport(25000)
      .then(function (ok) {
        if (!ok) {
          say("Still opening the encrypted channel to your Pi. Try again in a moment.");
          scheduleAuthRetry(1000);
          return null;
        }
        say("Waiting for your passkey…");
        return ownerFetch("/api/owner-access/login/start", { method: "POST" });
      })
      .then(function (start) {
        if (!start) return null;
        if (start.status === 404) { say("No passkey here yet — set up this device on your home network first.", "err"); return null; }
        if (!start.ok) { say("Sign-in unavailable (" + start.status + ").", "err"); return null; }
        return start.json();
      })
      .then(function (data) {
        if (!data) return false;
        var getOpts = { publicKey: decodeAssertionOptions(data.options) };
        // Request the PRF extension so the authenticator evaluates the per-wallet
        // secret we HKDF into the directory key (prf.js). Harmless on browsers /
        // authenticators that don't support it — they simply return no prf result
        // and we fall back to the browser-carried directory copy.
        try {
          if (window.ftwPrf && typeof window.ftwPrf.extensionInput === "function") {
            getOpts.publicKey.extensions = Object.assign(
              {}, getOpts.publicKey.extensions, window.ftwPrf.extensionInput());
          }
        } catch (e) {}
        return navigator.credentials.get(getOpts).then(function (cred) {
          if (!cred) { say(""); return false; }
          var finishBody = encodeAssertionResult(cred);
          var devicePubP = Promise.resolve(null);
          if (window.ftwDeviceKey && typeof window.ftwDeviceKey.exportPubHex === "function") {
            devicePubP = window.ftwDeviceKey.exportPubHex().catch(function () { return null; });
          }
          return devicePubP.then(function (devicePubHex) {
            if (devicePubHex) finishBody.device_pubkey = devicePubHex;
            return ownerFetch("/api/owner-access/login/finish?ceremony_token=" + encodeURIComponent(data.ceremony_token), {
              method: "POST",
              headers: { "Content-Type": "application/json" },
              body: JSON.stringify(finishBody),
              credentials: "same-origin",
            });
          }).then(function (finish) {
            if (!finish.ok) { say("Sign-in failed (" + finish.status + ").", "err"); return false; }
            // Session minted. Now derive the directory key from this SAME
            // assertion's PRF output, load + decrypt the relay directory, and
            // route (auto-open on exactly 1). Non-fatal: a PRF/decrypt failure
            // still signs the user in against the browser-carried copy.
            return openDirectoryAfterAssertion(cred, say).then(function () {
              clearManualSignout();
              say("Signed in.", "ok");
              return true;
            });
          });
        });
      })
      .catch(function (e) {
        if (e && e.name === "AbortError") { say(""); return false; }
        say((e && e.message) || "Sign-in error.", "err");
        return false;
      })
      .then(function (ok) {
        signInBusy = false;
        if (ok) applySignedIn();
        return ok;
      });
  }

  // setupAuth wires the gate/signout buttons once, then reflects the current
  // whoami state. Safe to call repeatedly (on load and whenever the P2P channel
  // (re)connects), since whoami needs the channel up to answer on a remote origin.
  var authRetryTimer = null;
  function scheduleAuthRetry(delayMs) {
    if (authRetryTimer || isLanFallbackOrigin()) return;
    authRetryTimer = setTimeout(function () {
      authRetryTimer = null;
      setupAuth();
    }, delayMs || 1500);
  }

  function showWaitingOrLandingGate() {
    if (!hasDecryptableDirectory()) {
      showSignInGate();
      return;
    }
    showGate("connecting");
    scheduleAuthRetry(1500);
  }

  function setupAuth() {
    if (authRetryTimer) {
      clearTimeout(authRetryTimer);
      authRetryTimer = null;
    }
    var signoutBtn = document.getElementById("signout-btn");
    var gateBtn = document.getElementById("signin-gate-btn");
    if (gateBtn && !gateBtn._wired) { gateBtn._wired = true; gateBtn.onclick = function () { runSignIn({ allowSilent: false }); }; }
    // The public-landing button runs the SAME ceremony as the gate button — one
    // runSignIn(), not a fork — so the landing and the returning-visitor card
    // converge on the identical passkey + PRF + directory flow.
    var landingBtn = document.getElementById("signin-landing-btn");
    if (landingBtn && !landingBtn._wired) { landingBtn._wired = true; landingBtn.onclick = function () { runSignIn({ allowSilent: false }); }; }
    if (signoutBtn && !signoutBtn._wired) {
      signoutBtn._wired = true;
      signoutBtn.onclick = function () {
        markManualSignout();
        ownerFetch("/api/owner-access/logout", { method: "POST", credentials: "same-origin" })
          .catch(function () {})
          .then(function () { location.reload(); });
      };
    }
    ownerFetch("/api/owner-access/whoami", { credentials: "same-origin" })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (me) {
        if (me && me.can_sign_out) {            // signed-in remote session
          if (signoutBtn) signoutBtn.hidden = false;
          hideGate();
          primeOwnerData();
          announceOwnerAuthenticated();
          return;
        }
        if (me && me.authenticated) {           // genuine-LAN bypass — full access
          if (signoutBtn) signoutBtn.hidden = true;
          hideGate();
          primeOwnerData();
          announceOwnerAuthenticated();
          return;
        }
        // Not signed in (remote). Try the SILENT device-key path (C3) ONCE before
        // showing the gate — a remembered device signs in with no Face ID. Only
        // when there's no device key / PoP fails do we fall back to the passkey
        // gate. silentAuthTried guards against re-running it on every channel
        // reconnect.
        if (signoutBtn) signoutBtn.hidden = true;
        if (!ownerTransportReady()) {
          showWaitingOrLandingGate();
          return;
        }
        if (manualSignoutActive()) {
          showSignInGate();
          return;
        }
        if (!silentAuthTried) {
          silentAuthTried = true;
          runDevicePoP().then(function (ok) {
            if (ok) { applySignedIn(); return; }
            showSignInGate();
          });
          return;
        }
        showSignInGate();
      })
      .catch(function () {
        if (!ownerTransportReady()) {
          showWaitingOrLandingGate();
          return;
        }
        showSignInGate();
      });
  }

  // silentAuthTried: the C3 silent device-PoP is attempted at most once per page
  // load (it's idempotent but pointless to repeat on every reconnect).
  var silentAuthTried = false;

  // showSignInGate routes the gate. The multi-tenant PUBLIC home route is purely
  // ADDITIVE: a visitor with NO decryptable directory (anonymous, fresh browser)
  // gets the public landing — brand + passkey + Learn more, and NO instance data.
  // Once a directory is cached (this session's PRF decrypt, or a migrated
  // single-home record) the existing single-tenant copy applies: the normal
  // "signin" card, or the "setup" card when p2p.js reports this origin is
  // UNENROLLED (no device key — never set up on the LAN). On the LAN the gate
  // never shows at all, so the existing flow there is untouched.
  function showSignInGate() {
    if (!hasDecryptableDirectory()) { showGate("public-landing"); return; }
    var unEnrolled = false;
    try {
      unEnrolled = !!(window.ftwP2P && window.ftwP2P.isUnenrolled && window.ftwP2P.isUnenrolled());
    } catch (e) { /* default to the normal sign-in gate */ }
    showGate(unEnrolled ? "setup" : "signin");
  }

  // ---- Init ----
  // On a remote/public origin, cover the dashboard with the gate ("connecting…")
  // BEFORE any data fetch renders, so a logged-out visitor never sees the empty
  // dashboard chrome. setupAuth() resolves it to the dashboard (signed in) or the
  // sign-in card (not). On the LAN (bypass) the gate never shows.
  if (typeof isLanFallbackOrigin === "function" && !isLanFallbackOrigin()) showGate("connecting");
  loadHistory(chartRange);
  fetchStatus();
  fetchLiveHistory();
  setupP2PIndicator();
  setupAuth();
  // whoami needs the P2P channel up to answer on a remote origin, so the load-time
  // call may race the connection — re-check whenever the channel (re)connects.
  if (window.ftwP2P && typeof window.ftwP2P.onState === "function") {
    window.ftwP2P.onState(function (s) { if (s === "direct" || s === "relay") setupAuth(); });
  }
  setInterval(fetchStatus, POLL_INTERVAL);
  setInterval(function () { fetchLiveHistory(true); }, 60_000); // 1-min refresh — always fresh
  window.addEventListener("resize", function () {
    // Redraw the 24h chart at the new width. Reuses the cached payload
    // (fetchHistory's short TTL) so a first-load resize storm doesn't
    // fan out into identical /api/history requests — only the data is
    // shared; renderLiveHistory still re-measures the canvas each call.
    fetchLiveHistory();
  });
  requestAnimationFrame(animationFrame);
})();
