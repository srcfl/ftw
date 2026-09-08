// twins.js — advanced-mode forecast-learning diagnostics.
// Polls only while advanced mode is visible. The box owns the model state;
// this view only asks it to begin a new learning period for one signal.

(function () {
  'use strict';

  const REFRESH_MS = 10000;
  const actions = {
    '/api/pvmodel/reset': { signal: 'solar production', other: 'consumption', label: 'Relearn solar production', id: 'pv' },
    '/api/loadmodel/reset': { signal: 'consumption', other: 'solar production', label: 'Relearn consumption', id: 'load' },
  };
  let refreshTimer = null;
  let refreshRevision = 0;
  let lastPV = null;
  let lastLoad = null;
  const pending = new Set();
  const availableActions = new Set();
  const actionMessages = new Map();
  const actionFocus = new Set();

  function apiFetch(path, opts) { return fetch(path, opts); }

  async function fetchModel(path) {
    const response = await apiFetch(path);
    if (!response.ok) throw new Error('HTTP ' + response.status);
    return response.json();
  }

  async function fetchAll() {
    const revision = ++refreshRevision;
    const [pv, load] = await Promise.all([
      fetchModel('/api/pvmodel').catch(() => ({ unavailable: true })),
      fetchModel('/api/loadmodel').catch(() => ({ unavailable: true })),
    ]);
    if (revision !== refreshRevision) return;
    lastPV = pv;
    lastLoad = load;
    render(pv, load);
  }

  function advancedVisible() { return !!(document.body && document.body.classList.contains('advanced')); }
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
    if (advancedVisible() && !document.hidden) startPolling();
    else stopPolling();
  }

  function fmtAge(ms) {
    if (!ms) return '—';
    const s = Math.round((Date.now() - ms) / 1000);
    if (s < 60) return s + 's ago';
    if (s < 3600) return Math.round(s / 60) + 'm ago';
    return Math.round(s / 3600) + 'h ago';
  }
  function fmtLocalTime(ms) {
    if (!ms || !Number.isFinite(Number(ms))) return '—';
    const date = new Date(Number(ms));
    return Number.isNaN(date.getTime()) ? '—' : date.toLocaleString();
  }
  function esc(value) {
    return String(value == null ? '' : value).replace(/&/g, '&amp;').replace(/</g, '&lt;')
      .replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }
  function learningStatus(learning) {
    if (!learning) return 'Model state unavailable';
    switch (learning.status) {
      case 'cold_start': return 'Cold start';
      case 'learning': return 'Learning';
      case 'ready': return 'Ready';
      case 'unavailable': return 'Unavailable';
      default: return 'Model state unavailable';
    }
  }
  function engineLabel(learning) {
    if (!learning) return 'Unavailable';
    return learning.engine === 'energyplan' ? 'Energyplan' : learning.engine === 'legacy' ? 'Legacy' : 'Unavailable';
  }
  function actionAvailable(d) {
    const learning = d && d.learning;
    return !!(d && d.enabled && learning && learning.reset_available === true);
  }

  function resetButton(endpoint, d) {
    const action = actions[endpoint];
    const isPending = pending.has(endpoint);
    const enabled = actionAvailable(d);
    const statusID = 'twin-status-' + action.id;
    if (enabled) availableActions.add(endpoint);
    const unavailable = !enabled;
    const label = isPending ? 'Starting new learning period…' : action.label;
    return `<button type="button" class="btn-reset-model" data-reset-twin="${endpoint}" aria-describedby="${statusID}" aria-busy="${isPending ? 'true' : 'false'}" ${unavailable || isPending ? 'disabled aria-disabled="true"' : ''}>${label}</button>`;
  }

  function loadProfileControl(d) {
    if (!d || !d.enabled) return '';
    const active = d.profile || d.active_profile || 'home';
    function btn(profile, label) {
      const cls = profile === active ? ' class="active"' : '';
      return `<button type="button" data-loadmodel-profile="${profile}"${cls}>${label}</button>`;
    }
    return '<div class="twin-row twin-profile-row"><span>profile</span>' +
      `<div class="twin-profile-toggle" role="tablist" data-active="${active}">` + btn('home', 'Home') + btn('away', 'Away') + '</div></div>';
  }

  function modelRows(d) {
    const learning = d.learning;
    const legacy = learning && learning.engine === 'energyplan';
    const prefix = legacy ? 'legacy ' : '';
    const rows = [];
    if (legacy) rows.push('<div class="twin-row"><span>legacy model stats</span><b>secondary</b></div>');
    rows.push(`<div class="twin-row"><span>${prefix}samples</span><b>${d.samples || 0}</b></div>`);
    if (d.mae_w != null) rows.push(`<div class="twin-row"><span>${prefix}MAE</span><b>${d.mae_w.toFixed(0)} W</b></div>`);
    if (d.peak_w != null) rows.push(`<div class="twin-row"><span>${prefix}peak ref</span><b>${(d.peak_w / 1000).toFixed(1)} kW</b></div>`);
    if (d.rated_w != null) rows.push(`<div class="twin-row"><span>${prefix}rated</span><b>${(d.rated_w / 1000).toFixed(1)} kW</b></div>`);
    if (d.heating_w_per_degc != null && d.heating_w_per_degc > 0) rows.push(`<div class="twin-row"><span>${prefix}heating</span><b>${d.heating_w_per_degc.toFixed(0)} W/°C</b></div>`);
    if (d.buckets_warm != null) rows.push(`<div class="twin-row"><span>${prefix}buckets warm</span><b>${d.buckets_warm}/${d.buckets_total}</b></div>`);
    rows.push(`<div class="twin-row"><span>${prefix}last update</span><b>${fmtAge(d.last_ms)}</b></div>`);
    if (d.quality != null) {
      const quality = Math.max(0, Math.min(1, d.quality));
      const qualityPct = (quality * 100).toFixed(0);
      const qualityColor = quality >= 0.7 ? '#22c55e' : quality >= 0.3 ? '#fbbf24' : '#ef4444';
      rows.push(`<div class="twin-row"><span>${prefix}quality</span><b>${qualityPct}%</b></div>`);
      rows.push(`<div class="twin-quality"><div class="twin-quality-fill" style="width:${qualityPct}%;background:${qualityColor}"></div></div>`);
    }
    return rows.join('');
  }

  function twinCard(title, d, endpoint, extraHtml) {
    const action = actions[endpoint];
    const statusID = 'twin-status-' + action.id;
    if (!d || d.unavailable) return `<div class="twin-card"><h3>${title}</h3><div class="twin-row"><span>model state</span><b>Unavailable</b></div><div id="${statusID}" role="status" aria-live="polite">The box did not provide this model state.</div></div>`;
    if (!d.enabled) return `<div class="twin-card"><h3>${title}</h3><div class="twin-row"><span>model state</span><b>Disabled</b></div><div id="${statusID}" role="status" aria-live="polite">This model is disabled on the box.</div></div>`;
    const learning = d.learning;
    const message = actionMessages.get(endpoint) || (!actionAvailable(d) ? 'Relearning is unavailable for this model.' : '');
    const learningRows = [
      `<div class="twin-row"><span>engine</span><b>${engineLabel(learning)}</b></div>`,
      `<div class="twin-row"><span>learning state</span><b>${learningStatus(learning)}</b></div>`,
    ];
    if (learning) {
      learningRows.push(`<div class="twin-row"><span>learning started</span><b>${fmtLocalTime(learning.started_ms)}</b></div>`);
      learningRows.push(`<div class="twin-row"><span>latest training</span><b>${fmtLocalTime(learning.latest_training_ms)}</b></div>`);
    }
    return `<div class="twin-card"><h3>${title}</h3>${extraHtml || ''}${learningRows.join('')}${modelRows(d)}${resetButton(endpoint, d)}<div id="${statusID}" role="status" aria-live="polite">${esc(message)}</div></div>`;
  }

  function focusedAction() {
    const active = document.activeElement;
    return active && active.dataset && actions[active.dataset.resetTwin] ? active.dataset.resetTwin : null;
  }
  function focusedControl() {
    const active = document.activeElement;
    if (!active || !active.dataset) return null;
    if (actions[active.dataset.resetTwin]) return `[data-reset-twin="${active.dataset.resetTwin}"]`;
    if (active.dataset.loadmodelProfile) return `[data-loadmodel-profile="${active.dataset.loadmodelProfile}"]`;
    return null;
  }
  function focusMayReturn() {
    const active = document.activeElement;
    return !active || active === document.body || active === document.documentElement;
  }
  function render(pv, load, restoreFocus) {
    const grid = document.getElementById('twins-grid');
    if (!grid) return;
    const selector = restoreFocus || focusedControl();
    availableActions.clear();
    grid.innerHTML = twinCard('Solar production', pv, '/api/pvmodel/reset') + twinCard('Consumption', load, '/api/loadmodel/reset', loadProfileControl(load));
    if (selector && grid.querySelector) {
      const button = grid.querySelector(selector);
      if (button && !button.disabled && typeof button.focus === 'function') button.focus();
    }
    const sub = document.getElementById('twins-subtitle');
    if (sub) sub.textContent = 'Forecast learning for solar production and consumption';
  }

  async function failedResponse(response) {
    let body = null;
    try {
      body = await response.json();
    } catch (_) { /* A non-JSON error still has an HTTP status worth showing. */ }
    const detail = body && (body.error || body.message) ? ': ' + (body.error || body.message) : '';
    return { body, error: new Error('The box did not accept this request (HTTP ' + response.status + ')' + detail) };
  }
  async function startRelearn(endpoint) {
    const action = actions[endpoint];
    if (!action || pending.has(endpoint) || !availableActions.has(endpoint)) return;
    if (!confirm(`Start a new learning period for ${action.signal}?\n\nThis keeps measured history and the ${action.other} model. Forecast confidence will be lower while ${action.signal} learns.`)) return;
    if (focusedAction() === endpoint) actionFocus.add(endpoint);
    pending.add(endpoint);
    actionMessages.set(endpoint, 'Requesting a new learning period…');
    render(lastPV, lastLoad);
    try {
      const response = await apiFetch(endpoint, { method: 'POST' });
      if (!response.ok) {
        const failure = await failedResponse(response);
        if (response.status === 503 && failure.body && failure.body.status === 'pending') {
          actionMessages.set(endpoint, failure.body.error || 'Learning period saved; model restart pending.');
          await fetchAll();
          return;
        }
        throw failure.error;
      }
      actionMessages.set(endpoint, 'The box accepted the request. The learning state above reports its progress.');
      await fetchAll();
    } catch (err) {
      const cancelled = err && err.name === 'AbortError';
      actionMessages.set(endpoint, cancelled ? 'The request was cancelled. The box did not confirm a new learning period.' : 'The box did not confirm a new learning period: ' + ((err && err.message) || 'request failed'));
    } finally {
      pending.delete(endpoint);
      const restoreFocus = actionFocus.has(endpoint) && focusMayReturn()
        ? `[data-reset-twin="${endpoint}"]` : null;
      actionFocus.delete(endpoint);
      render(lastPV, lastLoad, restoreFocus);
    }
  }

  function onGridClick(e) {
    const profile = e.target && e.target.dataset && e.target.dataset.loadmodelProfile;
    if (profile) {
      if (e.target.classList.contains('active')) return;
      apiFetch('/api/loadmodel/profile', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ profile }) })
        .then(r => { if (!r.ok) throw new Error('HTTP ' + r.status); return r.json(); })
        .then(() => fetchAll())
        .catch(err => { actionMessages.set('/api/loadmodel/reset', 'Load profile switch failed: ' + err.message); render(lastPV, lastLoad); });
      return;
    }
    const endpoint = e.target && e.target.dataset && e.target.dataset.resetTwin;
    if (endpoint) startRelearn(endpoint);
  }
  function init() {
    const grid = document.getElementById('twins-grid');
    if (grid) grid.addEventListener('click', onGridClick);
    document.addEventListener('ftw-ui-mode-change', syncPolling);
    document.addEventListener('visibilitychange', syncPolling);
    syncPolling();
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
  else init();
})();
