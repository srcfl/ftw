---
"ftw": patch
---

Redact secrets in support-dump logs the same way Ask why does, including OAuth JSON and Bearer tokens, and treat authorization, passwd, credential and a bare auth key as sensitive in the redacted config.
