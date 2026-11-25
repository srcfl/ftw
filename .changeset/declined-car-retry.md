---
"ftw": minor
---

A car that stopped asking for charge is no longer left out for the rest of
the plug-in. While it stays plugged in with a goal to meet, FTW plans it
again an hour after it declined, or when the next ready time starts. A car
that is full or at its own limit declines once more and is left out again
after 90 seconds, so FTW offers it current at most once an hour. FTW does
not retry when the goal is met, when the car reports it is done or at its
limit, after a one-time ready time has passed, or on a charger it only
observes. Unplugging, a higher goal and Charge now still act at once, and
the plan now follows a refusal and its end right away instead of at the
next scheduled replan.
