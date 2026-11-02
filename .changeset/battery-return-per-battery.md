---
"ftw": patch
---

The figure for Skatteverket's energy tax refund after battery storage now works for sites with more than one battery. Each battery keeps its own account of grid and other energy, with its own size and efficiency, so one battery's exports never carry another's grid energy, and for each battery solar, a car and the other batteries count first in every hour. Each battery takes its size from the driver that reads it, and the hourly CSV has one row per battery and hour.
