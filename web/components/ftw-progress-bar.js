// <ftw-progress-bar> — horizontal fill bar.
//
// Attributes:
//   value     — current value (default 0)
//   max       — scale (default 100)
//   mode      — "solid" (default) | "gradient"
//   status    — "ok" | "warn" | "bad" | "neutral" (default "ok")
//               picks the solid color when mode=solid
//   direction — "asc" (default, red→amber→green as value rises — SoC)
//             | "desc" (green→amber→red as value rises — fuse/peak)
//               only affects gradient mode
//
// Gradient mode paints a linear-gradient across the FILL element (same
// technique as the legacy .soc-fill). Using theme tokens — retheme by
// editing /components/theme.css.
//
// Example:
//   <ftw-progress-bar value="77" max="100" mode="gradient"></ftw-progress-bar>
//   <ftw-progress-bar value="9.5" max="16" mode="gradient" direction="desc"></ftw-progress-bar>
//   <ftw-progress-bar value="18" max="25" status="warn"></ftw-progress-bar>

import { FtwElement } from "./ftw-element.js";

class FtwProgressBar extends FtwElement {
  static styles = `
    :host {
      display: block;
      height: var(--ftw-progress-height, 8px);
      background: var(--ink-sunken);
      border-radius: 999px;
      overflow: hidden;
    }
    .fill {
      height: 100%;
      width: 0%;
      transition: width 0.3s ease, background 0.25s ease;
      border-radius: inherit;
    }
    .solid-ok       { background: var(--green-e); }
    .solid-warn     { background: var(--amber); }
    .solid-bad      { background: var(--red-e); }
    .solid-neutral  { background: var(--fg-muted); }

    /* Gradient stops: reveal red→amber→green as the fill grows (ASC,
     * low=bad, high=good — SoC) or green→amber→red (DESC, low=good,
     * high=bad — fuse/peak load). Uses the application functional tokens
     * so the ramp flips correctly in the light theme. */
    .grad-asc {
      background: linear-gradient(90deg,
        var(--red-e) 0%,
        var(--amber) 30%,
        var(--green-e) 60%);
    }
    .grad-desc {
      background: linear-gradient(90deg,
        var(--green-e) 0%,
        var(--amber) 60%,
        var(--red-e) 85%);
    }
  `;

  static get observedAttributes() {
    return ["value", "max", "mode", "status", "direction"];
  }

  attributeChangedCallback() {
    this.update();
  }

  render() {
    const value = Number(this.getAttribute("value") || 0);
    const max = Number(this.getAttribute("max") || 100) || 100;
    const mode = this.getAttribute("mode") || "solid";
    const status = this.getAttribute("status") || "ok";
    const direction = this.getAttribute("direction") || "asc";
    const pct = Math.max(0, Math.min(100, (value / max) * 100));

    let cls = "fill";
    if (mode === "gradient") {
      cls += direction === "desc" ? " grad-desc" : " grad-asc";
    } else {
      cls += ` solid-${status}`;
    }

    return `<div class="${cls}" style="width: ${pct.toFixed(1)}%"></div>`;
  }
}

customElements.define("ftw-progress-bar", FtwProgressBar);
