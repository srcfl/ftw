// node --test web/cars.test.mjs
//
// The owner's cars on the dashboard (docs/cars.md): each car shows, plugged
// in or not, with its level, where the level came from and how old it is;
// a charger whose car the box cannot name asks, and a tap answers.

import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { describe, it } from 'node:test';

import {
  ageText, asking, carTiles, choices, createCarsCard, createWhichCar, daysText, goalText,
  levelView, noticeTitle, tap, tapRefusedText, TAP_TIMEOUT_MS,
} from './cars.js';

const read = path => readFileSync(new URL(path, import.meta.url), 'utf8');

// Just enough of the DOM for the two builders.
class FakeElement {
  constructor(tag) {
    this.tagName = tag.toUpperCase();
    this.children = [];
    this.attributes = {};
    this.listeners = {};
    this.dataset = {};
    this.hidden = false;
    this.disabled = false;
    this.className = '';
    this._text = '';
  }
  appendChild(child) { this.children.push(child); child.parentNode = this; return child; }
  replaceChildren(...kids) { this.children = []; kids.forEach(k => this.appendChild(k)); }
  setAttribute(k, v) { this.attributes[k] = String(v); }
  getAttribute(k) { return this.attributes[k]; }
  addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
  click() { return Promise.all((this.listeners.click || []).map(fn => fn({}))); }
  focus() { this.ownerDocument.activeElement = this; }
  set textContent(v) { this._text = String(v); this.children = []; }
  get textContent() { return this._text + this.children.map(c => c.textContent).join(''); }
  all(predicate, out = []) {
    for (const c of this.children) { if (predicate(c)) out.push(c); c.all(predicate, out); }
    return out;
  }
}
function fakeDoc() {
  const doc = { activeElement: null };
  doc.createElement = tag => Object.assign(new FakeElement(tag), { ownerDocument: doc });
  return doc;
}
const buttons = el => el.all(c => c.tagName === 'BUTTON');

const tesla = {
  id: 'a'.repeat(32), name: 'Model Y', capacity_wh: 76000, soc_permille: 720, soc_source: 'vehicle',
  soc_age_s: 3600, energy_since_wh: 0, soc_estimate: false, charge_limit_permille: 800,
  goal: { target_permille: 800, ready_by_min: 420, days: 127, finish_at_limit: false }, surplus_only: false,
};
const volvo = { id: 'b'.repeat(32), name: 'XC40', capacity_wh: 69000, energy_since_wh: 0, soc_estimate: false, surplus_only: true };
const kia = { id: 'c'.repeat(32), name: 'EV6', capacity_wh: 77400, energy_since_wh: 0, soc_estimate: false, surplus_only: false };

describe('a car level says where it came from and how old it is', () => {
  it('keeps an hour-old reading with nothing since as the car’s own number', () => {
    // Fredrik's box on 17 October: 72 % from the car an hour ago.
    const v = levelView(tesla);
    assert.equal(v.value, '72%');
    assert.equal(v.source, 'From the car · 1 h ago');
    assert.equal(v.estimate, false);
  });

  it('ages the reading between reads, never shows it as live', () => {
    assert.equal(levelView(tesla, 15 * 60).source, 'From the car · 1 h 15 min ago');
    assert.equal(levelView({ ...tesla, soc_age_s: 20 }).source, 'From the car · just now');
    assert.equal(levelView({ ...tesla, soc_age_s: 3 * 86400 }).source, 'From the car · 3 days ago');
  });

  it('marks an estimate and the energy since the reading', () => {
    const v = levelView({ ...tesla, soc_permille: 750, soc_estimate: true, energy_since_wh: 2400 });
    assert.equal(v.value, '≈ 75%');
    assert.equal(v.source, 'From the car · 1 h ago + 2.4 kWh');
    assert.match(levelView({ ...tesla, soc_estimate: true, energy_since_wh: -1200 }).source, /− 1\.2 kWh$/);
  });

  it('names a level set by hand and an assumed one', () => {
    assert.equal(levelView({ ...tesla, soc_source: 'manual', soc_age_s: 300 }).source, 'Set by hand · 5 min ago');
    const assumed = levelView({ ...tesla, soc_permille: 300, soc_source: 'assumed', soc_estimate: true });
    assert.deepEqual([assumed.value, assumed.source], ['≈ 30%', 'Assumed at plug-in']);
  });

  it('shows no number for a car with no level', () => {
    assert.deepEqual([levelView(volvo).value, levelView(volvo).source], ['—', 'No level yet']);
  });

  it('counts ages in words', () => {
    assert.equal(ageText(59), 'just now');
    assert.equal(ageText(60), '1 min ago');
    assert.equal(ageText(3599), '59 min ago');
    assert.equal(ageText(10 * 3600 + 120), '10 h ago');
    assert.equal(ageText(86400), '1 day ago');
    assert.equal(ageText(undefined), '');
  });
});

describe('a goal in words', () => {
  it('says what, by when and on which days', () => {
    assert.equal(goalText(tesla.goal), '80% by 07:00, every day');
    assert.equal(goalText({ target_permille: 600, ready_by_min: 390, days: 0x1f }), '60% by 06:30, weekdays');
    assert.equal(goalText({ target_permille: 0, ready_by_min: 1200, days: 0, finish_at_limit: true }), 'Car’s limit by 20:00, once');
    assert.equal(goalText(null), 'No goal');
    assert.equal(daysText(0x60), 'weekends');
    assert.equal(daysText(0b0000101), 'Mon, Wed');
  });
});

describe('each car shows, plugged in or not', () => {
  const garage = { id: 'garage', driver_name: 'easee', plugged_in: true, session: 's1', car_id: volvo.id, car_identified_by: 'car' };
  it('shows an unplugged car with its level, limit and goal', () => {
    const [t] = carTiles([tesla], []);
    assert.equal(t.where, 'Not plugged in');
    assert.equal(t.charger, null);
    assert.equal(t.facts, 'Limit 80% · Goal 80% by 07:00, every day');
  });

  it('shows the charger a car is on and how the box knows', () => {
    const [, v] = carTiles([tesla, volvo], [garage]);
    assert.equal(v.where, 'On garage');
    assert.equal(v.knownBy, 'The car said so');
    assert.deepEqual(v.charger, { id: 'garage', driver_name: 'easee' });
    assert.match(v.facts, /Solar only/);
    const corrected = carTiles([tesla, volvo], [{ ...garage, car_corrected: true, car_corrected_from: tesla.id }])[1];
    assert.equal(corrected.knownBy, 'The car said so · not Model Y');
  });

  it('does not call a car unplugged while a charger asks whether it is that car', () => {
    const asks = { id: 'garage', plugged_in: true, session: 's1', car_identified_by: 'asking', choose: [tesla.id, kia.id] };
    const carport = { ...asks, id: 'carport', session: 's2', choose: [tesla.id] };
    const tiles = carTiles([tesla, volvo, kia], [asks, carport]);
    assert.deepEqual(tiles.map(t => t.where), ['On garage or carport?', 'Not plugged in', 'On garage?']);
    assert.equal(tiles[0].charger, null, 'a car the box asks about is on no charger yet');
  });

  it('says the charger status is unknown when the chargers’ read is old', () => {
    // The cars answered, the chargers did not: the level stands, the place does not.
    const [, v] = carTiles([tesla, volvo], [garage], 0, false);
    assert.equal(v.where, 'Charger status unknown');
    assert.equal(v.charger, null);
    assert.equal(v.knownBy, '');
    const [, quiet] = carTiles([tesla, volvo], [{ ...garage, charger: { available: false } }]);
    assert.equal(quiet.where, 'Charger status unknown', 'a charger that went quiet cannot say the car is on it');
    assert.equal(quiet.charger, null);
  });

  it('shows the charge limit only beside the car’s own reading', () => {
    assert.match(carTiles([tesla])[0].facts, /^Limit 80%/);
    assert.doesNotMatch(carTiles([{ ...tesla, soc_source: 'manual' }])[0].facts, /Limit/);
    assert.doesNotMatch(carTiles([{ ...tesla, soc_source: 'assumed' }])[0].facts, /Limit/);
  });

  it('does not put a car on a charger that has been unplugged', () => {
    assert.equal(carTiles([volvo], [{ ...garage, plugged_in: false }])[0].where, 'Not plugged in');
  });
});

describe('a charger whose car the box cannot name asks', () => {
  const askingLp = { id: 'garage', plugged_in: true, session: 's1', car_identified_by: 'asking', choose: [tesla.id, kia.id] };
  const carport = { id: 'carport', plugged_in: true, session: 's2', car_id: volvo.id, car_identified_by: 'only' };
  const cars = [tesla, volvo, kia];

  it('titles the notice by the car, or with the question', () => {
    assert.equal(noticeTitle(askingLp, cars), 'Which car is on garage?');
    assert.equal(noticeTitle(carport, cars), 'XC40 connected');
    assert.equal(noticeTitle({ ...carport, car_id: '', car_identified_by: 'none' }, cars), 'Car connected');
    assert.ok(asking(askingLp) && !asking(carport));
  });

  it('offers the box’s own choices while it asks, else every car not on another charger', () => {
    assert.deepEqual(choices(askingLp, cars, [askingLp, carport]).map(c => c.name), ['Model Y', 'EV6']);
    assert.deepEqual(choices(carport, cars, [askingLp, carport]).map(c => c.name), ['Model Y', 'XC40', 'EV6']);
    assert.deepEqual(choices({ ...askingLp, plugged_in: false }, cars, []), []);
    assert.deepEqual(choices({ ...askingLp, session: '' }, cars, []), [], 'a tap must name its plug-in');
  });

  it('shows one button per car and None of these, and a tap names the plug-in', async () => {
    const doc = fakeDoc();
    const taps = [];
    const which = createWhichCar(doc, { onTap: async (lp, id) => { taps.push([lp.session, id]); return { ok: true }; } });
    which.update(askingLp, cars, [askingLp, carport], true);
    const picks = buttons(which.el).filter(b => b.className === 'car-which-pick');
    assert.deepEqual(picks.map(b => b.textContent), ['Model Y', 'EV6', 'None of these']);
    assert.equal(which.el.hidden, false);
    await picks[1].click();
    await picks[2].click();
    assert.deepEqual(taps, [['s1', kia.id], ['s1', '']]);
  });

  it('keeps its buttons across polls that change nothing', () => {
    const doc = fakeDoc();
    const which = createWhichCar(doc, { onTap: async () => ({ ok: true }) });
    which.update(askingLp, cars, [askingLp], true);
    const before = buttons(which.el)[1];
    which.update({ ...askingLp }, cars.map(c => ({ ...c })), [askingLp], true);
    assert.equal(buttons(which.el)[1], before, 'a poll took the button from under the finger');
    which.update({ ...askingLp, choose: [tesla.id] }, cars, [askingLp], true);
    assert.notEqual(buttons(which.el)[1], before);
  });

  it('hides the choice when the charger’s data is out of date', () => {
    const which = createWhichCar(fakeDoc(), { onTap: async () => ({ ok: true }) });
    which.update(askingLp, cars, [askingLp], false);
    assert.equal(which.el.hidden, true);
  });

  it('lets an automatic answer be corrected, behind one button', async () => {
    const which = createWhichCar(fakeDoc(), { onTap: async () => ({ ok: true }) });
    which.update(carport, cars, [carport], true);
    const [open] = buttons(which.el);
    assert.equal(open.textContent, 'Not this car?');
    const row = which.el.children.find(c => c.className === 'car-which-row');
    assert.equal(row.hidden, true);
    await open.click();
    assert.equal(row.hidden, false);
    const current = buttons(which.el).find(b => b.textContent === 'XC40');
    assert.equal(current.getAttribute('aria-pressed'), 'true');
  });

  it('says a tap needs the house password rather than that the box refused it', () => {
    assert.match(tapRefusedText({ error: 'valid LAN password required' }, 401), /house password/);
    assert.equal(tapRefusedText({ error: 'only an owner' }, 403), 'This device may not choose the car.');
  });

  it('gives up on a tap the box does not answer', async () => {
    const fetchFn = (path, opts) => new Promise((_, reject) => {
      opts.signal.addEventListener('abort', () => reject(new Error('aborted')));
    });
    assert.deepEqual(await tap(fetchFn, { id: 'garage', session: 's1' }, kia.id, 10),
      { ok: false, text: 'The box did not answer. Try again.' });
    assert.equal(TAP_TIMEOUT_MS, 30000, 'as long as the EV modal waits');
  });

  it('says why the box refused a tap', async () => {
    assert.equal(tapRefusedText({ reason: 'session' }), 'That plug-in has ended.');
    assert.equal(tapRefusedText({ reason: 'elsewhere' }), 'That car is on another charger.');
    assert.equal(tapRefusedText(null), 'The box did not take that. Try again.');
    const sent = [];
    const fetchFn = async (path, opts) => {
      sent.push([path, opts.method, JSON.parse(opts.body)]);
      return { ok: false, status: 409, json: async () => ({ code: 'E_PRECONDITION', reason: 'session' }) };
    };
    const res = await tap(fetchFn, { id: 'gar age', session: 's1' }, kia.id);
    assert.deepEqual(sent, [['/api/loadpoints/gar%20age/car', 'POST', { session: 's1', car_id: kia.id }]]);
    assert.deepEqual(res, { ok: false, text: 'That plug-in has ended.' });
  });
});

describe('the card of cars', () => {
  it('hides with no cars and says when its list is old', () => {
    const doc = fakeDoc();
    const el = doc.createElement('section');
    const card = createCarsCard(doc, el);
    card.update({ cars: [] });
    assert.equal(el.hidden, true);
    const fetchedAt = Date.UTC(2026, 9, 17, 9, 0);
    card.update({ cars: [tesla], fetchedAt, now: fetchedAt + 60000, fresh: false });
    assert.equal(el.hidden, false);
    assert.match(el.textContent, /Not updated since/);
    assert.match(el.textContent, /1 h 1 min ago/, 'the age counts the time since the read');
  });

  it('puts a car’s name in as text', () => {
    const doc = fakeDoc();
    const el = doc.createElement('section');
    createCarsCard(doc, el).update({ cars: [{ ...kia, name: '<img src=x onerror=alert(1)>' }], fetchedAt: 1, now: 1 });
    const name = el.all(c => c.className === 'car-tile-name')[0];
    assert.equal(name.textContent, '<img src=x onerror=alert(1)>');
    assert.doesNotMatch(read('./cars.js'), /innerHTML/);
  });

  it('opens the charger’s controls from the car on it', async () => {
    const doc = fakeDoc();
    const el = doc.createElement('section');
    const opened = [];
    const card = createCarsCard(doc, el, { onOpenCharger: lp => opened.push(lp.driver_name) });
    const lp = { id: 'garage', driver_name: 'easee', plugged_in: true, session: 's1', car_id: tesla.id, car_identified_by: 'tap' };
    card.update({ cars: [tesla, volvo], loadpoints: [lp], fetchedAt: 1, now: 1 });
    const open = buttons(el);
    assert.equal(open.length, 1);
    assert.equal(open[0].textContent, 'Check charging');
    await open[0].click();
    assert.deepEqual(opened, ['easee']);
    assert.match(el.textContent, /You chose it/);
  });
});

describe('the dashboard wiring', () => {
  const index = read('./index.html');
  const app = read('./app.js');
  const css = read('./app.css');
  const overview = index.slice(index.indexOf('<main id="view-overview"'), index.indexOf('</main>'));

  it('reads the cars with the chargers on every poll', () => {
    assert.match(app, /boundedApiRead\("\/api\/cars"/);
    assert.match(app, /import\("\/cars\.js"\)/);
    assert.match(app, /ui\.noticeTitle\(lp, lastCars\)/);
    assert.match(app, /row\.which\.update\(lp, lastCars, chargingNoticePoints, available\)/);
  });

  it('dates the chargers’ read apart from the cars’', () => {
    assert.match(app, /loadpointsFetchedAt = Date\.now\(\)/);
    assert.match(app, /chargersFresh: !document\.hidden && Date\.now\(\) - loadpointsFetchedAt < 15000/);
  });

  it('places the cars after Power now and spans the row', () => {
    assert.ok(overview.indexOf('id="power-now"') < overview.indexOf('id="overview-cars"'));
    assert.ok(overview.indexOf('id="overview-cars"') < overview.indexOf('id="overview-price"'));
    assert.match(css, /body\.ftw-app #overview-cars\s*\{[^}]*grid-column:\s*1\s*\/\s*-1/);
    assert.match(css, /body\.ftw-app #charging-notices,\s*body\.ftw-app #overview-cars,/);
  });
});
