---
"ftw": patch
---

Stop asking a home battery to discharge when its charge is below 5 % or it reports that it cannot discharge. The slew limit used to ramp such a battery back toward the discharge it was already doing. If stopping it at once would push the site over its fuse or import limit, the battery ramps down toward 0 W instead.
