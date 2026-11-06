// A canvas cannot read var(), so charts take their colours from here and
// redraw on "ftw-theme-change". Fallbacks are the dark theme's values.
export function createThemeColors(readProperty) {
  const resolve = (name, fallback) => readProperty(name).trim() || fallback;

  return {
    resolve,
    palette() {
      return {
        text: resolve("--fg", "#eef3ff"),
        dim: resolve("--fg-dim", "#9aa5b8"),
        muted: resolve("--fg-muted", "#727d90"),
        line: resolve("--line", "#1b2433"),
        panel: resolve("--ink-raised", "#0b111c"),
        accent: resolve("--accent-e", "#eef3ff"),
        // Colour is energy: one hue per kind, the battery two.
        solar: resolve("--amber", "#ffe14d"),
        stores: resolve("--cyan", "#18d9ff"),
        gives: resolve("--energy-gives", "#ffb000"),
        grid: resolve("--red-e", "#ff5fa8"),
        car: resolve("--violet", "#b69cff"),
        // States, never levels.
        live: resolve("--live", "#83f52c"),
        alarm: resolve("--alarm", "#ff453a"),
      };
    },
  };
}

if (typeof window !== "undefined" && typeof document !== "undefined") {
  const style = () => getComputedStyle(document.documentElement);
  window.ftwThemeColors = createThemeColors(
    (name) => style().getPropertyValue(name),
  );
}
