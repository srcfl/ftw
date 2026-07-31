import assert from "node:assert/strict";
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import { describe, it } from "node:test";

const webRoot = new URL(".", import.meta.url).pathname;
const theme = readFileSync(join(webRoot, "components/theme.css"), "utf8");

// Tokens declared in a given selector's block, as {name: value}.
function tokensIn(selector) {
  const start = theme.indexOf(selector + " {");
  assert.notEqual(start, -1, `${selector} block not found in theme.css`);
  const end = theme.indexOf("\n}", start);
  const block = theme.slice(start, end);
  const out = {};
  for (const m of block.matchAll(/^\s*(--[a-z0-9-]+):\s*([^;]+);/gm)) {
    out[m[1]] = m[2].trim();
  }
  return out;
}

// A value that paints something: hex, oklch, rgb, or a var() pointing at
// another token. Sizes and bare numbers are theme-independent.
function isColour(value) {
  return /^(#|oklch\(|rgb|hsl|color-mix\(|var\(--)/.test(value);
}

function sourceFiles(dir, acc = []) {
  for (const entry of readdirSync(dir)) {
    if (entry === "node_modules" || entry === "vendor") continue;
    const path = join(dir, entry);
    if (statSync(path).isDirectory()) sourceFiles(path, acc);
    else if (/\.(js|css|html|mjs)$/.test(entry)) acc.push(path);
  }
  return acc;
}

const allSource = sourceFiles(webRoot)
  .filter((p) => !p.endsWith("theme-tokens.test.mjs"))
  .map((p) => readFileSync(p, "utf8"))
  .join("\n");

describe("theme tokens", () => {
  const dark = tokensIn(":root");
  const light = tokensIn('html[data-theme="light"]');

  // Tokens whose value is correct in both themes by design. --on-accent is
  // the ink placed on the accent fill; the accent stays amber on both
  // grounds, so the dark ink is right either way.
  const THEME_INDEPENDENT = new Set(["--on-accent"]);

  // A colour token follows the theme in one of two ways: it points at
  // another token (an alias, which moves when that token is redefined), or
  // light mode declares its own value. A literal that does neither is stuck
  // at its dark value everywhere — which is how --surface, #1a1a2e, ended up
  // painting the app header on a light page.
  it("leaves no colour token stuck at its dark value in light mode", () => {
    const stuck = [];
    for (const [name, value] of Object.entries(dark)) {
      if (!isColour(value)) continue;
      if (THEME_INDEPENDENT.has(name)) continue;
      if (value.startsWith("var(--")) continue;   // alias — follows its target
      if (name in light) continue;                // light declares its own
      const uses = allSource.split(`var(${name})`).length - 1 +
                   (allSource.split(`var(${name},`).length - 1);
      if (uses > 0) stuck.push(`${name} (${uses} uses, dark value ${value})`);
    }
    assert.deepEqual(stuck, [], "colour tokens stuck at their dark value in light mode");
  });

  // The pre-oklch names (--bg, --surface, --border, --text, …) are still
  // read in ~250 places. They stay as one-line aliases onto the canonical
  // roles rather than a second palette carrying its own values, so retuning
  // --line moves --border with it and the two cannot drift.
  it("keeps the legacy base palette as aliases, not a second palette", () => {
    for (const [legacy, expected] of Object.entries({
      "--bg": "var(--ink)",
      "--surface": "var(--ink-raised)",
      "--surface2": "var(--ink-sunken)",
      "--border": "var(--line)",
      "--text": "var(--fg)",
    })) {
      assert.equal(dark[legacy], expected, `${legacy} should alias ${expected}`);
    }
  });

  // Aliasing on :root is what makes one declaration serve both themes; a
  // light-only mapping leaves dark mode on whatever the legacy values were.
  it("declares the aliases once, on :root", () => {
    for (const legacy of ["--bg", "--surface", "--border", "--text"]) {
      assert.ok(legacy in dark, `${legacy} must be declared on :root`);
      assert.ok(
        !(legacy in light),
        `${legacy} should not need a light-mode override once it is an alias`,
      );
    }
  });
});
