// node --test web/settings/tabs/loadpoints.test.mjs

import { describe, it } from "node:test";
import assert from "node:assert/strict";

globalThis.window = {};
await import("./loadpoints.js");
const { evDriverNames, ocppChargerIds, ocppStateLabel, ocppSection, ocppServerForm,
  steerLabel, vehiclesSection, vehicleForIdentifier, parseIdentifiers } =
  globalThis.window.FTWSettings.tabs.loadpoints._pure;

const escHtml = (s) =>
  String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");

describe("charger driver dropdown sources", () => {
  it("includes OCPP charge points alongside catalog EV drivers", () => {
    // No catalog primed → no Lua drivers; OCPP ids must still appear.
    const names = evDriverNames({ drivers: [{ name: "easee", lua: "drivers/easee_cloud.lua" }] }, {
      enabled: true,
      chargers: [{ id: "garage-left" }, { id: "garage-right" }],
    });
    assert.deepEqual(names, ["garage-left", "garage-right"]);
  });

  it("does not duplicate an id that is both a driver and a charge point", () => {
    const status = { chargers: [{ id: "garage" }, { id: "garage" }] };
    assert.deepEqual(ocppChargerIds(status), ["garage", "garage"]);
    assert.deepEqual(evDriverNames({ drivers: [] }, status), ["garage"]);
  });

  it("survives a missing or disabled OCPP status", () => {
    assert.deepEqual(evDriverNames({ drivers: [] }, null), []);
    assert.deepEqual(evDriverNames({ drivers: [] }, { enabled: false }), []);
  });
});

describe("charger control capability", () => {
  it("keeps unknown distinct from telemetry-only", () => {
    assert.equal(steerLabel({ steerable: true }), "smart charging");
    assert.equal(steerLabel({ steerable: false }), "telemetry only");
    assert.equal(steerLabel({}), "not reported");
  });

  it("warns only when a charger reported no smart charging", () => {
    const warn = /telemetry only<\/b> answered FTW/;
    const withMeterOnly = ocppSection({
      enabled: true, port: 8887, path: "/",
      chargers: [{ id: "dumb", online: true, steerable: false, feature_profiles: "Core" }],
    }, "ftw.lan", escHtml, []);
    assert.match(withMeterOnly, /telemetry only/);
    assert.match(withMeterOnly, warn);
    // The raw answer is kept as a tooltip for whoever needs the detail.
    assert.match(withMeterOnly, /title="Core"/);

    const allSteerable = ocppSection({
      enabled: true, port: 8887, path: "/",
      chargers: [{ id: "good", online: true, steerable: true, feature_profiles: "Core,SmartCharging" }],
    }, "ftw.lan", escHtml, []);
    assert.match(allSteerable, /smart charging/);
    assert.doesNotMatch(allSteerable, warn);

    const unprobed = ocppSection({
      enabled: true, port: 8887, path: "/",
      chargers: [{ id: "quiet", online: true }],
    }, "ftw.lan", escHtml, []);
    assert.match(unprobed, /not reported/);
    assert.doesNotMatch(unprobed, warn);
  });
});

describe("vehicle profiles", () => {
  const vehicles = [
    { id: "leaf", name: "Nissan Leaf", identifiers: ["04A2B3C4", "aa:bb:cc:dd:ee:ff"] },
    { id: "model3", identifiers: ["DEADBEEF"] },
  ];

  it("matches identifiers case-insensitively and trimmed", () => {
    assert.equal(vehicleForIdentifier(vehicles, "  aa:BB:cc:DD:ee:FF ").id, "leaf");
    assert.equal(vehicleForIdentifier(vehicles, "deadbeef").id, "model3");
    assert.equal(vehicleForIdentifier(vehicles, "unknown"), null);
    assert.equal(vehicleForIdentifier(vehicles, ""), null);
    assert.equal(vehicleForIdentifier(null, "x"), null);
  });

  it("parses comma-separated identifiers", () => {
    assert.deepEqual(parseIdentifiers(" 04A2B3C4 , aa:bb , ,"), ["04A2B3C4", "aa:bb"]);
    assert.deepEqual(parseIdentifiers(""), []);
  });

  it("shows a matched profile name in the charger table, raw id otherwise", () => {
    const status = {
      enabled: true, port: 8887, path: "/",
      chargers: [
        { id: "garage", online: true, vehicle_id: "04A2B3C4" },
        { id: "street", online: true, vehicle_id: "FEEDF00D" },
      ],
    };
    const html = ocppSection(status, "ftw.lan", escHtml, vehicles);
    assert.match(html, /Nissan Leaf/);
    assert.match(html, /FEEDF00D/);
    assert.match(html, /no profile/);
  });

  it("renders vehicle cards with policy toggles", () => {
    const html = vehiclesSection({
      vehicles: [{
        id: "leaf", name: "Nissan Leaf", capacity_wh: 40000,
        identifiers: ["04A2B3C4"], surplus_only: true, target_soc: 0.80,
      }],
    }, escHtml, () => "");
    assert.match(html, /Nissan Leaf/);
    assert.match(html, /data-checkbox-path="vehicles\.0\.surplus_only" checked/);
    assert.match(html, /vehicles\.0\.target_soc/);
    assert.match(html, /04A2B3C4/);
    assert.match(html, /new-vehicle-add/);
  });
});

describe("charger state labels", () => {
  it("distinguishes the session from the vehicle", () => {
    assert.equal(ocppStateLabel({ online: false }), "offline");
    assert.equal(ocppStateLabel({ online: true }), "online, no vehicle");
    assert.equal(ocppStateLabel({ online: true, connected: true }), "vehicle plugged");
    assert.equal(ocppStateLabel({ online: true, connected: true, charging: true }), "charging");
  });
});

describe("OCPP section", () => {
  it("lets the operator turn a disabled server on, rather than sending them to config.yaml", () => {
    const html = ocppSection({ enabled: false, chargers: [] }, "192.168.1.209", escHtml, [], {});
    assert.match(html, /Set up OCPP/);
    assert.doesNotMatch(html, /data-path="ocpp\.password"/);
    assert.doesNotMatch(html, /data-checkbox-path="ocpp\.enabled"/);
  });

  it("offers the server settings when it is already on", () => {
    const html = ocppSection(
      { enabled: true, port: 8887, path: "/", chargers: [] },
      "ftw.lan", escHtml, [],
      { ocpp: { enabled: true, port: 8887, port_v201: 8888, username: "ftw" } },
    );
    assert.match(html, /data-checkbox-path="ocpp\.enabled"[^>]*checked/);
    assert.match(html, /data-path="ocpp\.port_v201" value="8888"/);
    assert.match(html, /data-path="ocpp\.username" value="ftw"/);
  });

  it("shows the dial-in URL and the DHCP reservation advice when enabled", () => {
    const html = ocppSection({ enabled: true, port: 8887, path: "/", chargers: [] }, "ftw.lan", escHtml);
    assert.match(html, /ws:\/\/ftw\.lan:8887\//);
    assert.match(html, /DHCP reservation/);
    assert.match(html, /No OCPP charger has connected yet/);
  });

  it("marks an unadopted charger as pending and explains the quarantine", () => {
    const html = ocppSection({
      enabled: true, port: 8887, path: "/",
      chargers: [{ id: "intruder", online: true, power_w: 7200, pending: true }],
    }, "ftw.lan", escHtml);
    assert.match(html, /intruder/);
    assert.match(html, /· pending/);
    assert.match(html, /ignores their telemetry/);
    assert.match(html, /FTW saves and adds it to the site/);
  });

  it("shows no quarantine note when every charger is adopted", () => {
    const html = ocppSection({
      enabled: true, port: 8887, path: "/",
      chargers: [{ id: "bench-dawn", online: true }],
    }, "ftw.lan", escHtml);
    assert.doesNotMatch(html, /· pending/);
    assert.doesNotMatch(html, /ignores their telemetry/);
  });

  it("renders a connected charger with hardware, dialect and power", () => {
    const html = ocppSection({
      enabled: true, port: 8887, port_v201: 8888, path: "/",
      chargers: [{
        id: "bench-dawn", online: true, connected: true, charging: true,
        power_w: 7400, session_wh: 1500, version: "1.6",
        vendor: "Charge Amps", model: "Dawn",
      }],
    }, "ftw.lan", escHtml);
    assert.match(html, /bench-dawn/);
    assert.match(html, /Charge Amps Dawn/);
    assert.match(html, /charging/);
    assert.match(html, /7\.4 kW/);
    assert.match(html, /1\.50 kWh/);
    assert.match(html, /2\.0\.1: port 8888/);
  });
});

describe("OCPP server form", () => {
  it("creates the fields even when the config has never had an ocpp section", () => {
    const html = ocppServerForm({}, escHtml);
    assert.match(html, /data-checkbox-path="ocpp\.enabled"/);
    // Defaults an operator would otherwise have to go and look up.
    assert.match(html, /data-path="ocpp\.port" value="8887"/);
    assert.match(html, /data-path="ocpp\.path" value="\/"/);
    // The username default is a real value, not a placeholder: validation
    // requires it when the server is enabled, so a field that only hinted
    // "ftw" made every first enable fail with a 400.
    assert.match(html, /data-path="ocpp\.username" value="ftw"/);
  });

  it("never renders the stored password back into the page", () => {
    const html = ocppServerForm({ ocpp: { password: "the-real-secret" } }, escHtml);
    assert.doesNotMatch(html, /the-real-secret/);
    // Blank means "keep the stored one", so the placeholder has to say so.
    assert.match(html, /data-path="ocpp\.password" value=""/);
    assert.match(html, /placeholder="unchanged"/);
  });

  it("shows an empty bind as every interface rather than inventing 0.0.0.0", () => {
    const html = ocppServerForm({ ocpp: {} }, escHtml);
    assert.match(html, /data-path="ocpp\.bind" value=""/);
    assert.match(html, /placeholder="every interface"/);
  });

  it("escapes values it puts back into the page", () => {
    const html = ocppServerForm({ ocpp: { username: '"><script>x</script>' } }, escHtml);
    assert.doesNotMatch(html, /<script>/);
  });
});


describe("first charger connection", () => {
  const settings = window.FTWSettings;
  const render = config => settings.tabs.loadpoints.render({ config, escHtml, help: () => '' });
  it("offers a connection action instead of an empty Add form", () => {
    settings.ocppStatus = { enabled: false, chargers: [] };
    const html = render({ drivers: [], loadpoints: [] });
    assert.match(html, /id="connect-charger"/);
    assert.doesNotMatch(html, /id="new-lp-add"/);
    assert.doesNotMatch(html, /ctek_hybrid|EV-capable driver/);
    assert.match(html, /Save OCPP connection settings/);
    assert.match(html, /Save car profiles/);
  });
  it("does not offer an unsaved connection as a usable charger", () => {
    settings.catalogByLua = { 'drivers/easee.lua': { capabilities: ['ev'] } };
    const config = { drivers: [{ name: 'easee', lua: 'drivers/easee.lua' }], loadpoints: [] };
    settings.chargerSetupPending = 'easee';
    assert.doesNotMatch(render(config), /id="new-lp-add"/);
    settings.chargerSetupPending = null;
    assert.match(render(config), /id="new-lp-add"/);
    assert.match(render(config), /value="easee" selected/);
  });
});
