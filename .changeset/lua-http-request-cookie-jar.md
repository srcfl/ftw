---
"ftw": patch
---

Lua drivers can now sign in through a web login. The new `host.http_request` returns the status, headers and redirect target. The host keeps the session cookies for the driver's allowed hosts, in memory only. A read-only driver may declare several sign-in paths with `auth_post_paths`. This lets the VW Group driver renew its portal session itself, instead of the owner pasting a new cookie every hour.
