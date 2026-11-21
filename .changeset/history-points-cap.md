---
"ftw": patch
---

History reads now return at most 2000 rows. `/api/history` and
`/api/series` cut a larger `points` value down to 2000 instead of failing
or building a huge reply, and they refuse a `points` value that is not a
whole number, or is below 0, with a 400. The dashboard's own reads are
unchanged.
