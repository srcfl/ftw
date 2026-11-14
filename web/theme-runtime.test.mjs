import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { describe, it } from "node:test";

import { createThemeColors } from "./theme-runtime.js";

const theme = readFileSync(new URL("./components/theme.css", import.meta.url), "utf8");

describe("theme canvas colors", () => {
  it("resolves canonical properties and falls back cleanly", () => {
    const values = new Map([["--fg", "rgb(232, 232, 232)"]]);
    const colors = createThemeColors((name) => values.get(name) || "");

    assert.equal(colors.resolve("--fg", "#fff"), "rgb(232, 232, 232)");
    assert.equal(colors.resolve("--missing", "#858585"), "#858585");
  });

  it("falls back to the dark theme's chrome, energy and state roles", () => {
    const colors = createThemeColors(() => "");

    assert.deepEqual(colors.palette(), {
      text: "#eef3ff",
      dim: "#9aa5b8",
      muted: "#727d90",
      line: "#1b2433",
      panel: "#0b111c",
      accent: "#eef3ff",
      solar: "#ffe14d",
      stores: "#18d9ff",
      gives: "#ffb000",
      grid: "#ff5fa8",
      car: "#b69cff",
      live: "#83f52c",
      alarm: "#ff453a",
    });
  });

  it("reads each role from its theme property, in both themes", () => {
    // Resolve theme.css by hand: the first declaration is dark, the second
    // light; var() aliases follow the same theme.
    const decls = (name) => [...theme.matchAll(new RegExp(`--${name}:\\s*([^;]+);`, "g"))].map((m) => m[1].trim());
    for (const look of [0, 1]) {
      const read = (name) => {
        const all = decls(name.slice(2));
        let value = all[look] ?? all[0] ?? "";
        const alias = /^var\((--[a-z0-9-]+)\)$/.exec(value);
        if (alias) value = read(alias[1]);
        return value;
      };
      const palette = createThemeColors(read).palette();
      const expected = look === 0
        ? { text: "#eef3ff", solar: "#ffe14d", stores: "#18d9ff", gives: "#ffb000", grid: "#ff5fa8", car: "#b69cff", live: "#83f52c", alarm: "#ff453a" }
        : { text: "#0a0f1a", solar: "#8a6c00", stores: "#00739a", gives: "#a35700", grid: "#c2186f", car: "#6a48d8", live: "#2f7a00", alarm: "#d70015" };
      for (const [role, value] of Object.entries(expected)) {
        assert.equal(palette[role], value, `${role} in ${look ? "light" : "dark"}`);
      }
    }
  });
});
