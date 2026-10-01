// Words for Core's answer to "Are we in control?". Core owns every verdict:
// status, severity, evidence and reason. This module only picks words, marks
// and receipt rows for them. The app copies this file byte for byte, so it
// stays free of DOM and window access.

const num = value => typeof value === 'number' && Number.isFinite(value);

export function controlRows(value) {
  if (!Array.isArray(value)) return [];
  return value.filter(row => row && typeof row === 'object' && typeof row.driver === 'string' &&
    typeof row.status === 'string' && typeof row.reason === 'string');
}

export function controlPower(watts) {
  if (!num(watts)) return 'unknown';
  const size = Math.abs(watts);
  if (size < 1) return '0 W';
  return size >= 1000 ? `${(size / 1000).toFixed(1)} kW` : `${Math.round(size)} W`;
}

const NOUNS = {battery: 'battery', ev: 'charger', v2x_charger: 'charger', pv: 'solar inverter', device: 'device'};
const noun = row => NOUNS[row.kind] || 'device';
const percent = soc => num(soc) ? `${Math.round(soc * 100)}%` : 'its current level';
const amps = value => num(value) ? `${Math.round(value)} A` : 'the current';
const clock = ms => num(ms) && ms > 0 ? new Date(ms).toLocaleTimeString([], {hour: '2-digit', minute: '2-digit'}) : '';
const capital = text => text.charAt(0).toUpperCase() + text.slice(1);

// Site convention: positive power is drawn into the site, so a battery or car
// charges and solar is negative.
function doing(row, watts) {
  if (row.kind === 'pv') return 'producing';
  if (!num(watts) || Math.abs(watts) < 100) return 'idle';
  return watts > 0 ? 'charging' : 'discharging';
}

export function controlFlow(row, watts) {
  if (!num(watts)) return 'unknown';
  if (row.kind === 'pv') return `${controlPower(watts)} solar`;
  if (Math.abs(watts) < 100) return row.kind === 'ev' ? 'no charging' : 'idle';
  return `${controlPower(watts)} ${watts > 0 ? 'charge' : 'discharge'}`;
}

function target(row) {
  if (!num(row.sent_w)) return 'the new target';
  return row.kind === 'pv' ? `a ${controlPower(row.sent_w)} cap` : controlFlow(row, row.sent_w);
}

function source(row) {
  if (row.mode === 'manual') return 'as you asked';
  if (row.mode === 'solar') return 'from spare solar';
  if (row.mode === 'plan' || String(row.mode || '').startsWith('planner')) return 'as planned';
  return 'as FTW asked';
}

const STATUS_TITLES = {
  following: 'Following FTW', waiting: 'Waiting', limited: 'Limited', not_following: 'Not following',
  no_contact: 'No contact', not_controlled: 'Not controlled by FTW',
};

// A reason title replaces the status word when the word alone says too little.
const REASON_TITLES = {
  waiting_response: 'Adjusting', no_command: 'No command yet', not_connected: 'No car connected',
  no_plan_budget: 'Waiting for the plan', pv_surplus_pause: 'Waiting for spare solar', wake_kick: 'Starting the charger',
  vehicle_complete: 'Car is full', vehicle_limit_completion: 'Car reached its limit', vehicle_not_requesting: 'Car not taking power',
  battery_full: 'Battery full', battery_nearly_full: 'Battery nearly full', battery_nearly_empty: 'Battery nearly empty',
  core_limit: 'Limited by FTW', fuse_limit: 'Limited by the main fuse', fuse_cooldown: 'Waiting after a fuse limit',
  charger_limit: 'Limited by FTW’s charger setting', device_limit: 'Charger limit', offered_current_lower: 'Charger offers less current',
  device_fault: 'Device fault', telemetry_stale: 'No fresh readings', readings_lost: 'Lost control',
  command_failed: 'Commands failing', command_unconfirmed: 'Command not confirmed', default_failed: 'Cannot hand back control',
  response_unknown: 'No power reading', observe_only: 'Monitoring only', disabled: 'Disabled', device_control: 'Own control',
  site_meter_stale: 'Paused for safety', site_phase_currents_stale: 'Paused for safety',
};

function sentence(row) {
  const name = noun(row), actual = row.actual_w;
  switch (row.reason) {
    case 'power_observed': case 'plan': case 'manual_hold': case 'pv_surplus': case 'idle': {
      const now = doing(row, actual);
      return now === 'idle' ? `Idle ${source(row)}.` : `${capital(now)} ${controlPower(actual)} ${source(row)}.`;
    }
    case 'solar_below_ceiling': return `Producing ${controlPower(actual)}, below ${target(row)}.`;
    case 'vehicle_complete': return 'Not charging: the car reports that it is full.';
    case 'vehicle_limit_completion': return 'Not charging: the car reached its own charge limit.';
    case 'vehicle_not_requesting': return 'The car is not asking for power right now.';
    case 'waiting_response': return `Moving to ${target(row)}. The ${name} usually responds within ${row.kind === 'ev' ? 'two minutes' : '15 seconds'}.`;
    case 'no_command': return 'FTW has not sent a command since it started.';
    case 'not_connected': return 'Plug in a car to charge.';
    case 'no_plan_budget': return 'The plan has no charging in this period.';
    case 'pv_surplus_pause': return 'Charging starts when there is enough spare solar.';
    case 'wake_kick': return 'FTW is waking the charger.';
    case 'battery_full': return `FTW paused charging at ${percent(row.battery_soc)}. Charging resumes at ${percent(row.charge_resume_soc)} or lower; discharging works as normal.`;
    case 'battery_nearly_full': return `Taking ${controlPower(actual)} of ${controlPower(row.sent_w)} at ${percent(row.battery_soc)}. A battery charges slower when it is nearly full.`;
    case 'battery_nearly_empty': return `Giving ${controlPower(actual)} of ${controlPower(row.sent_w)} at ${percent(row.battery_soc)}. A battery limits discharge when it is nearly empty.`;
    case 'core_limit': return `FTW’s safety limits set the target to ${target(row)}.`;
    case 'fuse_limit': return `FTW lowered the rate to ${target(row)} to protect the main fuse. It rises when the house uses less.`;
    case 'fuse_cooldown': return 'FTW waits a moment after a fuse limit before it raises the rate.';
    case 'charger_limit': return 'The charger limit in FTW’s settings caps the rate.';
    case 'device_limit': return `The charger’s own limit is ${amps(row.device_limit_a)}, below the ${amps(row.requested_a)} FTW asked for.`;
    case 'offered_current_lower': return `The charger offers ${amps(row.offered_a)}, below the ${amps(row.requested_a)} FTW asked for.`;
    case 'power_below_target': case 'power_above_target': return `Asked for ${target(row)}, delivering ${controlPower(actual)}.`;
    case 'power_wrong_direction': return `Asked for ${target(row)}, but it is ${doing(row, actual)} ${controlPower(actual)}.`;
    case 'no_power_response': return `Asked for ${target(row)}, but it delivers no power.`;
    case 'power_while_idle': return `Asked to stay idle, but it is ${doing(row, actual)} ${controlPower(actual)}.`;
    case 'setpoint_changed': return `The ${name} reports a target of ${controlFlow(row, row.readback_w)}, not the ${target(row)} FTW sent.`;
    case 'device_fault': return row.device_reason ? `The ${name} reports a fault: ${row.device_reason}.` : `The ${name} reports a fault that stops control.`;
    case 'telemetry_stale': return `FTW has no fresh readings from the ${name}.`;
    case 'readings_lost': return clock(row.observed_at_ms)
      ? `No usable readings from the ${name} since ${clock(row.observed_at_ms)}. FTW cannot see what its commands do.`
      : `No usable readings from the ${name}. FTW cannot see what its commands do.`;
    case 'command_failed': return `The last command to the ${name} failed. FTW keeps trying.`;
    case 'command_unconfirmed': return `The last command timed out. It may still have reached the ${name}.`;
    case 'default_failed': return `FTW could not return the ${name} to its own mode, so it sends no other commands until it can.`;
    case 'response_unknown': return `FTW has no power reading from the ${name} to compare with its command.`;
    case 'observe_only': return `FTW reads this ${name} but does not control it.`;
    case 'disabled': return 'This device is disabled in FTW’s settings.';
    case 'device_control': return `The ${name} runs its own mode. FTW is not sending commands.`;
    case 'site_meter_stale': return 'FTW stopped control because the grid meter readings are out of date.';
    case 'site_phase_currents_stale': return 'FTW stopped control because it lacks fresh phase currents for the main fuse check.';
    default: return `The box reports “${row.reason}”, which this version cannot describe.`;
  }
}

function nextStep(row) {
  const name = noun(row);
  switch (row.reason) {
    case 'power_below_target': case 'power_above_target': case 'power_wrong_direction':
    case 'no_power_response': case 'power_while_idle':
      return `The ${name} has not said why. Check its app or display.`;
    case 'setpoint_changed': return 'Another app or controller may be changing it.';
    case 'device_fault': return `Check the ${name}.`;
    case 'telemetry_stale': case 'readings_lost': case 'command_failed': case 'default_failed':
      return `Check the ${name}’s connection.`;
    case 'site_meter_stale': case 'site_phase_currents_stale': return 'Check the grid meter’s connection.';
    case 'device_limit': return 'Raise the limit in the charger’s app if you want faster charging.';
    case 'offered_current_lower': return 'Check the charger’s load balancing and limits.';
    case 'observe_only': return 'Turn on control in the device settings if you want FTW to steer it.';
    default: return '';
  }
}

function proofLine(row) {
  if (row.evidence === 'confirmed' && clock(row.confirmed_at_ms)) return `The grid meter confirmed this at ${clock(row.confirmed_at_ms)}.`;
  if (row.evidence === 'measured' && row.status === 'following') return `Based on the ${noun(row)}’s own readings.`;
  return '';
}

// One answer per device function: a title, one sentence and, when the owner
// can act, the next step. Tone follows Core's severity and status.
export function controlStatus(row, live = true) {
  if (!live) return {title: 'Status not current', text: 'FTW has not reported for a while.', proof: '', next: 'Waiting for a fresh report.', tone: 'stale'};
  const title = REASON_TITLES[row.reason] || STATUS_TITLES[row.status] || 'Checking';
  const tone = row.severity === 'alarm' ? 'alarm' : row.severity === 'warning' ? 'warning' : row.status === 'following' ? 'ok' : 'neutral';
  return {title, text: sentence(row), proof: proofLine(row), next: nextStep(row), tone};
}

const CONFIRMATION = {
  no_site_meter: ['No grid meter to compare with', 'none'],
  independent_source_unknown: ['The grid meter is not a separate sensor', 'none'],
  not_controlling: ['Not needed while FTW does not control it', 'none'],
  device_response_unconfirmed: ['Waits for the device’s measured response', 'wait'],
  waiting_for_meter: ['Waiting for fresh grid meter readings', 'wait'],
  no_baseline: ['Waiting for a clear change to compare', 'wait'],
  readings_not_aligned: ['Waiting for readings close enough in time', 'wait'],
  flows_changing: ['Other loads changed at the same time', 'wait'],
  site_change_differs: ['The grid change did not match; another load may have changed', 'wait'],
  no_clear_change: ['The change was too small to compare', 'wait'],
};

function modeName(mode) {
  if (mode === 'manual') return 'manual';
  if (mode === 'solar') return 'solar only';
  if (mode === 'plan' || String(mode || '').startsWith('planner')) return 'plan';
  return String(mode || 'FTW').replace(/_/g, ' ');
}

// The receipt shows how FTW knows: each step with its value and state
// (done, wait, fail or none).
export function controlReceipt(row, live = true) {
  const steps = [];
  const at = ms => clock(ms) ? ` · ${clock(ms)}` : '';
  const sent = row.sent_w, asked = row.requested_w;
  if (num(asked) && num(sent) && Math.abs(asked - sent) > Math.max(50, Math.abs(sent) * 0.02)) {
    steps.push({step: 'Asked', value: `${controlFlow(row, asked)} · ${modeName(row.mode)}`, state: 'done'});
  }
  steps.push({step: 'Sent', value: num(sent) ? `${controlFlow(row, sent)}${at(row.command_at_ms)}` : 'No command', state: num(sent) ? 'done' : 'none'});
  const failed = ['command_failed', 'command_unconfirmed', 'default_failed'].includes(row.reason);
  const accepted = ['accepted', 'measured', 'confirmed'].includes(row.evidence);
  steps.push({step: 'Accepted',
    value: failed ? REASON_TITLES[row.reason] : accepted
      ? (live && num(row.readback_w) ? `The ${noun(row)} reports target ${controlFlow(row, row.readback_w)}` : 'The driver took the command')
      : num(sent) ? 'Waiting for the driver' : '—',
    state: failed ? 'fail' : accepted ? 'done' : num(sent) ? 'wait' : 'none'});
  const measured = ['measured', 'confirmed'].includes(row.evidence);
  steps.push({step: 'Measured',
    value: !live ? 'Not current' : measured ? `${controlFlow(row, row.actual_w)}${at(row.observed_at_ms)}`
      : row.readings_fresh === false ? 'No fresh readings' : num(sent) ? 'Waiting for fresh readings' : '—',
    state: !live ? 'none' : measured ? 'done' : row.readings_fresh === false && num(sent) ? 'fail' : num(sent) ? 'wait' : 'none'});
  const change = row.site_evidence && row.site_evidence.device_change_w;
  const [why, state] = CONFIRMATION[row.site_confirmation] || ['Waiting for a clear change to compare', 'wait'];
  steps.push({step: 'Confirmed',
    value: !live ? 'Not current' : row.evidence === 'confirmed'
      ? `The grid meter matched ${num(change) ? `a ${controlPower(change)} change` : 'the change'}${at(row.confirmed_at_ms)}`
      : why,
    state: !live ? 'none' : row.evidence === 'confirmed' ? 'done' : state});
  return steps;
}

// Changes in words, never a bare sign: positive power is drawn into the site.
function deviceChange(row, watts) {
  if (row.kind === 'pv') return `${controlPower(watts)} ${watts < 0 ? 'more' : 'less'} solar`;
  if (row.kind === 'ev') return `${controlPower(watts)} ${watts < 0 ? 'less' : 'more'} charging`;
  return `${controlPower(watts)} toward ${watts < 0 ? 'discharge' : 'charge'}`;
}
const gridChange = watts => `${controlPower(watts)} toward ${watts < 0 ? 'export' : 'import'}`;
const drawnChange = watts => `${controlPower(watts)} ${watts < 0 ? 'less' : 'more'} drawn`;

// Numbers for experts and support.
export function controlNumbers(row, live = true) {
  if (!live) return [];
  const values = [];
  if (num(row.battery_soc)) values.push(['Battery charge', percent(row.battery_soc)]);
  if (num(row.tolerance_w)) values.push(['Response tolerance', `±${controlPower(row.tolerance_w)}`]);
  if (num(row.requested_a)) values.push(['Requested current', amps(row.requested_a)]);
  if (num(row.offered_a)) values.push(['Charger offer', amps(row.offered_a)]);
  if (num(row.device_limit_a)) values.push(['Charger limit', amps(row.device_limit_a)]);
  const e = row.site_evidence;
  if (e && typeof e === 'object') {
    for (const [label, key] of [['Grid before', 'grid_before_w'], ['Grid after', 'grid_after_w']]) {
      if (num(e[key])) values.push([label, `${controlPower(e[key])} ${e[key] < 0 ? 'export' : 'import'}`]);
    }
    for (const [label, key, words] of [['Device change', 'device_change_w', w => deviceChange(row, w)], ['Grid change', 'grid_change_w', gridChange],
      ['Other measured change', 'other_change_w', drawnChange], ['Unexplained change', 'unexplained_change_w', drawnChange]]) {
      if (num(e[key])) values.push([label, words(e[key])]);
    }
    if (num(e.samples) && e.samples > 0) values.push(['Compared', `${e.samples} readings over ${Math.round(e.window_s)} s`]);
    if (num(e.tolerance_w) && e.tolerance_w > 0) values.push(['Match tolerance', `±${controlPower(e.tolerance_w)}`]);
    if (Array.isArray(e.unmeasured_flows) && e.unmeasured_flows.length) {
      const kinds = {battery: 'battery', ev: 'car charger', v2x_charger: 'car charger', pv: 'solar', meter: 'meter'};
      values.push(['Left in the background', e.unmeasured_flows.filter(v => typeof v === 'string')
        .map(v => { const [driver, kind] = v.split(':'); return kinds[kind] ? `${driver} (${kinds[kind]})` : driver; }).join(', ')]);
    }
    if (num(e.max_skew_ms) && e.samples > 0) values.push(['Largest time gap', `${Math.round(e.max_skew_ms)} ms`]);
  }
  return values;
}

export function controlCurve(row, live = true) {
  const trace = row.site_evidence?.trace;
  if (!live || !Array.isArray(trace) || trace.length < 3 || trace.length > 64) return null;
  if (!trace.every(p => p && num(p.at_ms) && num(p.device_change_w) && num(p.adjusted_site_change_w))) return null;
  const start = trace[0].at_ms, span = trace[trace.length - 1].at_ms - start;
  if (span <= 0 || trace.some((p, i) => i > 0 && p.at_ms <= trace[i - 1].at_ms)) return null;
  const scale = Math.max(500, ...trace.flatMap(p => [Math.abs(p.device_change_w), Math.abs(p.adjusted_site_change_w)]));
  const line = key => trace.map(p => `${(10 + (p.at_ms - start) / span * 260).toFixed(1)},${(55 - p[key] / scale * 40).toFixed(1)}`).join(' ');
  return {device: line('device_change_w'), site: line('adjusted_site_change_w'),
    label: `Measured changes over ${Math.round(span / 1000)} seconds. Device: solid line. Grid after other measured flows: dashed line.`,
    scale: controlPower(scale), duration: `${Math.round(span / 1000)} s`};
}

// Rows for one bubble or device sheet. A combined bubble includes every device.
export function controlForPlanet(value, planet = {}) {
  const name = planet.id?.startsWith('agg-') ? '' : planet.name;
  return controlRows(value).filter(row => (!name || row.driver === name) &&
    (!planet.role || row.kind === planet.role || row.kind === 'device' || planet.role === 'ev' && row.kind === 'v2x_charger'));
}

export function controlSummary(value, live = true) {
  const rows = controlRows(value);
  if (!live) return {title: 'Status not current', tone: 'stale'};
  const count = (n, one, many) => `${n} ${n === 1 ? one : many}`;
  const alarms = rows.filter(row => row.severity === 'alarm').length;
  const warnings = rows.filter(row => row.severity === 'warning').length;
  if (alarms) return {title: `${count(alarms, 'device needs', 'devices need')} attention now`, tone: 'alarm'};
  if (warnings) return {title: `${count(warnings, 'device needs', 'devices need')} a look`, tone: 'warning'};
  const controlled = rows.filter(row => row.status !== 'not_controlled');
  if (!controlled.length) return {title: 'FTW controls no device right now', tone: 'neutral'};
  return {title: 'Yes, FTW is in control', tone: 'ok'};
}

// Overview marks stay off while all is well. Only Core's warning and alarm
// severities draw one; a device that stopped reporting keeps its bubble.
export function withControlMarks(planets, value, live = true) {
  const result = planets.map(p => ({...p}));
  const corners = {battery: 'top-right', ev: 'bottom-right', pv: 'top-left', v2x_charger: 'bottom-right'};
  const titles = {battery: 'BATTERY', pv: 'SOLAR', ev: 'EV CHARGER'};
  for (const row of controlRows(value)) {
    const role = row.kind === 'v2x_charger' ? 'ev' : row.kind;
    let planet = result.find(p => p.name === row.driver && (row.kind === 'device' || p.role === role));
    if (!planet) {
      if (!corners[row.kind]) continue;
      planet = {id: `control-${row.kind}-${row.driver}`, name: row.driver, role, corner: corners[row.kind],
        title: titles[role], kw: 0, toHub: false, color: 'var(--fg-muted)', sub: 'no data', placeholder: true};
      result.push(planet);
    }
    if (live && row.readings_fresh === false && row.status !== 'not_controlled') {
      Object.assign(planet, {placeholder: true, kw: 0, sub: 'no data', color: 'var(--fg-muted)'});
    }
    if (row.status !== 'not_controlled') planet.clickable = true;
    if (!live || !['warning', 'alarm'].includes(row.severity)) continue;
    const mark = {tone: row.severity, label: controlStatus(row).title};
    if (!planet.controlMark || mark.tone === 'alarm' && planet.controlMark.tone !== 'alarm') planet.controlMark = mark;
  }
  return result;
}
