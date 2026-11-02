---
"ftw": minor
---

History shows the figure a Swedish household gives Skatteverket to get back the energy tax on grid electricity it stored in a battery and later sent back to the grid. Pick a calendar year to see the kWh, month by month, and download the hourly basis as CSV to keep with the application. Where the readings cannot tell where energy went, FTW counts low: solar counts first in every hour, what the house takes from the battery counts as grid energy first and what goes to the grid as other energy first, losses count as other energy, and an hour counts only when every meter read all of it. The figure needs one battery with its size set, and it assumes FTW reads every solar inverter, battery and car charger on the site. The same figure is at `GET /api/energy/battery-return?year=` and the hours at `/api/energy/battery-return.csv`.
