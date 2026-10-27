---
"ftw": patch
---

End manual battery Charge when FTW restarts. Return to automatic planning
with the saved battery export permission, or Idle when the planner is disabled.
Ignore old retained MQTT Charge commands so reconnecting cannot resume the
override. A new Charge command still works.
