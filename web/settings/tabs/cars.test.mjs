// node --test web/settings/tabs/cars.test.mjs
//
// Settings → Cars keeps the owner's cars on this box with the car routes:
// the list with how the box knows each car, rename, battery size, add and
// remove. Nothing here goes through the Save button's config write.

import { readFileSync } from "node:fs";
import { describe, it } from "node:test";
import assert from "node:assert/strict";

globalThis.window = {};
await import("./cars.js");
const tab = globalThis.window.FTWSettings.tabs.cars;
const P = tab._pure;

const read = (path) => readFileSync(new URL(path, import.meta.url), "utf8");
const source = read("./cars.js");
const index = read("../../index.html");
const shell = read("../../settings.js");

describe("a new car", () => {
  it("gets an id as the owner's app mints one", () => {
    const id = P.newCarId((b) => b.fill(0xab));
    assert.equal(id, "ab".repeat(16));
    assert.match(P.newCarId(), /^[0-9a-f]{32}$/);
    assert.notEqual(P.newCarId(), P.newCarId());
  });

  it("sends its name, size in Wh and an optional VIN", () => {
    const body = P.addBody(" EV6 ", "77,4", "", (b) => b.fill(1));
    assert.deepEqual(body, { id: "01".repeat(16), name: "EV6", capacity_wh: 77400 });
    assert.equal(P.addBody("Model Y", "75", " 5yjygdee0mf000001 ").vin, "5YJYGDEE0MF000001");
  });

  it("reads a battery size the box takes, and refuses one it would not", () => {
    assert.equal(P.kWh("76"), 76000);
    assert.equal(P.kWh("77.4"), 77400);
    assert.equal(P.kWh("0.5"), null);
    assert.equal(P.kWh("301"), null);
    assert.equal(P.kWh("big"), null);
    assert.equal(P.kWhText(77400), "77.4");
    assert.equal(P.kWhText(0), "");
  });
});

describe("a VIN the box already has", () => {
  // Adding a car with a VIN a car the box made has is how the owner's app
  // takes that car over. From this page it would rename and resize the car
  // the owner already sees, so the page stops it and says which car.
  it("names the car that has it", () => {
    const cars = [{ id: "a".repeat(32), name: "Model Y", vin: "5YJYGDEE0MF000001", box_minted: true }];
    assert.equal(P.vinTakenBy(cars, " 5yjygdee0mf000001").name, "Model Y");
    assert.equal(P.vinTakenBy(cars, "YV1AB0CD9EF123456"), null);
    assert.equal(P.vinTakenBy(cars, ""), null);
    assert.match(source, /already has that VIN/);
  });
});

describe("a car's edit", () => {
  const car = { id: "a".repeat(32), name: "Car 1", capacity_wh: 60000 };
  it("sends only what changed", () => {
    assert.deepEqual(P.updateBody(car, "Model Y", "76"), { name: "Model Y", capacity_wh: 76000 });
    assert.deepEqual(P.updateBody(car, "Car 1", "77"), { capacity_wh: 77000 });
    assert.equal(P.updateBody(car, "Car 1", "60"), null);
    assert.equal(P.updateBody(car, "  ", ""), null, "an empty name or size is not a change");
  });

  it("goes to the car's own route and never to the config", () => {
    assert.match(source, /"PATCH", "\/api\/cars\/" \+ encodeURIComponent\(car\.id\)/);
    assert.match(source, /"DELETE", "\/api\/cars\/" \+ encodeURIComponent\(car\.id\)/);
    assert.match(source, /"POST", "\/api\/cars"/);
    assert.doesNotMatch(source, /\/api\/config/);
    assert.match(shell, /tab === "cars"/, "the Save button hides on Cars");
  });
});

describe("how the box knows a car", () => {
  it("lists the VIN, the level sources and the charger tags", () => {
    const rows = P.knownBy({ vin: "5YJYGDEE0MF000001", make: "Tesla", sources: ["tesla-ha"], identifiers: ["RFID-1", "MAC-2"] });
    assert.deepEqual(rows, [
      ["VIN", "5YJYGDEE0MF000001"],
      ["Make", "Tesla"],
      ["Level from", "tesla-ha"],
      ["Charger tags", "RFID-1, MAC-2"],
    ]);
    const bare = P.knownBy({});
    assert.equal(bare[0][1], "Not set");
    assert.match(bare[1][1], /No source here/);
    assert.equal(bare[2][1], "None");
  });

  it("says they are hidden when the box left them out for this device", () => {
    const rows = P.knownBy({ name: "Model Y" }, true);
    assert.deepEqual(rows.map((r) => r[0]), ["VIN", "Level from", "Charger tags"]);
    for (const [, v] of rows) assert.match(v, /^Hidden/);
    assert.match(source, /state\.redacted = !!\(data && data\.redacted\)/);
  });

  it("puts names and sources in as text", () => {
    assert.doesNotMatch(source.slice(source.indexOf("function carRow")), /innerHTML/);
  });
});

describe("a refusal in words", () => {
  it("names each reason the car ops give", () => {
    for (const reason of ["duplicate", "invalid", "cars", "car", "persistence_unconfirmed"]) {
      assert.equal(P.refusalText({ reason }), P.REFUSED[reason]);
    }
    assert.equal(P.refusalText({ error: "something else" }), "something else");
    assert.equal(P.refusalText(null), "The box did not take that. Try again.");
  });
});

describe("the tab", () => {
  it("sits beside Chargers and loads", () => {
    assert.match(index, /data-tab="loadpoints">Chargers<\/button>\s*<button data-tab="cars">Cars<\/button>/);
    assert.match(index, /<script src="\/settings\/tabs\/cars\.js\?v=\w+"><\/script>/);
    assert.match(tab.render(), /id="cars-list"/);
    assert.match(tab.render(), /id="cars-add"/);
  });
});
