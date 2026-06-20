// heating.js — heat-pump telemetry card on the main dashboard.
//
// Read-only view over the MyUplink driver's hp_* metrics (compressor power +
// hot-water/indoor/outdoor temperatures). The section stays hidden until a
// driver actually reports hp_power_w, so a site without a heat pump never
// sees an empty card. Discovery runs once on load (one /api/drivers/{name}
// fetch per driver); steady-state polling then only touches the heat-pump
// drivers, so remote routes don't pay for every driver every 30 s.
//
// See docs/myuplink-oauth.md. No control here — telemetry only.

(function () {
  'use strict';

  var REFRESH_MS = 30000;
  var timer = null;
  var heatPumpDrivers = null; // cached after discovery: array of driver names

  // Route reads over the owner/P2P transport when present (remote home
  // route), else plain fetch (LAN / tests). Mirrors twins.js.
  function ownerFetch(path, opts) {
    if (typeof window.ownerFetch === 'function') return window.ownerFetch(path, opts);
    return fetch(path, opts);
  }

  // hp_* metric → display label + formatter. Order here is render order.
  var METRICS = [
    { key: 'hp_power_w', label: 'Compressor', fmt: fmtPower },
    { key: 'hp_hw_top_temp_c', label: 'Hot water', fmt: fmtTemp },
    { key: 'hp_indoor_temp_c', label: 'Indoor', fmt: fmtTemp },
    { key: 'hp_outdoor_temp_c', label: 'Outdoor', fmt: fmtTemp },
  ];

  function fmtPower(v) {
    if (v == null) return '—';
    if (Math.abs(v) >= 1000) return (v / 1000).toFixed(2) + ' kW';
    return Math.round(v) + ' W';
  }
  function fmtTemp(v) {
    if (v == null) return '—';
    return v.toFixed(1) + ' °C';
  }

  function injectStyles() {
    if (document.getElementById('ftw-heating-styles')) return;
    var css = [
      '#heating-grid{display:flex;flex-direction:column;gap:18px}',
      '.ftw-hp{display:flex;flex-direction:column;gap:12px}',
      '.ftw-hp-name{font-family:var(--mono);font-size:0.72rem;letter-spacing:0.18em;text-transform:uppercase;color:var(--fg-muted)}',
      '.ftw-hp-tiles{display:grid;grid-template-columns:repeat(auto-fit,minmax(110px,1fr));gap:10px 18px}',
      '.ftw-hp-tile{display:flex;flex-direction:column;gap:3px}',
      '.ftw-hp-tile-label{font-family:var(--mono);font-size:0.68rem;letter-spacing:0.12em;text-transform:uppercase;color:var(--fg-muted)}',
      '.ftw-hp-tile-val{font-family:var(--mono);font-size:1.05rem;font-variant-numeric:tabular-nums;color:var(--fg)}',
      '.ftw-hp-spark{display:flex;flex-direction:column;gap:4px}',
      '.ftw-hp-spark-label{font-family:var(--mono);font-size:0.66rem;letter-spacing:0.12em;text-transform:uppercase;color:var(--fg-muted)}',
      '.ftw-hp-spark svg{width:100%;height:48px;display:block}',
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

  function renderPump(name, detail, sparkPoints) {
    var m = metricMap(detail && detail.metrics);
    var tiles = METRICS.map(function (def) {
      var has = Object.prototype.hasOwnProperty.call(m, def.key);
      return '<div class="ftw-hp-tile">' +
        '<span class="ftw-hp-tile-label">' + def.label + '</span>' +
        '<span class="ftw-hp-tile-val">' + (has ? def.fmt(m[def.key]) : '—') + '</span>' +
        '</div>';
    }).join('');
    var spark = sparkline(sparkPoints);
    var sparkBlock = spark
      ? '<div class="ftw-hp-spark"><span class="ftw-hp-spark-label">Compressor power · 24h</span>' + spark + '</div>'
      : '';
    return '<div class="ftw-hp">' +
      '<span class="ftw-hp-name">' + escapeHtml(name) + '</span>' +
      '<div class="ftw-hp-tiles">' + tiles + '</div>' +
      sparkBlock +
      '</div>';
  }

  function escapeHtml(s) {
    return String(s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  function refresh() {
    var section = document.getElementById('heating-section');
    var grid = document.getElementById('heating-grid');
    if (!section || !grid) return;

    var ready = heatPumpDrivers
      ? Promise.resolve(heatPumpDrivers)
      : discover().then(function (names) { heatPumpDrivers = names; return names; });

    ready.then(function (names) {
      if (!names || names.length === 0) {
        section.hidden = true;
        return;
      }
      // Fetch detail + 24h power series for each heat pump in parallel.
      return Promise.all(names.map(function (n) {
        return Promise.all([
          fetchJSON('/api/drivers/' + encodeURIComponent(n)),
          fetchJSON('/api/series?driver=' + encodeURIComponent(n) + '&metric=hp_power_w&range=24h&points=200'),
        ]).then(function (parts) {
          return { name: n, detail: parts[0], series: (parts[1] && parts[1].points) || [] };
        });
      })).then(function (pumps) {
        var live = pumps.filter(function (p) { return p.detail && isHeatPump(p.detail); });
        if (live.length === 0) { section.hidden = true; return; }
        injectStyles();
        section.hidden = false;
        grid.innerHTML = live.map(function (p) {
          return renderPump(p.name, p.detail, p.series);
        }).join('');
      });
    });
  }

  function start() {
    if (timer) return;
    refresh();
    timer = setInterval(refresh, REFRESH_MS);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', start);
  } else {
    start();
  }
})();
