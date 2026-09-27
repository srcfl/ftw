---
"ftw": patch
---

Changing the planner mode or forecast caution now returns at once and replans in the background, so leaving the page mid-save no longer publishes a fallback plan that shows the optimizer as unhealthy. A config save that lands while the planner is working no longer produces a plan that mixes the old and new fuse, export ceiling or tariff. Earlier peaks this month now count once per day for every demand tariff, and with the Ellevio night weight a night peak counts at its weighted value, so the planner no longer treats daytime import up to that night peak as free. Prices fetched again at another resolution now replace the cached rows instead of overlapping them, which had stopped every replan until those rows aged out.
