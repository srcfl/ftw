// Box renderer for Core's control status. The words live in
// control-feedback.js, which the app shares. This file owns the DOM and
// updates each device's block in place, so open disclosures and keyboard
// focus survive the two-second status refresh.
import {controlRows, controlStatus, controlReceipt, controlNumbers, controlCurve, controlForPlanet, controlSummary} from './control-feedback.js';

const ICONS = {ok: '✓', neutral: '•', warning: '▲', alarm: '!', stale: '…'};
const KINDS = {battery: 'battery', ev: 'charger', v2x_charger: 'charger', pv: 'solar', device: 'device'};

function el(tag, parent, cls, text) {
  const node = document.createElement(tag);
  if (cls) node.className = cls;
  if (text) node.textContent = text;
  if (parent) parent.appendChild(node);
  return node;
}

function setText(node, text) {
  node.textContent = text || '';
  node.hidden = !text;
}

function keyed(root, selector, key, build) {
  const found = Array.from(root.querySelectorAll(selector)).find(node => node.dataset.key === key);
  return found || build();
}

function curveSvg(curve) {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('viewBox', '0 0 280 110');
  svg.setAttribute('role', 'img');
  svg.setAttribute('aria-label', curve.label);
  svg.classList.add('control-curve');
  for (const [points, cls] of [['10,55 270,55', 'zero'], [curve.device, 'device'], [curve.site, 'site']]) {
    const line = document.createElementNS(svg.namespaceURI, 'polyline');
    line.setAttribute('points', points);
    line.setAttribute('class', cls);
    line.setAttribute('fill', 'none');
    svg.appendChild(line);
  }
  return svg;
}

function buildBlock(root, key) {
  const section = el('section', root, 'cs-block');
  section.dataset.key = key;
  const title = el('h3', section, 'cs-title');
  el('span', title, 'cs-icon').setAttribute('aria-hidden', 'true');
  el('span', title, 'cs-title-text');
  el('p', section, 'cs-text');
  el('p', section, 'cs-proof');
  el('p', section, 'cs-next');
  const receipt = el('details', section, 'cs-receipt');
  el('summary', receipt, '', 'How FTW knows');
  el('ol', receipt, 'cs-steps');
  const numbers = el('details', receipt, 'cs-numbers');
  el('summary', numbers, '', 'Numbers');
  el('dl', numbers);
  el('div', numbers, 'cs-curve');
  return section;
}

function fillBlock(section, row, live) {
  const status = controlStatus(row, live);
  section.dataset.tone = status.tone;
  section.querySelector('.cs-icon').textContent = ICONS[status.tone] || ICONS.neutral;
  section.querySelector('.cs-title-text').textContent = status.title;
  setText(section.querySelector('.cs-text'), status.text);
  setText(section.querySelector('.cs-proof'), status.proof);
  setText(section.querySelector('.cs-next'), status.next);
  const steps = section.querySelector('.cs-steps');
  steps.replaceChildren(...controlReceipt(row, live).map(step => {
    const item = el('li');
    item.dataset.state = step.state;
    el('span', item, 'cs-step', step.step);
    el('span', item, 'cs-value', step.value);
    return item;
  }));
  const numbers = controlNumbers(row, live), curve = controlCurve(row, live);
  const more = section.querySelector('.cs-numbers');
  more.hidden = !numbers.length && !curve;
  more.querySelector('dl').replaceChildren(...numbers.flatMap(([label, value]) => [el('dt', null, '', label), el('dd', null, '', value)]));
  more.querySelector('.cs-curve').replaceChildren(...(curve ? [curveSvg(curve)] : []));
}

function fillList(root, rows, live) {
  const summary = controlSummary(rows, live);
  const head = keyed(root, '.cs-summary', 'summary', () => {
    const node = el('p', root, 'cs-summary');
    node.dataset.key = 'summary';
    return node;
  });
  head.textContent = summary.title;
  head.dataset.tone = summary.tone;
  const list = root.querySelector('.cs-list') || el('ul', root, 'cs-list');
  const keep = new Set();
  rows.forEach((row, index) => {
    const key = `${row.driver}:${row.kind}`;
    keep.add(key);
    const item = keyed(list, 'li', key, () => {
      const node = el('li', list);
      node.dataset.key = key;
      const button = el('button', node, 'cs-row');
      button.type = 'button';
      el('span', button, 'cs-device');
      el('span', button, 'cs-row-status');
      return node;
    });
    if (list.children[index] !== item) list.insertBefore(item, list.children[index] || null);
    const status = controlStatus(row, live);
    const button = item.querySelector('button');
    button.dataset.driver = row.driver;
    button.dataset.kind = row.kind;
    button.dataset.tone = status.tone;
    button.setAttribute('aria-label', `${row.driver} ${KINDS[row.kind] || row.kind}: ${status.title}. Open details`);
    item.querySelector('.cs-device').textContent = `${row.driver} · ${KINDS[row.kind] || row.kind}`;
    item.querySelector('.cs-row-status').textContent = status.title;
  });
  for (const item of Array.from(list.children)) if (!keep.has(item.dataset.key)) item.remove();
}

// Sheet mode shows one answer block per device function; compact mode lists
// the devices with a one-line answer each.
export function renderControlStatus(root, value, live = true, {compact = false} = {}) {
  if (!root) return;
  const rows = controlRows(value);
  root.hidden = rows.length === 0;
  if (root.dataset.mode !== (compact ? 'compact' : 'sheet')) {
    root.replaceChildren();
    root.dataset.mode = compact ? 'compact' : 'sheet';
  }
  if (!rows.length) return;
  if (!root.querySelector(':scope > .cs-question')) el('p', root, 'cs-question', 'Are we in control?');
  if (compact) {
    fillList(root, rows, live);
    return;
  }
  const keep = new Set();
  rows.forEach(row => {
    const key = `${row.driver}:${row.kind}`;
    keep.add(key);
    fillBlock(keyed(root, ':scope > .cs-block', key, () => buildBlock(root, key)), row, live);
  });
  for (const section of Array.from(root.querySelectorAll(':scope > .cs-block'))) {
    if (!keep.has(section.dataset.key)) section.remove();
  }
}

if (typeof window !== 'undefined') {
  window.FTWControlFeedback = {render: renderControlStatus, forPlanet: controlForPlanet, status: controlStatus};
}
