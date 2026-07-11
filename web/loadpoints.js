// loadpoints.js — advanced-mode panel: per-loadpoint configuration
// summary + the planner's per-slot charging schedule. Refreshes every
// 10 s only while advanced mode is visible; remote routes pay for every
// owner request over the P2P channel.
//
// Data sources:
//   GET /api/loadpoints  — array of LP states (id, driver, surplus_only,
//                          plugged_in, max_charge_w, allowed_steps_w,
//                          current_soc_pct, target_soc_pct, target_time,
//                          vehicle_*, soc_source).
//   GET /api/mpc/plan    — current plan; actions[] include optional
//                          loadpoint_w + loadpoint_soc_pct when an LP
//                          is part of the DP. Only one LP at a time
//                          today (mpc/service.go: "One loadpoint at a
//                          time — multi-LP support is on the roadmap").

(function () {
  'use strict';

  const REFRESH_MS = 10000;
  let refreshTimer = null;
  // While an inline SoC editor is open, render() skips re-rendering so the
  // poll doesn't clobber the input. Self-healing: cleared if the editor DOM
  // is gone (see render()).
  let socEditingId = null;
  // How many forward slots of the schedule to render. The plan is
  // 193 slots × 15 min = 48 h. 4 h was too narrow — operators looking
  // at "why does the plan chart show grid burst at 13:00 but my schedule
  // table doesn't list it?" were confused. 96 slots = 24 h covers the
  // typical overnight charging window plus the next afternoon.
  const SCHEDULE_SLOTS = 96;

  function ownerFetch(path, opts) {
    if (typeof window.ownerFetch === 'function') return window.ownerFetch(path, opts);
    return fetch(path, opts);
  }

  async function fetchAll() {
    const [lps, plan] = await Promise.all([
      ownerFetch('/api/loadpoints').then(r => r.json()).catch(() => ({ loadpoints: [] })),
      ownerFetch('/api/mpc/plan').then(r => r.json()).catch(() => null),
    ]);
    render(lps && lps.loadpoints ? lps.loadpoints : [], plan);
  }

  function fmtW(w) {
    if (w == null || !isFinite(w)) return '—';
    if (Math.abs(w) >= 1000) return (w / 1000).toFixed(2) + ' kW';
    return Math.round(w) + ' W';
  }

  function fmtPct(p) {
    if (p == null || !isFinite(p)) return '—';
    return p.toFixed(1) + '%';
  }

  function fmtPriceOre(o) {
    if (o == null || !isFinite(o)) return '—';
    return o.toFixed(0);
  }

  function fmtSlotTime(ms) {
    if (!ms) return '—';
    const d = new Date(ms);
    const hh = String(d.getHours()).padStart(2, '0');
    const mm = String(d.getMinutes()).padStart(2, '0');
    return `${hh}:${mm}`;
  }

  function fmtDeadline(iso) {
    if (!iso || iso.startsWith('0001-')) return null;
    const d = new Date(iso);
    if (isNaN(d.getTime())) return null;
    const hh = String(d.getHours()).padStart(2, '0');
    const mm = String(d.getMinutes()).padStart(2, '0');
    return `${d.toLocaleDateString()} ${hh}:${mm}`;
  }

  // Badge: small pill matching the existing .ftw-badge convention from
  // ftw-badge.js / next.css. Uses theme tokens so light mode flips
  // correctly. Two variants: "on" = amber accent (matches .accent-e
  // affordance), "off" = muted hairline.
  function badge(label, on) {
    const cls = on ? 'lp-badge lp-badge-on' : 'lp-badge';
    return `<span class="${cls}">${label}</span>`;
  }

  function configBlock(lp) {
    const d = fmtDeadline(lp.target_time);
    const target = (lp.target_soc_pct > 0)
      ? `${lp.target_soc_pct.toFixed(0)}%${d ? ' by ' + d : ''}`
      : 'opportunistic';
    const vehicle = (lp.vehicle_driver)
      ? `${lp.vehicle_driver}${lp.vehicle_charging_state ? ' · ' + lp.vehicle_charging_state : ''}${lp.vehicle_stale ? ' · stale' : ''}`
      : '—';
    // When soc_source is "vehicle", the BMS reading (vehicle_soc_pct)
    // is ground truth and what the operator expects to see — render
    // that. current_soc_pct stays as the controller's inference state
    // (the planner's input for stability across ticks) and is shown
    // in parens as "(inferred: 65.1%)" so the discrepancy is visible
    // when it exists. When soc_source is "inferred" the inference
    // value is the only one we have, so display it directly.
    let soc = '—';
    if (lp.soc_source === 'vehicle' && lp.vehicle_soc_pct != null) {
      soc = `${lp.vehicle_soc_pct.toFixed(0)}% (vehicle)`;
      if (lp.current_soc_pct != null &&
          Math.abs(lp.vehicle_soc_pct - lp.current_soc_pct) >= 1) {
        soc += ` · inferred ${lp.current_soc_pct.toFixed(1)}%`;
      }
    } else if (lp.current_soc_pct != null) {
      soc = `${lp.current_soc_pct.toFixed(1)}%${lp.soc_source ? ' (' + lp.soc_source + ')' : ''}`;
    }
    // Inline manual SoC correction. The backend (POST /api/loadpoints/{id}/soc)
    // re-anchors the inferred SoC, so it only works during an active session —
    // show the ✎ affordance only when plugged in.
    let socCell = soc;
    if (lp.plugged_in) {
      const cur = (lp.current_soc_pct != null) ? lp.current_soc_pct.toFixed(1) : '';
      socCell = `${soc} <button class="lp-soc-edit" type="button" data-lp="${lp.id}" data-cur="${cur}" title="Set SoC manually" ` +
        `style="background:none;border:none;cursor:pointer;color:var(--accent-e);font-size:0.9em;padding:0 4px">✎</button>`;
    }
    const rows = [
      ['Driver',       lp.driver_name || '—'],
      ['Plugged in',   badge(lp.plugged_in ? 'YES' : 'NO', lp.plugged_in)],
      ['Surplus only', badge(lp.surplus_only ? 'ON' : 'OFF', lp.surplus_only)],
      ['Live power',   fmtW(lp.current_power_w)],
      ['Max',          fmtW(lp.max_charge_w)],
      ['Min',          fmtW(lp.min_charge_w)],
      ['Target',       target],
      ['Vehicle',      vehicle],
      ['SoC',          socCell],
    ];
    const html = rows.map(([k, v]) =>
      `<div class="lp-cfg-row"><span class="lp-cfg-key">${k}</span><span class="lp-cfg-val">${v}</span></div>`
    ).join('');
    return `<div class="lp-cfg">${html}</div>`;
  }

  function scheduleTable(lp, plan) {
    if (!plan || !plan.plan || !Array.isArray(plan.plan.actions)) {
      return '<div class="lp-empty">No active plan.</div>';
    }
    // Plan only carries one LP today. Match by checking whether *any*
    // action has a loadpoint_w / loadpoint_soc_pct field — if it does,
    // assume those refer to this LP. Once multi-LP support lands the
    // plan will need to namespace these by LP id; for now the comment
    // in mpc/service.go is the source of truth.
    const planLpId = plan.plan.loadpoint_id || null;
    const isOurs = !planLpId || planLpId === lp.id;
    if (!isOurs) {
      return `<div class="lp-empty">Plan is scheduling <code>${planLpId}</code>, not this loadpoint.</div>`;
    }
    const slots = plan.plan.actions
      .filter(a => a.loadpoint_w != null || a.loadpoint_soc_pct != null)
      .slice(0, SCHEDULE_SLOTS);
    if (slots.length === 0) {
      return '<div class="lp-empty">Planner did not allocate this loadpoint in the current horizon ' +
             '(no target SoC + no surplus_only opportunistic slots).</div>';
    }
    const rows = slots.map(a => {
      const charging = a.loadpoint_w && a.loadpoint_w > 50;
      const cls = charging ? 'lp-row-charging' : '';
      return `<tr class="${cls}">` +
        `<td>${fmtSlotTime(a.slot_start_ms)}</td>` +
        `<td>${fmtPriceOre(a.price_ore)}</td>` +
        `<td>${fmtW(a.loadpoint_w || 0)}</td>` +
        `<td>${fmtPct(a.loadpoint_soc_pct)}</td>` +
        `<td>${fmtW(a.battery_w)}</td>` +
        `<td>${a.reason || ''}</td>` +
        '</tr>';
    }).join('');
    return '<div class="lp-schedule-wrap">' +
      '<table class="diag-table lp-schedule">' +
      '<thead><tr>' +
        '<th>Slot</th>' +
        '<th>Price (öre)</th>' +
        '<th>EV W</th>' +
        '<th>EV SoC</th>' +
        '<th>Battery W</th>' +
        '<th>Reason</th>' +
      '</tr></thead>' +
      `<tbody>${rows}</tbody>` +
      '</table></div>';
  }

  function loadpointCard(lp, plan) {
    return `<div class="lp-card" data-lp-id="${lp.id}">` +
      `<div class="lp-card-header"><h3>${lp.id}</h3></div>` +
      '<div class="lp-card-body">' +
      configBlock(lp) +
      scheduleTable(lp, plan) +
      '</div></div>';
  }

  function render(lps, plan) {
    const grid = document.getElementById('loadpoints-grid');
    if (!grid) return;

    // Don't clobber an open inline SoC editor. If the editor DOM is gone
    // (mode toggled, card removed), resume normal rendering.
    if (socEditingId) {
      if (grid.querySelector('.lp-soc-input')) return;
      socEditingId = null;
    }

    // Capture scroll positions BEFORE swapping innerHTML — otherwise the
    // 5 s auto-refresh yanks the page (and any per-card schedule scroll)
    // back to the top mid-read. Page scroll comes from the document's
    // scrolling element; per-LP scrolls come from each `.lp-schedule-wrap`
    // keyed by the card's data-lp-id so we restore to the right card even
    // if LP order or count changes.
    const scroller = document.scrollingElement || document.documentElement;
    const pageScroll = { top: scroller.scrollTop, left: scroller.scrollLeft };
    const wrapScrolls = {};
    grid.querySelectorAll('.lp-card[data-lp-id]').forEach(card => {
      const id = card.getAttribute('data-lp-id');
      const wrap = card.querySelector('.lp-schedule-wrap');
      if (id && wrap) {
        wrapScrolls[id] = { top: wrap.scrollTop, left: wrap.scrollLeft };
      }
    });

    if (!lps || lps.length === 0) {
      grid.innerHTML = '<div class="lp-empty">No loadpoints configured.</div>';
    } else {
      grid.innerHTML = lps.map(lp => loadpointCard(lp, plan)).join('');
    }

    // Restore per-LP wrap scrolls first, then the page. Restoring the
    // page last avoids browsers re-anchoring the page scroll to the
    // freshly-laid-out content height.
    grid.querySelectorAll('.lp-card[data-lp-id]').forEach(card => {
      const id = card.getAttribute('data-lp-id');
      const prev = wrapScrolls[id];
      const wrap = card.querySelector('.lp-schedule-wrap');
      if (prev && wrap) {
        wrap.scrollTop = prev.top;
        wrap.scrollLeft = prev.left;
      }
    });
    scroller.scrollTop = pageScroll.top;
    scroller.scrollLeft = pageScroll.left;

    const sub = document.getElementById('loadpoints-subtitle');
    if (sub) {
      const planMode = plan && plan.plan ? plan.plan.mode : 'no plan';
      sub.textContent = `${lps.length} loadpoint${lps.length === 1 ? '' : 's'} · planner: ${planMode}`;
    }
  }

  // ---- Inline manual SoC editing ----

  function openSocEditor(btn) {
    const cell = btn.closest('.lp-cfg-val');
    if (!cell) return;
    socEditingId = btn.dataset.lp;
    const cur = btn.dataset.cur || '';
    cell.innerHTML =
      `<input type="number" class="lp-soc-input" min="0" max="100" step="0.1" value="${cur}" ` +
      `style="width:64px;font-family:var(--mono)"> ` +
      `<button class="lp-soc-save" type="button" data-lp="${socEditingId}" title="Save">✓</button> ` +
      `<button class="lp-soc-cancel" type="button" title="Cancel">✗</button> ` +
      `<span class="lp-soc-msg" style="color:var(--red-e,#c23b3b);font-size:0.8em;margin-left:6px"></span>`;
    const inp = cell.querySelector('.lp-soc-input');
    if (inp) { inp.focus(); inp.select(); }
  }

  function saveSoc(btn) {
    const cell = btn.closest('.lp-cfg-val');
    const inp = cell && cell.querySelector('.lp-soc-input');
    const msg = cell && cell.querySelector('.lp-soc-msg');
    if (!inp) return;
    const val = parseFloat(inp.value);
    if (!isFinite(val) || val < 0 || val > 100) { if (msg) msg.textContent = '0–100 only'; return; }
    btn.disabled = true;
    ownerFetch('/api/loadpoints/' + encodeURIComponent(btn.dataset.lp) + '/soc', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ soc_pct: val }),
    })
      .then(r => r.json().then(j => ({ ok: r.ok, body: j })))
      .then(res => {
        if (res.ok && res.body && res.body.ok) { socEditingId = null; fetchAll(); }
        else { if (msg) msg.textContent = (res.body && res.body.error) || 'failed'; btn.disabled = false; }
      })
      .catch(e => { if (msg) msg.textContent = e.message; btn.disabled = false; });
  }

  function cancelSoc() { socEditingId = null; fetchAll(); }

  function onGridClick(e) {
    const edit = e.target.closest && e.target.closest('.lp-soc-edit');
    if (edit) { openSocEditor(edit); return; }
    const save = e.target.closest && e.target.closest('.lp-soc-save');
    if (save) { saveSoc(save); return; }
    const cancel = e.target.closest && e.target.closest('.lp-soc-cancel');
    if (cancel) { cancelSoc(); return; }
  }

  function onGridKey(e) {
    if (!e.target.classList || !e.target.classList.contains('lp-soc-input')) return;
    if (e.key === 'Enter') {
      e.preventDefault();
      const save = e.target.closest('.lp-cfg-val').querySelector('.lp-soc-save');
      if (save) saveSoc(save);
    } else if (e.key === 'Escape') {
      e.preventDefault();
      cancelSoc();
    }
  }

  function init() {
    const grid = document.getElementById('loadpoints-grid');
    if (grid) {
      grid.addEventListener('click', onGridClick);
      grid.addEventListener('keydown', onGridKey);
    }
    function advancedVisible() {
      return !!(document.body && document.body.classList.contains('advanced'));
    }
    function startPolling() {
      if (refreshTimer) return;
      fetchAll();
      refreshTimer = setInterval(fetchAll, REFRESH_MS);
    }
    function stopPolling() {
      if (!refreshTimer) return;
      clearInterval(refreshTimer);
      refreshTimer = null;
    }
    function syncPolling() {
      if (advancedVisible()) startPolling();
      else stopPolling();
    }
    document.addEventListener('ftw-ui-mode-change', syncPolling);
    syncPolling();
    if (window.ftwP2P && typeof window.ftwP2P.onState === 'function') {
      var waitingForDirect = false;
      window.ftwP2P.onState(function (s) {
        if (s !== 'direct') {
          waitingForDirect = true;
          return;
        }
        if (!waitingForDirect) return;
        waitingForDirect = false;
        if (advancedVisible()) fetchAll();
      });
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
