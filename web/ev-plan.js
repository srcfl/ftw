// Shared charging-plan data and view for the car and household plan.
export function plannedEVWatts(action = {}) {
  const powers = action.loadpoint_power_w;
  if (powers && Object.keys(powers).length) {
    return Object.values(powers).reduce((sum, w) => sum + (Number.isFinite(w) ? Math.max(0, w) : 0), 0);
  }
  return Number.isFinite(action.loadpoint_w) ? Math.max(0, action.loadpoint_w) : 0;
}

const fraction = value => Number.isFinite(value) && value >= 0 && value <= 1;
const percent = value => `${Math.round(value * 100)}%`;

export function chargingLevels(lp = {}) {
  if (lp.read_unavailable) return { now: 'Unknown', source: 'Current data unavailable', limit: 'Unknown',
    limitSource: 'Current data unavailable', fromCar: false, explanation: 'Waiting for current car and charger data.' };
  const freshCar = !!lp.plugged_in && !!lp.vehicle_driver && !lp.vehicle_stale;
  // A reported 0% is omitted by older Core JSON encoders.
  const fromCar = freshCar && lp.soc_source === 'vehicle' && fraction(lp.vehicle_soc ?? 0);
  const now = fromCar ? (lp.vehicle_soc ?? 0) : lp.current_soc;
  const limit = freshCar && fraction(lp.vehicle_charge_limit) && lp.vehicle_charge_limit > 0
    ? lp.vehicle_charge_limit : null;
  const unconfirmed = !fraction(now) || lp.soc_source === 'assumed' || lp.soc_source === 'completed';
  const vehicleGoal = lp.schedule?.finish_at_vehicle_limit === true || lp.finish_at_vehicle_limit === true;
  const goal = vehicleGoal ? (limit ?? 1) : (lp.schedule?.soc || lp.target_soc);
  const target = fraction(goal) && goal > 0 ? Math.min(goal, limit ?? 1) : null;
  return {
    now: fraction(now) ? percent(now) : 'Unknown',
    source: fromCar ? 'Reported by car' : unconfirmed ? 'Needs confirmation' : 'Estimated by FTW',
    limit: limit == null ? 'Unknown' : percent(limit),
    limitSource: limit == null ? 'Not reported by car' : 'Reported by car',
    fromCar,
    explanation: lp.manual_active ? 'Your saved goal resumes when you return to the plan.'
      : lp.goal_complete === true ? 'The car has confirmed this goal is complete.'
      : vehicleGoal && limit == null
      ? 'FTW does not know the car’s limit. It reserves charging for up to 100%; the car decides when to stop.'
      : target != null ? `Planning from ${fraction(now) ? percent(now) : 'an unknown level'} to ${percent(target)}${unconfirmed ? ' · confirm the current level' : ''}.` : 'Set a goal to plan charging.',
  };
}

export function chargingPlan(lp, start = Date.now(), end = start + 24 * 3600000) {
  let message = '';
  if (!lp) message = 'Charging plan unavailable. Trying again…';
  else if (lp.charger?.available === false || lp.power_unavailable) message = 'Waiting for current charger data.';
  else if (!lp.plugged_in) message = 'Plug in to plan charging.';
  else if (lp.manual_active) message = lp.manual_charge_w === 0 ? 'Charging paused by you.' : 'Charge now is active. Scheduled charging resumes when you return to the plan.';
  else if (lp.goal_complete === true) message = 'Charging goal complete.';
  else if (lp.plan_pending) message = 'Updating charging times…';
  else if (lp.plan_outdated) message = 'Charging times unavailable. Your goal is saved.';
  else if (lp.surplus_only) message = 'PV only · charging follows available solar power.';
  else if (lp.grid_deferred) message = 'Waiting for tomorrow’s electricity prices.';
  const windows = message ? [] : (lp.plan_windows || []).filter(w =>
    Number.isFinite(w.start_ms) && Number.isFinite(w.end_ms) && w.end_ms > w.start_ms &&
    Number.isFinite(w.wh) && w.wh > 0 && w.end_ms > start && w.start_ms < end
  ).map(w => {
    const left = Math.max(start, w.start_ms), right = Math.min(end, w.end_ms);
    return { start_ms: left, end_ms: right, wh: w.wh * (right - left) / (w.end_ms - w.start_ms),
      partial: left !== w.start_ms || right !== w.end_ms };
  }).sort((a, b) => a.start_ms - b.start_ms);
  return { windows, message: message || (windows.length ? '' : 'No charging planned in this period.'),
    wh: windows.reduce((sum, w) => sum + w.wh, 0), approximate: windows.some(w => w.partial) };
}

function clock(ts) {
  return new Date(ts).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false });
}
function dayClock(ts, now) {
  const date = new Date(ts), today = new Date(now), tomorrow = new Date(now);
  tomorrow.setDate(tomorrow.getDate() + 1);
  const day = date.toDateString() === today.toDateString() ? ''
    : date.toDateString() === tomorrow.toDateString() ? 'Tomorrow ' : date.toLocaleDateString([], { weekday: 'short' }) + ' ';
  return day + clock(ts);
}

export function createChargingTimeline(doc = document) {
  const el = doc.createElement('section');
  el.className = 'ev-timeline';
  el.setAttribute('aria-label', 'Planned car charging');
  function node(tag, className, parent) {
    const item = doc.createElement(tag); item.className = className; parent.appendChild(item); return item;
  }
  const heading = node('div', 'ev-timeline-heading', el);
  const title = node('strong', '', heading), energy = node('span', '', heading);
  const message = node('p', 'ev-timeline-note', el);
  const graphic = node('div', '', el);
  const track = node('div', 'ev-plan-track', graphic);
  track.setAttribute('aria-hidden', 'true');
  const ticks = node('div', 'ev-plan-ticks', graphic);
  const list = node('div', 'ev-plan-windows', el);
  const more = node('details', 'ev-plan-more', el);
  const summary = node('summary', '', more), rest = node('div', 'ev-plan-windows', more);
  const note = node('small', 'ev-timeline-note', el);
  note.textContent = 'Planned charging · times can change as FTW replans.';
  return { el, update(lp, { start = Date.now(), end = start + 24 * 3600000, label = 'Next 24 hours' } = {}) {
    const data = chargingPlan(lp, start, end);
    title.textContent = label;
    energy.textContent = data.windows.length ? `${data.approximate ? '≈ ' : ''}${(data.wh / 1000).toFixed(1)} kWh` : '';
    message.textContent = data.message; message.hidden = !data.message;
    graphic.hidden = !data.windows.length; list.hidden = !data.windows.length;
    note.hidden = !data.windows.length; more.hidden = data.windows.length <= 2;
    track.replaceChildren(); ticks.replaceChildren(); list.replaceChildren(); rest.replaceChildren();
    for (let i = 0; i <= 4; i++) {
      const tick = node('span', '', ticks), ts = start + (end - start) * i / 4;
      const isNow = i === 0 && Math.abs(Date.now() - start) < 60000;
      node('span', '', tick).textContent = isNow ? 'Now' : clock(ts);
      const day = dayClock(ts, start).replace(clock(ts), '').trim();
      node('small', '', tick).textContent = day || (isNow ? '' : 'Today');
    }
    data.windows.forEach((w, i) => {
      const bar = node('span', 'ev-plan-window', track);
      bar.style.left = `${(w.start_ms - start) / (end - start) * 100}%`;
      bar.style.width = `${(w.end_ms - w.start_ms) / (end - start) * 100}%`;
      const slot = node('div', 'ev-plan-slot', i < 2 ? list : rest);
      node('strong', '', slot).textContent = `${dayClock(w.start_ms, start)}–${dayClock(w.end_ms, w.start_ms)}`;
      node('span', '', slot).textContent = `${w.partial ? '≈ ' : ''}${(w.wh / 1000).toFixed(1)} kWh planned`;
    });
    summary.textContent = `${Math.max(0, data.windows.length - 2)} more charging windows`;
  } };
}
