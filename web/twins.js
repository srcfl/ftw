// Forecast status with model diagnostics behind an explicit disclosure.
// The box owns learning state; elapsed time never establishes model quality.

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
  const expanded = new Set();

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

  function forecastsVisible() { return location.hash.split('/')[0] === '#more'; }
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
    if (forecastsVisible() && !document.hidden) startPolling();
    else stopPolling();
  }

  function fmtAge(ms) {
    if (!ms) return '—';
    const s = Math.max(0, Math.round((Date.now() - ms) / 1000));
    if (s < 60) return s + 's ago';
    if (s < 3600) return Math.round(s / 60) + 'm ago';
    if (s < 86400) return Math.round(s / 3600) + 'h ago';
    return Math.floor(s / 86400) + 'd ago';
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
    return learning.engine === 'energyplan' ? 'Energyplan' : learning.engine === 'legacy' ? 'Local model' : 'Unavailable';
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
    const active = d.profile || d.active_profile;
    function btn(profile, label) {
      const cls = profile === active ? ' class="active"' : '';
      return `<button type="button" data-loadmodel-profile="${profile}" aria-pressed="${profile === active}"${cls}>${label}</button>`;
    }
    return '<div class="twin-row twin-profile-row"><span>Household profile</span>' +
      '<div class="twin-profile-toggle" role="group" aria-label="Household profile">' + btn('home', 'Home') + btn('away', 'Away') + '</div></div>';
  }

  function row(label, value) {
    return `<div class="twin-row"><span>${esc(label)}</span><b>${esc(value)}</b></div>`;
  }
  function watts(value) { return Number.isFinite(value) ? Math.round(value).toLocaleString() + ' W' : 'Not available'; }

  function modelRows(d) {
    const older = d.learning && d.learning.engine === 'energyplan';
    const rows = [row('Training samples', d.samples ?? 'Not available')];
    if (d.mae_w != null) rows.push(row('Average training error', watts(d.mae_w)));
    if (Number.isFinite(d.peak_w)) rows.push(row('Load reference', (d.peak_w / 1000).toFixed(1) + ' kW'));
    if (Number.isFinite(d.rated_w)) rows.push(row('Solar starting estimate', (d.rated_w / 1000).toFixed(1) + ' kW'));
    if (d.heating_w_per_degc > 0) rows.push(row('Heating estimate below 18 °C', Math.round(d.heating_w_per_degc) + ' W/°C'));
    if (d.buckets_warm != null) rows.push(row('Weekly hours with 8+ samples', d.buckets_warm + '/' + d.buckets_total));
    rows.push(row('Last model update', fmtLocalTime(d.last_ms)));
    return `<h4>${older ? 'Older local model' : 'Local model diagnostics'}</h4>` +
      `<p class="forecast-note">${older ? 'Kept for comparison and fallback. These figures describe the older model, not Energyplan.' : 'These figures describe the model’s training data.'} Training error is not measured forecast accuracy.</p>` + rows.join('');
  }

  function overview(d, signal) {
    if (!d || d.unavailable) return { label: 'Unavailable', tone: 'muted', note: 'Cannot read forecast status. Trying again.' };
    if (!d.enabled) return { label: 'Off', tone: 'muted', note: 'This forecast model is turned off.' };
    const l = d.learning;
    if (l && l.health === 'degraded') {
      return { label: 'Needs attention', tone: 'warning', note: 'Learning has a problem. Open details for more information.' };
    }
    if (l && l.health === 'waiting_for_data') {
      return { label: 'Waiting for data', tone: 'warning', note: 'Waiting for usable measurements to continue learning.' };
    }
    if (!l || !['cold_start', 'learning', 'ready'].includes(l.status)) {
      return { label: 'Needs attention', tone: 'warning', note: 'Forecast learning is unavailable. Open details to check its status.' };
    }
    // Training is normally quarter-hourly. Allow two hours for load data
    // and 36 hours for solar, which does not train throughout the night.
    const maxAge = (signal === 'pv' ? 36 : 2) * 3600000;
    const latest = Number(l.latest_training_ms);
    const started = Number(l.started_ms);
    const reference = latest > 0 ? latest : started;
    if (reference > 0 && Date.now() - reference > maxAge) {
      return { label: 'Waiting for data', tone: 'warning', note: latest > 0 ? 'Last learned from new measurements ' + fmtAge(latest) + '.' : 'Waiting for usable measurements to begin learning.' };
    }
    if (latest > Date.now() || started > Date.now() || (l.status === 'ready' && !(latest > 0))) {
      return { label: 'Status unknown', tone: 'muted', note: 'The box has not provided a valid training time.' };
    }
    if (l.status === 'ready' && l.health === 'healthy') return { label: 'Healthy', tone: 'healthy', note: 'Ready and continuing to learn from your home.' };
    if (l.status === 'ready') return { label: 'Ready', tone: 'muted', note: 'The model is ready. Its health has not been confirmed.' };
    return { label: 'Learning', tone: 'learning', note: l.status === 'cold_start' ? 'Getting started with your home’s measurements.' : 'Getting to know your home’s energy patterns.' };
  }

  function learningDuration(learning) {
    const started = Number(learning && learning.started_ms);
    if (!(started > 0) || started > Date.now()) return '';
    const days = Math.floor((Date.now() - started) / 86400000);
    return days < 1 ? 'First day of learning' : days + (days === 1 ? ' day learning' : ' days learning');
  }

  function twinCard(title, d, endpoint) {
    const action = actions[endpoint];
    const statusID = 'twin-status-' + action.id;
    const state = overview(d, action.id);
    const learning = d && d.learning;
    const duration = learningDuration(learning);
    const activeProfile = d && (d.profile || d.active_profile);
    const profile = action.id === 'load' && d && d.enabled ? (activeProfile === 'away' ? 'Away profile' : activeProfile === 'home' ? 'Home profile' : '') : '';
    const meta = [duration, profile].filter(Boolean).join(' · ');
    const open = expanded.has(action.id);
    const detailsID = 'forecast-details-' + action.id;
    const usable = d && d.enabled && !d.unavailable;
    const details = usable ? row('Learning model', engineLabel(learning)) +
      row('Model state', learningStatus(learning)) +
      row('Health check', learning && learning.health === 'healthy' ? 'Healthy' : learning && learning.health === 'degraded' ? 'Needs attention' : learning && learning.health === 'waiting_for_data' ? 'Waiting for data' : 'Not available') +
      (learning && learning.health_reason ? `<p class="forecast-note">${esc(learning.health_reason)}</p>` : '') +
      row('Learning period started', learning && learning.started_ms > 0 ? fmtLocalTime(learning.started_ms) : 'Start date not recorded') +
      row('Latest training data', fmtLocalTime(learning && learning.latest_training_ms)) +
      '<p class="forecast-note">Healthy requires a health check from the box, a ready model and recent training data. It is not a forecast accuracy score. Learning days count elapsed time, not data coverage.</p>' +
      (action.id === 'load' ? loadProfileControl(d) : '') +
      '<h4>Forecast accuracy</h4><p class="forecast-note">Measured forecast accuracy is not available in this view yet.</p>' + modelRows(d) +
      '<h4>Restart learning</h4><p class="forecast-note">Use this after a lasting change to your home or solar system. It restarts this model and its fallback; measured history stays saved.</p>' +
      resetButton(endpoint, d) + (!actionAvailable(d) ? '<p class="forecast-note">Relearning is unavailable for this model.</p>' : '') :
      '<p class="forecast-note">' + esc(state.note) + '</p>';
    return `<div class="twin-card forecast-card"><div class="forecast-card-heading"><h3>${title}</h3><span class="forecast-state" data-tone="${state.tone}">${state.label}</span></div>` +
      `<p class="forecast-description">${esc(state.note)}</p>${meta ? `<p class="forecast-meta">${esc(meta)}</p>` : ''}` +
      `<button type="button" class="forecast-details-button" data-twin-details="${action.id}" aria-expanded="${open}" aria-controls="${detailsID}">${open ? 'Hide details' : 'Details'} <span aria-hidden="true">${open ? '−' : '+'}</span></button>` +
      `<div class="forecast-details" id="${detailsID}"${open ? '' : ' hidden'}>${details}</div>` +
      `<div class="forecast-action-status" id="${statusID}" role="status" aria-live="polite">${esc(actionMessages.get(endpoint) || '')}</div></div>`;
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
    if (active.dataset.twinDetails) return `[data-twin-details="${active.dataset.twinDetails}"]`;
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
    grid.innerHTML = twinCard('Solar production', pv, '/api/pvmodel/reset') + twinCard('Consumption', load, '/api/loadmodel/reset');
    if (selector && grid.querySelector) {
      const button = grid.querySelector(selector);
      if (button && !button.disabled && typeof button.focus === 'function') button.focus();
    }
    const sub = document.getElementById('twins-subtitle');
    if (sub) sub.textContent = 'Learning from your home';
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
    const target = e.target && e.target.closest ? e.target.closest('button') : e.target;
    const details = target && target.dataset && target.dataset.twinDetails;
    if (details) {
      if (expanded.has(details)) expanded.delete(details);
      else expanded.add(details);
      render(lastPV, lastLoad, `[data-twin-details="${details}"]`);
      return;
    }
    const profile = target && target.dataset && target.dataset.loadmodelProfile;
    if (profile) {
      if (target.classList.contains('active')) return;
      apiFetch('/api/loadmodel/profile', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ profile }) })
        .then(r => { if (!r.ok) throw new Error('HTTP ' + r.status); return r.json(); })
        .then(() => fetchAll())
        .catch(err => { actionMessages.set('/api/loadmodel/reset', 'Load profile switch failed: ' + err.message); render(lastPV, lastLoad); });
      return;
    }
    const endpoint = target && target.dataset && target.dataset.resetTwin;
    if (endpoint) startRelearn(endpoint);
  }
  function init() {
    const grid = document.getElementById('twins-grid');
    if (grid) grid.addEventListener('click', onGridClick);
    window.addEventListener('hashchange', syncPolling);
    document.addEventListener('visibilitychange', syncPolling);
    syncPolling();
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
  else init();
})();
