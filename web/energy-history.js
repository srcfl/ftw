(function () {
  'use strict';

  var activeRange = '7d';
  var loadedAssets = false;
  var dailyRequest = 0;
  var ledgerRequest = 0;
  var lastDays = [];
  var tablePoints = [];
  var tablePage = 0;
  var tablePageSize = 50;
  var rangeDays = { '24h': 1, '7d': 7, '30d': 30 };
  var rangeMS = { '24h': 86400000, '7d': 7 * 86400000, '30d': 30 * 86400000 };
  var ledgerBucket = { '24h': '15m', '7d': '1h', '30d': '6h' };
  var periodTitle = { '24h': 'Today', '7d': 'Last 7 days', '30d': 'Last 30 days' };
  var flowLabel = {
    grid_import: 'Grid import', grid_export: 'Grid export',
    battery_charge: 'Battery charge', battery_discharge: 'Battery discharge',
    pv_generation: 'Solar', consumer_use: 'Home use',
    vehicle_charge: 'Vehicle charge', vehicle_discharge: 'Vehicle discharge'
  };
  var dailySeries = [
    { key: 'load_wh', label: 'Home use', color: '#eab308' },
    { key: 'pv_wh', label: 'Solar', color: '#22c55e' },
    { key: 'import_wh', label: 'Grid import', color: '#ef4444' },
    { key: 'export_wh', label: 'Grid export', color: '#f97316' }
  ];

  function canvasColors() {
    return window.ftwThemeColors
      ? window.ftwThemeColors.palette()
      : {
          text: '#e8e8e8',
          dim: '#a0a0a0',
          muted: '#858585',
          line: '#2a2a2a',
          panel: '#161616',
          accent: '#f5b942'
        };
  }

  function escapeHtml(value) {
    return String(value).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  function sum(days, key) {
    return days.reduce(function (total, day) {
      var value = Number(day[key] || 0);
      return total + (Number.isFinite(value) && value > 0 ? value : 0);
    }, 0);
  }

  function formatKWh(wh) {
    var kwh = Math.max(0, Number(wh) || 0) / 1000;
    var digits = kwh < 10 ? 1 : 0;
    return kwh.toLocaleString(undefined, {
      minimumFractionDigits: digits,
      maximumFractionDigits: digits
    }) + ' kWh';
  }

  function formatDay(day) {
    var stamp = new Date(day + 'T12:00:00');
    return stamp.toLocaleDateString(undefined, { weekday: 'short', month: 'short', day: 'numeric' });
  }

  function setRange(next) {
    if (!rangeDays[next] || next === activeRange) return;
    activeRange = next;
    var title = document.getElementById('energy-history-period-title');
    if (title) title.textContent = periodTitle[next];
    var tabs = document.querySelectorAll('#energy-history-range button[data-range]');
    tabs.forEach(function (tab) {
      var selected = tab.getAttribute('data-range') === next;
      tab.classList.toggle('active', selected);
      tab.setAttribute('aria-selected', selected ? 'true' : 'false');
    });
    loadDaily();
    var details = document.getElementById('energy-history-details');
    if (details && details.open) loadLedger();
  }

  function renderSummary(days) {
    var summary = document.getElementById('energy-history-summary');
    if (!summary) return;
    var cards = [
      { label: 'Used at home', key: 'load_wh', note: 'All household use' },
      { label: 'Made by solar', key: 'pv_wh', note: 'PV production' },
      { label: 'Bought from grid', key: 'import_wh', note: 'Grid import' },
      { label: 'Sent to grid', key: 'export_wh', note: 'Grid export' }
    ];
    summary.innerHTML = cards.map(function (card) {
      return '<article><span>' + card.label + '</span><strong>' +
        formatKWh(sum(days, card.key)) + '</strong><small>' + card.note + '</small></article>';
    }).join('');
  }

  function renderInsights(days) {
    var target = document.getElementById('energy-history-insight');
    if (!target) return;
    if (!days.length) {
      target.innerHTML = '<h3>What stands out</h3><p>No energy history is available for this period.</p>';
      return;
    }
    var totalLoad = sum(days, 'load_wh');
    var totalPV = sum(days, 'pv_wh');
    var charged = sum(days, 'bat_charged_wh');
    var discharged = sum(days, 'bat_discharged_wh');
    var bestSolar = days.reduce(function (best, day) {
      return !best || Number(day.pv_wh || 0) > Number(best.pv_wh || 0) ? day : best;
    }, null);
    var highestUse = days.reduce(function (best, day) {
      return !best || Number(day.load_wh || 0) > Number(best.load_wh || 0) ? day : best;
    }, null);
    var comparison = totalLoad > 0 ? Math.round(totalPV / totalLoad * 100) : 0;
    target.innerHTML = '<h3>What stands out</h3><ul>' +
      '<li>Solar made <strong>' + comparison + '%</strong> as much energy as the home used.</li>' +
      '<li>Best solar day was <strong>' + escapeHtml(formatDay(bestSolar.day)) + '</strong> at ' +
        formatKWh(bestSolar.pv_wh) + '.</li>' +
      '<li>Highest home use was <strong>' + escapeHtml(formatDay(highestUse.day)) + '</strong> at ' +
        formatKWh(highestUse.load_wh) + '.</li>' +
      '<li>The battery charged ' + formatKWh(charged) + ' and discharged ' +
        formatKWh(discharged) + '.</li></ul>';
  }

  function drawDailyChart(days) {
    var canvas = document.getElementById('energy-history-chart');
    if (!canvas) return;
    var width = Math.max(280, Math.round(canvas.clientWidth || 0));
    var height = 250;
    var ratio = Math.max(1, window.devicePixelRatio || 1);
    canvas.width = Math.round(width * ratio);
    canvas.height = Math.round(height * ratio);
    var ctx = canvas.getContext('2d');
    ctx.scale(ratio, ratio);
    ctx.clearRect(0, 0, width, height);
    var C = canvasColors();

    var legend = document.getElementById('energy-history-legend');
    if (legend) {
      legend.innerHTML = dailySeries.map(function (series) {
        return '<span><i style="background:' + series.color + '"></i>' + series.label + '</span>';
      }).join('');
    }
    if (!days.length) {
      ctx.fillStyle = C.muted;
      ctx.font = '13px system-ui, sans-serif';
      ctx.fillText('No energy history is available for this period.', 16, 28);
      return;
    }

    var pad = { left: 52, right: 12, top: 14, bottom: 42 };
    var plotW = Math.max(1, width - pad.left - pad.right);
    var plotH = Math.max(1, height - pad.top - pad.bottom);
    var maxWh = Math.max.apply(null, days.flatMap(function (day) {
      return dailySeries.map(function (series) { return Math.max(0, Number(day[series.key]) || 0); });
    }));
    maxWh = Math.max(1000, maxWh);
    var textColor = C.dim;
    var borderColor = C.line;
    ctx.font = '11px system-ui, sans-serif';
    ctx.fillStyle = textColor;
    ctx.strokeStyle = borderColor;
    ctx.lineWidth = 1;
    for (var tick = 0; tick <= 4; tick += 1) {
      var y = pad.top + plotH * tick / 4;
      ctx.beginPath();
      ctx.moveTo(pad.left, y);
      ctx.lineTo(width - pad.right, y);
      ctx.stroke();
      ctx.textAlign = 'right';
      ctx.fillText((maxWh * (1 - tick / 4) / 1000).toFixed(0) + ' kWh', pad.left - 7, y + 4);
    }

    var groupW = plotW / days.length;
    var barGap = Math.min(2, groupW * 0.04);
    var barW = Math.max(1, Math.min(16, (groupW - 6) / dailySeries.length));
    days.forEach(function (day, dayIndex) {
      var totalBarsW = dailySeries.length * barW + (dailySeries.length - 1) * barGap;
      var startX = pad.left + groupW * dayIndex + (groupW - totalBarsW) / 2;
      dailySeries.forEach(function (series, seriesIndex) {
        var value = Math.max(0, Number(day[series.key]) || 0);
        var barH = plotH * value / maxWh;
        ctx.fillStyle = series.color;
        ctx.fillRect(startX + seriesIndex * (barW + barGap), pad.top + plotH - barH, barW, barH);
      });
    });

    ctx.fillStyle = textColor;
    ctx.textAlign = 'center';
    var labelCount = Math.min(activeRange === '30d' ? 6 : 7, days.length);
    for (var label = 0; label < labelCount; label += 1) {
      var index = labelCount === 1 ? 0 : Math.round(label * (days.length - 1) / (labelCount - 1));
      var stamp = new Date(days[index].day + 'T12:00:00');
      var text = activeRange === '24h'
        ? 'Today'
        : stamp.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
      ctx.fillText(text, pad.left + groupW * (index + 0.5), height - 14);
    }
  }

  function loadDaily() {
    var request = ++dailyRequest;
    var summary = document.getElementById('energy-history-summary');
    if (summary) summary.innerHTML = '<div class="diagnose-empty">Loading energy history…</div>';
    return fetch('/api/energy/daily?days=' + rangeDays[activeRange]).then(function (response) {
      if (!response.ok) throw new Error('energy history unavailable');
      return response.json();
    }).then(function (data) {
      if (request !== dailyRequest) return;
      lastDays = data.days || [];
      renderSummary(lastDays);
      renderInsights(lastDays);
      drawDailyChart(lastDays);
    }).catch(function (error) {
      if (request !== dailyRequest) return;
      if (summary) summary.innerHTML = '<div class="diagnose-empty">' + escapeHtml(error.message) + '</div>';
      var insight = document.getElementById('energy-history-insight');
      if (insight) insight.innerHTML = '<h3>What stands out</h3><p>History could not be loaded.</p>';
      drawDailyChart([]);
    });
  }

  function ledgerQuery() {
    var asset = document.getElementById('energy-history-asset');
    var until = Date.now();
    var params = new URLSearchParams({
      scope: asset && asset.value ? 'asset' : 'system',
      since: String(until - rangeMS[activeRange]),
      until: String(until),
      bucket: ledgerBucket[activeRange],
      limit: '5000'
    });
    if (asset && asset.value) params.set('asset_id', asset.value);
    return params;
  }

  function loadAssets() {
    if (loadedAssets) return Promise.resolve();
    return fetch('/api/energy/assets').then(function (response) {
      if (!response.ok) throw new Error('devices unavailable');
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

  function normalizeLedgerPoint(point) {
    var energyWh = Number(point.energy_wh || 0);
    var durationMS = Number(point.bucket_len_ms || 0);
    var maxWh = 1_000_000 * durationMS / 3_600_000;
    if (durationMS > 0 && energyWh > maxWh) {
      return Object.assign({}, point, {
        quality: 'invalid',
        provenance: 'implausible_energy'
      });
    }
    return point;
  }

  function renderLedger(data) {
    tablePoints = (data.points || []).map(normalizeLedgerPoint).reverse();
    tablePage = 0;
    var rejected = tablePoints.filter(function (point) { return point.quality === 'invalid'; }).length;
    var uncertain = tablePoints.filter(function (point) {
      return point.quality === 'gap' || point.quality === 'reset';
    }).length;
    var note = document.getElementById('energy-history-quality');
    if (note) {
      if (rejected) {
        note.className = 'energy-history-quality-note warning';
        note.textContent = rejected + ' impossible counter reading' + (rejected === 1 ? ' was' : 's were') +
          ' rejected and excluded from totals. The raw rows remain visible for diagnosis.';
      } else if (uncertain) {
        note.className = 'energy-history-quality-note';
        note.textContent = uncertain + ' gap or counter reset marker' + (uncertain === 1 ? ' is' : 's are') +
          ' recorded in this period. Trusted daily totals stay above.';
      } else {
        note.className = 'energy-history-quality-note good';
        note.textContent = 'No counter jumps or data gaps were found in this selection.';
      }
    }
    renderTablePage();
  }

  function renderTablePage() {
    var rows = document.getElementById('energy-history-rows');
    if (!rows) return;
    var pageCount = Math.max(1, Math.ceil(tablePoints.length / tablePageSize));
    tablePage = Math.max(0, Math.min(tablePage, pageCount - 1));
    var start = tablePage * tablePageSize;
    var visible = tablePoints.slice(start, start + tablePageSize);
    rows.innerHTML = visible.length ? visible.map(function (point) {
      var label = point.quality === 'invalid' ? 'rejected' : point.quality;
      return '<tr><td>' + escapeHtml(new Date(point.bucket_start_ms).toLocaleString()) + '</td><td>' +
        escapeHtml(flowLabel[point.flow] || point.flow) + '</td><td>' +
        formatKWh(point.energy_wh) + '</td><td><span class="energy-quality energy-quality-' +
        escapeHtml(point.quality) + '">' + escapeHtml(label) + '</span></td><td>' +
        escapeHtml(point.source + ' · ' + point.provenance) + '</td></tr>';
    }).join('') : '<tr><td colspan="5">No source data was recorded in this period.</td></tr>';
    var page = document.getElementById('energy-history-page');
    if (page) page.textContent = 'Page ' + (tablePage + 1) + ' of ' + pageCount;
    var previous = document.getElementById('energy-history-prev');
    var next = document.getElementById('energy-history-next');
    if (previous) previous.disabled = tablePage === 0;
    if (next) next.disabled = tablePage >= pageCount - 1;
    var meta = document.getElementById('energy-history-detail-meta');
    if (meta) meta.textContent = tablePoints.length + ' row' + (tablePoints.length === 1 ? '' : 's');
  }

  function loadLedger() {
    var request = ++ledgerRequest;
    var rows = document.getElementById('energy-history-rows');
    if (rows) rows.innerHTML = '<tr><td colspan="5">Loading source data…</td></tr>';
    return loadAssets().then(function () {
      var params = ledgerQuery();
      var csv = document.getElementById('energy-history-csv');
      if (csv) csv.href = '/api/energy/history.csv?' + params.toString();
      return fetch('/api/energy/history?' + params.toString());
    }).then(function (response) {
      if (!response.ok) throw new Error('source data unavailable');
      return response.json();
    }).then(function (data) {
      if (request === ledgerRequest) renderLedger(data);
    }).catch(function (error) {
      if (request !== ledgerRequest) return;
      if (rows) rows.innerHTML = '<tr><td colspan="5">' + escapeHtml(error.message) + '</td></tr>';
    });
  }

  function load() {
    loadDaily();
    var details = document.getElementById('energy-history-details');
    if (details && details.open) loadLedger();
  }

  window.ftwEnergyHistoryLoad = load;
  var range = document.getElementById('energy-history-range');
  if (range) range.addEventListener('click', function (event) {
    var button = event.target.closest('button[data-range]');
    if (button) setRange(button.getAttribute('data-range'));
  });
  var refresh = document.getElementById('energy-history-refresh');
  if (refresh) refresh.addEventListener('click', load);
  var details = document.getElementById('energy-history-details');
  if (details) details.addEventListener('toggle', function () {
    if (details.open) loadLedger();
  });
  var asset = document.getElementById('energy-history-asset');
  if (asset) asset.addEventListener('change', loadLedger);
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
    if (lastDays.length) drawDailyChart(lastDays);
  });
  window.addEventListener('ftw-theme-change', function () {
    drawDailyChart(lastDays);
  });
})();
