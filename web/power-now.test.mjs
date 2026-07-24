import assert from "node:assert/strict";
import { describe, it } from "node:test";

import {
  initPowerNow,
  normalizePowerNowMode,
  storedPowerNowMode,
} from "./power-now.js";

class FakeClassList {
  constructor() {
    this.values = new Set();
  }

  add(...names) {
    names.forEach((name) => this.values.add(name));
  }

  remove(...names) {
    names.forEach((name) => this.values.delete(name));
  }

  contains(name) {
    return this.values.has(name);
  }
}

class FakeElement {
  constructor(id, mode = null) {
    this.id = id;
    this.dataset = mode ? { powerNowMode: mode } : {};
    this.attributes = new Map();
    this.listeners = new Map();
    this.hidden = false;
    this.focused = false;
  }

  addEventListener(type, listener) {
    const listeners = this.listeners.get(type) || [];
    listeners.push(listener);
    this.listeners.set(type, listeners);
  }

  removeEventListener(type, listener) {
    const listeners = this.listeners.get(type) || [];
    this.listeners.set(type, listeners.filter((candidate) => candidate !== listener));
  }

  dispatch(type, detail = {}) {
    const event = {
      currentTarget: this,
      target: this,
      defaultPrevented: false,
      preventDefault() {
        this.defaultPrevented = true;
      },
      ...detail,
    };
    for (const listener of this.listeners.get(type) || []) listener(event);
    return event;
  }

  setAttribute(name, value) {
    this.attributes.set(name, String(value));
  }

  getAttribute(name) {
    return this.attributes.get(name) ?? null;
  }

  focus() {
    this.focused = true;
  }
}

function fixture() {
  const valuesTab = new FakeElement("power-now-tab-values", "values");
  const flowTab = new FakeElement("power-now-tab-flow", "flow");
  const valuesPanel = new FakeElement("power-now-values");
  const flowPanel = new FakeElement("power-now-flow");
  const elements = new Map([
    [valuesTab.id, valuesTab],
    [flowTab.id, flowTab],
    [valuesPanel.id, valuesPanel],
    [flowPanel.id, flowPanel],
  ]);
  return {
    root: {
      body: { classList: new FakeClassList() },
      getElementById(id) {
        return elements.get(id) || null;
      },
      querySelectorAll(selector) {
        return selector === "[data-power-now-mode]" ? [valuesTab, flowTab] : [];
      },
    },
    valuesTab,
    flowTab,
    valuesPanel,
    flowPanel,
  };
}

describe("Power now preference compatibility", () => {
  it("maps legacy and current stored values to the two visible modes", () => {
    for (const [stored, expected] of [
      [null, "values"],
      ["numbers", "values"],
      ["values", "values"],
      ["hero", "flow"],
      ["flow", "flow"],
      ["corrupt", "values"],
    ]) {
      assert.equal(normalizePowerNowMode(stored), expected);
    }
  });

  it("writes the legacy values so existing FTW versions remain compatible", () => {
    assert.equal(storedPowerNowMode("values"), "numbers");
    assert.equal(storedPowerNowMode("flow"), "hero");
  });
});

describe("Power now controller", () => {
  it("restores Flow, updates accessible state, and persists a Values click", () => {
    const ui = fixture();
    const writes = [];
    const storage = {
      getItem() {
        return "hero";
      },
      setItem(key, value) {
        writes.push([key, value]);
      },
    };

    const cleanup = initPowerNow(ui.root, storage);

    assert.equal(ui.flowTab.getAttribute("aria-selected"), "true");
    assert.equal(ui.valuesTab.getAttribute("aria-selected"), "false");
    assert.equal(ui.valuesPanel.hidden, true);
    assert.equal(ui.flowPanel.hidden, false);
    assert.equal(ui.root.body.classList.contains("mode-hero"), true);

    ui.valuesTab.dispatch("click");

    assert.equal(ui.valuesTab.getAttribute("aria-selected"), "true");
    assert.equal(ui.flowPanel.hidden, true);
    assert.equal(ui.root.body.classList.contains("mode-numbers"), true);
    assert.deepEqual(writes.at(-1), ["ftw-hero-mode", "numbers"]);
    cleanup();
  });

  it("defaults to Values and remains interactive when storage is unavailable", () => {
    const ui = fixture();
    const storage = {
      getItem() {
        throw new Error("blocked");
      },
      setItem() {
        throw new Error("blocked");
      },
    };

    initPowerNow(ui.root, storage);

    assert.equal(ui.valuesTab.getAttribute("aria-selected"), "true");
    assert.equal(ui.valuesPanel.hidden, false);
    assert.equal(ui.flowPanel.hidden, true);

    const event = ui.valuesTab.dispatch("keydown", { key: "ArrowRight" });

    assert.equal(event.defaultPrevented, true);
    assert.equal(ui.flowTab.focused, true);
    assert.equal(ui.flowTab.getAttribute("aria-selected"), "true");
    assert.equal(ui.flowPanel.hidden, false);
  });
});
