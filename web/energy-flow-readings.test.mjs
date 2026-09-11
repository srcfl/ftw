import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { flowReadingsFromStatus, fmtKwhShort } from "./components/energy-flow-readings.js";

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
