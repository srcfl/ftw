---
"ftw": patch
---

The home battery now protects each fuse phase in the direction its current flows. It no longer discharges into a phase that already exports past the fuse, and no longer cuts a discharge that keeps an importing phase under it. It counts its own share of each phase, so it holds a steady relief instead of switching between full relief and none on every dispatch, also when its own power reading lags the meter. When one phase imports past the line and another exports past it, it takes the move that leaves the worse phase least over instead of holding its last command. When the meter does not show its moves, because the meter repeats one reading or the phase over the line is one the battery does not reach, it stops adding relief once it has moved one ampere per phase, instead of running out to its discharge limit. It relieves to the fuse less `safety_margin_a`, the same line the car uses.
