---
"ftw": patch
---

Bound Lua driver safety so a stuck poll, a huge watchdog override, or a
redirected POST cannot leave hardware on its last setpoint. Every driver VM
now drops `os.execute`/`io`/`load`, poll can be cancelled so default mode
still runs, watchdog overrides cap at 15 minutes, and `http_post` refuses
301/302/303 redirects the same way PATCH already does.
