---
"ftw": patch
---

Keep optional battery charging out of the power still needed by an EV whose accepted current offer is not fully delivered, including Charge now. Missing or unaligned EV measurements retain the full reservation; EV starts and increases still wait for measured site headroom.

Keep the measured phase reservation active while a managed EV pause propagates, and do not release permitted battery relief twice before a prior import increase is observed. Existing fuse protection waits are unchanged.
