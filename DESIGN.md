# FTW — UI design system

One source of truth for tokens, typography, and the component vocabulary
shared across the landing page (ftw.sourceful.energy), the `/next` dashboard,
and the `/setup` wizard. Tokens live in `web/components/theme.css` and
inherit into every shadow DOM via `:root`.

## Principles

- **Flat, technical, editorial.** No drop shadows, no gradients (outside
  the hero flow diagram), no noise / grain. Hairline 1 px borders and
  a single warm accent do all the visual work.
- **One accent.** Amber (`--accent-e`). Everything else is ink or text.
  Status colours (red / green / cyan) exist but are reserved for
  state — not decoration.
- **Monospace as a texture.** Tabular numerics, eyebrow labels, API
  output, and section titles use `var(--mono)`. Prose uses `var(--sans)`.
- **Dark-first.** Dark is default; light is opt-in via
  `<html data-theme="light">`. Both themes share token *names*; only
  their values flip.
- **No network fonts.** Fallbacks resolve to `system-ui` / `ui-monospace`
  so every page renders on a fresh Pi with no WAN.

## Palette

### Dark (default)

| Token | Role | Value |
|---|---|---|
| `--ink` | Page background | `oklch(0.14 0.015 250)` |
| `--ink-raised` | Cards, surfaces | `oklch(0.18 0.02 250)` |
| `--ink-sunken` | Inputs, recessed areas | `oklch(0.11 0.015 250)` |
| `--line` | Default 1 px border | `oklch(0.26 0.015 250)` |
| `--line-soft` | Subtle divider | `oklch(0.22 0.015 250)` |
| `--fg` | Body text | `oklch(0.96 0.01 250)` |
| `--fg-dim` | Secondary text | `oklch(0.70 0.015 250)` |
| `--fg-muted` | Eyebrow / caption text | `oklch(0.50 0.015 250)` |
| `--accent-e` | Primary accent (amber) | `oklch(0.82 0.16 65)` |
| `--amber`, `--amber-d` | Warm chart / decorative | amber pair |
| `--green-e` | "Ok", online, success | `oklch(0.78 0.16 150)` |
| `--red-e` | Error, remove, critical | `oklch(0.72 0.18 20)` |
| `--cyan`, `--cyan-dim` | PV / informational | cyan pair |
| `--violet` | MPC / plan layer | `oklch(0.80 0.14 300)` |
| `--white-s` | Strong on-accent text | `oklch(0.95 0.01 250)` |

### Light (flipped when `data-theme="light"`)

Only the next-era tokens flip — legacy hex tokens stay for `/legacy`.

| Token | Value |
|---|---|
| `--ink` | `oklch(0.985 0.005 250)` |
| `--ink-raised` | `oklch(0.975 0.005 250)` |
| `--line` | `oklch(0.85 0.01 250)` |
| `--fg` | `oklch(0.22 0.02 250)` |
| `--fg-dim` | `oklch(0.40 0.02 250)` |
| `--accent-e` | `oklch(0.68 0.16 65)` |

Functional colours (`--red-e`, `--green-e`, …) pull ~15 % darker in
light mode so they keep contrast on paper without looking neon.

### Legacy palette (deprecated, still loaded)

Hex tokens `--bg / --surface / --surface2 / --border / --text /
--text-dim / --accent / --green / --red / --radius` live at `:root`
alongside the next palette so `/legacy` (the pre-redesign dashboard,
still served for regression comparison) keeps its historical
appearance. **Do not use these for new work** — reach for the oklch
tokens above.

## Accent hue

`--accent-e` is derived from `--accent-hue` (default `65`, warm amber).
Changing the hue once rotates every accent-derived surface (hero
stroke, ring, load-text, glow) to match — do not hard-code amber
values.

## Typography

### Stacks (no CDN)

```css
--sans: 'Inter', system-ui, -apple-system, 'Segoe UI', Roboto,
        'Helvetica Neue', Arial, sans-serif;
--mono: 'JetBrains Mono', ui-monospace, 'SF Mono', Menlo, Consolas,
        'Liberation Mono', monospace;
```

Inter / JetBrains Mono win if locally installed; otherwise the page
resolves to the OS's native UI font (Segoe UI, SF, Roboto). We
deliberately do **not** fetch either from Google Fonts — fresh-Pi
deploys have to boot without WAN. If exact typographic parity with
the landing page becomes a hard requirement, self-host the variable
woff2 under `/web/fonts/` and add `@font-face` — do not reintroduce
fonts.googleapis.com.

### Scale

| Role | Family | Size | Weight | Letter-spacing |
|---|---|---|---|---|
| H1 / hero | sans | clamp(44–80 px) | 700 | −0.035em |
| H2 / page heading | sans | 1.35 rem | 700 | −0.015em |
| H3 / card title | sans | 1 rem | 600 | — |
| Eyebrow / section label | **mono** | 0.68–0.72 rem | 500 | **0.18em**, UPPERCASE |
| Body | sans | 14 px / 0.9 rem | 400 | — |
| Numeric callout | **mono** | varies | 500–700 | `font-variant-numeric: tabular-nums` |
| Button label | sans | 14 px | 500 | — |
| Dim caption | sans | 0.78–0.82 rem | 400 | — |

Body `line-height: 1.58`. Headings `line-height: 1.1–1.15`.

## Shape & depth

| Token | Value | Used on |
|---|---|---|
| `--radius-lg` | 18 px | Hero, large pill headers |
| `--radius-md` | 14 px | Header drawer, big cards |
| `--radius-sm` | 10 px | Default card, input group |
| `--radius` (legacy) | 8 px | Buttons, small controls |
| (inline) | 999 px | Eyebrow / status pills |

- **Borders**: 1 px solid `var(--line)` everywhere a card exists.
- **Shadows**: avoid. Single permitted case is
  `box-shadow: 0 0 10px var(--accent-e)` on a 6 px status / step dot.
- **Backdrop filters**: `blur(14px)` on the `/next` header
  pseudo-element only — never on the element itself (it creates a new
  containing block and breaks `position: fixed` modal descendants,
  which is why the `<ftw-update-badge>` modal was centring against
  the header pill before #127).

## Component vocabulary

### Primary CTA

```css
background: var(--accent-e);
color: #0a0a0a;          /* on-accent text is near-black, never white */
padding: 11px 18px;
border-radius: 8px;
font-weight: 500; font-size: 14px;
```

Hover: `transform: translateY(-1px)`, no colour shift.

### Secondary / ghost

```css
background: transparent;
color: var(--fg);
border: 1px solid var(--line);
border-radius: 8px;
```

Hover: `border-color: var(--fg-dim)`. Never change background.

### Eyebrow label / section header

```css
font-family: var(--mono);
font-size: 0.7rem;
text-transform: uppercase;
letter-spacing: 0.18em;
color: var(--accent-e);   /* or var(--fg-muted) for input labels */
font-weight: 500;
```

Use this for every `<label>`, `<h3>` above a card, step title prefix,
and status-pill content.

### Card

```css
background: var(--ink-raised);
border: 1px solid var(--line);
border-radius: 10px;
padding: 12px 14px;       /* tight */
                          /* or 16px 18px for integration sections */
```

### Input / select

```css
background: var(--ink-raised);
border: 1px solid var(--line);
border-radius: 8px;
padding: 10px 12px;
color: var(--fg);
font-family: var(--sans);
font-size: 0.9rem;
```

Focus: `border-color: var(--accent-e)`, no outline.

### Status / step dot

```css
width: 6px; height: 6px;
border-radius: 999px;
background: var(--line);        /* inactive */
/* active */
background: var(--accent-e);
box-shadow: 0 0 10px var(--accent-e);
/* done */
background: var(--fg-muted);
```

## Decorative touches worth echoing

- **Mono numeric prefixes** (`01`, `02`, …) in `var(--accent-e)` before
  step titles when the page is a workflow.
- **Pill eyebrows** with `background: color-mix(in srgb, var(--accent-e) 10%, transparent)`,
  accent text, 999 px radius, mono uppercase.
- **Accent glow** on small status / step dots
  (`box-shadow: 0 0 10px var(--accent-e)`).
- **Hairline section dividers** (1 px `var(--line)`, no shadow).

Avoid: marketing-style radial gradients, noise textures, drop shadows
on cards, large rounded boxes (> 18 px radius), gradient-filled buttons.

## Files

| File | Owns |
|---|---|
| `web/components/theme.css` | All tokens, both themes. Loaded first by every page. |
| `web/components/ftw-element.js` | Base class that gives every `<ftw-*>` shadow DOM the token inheritance for free. |
| `web/style.css` | Shared foundation — loaded by `/`, `/legacy`, `/setup`. Holds resets, form defaults, and legacy dashboard rules that haven't migrated yet. Retire once `/legacy` goes away. |
| `web/next.css` | `/` (default dashboard) specific layer — scoped to `body.ftw-next` so it doesn't bleed into `/legacy`. |
| `web/next.css` | `/next` dashboard specific layout. |
| `web/setup.html` (inline `<style>`) | `/setup` wizard chrome. |

New components read tokens from `:root`; they never redeclare
colours locally. When a `<ftw-*>` component needs a
component-specific hue, extend the `--*-e` naming convention so the
light theme can flip it cleanly.

## What NOT to do

- Do not reintroduce Google Fonts. Self-host or fall back to system.
- Do not hard-code `#ffb020` — use `var(--accent-e)`.
- Do not use the legacy hex palette (`--surface`, `--accent`, `--green`)
  in new code; those tokens will be retired once `/legacy` is
  decommissioned.
- Do not stack multiple accent colours. One accent, restraint, hierarchy
  through mono / sans contrast and type weight.
- Do not add `box-shadow` on cards or modals. The only sanctioned shadow
  is the accent glow on status dots.
