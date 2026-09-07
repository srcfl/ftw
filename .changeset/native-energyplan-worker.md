---
"ftw": minor
---

Use the compiled Energyplan worker first in beta releases when no planner engine
is set. Core validates its plan, then runs Core DP as a background shadow on the
same downside PV input. Core DP remains the validated fallback, with a visible
reason when it takes over. The worker and its license ship and update with Core;
source stays private. Explicit core and python settings keep their roles.

Reject EV plans above the battery limit and clip DP power at the operating band
so fallback energy matches the power it schedules.
