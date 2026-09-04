---
"ftw": patch
---

The Weather settings tab is gone. Its sections — location, forecast
provider, PV arrays and roof derivation — now render inside the Control
tab, next to the site scalars they belong with; the Planner tab keeps only
the MPC scalars and engine controls, and points to where its inputs live.
Nothing about the fields themselves changed: `tabs/weather.js` still owns
the sections, their behaviour and their tests.
