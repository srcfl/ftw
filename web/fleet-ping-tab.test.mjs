import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { describe, it } from "node:test";
import vm from "node:vm";

// The fleet statistics panel makes one claim: what you see is what the box sends.
// These tests hold it to that. The rows must come from the payload rather
// than from a list in the tab, and the checkbox must reflect the box's own
// default rather than a friendlier one.
//
// fleet.js is a plain IIFE, so it loads under a small shim and these tests
// drive the real render() and the real row builder.

const source = readFileSync(new URL("./settings/tabs/fleet.js", import.meta.url), "utf8");

function loadTab() {
  const win = { FTWSettings: { tabs: {} } };
  const sandbox = {
    window: win,
    // render() defers its fetch; the tests below read the returned markup and
    // call the pure helpers, so the callback never needs to run.
    setTimeout: () => 0,
    document: { getElementById: () => null },
    fetch: () => new Promise(() => {}),
    JSON,
    Array,
    Object,
    String,
    console,
  };
  sandbox.globalThis = sandbox;
  vm.createContext(sandbox);
  vm.runInContext(source, sandbox);
  const panel = win.FTWSettings.fleetPing;
  assert.ok(panel && typeof panel.render === "function", "fleet.js registered no panel");
  assert.equal(win.FTWSettings.tabs.fleet, undefined, "fleet.js registered a standalone tab");
  return panel;
}

const tab = loadTab();
const { payloadRows, whereLine } = tab._pure;

function render(config) {
  return tab.render({ config });
}

const payload = {
  schema: "ftw.fleet/2",
  ftw_version: "v1.4.0-beta.2",
  channel: "beta",
  drivers: ["easee_cloud", "sungrow"],
  battery_kwh: "5-15",
  price_zone: "SE3",
  install_age: "6-12m",
  plan_solve: "1-5s",
  plan_fallbacks: "0",
  driver_errors: "0-1%",
  update: "failed",
};

describe("the fleet statistics panel", () => {
  it("offers one statistics switch, not a config setting", () => {
    // Statistics are one answer for both reports, kept by the box, not a
    // field in the config document that Save writes.
    const config = {};
    const html = render(config);
    assert.match(html, /id="statistics-share"/);
    assert.doesNotMatch(html, /data-checkbox-path/);
    assert.equal(config.fleet_ping, undefined, "render() wrote a config section");
  });

  it("says what goes beside each message through the relay", () => {
    // The sealed request carries a random report id and the send day. A
    // screen that says "no id, no timestamp" and leaves those out would be
    // the claim this tab exists not to make.
    const html = render({});
    assert.match(html, /random number, drawn new for each report so FTW Cloud counts it once/);
    assert.match(html, /UTC day it was sent/);
    assert.match(html, /with what goes beside it/);
  });

  it("names both reports and says they never travel together", () => {
    const html = render({});
    assert.match(html, /two anonymous reports a day/);
    assert.match(html, /never travel together/);
    assert.match(html, /part of Cloud/);
  });

  it("names every field the message carries", () => {
    // This screen is the whole argument for a feature that is on by default,
    // and the sentence people actually read is the paragraph, not the table
    // under it. A paragraph that names some of the fields is the screen
    // making its case with the awkward ones left out — the price zone above
    // all, since it is the only field that says roughly where the house is.
    const html = render({});
    for (const field of [/version/i, /channel/i, /device|driver/i, /battery/i, /price zone/i, /how old/i,
      /slowest plan/i, /fell back/i, /failed to read/i, /update/i]) {
      assert.match(html, field, `the paragraph does not name ${field}`);
    }
  });

  it("counts the fields it names, and the count is the fixture's", () => {
    // "ten things" is a claim like any other on this screen, and the only one
    // that can go stale in silence.
    //
    // What this holds is the sentence against the fixture above, which is a
    // copy of the payload written here by hand. It is not a guard on the box:
    // an eleventh field would leave this passing. That guard is
    // TestPayloadCarriesOnlyTheAgreedFields in go/internal/fleetping, which
    // counts the marshalled fields and fails on the eleventh — and whoever
    // answers it has to come back through here.
    const words = ["no", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten", "eleven"];
    const fields = Object.keys(payload).length - 1; // schema names the format, not the box
    assert.ok(words[fields], `no number word for ${fields} fields`);
    assert.match(render({}), new RegExp(words[fields] + " things"));
  });

  it("names what the message leaves out, counter and timestamp included", () => {
    const html = render({});
    for (const absent of [/no id/i, /no key/i, /no serial/i, /no counter/i, /no timestamp/i]) {
      assert.match(html, absent, `the paragraph does not say ${absent}`);
    }
  });

  it("concedes the limits instead of claiming the box cannot be recognised", () => {
    // go/internal/fleetping says it plainly: the payload is still a
    // quasi-identifier, and the source IP is the honest limit. A screen that
    // promises more than the package behind it claims is the one thing this
    // tab must never do.
    const html = render({});
    assert.match(html, /the only one of its kind/i);
    assert.match(html, /address during the request/i);
    assert.doesNotMatch(html, /nothing that lets two days be tied to the same box/i);
    // And the first paragraph must not settle the question the second one
    // exists to concede. go/internal/fleetping says two pings from an unusual
    // box could be guessed to be the same box; a reader who stops after the
    // first paragraph would already have been told they could not.
    assert.doesNotMatch(html, /came from the same box/i);
    // And set apart, because .hint carries no margin: two of them in a row
    // render as one slab, which is how a paragraph nobody reads happens.
    assert.match(html, /<p class="hint fleet-ping-limits">Two limits/);
  });

  it("says the relay keeps totals rather than reports or unique-box counts", () => {
    const line = whereLine({ enabled: true, endpoint: "https://relay.ftw.energy/fleet", ftw_relay: true, payload });
    assert.match(line, /adds the report to that day.s totals/i);
    assert.match(line, /then drops it/i);
    assert.match(line, /does not save the source address/i);
    assert.match(line, /reports, not unique boxes/i);
  });

  it("does not promise FTW relay handling for a custom collector", () => {
    const line = whereLine({ enabled: true, endpoint: "https://collector.example.test/fleet", ftw_relay: false, payload });
    assert.match(line, /custom collector/i);
    assert.match(line, /cannot say what that service stores/i);
    assert.doesNotMatch(line, /drops it|does not save the source address/i);
  });

  it("leaves a slot for the payload rather than writing values into the markup", () => {
    // If the tab printed the fields itself, the screen would be a second
    // rendering that could disagree with what is sent.
    const html = render({});
    assert.match(html, /id="fleet-ping-payload"/);
    assert.doesNotMatch(html, /sungrow|SE3|v1\.\d/);
  });
});

describe("the line under the payload", () => {
  const endpoint = "https://relay.ftw.energy/fleet";

  it("says where the message goes while the ping is on", () => {
    const line = whereLine({ enabled: true, endpoint, ftw_relay: true, payload });
    assert.match(line, /Sent to https:\/\/relay\.ftw\.energy/);
    assert.match(line, /once a day/);
  });

  it("does not promise the box cannot be known by when it calls", () => {
    // The send time is a random walk in six-hour steps, so it takes about four
    // days to work its way round the clock. Until then one box's arrival times
    // are still loosely related, and a line saying otherwise is a promise the
    // schedule does not keep.
    const line = whereLine({ enabled: true, endpoint, ftw_relay: true, payload });
    assert.doesNotMatch(line, /cannot be recognised/i);
    assert.match(line, /a few days/i);
  });

  it("does not claim anything is sent while the ping is off", () => {
    // This is the one screen whose entire justification is that it makes no
    // claim it cannot keep. "Sent to https://… once a day" under a switched
    // off ping is a false one.
    const line = whereLine({ enabled: false, endpoint, ftw_relay: true, payload });
    assert.doesNotMatch(line, /^Sent to/);
    assert.match(line, /Nothing is sent/);
    // The address still shows: somebody deciding whether to switch it on
    // needs to see where it would go.
    assert.match(line, /relay\.ftw\.energy/);
  });

  it("reads a missing body as off rather than as sending", () => {
    assert.match(whereLine(undefined), /Nothing is sent/);
    assert.match(whereLine({}), /Nothing is sent/);
  });
});

describe("the statistics switch", () => {
  const { shareLine } = tab._pure;

  it("is part of FTW Cloud while Cloud is on", () => {
    const line = shareLine({ statistics: true, statisticsRequired: true });
    assert.match(line, /Part of FTW Cloud/);
    assert.match(line, /no separate off switch/);
  });

  it("says nothing goes under earlier Cloud terms", () => {
    const line = shareLine({ statistics: false, statisticsRequired: false, statisticsNeedTerms: true });
    assert.match(line, /earlier terms/);
    assert.match(line, /Nothing is sent until you accept the current terms/);
  });

  it("says what on and off mean without Cloud", () => {
    assert.match(shareLine({ statistics: true, statisticsRequired: false }), /takes effect at once, with no restart/);
    assert.match(shareLine({ statistics: false, statisticsRequired: false }), /^Off\. Nothing is sent/);
    assert.match(shareLine(null), /Could not read/);
  });

  it("does not claim a report goes while it waits for the relay", () => {
    const line = whereLine({ enabled: false, waits_for_relay: true, endpoint: "https://cloud.ftw.energy/v1/fleet", ftw_relay: true, payload });
    assert.match(line, /^Not sent yet/);
    assert.match(line, /only through an independent relay/);
    assert.doesNotMatch(line, /^Sent/);
  });

  it("says who sees what when the report goes through the relay", () => {
    const line = whereLine({ enabled: true, endpoint: "https://relay.example.net/ftw", ftw_relay: true, through_relay: true, payload });
    assert.match(line, /^Sent through https:\/\/relay\.example\.net\/ftw, an independent relay, to FTW Cloud/);
    assert.match(line, /sees this box's address but not the report/);
    assert.match(line, /reads the report but not the address/);
  });
});

describe("keeping the line true after a save", () => {
  it("registers a post-save hook, because render() runs only on a tab switch", () => {
    // Without this the line keeps whatever the box said when the tab opened:
    // switch the ping off, save, and the screen still reads "Sent to …".
    assert.equal(typeof tab.afterSave, "function");
  });

  it("asks the box again when the shell calls it", () => {
    // The hook has to re-read /api/fleet-ping — the new answer is the box's,
    // and the checkbox in the form is not it.
    const asked = [];
    const sandbox = {
      window: { FTWSettings: { tabs: {} } },
      setTimeout: () => 0,
      document: { getElementById: () => ({ textContent: "", appendChild() {} }), createElement: () => ({ classList: {}, appendChild() {} }) },
      fetch: (path) => { asked.push(path); return new Promise(() => {}); },
      JSON, Array, Object, String, console,
    };
    sandbox.globalThis = sandbox;
    vm.createContext(sandbox);
    vm.runInContext(source, sandbox);
    sandbox.window.FTWSettings.fleetPing.afterSave();
    // And the statistics answer, which the switch shows.
    assert.deepEqual([...asked].sort(), ["/api/cloud", "/api/cloud/stats-policy", "/api/fleet-ping"]);
  });
});

describe("payloadRows", () => {
  it("shows every field the box sends, in plain words", () => {
    const rows = payloadRows(payload);
    const byKey = Object.fromEntries(rows.map((r) => [r.key, r]));

    assert.equal(byKey.ftw_version.value, "v1.4.0-beta.2");
    assert.equal(byKey.channel.value, "beta");
    assert.equal(byKey.drivers.value, "easee_cloud, sungrow");
    assert.equal(byKey.battery_kwh.value, "5 to 15 kWh");
    assert.equal(byKey.price_zone.value, "SE3");
    assert.equal(byKey.install_age.value, "six to twelve months");
    assert.equal(byKey.plan_solve.value, "one to five seconds");
    assert.equal(byKey.plan_fallbacks.value, "none");
    assert.equal(byKey.driver_errors.value, "under 1 in 100");
    assert.equal(byKey.update.value, "failed");

    assert.equal(byKey.battery_kwh.label, "Battery size");
    assert.equal(byKey.plan_solve.label, "Slowest plan, last day");
  });

  it("drops only the schema name", () => {
    const keys = payloadRows(payload).map((r) => r.key);
    assert.ok(!keys.includes("schema"), "schema is the message format, not the household");
    assert.equal(keys.length, Object.keys(payload).length - 1);
  });

  it("cannot hide a field the box started sending", () => {
    // The whole point of this screen. If a future version of the box added
    // something it should not, a tab driven by a fixed list would look
    // unchanged; this one shows the field under its own name.
    const rows = payloadRows({ ...payload, gateway_id: "8f2a1c" });
    const row = rows.find((r) => r.key === "gateway_id");
    assert.ok(row, "an unexpected field was silently dropped");
    assert.equal(row.label, "gateway_id");
    assert.equal(row.value, "8f2a1c");
  });

  it("reads an empty driver list as none rather than blank", () => {
    const rows = payloadRows({ ...payload, drivers: [] });
    assert.equal(rows.find((r) => r.key === "drivers").value, "none");
  });

  it("survives having nothing to show", () => {
    // Lengths, not deepEqual: the arrays are built inside the vm sandbox, so
    // their prototype comes from another realm.
    assert.equal(payloadRows(null).length, 0);
    assert.equal(payloadRows(undefined).length, 0);
    assert.equal(payloadRows("not an object").length, 0);
  });
});

describe("the daily energy report", () => {
  const { energyRows, energyLine } = tab._pure;
  const energy = {
    schema: "ftw.energy/1", date: "2026-10-23", pv_generation_kwh: 12, grid_import_kwh: 30, grid_export_kwh: 4,
    battery_charge_kwh: 9, battery_discharge_kwh: 8, vehicle_charge_kwh: 0,
  };

  it("shows every field the report carries, in kWh", () => {
    const rows = energyRows(energy);
    const byKey = Object.fromEntries(rows.map((r) => [r.key, r]));
    assert.equal(rows.length, Object.keys(energy).length - 1);
    assert.equal(byKey.pv_generation_kwh.value, "12 kWh");
    assert.equal(byKey.vehicle_charge_kwh.label, "Vehicle charge");
    assert.equal(byKey.date.value, "2026-10-23");
    const extra = energyRows({ ...energy, home: "x" }).find((r) => r.key === "home");
    assert.ok(extra, "an unexpected field was silently dropped");
  });

  it("says nothing is sent from a release without a relay", () => {
    const line = energyLine({ relay: "", sending: false, day: "2026-10-23", payload: energy });
    assert.match(line, /no relay yet/i);
    assert.doesNotMatch(line, /^Sent/);
  });

  it("does not point at a report it is not showing", () => {
    assert.doesNotMatch(energyLine({ relay: "", day: "2026-10-23", payload: null }), /this is the report/i);
    assert.doesNotMatch(energyLine({ relay: "https://relay.example.net/ftw", day: "2026-10-23", payload: null }), /this is the report/i);
  });

  it("says who sees what while it is sent, and nothing is sent while it is off", () => {
    const relay = "https://relay.example.net/ftw";
    const on = energyLine({ relay, sending: true, payload: energy });
    assert.match(on, /^Sent through https:\/\/relay\.example\.net/);
    assert.match(on, /sees this box's address but not the report/);
    assert.match(on, /reads the report but not the address/);
    assert.match(energyLine({ relay, sending: false, payload: energy }), /^Nothing is sent/);
  });

  it("names the fields, the missing id and the relay in its paragraph", () => {
    const html = render({});
    assert.match(html, /id="fleet-energy-payload"/);
    for (const want of [/solar generation/i, /grid import and export/i, /battery charge/i, /vehicle charge/i, /whole kWh/i, /no box id/i, /independent relay/i, /skipped/i]) {
      assert.match(html, want, `the paragraph does not say ${want}`);
    }
  });
});
