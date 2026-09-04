---
"ftw": minor
---

A static / time-of-use price provider, so price-driven planning works outside Europe.

`price.provider: static` takes a flat rate in the install currency, plus optional local-time windows that override peak hours (overnight wrap, optional weekdays). Grid tariff and VAT still apply. Live day-ahead spot remains Europe-only; this is the stand-in for every other market. Settings and the setup wizard offer it next to Sourceful and ENTSO-E.
