// Status → <ftw-energy-flow> readings.
//
// The dashboard (app.js) feeds the hero through this file. A missing
// grid_w is "no data", never 0 W "balanced". Hardware that exists but
// is offline stays on the diagram as a placeholder — hiding it makes a
// hybrid inverter look like the house never had solar or a battery.
// A spare offline inverter next to a live one of the same role stays
// hidden; that is an extra dead device, not the whole category gone.

// Mirror of ftw-energy-flow.js's FLOW_IDLE_W. Read at use-time so the
// component's window.FTW_FLOW_IDLE_W wins once that module has loaded;
// 42 is the no-modules / unit-test fallback. Do not import the
// component from here — that would pull Custom Elements into Node tests.
function idleW() {
  return (typeof window !== "undefined" && window.FTW_FLOW_IDLE_W) || 42;
}

export function driverOnline(d) {
  const status = typeof d?.status === "string" ? d.status : "";
  return status !== "offline" && status !== "disabled" && d?.not_running !== true;
}

export function num(v) {
  return typeof v === "number" && Number.isFinite(v) ? v : null;
}

function isIdle(w, thresholdW) {
  return Math.abs(w) <= thresholdW;
}

export function fmtKwhShort(kwh) {
  if (kwh == null || !Number.isFinite(kwh)) return "—";
  const v = Math.abs(kwh);
  if (v >= 100) return kwh.toFixed(0);
  if (v >= 10) return kwh.toFixed(1);
  return kwh.toFixed(2);
}

function planetColor(role, watts, thresholdW) {
  if (watts === undefined || watts === null) {
    if (role === "grid" || role === "pv") return "var(--fg-muted)";
    if (role === "battery") return "var(--cyan)";
    if (role === "ev") return "var(--white-s)";
    return "var(--fg)";
  }
  if (role === "grid") {
    return isIdle(watts, thresholdW) ? "var(--fg-muted)" : watts >= 0 ? "var(--red-e)" : "var(--green-e)";
  }
  if (role === "pv") return isIdle(watts, thresholdW) ? "var(--fg-muted)" : "var(--amber)";
  if (role === "battery") {
    return isIdle(watts, thresholdW) ? "var(--cyan)" : watts >= 0 ? "var(--green-e)" : "var(--red-e)";
  }
  if (role === "ev") return isIdle(watts, thresholdW) ? "var(--white-s)" : "var(--green-e)";
  return "var(--fg)";
}

function placeholderPlanet(partial) {
  return {
    kw: 0,
    toHub: true,
    color: "var(--fg-muted)",
    sub: "no data",
    clickable: false,
    placeholder: true,
    ...partial,
  };
}

/**
 * @param {object} status  GET /api/status body
 * @param {object} [opts]
 * @param {number} [opts.idleW]
 * @param {(name: string, driver: object, batW: number) => string} [opts.batterySub]
 */
export function flowReadingsFromStatus(status, opts) {
  const thresholdW = (opts && opts.idleW) || idleW();
  const batterySub = opts && opts.batterySub;
  const planets = [];
  const today = (status && status.energy && status.energy.today) || {};
  const importKwh = (num(today.import_wh) ?? 0) / 1000;
  const exportKwh = (num(today.export_wh) ?? 0) / 1000;
  const pvKwhTotal = (num(today.pv_wh) ?? 0) / 1000;
  const loadKwhTotal = (num(today.load_wh) ?? 0) / 1000;
  const batChargedKwh = (num(today.bat_charged_wh) ?? 0) / 1000;
  const batDischargedKwh = (num(today.bat_discharged_wh) ?? 0) / 1000;

  const pvDailyStr = `${fmtKwhShort(pvKwhTotal)} kWh`;
  const gridDailyParts = [
    { text: `↓ ${fmtKwhShort(importKwh)}`, color: "var(--red-e)", bold: true },
    { text: `↑ ${fmtKwhShort(exportKwh)}`, color: "var(--green-e)", bold: true },
  ];
  const batDailyParts = [
    { text: `↑ ${fmtKwhShort(batChargedKwh)}`, color: "var(--green-e)", bold: true },
    { text: `↓ ${fmtKwhShort(batDischargedKwh)}`, color: "var(--red-e)", bold: true },
  ];

  const gridW = num(status && status.grid_w);
  if (gridW === null) {
    planets.push(placeholderPlanet({
      id: "grid", corner: "bottom-left", title: "GRID", role: "grid",
    }));
  } else {
    const gIdle = isIdle(gridW, thresholdW);
    planets.push({
      id: "grid", corner: "bottom-left", title: "GRID", role: "grid",
      kw: Math.abs(gridW) / 1000, toHub: gridW >= 0,
      color: planetColor("grid", gridW, thresholdW),
      sub: gIdle ? "balanced" : gridW >= 0 ? "importing" : "exporting",
      dailyKwhParts: gridDailyParts,
      clickable: true,
    });
  }

  const drivers = (status && status.drivers) || {};
  const names = Object.keys(drivers);
  let pvDailyMembers = 0;
  let batDailyMembers = 0;
  for (const name of names) {
    const d = drivers[name];
    if (!d) continue;
    if (d.pv_w != null) pvDailyMembers++;
    if (d.bat_w != null) batDailyMembers++;
  }

  const live = { pv: false, battery: false, ev: false };
  const offline = { pv: [], battery: [], ev: [] };

  for (const name of names) {
    const d = drivers[name];
    if (!d) continue;
    const online = driverOnline(d);

    const pvW = num(d.pv_w);
    if (pvW !== null) {
      const planet = {
        id: `pv-${name}`, corner: "top-left", title: "SOLAR", role: "pv", name,
        kw: -pvW / 1000, toHub: true,
        color: planetColor("pv", pvW, thresholdW),
        sub: "",
        dailyKwh: pvDailyStr,
        dailyScope: "aggregate",
        dailyAggregateMembers: pvDailyMembers,
        clickable: true,
      };
      if (online) {
        live.pv = true;
        planets.push(planet);
      } else {
        offline.pv.push(placeholderPlanet({
          id: planet.id, corner: planet.corner, title: planet.title,
          role: planet.role, name,
        }));
      }
    }

    const batW = num(d.bat_w);
    if (batW !== null) {
      const bIdle = isIdle(batW, thresholdW);
      const soc = num(d.bat_soc);
      const defaultSub = d.observe_only === true
        ? "observe only"
        : bIdle ? "idle" : batW >= 0 ? "charging" : "discharging";
      const sub = batterySub ? batterySub(name, d, batW) : defaultSub;
      const planet = {
        id: `bat-${name}`, corner: "top-right", title: "BATTERY", role: "battery", name,
        kw: batW / 1000, toHub: batW < 0,
        color: planetColor("battery", batW, thresholdW),
        sub,
        soc: soc === null ? null : Math.round(soc * 100),
        dailyKwhParts: batDailyParts,
        dailyScope: "aggregate",
        dailyAggregateMembers: batDailyMembers,
        clickable: d.observe_only !== true,
      };
      if (online) {
        live.battery = true;
        planets.push(planet);
      } else {
        offline.battery.push(placeholderPlanet({
          id: planet.id, corner: planet.corner, title: planet.title,
          role: planet.role, name,
        }));
      }
    }

    const evW = num(d.ev_w);
    if (evW !== null) {
      const active = !isIdle(evW, thresholdW);
      const planet = {
        id: `ev-${name}`, corner: "bottom-right", title: "EV CHARGER", role: "ev", name,
        kw: Math.abs(evW) / 1000, toHub: false,
        color: planetColor("ev", evW, thresholdW),
        sub: active ? "charging" : "idle",
        clickable: true,
      };
      if (online) {
        live.ev = true;
        planets.push(planet);
      } else {
        offline.ev.push(placeholderPlanet({
          id: planet.id, corner: planet.corner, title: planet.title,
          role: planet.role, name,
        }));
      }
    }
  }

  for (const role of ["pv", "battery", "ev"]) {
    if (!live[role]) planets.push(...offline[role]);
  }

  const loadW = num(status && status.load_w);
  // Today's share is kWh already in the box, not the live meter. A
  // quiet meter blanks % SELF-POWERED NOW (unknown instantaneous load)
  // but must not hide a day that already happened.
  let selfPoweredPctToday = null;
  if (loadKwhTotal > 0.001) {
    selfPoweredPctToday = Math.max(0, Math.min(100, (1 - importKwh / loadKwhTotal) * 100));
  }

  return {
    load: loadW === null ? null : loadW / 1000,
    planets,
    selfPoweredPctToday,
  };
}

if (typeof window !== "undefined") {
  window.ftwFlowReadingsFromStatus = flowReadingsFromStatus;
  window.ftwDriverOnline = driverOnline;
  // The dashboard script starts before this module. If a status payload
  // arrived in that window, tell it the mapper exists so the hero can
  // paint without waiting for the next poll.
  if (typeof window.ftwOnFlowMapperReady === "function") window.ftwOnFlowMapperReady();
}
