(function () {
  'use strict';

  var loadedAssets = false;
  var loading = false;
  var rangeMS = { '24h': 86400000, '7d': 7 * 86400000, '30d': 30 * 86400000 };
  var bucket = { '24h': '15m', '7d': '1h', '30d': '6h' };
  var flowLabel = {
    grid_import: 'Grid import', grid_export: 'Grid export',
    battery_charge: 'Battery charge', battery_discharge: 'Battery discharge',
    pv_generation: 'PV generation', consumer_use: 'Consumer use',
    vehicle_charge: 'Vehicle charge', vehicle_discharge: 'Vehicle discharge'
  };

  function escapeHtml(value) {
    return String(value).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  function query() {
    var range = document.getElementById('energy-history-range');
    var asset = document.getElementById('energy-history-asset');
    var key = range ? range.value : '7d';
    var until = Date.now();
    var params = new URLSearchParams({
      scope: asset && asset.value ? 'asset' : 'system',
      since: String(until - (rangeMS[key] || rangeMS['7d'])),
      until: String(until), bucket: bucket[key] || '1h', limit: '5000'
    });
    if (asset && asset.value) params.set('asset_id', asset.value);
    return params;
  }

  function loadAssets() {
    if (loadedAssets) return Promise.resolve();
    return fetch('/api/energy/assets').then(function (response) {
      if (!response.ok) throw new Error('assets unavailable');
      return response.json();
    }).then(function (data) {
      var select = document.getElementById('energy-history-asset');
      if (!select) return;
      (data.assets || []).forEach(function (asset) {
        var option = document.createElement('option');
        option.value = asset.asset_id;
        option.textContent = (asset.label || asset.asset_id) + ' · ' + asset.kind.replace(/_/g, ' ');
        select.appendChild(option);
      });
      loadedAssets = true;
    });
  }

  function render(data) {
    var points = data.points || [];
    var totals = {};
    points.forEach(function (point) {
      if (point.quality === 'gap' || point.quality === 'reset') return;
      totals[point.flow] = (totals[point.flow] || 0) + Number(point.energy_wh || 0);
    });
    var summary = document.getElementById('energy-history-summary');
    if (summary) {
      var names = Object.keys(totals).sort();
      summary.innerHTML = names.length ? names.map(function (name) {
        return '<article><span>' + escapeHtml(flowLabel[name] || name) + '</span><strong>' +
          (totals[name] / 1000).toFixed(2) + ' kWh</strong></article>';
      }).join('') : '<div class="diagnose-empty">No energy recorded in this range.</div>';
    }
    var rows = document.getElementById('energy-history-rows');
    if (!rows) return;
    rows.innerHTML = points.length ? points.slice().reverse().map(function (point) {
      return '<tr><td>' + escapeHtml(new Date(point.bucket_start_ms).toLocaleString()) + '</td><td>' +
        escapeHtml(flowLabel[point.flow] || point.flow) + '</td><td>' +
        Number(point.energy_wh || 0).toFixed(2) + ' Wh</td><td><span class="energy-quality energy-quality-' +
        escapeHtml(point.quality) + '">' + escapeHtml(point.quality) + '</span></td><td>' +
        escapeHtml(point.source + ' · ' + point.provenance) + '</td></tr>';
    }).join('') : '<tr><td colspan="5">No energy recorded in this range.</td></tr>';
  }

  function load() {
    if (loading) return;
    loading = true;
    var rows = document.getElementById('energy-history-rows');
    if (rows) rows.innerHTML = '<tr><td colspan="5">Loading…</td></tr>';
    loadAssets().then(function () {
      var params = query();
      var csv = document.getElementById('energy-history-csv');
      if (csv) csv.href = '/api/energy/history.csv?' + params.toString();
      return fetch('/api/energy/history?' + params.toString());
    }).then(function (response) {
      if (!response.ok) throw new Error('history unavailable');
      return response.json();
    }).then(render).catch(function (error) {
      if (rows) rows.innerHTML = '<tr><td colspan="5">' + escapeHtml(error.message) + '</td></tr>';
    }).finally(function () { loading = false; });
  }

  window.ftwEnergyHistoryLoad = load;
  ['energy-history-range', 'energy-history-asset'].forEach(function (id) {
    var element = document.getElementById(id);
    if (element) element.addEventListener('change', load);
  });
  var refresh = document.getElementById('energy-history-refresh');
  if (refresh) refresh.addEventListener('click', load);
})();
