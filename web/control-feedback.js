// Presentation only. Core owns evidence, freshness, limits and response checks.
const messages = {
  solar_below_ceiling: ['Solar is below the requested ceiling', 'The measured output respects the ceiling, but available sunshine may already limit it.', 'FTW cannot yet confirm that curtailment caused the lower output.'],
  device_limit: ['Charger limit', 'The charger’s own current limit is below this request.', 'Check the charger’s current limit in its app or settings.'],
  offered_current_lower: ['Charger offers less current', 'The charger reports a lower current offer than FTW requested.', 'Check its load balancing and current limits. The reason is not confirmed.'],
  setpoint_changed: ['Setpoint does not match', 'The device reports a different setpoint from the command FTW sent.', 'Check for another controller or a device mode that changes the setpoint. FTW cannot tell which caused this.'],
  power_differs: ['Power does not follow the command', 'Fresh measurements differ from the command after the response wait.', 'The device has not reported a confirmed cause. Check its app or status display.'],
  command_failed: ['Command failed', 'The driver could not complete this command. The device may still have received part of it.', 'Check the device connection and status. FTW attempts to restore its safe default.'],
  command_unconfirmed: ['Command result unknown', 'FTW stopped waiting before it could confirm the call’s result.', 'The command may have reached the device. Check fresh power readings before trying again.'],
  default_failed: ['Safe default not confirmed', 'FTW has blocked further control while it retries the device’s safe default.', 'Check the device connection and its own status.'],
  telemetry_stale: ['Waiting for fresh readings', 'FTW cannot verify the effect with the readings available.', 'Check the device connection. This view updates when fresh readings arrive.'],
  site_meter_stale: ['Waiting for the site meter', 'FTW has stopped dispatch because site power readings are missing or too old.', 'Check the site meter connection. Dispatch resumes when readings recover.'],
  site_phase_currents_stale: ['Waiting for phase currents', 'FTW has stopped dispatch because it lacks fresh current readings for the main-fuse check.', 'Check the site meter connection and its phase readings. Dispatch resumes when readings recover.'],
  device_fault: ['Device cannot follow commands', 'The device or its driver reports a fault that blocks control.', 'Check the reported reason and the device’s status.'],
  fuse_limit: ['Limited by the main fuse', 'FTW reduced the request to keep site current within its limit.', 'The rate can rise when other household demand falls.'],
  fuse_cooldown: ['Waiting after a fuse limit', 'FTW is waiting before restoring the charging rate.', 'Charging resumes when the fuse protection allows it.'],
  charger_limit: ['Limited by FTW’s charger setting', 'FTW reduced the request to the configured charging limit.', 'Check the charger limit in FTW settings.'],
  core_limit: ['Adjusted by FTW', 'FTW changed the target through its safety or ramp limits.', 'The control tick does not report which limit applied.'],
  observe_only: ['Monitoring only', 'FTW reads this device but does not send commands to it.', 'Enable control in the device settings if you want FTW to control it.'],
  disabled: ['Device disabled', 'FTW does not control this device while it is disabled.', 'Check its device settings.'],
  device_control: ['Device controls itself', 'FTW returned the device to its own control.', 'The earlier FTW command no longer describes its target.'],
  not_connected: ['No car connected', 'The charger reports that no car is connected.', 'Connect a car to charge.'],
  waiting_response: ['Waiting for measured response', 'FTW is checking fresh power readings against the command.', 'Starting and changing phases can take time. A driver reply alone does not confirm the effect.'],
  response_unknown: ['Effect not verified', 'FTW does not have enough evidence to compare the command with measured power.', 'A successful driver call alone does not confirm the effect.'],
  no_command: ['No command recorded yet', 'FTW has no command result for this device since it started.', 'Fresh measurements will show the effect once control runs.'],
  power_observed: ['Device follows the command', 'The device’s own power readings follow the command within the response tolerance.', 'Independent confirmation is a separate step. A charging goal needs its own completion evidence.'],
  idle: ['No power requested', 'FTW currently requests no charge or discharge.', 'The active mode decides when the next request is needed.'],
  no_plan_budget: ['Waiting for the plan', 'The plan assigns no charging energy to this interval.', 'Check the next charging window, or choose Charge now.'],
  pv_surplus_pause: ['Waiting for spare solar', 'There is not enough spare solar to start charging.', 'Charging resumes when the solar rule allows it.'],
  pv_surplus: ['Using spare solar', 'FTW is matching charging to the available solar surplus.', 'The rate changes with household demand and solar output.'],
  plan: ['Following the plan', 'FTW is using the charging plan for this interval.', 'Power readings show whether the charger follows the request.'],
  manual_hold: ['Manual request', 'FTW is using your manual charging request.', 'Power readings show whether the charger follows it.'],
  wake_kick: ['Starting the charger', 'FTW is trying to wake the connected charger.', 'Waiting for fresh power readings.'],
};

export function feedbackRows(value) {
  if (!Array.isArray(value)) return [];
  return value.filter(row => row && typeof row === 'object' && typeof row.driver === 'string' && typeof row.reason === 'string');
}

export function feedbackText(row, live = true) {
  if (!live) return {title:'Last known control status', detail:'The box’s current control result is unavailable.', action:'Waiting for a fresh status report.'};
  const words = messages[row.reason] || ['Effect not verified', 'This box reports a control state this app does not recognise.', 'Check the device status.'];
  return {title:words[0], detail:words[1], action:words[2]};
}
const number = value => typeof value === 'number' && Number.isFinite(value);
export function feedbackPower(value, kind) {
  if (!number(value)) return 'Unknown';
  if (Math.abs(value) < 1) return '0 W';
  const size = Math.abs(value) >= 1000 ? `${(Math.abs(value)/1000).toFixed(1)} kW` : `${Math.round(Math.abs(value))} W`;
  return `${size} ${kind === 'pv' ? 'generation' : value < 0 ? 'discharge' : 'charge'}`;
}
export function feedbackValues(row, live = true) {
  const values = [
    ['Requested', feedbackPower(row.requested_w, row.kind)],
    ['Sent to driver', feedbackPower(row.sent_w, row.kind)],
    ['Device setpoint', live ? feedbackPower(row.readback_w, row.kind) : 'Not current'],
    ['Measured', live ? feedbackPower(row.actual_w, row.kind) : 'Not current'],
  ];
  if (number(row.tolerance_w)) values.push(['Response tolerance', `${Math.round(row.tolerance_w)} W`]);
  if (number(row.requested_a)) values.push(['Requested current', `${row.requested_a.toFixed(1)} A`]);
  if (number(row.offered_a)) values.push(['Charger offer', live ? `${row.offered_a.toFixed(1)} A` : 'Not current']);
  if (number(row.device_limit_a)) values.push(['Charger limit', live ? `${row.device_limit_a.toFixed(1)} A` : 'Not current']);
  const grid = value => `${(Math.abs(value)/1000).toFixed(2)} kW ${value < 0 ? 'export' : 'import'}`;
  if (number(row.site_before_w)) values.push(['Site before command', live ? grid(row.site_before_w) : 'Not current']);
  if (number(row.site_after_w)) values.push(['Site after command', live ? grid(row.site_after_w) : 'Not current']);
  if (number(row.device_delta_w)) values.push(['Device change', live ? `${(Math.abs(row.device_delta_w)/1000).toFixed(2)} kW ${row.device_delta_w < 0 ? "less" : "more"} site demand` : 'Not current']);
  if (number(row.site_delta_w)) values.push(['Site change', live ? `${(Math.abs(row.site_delta_w)/1000).toFixed(2)} kW ${row.site_delta_w < 0 ? 'less' : 'more'} import` : 'Not current']);
  const e = row.site_evidence;
  if (e && live) {
    const change = value => `${(Math.abs(value)/1000).toFixed(2)} kW ${value < 0 ? 'less' : 'more'} site demand`;
    for (const [label,key] of [['Other measured flows','other_change_w'],['Site after adjustment','adjusted_site_change_w'],['Unexplained change','unexplained_change_w']]) {
      if (number(e[key])) values.push([label, change(e[key])]);
    }
    if (number(e.samples) && e.samples > 0) values.push(['Comparison',`${e.samples} samples across ${Math.round(e.window_s)} s`]);
    if (number(e.tolerance_w) && e.tolerance_w > 0) values.push(['Site tolerance',`${Math.round(e.tolerance_w)} W`]);
    if (number(e.max_skew_ms)) values.push(['Largest time gap',`${(e.max_skew_ms/1000).toFixed(1)} s`]);
  }
  return values;
}
export function feedbackProof(row, live = true) {
  if (!live) return 'Current effect unknown';
  if (['observe_only','disabled','device_control'].includes(row.reason)) return 'No active FTW power command';
  if (row.verification_tier === 2) return 'Tier 2 · Confirmed at site meter';
  if (row.verification_tier === 1) return 'Tier 1 · Device reports the expected power';
  if (row.verification_tier === 0) return 'Tier 0 · Driver accepted the command';
  return 'No command acknowledgement';
}
export function feedbackSite(row, live = true) {
  if (!live) return 'Waiting for fresh measurements.';
  if (['observe_only','disabled','device_control'].includes(row.reason)) return 'Measurements remain context for the site; this device has no current control verdict.';
  const text = {
    confirmed: 'A separate site meter follows the device’s change across several samples, after accounting for other measured flows.',
    independent_source_unknown: 'Independent confirmation needs a separate, identified meter. These sources do not establish that.',
    no_site_meter: 'No site meter is available for independent confirmation.',
    no_baseline: 'No steady reading before the command is available for a site comparison.',
    waiting_for_meter: 'Waiting for a fresh, stable site-meter window.',
    readings_not_aligned: 'There are not enough distinct, time-aligned measurements to compare the curves.',
    flows_changing: 'Power is changing during the comparison. Independent confirmation remains uncertain.',
    no_clear_change: 'The change is too small to distinguish from other site activity.',
    other_flows_changed: 'Other equipment changed or its readings are missing. FTW cannot isolate this response.',
    other_flows_missing: 'Another measured flow is missing or stale. FTW cannot account for its effect on the site.',
    measurement_sources_unclear: 'Some site measurement sources are missing or may overlap. Independent confirmation remains uncertain.',
    energy_balance_conflict: 'The readings do not form a plausible site power balance. Check measurement sources, signs and shared flows.',
    site_change_differs: 'The site-meter change does not match. Another household load may have changed; the cause is not confirmed.',
    device_response_unconfirmed: 'Independent confirmation waits for the device’s measured response.',
  };
  return text[row.site_confirmation] || 'Independent confirmation is not available.';
}

export function feedbackCurve(row, live = true) {
  const trace = row.site_evidence?.trace;
  if (!live || !Array.isArray(trace) || trace.length < 3 || trace.length > 64) return null;
  if (!trace.every(p => p && number(p.at_ms) && number(p.device_change_w) && number(p.adjusted_site_change_w))) return null;
  const start=trace[0].at_ms, span=trace[trace.length-1].at_ms-start;
  if (span <= 0 || trace.some((p,i) => i > 0 && p.at_ms <= trace[i-1].at_ms)) return null;
  const scale=Math.max(500,...trace.flatMap(p=>[Math.abs(p.device_change_w),Math.abs(p.adjusted_site_change_w)]));
  const line=key=>trace.map(p=>`${(10+(p.at_ms-start)/span*260).toFixed(1)},${(55-p[key]/scale*40).toFixed(1)}`).join(' ');
  return {device:line('device_change_w'),site:line('adjusted_site_change_w'),label:`Measured changes over ${(span/1000).toFixed(0)} seconds. Device: solid line. Site after other measured flows: dashed line.`,scale:`${(scale/1000).toFixed(1)} kW`,duration:`${Math.round(span/1000)} s`};
}

// On-box renderer. The app uses the same presentation functions in Svelte.
export function renderFeedback(root, value, live = true) {
  if (!root) return;
  const rows = feedbackRows(value);
  root.hidden = rows.length === 0;
  // Preserve an open evidence table across the two-second status refresh.
  const open = new Set(Array.from(root.querySelectorAll('details[open]')).map(el => el.dataset.device));
  root.replaceChildren();
  if (!rows.length) return;
  const el = (tag, text, parent, cls) => { const node=document.createElement(tag); if (text) node.textContent=text; if(cls) node.className=cls; parent.appendChild(node); return node; };
  el('h2','Are you in control?',root);
  for (const row of rows) {
    const text=feedbackText(row,live);
    const card=el('article','',root,'control-result' + (live && row.severity === 'warning' ? ' needs-attention' : '') + (!live ? ' not-current' : ''));
    el('div',`${row.driver} · ${row.kind || 'device'}`,card,'control-device');
    el('h3',text.title,card);
    el('p',text.detail,card);
    if (live && row.device_reason) el('p',`Device reports: ${row.device_reason}`,card,'device-reason');
    el('p',text.action,card,'control-action');
    const proof=el('p',feedbackProof(row,live),card,'control-proof'); proof.dataset.tier=live ? String(row.verification_tier) : '';
    el('p',feedbackSite(row,live),card,'control-action');
    const details=el('details','',card); details.dataset.device=`${row.driver}:${row.kind}`; details.open=open.has(details.dataset.device);
    el('summary','Request and measurements',details);
    const dl=el('dl','',details);
    for (const [label,value] of feedbackValues(row,live)) {el('dt',label,dl);el('dd',value,dl);}
    const curve=feedbackCurve(row,live);
    if (curve) {
      const svg=document.createElementNS('http://www.w3.org/2000/svg','svg');
      svg.setAttribute('viewBox','0 0 280 110'); svg.setAttribute('role','img'); svg.setAttribute('aria-label',curve.label); svg.classList.add('control-curve'); details.appendChild(svg);
      for (const [points,cls] of [['10,55 270,55','zero'],[curve.device,'device'],[curve.site,'site']]) {
        const line=document.createElementNS(svg.namespaceURI,'polyline'); line.setAttribute('points',points); line.setAttribute('class',cls); line.setAttribute('fill','none'); svg.appendChild(line);
      }
      el('p',`Device: solid · Adjusted site: dashed · ±${curve.scale} · ${curve.duration}`,details,'control-time');
    }
    if (live && row.site_meter && number(row.site_after_at_ms)) el('p',`Site meter: ${row.site_meter} · ${new Date(row.site_after_at_ms).toLocaleTimeString()}`,details,'control-time');
    if (number(row.observed_at_ms)) el('p',`Last reading: ${new Date(row.observed_at_ms).toLocaleTimeString()}`,details,'control-time');
  }
}
if (typeof window !== 'undefined') window.FTWControlFeedback = {render:renderFeedback,text:feedbackText};
