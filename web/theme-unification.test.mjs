import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { describe, it } from "node:test";
import { fileURLToPath } from "node:url";

const webRoot = dirname(fileURLToPath(import.meta.url));
const read = (path) => readFileSync(join(webRoot, path), "utf8");
const theme = read("components/theme.css");
const appCss = read("app.css");
const styleCss = read("style.css");
const html = read("index.html");
const setup = read("setup.html");

describe("terminal-native palette", () => {
  it("defines the approved neutral dark and light roles", () => {
    for (const value of [
      "#0d0d0d",
      "#101010",
      "#161616",
      "#1e1e1e",
      "#2a2a2a",
      "#e8e8e8",
      "#a0a0a0",
      "#858585",
      "#f4f4f2",
      "#ecece8",
      "#fafaf8",
      "#ffffff",
      "#cecec7",
      "#191919",
      "#4f4f4b",
      "#686862",
    ]) {
      assert.match(theme, new RegExp(value, "i"), value);
    }
    assert.match(theme, /--on-accent:\s*#0a0a0a/i);
  });

  it("bridges every legacy role to the canonical palette", () => {
    const aliases = {
      bg: "ink",
      surface: "ink-raised",
      surface2: "ink-sunken",
      border: "line",
      text: "fg",
      "text-dim": "fg-dim",
      green: "green-e",
      red: "red-e",
      yellow: "amber",
      blue: "cyan",
      accent: "accent-e",
    };
    for (const [legacy, canonical] of Object.entries(aliases)) {
      assert.match(
        theme,
        new RegExp(`--${legacy}:\\s*var\\(--${canonical}\\)`),
      );
    }
  });
});
