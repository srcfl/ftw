---
"ftw": patch
---

Add `price.base_url`, for tests and development only. It points the elprisetjustnu price source at another server and turns off the Nord Pool fallback. The site rig uses it to pin prices. Real sites leave it unset and see no change.
