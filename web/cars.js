// The owner's cars on the dashboard (docs/cars.md). A car is not the charger:
// it shows while unplugged, with its level, where the level came from and how
// old it is. A charger in use whose car the box cannot name asks which car it
// is, and a tap answers with session.car.set's route.
//
// Names that come from outside, a car's name or a charger's id, only ever go
// into the page as text.

const SOURCE_WORDS = { vehicle: 'From the car', manual: 'Set by hand', assumed: 'Assumed at plug-in' };

// How the box knows which car is on a charger (car_identified_by).
const KNOWN_BY = { charger: 'the charger named it', car: 'the car said so', only: 'the only car', tap: 'you chose it' };

const DAY_NAMES = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'];

// A tap the box refused, in words, by the op's reason.
const TAP_REFUSED = {
  session: 'That plug-in has ended.',
  elsewhere: 'That car is on another charger.',
  unplugged: 'Nothing is plugged in now.',
  car: 'The box no longer has that car.',
  charger: 'The box no longer has that charger.',
};

const pct = permille => `${Math.round(permille / 10)}%`;

export function ageText(s) {
  if (!Number.isFinite(s) || s < 0) return '';
  if (s < 60) return 'just now';
  const min = Math.floor(s / 60);
  if (min < 60) return `${min} min ago`;
  const h = Math.floor(min / 60);
  if (h < 24) return min % 60 && h < 10 ? `${h} h ${min % 60} min ago` : `${h} h ago`;
  const d = Math.floor(h / 24);
  return d === 1 ? '1 day ago' : `${d} days ago`;
}

function kWh(wh) {
  const v = Math.abs(wh) / 1000;
  return `${wh < 0 ? '− ' : '+ '}${v >= 10 ? v.toFixed(0) : v.toFixed(1)} kWh`;
}

// levelView is the car's level in words. sinceS is the time since the box
// counted soc_age_s, so the age stays true between reads.
export function levelView(car = {}, sinceS = 0) {
  const known = Number.isFinite(car.soc_permille);
  if (!known) return { value: '—', source: 'No level yet', known: false, estimate: false };
  const estimate = car.soc_estimate === true;
  const age = Number.isFinite(car.soc_age_s) ? ageText(car.soc_age_s + Math.max(0, sinceS)) : '';
  let source = SOURCE_WORDS[car.soc_source] || 'Estimated';
  if (car.soc_source !== 'assumed' && age) source += ` · ${age}`;
  if (estimate && car.soc_source !== 'assumed' && Number.isFinite(car.energy_since_wh) && car.energy_since_wh !== 0) {
    source += ` ${kWh(car.energy_since_wh)}`;
  }
  return { value: (estimate ? '≈ ' : '') + pct(car.soc_permille), source, known: true, estimate };
}

export function daysText(mask) {
  if (!mask) return 'once';
  if (mask === 0x7f) return 'every day';
  if (mask === 0x1f) return 'weekdays';
  if (mask === 0x60) return 'weekends';
  return DAY_NAMES.filter((_, i) => mask & (1 << i)).join(', ');
}

function clock(min) {
  return `${String(Math.floor(min / 60)).padStart(2, '0')}:${String(min % 60).padStart(2, '0')}`;
}

export function goalText(goal) {
  if (!goal) return 'No goal';
  const what = goal.finish_at_limit ? 'Car’s limit' : pct(goal.target_permille || 0);
  return `${what} by ${clock(goal.ready_by_min || 0)}, ${daysText(goal.days)}`;
}

// The charger a car is on now, if any.
export function chargerOf(car, loadpoints = []) {
  return loadpoints.find(lp => lp && lp.plugged_in && lp.car_id && lp.car_id === car.id) || null;
}

// How the box knows the car on a charger, and a correction it made, in words.
export function knownByText(lp, names = {}) {
  const parts = [];
  if (KNOWN_BY[lp.car_identified_by]) parts.push(KNOWN_BY[lp.car_identified_by]);
  if (lp.car_corrected && lp.car_corrected_from) parts.push(`not ${names[lp.car_corrected_from] || 'the car first named'}`);
  const text = parts.join(' · ');
  return text.charAt(0).toUpperCase() + text.slice(1);
}

// The cars a tap may pick on a charger: the box's own list while it asks,
// otherwise every car not on another charger.
export function choices(lp, cars = [], loadpoints = []) {
  if (!lp || !lp.plugged_in || !lp.session) return [];
  const byId = new Map(cars.map(c => [c.id, c]));
  if (lp.car_identified_by === 'asking' && Array.isArray(lp.choose) && lp.choose.length) {
    return lp.choose.map(id => byId.get(id)).filter(Boolean);
  }
  const elsewhere = new Set(loadpoints.filter(o => o && o.id !== lp.id && o.plugged_in && o.car_id).map(o => o.car_id));
  return cars.filter(c => !elsewhere.has(c.id));
}

export function asking(lp) {
  return !!(lp && lp.plugged_in && lp.car_identified_by === 'asking');
}

// The notice's title for a charger in use.
export function noticeTitle(lp, cars = []) {
  if (asking(lp)) return `Which car is on ${lp.id}?`;
  const car = lp && lp.car_id && cars.find(c => c.id === lp.car_id);
  return car ? `${car.name} connected` : 'Car connected';
}

// tapRefusedText says why a tap did not land. status is the HTTP status:
// a 401 is a device that has not signed in with the house password (the
// page asked and the person cancelled), not a refusal of the car.
export function tapRefusedText(body, status) {
  if (status === 401) return 'Choosing the car needs the house password. Sign in and try again.';
  if (status === 403) return 'This device may not choose the car.';
  return (body && TAP_REFUSED[body.reason]) || 'The box did not take that. Try again.';
}

// TAP_TIMEOUT_MS is how long a tap waits for the box, as the EV modal's
// writes do.
export const TAP_TIMEOUT_MS = 30000;

// tap sends session.car.set for the plug-in the page shows. An empty carId
// is "None of these", a visitor's car. It gives up after TAP_TIMEOUT_MS.
export async function tap(fetchFn, lp, carId, timeoutMs = TAP_TIMEOUT_MS) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), timeoutMs);
  try {
    const r = await fetchFn(`/api/loadpoints/${encodeURIComponent(lp.id)}/car`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ session: lp.session, car_id: carId }), signal: controller.signal,
    });
    if (r.ok) return { ok: true };
    const body = await r.json().catch(() => null);
    return { ok: false, text: tapRefusedText(body, r.status) };
  } catch {
    return { ok: false, text: 'The box did not answer. Try again.' };
  } finally {
    clearTimeout(timer);
  }
}

function node(doc, tag, className, parent, text) {
  const el = doc.createElement(tag);
  if (className) el.className = className;
  if (text != null) el.textContent = text;
  if (parent) parent.appendChild(el);
  return el;
}

// createWhichCar is the choice under a charger's notice: one button per car
// and "None of these". While the box asks it shows at once; otherwise a
// "Not this car?" button opens it, since a tap may correct the box.
export function createWhichCar(doc, { onTap }) {
  const el = node(doc, 'div', 'car-which');
  const open = node(doc, 'button', 'car-which-open', el);
  open.type = 'button';
  const row = node(doc, 'div', 'car-which-row', el);
  row.setAttribute('role', 'group');
  const note = node(doc, 'p', 'car-which-note', el);
  note.setAttribute('aria-live', 'polite');
  let expanded = false, busy = false, last = null, shown = '';
  open.addEventListener('click', () => { expanded = !expanded; if (last) update(...last); });
  function update(lp, cars, loadpoints, fresh) {
    last = [lp, cars, loadpoints, fresh];
    const list = fresh ? choices(lp, cars, loadpoints) : [];
    const isAsking = asking(lp);
    el.hidden = !list.length;
    open.hidden = isAsking;
    open.textContent = expanded ? 'Cancel' : (lp.car_id ? 'Not this car?' : 'Which car is this?');
    open.setAttribute('aria-expanded', String(expanded));
    row.hidden = !(isAsking || expanded);
    row.setAttribute('aria-label', `Which car is on ${lp.id}`);
    const picks = list.map(c => [c.name, c.id, !isAsking && lp.car_id === c.id]);
    picks.push(['None of these', '', !isAsking && !lp.car_id && lp.car_identified_by === 'none']);
    // Rebuilt only when what it shows changes, so a poll never takes a
    // button from under a finger or the keyboard.
    const key = JSON.stringify([lp.id, lp.session, busy, picks]);
    if (key === shown) return;
    shown = key;
    row.replaceChildren();
    picks.forEach(([label, id, current]) => {
      const b = node(doc, 'button', 'car-which-pick', row, label);
      b.type = 'button';
      b.disabled = busy || current;
      if (current) b.setAttribute('aria-pressed', 'true');
      b.addEventListener('click', async () => {
        busy = true; note.textContent = 'Saving…'; update(...last);
        const res = await onTap(lp, id).catch(() => ({ ok: false, text: 'The box did not answer. Try again.' }));
        busy = false; expanded = false;
        note.textContent = res.ok ? '' : res.text;
        update(...last);
      });
    });
  }
  return { el, update };
}

// carTiles is what the card shows of each car, in words. sinceS is the time
// since the list was read. chargersFresh is false while the chargers' own
// read is old: the car's level still stands, but where it is does not.
export function carTiles(cars = [], loadpoints = [], sinceS = 0, chargersFresh = true) {
  const names = Object.fromEntries(cars.map(c => [c.id, c.name]));
  return cars.map(car => {
    const lp = chargersFresh ? chargerOf(car, loadpoints) : null;
    const level = levelView(car, sinceS);
    const facts = [];
    // The limit is the car's, read with its level; only beside that reading
    // does it say how old it is.
    if (car.soc_source === 'vehicle' && Number.isFinite(car.charge_limit_permille)) {
      facts.push(`Limit ${pct(car.charge_limit_permille)}`);
    }
    facts.push(car.goal ? `Goal ${goalText(car.goal)}` : 'No goal');
    if (car.surplus_only) facts.push('Solar only');
    // While a charger asks, a car it may be is not known to be unplugged.
    const maybe = lp || !chargersFresh ? [] : loadpoints.filter(o => asking(o) && Array.isArray(o.choose) && o.choose.includes(car.id));
    // A charger that has gone quiet cannot say the car is still on it.
    const quiet = !!(lp && lp.charger && lp.charger.available === false);
    const where = !chargersFresh || quiet ? 'Charger status unknown'
      : lp ? `On ${lp.id}` : maybe.length ? `On ${maybe.map(o => o.id).join(' or ')}?` : 'Not plugged in';
    const on = !!lp && !quiet;
    return {
      id: car.id, name: car.name, where,
      level: level.value, estimate: level.estimate, source: level.source, facts: facts.join(' · '),
      charger: on ? { id: lp.id, driver_name: lp.driver_name } : null, knownBy: on ? knownByText(lp, names) : '',
    };
  });
}

// createCarsCard fills the dashboard's card of cars, el, which the page
// holds in its place in the overview.
export function createCarsCard(doc, el, { onOpenCharger } = {}) {
  el.replaceChildren();
  const head = node(doc, 'div', 'overview-card-head', el);
  const titles = node(doc, 'div', '', head);
  node(doc, 'p', 'section-kicker', titles, 'Cars');
  const h2 = node(doc, 'h2', '', titles, 'Your cars');
  h2.id = 'overview-cars-title';
  const age = node(doc, 'span', 'overview-cars-age', head);
  const list = node(doc, 'div', 'car-tiles', el);
  let shown = '';
  function update({ cars = [], loadpoints = [], fetchedAt = 0, now = Date.now(), fresh = true, chargersFresh = true } = {}) {
    el.hidden = !cars.length;
    age.textContent = fresh || !fetchedAt ? ''
      : `Not updated since ${new Date(fetchedAt).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false })}`;
    const tiles = carTiles(cars, loadpoints, fetchedAt ? Math.max(0, (now - fetchedAt) / 1000) : 0, fresh && chargersFresh);
    // Rebuilt only when what it shows changes; ages move once a minute.
    const key = JSON.stringify(tiles);
    if (key === shown) return;
    shown = key;
    const focused = doc.activeElement && doc.activeElement.dataset && doc.activeElement.dataset.openCar;
    list.replaceChildren();
    tiles.forEach(t => {
      const tile = node(doc, 'article', 'car-tile', list);
      tile.dataset.carId = t.id;
      const top = node(doc, 'div', 'car-tile-top', tile);
      node(doc, 'strong', 'car-tile-name', top, t.name);
      node(doc, 'span', 'car-tile-where' + (t.charger ? ' is-plugged' : ''), top, t.where);
      node(doc, 'div', 'car-tile-level' + (t.estimate ? ' is-estimate' : ''), tile, t.level);
      node(doc, 'div', 'car-tile-source', tile, t.source);
      node(doc, 'div', 'car-tile-facts', tile, t.facts);
      if (!t.charger) return;
      const foot = node(doc, 'div', 'car-tile-foot', tile);
      node(doc, 'span', '', foot, t.knownBy);
      if (!onOpenCharger) return;
      const b = node(doc, 'button', 'car-tile-open', foot, 'Check charging');
      b.type = 'button';
      b.dataset.openCar = t.id;
      b.addEventListener('click', () => onOpenCharger(t.charger));
      if (focused === t.id) b.focus();
    });
  }
  return { el, update };
}
