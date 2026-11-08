---
"ftw": minor
---

The box now says when the batteries are not doing what the plan meant, and why. `GET /api/status` gains `plan_now`: this quarter's planned and measured battery, solar, home and car power, `as_planned` or `off_plan` after a minute, and the measured cause. A battery's control row says "as planned" only when that is true, and the plan card shows what the battery does now beside what the plan said. The control journal keeps each stretch as `site:plan`.
