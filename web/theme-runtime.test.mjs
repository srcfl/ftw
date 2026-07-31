import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { createThemeColors } from "./theme-runtime.js";

describe("theme canvas colors", () => {
  it("resolves canonical properties and falls back cleanly", () => {
    const values = new Map([["--fg", "rgb(232, 232, 232)"]]);
    const colors = createThemeColors((name) => values.get(name) || "");

    assert.equal(colors.resolve("--fg", "#fff"), "rgb(232, 232, 232)");
    assert.equal(colors.resolve("--missing", "#858585"), "#858585");
  });

  it("builds all neutral canvas chrome roles", () => {
    const colors = createThemeColors(() => "");

    assert.deepEqual(colors.palette(), {
      text: "#e8e8e8",
      dim: "#a0a0a0",
      muted: "#858585",
      line: "#2a2a2a",
      panel: "#161616",
      accent: "#f5b942",
    });
  });
});
