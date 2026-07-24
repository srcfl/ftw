// diagnose.js — time-travel through persisted planner snapshots.
//
// Owns the five-destination dashboard router. Historical deep links remain
// compatible: #live maps to #overview and #diagnose/<ts> opens the
// matching decision under Plan.
//
// Fetches from /api/mpc/diagnose/history for the timeline list and
// /api/mpc/diagnose/at?ts=<ms> for the detail pane. Persistence lands
// in SQLite planner_diagnostics + parquet cold storage — see PR-A.
//
// The detail canvas chart is intentionally simpler than plan.js — just
// a stacked band of price + battery + grid + EV power + SoC so the
// operator can eyeball "what did the DP decide each slot and why".

(function () {
  'use strict';

  function apiFetch(path, opts) {
    return fetch(path, opts);
  }

  function canvasColors() {
    return window.ftwThemeColors
      ? window.ftwThemeColors.palette()
      : {
          text: '#e8e8e8',
          dim: '#a0a0a0',
          muted: '#858585',
          line: '#2a2a2a',
          panel: '#161616',
          accent: '#f5b942',
        };
  }

  // ---- Destination layout + routing ----
  const VIEW_NAMES = ['overview', 'energy', 'plan', 'history', 'more'];

  // The dashboard predates top-level destinations. Move the existing,
  // stateful sections into their destination shells rather than cloning
  // them: custom elements, canvas bindings and polling timers keep the same
  // DOM identity.
  function organizeDestinations() {
    const energy = document.getElementById('view-energy');
    const plan = document.getElementById('view-plan');
    const more = document.getElementById('view-more');
    const append = (host, selector) => {
      const el = document.querySelector(selector);
      if (host && el) host.appendChild(el);
    };

    ['#chart-section', '.energy-row', '.prices-row', '#heating-section']
      .forEach(selector => append(energy, selector));
    append(energy, '.history-row');
    append(plan, '#plan-section');
    append(plan, '#plan-history-details');
    ['#ui-mode-row', '#twins-section', '#loadpoints-section', '#drivers-section', '#models-section']
      .forEach(selector => append(more, selector));

    const oldChartRow = document.getElementById('live-plan-row');
    if (oldChartRow && oldChartRow.children.length === 0) oldChartRow.remove();

    const settingsShortcut = document.getElementById('more-settings-btn');
    if (settingsShortcut) settingsShortcut.addEventListener('click', () => {
      const settings = document.getElementById('settings-btn');
      if (settings) settings.click();
    });
    const updateShortcut = document.getElementById('more-update-btn');
    if (updateShortcut) updateShortcut.addEventListener('click', () => {
      const badge = document.querySelector('ftw-update-badge');
      if (badge && typeof badge.open === 'function') badge.open();
    });
  }

  function applyHash() {
    const h = (location.hash || '#overview').replace(/^#/, '');
    const parts = h.split('/');
    const requested = parts[0] === 'live' ? 'overview'
      : parts[0] === 'diagnose' ? 'plan'
      : parts[0];
    const view = VIEW_NAMES.includes(requested) ? requested : 'overview';
    document.querySelectorAll('.app-view').forEach(panel => {
      panel.classList.toggle('hidden', panel.id !== 'view-' + view);
    });
    document.querySelectorAll('.app-nav-btn[data-view]').forEach(b => {
      const active = b.dataset.view === view;
      b.classList.toggle('active', active);
      b.setAttribute('aria-selected', active ? 'true' : 'false');
      b.tabIndex = active ? 0 : -1;
    });
    document.body.dataset.view = view;
    requestAnimationFrame(() => window.dispatchEvent(new Event('resize')));
    if (view === 'history') {
      if (typeof window.ftwEnergyHistoryLoad === 'function') {
        window.ftwEnergyHistoryLoad();
      }
    }
    if (view === 'plan' && parts[0] === 'diagnose') {
      const selectedTs = parts[1] ? Number(parts[1]) : null;
      const plannerDetails = document.getElementById('plan-history-details');
      state.selectedTs = selectedTs;
      if (selectedTs && plannerDetails) plannerDetails.open = true;
      if (selectedTs || (plannerDetails && plannerDetails.open)) {
        loadTimeline().then(() => {
          if (selectedTs) loadDetail(selectedTs);
          else if (!state.selectedTs && state.timeline.length > 0) {
            loadDetail(state.timeline[0].ts_ms);
          }
        });
      }
    }
  }

  document.addEventListener('click', (e) => {
    const b = e.target.closest('.app-nav-btn[data-view]');
    if (!b) return;
    location.hash = '#' + b.dataset.view;
    const hdr = document.querySelector('body.ftw-app > header');
    if (hdr) {
      hdr.classList.remove('menu-open');
      const mbtn = document.getElementById('mobile-menu-btn');
      if (mbtn) mbtn.setAttribute('aria-expanded', 'false');
    }
  });
  document.querySelectorAll('[role="tablist"]').forEach(tablist => {
    tablist.addEventListener('keydown', (e) => {
      if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(e.key)) return;
      const buttons = [...tablist.querySelectorAll('.app-nav-btn[data-view]')];
      if (!buttons.length) return;
      const current = Math.max(0, buttons.indexOf(document.activeElement));
      let next = e.key === 'Home' ? 0 : e.key === 'End' ? buttons.length - 1
        : (current + (e.key === 'ArrowRight' ? 1 : -1) + buttons.length) % buttons.length;
      e.preventDefault();
      buttons[next].focus();
      buttons[next].click();
    });
  });
  window.addEventListener('hashchange', applyHash);

  // ---- Data state ----
  const state = {
    timeline: [],      // [{ts_ms, reason, zone, total_cost_ore, horizon_slots}]
    selectedTs: null,  // currently loaded snapshot
    detail: null,      // full snapshot object
    rangeMs: 24 * 3600 * 1000,
    chartGeom: null,   // {padL, padT, barW, plotH, nSlots} — for hover hit-testing
    hoverSlotIdx: null,
    timelineLoading: null,
  };

  const rangeSelect = document.getElementById('diagnose-range-select');
  if (rangeSelect) {
    rangeSelect.addEventListener('change', () => {
      state.rangeMs = parseRange(rangeSelect.value);
      loadTimeline();
    });
  }
  const refreshBtn = document.getElementById('diagnose-refresh');
  if (refreshBtn) refreshBtn.addEventListener('click', () => loadTimeline());
  const plannerDetails = document.getElementById('plan-history-details');
  if (plannerDetails) {
    plannerDetails.addEventListener('toggle', () => {
      if (!plannerDetails.open) return;
      loadTimeline().then(() => {
        if (!state.selectedTs && state.timeline.length > 0) {
          loadDetail(state.timeline[0].ts_ms);
        }
      });
    });
  }

  function parseRange(v) {
    const map = { '1h': 3600e3, '6h': 6 * 3600e3, '24h': 86400e3,
                  '7d': 7 * 86400e3, '30d': 30 * 86400e3 };
    return map[v] || 86400e3;
  }

  // ---- Timeline fetch + render ----
  function loadTimeline() {
    if (state.timelineLoading) return state.timelineLoading;
    state.timelineLoading = fetchTimeline().finally(() => {
      state.timelineLoading = null;
    });
    return state.timelineLoading;
  }

  async function fetchTimeline() {
    const until = Date.now();
    const since = until - state.rangeMs;
    try {
      const r = await apiFetch(`/api/mpc/diagnose/history?since=${since}&until=${until}&limit=2000`);
      const j = await r.json();
      state.timeline = (j && j.snapshots) || [];
      renderTimeline();
    } catch (e) {
      const el = document.getElementById('diagnose-timeline');
      if (el) el.innerHTML = `<div class="diagnose-empty">Error loading: ${escapeHtml(e.message)}</div>`;
    }
    const meta = document.getElementById('diagnose-meta');
    if (meta) meta.textContent = state.timeline.length + ' decision' +
      (state.timeline.length === 1 ? '' : 's');
  }

  function renderTimeline() {
    const el = document.getElementById('diagnose-timeline');
    if (!el) return;
    if (state.timeline.length === 0) {
      el.innerHTML = '<div class="diagnose-empty">No snapshots in range.</div>';
      return;
    }
    const rows = state.timeline.map(s => {
      const cls = 'diag-row diag-reason-' + reasonClass(s.reason);
      const active = s.ts_ms === state.selectedTs ? ' active' : '';
      return `<button class="${cls}${active}" data-ts="${s.ts_ms}">
        <span class="diag-row-time">${fmtHHMM(s.ts_ms)} <span class="diag-row-date">${fmtMonthDay(s.ts_ms)}</span></span>
        <span class="diag-row-reason">${escapeHtml(s.reason)}</span>
        <span class="diag-row-cost">${Math.round(s.total_cost_ore)} öre · ${s.horizon_slots} slots</span>
      </button>`;
    }).join('');
    el.innerHTML = rows;
    el.querySelectorAll('.diag-row').forEach(b => {
      b.addEventListener('click', () => {
        const ts = Number(b.dataset.ts);
        location.hash = '#diagnose/' + ts;
      });
    });
    const activeRow = el.querySelector('.diag-row.active');
    if (activeRow) activeRow.scrollIntoView({ block: 'nearest' });
  }

  function reasonClass(r) {
    if (r === 'scheduled') return 'scheduled';
    if (r === 'reactive-pv') return 'reactive-pv';
    if (r === 'reactive-load') return 'reactive-load';
    if (r === 'manual') return 'manual';
    return 'unknown';
  }

  // ---- Detail fetch + render ----
  async function loadDetail(tsMs) {
    state.selectedTs = tsMs;
    const el = document.getElementById('diagnose-detail');
    if (el) el.innerHTML = '<div class="diagnose-empty">Loading snapshot…</div>';
    renderTimeline();  // refresh "active" class
    try {
      const r = await apiFetch('/api/mpc/diagnose/at?ts=' + tsMs);
      const j = await r.json();
      // Discard stale responses: if the user clicked a different
      // snapshot while this fetch was in flight, state.selectedTs
      // has moved on. Rendering here would flash the old detail
      // on top of the user's newer click.
      if (state.selectedTs !== tsMs) return;
      if (!j || !j.snapshot) {
        if (el) el.innerHTML = '<div class="diagnose-empty">Snapshot not found.</div>';
        return;
      }
      state.detail = j.snapshot;
      renderDetail();
    } catch (e) {
      if (state.selectedTs !== tsMs) return;
      if (el) el.innerHTML = `<div class="diagnose-empty">Error: ${escapeHtml(e.message)}</div>`;
    }
  }

  function renderDetail() {
    const s = state.detail;
    if (!s) return;
    const d = s.diagnostic;
    const el = document.getElementById('diagnose-detail');
    if (!el) return;
    // Header metadata
    const ageMin = Math.round((Date.now() - s.ts_ms) / 60000);
    const params = d.params || {};
    // Show EV columns whenever the snapshot tracked EV state — either
    // because the DP chose to charge (loadpoint_w > 0) OR because a
    // loadpoint was plugged in so ev_soc is meaningful even when
    // charging = 0. Otherwise we'd hide the columns on snapshots
    // where the plan said "idle the EV this hour" and operators
    // would never see the SoC trajectory.
    const lpActive = d.slots && d.slots.some(x =>
      x.loadpoint_w || x.loadpoint_soc_pct);
    const lpBadges = lpActive ? [
      '<span class="diag-pill diag-ev">EV in plan</span>',
      params.loadpoint_surplus_only ? '<span class="diag-pill diag-lp-policy">surplus only</span>' : '',
      params.loadpoint_blocks_battery_to_ev
        ? '<span class="diag-pill diag-lp-policy" title="Home battery discharge cannot satisfy planned EV charging in this plan.">battery to EV blocked</span>'
        : '<span class="diag-pill diag-lp-warn" title="This plan may let home battery discharge satisfy EV charging.">battery may cover EV</span>'
    ].join('') : '';
    const header = `
      <div class="diagnose-detail-header">
        <div>
          <div class="detail-title">Plan @ ${fmtDateTime(s.ts_ms)}</div>
          <div class="detail-sub">
            <span class="diag-reason-${reasonClass(s.reason)} diag-pill">${escapeHtml(s.reason)}</span>
            zone <b>${escapeHtml(s.zone)}</b> ·
            ${s.horizon_slots} slots ·
            expected ${Math.round(s.total_cost_ore)} öre ·
            ${ageMin} min ago
          </div>
        </div>
        <div class="detail-params">
          <span title="Mode"><b>${escapeHtml(params.mode || '—')}</b></span>
          <span title="Initial SoC">SoC start ${params.initial_soc_pct != null ? params.initial_soc_pct.toFixed(1) : '—'}%</span>
          <span title="Battery capacity">${params.capacity_wh ? (params.capacity_wh/1000).toFixed(1)+' kWh' : ''}</span>
          ${lpBadges}
        </div>
      </div>
      <div class="diagnose-chart-wrap">
        <canvas id="diag-chart" height="320"></canvas>
        <div id="diag-chart-highlight" class="diag-chart-highlight hidden"></div>
      </div>
      <div class="diagnose-table-wrap">
        <table class="diag-table">
          <thead>
            <tr>
              <th>#</th><th>Time</th>
              <th>Price</th><th>Spot</th><th>Conf</th>
              <th>PV</th><th>Load</th>
              <th>Battery</th><th>Grid</th><th>SoC end</th>
              ${lpActive ? '<th>EV W</th><th>EV SoC</th>' : ''}
              <th>Cost</th><th>Reason</th>
            </tr>
          </thead>
          <tbody>
            ${(d.slots || []).map((sl, i) => slotRow(sl, i, lpActive)).join('')}
          </tbody>
        </table>
      </div>
    `;
    el.innerHTML = header;
    drawChart(d);
    bindRowHover(el);
    bindChartHover(el);
  }

  function bindRowHover(root) {
    const rows = root.querySelectorAll('.diag-table tbody tr');
    rows.forEach(tr => {
      tr.addEventListener('mouseenter', () => {
        const i = Number(tr.dataset.slotIdx);
        showChartHighlight(i);
      });
      tr.addEventListener('mouseleave', hideChartHighlight);
    });
  }

  function bindChartHover(root) {
    const canvas = root.querySelector('#diag-chart');
    if (!canvas) return;
    canvas.addEventListener('mousemove', onChartMove);
    canvas.addEventListener('mouseleave', onChartLeave);
  }

  function onChartMove(e) {
    const g = state.chartGeom;
    if (!g) return;
    const canvas = e.currentTarget;
    const rect = canvas.getBoundingClientRect();
    // Scale from client px to the CSS px coords captured at draw time,
    // in case the canvas was resized after drawChart.
    const scale = rect.width > 0 ? (canvas.clientWidth / rect.width) : 1;
    const xCss = (e.clientX - rect.left) * scale;
    const xInPlot = xCss - g.padL;
    if (xInPlot < 0 || xInPlot >= g.barW * g.nSlots) {
      onChartLeave();
      return;
    }
    const i = Math.floor(xInPlot / g.barW);
    if (i === state.hoverSlotIdx) return;
    state.hoverSlotIdx = i;
    showChartHighlight(i);
    applyRowFilter(i);
  }

  function onChartLeave() {
    if (state.hoverSlotIdx == null) return;
    state.hoverSlotIdx = null;
    hideChartHighlight();
    clearRowFilter();
  }

  function applyRowFilter(center) {
    const rows = document.querySelectorAll('.diag-table tbody tr');
    const window = 6;
    rows.forEach((tr, idx) => {
      const inWindow = Math.abs(idx - center) <= window;
      tr.classList.toggle('diag-row-hidden', !inWindow);
      tr.classList.toggle('diag-row-hover', idx === center);
    });
  }

  function clearRowFilter() {
    document.querySelectorAll('.diag-table tbody tr').forEach(tr => {
      tr.classList.remove('diag-row-hidden', 'diag-row-hover');
    });
  }

  function showChartHighlight(i) {
    const g = state.chartGeom;
    const hi = document.getElementById('diag-chart-highlight');
    const canvas = document.getElementById('diag-chart');
    if (!g || !hi || !canvas || i < 0 || i >= g.nSlots) return;
    const offX = canvas.offsetLeft;
    const offY = canvas.offsetTop;
    hi.style.left = (offX + g.padL + i * g.barW) + 'px';
    hi.style.top = (offY + g.padT) + 'px';
    hi.style.width = Math.max(2, g.barW) + 'px';
    hi.style.height = g.plotH + 'px';
    hi.classList.remove('hidden');
  }

  function hideChartHighlight() {
    const hi = document.getElementById('diag-chart-highlight');
    if (hi) hi.classList.add('hidden');
  }

  function slotRow(sl, i, lpActive) {
    const conf = sl.confidence != null ? sl.confidence.toFixed(2) : '—';
    const confCls = sl.confidence < 0.9 ? 'conf-low' : '';
    const gridCls = sl.grid_w > 0 ? 'val-import' : (sl.grid_w < 0 ? 'val-export' : 'val-neutral');
    const batCls = sl.battery_w > 0 ? 'val-charging' : (sl.battery_w < 0 ? 'val-discharging' : 'val-neutral');
    return `<tr data-slot-idx="${i}">
      <td>${sl.idx}</td>
      <td>${fmtHHMM(sl.slot_start_ms)}</td>
      <td>${fmt1(sl.price_ore)}</td>
      <td>${fmt1(sl.spot_ore)}</td>
      <td class="${confCls}">${conf}</td>
      <td class="val-generation">${fmtW(sl.pv_w)}</td>
      <td>${fmtW(sl.load_w)}</td>
      <td class="${batCls}">${fmtW(sl.battery_w)}</td>
      <td class="${gridCls}">${fmtW(sl.grid_w)}</td>
      <td>${fmt1(sl.soc_pct)}%</td>
      ${lpActive ? `<td>${fmtW(sl.loadpoint_w || 0)}</td><td>${fmt1(sl.loadpoint_soc_pct || 0)}%</td>` : ''}
      <td>${fmt1(sl.cost_ore)}</td>
      <td class="diag-reason-cell">${escapeHtml(sl.reason || '')}</td>
    </tr>`;
  }

  // ---- Chart: stacked bands for price / power / SoC ----
  function drawChart(d) {
    const canvas = document.getElementById('diag-chart');
    if (!canvas || !d.slots || d.slots.length === 0) return;
    const dpr = window.devicePixelRatio || 1;
    const cssW = canvas.clientWidth || 800;
    const cssH = 320;
    canvas.width = cssW * dpr;
    canvas.height = cssH * dpr;
    canvas.style.width = cssW + 'px';
    canvas.style.height = cssH + 'px';
    const ctx = canvas.getContext('2d');
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, cssW, cssH);
    const C = canvasColors();

    const pad = { t: 16, r: 40, b: 24, l: 44 };
    const plotW = cssW - pad.l - pad.r;
    const plotH = cssH - pad.t - pad.b;

    const slots = d.slots;
    const nSlots = slots.length;
    const barW = Math.max(1, plotW / nSlots);

    // Geometry for the row-hover highlight overlay.
    state.chartGeom = { padL: pad.l, padT: pad.t, barW, plotH, nSlots };

    // Three horizontal bands: top third = price, middle third = power, bottom third = SoC
    const priceH = plotH * 0.30;
    const powerH = plotH * 0.45;
    const socH = plotH * 0.25;
    const priceY0 = pad.t;
    const powerY0 = pad.t + priceH;
    const socY0 = pad.t + priceH + powerH;

    // Price scale
    let maxPrice = 1;
    slots.forEach(s => { if (s.price_ore > maxPrice) maxPrice = s.price_ore; });
    // Power scale — symmetric around 0
    let maxAbsPower = 1;
    slots.forEach(s => {
      [s.grid_w || 0, s.battery_w || 0, s.pv_w || 0, s.load_w || 0, s.loadpoint_w || 0]
        .forEach(v => { if (Math.abs(v) > maxAbsPower) maxAbsPower = Math.abs(v); });
    });

    // Background bands
    ctx.save();
    ctx.globalAlpha = 0.03;
    ctx.fillStyle = C.text;
    ctx.fillRect(pad.l, priceY0, plotW, priceH);
    ctx.fillRect(pad.l, socY0, plotW, socH);
    ctx.restore();

    // Price bars — green cheap, red expensive (relative to horizon mean)
    const priceMean = slots.reduce((a, s) => a + s.price_ore, 0) / nSlots;
    slots.forEach((s, i) => {
      const x = pad.l + i * barW;
      const h = (s.price_ore / maxPrice) * priceH;
      const y = priceY0 + priceH - h;
      // Cheap slots (below mean) in green, expensive in red, low-confidence dimmed
      const alpha = s.confidence < 0.9 ? 0.35 : 0.75;
      ctx.fillStyle = s.price_ore < priceMean
        ? `rgba(34,197,94,${alpha})`
        : `rgba(239,68,68,${alpha})`;
      ctx.fillRect(x, y, barW, h);
    });

    // Power zero line
    const powerMidY = powerY0 + powerH / 2;
    ctx.strokeStyle = C.line;
    ctx.lineWidth = 1;
    ctx.beginPath();
    ctx.moveTo(pad.l, powerMidY);
    ctx.lineTo(pad.l + plotW, powerMidY);
    ctx.stroke();

    // Battery + EV (stacked positive = load on site, negative = source)
    const powerScale = (powerH / 2) / maxAbsPower;
    slots.forEach((s, i) => {
      const x = pad.l + i * barW;
      // Battery — orange charge up, purple discharge down
      const bw = s.battery_w || 0;
      if (bw !== 0) {
        const h = bw * powerScale;
        ctx.fillStyle = bw > 0 ? 'rgba(245,158,11,0.75)' : 'rgba(139,92,246,0.75)';
        ctx.fillRect(x, powerMidY - h, barW, h);
      }
      // EV — stacked atop battery when both charging
      const ew = s.loadpoint_w || 0;
      if (ew > 0) {
        const bh = (bw > 0 ? bw : 0) * powerScale;
        const h = ew * powerScale;
        ctx.fillStyle = 'rgba(6,182,212,0.75)';
        ctx.fillRect(x, powerMidY - bh - h, barW, h);
      }
    });

    // Grid line — overlay
    ctx.strokeStyle = '#ef4444';
    ctx.lineWidth = 1.5;
    ctx.beginPath();
    slots.forEach((s, i) => {
      const x = pad.l + i * barW + barW / 2;
      const y = powerMidY - (s.grid_w || 0) * powerScale;
      if (i === 0) ctx.moveTo(x, y);
      else ctx.lineTo(x, y);
    });
    ctx.stroke();

    // PV line — green, negative is pulled up visually (PV is −W site sign)
    ctx.strokeStyle = '#22c55e';
    ctx.lineWidth = 1;
    ctx.beginPath();
    slots.forEach((s, i) => {
      const x = pad.l + i * barW + barW / 2;
      const y = powerMidY - (s.pv_w || 0) * powerScale;
      if (i === 0) ctx.moveTo(x, y);
      else ctx.lineTo(x, y);
    });
    ctx.stroke();

    // SoC line — blue, in bottom band
    ctx.strokeStyle = '#60a5fa';
    ctx.lineWidth = 2;
    ctx.beginPath();
    slots.forEach((s, i) => {
      const x = pad.l + i * barW + barW / 2;
      const y = socY0 + socH - ((s.soc_pct || 0) / 100) * socH;
      if (i === 0) ctx.moveTo(x, y);
      else ctx.lineTo(x, y);
    });
    ctx.stroke();

    // Y-axis labels
    ctx.fillStyle = C.dim;
    ctx.font = '10px system-ui, -apple-system, sans-serif';
    ctx.textAlign = 'right';
    ctx.fillText(maxPrice.toFixed(0) + 'ö', pad.l - 4, priceY0 + 10);
    ctx.fillText('0', pad.l - 4, priceY0 + priceH - 2);
    ctx.fillText('+' + (maxAbsPower / 1000).toFixed(1) + 'kW', pad.l - 4, powerY0 + 10);
    ctx.fillText('0', pad.l - 4, powerMidY + 3);
    ctx.fillText('−' + (maxAbsPower / 1000).toFixed(1) + 'kW', pad.l - 4, powerY0 + powerH - 2);
    ctx.fillText('100%', pad.l - 4, socY0 + 10);
    ctx.fillText('0', pad.l - 4, socY0 + socH - 2);

    // X-axis time labels — every 6th slot
    ctx.textAlign = 'center';
    for (let i = 0; i < nSlots; i += Math.max(1, Math.floor(nSlots / 8))) {
      const x = pad.l + i * barW + barW / 2;
      ctx.fillText(fmtHHMM(slots[i].slot_start_ms), x, cssH - 6);
    }
  }

  // ---- Helpers ----
  function fmtHHMM(ts) {
    const d = new Date(ts);
    return d.getHours().toString().padStart(2, '0') + ':' +
      d.getMinutes().toString().padStart(2, '0');
  }
  function fmtMonthDay(ts) {
    const d = new Date(ts);
    return (d.getMonth() + 1) + '/' + d.getDate();
  }
  function fmtDateTime(ts) {
    const d = new Date(ts);
    return d.toLocaleString();
  }
  function fmt1(n) {
    if (n == null) return '—';
    return n.toFixed(1);
  }
  function fmtW(w) {
    if (w == null) return '—';
    if (Math.abs(w) >= 1000) return (w / 1000).toFixed(2) + ' kW';
    return w.toFixed(0) + ' W';
  }
  function escapeHtml(s) {
    return String(s).replace(/[&<>"']/g, c => ({
      '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
    })[c]);
  }

  // ---- Boot ----
  // Build the destination layout before first paint and keep deep links.
  window.addEventListener('ftw-theme-change', function () {
    if (state.detail) drawChart(state.detail);
  });
  organizeDestinations();
  applyHash();
})();
