---
"ftw": patch
---

Price the clock hour in progress when a plan starts part way through it. With 15-minute prices Core replans at :15, :30 and :45, and the peak-power tariff it sends the planner left out the hour already running, so a car plugged in at 17:20 could set a new monthly peak in that hour. Core now sends that hour with the import it has measured so far (from the site meter's hourly counter, else the history; time neither covers counts at no less than the house's current import), and today's completed peak goes in today's group, so each day is one sample. Only a planner that announces `demand_elapsed_hours` gets this shape; a planner without it, such as the bundled one, gets exactly the request it got before.
