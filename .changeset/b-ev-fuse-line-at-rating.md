---
"ftw": patch
---

The car now charges up to the fuse rating less `fuse.safety_margin_a`, not a fixed 1 A below it. A charging car rises one step once that step fits under the line, instead of needing two steps of room and keeping one free; an idle car starts with all the room it has. Home battery charge beside a car uses the same line, with no 1 A floor. On a three-phase site with the default 0.5 A margin, a car near the fuse gets 0.5 A to 1.5 A more on each phase. Cuts above the line still wait for the fuse's heat budget, and the hour's import limit keeps its own margins. A car never rises or starts into battery discharge that the fuse guard added.
