---
"ftw": minor
---

Each car on the box's page now has its own goal button. "Set goal" or "Change goal" opens the same goal editor and "Only spare solar" switch as the charger view, saved with the car, so you can set a car's goal while it is away or unplugged. It applies wherever the car plugs in. The page sends it through a new route, `PUT /api/cars/{id}/goal`, which takes `car.goal.set`'s arguments and is served only on the box's own page, as the other car routes are.
