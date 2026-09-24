---
"ftw": minor
---

Ask why is removed: the Plan card question box, the header chip, the Settings
fieldset and the `/api/assistant/*` routes. On first start, Core deletes the
stored Ask why settings and OpenRouter key. Old conversations stay unused in
state.db. The help report is unchanged and is again the one button under the plan.
