---
"ftw": patch
---

Stop asking a home battery to discharge when its charge is below 5 % or it reports that it cannot discharge. The slew limit used to ramp such a battery back toward the discharge it was already doing. If stopping it at once would put the site or one phase over the fuse or import limit, other batteries take that load first, and the blocked battery keeps only the discharge still needed.
