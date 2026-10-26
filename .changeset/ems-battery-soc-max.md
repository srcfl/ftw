---
"ftw": patch
---

Stop battery charging at the configured maximum charge level in every EMS mode,
including manual holds and self-tuning. Use each battery's soc_max, or the
planner's limit when the battery has none. Charging resumes one percentage
point below that limit; discharge remains available.
