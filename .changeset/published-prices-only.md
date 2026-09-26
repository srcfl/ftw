---
"ftw": minor
---

The planner uses the electricity prices that are published, and no longer estimates prices beyond them. The plan reaches as far as the day-ahead prices, which is 11–35 hours: tomorrow's prices usually arrive around 13:00. The Plan view has no "predicted" zone any more. It says how far the plan reaches, for example "Plan until Sat 24:00", and the chart ends there. The Home Assistant plan and price attributes no longer include `confidence`, which was always 1 outside the removed forecast. An EV target later than the last published price is planned within the known prices until the next day's prices arrive.
