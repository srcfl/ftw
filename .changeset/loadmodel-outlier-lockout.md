---
"ftw": patch
---

Load model: replace the MAE-band outlier filter with a physical ceiling
taken from the main fuse. The band rejected the upper half of the real
load distribution, and could lock the model out of a level it had not seen
before, permanently.

A rejected sample updates neither `MAE` nor `Samples`, so the band
`max(MAE × 10, 200)` never grew in response to being persistently wrong.
Measured on a clean model: an hour at 400 W left MAE at 57 W and the band
at 570 W; a following week at 5 kW was rejected in full, 100% of samples,
and the prediction never moved off 1794 W.

The band was the wrong instrument, not merely mistuned. Household load is
multimodal — a few hundred watts of baseline, then 11 kW when the sauna,
oven and car overlap — and nothing about a residual's size separates
"unusual but real" from "wrong". A band fitted to the quiet hours always
excludes the busy ones. Short-term measurement noise is already handled a
layer down, by the Kalman filter in telemetry whose smoothed output this
model reads, so the second filter was both harmful and redundant.

What remains is the rejection physics licenses: a house cannot draw more
than its main fuse passes. The ceiling is `fuse capacity × 1.25`, passed in
from configuration and never derived from what the model has learned, so it
holds from the first sample and cannot be talked down by a model that has
mislearned. With no fuse configured the check disables itself rather than
inventing a limit.

A sustained shift to a new load level is now learned directly. A one-minute
spike still trains — it is real consumption — and the bucket EMA damps it
to a tenth of the gap, which is what keeps an hour from being defined by
its loudest minute.
