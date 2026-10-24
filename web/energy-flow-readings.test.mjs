import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { flowReadingsFromStatus, fmtKwhShort } from "./components/energy-flow-readings.js";

it("shows current household power without a note about the past energy mean", () => {
  const estimate = {watts: 886, from_ms: Date.UTC(2026,9,18,10), to_ms: Date.UTC(2026,9,18,10,1), age_ms: 90000, end_age_ms:60000, energy_basis:'mixed', timing_basis: 'mixed'};
  const status = {load_w: 1800, load_quality: "measured", load_estimate: estimate, energy:{today:{load_wh:2000,import_wh:500,history_quality:'estimated'}}};
  const result = flowReadingsFromStatus(status);
  assert.equal(result.load, 1.8);
  // An older Core also sent an energy mean; it never marks live Home use.
  assert.equal(result.loadEstimated, false);
  assert.equal(status.load_w, 1800);
  assert.equal(result.selfPoweredPctToday, null);
  assert.equal("homeEstimateNote" in result, false);
  assert.equal(flowReadingsFromStatus({...status,load_estimate:{watts:null}}).load, 1.8);
  assert.equal(flowReadingsFromStatus({...status,load_estimate:{...estimate,end_age_ms:181000}}).load, 1.8);
  assert.equal(flowReadingsFromStatus({...status,load_w:null}).load, null);
});

it("marks Home use estimated only when Core says so", () => {
  for (const [quality, estimated] of [["measured", false], [undefined, false], ["estimated", true], ["unknown", false]]) {
    const result = flowReadingsFromStatus({load_w: 640, load_quality: quality, history_quality: "complete"});
    assert.equal(result.load, 0.64);
    assert.equal(result.loadEstimated, estimated, String(quality));
  }
});

it("never lets a past zero mean overwrite a positive current Load", () => {
  const status={load_w:1234,history_quality:"complete",load_estimate:{watts:0,from_ms:1000,to_ms:11000,end_age_ms:60000}};
  const result=flowReadingsFromStatus(status);
  assert.equal(result.load,1.234);
  const missing=flowReadingsFromStatus({...status,load_w:null,history_quality:"incomplete"});
  assert.equal(missing.load,null);
});

it("keeps a qualified power balance visible while the energy interval is unavailable", () => {
  for (const reason of ["incomplete_source_window", "charger_source_time_unavailable", "stale_source_window"]) {
    const status = {load_w: 1234, history_quality: "complete", load_quality: "estimated", load_estimate:{watts:null, reason}};
    const result = flowReadingsFromStatus(status);
    assert.equal(result.load, 1.234);
    assert.equal(result.loadEstimated, true);
    assert.equal(status.load_estimate.watts, null);
  }
});

it("distinguishes a real zero balance from missing, invalid or stale inputs", () => {
  const base = {history_quality:"complete",load_estimate:{watts:null,reason:"incomplete_source_window"}};
  assert.equal(flowReadingsFromStatus({...base,load_w:0}).load,0);
  // A negative balance is shown: it points at production Core cannot see.
  assert.equal(flowReadingsFromStatus({...base,load_w:-300}).load,-0.3);
  for (const value of [undefined, null, "0", NaN, Infinity]) {
    const result = flowReadingsFromStatus({...base,load_w:value});
    assert.equal(result.load,null);
  }
});

it("renders Core load validity without another history-quality decision", () => {
  for (const quality of ["complete", "incomplete", "balance_mismatch"]) {
    const status = {history_quality: quality, load_estimate: {watts: null}};
    assert.equal(flowReadingsFromStatus({...status, load_w: 1234}).load, 1.234);
    assert.equal(flowReadingsFromStatus({...status, load_w: 0}).load, 0);
    assert.equal(flowReadingsFromStatus({...status, load_w: null}).load, null);
  }
});

const LIVE = {
  grid_w: 500,
  load_w: 970,
  energy: {
    today: {
      import_wh: 5200,
      export_wh: 12_400,
      pv_wh: 18_100,
      load_wh: 14_000,
      bat_charged_wh: 4100,
      bat_discharged_wh: 2800,
    },
  },
  drivers: {
    east: { status: "ok", pv_w: -1800 },
    west: { status: "ok", pv_w: -500 },
    lynx: { status: "ok", bat_w: 1800, bat_soc: 0.687 },
    easee: { status: "ok", ev_w: 7200 },
    dead: { status: "offline", pv_w: -900 },
  },
};

describe("flowReadingsFromStatus", () => {
  it("keeps a missing solar day total unknown instead of inventing zero", () => {
    const status = structuredClone(LIVE);
    status.energy.today.pv_wh = null;
    const solar = flowReadingsFromStatus(status).planets.find(p => p.role === "pv");
    assert.equal(solar.dailyKwh, "Unknown");
  });

  it("marks measured day energy after a gap without inferring a daily share", () => {
    const status = structuredClone(LIVE);
    status.energy.today.pv_wh = null;
    status.energy.today.load_wh = null;
    status.energy.today.measured = {pv_wh: 600, load_wh: 1200};
    const result = flowReadingsFromStatus(status);
    assert.equal(result.planets.find(p => p.role === "pv").dailyKwh, "≥ 0.60 kWh");
    assert.equal(result.selfPoweredPctToday, null);
  });

  it("draws one planet per live driver and hides a spare offline inverter", () => {
    const r = flowReadingsFromStatus(LIVE);
    assert.deepEqual(
      r.planets.map((p) => p.id).sort(),
      ["bat-lynx", "ev-easee", "grid", "pv-east", "pv-west"],
    );
    assert.equal(
      r.planets.some((p) => p.id === "pv-dead"),
      false,
      "an extra offline inverter became a planet next to live solar",
    );
    assert.equal(r.load, 0.97);
  });

  it("keeps a faulted charger on the diagram", () => {
    const r = flowReadingsFromStatus({
      grid_w: 0,
      load_w: 200,
      drivers: { easee: { status: "fault", ev_w: 11_400 } },
    });
    assert.equal(r.planets.find((p) => p.id === "ev-easee")?.kw, 11.4);
  });

  it("still says balanced for a real 0 W meter reading", () => {
    const r = flowReadingsFromStatus({
      grid_w: 0,
      load_w: 200,
      drivers: { easee: { status: "ok", ev_w: 0 } },
    });
    const grid = r.planets.find((p) => p.id === "grid");
    assert.equal(grid.sub, "balanced");
    assert.equal(grid.placeholder, undefined);
    assert.equal(r.load, 0.2);
  });

  it("says no data instead of 0 W balanced when the meter is missing", () => {
    const r = flowReadingsFromStatus({
      grid_w: null,
      load_w: null,
      drivers: { easee: { status: "ok", ev_w: 0 } },
    });
    const grid = r.planets.find((p) => p.id === "grid");
    assert.equal(grid.sub, "no data");
    assert.equal(grid.placeholder, true);
    assert.equal(grid.clickable, false);
    assert.equal(r.load, null);
  });

  it("keeps solar and battery on the diagram when the only inverter goes quiet", () => {
    // The phone screenshot: a hybrid that is the meter, the PV and the
    // battery goes offline. Skipping it left GRID + EV at 0 W and looked
    // like the house never had solar.
    const r = flowReadingsFromStatus({
      grid_w: null,
      load_w: null,
      drivers: {
        ferroamp: { status: "offline", pv_w: -3400, bat_w: 900, bat_soc: 0.62 },
        easee: { status: "ok", ev_w: 0 },
      },
    });
    const ids = r.planets.map((p) => p.id).sort();
    assert.deepEqual(ids, ["bat-ferroamp", "ev-easee", "grid", "pv-ferroamp"]);
    const solar = r.planets.find((p) => p.id === "pv-ferroamp");
    const battery = r.planets.find((p) => p.id === "bat-ferroamp");
    const grid = r.planets.find((p) => p.id === "grid");
    assert.equal(solar.placeholder, true);
    assert.equal(solar.sub, "no data");
    assert.equal(battery.placeholder, true);
    assert.equal(grid.placeholder, true);
    const ev = r.planets.find((p) => p.id === "ev-easee");
    assert.equal(ev.placeholder, undefined);
    assert.equal(ev.sub, "idle");
  });

  it("writes today onto live bubbles", () => {
    const r = flowReadingsFromStatus(LIVE);
    const grid = r.planets.find((p) => p.id === "grid");
    assert.deepEqual(grid.dailyKwhParts?.map((p) => p.text), ["↓ 5.20", "↑ 12.4"]);
    const solar = r.planets.find((p) => p.id === "pv-east");
    assert.equal(solar.dailyKwh, "18.1 kWh");
    assert.ok(Math.abs(r.selfPoweredPctToday - (1 - 5.2 / 14) * 100) < 1e-6);
  });

  it("keeps today's self-powered share when the meter is currently quiet", () => {
    const r = flowReadingsFromStatus({
      grid_w: null,
      load_w: null,
      energy: LIVE.energy,
      drivers: {
        ferroamp: { status: "offline", pv_w: -3400, bat_w: 900, bat_soc: 0.62 },
      },
    });
    assert.equal(r.load, null);
    assert.ok(Math.abs(r.selfPoweredPctToday - (1 - 5.2 / 14) * 100) < 1e-6);
  });

  it("keeps battery sign so two discharging packs do not look like charging", () => {
    const r = flowReadingsFromStatus({
      grid_w: 0,
      load_w: 3500,
      drivers: {
        a: { status: "ok", bat_w: -2000, bat_soc: 0.4 },
        b: { status: "ok", bat_w: -1500, bat_soc: 0.5 },
      },
    });
    const bats = r.planets.filter((p) => p.role === "battery");
    assert.ok(Math.abs(bats.reduce((sum, p) => sum + p.kw, 0) + 3.5) < 1e-9);
    assert.ok(bats.every((p) => p.sub === "discharging"));
  });
});

describe("fmtKwhShort", () => {
  it("matches the dashboard bubble rounding", () => {
    assert.equal(fmtKwhShort(5.2), "5.20");
    assert.equal(fmtKwhShort(12.4), "12.4");
    assert.equal(fmtKwhShort(100.6), "101");
  });
});

it('marks only devices that need attention and keeps an offline sibling visible without old watts', () => {
  const status = {grid_w:1000,load_w:500,drivers:{a:{status:'ok',bat_w:1000},b:{status:'offline',bat_w:5000}},control_feedback:[
    {driver:'a',kind:'battery',status:'following',reason:'power_observed',severity:'info',evidence:'confirmed',readings_fresh:true},
    {driver:'b',kind:'battery',status:'no_contact',reason:'readings_lost',severity:'alarm',evidence:'accepted',readings_fresh:false,actual_w:null},
  ]};
  const planets=flowReadingsFromStatus(status).planets;
  assert.equal(planets.find(p=>p.name==='a').controlMark,undefined);
  const failed=planets.find(p=>p.name==='b');
  assert.deepEqual(failed.controlMark,{tone:'alarm',label:'Lost control'});
  assert.equal(failed.placeholder,true);
  assert.equal(failed.kw,0);
  assert.equal(failed.clickable,true);
  assert.equal(flowReadingsFromStatus(status,{live:false}).planets.find(p=>p.name==='b').controlMark,undefined);
});

it("draws Core's estimated Home use as an estimate, not a blank", () => {
  const status = {load_w: 980, load_quality: "estimated", load_reason: "offline:growatt:pv", history_quality: "incomplete"};
  const result = flowReadingsFromStatus(status);
  assert.equal(result.load, 0.98);
  assert.equal(result.loadEstimated, true);
  assert.equal(flowReadingsFromStatus({load_w: 980, load_quality: "measured"}).loadEstimated, false);
});
