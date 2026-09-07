---
"ftw": patch
---

Restore and save rotated OAuth tokens before drivers start so myUplink can stay connected across Core restarts and updates.

Apply signed OAuth rules to managed drivers, including official beta installs. Allow token exchange only at the declared path, block redirects, and let each driver save only its declared secret keys with bounded keys and values.
