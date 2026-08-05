---
"ftw": patch
---

Learned PV and load state is now stored with a fingerprint of the feature vector it was fitted against, and is discarded when that fingerprint no longer matches. Coefficients only mean something against the features that produced them: change the number of time-of-day harmonics, the bucket a moment maps to, or what clear-sky irradiance refers to, and the old model keeps predicting — with a plausible-looking error — from a fit that describes a different world. Nothing in the stored numbers gave that away before. The fingerprint is derived from the feature functions themselves, so a code change to the features moves it without anyone having to remember. On a mismatch the model logs both fingerprints and cold-starts, which both models recover from; wrong coefficients they do not recover from. Existing state is carried over unchanged on upgrade.
