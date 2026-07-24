import assert from "node:assert/strict";
import { describe, it } from "node:test";

import {
  initMobileDestinationScroll,
  shouldResetMobileScroll,
} from "./mobile-navigation.js";

class FakeEventTarget {
  constructor() {
    this.listeners = new Map();
  }

  addEventListener(type, listener) {
    const listeners = this.listeners.get(type) || [];
    listeners.push(listener);
    this.listeners.set(type, listeners);
  }

  removeEventListener(type, listener) {
    const listeners = this.listeners.get(type) || [];
    this.listeners.set(
      type,
      listeners.filter((candidate) => candidate !== listener),
    );
  }

  dispatch(type, event = {}) {
    for (const listener of this.listeners.get(type) || []) listener(event);
  }
}

function fixture(currentView = "overview") {
  const nav = new FakeEventTarget();
  nav.id = "mobile-destinations";
  nav.contains = (element) => element && element.owner === nav;

  const viewport = new FakeEventTarget();
  viewport.frames = [];
  viewport.scrolls = [];
  viewport.requestAnimationFrame = (callback) => {
    viewport.frames.push(callback);
  };
  viewport.scrollTo = (options) => {
    viewport.scrolls.push(options);
  };

  const root = {
    body: { dataset: { view: currentView } },
    getElementById(id) {
      return id === nav.id ? nav : null;
    },
  };

  const click = (nextView) => {
    const button = {
      owner: nav,
      dataset: { view: nextView },
    };
    nav.dispatch("click", {
      target: {
        closest() {
          return button;
        },
      },
    });
  };

  return { root, nav, viewport, click };
}

describe("mobile destination scroll reset", () => {
  it("recognizes only changed mobile destinations", () => {
    assert.equal(
      shouldResetMobileScroll("mobile-destinations", "overview", "energy"),
      true,
    );
    assert.equal(
      shouldResetMobileScroll("mobile-destinations", "energy", "energy"),
      false,
    );
    assert.equal(
      shouldResetMobileScroll("app-tabs", "overview", "energy"),
      false,
    );
    assert.equal(
      shouldResetMobileScroll("mobile-destinations", "overview", ""),
      false,
    );
  });

  it("scrolls after the router applies a user-selected destination", () => {
    const ui = fixture();
    const cleanup = initMobileDestinationScroll(ui.root, ui.viewport);

    ui.click("energy");
    assert.deepEqual(ui.viewport.scrolls, []);
    ui.root.body.dataset.view = "energy";
    ui.viewport.dispatch("hashchange");
    assert.equal(ui.viewport.frames.length, 1);
    ui.viewport.frames[0]();
    assert.deepEqual(ui.viewport.scrolls, [
      { top: 0, left: 0, behavior: "auto" },
    ]);
    cleanup();
  });

  it("preserves scroll for the active destination and browser history", () => {
    const ui = fixture("energy");
    initMobileDestinationScroll(ui.root, ui.viewport);

    ui.click("energy");
    ui.viewport.dispatch("hashchange");
    ui.root.body.dataset.view = "history";
    ui.viewport.dispatch("hashchange");

    assert.deepEqual(ui.viewport.frames, []);
    assert.deepEqual(ui.viewport.scrolls, []);
  });
});
