const STORAGE_KEY = "ftw-hero-mode";
const MODES = ["flow", "values"];

export function normalizePowerNowMode(storedValue) {
  return storedValue === "numbers" || storedValue === "values"
    ? "values"
    : "flow";
}

export function storedPowerNowMode(mode) {
  return mode === "flow" ? "hero" : "numbers";
}

export function initPowerNow(root = document, storage = null) {
  const tabs = [...root.querySelectorAll("[data-power-now-mode]")];
  const panels = {
    values: root.getElementById("power-now-values"),
    flow: root.getElementById("power-now-flow"),
  };
  const body = root.body;
  if (tabs.length !== MODES.length || !panels.values || !panels.flow || !body) {
    return () => {};
  }

  if (!storage) {
    try {
      storage = globalThis.localStorage;
    } catch (_) {
      storage = null;
    }
  }

  let stored = null;
  try {
    stored = storage && storage.getItem(STORAGE_KEY);
  } catch (_) {
    stored = null;
  }

  const apply = (requestedMode, { persist = true, focus = false } = {}) => {
    const mode = MODES.includes(requestedMode) ? requestedMode : "flow";
    tabs.forEach((tab) => {
      const selected = tab.dataset.powerNowMode === mode;
      tab.setAttribute("aria-selected", selected ? "true" : "false");
      tab.setAttribute("tabindex", selected ? "0" : "-1");
      if (selected && focus) tab.focus();
    });
    panels.values.hidden = mode !== "values";
    panels.flow.hidden = mode !== "flow";
    body.classList.remove("mode-hero", "mode-numbers");
    body.classList.add(mode === "flow" ? "mode-hero" : "mode-numbers");

    if (persist) {
      try {
        if (storage) storage.setItem(STORAGE_KEY, storedPowerNowMode(mode));
      } catch (_) {
        // Private browsing or disabled storage should not disable the control.
      }
    }
  };

  const listeners = [];
  tabs.forEach((tab, index) => {
    const onClick = () => apply(tab.dataset.powerNowMode);
    const onKeyDown = (event) => {
      if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
      event.preventDefault();
      const nextIndex = event.key === "Home"
        ? 0
        : event.key === "End"
          ? tabs.length - 1
          : (index + (event.key === "ArrowRight" ? 1 : -1) + tabs.length) % tabs.length;
      apply(tabs[nextIndex].dataset.powerNowMode, { focus: true });
    };
    tab.addEventListener("click", onClick);
    tab.addEventListener("keydown", onKeyDown);
    listeners.push([tab, "click", onClick], [tab, "keydown", onKeyDown]);
  });

  apply(normalizePowerNowMode(stored), { persist: false });

  return () => {
    listeners.forEach(([element, type, listener]) => {
      element.removeEventListener(type, listener);
    });
  };
}

function start() {
  initPowerNow(document);
}

if (typeof document !== "undefined") {
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", start, { once: true });
  } else {
    start();
  }
}
