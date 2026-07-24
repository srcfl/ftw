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
    assert.equal((setup.match(/class="wizard-logo"/g) || []).length, 1);
    assert.match(setup, /<img[^>]+alt="FTW"[^>]+class="wizard-logo"/);
    assert.doesNotMatch(setup, /<h1>\s*FTW\s*<\/h1>/);
    assert.match(
      setup,
      /\.btn-primary\s*\{[^}]*color:\s*var\(--on-accent\)/,
    );
    assert.doesNotMatch(setup, /\.btn-primary\s*\{[^}]*color:\s*#0a0a0a/);
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

  it("contains no legacy blue-slate neutral chrome", () => {
    const sources = [
      appCss,
      styleCss,
      read("app.js"),
      html,
      read("settings/tabs/system.js"),
    ];
    const legacyNeutral = /#(?:14141f|1e1e2e|94a3b8|e2e8f0)\b/gi;
    for (const source of sources) {
      assert.deepEqual(source.match(legacyNeutral) || [], []);
    }
    assert.doesNotMatch(appCss, /oklch\([^)]*\b250\b[^)]*\)/i);
    assert.match(theme, /--white-s:\s*var\(--fg\)/);
  });

  it("loads canonical tokens before shared setup styles", () => {
    assert.ok(
      setup.indexOf("/components/theme.css") < setup.indexOf("/style.css"),
      "setup must load theme.css before style.css",
    );
  });
});

describe("component fallback palette", () => {
  const registeredComponents = [
    "components/ftw-modal.js",
    "components/ftw-progress-bar.js",
    "components/ftw-badge.js",
    "components/ftw-card.js",
    "components/ftw-tabs.js",
    "components/ftw-legend.js",
    "components/ftw-energy-flow.js",
    "components/ftw-battery-control.js",
    "components/ftw-pv-control.js",
    "components/ftw-price-chart.js",
    "components/ftw-energy-cake.js",
    "components/ftw-bar-chart.js",
    "components/ftw-history-card.js",
    "components/ftw-savings-card.js",
    "components/ftw-update-check.js",
    "components/ftw-notif-status.js",
    "components/ftw-notif-test-button.js",
    "components/ftw-notif-history.js",
    "components/ftw-pv-arrays-3d.js",
  ];
  const audited = [
    ...registeredComponents,
    "update-badge.js",
    "settings/tabs/devices.js",
    "settings/tabs/weather.js",
    "settings/tabs/system.js",
  ];

  it("contains no legacy blue-slate chrome fallback", () => {
    const disallowed =
      /#(?:0f172a|1e293b|334155|94a3b8|3b82f6|375a8f|6cf)\b/gi;
    for (const path of audited) {
      assert.deepEqual(read(path).match(disallowed) || [], [], path);
    }
  });

  it("uses the shared on-accent token", () => {
    assert.doesNotMatch(read("update-badge.js"), /color:\s*#0a0a0a/);
    for (const path of [
      "update-badge.js",
      "components/ftw-energy-flow.js",
      "components/ftw-savings-card.js",
      "components/ftw-notif-history.js",
      "components/ftw-notif-test-button.js",
    ]) {
      assert.match(read(path), /var\(--on-accent,\s*#0a0a0a\)/, path);
    }
  });

  it("resolves PV scene label chrome from the active theme", () => {
    const pvScene = read("components/ftw-pv-arrays-3d.js");
    assert.match(pvScene, /themeColor\("--on-accent",\s*"#0a0a0a"\)/);
    assert.match(pvScene, /themeColor\("--accent-e",\s*"#f5b942"\)/);
  });

  it("uses tokens for tooltip and recessed component surfaces", () => {
    const savings = read("components/ftw-savings-card.js");
    assert.match(savings, /\.spark-tip\s*\{[^}]*var\(--ink-elevated\)/s);
    assert.match(savings, /\.spark-tip\s*\{[^}]*var\(--line\)/s);
    assert.doesNotMatch(
      read("update-badge.js"),
      /rgba\(\s*(?:255,\s*255,\s*255|148,\s*163,\s*184)/,
    );
  });

  it("keeps every registered component in the explicit theme audit", () => {
    const registry = read("components/index.js");
    for (const path of registeredComponents.filter(
      (path) => !path.endsWith("ftw-pv-arrays-3d.js"),
    )) {
      const file = path.split("/").at(-1).replace(".", "\\.");
      assert.match(registry, new RegExp(file), path);
    }
    assert.match(
      read("settings/tabs/weather.js"),
      /ftw-pv-arrays-3d\.js/,
    );
  });
});

describe("canvas theme integration", () => {
  it("loads the resolver and redraws themed canvases", () => {
    assert.match(html, /src="\/theme-runtime\.js\?v=theme1"/);
    assert.match(
      html,
      /new CustomEvent\("ftw-theme-change"[\s\S]*?theme:\s*theme/,
    );
    for (const path of ["energy-history.js", "diagnose.js", "plan.js"]) {
      const source = read(path);
      assert.match(source, /ftwThemeColors/, path);
      assert.match(source, /ftw-theme-change/, path);
    }
  });
});
