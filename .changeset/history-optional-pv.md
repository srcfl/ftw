---
"ftw": patch
---

Keep the history chart and site energy running when a device can report PV but does not, such as a Zap P1 meter with PV reading off, or while a device identity is still unconfirmed. Before, adding such a device stopped history until it was removed, even across restarts. Forecast learning keeps its stricter checks.
