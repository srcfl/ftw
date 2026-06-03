---
"forty-two-watts": minor
---

prices: implement the ENTSO-E day-ahead XML parser

The `entsoe` provider previously returned `"entsoe: XML parser not yet
implemented"` for every fetch — selecting it in Settings → Price (or as a
fallback when elprisetjustnu fails) silently produced no prices at all.

It now decodes the A44 `Publication_MarketDocument` (TimeSeries > Period >
Point), handling both PT60M and PT15M resolutions and the sparse
carry-forward representation, and converts EUR/MWh to the configured
currency per kWh via the existing FX converter (ballpark 11.5 SEK/EUR when
rates aren't wired). A day the auction hasn't published yet returns no
rows, mirroring the elprisetjustnu path so the hourly scheduler just
retries.
