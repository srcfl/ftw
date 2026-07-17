// heating.js — heat-pump telemetry card on the main dashboard.
//
// Read-only view over the MyUplink driver's hp_* metrics (compressor power +
// hot-water/indoor/outdoor temperatures). The section stays hidden until a
// driver actually reports hp_power_w, so a site without a heat pump never
// sees an empty card. Discovery runs once on load (one /api/drivers/{name}
// fetch per driver); steady-state polling then only touches the heat-pump
// drivers.
//
// See docs/myuplink-oauth.md. No control here — telemetry only.

(function () {
  'use strict';

  var REFRESH_MS = 30000;
  var timer = null;
  var heatPumpDrivers = null; // cached after discovery: array of driver names
  var lastDiscoverMs = 0;             // Date.now() of the last discovery scan
  var DISCOVER_EVERY_MS = 300000;     // re-scan for newly-added heat pumps (5 min)
  var HISTORY_REFRESH_MS = 300000;    // long TS queries refresh at most every 5 min
  var historyCache = Object.create(null);
  var refreshInFlight = false;

  function ownerFetch(path, opts) {
    return fetch(path, opts);
  }

  // ── Card formatters ──────────────────────────────────────────────
  function fmtPower(v) {
    if (v == null) return '—';
    if (Math.abs(v) >= 1000) return (v / 1000).toFixed(2) + ' kW';
    return Math.round(v) + ' W';
  }
  function fmtTemp(v)   { return v == null ? '—' : v.toFixed(1) + ' °C'; }
  function fmtKW(v)     { return v == null ? '—' : v.toFixed(2) + ' kW'; }
  function fmtHz(v)     { return v == null ? '—' : Math.round(v) + ' Hz'; }
  function fmtAmp(v)    { return v == null ? '—' : v.toFixed(1) + ' A'; }
  function fmtPct(v)    { return v == null ? '—' : Math.round(v) + ' %'; }
  function fmtFlow(v)   { return v == null ? '—' : Math.round(v) + ' m³/h'; }
  function fmtDM(v)     { return v == null ? '—' : Math.round(v) + ' DM'; }
  function fmtKwh(v)    { return v == null ? '—' : Math.round(v).toLocaleString('en-US') + ' kWh'; }
  function fmtRaw(v)    { return v == null ? '—' : String(Math.round(v * 100) / 100); }
  function fmtOffset(v) { return v == null ? '—' : (v > 0 ? '+' : '') + Math.round(v); }
  function fmtOnOff(v)  { return v == null ? '—' : (Math.round(v) ? 'On' : 'Off'); }
  // Compressor priority code → word (0/10 idle, 20 hot water, 30 heating, …).
  var PRIO = { 0: 'Off', 10: 'Off', 20: 'Hot water', 30: 'Heating', 40: 'Pool', 60: 'Cooling' };
  function fmtPrio(v) { if (v == null) return '—'; var n = Math.round(v); return PRIO[n] || ('Mode ' + n); }
  function fmtVent(v) { if (v == null) return '—'; var n = Math.round(v); return n === 0 ? 'Normal' : ('Mode ' + n); }

  // ── Card layout: grouped tiles. Each item = { key (hp_* metric), label,
  // optional sensor designation (BT21 …), formatter, info (hover tooltip on
  // the "?" help icon) }. Render order = array order. The detail pop-up still
  // lists ALL ~960 signals; this is the curated at-a-glance set.
  var GROUPS = [
    { title: 'Power & electrical', items: [
      { key: 'hp_energy_log_current_power_consumption', label: 'Total power now', fmt: fmtKW, info: "The whole heat pump's instantaneous electrical draw right now — compressor + fan + circulation pumps + electronics." },
      { key: 'hp_power_w', label: 'Compressor', fmt: fmtPower, info: 'Power to the compressor only. 0 W when the compressor is idle — the pump still draws power (see Total power now).' },
      { key: 'hp_power_internal_additional_heat', label: 'Internal add. heat', fmt: fmtKW, info: 'Power to the internal immersion heater (supplementary heat). 0 when only the compressor runs.' },
      { key: 'hp_compressor_frequency_current', label: 'Compr. freq.', fmt: fmtHz, info: 'Compressor speed/frequency right now. 0 Hz = compressor idle.' },
      { key: 'hp_current_be1', label: 'Current', sensor: 'BE1', fmt: fmtAmp, info: 'Measured current on phase 1 (current transformer BE1). Used by the load monitor / fuse protection.' },
      { key: 'hp_current_be2', label: 'Current', sensor: 'BE2', fmt: fmtAmp, info: 'Measured current on phase 2 (BE2).' },
      { key: 'hp_current_be3', label: 'Current', sensor: 'BE3', fmt: fmtAmp, info: 'Measured current on phase 3 (BE3).' },
      { key: 'hp_power_limitation_activation', label: 'Power limit', fmt: fmtOnOff, info: 'Whether the built-in power limitation (load monitor) is actively throttling to stay under the main fuse. Off = not limiting. Throttling only reduces output — it never damages the pump.' },
      { key: 'hp_fuse', label: 'Fuse', sensor: null, fmt: fmtAmp, info: 'Main fuse size the pump is configured for. It limits compressor + immersion heater to stay under this.' },
      { key: 'hp_max_internal_additional_heat', label: 'Max add. heat', fmt: fmtKW, info: 'Maximum permitted internal immersion-heater power. Lower this to cap the supplementary heat draw.' },
    ] },
    { title: 'Temperatures', items: [
      { key: 'hp_hw_top_temp_c', label: 'Hot water top', sensor: 'BT7', fmt: fmtTemp, info: 'Temperature at the top of the hot-water tank — what you get first from the tap.' },
      { key: 'hp_hot_water_charging_bt6', label: 'HW charging', sensor: 'BT6', fmt: fmtTemp, info: 'Controlling sensor for hot-water charging — decides when the tank is fully charged.' },
      { key: 'hp_hot_water_start_bt5', label: 'HW start', sensor: 'BT5', fmt: fmtTemp, info: 'Hot-water start sensor — triggers a new charge when it drops below the start value.' },
      { key: 'hp_supply_line_bt2', label: 'Supply', sensor: 'BT2', fmt: fmtTemp, info: 'Temperature of the water going OUT to the heating system (supply line).' },
      { key: 'hp_return_line_bt3', label: 'Return', sensor: 'BT3', fmt: fmtTemp, info: 'Temperature of the water coming BACK from the heating system (return line).' },
      { key: 'hp_calculated_supply_climate_system_1', label: 'Calc. supply', fmt: fmtTemp, info: 'Calculated (target) supply temperature the control derives from the heating curve.' },
      { key: 'hp_fr_nluft_bt20', label: 'Extract air', sensor: 'BT20', fmt: fmtTemp, info: "Ventilation air drawn from the rooms — the heat pump's heat source (into the evaporator)." },
      { key: 'hp_avluft_bt21', label: 'Exhaust air', sensor: 'BT21', fmt: fmtTemp, info: 'Air after heat recovery, on its way out of the house. Extract − exhaust = recovered heat.' },
      { key: 'hp_outdoor_temp_c', label: 'Outdoor', sensor: 'BT1', fmt: fmtTemp, info: 'Outdoor temperature (BT1) — drives the heating curve.' },
    ] },
    { title: 'Ventilation', items: [
      { key: 'hp_ventilation_mode', label: 'Vent. mode', fmt: fmtVent, info: 'Active ventilation mode. 0 = normal.' },
      { key: 'hp_exhaust_air_fan_speed_gq2', label: 'Fan speed', sensor: 'GQ2', fmt: fmtPct, info: 'Exhaust-air fan speed right now. Normal = 54 %; lower (e.g. 30 %) = reduced ventilation (speed 2).' },
      { key: 'hp_real_air_flow', label: 'Air flow', fmt: fmtFlow, info: 'Measured air flow through the unit.' },
    ] },
    { title: 'Operation', items: [
      { key: 'hp_priority', label: 'Priority', fmt: fmtPrio, info: 'What the compressor is prioritising right now: Off / Hot water / Heating / Pool / Cooling.' },
      { key: 'hp_degree_minutes', label: 'Degree minutes', fmt: fmtDM, info: 'Heating deficit integrated over time. When it reaches the start threshold the compressor starts. 0 = no deficit (heating off in summer).' },
      { key: 'hp_heating_medium_pump_speed_gp1', label: 'Circ. pump', sensor: 'GP1', fmt: fmtPct, info: 'Heating circulation pump (GP1) speed.' },
      { key: 'hp_heating_curve_climate_system_1', label: 'Heating curve', fmt: fmtRaw, info: 'Configured heating curve (slope) for climate system 1. Higher = warmer supply when it is cold outside.' },
      { key: 'hp_heating_offset_climate_system_1', label: 'Curve offset', fmt: fmtOffset, info: 'Parallel offset of the heating curve — warmer (+) or cooler (−) overall.' },
    ] },
    { title: 'Energy (lifetime)', items: [
      { key: 'hp_energy_consumed_kwh', label: 'Total consumed', fmt: fmtKwh, info: 'Total electricity supplied to the heat pump since installation (lifetime counter).' },
      { key: 'hp_energy_produced_kwh', label: 'Total produced', fmt: fmtKwh, info: 'Total heat energy delivered since installation. Produced ÷ consumed ≈ heat factor (SCOP).' },
      { key: 'hp_heating_compressor_only', label: 'Heating (compr.)', fmt: fmtKwh, info: 'Heat delivered to heating, from the compressor only (excl. immersion heater).' },
      { key: 'hp_hot_water_compressor_only', label: 'Hot water (compr.)', fmt: fmtKwh, info: 'Heat delivered to hot water, from the compressor only (excl. immersion heater).' },
    ] },
  ];

  function injectStyles() {
    if (document.getElementById('ftw-heating-styles')) return;
    var css = [
      '#heating-grid{display:flex;flex-direction:column;gap:18px}',
      '.ftw-hp{display:flex;flex-direction:column;gap:16px}',
      '.ftw-hp-clickable{cursor:pointer;border-radius:8px;margin:-6px;padding:6px;transition:background 0.12s}',
      '.ftw-hp-clickable:hover,.ftw-hp-clickable:focus{background:var(--bg-hover,rgba(127,127,127,0.06));outline:none}',
      '.ftw-hp-head{display:flex;align-items:baseline;justify-content:space-between;gap:10px}',
      '.ftw-hp-more{font-family:var(--mono);font-size:0.66rem;letter-spacing:0.1em;text-transform:uppercase;color:var(--accent-e)}',
      '.ftw-hp-name{font-family:var(--mono);font-size:0.72rem;letter-spacing:0.18em;text-transform:uppercase;color:var(--fg-muted)}',
      '.ftw-hp-group{display:flex;flex-direction:column;gap:9px;background:var(--ink-sunken);border:1px solid var(--line);border-radius:10px;padding:12px 14px}',
      '.ftw-hp-group-title{font-family:var(--mono);font-size:0.62rem;letter-spacing:0.16em;text-transform:uppercase;color:var(--accent-e)}',
      '.ftw-hp-tiles{display:grid;grid-template-columns:repeat(auto-fit,minmax(120px,1fr));gap:10px 18px}',
      '.ftw-hp-tile{display:flex;flex-direction:column;gap:3px}',
      '.ftw-hp-tile-label{font-family:var(--mono);font-size:0.64rem;letter-spacing:0.08em;text-transform:uppercase;color:var(--fg-muted);display:flex;align-items:baseline;gap:3px;flex-wrap:wrap}',
      '.ftw-hp-sensor{color:var(--fg-muted);opacity:0.6}',
      '.ftw-hp-i{display:inline-flex;align-items:center;justify-content:center;width:13px;height:13px;border:1px solid var(--line);border-radius:50%;color:var(--fg-dim);font-family:var(--sans);font-size:9px;font-weight:600;line-height:1;cursor:help;transition:color 0.18s,border-color 0.18s}',
      '.ftw-hp-i:hover{color:var(--accent-e);border-color:var(--accent-e)}',
      '.ftw-hp-tile-val{font-family:var(--mono);font-size:1.02rem;font-variant-numeric:tabular-nums;color:var(--fg)}',
      '.ftw-hp-spark{display:flex;flex-direction:column;gap:4px}',
      '.ftw-hp-spark-label{font-family:var(--mono);font-size:0.66rem;letter-spacing:0.12em;text-transform:uppercase;color:var(--fg-muted)}',
      '.ftw-hp-spark svg{width:100%;height:48px;display:block}',
      '.ftw-hp-chartrow{display:flex;gap:6px;align-items:stretch}',
      '.ftw-hp-tchart{flex:1;min-width:0;height:150px;display:block}',
      '.ftw-hp-pchart{flex:1;min-width:0;height:90px;display:block}',
      '.ftw-hp-yax{display:flex;flex-direction:column;justify-content:space-between;text-align:right;min-width:26px;padding:6px 0;font-family:var(--mono);font-variant-numeric:tabular-nums;font-size:0.6rem;color:var(--fg-muted)}',
      '.ftw-hp-xax{display:flex;justify-content:space-between;margin:3px 0 0 32px;font-family:var(--mono);font-size:0.6rem;color:var(--fg-muted)}',
      '.ftw-hp-legend{display:flex;gap:14px;flex-wrap:wrap}',
      '.ftw-hp-chartsub{font-family:var(--sans);font-size:0.72rem;color:var(--fg-muted);margin:-2px 0 6px}',
      '.ftw-hp-asof{font-family:var(--mono);font-size:0.62rem;letter-spacing:0.04em;color:var(--fg-muted);margin:-4px 0 10px}',
      '.ftw-hp-leg{font-family:var(--mono);font-size:0.62rem;text-transform:uppercase;letter-spacing:0.06em;color:var(--fg-muted);display:inline-flex;align-items:center;gap:4px}',
      '.ftw-hp-leg-dot{width:8px;height:8px;border-radius:2px;display:inline-block}',
      '.ftw-hp-erow{display:grid;grid-template-columns:1fr auto auto;gap:7px 18px;align-items:baseline}',
      '.ftw-hp-ehead{font-size:0.58rem;letter-spacing:0.12em;text-transform:uppercase;color:var(--fg-muted);font-family:var(--mono)}',
      '.ftw-hp-elabel{font-family:var(--mono);font-size:0.66rem;text-transform:uppercase;letter-spacing:0.06em;color:var(--fg-muted)}',
      '.ftw-hp-eval{font-family:var(--mono);font-variant-numeric:tabular-nums;font-size:0.92rem;color:var(--fg);text-align:right;min-width:72px}',
      '.ftw-hp-acc{color:var(--fg-muted);opacity:0.7;font-size:0.74rem}',
      '.ftw-hp-empty{font-family:var(--mono);font-size:0.8rem;color:var(--fg-muted)}',
    ].join('');
    var el = document.createElement('style');
    el.id = 'ftw-heating-styles';
    el.textContent = css;
    document.head.appendChild(el);
  }

  // Build a sparkline <svg> from [{ts, v}] points. Pure SVG so it themes via
  // CSS vars (stroke = accent). Returns '' when there's nothing to draw.
  function sparkline(points) {
    if (!points || points.length < 2) return '';
    var w = 240, h = 48, pad = 3;
    var vals = points.map(function (p) { return p.v; });
    var min = Math.min.apply(null, vals);
    var max = Math.max.apply(null, vals);
    var span = max - min || 1;
    var n = points.length;
    var coords = points.map(function (p, i) {
      var x = pad + (i / (n - 1)) * (w - 2 * pad);
      var y = h - pad - ((p.v - min) / span) * (h - 2 * pad);
      return x.toFixed(1) + ',' + y.toFixed(1);
    });
    return '<svg viewBox="0 0 ' + w + ' ' + h + '" preserveAspectRatio="none" aria-hidden="true">' +
      '<polyline fill="none" stroke="var(--accent-e)" stroke-width="1.5" ' +
      'stroke-linejoin="round" stroke-linecap="round" points="' + coords.join(' ') + '"></polyline>' +
      '</svg>';
  }

  function metricMap(metrics) {
    var m = {};
    (metrics || []).forEach(function (s) { if (s && s.name) m[s.name] = s.value; });
    return m;
  }

  function isHeatPump(detail) {
    var m = metricMap(detail && detail.metrics);
    return Object.prototype.hasOwnProperty.call(m, 'hp_power_w');
  }

  function fetchJSON(path) {
    return ownerFetch(path).then(function (r) { return r.json(); }).catch(function () { return null; });
  }

  // One-time discovery: list drivers, fetch each detail, keep the ones that
  // report hp_power_w.
  function discover() {
    return fetchJSON('/api/drivers').then(function (health) {
      if (!health || typeof health !== 'object') return [];
      var names = Object.keys(health);
      return Promise.all(names.map(function (n) {
        return fetchJSON('/api/drivers/' + encodeURIComponent(n)).then(function (d) {
          return d && isHeatPump(d) ? n : null;
        });
      })).then(function (found) {
        return found.filter(Boolean);
      });
    });
  }

  // ── Month temperature chart (outdoor / supply / return) ──────────
  // Multi-line SVG; line hues are fixed (read on both themes), axis chrome
  // uses theme tokens. Spans whatever history exists (fills toward a month).
  var TCHART = [
    { key: 'outdoor', label: 'Outdoor (BT1)', color: '#38bdf8' },
    { key: 'supply',  label: 'Supply (BT2)',  color: '#ef4444' },
    { key: 'ret',     label: 'Return (BT3)',  color: '#22c55e' },
    { key: 'extract', label: 'Extract air (BT20)', color: '#a78bfa' },
  ];
  function tempChartBlock(temps) {
    if (!temps) return '';
    var lines = TCHART.map(function (s) { return { color: s.color, points: temps[s.key] || [] }; });
    var all = [];
    lines.forEach(function (l) { l.points.forEach(function (p) { if (p && p.v != null) all.push(p); }); });
    if (all.length < 2) return '';
    var w = 600, h = 150, padR = 4, padL = 2, padT = 6, padB = 6;
    var ts = all.map(function (p) { return p.ts; });
    var vs = all.map(function (p) { return p.v; });
    var t0 = Math.min.apply(null, ts), t1 = Math.max.apply(null, ts), tspan = (t1 - t0) || 1;
    var vMin = Math.min.apply(null, vs), vMax = Math.max.apply(null, vs);
    var vp = (vMax - vMin) * 0.1 || 1; vMin -= vp; vMax += vp;
    var vspan = (vMax - vMin) || 1;
    function X(t) { return (padL + (t - t0) / tspan * (w - padL - padR)).toFixed(1); }
    function Y(v) { return (padT + (1 - (v - vMin) / vspan) * (h - padT - padB)).toFixed(1); }
    // Gridlines only — axis labels are HTML (below/left) so they don't distort
    // under preserveAspectRatio="none".
    var grid = [vMin, (vMin + vMax) / 2, vMax].map(function (v) {
      return '<line x1="' + padL + '" y1="' + Y(v) + '" x2="' + (w - padR) + '" y2="' + Y(v) + '" stroke="var(--line)" stroke-width="0.5"/>';
    }).join('');
    var paths = lines.map(function (l) {
      var pp = l.points.filter(function (p) { return p && p.v != null; });
      if (pp.length < 2) return '';
      var d = pp.map(function (p, i) { return (i ? 'L' : 'M') + X(p.ts) + ',' + Y(p.v); }).join(' ');
      return '<path d="' + d + '" fill="none" stroke="' + l.color + '" stroke-width="1.4" stroke-linejoin="round"/>';
    }).join('');
    var legend = TCHART.map(function (s) {
      return '<span class="ftw-hp-leg"><span class="ftw-hp-leg-dot" style="background:' + s.color + '"></span>' + s.label + '</span>';
    }).join('');
    var yax = [vMax, (vMin + vMax) / 2, vMin].map(function (v) {
      return '<span>' + Math.round(v) + '°</span>';
    }).join('');
    var fmtDate = function (t) { return new Date(t).toLocaleDateString('en-US', { month: 'short', day: 'numeric' }); };
    var xticks = 4, xax = '';
    for (var xi = 0; xi < xticks; xi++) { xax += '<span>' + escapeHtml(fmtDate(t0 + tspan * xi / (xticks - 1))) + '</span>'; }
    return '<div class="ftw-hp-group"><div class="ftw-hp-group-title">Temperatures (°C)</div>' +
      '<div class="ftw-hp-chartsub">Supply / return = the heating loop (in-floor / radiators)</div>' +
      '<div class="ftw-hp-legend">' + legend + '</div>' +
      '<div class="ftw-hp-chartrow"><div class="ftw-hp-yax">' + yax + '</div>' +
      '<svg viewBox="0 0 ' + w + ' ' + h + '" preserveAspectRatio="none" class="ftw-hp-tchart" aria-hidden="true">' + grid + paths + '</svg></div>' +
      '<div class="ftw-hp-xax">' + xax + '</div>' +
      '</div>';
  }

  // Power over 24h — Total drawn / Compressor / Internal add. heat, all in kW.
  // Separate from sparkline() so the ~937 tiny detail sparklines stay axis-free.
  var PCHART = [
    { key: 'total',      label: 'Total drawn',        color: 'var(--accent-e)', scale: 1 },
    { key: 'compressor', label: 'Compressor',         color: '#38bdf8', scale: 0.001 },
    { key: 'internal',   label: 'Internal add. heat', color: '#f472b6', scale: 1 },
  ];
  function powerChartBlock(power) {
    if (!power) return '';
    var lines = PCHART.map(function (s) {
      return { color: s.color, points: (power[s.key] || []).map(function (p) {
        return { ts: p.ts, v: p.v == null ? null : p.v * s.scale };
      }) };
    });
    var all = [];
    lines.forEach(function (l) { l.points.forEach(function (p) { if (p && p.v != null) all.push(p); }); });
    if (all.length < 2) return '';
    var w = 600, h = 90, padR = 4, padL = 2, padT = 6, padB = 6;
    var ts = all.map(function (p) { return p.ts; });
    var t0 = Math.min.apply(null, ts), t1 = Math.max.apply(null, ts), tspan = (t1 - t0) || 1;
    var vMax = Math.max(Math.max.apply(null, all.map(function (p) { return p.v; })) || 1, 0.1);
    var vspan = vMax || 1;
    function X(t) { return (padL + (t - t0) / tspan * (w - padL - padR)).toFixed(1); }
    function Y(v) { return (padT + (1 - v / vspan) * (h - padT - padB)).toFixed(1); }
    var grid = [0, vMax / 2, vMax].map(function (v) {
      return '<line x1="' + padL + '" y1="' + Y(v) + '" x2="' + (w - padR) + '" y2="' + Y(v) + '" stroke="var(--line)" stroke-width="0.5"/>';
    }).join('');
    var paths = lines.map(function (l) {
      var pp = l.points.filter(function (p) { return p && p.v != null; });
      if (pp.length < 2) return '';
      var d = pp.map(function (p, i) { return (i ? 'L' : 'M') + X(p.ts) + ',' + Y(p.v); }).join(' ');
      return '<path d="' + d + '" fill="none" stroke="' + l.color + '" stroke-width="1.4" stroke-linejoin="round"/>';
    }).join('');
    var legend = PCHART.map(function (s) {
      return '<span class="ftw-hp-leg"><span class="ftw-hp-leg-dot" style="background:' + s.color + '"></span>' + s.label + '</span>';
    }).join('');
    var yax = [vMax, vMax / 2, 0].map(function (v) { return '<span>' + v.toFixed(1) + '</span>'; }).join('');
    var fmtTime = function (t) { return new Date(t).toLocaleTimeString('en-US', { hour: '2-digit', minute: '2-digit', hour12: false }); };
    var xticks = 4, xax = '';
    for (var xi = 0; xi < xticks; xi++) { xax += '<span>' + escapeHtml(fmtTime(t0 + tspan * xi / (xticks - 1))) + '</span>'; }
    return '<div class="ftw-hp-group"><div class="ftw-hp-group-title">Power (kW) · 24h</div>' +
      '<div class="ftw-hp-legend">' + legend + '</div>' +
      '<div class="ftw-hp-chartrow"><div class="ftw-hp-yax">' + yax + '</div>' +
      '<svg viewBox="0 0 ' + w + ' ' + h + '" preserveAspectRatio="none" class="ftw-hp-pchart" aria-hidden="true">' + grid + paths + '</svg></div>' +
      '<div class="ftw-hp-xax">' + xax + '</div>' +
      '</div>';
  }

  // ── Energy per calendar period, from the lifetime kWh counters ───
  // delta = current − value at the period boundary; null when the logged
  // history doesn't reach back that far (shown as "accumulating").
  function energyDeltas(points) {
    if (!points || points.length < 2) return {};
    var last = points[points.length - 1].v;
    var earliest = points[0].ts;
    var now = new Date();
    var startToday = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime();
    var dow = (now.getDay() + 6) % 7; // Monday = 0
    var startWeek = startToday - dow * 86400000;
    var startMonth = new Date(now.getFullYear(), now.getMonth(), 1).getTime();
    var startYear = new Date(now.getFullYear(), 0, 1).getTime();
    function delta(b) {
      if (earliest > b) return null;
      var base = points[0].v;
      for (var i = 0; i < points.length; i++) { if (points[i].ts <= b) base = points[i].v; else break; }
      return Math.max(0, last - base);
    }
    return { today: delta(startToday), week: delta(startWeek), month: delta(startMonth), year: delta(startYear) };
  }
  function energyPeriodsBlock(energy) {
    if (!energy || (energy.consumed || []).length < 2) return '';
    var c = energyDeltas(energy.consumed), p = energyDeltas(energy.produced);
    function cell(v) { return v == null ? '<span class="ftw-hp-acc">accumulating</span>' : (Math.round(v * 10) / 10).toLocaleString('en-US') + ' kWh'; }
    var rows = [
      { k: 'today', label: 'Today' },
      { k: 'week', label: 'This week' },
      { k: 'month', label: 'This month' },
      { k: 'year', label: 'This year' },
    ].map(function (r) {
      return '<div class="ftw-hp-erow"><span class="ftw-hp-elabel">' + r.label + '</span>' +
        '<span class="ftw-hp-eval">' + cell(c[r.k]) + '</span>' +
        '<span class="ftw-hp-eval">' + cell(p[r.k]) + '</span></div>';
    }).join('');
    return '<div class="ftw-hp-group"><div class="ftw-hp-group-title">Energy per period</div>' +
      '<div class="ftw-hp-erow ftw-hp-ehead"><span class="ftw-hp-elabel"></span><span class="ftw-hp-eval">Consumed</span><span class="ftw-hp-eval">Produced</span></div>' +
      rows + '</div>';
  }

  function tileHtml(def, m) {
    var has = Object.prototype.hasOwnProperty.call(m, def.key);
    var sensor = def.sensor ? ' <span class="ftw-hp-sensor">(' + escapeHtml(def.sensor) + ')</span>' : '';
    var info = def.info ? ' <span class="ftw-hp-i" role="img" aria-label="info" title="' + escapeHtml(def.info) + '">?</span>' : '';
    return '<div class="ftw-hp-tile">' +
      '<span class="ftw-hp-tile-label">' + escapeHtml(def.label) + sensor + info + '</span>' +
      '<span class="ftw-hp-tile-val">' + (has ? def.fmt(m[def.key]) : '—') + '</span>' +
      '</div>';
  }

  function renderPump(name, detail, temps, energy, power) {
    var m = metricMap(detail && detail.metrics);
    var groups = GROUPS.map(function (g) {
      var tiles = g.items.map(function (def) { return tileHtml(def, m); }).join('');
      return '<div class="ftw-hp-group"><div class="ftw-hp-group-title">' + escapeHtml(g.title) + '</div>' +
        '<div class="ftw-hp-tiles">' + tiles + '</div></div>';
    }).join('');
    var powerBlock = powerChartBlock(power);
    // Freshness: newest updated_at across the reported metrics (one poll).
    var latest = 0;
    ((detail && detail.metrics) || []).forEach(function (x) {
      var t = x.updated_at ? Date.parse(x.updated_at) : 0; if (t > latest) latest = t;
    });
    var asof = latest ? '<div class="ftw-hp-asof">data as of ' +
      escapeHtml(new Date(latest).toLocaleString('en-US', { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false })) + '</div>' : '';
    // The whole card is a button into the detail view (all signals + register).
    return '<div class="ftw-hp ftw-hp-clickable" data-hp-driver="' + escapeHtml(name) + '" role="button" tabindex="0" title="View all signals">' +
      '<div class="ftw-hp-head"><span class="ftw-hp-name">' + escapeHtml(name) + '</span>' +
      '<span class="ftw-hp-more">All signals →</span></div>' +
      asof +
      groups +
      energyPeriodsBlock(energy) +
      tempChartBlock(temps) +
      powerBlock +
      '</div>';
  }

  function escapeHtml(s) {
    return String(s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  // The live values are cheap and refresh every 30 s. Month/year history is
  // comparatively expensive (SQLite/Parquet range scans, possibly over the
  // dashboard), so cache those series for five minutes per heat pump.
  function fetchPumpHistory(name) {
    var cached = historyCache[name];
    var now = Date.now();
    if (cached && now - cached.at < HISTORY_REFRESH_MS) return Promise.resolve(cached.data);
    var ser = function (metric, range, pts) {
      return fetchJSON('/api/series?driver=' + encodeURIComponent(name) + '&metric=' + metric + '&range=' + range + '&points=' + pts);
    };
    return Promise.all([
      ser('hp_power_w', '24h', 200),
      ser('hp_outdoor_temp_c', '30d', 400),
      ser('hp_supply_line_bt2', '30d', 400),
      ser('hp_return_line_bt3', '30d', 400),
      ser('hp_energy_consumed_kwh', '366d', 800),
      ser('hp_energy_produced_kwh', '366d', 800),
      ser('hp_fr_nluft_bt20', '30d', 400),
      ser('hp_energy_log_current_power_consumption', '24h', 200),
      ser('hp_power_internal_additional_heat', '24h', 200),
    ]).then(function (parts) {
      var pp = function (r) { return (r && r.points) || []; };
      var data = {
        temps: { outdoor: pp(parts[1]), supply: pp(parts[2]), ret: pp(parts[3]), extract: pp(parts[6]) },
        energy: { consumed: pp(parts[4]), produced: pp(parts[5]) },
        power: { compressor: pp(parts[0]), total: pp(parts[7]), internal: pp(parts[8]) },
      };
      historyCache[name] = { at: Date.now(), data: data };
      return data;
    });
  }

  function refresh() {
    var section = document.getElementById('heating-section');
    var grid = document.getElementById('heating-grid');
    if (!section || !grid) return;
    if (refreshInFlight) return;
    refreshInFlight = true;

    // Re-run discovery on first call, then periodically — so a heat-pump
    // driver added while the dashboard is open shows up without a manual
    // reload. (The old code cached forever; an empty result is also truthy,
    // so a site that discovered before its pump reported hp_power_w stayed
    // blank.) Steady-state stays cheap: between scans we only touch the
    // already-known heat-pump drivers.
    var nowMs = Date.now();
    var rediscover = heatPumpDrivers === null || (nowMs - lastDiscoverMs) >= DISCOVER_EVERY_MS;
    var ready = rediscover
      ? discover().then(function (names) { heatPumpDrivers = names; lastDiscoverMs = nowMs; return names; })
      : Promise.resolve(heatPumpDrivers);

    ready.then(function (names) {
      if (!names || names.length === 0) {
        section.hidden = true;
        return;
      }
      // Refresh live detail every cycle; reuse bounded-age history.
      return Promise.all(names.map(function (n) {
        return Promise.all([
          fetchJSON('/api/drivers/' + encodeURIComponent(n)),
          fetchPumpHistory(n),
        ]).then(function (parts) {
          var h = parts[1] || {};
          return { name: n, detail: parts[0], temps: h.temps, energy: h.energy, power: h.power };
        });
      })).then(function (pumps) {
        var live = pumps.filter(function (p) { return p.detail && isHeatPump(p.detail); });
        if (live.length === 0) { section.hidden = true; return; }
        injectStyles();
        section.hidden = false;
        grid.innerHTML = live.map(function (p) {
          return renderPump(p.name, p.detail, p.temps, p.energy, p.power);
        }).join('');
      });
    }).then(function () {
      refreshInFlight = false;
    }, function () {
      refreshInFlight = false;
    });
  }

  // ---- Detail drill-in: all points grouped by unit ----

  // Ordered unit groups. First matching predicate wins; anything unmatched
  // falls into "State / other".
  var UNIT_GROUPS = [
    { title: 'Temperatures', match: function (u) { return u === '°C' || u === '°F' || u === 'K'; } },
    { title: 'Power & energy', match: function (u) { return u === 'W' || u === 'kW' || u === 'Wh' || u === 'kWh'; } },
    { title: 'Frequency', match: function (u) { return u === 'Hz'; } },
    { title: 'Percent', match: function (u) { return u === '%'; } },
    { title: 'Flow & pressure', match: function (u) { return u === 'l/m' || u === 'l/min' || u === 'bar' || u === 'kPa'; } },
    { title: 'Electrical', match: function (u) { return u === 'A' || u === 'V'; } },
    { title: 'Counters & degree-minutes', match: function (u) { return u === 'GM' || u === 'DM' || u === 'h' || u === 'min' || u === 's' || /count/i.test(u); } },
  ];

  function groupForUnit(unit) {
    for (var i = 0; i < UNIT_GROUPS.length; i++) {
      if (UNIT_GROUPS[i].match(unit || '')) return UNIT_GROUPS[i].title;
    }
    return 'State / other';
  }

  // hp_supply_line_bt2 → "Supply line bt2"
  function prettyLabel(name) {
    var s = String(name).replace(/^hp_/, '').replace(/_/g, ' ').trim();
    return s.charAt(0).toUpperCase() + s.slice(1);
  }

  function fmtValue(v, unit) {
    if (v == null) return '—';
    var n = Math.abs(v) >= 100 ? Math.round(v) : Math.round(v * 100) / 100;
    return n + (unit ? ' ' + unit : '');
  }

  function injectDetailStyles() {
    if (document.getElementById('ftw-heating-detail-styles')) return;
    var css = [
      '.ftw-hpd-backdrop{position:fixed;inset:0;background:rgba(0,0,0,0.55);display:flex;align-items:flex-start;justify-content:center;z-index:1000;overflow:auto;padding:5vh 16px}',
      '.ftw-hpd{background:var(--ink-raised);border:1px solid var(--line);border-radius:10px;max-width:760px;width:100%;padding:20px 22px}',
      '.ftw-hpd-top{display:flex;align-items:baseline;justify-content:space-between;gap:12px;margin-bottom:14px}',
      '.ftw-hpd-title{font-family:var(--mono);font-size:0.74rem;letter-spacing:0.18em;text-transform:uppercase;color:var(--fg-muted)}',
      '.ftw-hpd-close{background:none;border:1px solid var(--line);color:var(--fg);border-radius:6px;cursor:pointer;font-size:1rem;line-height:1;padding:4px 9px}',
      '.ftw-hpd-group{margin:16px 0 4px;font-family:var(--mono);font-size:0.68rem;letter-spacing:0.14em;text-transform:uppercase;color:var(--accent-e)}',
      '.ftw-hpd-item{padding:6px 0;border-bottom:1px solid var(--line-soft,var(--line))}',
      '.ftw-hpd-row{display:grid;grid-template-columns:1fr auto auto 150px;gap:10px 14px;align-items:center}',
      '.ftw-hpd-sub{font-family:var(--mono);font-size:0.62rem;color:var(--fg-muted);margin:-6px 0 12px;letter-spacing:0.02em}',
      '.ftw-hpd-explain{font-size:0.74rem;color:var(--fg-muted);line-height:1.35;margin-top:3px;max-width:66ch}',
      '.ftw-hpd-explain::before{content:"↳ ";opacity:0.55}',
      '.ftw-hpd-label{color:var(--fg-muted);font-size:0.85rem}',
      '.ftw-hpd-reg{font-family:var(--mono);font-variant-numeric:tabular-nums;font-size:0.76rem;color:var(--fg-muted);text-align:right;opacity:0.8}',
      '.ftw-hpd-val{font-family:var(--mono);font-variant-numeric:tabular-nums;color:var(--fg);text-align:right}',
      '.ftw-hpd-spark{display:flex;align-items:center;gap:5px}',
      '.ftw-hpd-spark svg{flex:1;min-width:0;height:24px;display:block}',
      '.ftw-hpd-yr{display:flex;flex-direction:column;justify-content:space-between;height:22px;min-width:30px;text-align:right;font-family:var(--mono);font-variant-numeric:tabular-nums;font-size:0.52rem;line-height:1;color:var(--fg-muted)}',
      '.ftw-hpd-empty{color:var(--fg-muted);font-family:var(--mono);font-size:0.82rem}',
    ].join('');
    var el = document.createElement('style');
    el.id = 'ftw-heating-detail-styles';
    el.textContent = css;
    document.head.appendChild(el);
  }

  function closeDetail() {
    var b = document.getElementById('ftw-hpd-backdrop');
    if (b) b.remove();
    document.removeEventListener('keydown', onDetailKey);
  }
  function onDetailKey(e) { if (e.key === 'Escape') closeDetail(); }

  // Curated explanations from the card GROUPS, keyed by metric name. Built
  // once, lazily (GROUPS is defined above at module scope).
  var _infoByKey = null;
  function infoForKey(key) {
    if (!_infoByKey) {
      _infoByKey = {};
      GROUPS.forEach(function (g) {
        (g.items || []).forEach(function (it) { if (it.info) _infoByKey[it.key] = it.info; });
      });
    }
    return _infoByKey[key] || '';
  }

  // Compact number for the sparkline min/max scale (keeps the 150px column tight).
  function sparkNum(v) {
    var a = Math.abs(v);
    if (a >= 10000) return Math.round(v / 1000) + 'k';
    if (a >= 1000) return (v / 1000).toFixed(1) + 'k';
    if (a >= 10) return Math.round(v).toString();
    return v.toFixed(1);
  }

  function openDetail(name) {
    injectDetailStyles();
    closeDetail();
    var backdrop = document.createElement('div');
    backdrop.className = 'ftw-hpd-backdrop';
    backdrop.id = 'ftw-hpd-backdrop';
    backdrop.innerHTML = '<div class="ftw-hpd">' +
      '<div class="ftw-hpd-top"><span class="ftw-hpd-title">Heat pump · ' + escapeHtml(name) + '</span>' +
      '<button class="ftw-hpd-close" type="button" aria-label="Close">✕</button></div>' +
      '<div class="ftw-hpd-sub">Trend column = last 24 h · the two small figures are its min / max over that window.</div>' +
      '<div id="ftw-hpd-body"><div class="ftw-hpd-empty">Loading signals…</div></div></div>';
    backdrop.addEventListener('click', function (e) { if (e.target === backdrop) closeDetail(); });
    backdrop.querySelector('.ftw-hpd-close').addEventListener('click', closeDetail);
    document.body.appendChild(backdrop);
    document.addEventListener('keydown', onDetailKey);

    fetchJSON('/api/drivers/' + encodeURIComponent(name)).then(function (d) {
      var body = document.getElementById('ftw-hpd-body');
      if (!body) return;
      var metrics = (d && d.metrics) || [];
      if (!metrics.length) { body.innerHTML = '<div class="ftw-hpd-empty">No signals reported yet.</div>'; return; }
      // Bucket metrics into ordered groups.
      var buckets = {};
      metrics.forEach(function (m) {
        var g = groupForUnit(m.unit);
        (buckets[g] = buckets[g] || []).push(m);
      });
      var order = UNIT_GROUPS.map(function (g) { return g.title; }).concat(['State / other']);
      var html = '';
      order.forEach(function (g) {
        var rows = buckets[g];
        if (!rows || !rows.length) return;
        rows.sort(function (a, b) { return a.name < b.name ? -1 : 1; });
        html += '<div class="ftw-hpd-group">' + escapeHtml(g) + '</div>';
        rows.forEach(function (m) {
          var explain = infoForKey(m.name) || m.title || '';
          html += '<div class="ftw-hpd-item">' +
            '<div class="ftw-hpd-row">' +
            '<span class="ftw-hpd-label">' + escapeHtml(prettyLabel(m.name)) + '</span>' +
            '<span class="ftw-hpd-reg" title="Modbus register">' + (m.register ? escapeHtml(String(m.register)) : '—') + '</span>' +
            '<span class="ftw-hpd-val">' + escapeHtml(fmtValue(m.value, m.unit)) + '</span>' +
            '<span class="ftw-hpd-spark" data-spark-metric="' + escapeHtml(m.name) + '"></span>' +
            '</div>' +
            (explain ? '<div class="ftw-hpd-explain">' + escapeHtml(explain) + '</div>' : '') +
            '</div>';
        });
      });
      body.innerHTML = html;
      // Fetch trends only for the curated dashboard signals. A NIBE driver can
      // expose ~980 points; issuing one series query per point here could flood
      // a Pi with nearly a thousand concurrent requests.
      metrics.filter(function (m) { return !!infoForKey(m.name); }).forEach(function (m) {
        fetchJSON('/api/series?driver=' + encodeURIComponent(name) + '&metric=' + encodeURIComponent(m.name) + '&range=24h&points=120')
          .then(function (s) {
            var slot = body.querySelector('.ftw-hpd-spark[data-spark-metric="' + (window.CSS && CSS.escape ? CSS.escape(m.name) : m.name) + '"]');
            if (!slot) return;
            var pp = (s && s.points) || [];
            var vals = pp.map(function (p) { return p.v; }).filter(function (v) { return v != null; });
            var yr = vals.length
              ? '<span class="ftw-hpd-yr"><span>' + escapeHtml(sparkNum(Math.max.apply(null, vals))) + '</span>' +
                '<span>' + escapeHtml(sparkNum(Math.min.apply(null, vals))) + '</span></span>'
              : '';
            slot.innerHTML = sparkline(pp) + yr;
          });
      });
    });
  }

  function onGridClick(e) {
    // The ? help icons explain a metric in place (native tooltip) — a click on
    // one must NOT navigate into the all-signals detail.
    if (e.target.closest && e.target.closest('.ftw-hp-i')) return;
    var card = e.target.closest && e.target.closest('.ftw-hp-clickable');
    if (card && card.dataset.hpDriver) openDetail(card.dataset.hpDriver);
  }

  function start() {
    if (timer) return;
    var grid = document.getElementById('heating-grid');
    if (grid) {
      grid.addEventListener('click', onGridClick);
      grid.addEventListener('keydown', function (e) {
        if ((e.key === 'Enter' || e.key === ' ') && e.target.classList && e.target.classList.contains('ftw-hp-clickable')) {
          e.preventDefault(); onGridClick(e);
        }
      });
    }
    refresh();
    timer = setInterval(refresh, REFRESH_MS);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', start);
  } else {
    start();
  }
})();
