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
  var lastDiscoverMs = 0;             // Date.now() of the last discovery scan
  var DISCOVER_EVERY_MS = 300000;     // re-scan for newly-added heat pumps (5 min)

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
      '.ftw-hp-clickable{cursor:pointer;border-radius:8px;margin:-6px;padding:6px;transition:background 0.12s}',
      '.ftw-hp-clickable:hover,.ftw-hp-clickable:focus{background:var(--bg-hover,rgba(127,127,127,0.06));outline:none}',
      '.ftw-hp-head{display:flex;align-items:baseline;justify-content:space-between;gap:10px}',
      '.ftw-hp-more{font-family:var(--mono);font-size:0.66rem;letter-spacing:0.1em;text-transform:uppercase;color:var(--accent-e)}',
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
    // The whole card is a button into the detail view (all signals grouped).
    return '<div class="ftw-hp ftw-hp-clickable" data-hp-driver="' + escapeHtml(name) + '" role="button" tabindex="0" title="View all heat-pump signals">' +
      '<div class="ftw-hp-head"><span class="ftw-hp-name">' + escapeHtml(name) + '</span>' +
      '<span class="ftw-hp-more">All signals →</span></div>' +
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
      '.ftw-hpd-row{display:grid;grid-template-columns:1fr auto 120px;gap:10px 14px;align-items:center;padding:5px 0;border-bottom:1px solid var(--line-soft,var(--line))}',
      '.ftw-hpd-label{color:var(--fg-muted);font-size:0.85rem}',
      '.ftw-hpd-val{font-family:var(--mono);font-variant-numeric:tabular-nums;color:var(--fg);text-align:right}',
      '.ftw-hpd-spark svg{width:120px;height:24px;display:block}',
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

  function openDetail(name) {
    injectDetailStyles();
    closeDetail();
    var backdrop = document.createElement('div');
    backdrop.className = 'ftw-hpd-backdrop';
    backdrop.id = 'ftw-hpd-backdrop';
    backdrop.innerHTML = '<div class="ftw-hpd">' +
      '<div class="ftw-hpd-top"><span class="ftw-hpd-title">Heat pump · ' + escapeHtml(name) + '</span>' +
      '<button class="ftw-hpd-close" type="button" aria-label="Close">✕</button></div>' +
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
          html += '<div class="ftw-hpd-row">' +
            '<span class="ftw-hpd-label">' + escapeHtml(prettyLabel(m.name)) + '</span>' +
            '<span class="ftw-hpd-val">' + escapeHtml(fmtValue(m.value, m.unit)) + '</span>' +
            '<span class="ftw-hpd-spark" data-spark-metric="' + escapeHtml(m.name) + '"></span>' +
            '</div>';
        });
      });
      body.innerHTML = html;
      // Lazily fill sparklines (one /api/series per metric) — values already
      // shown, so a slow series fetch never blocks the table.
      metrics.forEach(function (m) {
        fetchJSON('/api/series?driver=' + encodeURIComponent(name) + '&metric=' + encodeURIComponent(m.name) + '&range=24h&points=120')
          .then(function (s) {
            var slot = body.querySelector('.ftw-hpd-spark[data-spark-metric="' + (window.CSS && CSS.escape ? CSS.escape(m.name) : m.name) + '"]');
            if (slot) slot.innerHTML = sparkline((s && s.points) || []);
          });
      });
    });
  }

  function onGridClick(e) {
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
