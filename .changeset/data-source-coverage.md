---
"ftw": patch
---

Every external data source now declares where in the world it works, and the
Settings map says so before you commit to a location. `GET /api/data-sources`
reports each source's kind, coverage area, countries and licence, plus an
advisory `covers` verdict for the configured site (or an explicit `?lat=&lon=`
preview). The Weather tab renders it under the location picker and refreshes
as the pin drags, so a site outside Europe learns up front that price-driven
planning has no source there instead of getting an empty price curve with no
explanation.

The registry's European price-country list is held in lockstep with
`prices/zones.go` by a test, so a bidding zone added there cannot silently
go missing from the coverage answer.
