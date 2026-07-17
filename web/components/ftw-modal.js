// <ftw-modal> — generic modal with backdrop, header, body, footer.
//
// Contract:
//   <ftw-modal open>
//     <span slot="title">Settings</span>
//     <!-- default slot: body content -->
//     <form>...</form>
//     <div slot="footer">
//       <button>Save</button>
//     </div>
//   </ftw-modal>
//
// API:
//   el.open()        -> set attribute and render
//   el.close()       -> unset attribute + fire ftw-modal-close
//   attribute `open` reflects state; settable as well
//   ESC key and backdrop click call close()
//   Close (×) button in the header calls close()
//
// Events:
//   ftw-modal-close  — bubbles; cancelable. preventDefault() keeps it open.
//
// Styling:
//   :host([open])    — shown; adjust --ftw-modal-max-width on the host
//                      to override the default 640px.

import { FtwElement } from "./ftw-element.js";

class FtwModal extends FtwElement {
  static styles = `
    :host {
      display: none;
    }
    :host([open]) {
      display: block;
    }
    .backdrop {
      position: fixed;
      inset: 0;
      background: rgba(0, 0, 0, 0.55);
      z-index: 1000;
    }
    .dialog {
      position: fixed;
      top: 50%;
      left: 50%;
      transform: translate(-50%, -50%);
      z-index: 1001;
      width: min(92vw, var(--ftw-modal-max-width, 640px));
      max-height: 90vh;
      display: flex;
      flex-direction: column;
      background: var(--ink-raised);
      color: var(--fg);
      border: 1px solid var(--line);
      border-radius: var(--radius-sm);
      /* No shadow — the shared design system forbids drop-shadows on cards/modals.
         The 0.55 backdrop carries the elevation contrast instead. */
    }
    header {
      display: flex;
      align-items: center;
      justify-content: space-between;
      padding: 0.9rem 1rem;
      border-bottom: 1px solid var(--line);
    }
    header ::slotted([slot="title"]) {
      font-size: 1rem;
      font-weight: 600;
    }
    .close {
      appearance: none;
      background: transparent;
      border: none;
      color: var(--fg-dim);
      cursor: pointer;
      font-size: 1.25rem;
      line-height: 1;
      padding: 0 0.25rem;
    }
    .close:hover {
      color: var(--fg);
    }
    .body {
      padding: 1rem;
      overflow: auto;
      flex: 1 1 auto;
    }
    footer {
      padding: 0.75rem 1rem;
      border-top: 1px solid var(--line);
      display: flex;
      gap: 0.5rem;
      justify-content: flex-end;
      flex-wrap: wrap;
    }
    /* Hide the footer entirely when nothing is slotted, so consumers that
       don't need actions don't get an empty border-topped strip. */
    .footer-wrap:not([data-has-footer]) {
      display: none;
    }
  `;

  static get observedAttributes() {
    return ["open"];
  }

  connectedCallback() {
    super.connectedCallback();
    // Listen for ESC globally; we unbind in disconnectedCallback. Live on
    // document so shadow-DOM focus doesn't swallow the key.
    this._onKeyDown = (e) => {
      if (e.key === "Escape" && this.hasAttribute("open")) {
        this.close();
      }
    };
    document.addEventListener("keydown", this._onKeyDown);
  }

  disconnectedCallback() {
    document.removeEventListener("keydown", this._onKeyDown);
  }

  attributeChangedCallback(name) {
    if (name === "open") this.update();
  }

  open() {
    if (!this.hasAttribute("open")) this.setAttribute("open", "");
  }

  close() {
    const ev = new CustomEvent("ftw-modal-close", { bubbles: true, cancelable: true });
    this.dispatchEvent(ev);
    if (ev.defaultPrevented) return;
    this.removeAttribute("open");
  }

  render() {
    return `
      <div class="backdrop" part="backdrop"></div>
      <div class="dialog" role="dialog" aria-modal="true" part="dialog">
        <header part="header">
          <slot name="title"></slot>
          <button class="close" aria-label="Close" part="close">×</button>
        </header>
        <div class="body" part="body">
          <slot></slot>
        </div>
        <div class="footer-wrap">
          <footer part="footer">
            <slot name="footer"></slot>
          </footer>
        </div>
      </div>
    `;
  }

  afterRender() {
    this.shadowRoot.querySelector(".backdrop").addEventListener("click", () => this.close());
    this.shadowRoot.querySelector(".close").addEventListener("click", () => this.close());
    // Hide the footer border when the slot has no assigned nodes — avoids
    // an empty bar under body-only modals.
    const footerSlot = this.shadowRoot.querySelector('slot[name="footer"]');
    const wrap = this.shadowRoot.querySelector(".footer-wrap");
    const syncFooter = () => {
      if (footerSlot.assignedNodes({ flatten: true }).length > 0) {
        wrap.setAttribute("data-has-footer", "");
      } else {
        wrap.removeAttribute("data-has-footer");
      }
    };
    footerSlot.addEventListener("slotchange", syncFooter);
    syncFooter();
  }
}

customElements.define("ftw-modal", FtwModal);
