---
"ftw": patch
---

Reject unsafe Nova key files and persist first-boot identity atomically.
Key storage now fails closed without Unix owner and link metadata, hard links,
no-follow opens, file sync, and directory sync. It creates only the final key
directory and requires its trusted parent to exist.
