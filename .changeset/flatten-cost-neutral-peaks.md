---
"ftw": minor
---

The planner now flattens cost-neutral grid peaks and reports unmet EV or thermal service in the solver payload.

After the economic solve, a preference stage keeps the same money and then minimises the horizon's import and export peaks. Site fuse limits stay hard. If the extra solve is late or fails, the economic plan is kept. The solver payload names `preference_stage`, the resulting peaks, and any remaining flex/storage/thermal shortfall so the UI can say why a deadline was missed. Dual-release: Core understands the new fields; an older optimizer image simply omits them.
