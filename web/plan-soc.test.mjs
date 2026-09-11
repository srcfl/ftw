import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { describe, it } from "node:test";

import { derivePlanBrief } from "./plan-brief.js";
import { actionSoC, fillPlanSoC, finiteNumber } from "./plan-soc.js";

const planSource = readFileSync(new URL("./plan.js", import.meta.url), "utf8");

// Björn / MilliWatt help dump 2026-08-27 20:33 UTC: 20 kWh Pixii, live
// 53.5%, first slot battery -577 W for 15 min, Charge end 52.7%.
const BJORN = {
  capacityWh: 20000,
  initialSoC: 0.535,
  batteryW: -577,
  slotLenMin: 15,
  chargeEff: 0.95,
  dischargeEff: 0.95,
};

describe("plan SoC reconstruction", () => {
  it("prefers a finite soc over stored energy or integration", () => {
    const soc = actionSoC(
      { soc: 0.527, battery_w: 7300, storage_energy_wh: { pixii: 0 } },
      { capacityWh: 20000, prevSoC: 0.40 },
    );
    assert.equal(soc, 0.527);
  });

  it("accepts legacy soc_pct as a 0–100 value", () => {
    const soc = actionSoC(
      { soc_pct: 52.7, battery_w: 7300, storage_energy_wh: { pixii: 0 } },
      { capacityWh: 20000, prevSoC: 0.40 },
    );
    assert.equal(soc, 0.527);
  });

  it("uses summed storage_energy_wh when soc is missing", () => {
    const soc = actionSoC(
      { battery_w: 7300, storage_energy_wh: { pixii: 10540 } },
      { capacityWh: 20000, prevSoC: 0.40 },
    );
    assert.equal(soc, 0.527);
  });

  it("replays the Björn first slot from battery_w and efficiencies", () => {
    const soc = actionSoC(
      { battery_w: BJORN.batteryW, slot_len_min: BJORN.slotLenMin },
      {
        capacityWh: BJORN.capacityWh,
        prevSoC: BJORN.initialSoC,
        chargeEff: BJORN.chargeEff,
        dischargeEff: BJORN.dischargeEff,
      },
    );
    assert.ok(finiteNumber(soc));
    assert.ok(Math.abs(soc - 0.527) < 0.0005, `got ${soc}, want ~0.527`);
  });

  it("defaults missing efficiencies to 0.95 like mpc.Optimize", () => {
    const omitted = actionSoC(
      { battery_w: BJORN.batteryW, slot_len_min: BJORN.slotLenMin },
      { capacityWh: BJORN.capacityWh, prevSoC: BJORN.initialSoC },
    );
    const explicit = actionSoC(
      { battery_w: BJORN.batteryW, slot_len_min: BJORN.slotLenMin },
      {
        capacityWh: BJORN.capacityWh,
        prevSoC: BJORN.initialSoC,
        chargeEff: 0.95,
        dischargeEff: 0.95,
      },
    );
    const lossless = actionSoC(
      { battery_w: BJORN.batteryW, slot_len_min: BJORN.slotLenMin },
      {
        capacityWh: BJORN.capacityWh,
        prevSoC: BJORN.initialSoC,
        chargeEff: 1,
        dischargeEff: 1,
      },
    );
    assert.equal(omitted, explicit);
    assert.notEqual(omitted, lossless);
  });

  it("does not treat a missing soc as 0", () => {
    assert.equal(actionSoC({ battery_w: 7300 }, {}), null);
    assert.equal(finiteNumber(null), false);
    assert.equal(finiteNumber(undefined), false);
    assert.equal(finiteNumber(NaN), false);
    assert.equal(finiteNumber(0), true);
  });

  it("fills a whole plan so later slots follow the first reconstructed SoC", () => {
    const plan = {
      capacity_wh: 20000,
      initial_soc: 0.535,
      actions: [
        { slot_len_min: 15, battery_w: -577 },
        { slot_len_min: 15, battery_w: -577 },
      ],
    };
    fillPlanSoC(plan, { chargeEff: 0.95, dischargeEff: 0.95 });
    assert.ok(Math.abs(plan.actions[0].soc - 0.527) < 0.0005);
    assert.ok(plan.actions[1].soc < plan.actions[0].soc);
    assert.ok(plan.actions[1].soc > 0.51);
  });

  it("leaves an already-finite soc untouched", () => {
    const plan = {
      capacity_wh: 20000,
      initial_soc: 0.535,
      actions: [{ slot_len_min: 15, battery_w: -10000, soc: 0.527 }],
    };
    fillPlanSoC(plan, { chargeEff: 0.95, dischargeEff: 0.95 });
    assert.equal(plan.actions[0].soc, 0.527);
  });

  it("clamps reconstructed SoC instead of drawing past 0–1", () => {
    const plan = {
      capacity_wh: 20000,
      initial_soc: 0.535,
      actions: Array.from({ length: 40 }, () => ({
        slot_len_min: 15,
        battery_w: 10000,
      })),
    };
    fillPlanSoC(plan, { chargeEff: 0.95, dischargeEff: 0.95 });
    assert.equal(plan.actions[0].soc < 1, true);
    assert.equal(plan.actions.at(-1).soc, 1);
  });
});

describe("plan UI uses reconstructed SoC", () => {
  it("loads fillPlanSoC before drawing or describing the plan", () => {
    assert.match(planSource, /import \{ fillPlanSoC \} from "\.\/plan-soc\.js"/);
    assert.match(planSource, /import \{ derivePlanBrief, unavailablePlannerCopy \} from "\.\/plan-brief\.js"/);
    assert.match(planSource, /fillPlanSoC\(/);
    assert.match(planSource, /socPercent\(a\.soc\)/);
    assert.match(planSource, /socPercent\(plan\.initial_soc\)/);
    assert.equal(planSource.includes("socPercent(a.soc) || 0"), false);
    assert.equal(planSource.includes("socPercent(plan.initial_soc) || 0"), false);
    assert.match(planSource, /if \(endSoc == null\) continue/);
  });

  it("surfaces Expected charge from a Björn-like plan that omitted soc", () => {
    const now = Date.now();
    const plan = {
      capacity_wh: 20000,
      initial_soc: 0.535,
      actions: [{
        slot_start_ms: now - 7 * 60_000,
        slot_len_min: 15,
        battery_w: -577,
        loadpoint_w: 0,
        pv_limit_w: 0,
        reason: "discharge — price above horizon mean",
        confidence: 1,
      }],
      solver: { engine: "cvxpy", backend: "highs", status: "optimal" },
    };
    const brief = derivePlanBrief({
      enabled: true,
      plan,
      status: { mode: "planner_passive_arbitrage", bat_soc: 0.535 },
      now,
      socOpts: { chargeEff: 0.95, dischargeEff: 0.95 },
    });
    assert.match(brief.soc.label, /53% after next step/);
    assert.match(brief.soc.detail, /53% at the end of the plan/);
  });
});
