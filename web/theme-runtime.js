export function createThemeColors(readProperty) {
  const resolve = (name, fallback) => readProperty(name).trim() || fallback;

  return {
    resolve,
    palette() {
      return {
        text: resolve("--fg", "#e8e8e8"),
        dim: resolve("--fg-dim", "#a0a0a0"),
        muted: resolve("--fg-muted", "#858585"),
        line: resolve("--line", "#2a2a2a"),
        panel: resolve("--ink-raised", "#161616"),
        accent: resolve("--accent-e", "#f5b942"),
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
