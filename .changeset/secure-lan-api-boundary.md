---
"ftw": patch
---

Harden the LAN/API trust boundary in setup and normal operation: remove wildcard CORS, block browser cross-site mutations and active reads, require JSON content types, and require an opt-in Bearer token for protected requests addressed through public or fully qualified hostnames. Existing loopback and private-LAN UI/API clients remain compatible, with documented remote-access onboarding and local recovery.
