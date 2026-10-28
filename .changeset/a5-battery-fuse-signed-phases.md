---
"ftw": patch
---

The home battery now protects each fuse phase in the direction its current flows. It no longer discharges into a phase that already exports past the fuse, and no longer cuts a discharge that keeps an importing phase under it. It counts its own share of each phase, so it holds a steady relief instead of switching between full relief and none on every dispatch, also when its own power reading lags the meter. It relieves to the fuse less `safety_margin_a`, the same line the car uses.
