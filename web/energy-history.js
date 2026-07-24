(function () {
  'use strict';

  var loadedAssets = false;
  var loading = false;
  var lastData = null;
  var tablePoints = [];
  var tablePage = 0;
  var tablePageSize = 50;
  var rangeMS = { '24h': 86400000, '7d': 7 * 86400000, '30d': 30 * 86400000 };
  var bucket = { '24h': '15m', '7d': '1h', '30d': '6h' };
  var flowLabel = {
    grid_import: 'Grid import', grid_export: 'Grid export',
    battery_charge: 'Battery charge', battery_discharge: 'Battery discharge',
    pv_generation: 'PV generation', consumer_use: 'Consumer use',
    vehicle_charge: 'Vehicle charge', vehicle_discharge: 'Vehicle discharge'
  };
  var flowColor = {
    grid_import: '#ef4444', grid_export: '#f97316',
    battery_charge: '#3b82f6', battery_discharge: '#8b5cf6',
    pv_generation: '#22c55e', consumer_use: '#eab308',
    vehicle_charge: '#06b6d4', vehicle_discharge: '#14b8a6'
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
    lastData = data;
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
    tablePoints = points.slice().reverse();
    tablePage = 0;
    renderTablePage();
    drawChart(data);
  }

  function renderTablePage() {
    var rows = document.getElementById('energy-history-rows');
    if (!rows) return;
    var pageCount = Math.max(1, Math.ceil(tablePoints.length / tablePageSize));
    tablePage = Math.max(0, Math.min(tablePage, pageCount - 1));
    var start = tablePage * tablePageSize;
    var visible = tablePoints.slice(start, start + tablePageSize);
    rows.innerHTML = visible.length ? visible.map(function (point) {
      return '<tr><td>' + escapeHtml(new Date(point.bucket_start_ms).toLocaleString()) + '</td><td>' +
        escapeHtml(flowLabel[point.flow] || point.flow) + '</td><td>' +
        Number(point.energy_wh || 0).toFixed(2) + ' Wh</td><td><span class="energy-quality energy-quality-' +
        escapeHtml(point.quality) + '">' + escapeHtml(point.quality) + '</span></td><td>' +
        escapeHtml(point.source + ' · ' + point.provenance) + '</td></tr>';
    }).join('') : '<tr><td colspan="5">No energy recorded in this range.</td></tr>';
    var page = document.getElementById('energy-history-page');
    if (page) page.textContent = 'Page ' + (tablePage + 1) + ' of ' + pageCount;
    var previous = document.getElementById('energy-history-prev');
    var next = document.getElementById('energy-history-next');
    if (previous) previous.disabled = tablePage === 0;
    if (next) next.disabled = tablePage >= pageCount - 1;
    var meta = document.getElementById('energy-history-detail-meta');
    if (meta) {
      meta.textContent = tablePoints.length + ' row' + (tablePoints.length === 1 ? '' : 's');
    }
  }

  function drawChart(data) {
    var canvas = document.getElementById('energy-history-chart');
    if (!canvas) return;
    var points = (data.points || []).filter(function (point) {
      return point.quality !== 'gap' && point.quality !== 'reset' &&
        Number.isFinite(Number(point.energy_wh));
    });
    var width = Math.max(280, Math.round(canvas.clientWidth || 0));
    var height = 260;
    var ratio = Math.max(1, window.devicePixelRatio || 1);
    canvas.width = Math.round(width * ratio);
    canvas.height = Math.round(height * ratio);
    var ctx = canvas.getContext('2d');
    ctx.scale(ratio, ratio);
    ctx.clearRect(0, 0, width, height);

    var legend = document.getElementById('energy-history-legend');
    var flows = Array.from(new Set(points.map(function (point) { return point.flow; }))).sort();
    if (legend) {
      legend.innerHTML = flows.map(function (flow) {
        var color = flowColor[flow] || '#94a3b8';
        return '<span><i style="background:' + color + '"></i>' +
          escapeHtml(flowLabel[flow] || flow) + '</span>';
      }).join('');
    }
    if (!points.length) {
      ctx.fillStyle = '#94a3b8';
      ctx.font = '13px system-ui, sans-serif';
      ctx.fillText('No energy recorded in this range.', 16, 28);
      return;
    }

    var times = Array.from(new Set(points.map(function (point) {
      return Number(point.bucket_start_ms);
    }))).sort(function (a, b) { return a - b; });
    var pad = { left: 54, right: 16, top: 18, bottom: 36 };
    var plotW = Math.max(1, width - pad.left - pad.right);
    var plotH = Math.max(1, height - pad.top - pad.bottom);
    var maxWh = Math.max.apply(null, points.map(function (point) {
      return Math.max(0, Number(point.energy_wh) || 0);
    }));
    maxWh = Math.max(1, maxWh);
    var textColor = getComputedStyle(document.documentElement).getPropertyValue('--text-dim').trim() || '#94a3b8';
    var borderColor = getComputedStyle(document.documentElement).getPropertyValue('--border').trim() || '#334155';
    ctx.font = '11px system-ui, sans-serif';
    ctx.fillStyle = textColor;
    ctx.strokeStyle = borderColor;
    ctx.lineWidth = 1;
    for (var yTick = 0; yTick <= 4; yTick += 1) {
      var y = pad.top + plotH * yTick / 4;
      ctx.beginPath();
      ctx.moveTo(pad.left, y);
      ctx.lineTo(width - pad.right, y);
      ctx.stroke();
      ctx.textAlign = 'right';
      var tickKWh = maxWh * (1 - yTick / 4) / 1000;
      ctx.fillText(tickKWh.toFixed(tickKWh < 0.1 ? 2 : 1) + ' kWh', pad.left - 7, y + 4);
    }
    var xFor = function (index) {
      return times.length === 1 ? pad.left + plotW / 2 : pad.left + plotW * index / (times.length - 1);
    };
    var yFor = function (value) { return pad.top + plotH * (1 - value / maxWh); };
    var timeIndex = new Map(times.map(function (time, index) { return [time, index]; }));
    flows.forEach(function (flow) {
      var byTime = new Map();
      points.forEach(function (point) {
        if (point.flow === flow) byTime.set(Number(point.bucket_start_ms), Math.max(0, Number(point.energy_wh) || 0));
      });
      ctx.strokeStyle = flowColor[flow] || '#94a3b8';
      ctx.lineWidth = 2;
      ctx.beginPath();
      var drawing = false;
      times.forEach(function (time) {
        if (!byTime.has(time)) {
          drawing = false;
          return;
        }
        var x = xFor(timeIndex.get(time));
        var y = yFor(byTime.get(time));
        if (drawing) ctx.lineTo(x, y);
        else ctx.moveTo(x, y);
        drawing = true;
      });
      ctx.stroke();
      ctx.fillStyle = flowColor[flow] || '#94a3b8';
      byTime.forEach(function (value, time) {
        ctx.beginPath();
        ctx.arc(xFor(timeIndex.get(time)), yFor(value), 3, 0, Math.PI * 2);
        ctx.fill();
      });
    });
    ctx.fillStyle = textColor;
    ctx.textAlign = 'center';
    var labelCount = Math.min(5, times.length);
    for (var label = 0; label < labelCount; label += 1) {
      var index = labelCount === 1 ? 0 : Math.round(label * (times.length - 1) / (labelCount - 1));
      var stamp = new Date(times[index]);
      ctx.fillText(stamp.toLocaleDateString(undefined, { month: 'short', day: 'numeric' }), xFor(index), height - 12);
    }
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
  var previous = document.getElementById('energy-history-prev');
  if (previous) previous.addEventListener('click', function () {
    if (tablePage > 0) {
      tablePage -= 1;
      renderTablePage();
    }
  });
  var next = document.getElementById('energy-history-next');
  if (next) next.addEventListener('click', function () {
    if ((tablePage + 1) * tablePageSize < tablePoints.length) {
      tablePage += 1;
      renderTablePage();
    }
  });
  window.addEventListener('resize', function () {
    if (lastData) drawChart(lastData);
  });
})();
