import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { describe, it } from "node:test";
import {
  PLAN_STYLES,
  SAFETY_K_DEFAULT,
  clampSafetyK,
  formatSafetyK,
  trustFromSafetyK,
  safetyK,
  styleForK,
  sunWindowLabel,
  sunLine,
  marginSplitLine,
  extraSunLine,
  prefsKnown,
  isBatterySale,
  exportSentence,
  prefsFromStatus,
  SAFETY_K_STEP,
  prefsQueue,
} from "./plan-prefs.js";

const html = readFileSync(new URL("./index.html", import.meta.url), "utf8");
const app = readFileSync(new URL("./app.js", import.meta.url), "utf8");
const plan = readFileSync(new URL("./plan.js", import.meta.url), "utf8");
const goPrefs = readFileSync(new URL("../go/internal/config/planner_prefs.go", import.meta.url), "utf8");

describe("safety k", () => {
  it("clamps to the fine-tune range and treats junk as Balanced", () => {
    assert.equal(clampSafetyK(0), 0);
    assert.equal(clampSafetyK(0.85), 0.85);
    assert.equal(clampSafetyK("0.85"), 0.85);
    assert.equal(clampSafetyK(2), 2);
    assert.equal(clampSafetyK(2.5), 2);
    assert.equal(clampSafetyK(-1), 0);
    assert.equal(clampSafetyK("nope"), SAFETY_K_DEFAULT);
    assert.equal(clampSafetyK(undefined), SAFETY_K_DEFAULT);
  });

  it("keeps every 0.05 fine-tune step distinct", () => {
    assert.equal(SAFETY_K_STEP, 0.05);
    const seen = new Set();
    for (let i = 0; i <= 40; i++) seen.add(formatSafetyK(i * SAFETY_K_STEP));
    assert.equal(seen.size, 41);
    assert.equal(formatSafetyK(0.85), "0.85");
    assert.equal(formatSafetyK(1), "1");
  });

  it("derives the legacy enum from k the way the box does", () => {
    assert.equal(trustFromSafetyK(0), "bold");
    assert.equal(trustFromSafetyK(0.25), "bold");
    assert.equal(trustFromSafetyK(0.3), "balanced");
    assert.equal(trustFromSafetyK(0.85), "balanced");
    assert.equal(trustFromSafetyK(1.5), "cautious");
    assert.equal(trustFromSafetyK(2), "cautious");
  });

  it("maps the legacy enum onto k, with balanced as the Balanced style", () => {
    assert.equal(safetyK("cautious"), 2);
    assert.equal(safetyK("balanced"), SAFETY_K_DEFAULT);
    assert.equal(safetyK("bold"), 0);
  });
});

describe("planning styles", () => {
  it("runs from keeping the most in the battery to counting most on the forecast", () => {
    assert.deepEqual(PLAN_STYLES.map((s) => s.name), ["Very careful", "Careful", "Balanced", "Bold", "Very bold"]);
    for (let i = 1; i < PLAN_STYLES.length; i++) assert.ok(PLAN_STYLES[i].k < PLAN_STYLES[i - 1].k);
    assert.equal(PLAN_STYLES[PLAN_STYLES.length - 1].k, 0);
    for (const s of PLAN_STYLES) assert.ok(s.k >= 0 && s.k <= 2 && s.text.endsWith("."));
  });

  it("puts Balanced in the middle, as the box default, leaning toward the forecast", () => {
    const middle = PLAN_STYLES[2];
    assert.equal(middle.key, "balanced");
    assert.equal(middle.k, SAFETY_K_DEFAULT);
    assert.ok(middle.k < 1, "the old default k=1 is now Very careful");
    // The Go default is the same number.
    assert.match(goPrefs, new RegExp(`SafetyKDefault = ${SAFETY_K_DEFAULT}\\b`));
  });

  it("names the style of a stored k, and says when it was fine-tuned", () => {
    for (const s of PLAN_STYLES) assert.deepEqual(styleForK(s.k), { style: s, exact: true });
    assert.equal(styleForK(0.15).style.name, "Bold");
    assert.deepEqual(
      [styleForK(0.7).style.name, styleForK(0.7).exact],
      ["Careful", false],
    );
    assert.deepEqual([styleForK(2).style.name, styleForK(2).exact], ["Very careful", false]);
    assert.equal(styleForK("junk").style.name, "Balanced");
  });
});

describe("plan lines", () => {
  it("names the window the chart shows", () => {
    const tomorrow = 1_000_000;
    assert.equal(sunWindowLabel("today", 2_000_000, tomorrow), "Rest of today");
    assert.equal(sunWindowLabel("tomorrow", 2_000_000, tomorrow), "Tomorrow");
    assert.equal(sunWindowLabel("all", 2_000_000, tomorrow), "Today and tomorrow");
    // Before tomorrow's prices arrive the plan ends tonight.
    assert.equal(sunWindowLabel("all", tomorrow, tomorrow), "Rest of today");
  });

  it("says how much of the spare sun the plan counts on", () => {
    const m = (forecast, planned) => ({ sunBeyondKWh: forecast, plannedSunBeyondKWh: planned, sunHeldKWh: 0, useAddedKWh: 0 });
    assert.equal(sunLine(m(12.44, 3.06), "Rest of today"),
      "Rest of today: the forecast expects 12.4 kWh more sun than your home uses. The plan counts on 3.1 kWh of it.");
    assert.equal(sunLine(m(6.4, 6.38), "Tomorrow"),
      "Tomorrow: the forecast expects 6.4 kWh more sun than your home uses. The plan counts on all of it.");
    assert.equal(sunLine(m(0.01, 0), "Rest of today"),
      "Rest of today: the forecast expects no sun beyond what your home uses.");
    assert.equal(sunLine(null, "Rest of today"), null);
  });

  it("splits the margin into sun and use for Settings", () => {
    assert.equal(marginSplitLine({ sunHeldKWh: 0.9, useAddedKWh: 2.4 }),
      "The current plan counts on 0.9 kWh less sun and 2.4 kWh more use than forecast.");
    assert.equal(marginSplitLine({ sunHeldKWh: 0, useAddedKWh: 0.01 }), "The current plan uses the forecast as it is.");
    assert.equal(marginSplitLine(null), null);
  });

  describe("extra sun", () => {
    const slot = (start, pv, soc, cap, battery = 0) => ({ slot_start_ms: start, slot_len_min: 15,
      forecast_pv_w: pv, pv_w: pv, soc, live_pv_surplus_soc_cap: cap, battery_w: battery });
    const q = 15 * 60_000;
    const stores = "Sun beyond what your home and the plan need goes into the battery.";
    const exports = "Sun beyond what your home and the plan need goes to the grid.";

    it("says nothing when the box does not send either cap", () => {
      const actions = [slot(0, -3000, 0.4, 0.8)];
      assert.equal(extraSunLine(actions, 0, q, undefined), null);
      delete actions[0].live_pv_surplus_soc_cap;
      assert.equal(extraSunLine(actions, 0, q, 0), null);
    });

    it("says nothing for a window without sun", () => {
      assert.equal(extraSunLine([slot(0, 0, 0.4, 0.8)], 0, q, 0), null);
    });

    it("stores, sends to the grid, or splits, from Core's per-slot permission", () => {
      assert.equal(extraSunLine([slot(0, -3000, 0.4, 0.8), slot(q, -2000, 0.4, 0.6)], 0, 2 * q, 0), stores);
      assert.equal(extraSunLine([slot(0, -3000, 0.4, 0), slot(q, -2000, 0.4, 0)], 0, 2 * q, 0), exports);
      assert.equal(extraSunLine([slot(0, -3000, 0.4, 0.8), slot(q, -2000, 0.4, 0)], 0, 2 * q, 0),
        "Sun beyond what your home and the plan need goes partly into the battery and partly to the grid.");
    });

    it("needs room above the planned charge to call it stored", () => {
      assert.equal(extraSunLine([slot(0, -3000, 0.8, 0.8)], 0, q, 0), exports);
    });

    it("follows the operator's cap ahead of the plan's, as dispatch does", () => {
      assert.equal(extraSunLine([slot(0, -3000, 0.4, 0)], 0, q, 0.88), stores);
      assert.equal(extraSunLine([slot(0, -3000, 0.9, 0.95)], 0, q, 0.88), exports);
    });

    it("never stores during a planned discharge", () => {
      assert.equal(extraSunLine([slot(0, -3000, 0.4, 0, -2000)], 0, q, 0.88), exports);
    });

    it("says nothing where the plan caps the panels", () => {
      const capped = { ...slot(q, -2000, 0.9, 0), pv_curtail_active: true, pv_limit_w: 500 };
      assert.equal(extraSunLine([slot(0, -3000, 0.4, 0), capped], 0, 2 * q, 0), null);
      assert.equal(extraSunLine([slot(0, -3000, 0.4, 0), { ...slot(q, -2000, 0.9, 0), pv_limit_w: 500 }], 0, 2 * q, 0), null);
    });
  });
});

describe("export sentences", () => {
  const noon = Date.UTC(2026, 7, 21, 10, 0, 0);
  const slot = (start, battery_w, grid_w) => ({
    slot_start_ms: start,
    slot_len_min: 15,
    battery_w,
    grid_w,
  });

  it("names a planned battery sale window", () => {
    const actions = [
      slot(noon, -2000, -1500),
      slot(noon + 15 * 60_000, -1800, -1200),
    ];
    const text = exportSentence({ actions, exportPermission: "allowed", nowMs: noon });
    assert.match(text, /^Battery sale planned \d{2}:\d{2}–\d{2}:\d{2}\.$/);
  });

  it("reports solar export when the battery is not selling", () => {
    const actions = [slot(noon, 0, -800)];
    assert.equal(
      exportSentence({ actions, exportPermission: "allowed", nowMs: noon }),
      "Solar export only; the battery is not selling.",
    );
  });

  it("reports no worthwhile sale when export is allowed and nothing exports", () => {
    const actions = [slot(noon, 500, 200)];
    assert.equal(
      exportSentence({ actions, exportPermission: "allowed", nowMs: noon }),
      "Battery export is allowed, but FTW found no worthwhile sale.",
    );
  });

  it("reports a blocked sale when permission is off or unknown", () => {
    const actions = [slot(noon, 0, 100)];
    assert.equal(
      exportSentence({ actions, exportPermission: "not_allowed", nowMs: noon }),
      "Battery sale blocked: permission is off or not checked.",
    );
    assert.equal(
      exportSentence({ actions, exportPermission: "unknown", nowMs: noon }),
      "Battery sale blocked: permission is off or not checked.",
    );
  });

  it("does not treat house-only discharge as a battery sale", () => {
    assert.equal(isBatterySale({ battery_w: -2000, grid_w: 300 }), false);
    assert.equal(isBatterySale({ battery_w: -2000, grid_w: -400 }), true);
  });
});

describe("prefsKnown", () => {
  it("waits for the box to name its margin", () => {
    assert.equal(prefsKnown({}), false);
    assert.equal(prefsKnown(null), false);
    assert.equal(prefsKnown({ safety_k: 0 }), true);
    assert.equal(prefsKnown({ planner_mapped_k: 1 }), true);
    assert.equal(prefsKnown({ forecast_trust: "bold" }), true);
  });
});

describe("prefsFromStatus", () => {
  it("defaults to Balanced + unknown", () => {
    const p = prefsFromStatus({});
    assert.equal(p.forecast_trust, "balanced");
    assert.equal(p.battery_export, "unknown");
    assert.equal(p.safety_k, SAFETY_K_DEFAULT);
  });

  it("reads safety_k — the stored number owns the style (#1017, #1020)", () => {
    const p = prefsFromStatus({
      forecast_trust: "balanced",
      battery_export: "allowed",
      safety_k: 0.85,
      planner_mapped_k: 0.85,
    });
    assert.equal(p.forecast_trust, "balanced");
    assert.equal(p.battery_export, "allowed");
    assert.equal(p.safety_k, 0.85);
  });

  it("falls back to planner_mapped_k, then to the enum, on an older box", () => {
    assert.equal(prefsFromStatus({ forecast_trust: "bold", planner_mapped_k: 0 }).safety_k, 0);
    assert.equal(prefsFromStatus({ forecast_trust: "cautious" }).safety_k, 2);
    assert.equal(prefsFromStatus({ forecast_trust: "bold" }).safety_k, 0);
    assert.equal(prefsFromStatus({ forecast_trust: "balanced" }).safety_k, SAFETY_K_DEFAULT);
  });
});

describe("Plan card markup and wiring", () => {
  it("offers five planning styles in one row, none checked until the box answers", () => {
    assert.match(html, /Planning style/);
    const group = html.match(/<div class="plan-style-steps" id="plan-style-steps" role="radiogroup"[^>]*>([\s\S]*?)<\/div>/);
    assert.ok(group, "style radiogroup not found");
    const keys = [...group[1].matchAll(/data-style="([a-z_]+)">([^<]+)</g)].map((m) => [m[1], m[2]]);
    assert.deepEqual(keys, PLAN_STYLES.map((s) => [s.key, s.name]));
    assert.doesNotMatch(group[1], /aria-checked="true"/);
    assert.match(html, /<p id="plan-style-text" class="plan-style-text" aria-live="polite" hidden><\/p>/);
    assert.match(html, /Keeps more in the battery/);
    assert.match(html, /Counts more on the forecast/);
    assert.match(html, /Safety limits are the same in every style\./);
    assert.match(html, /id="plan-style-settings"/);
  });

  it("drops the k slider from the Plan card", () => {
    assert.doesNotMatch(html, /forecast-trust-slider|Follow the forecast|Hold reserve|Trust forecast/);
    assert.doesNotMatch(plan, /Shown intervals|used by plan/);
  });

  it("keeps the export permission as it was", () => {
    assert.match(html, /id="plan-export-check"/);
    assert.match(
      html,
      /Allow the battery to sell to the grid when the plan expects a worthwhile sale\./,
    );
    assert.match(
      html,
      /Solar can still export when this is off\. Check your electricity contract\./,
    );
    assert.match(html, /Not checked — battery export stays off\./);
    assert.match(html, /FTW used to sell from the battery on high-price hours\. Allow that to continue\?/);
  });

  it("speaks of styles, not risk, and keeps Passive/Active off the card", () => {
    assert.doesNotMatch(html, />Strategy</);
    assert.match(app, /String\(m\.key \|\| ""\)\.indexOf\("planner_"\) === 0\) return/);
    assert.doesNotMatch(html + plan + app, /\brisk\b/i);
    for (const s of PLAN_STYLES) assert.doesNotMatch(s.text, /\brisk\b/i);
  });

  it("moves the style keys from the focused style", () => {
    assert.match(plan, /const focused = e\.target\.closest\("\[data-style\]"\);/);
    assert.match(plan, /PLAN_STYLES\.findIndex\(function \(s\) \{ return s\.key === focused\.dataset\.style; \}\)/);
  });

  it("POSTs only the changed preference and marks the plan replanning", () => {
    assert.match(plan, /\/api\/planner\/prefs/);
    assert.match(plan, /postPlannerPrefs\(\{ safety_k: clampSafetyK\(k\) \}\)/);
    assert.match(plan, /postPlannerPrefs\(\{ battery_export: exportPerm \}\)/);
    assert.match(plan, /setReplanPending\(true\)/);
    assert.match(plan, /setReplanPending\(false\)/);
    assert.match(plan, /Replanning…/);
  });

  it("shades the margin instead of drawing dashed planning lines", () => {
    assert.match(plan, /Shaded: margin the plan holds back/);
    assert.doesNotMatch(plan, /Solid: forecast · dashed: used by plan/);
  });
});

describe("prefsQueue", () => {
  const settle = async () => { for (let i = 0; i < 10; i++) await new Promise((r) => setImmediate(r)); };

  it("keeps writes in the order they were made, even when the first answers late", async () => {
    const order = [];
    const answers = [];
    const send = (change) => new Promise((resolve) => answers.push(() => {
      order.push(change.safety_k);
      resolve({ safety_k: change.safety_k });
    }));
    const announced = [];
    const save = prefsQueue(send, (answer, source) => announced.push([answer.safety_k, source]));
    const first = save({ safety_k: 1 }, "card");
    const second = save({ safety_k: 0.45 }, "settings");
    await settle();
    assert.equal(answers.length, 1, "the second write left before the first was answered");
    answers.shift()();
    await first;
    await settle();
    answers.shift()();
    await second;
    assert.deepEqual(order, [1, 0.45]);
    assert.deepEqual(announced, [[1, "card"], [0.45, "settings"]]);
  });

  it("lets a newer change to the same preference replace one still waiting", async () => {
    const sent = [];
    const answers = [];
    const send = (change) => {
      sent.push(change);
      return new Promise((resolve) => answers.push(() => resolve(change)));
    };
    const save = prefsQueue(send, () => {});
    const veryCareful = save({ safety_k: 1 }, "card"); // on its way
    const bold = save({ safety_k: 0.15 }, "card"); // waits
    const fine = save({ safety_k: 0.8 }, "settings"); // the latest choice replaces Bold
    await settle();
    answers.shift()();
    await veryCareful;
    await settle();
    answers.shift()();
    assert.deepEqual(await bold, { safety_k: 0.8 }, "the replaced caller did not get the newer answer");
    assert.deepEqual(await fine, { safety_k: 0.8 });
    assert.deepEqual(sent, [{ safety_k: 1 }, { safety_k: 0.8 }]);
  });

  it("keeps the order of changes to different preferences", async () => {
    const sent = [];
    const answers = [];
    const save = prefsQueue((change) => {
      sent.push(change);
      return new Promise((resolve) => answers.push(() => resolve(change)));
    }, () => {});
    save({ safety_k: 1 }, "card");
    save({ battery_export: "allowed" }, "card");
    save({ safety_k: 0.15 }, "settings");
    for (let i = 0; i < 3; i++) {
      await settle();
      answers.shift()();
    }
    await settle();
    assert.deepEqual(sent, [{ safety_k: 1 }, { battery_export: "allowed" }, { safety_k: 0.15 }]);
  });

  it("goes on after a failed write and announces only confirmed ones", async () => {
    const announced = [];
    const save = prefsQueue((c) => (c.fail ? Promise.reject(new Error("HTTP 503")) : Promise.resolve(c)),
      (answer) => announced.push(answer));
    await assert.rejects(save({ fail: true }));
    assert.deepEqual(await save({ safety_k: 0.3 }), { safety_k: 0.3 });
    assert.deepEqual(announced, [{ safety_k: 0.3 }]);
  });
});
