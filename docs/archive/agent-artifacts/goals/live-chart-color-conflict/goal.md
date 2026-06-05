# Fix duplicate purple series in Live chart

## Objective

In the dashboard Live chart, two series currently render with the same
purple colour — `Laddning bil EV` and `Pixii` — making them
indistinguishable. Recolour one of them so both series are visually
unambiguous, using the design tokens documented in
`web/components/theme.css` and respecting `DESIGN.md`.

## Original Request

Anders (field tester, Discord): "Ett litet önskemål, byta färg i
diagrammet, 2st lila just nu" — a small wish to change the colour in
the chart because two are purple right now.

Fredrik: "Enkel fix ska fixa." (Simple fix, will do.)

The two affected series in the legend are `Laddning bil EV` and
`Pixii`.

## Intake Summary

- Input shape: `specific`
- Audience: Anders (field tester) and any other dashboard user — two
  series of the same colour are unreadable on the Live chart.
- Authority: `approved` (owner committed to fix in chat)
- Proof type: `artifact` — visual confirmation in the dashboard plus a
  diff that swaps the colour token cleanly.
- Completion proof: The Live chart legend shows `Laddning bil EV` and
  `Pixii` rendered in two visually distinct colours, both pulled from
  `web/components/theme.css` tokens, with no hard-coded hex literals
  added; light and dark themes both look correct.
- Likely misfire: introducing a brand-new hard-coded hex colour or
  Google-font dependency, or recolouring a different series and
  leaving the duplicate in place.
- Blind spots considered:
  - Which of the two should keep purple — the user did not say.
    Heuristic: `Pixii` (battery) is more brand-shaped, so it likely
    keeps purple; `Laddning bil EV` gets a new colour. Judge confirms
    via Scout evidence.
  - Forecast variants (`PV fc`, `Load fc`) may already use dashed
    style of the same hue; a new colour must not collide with any
    other series, including forecast lines and the accent amber.
  - Light theme: per `DESIGN.md`, no hex literals, no `data-theme`
    branching; use `--*-e` tokens so the light theme flips cleanly.
- Existing plan facts:
  - Use `web/components/theme.css` tokens; do not introduce raw hex.
  - One amber accent only; the new colour must be a distinct hue.
  - 1 px hairlines, no shadows on cards.
  - Fresh-Pi deploys must boot without WAN — no new Google Fonts.

## Goal Kind

`specific`

## Current Tranche

Single safe slice: identify the file(s) that map series → colour in the
Live chart, choose a token-based recolour for one of the two purple
series, implement it, and confirm visually that both series are now
distinct in both light and dark themes.

## Non-Negotiable Constraints

- All colour values come from `web/components/theme.css` tokens.
- No hard-coded hex literals.
- No new Google Fonts or other WAN dependencies.
- The chosen colour must not collide with `Grid`, `PV`, `PV fc`,
  `Load`, `Load fc`, the remaining purple series, or the amber accent.
- Light theme must still look correct without `data-theme` branching.

## Stop Rule

Stop only when a final audit proves the full original outcome is
complete: legend shows two visually distinct colours for the formerly
duplicated series, tokens are used (not hex), and both themes render
correctly.

## Canonical Board

Machine truth lives at:

`docs/goals/live-chart-color-conflict/state.yaml`

If this charter and `state.yaml` disagree, `state.yaml` wins.

## Run Command

```text
/goal Follow docs/goals/live-chart-color-conflict/goal.md.
```

## PM Loop

On every `/goal` continuation:

1. Read this charter.
2. Read `state.yaml`.
3. Run the bundled GoalBuddy update checker when available and mention
   a newer version without blocking.
4. Re-check the intake.
5. Work only on the active board task.
6. Assign Scout, Judge, Worker, or PM according to the task.
7. Write a compact task receipt.
8. Update the board.
9. If Judge selected a safe Worker task with `allowed_files`,
   `verify`, and `stop_if`, activate it and continue unless blocked.
10. Treat any slice audit as a checkpoint, not completion, unless it
    proves the full original outcome is complete.
11. Finish only with a Judge/PM audit receipt that records
    `full_outcome_complete: true`.
