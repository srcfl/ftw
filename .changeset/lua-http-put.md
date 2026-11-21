---
"ftw": patch
---

Lua drivers can now send `host.http_put(url, body, headers)`. It has the same
safety gates as `host.http_patch`: it needs `capabilities.http.allow_write`,
stays inside `allowed_hosts`, refuses redirects and cannot write during a
fingerprint probe. Some device APIs, such as the sonnenBatterie JSON API v2,
accept settings only through PUT. No existing driver changes behaviour.
