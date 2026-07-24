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

describe("terminal-native light DOM chrome", () => {
  it("uses one accessible FTW brand mark", () => {
    assert.equal((html.match(/class="header-logo"/g) || []).length, 1);
    assert.match(html, /<img[^>]+alt="FTW"[^>]+class="header-logo"/);
    assert.doesNotMatch(html, /<h1>\s*FTW\s*<\/h1>/);
  });

  it("uses flat token surfaces for page, header, and mobile destinations", () => {
    assert.doesNotMatch(
      appCss,
      /body\.ftw-app::before\s*\{[\s\S]*?radial-gradient/,
    );
    assert.doesNotMatch(
      appCss,
      /body\.ftw-app > header::before\s*\{[\s\S]*?backdrop-filter/,
    );
    assert.match(
      appCss,
      /body\.ftw-app > header\s*\{[\s\S]*?background:\s*var\(--ink-elevated\)/,
    );
    assert.match(
      appCss,
      /\.mobile-destinations\s*\{[\s\S]*?background:\s*var\(--ink-elevated\)/,
    );
  });

  it("uses amber instead of blue for selected interface chrome", () => {
    assert.match(
      styleCss,
      /\.diag-row\.active\s*\{[^}]*var\(--accent-e\)/,
    );
    assert.match(
      styleCss,
      /\.modal-tabs button\.active\s*\{[^}]*var\(--accent-e\)/,
    );
    assert.doesNotMatch(
      styleCss,
      /\.diag-row\.active\s*\{[^}]*#60a5fa/,
    );
  });

  it("loads canonical tokens before shared setup styles", () => {
    assert.ok(
      setup.indexOf("/components/theme.css") < setup.indexOf("/style.css"),
      "setup must load theme.css before style.css",
    );
  });
});
